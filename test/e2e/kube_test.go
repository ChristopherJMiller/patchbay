//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// fakephoneResult is one finished fakephone pod.
type fakephoneResult struct {
	Pod       *corev1.Pod
	Logs      string
	Succeeded bool
}

// runFakephone runs fakephone with args as a one-shot Pod on the pod
// network (not the host network: like a phone, it reaches the PBX at the
// node's address from elsewhere), waits for it to finish, and returns its
// logs. A pod that succeeded is deleted; a failed one is kept for the
// diagnostics.
func runFakephone(ctx context.Context, t *testing.T, name string, extraEnv []corev1.EnvVar, args ...string) fakephoneResult {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "fakephone-" + name + "-",
			Namespace:    namespace,
			Labels:       map[string]string{"app.kubernetes.io/name": "fakephone"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         ptr.To[int64](120),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "fakephone",
				Image:           toolsImage,
				ImagePullPolicy: corev1.PullNever,
				Args:            args,
				Env: append([]corev1.EnvVar{
					{Name: "FAKEPHONE_SERVER", Value: fmt.Sprintf("%s:%d", h.nodeIP, sipPort)},
					{Name: "FAKEPHONE_USER", Value: sipUser},
					{Name: "FAKEPHONE_PASSWORD", Value: sipPassword},
					{Name: "FAKEPHONE_LOCAL_IP", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
					}},
				}, extraEnv...),
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	pods := h.cs.CoreV1().Pods(namespace)
	created, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create fakephone pod: %v", err)
	}
	t.Logf("fakephone pod %s: %s", created.Name, strings.Join(args, " "))

	var done *corev1.Pod
	err = wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := pods.Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		done = p
		return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed, nil
	})
	if err != nil {
		phase := corev1.PodPhase("unknown")
		if done != nil {
			phase = done.Status.Phase
		}
		t.Fatalf("fakephone pod %s did not finish: %v (phase %s)", created.Name, err, phase)
	}
	raw, err := pods.GetLogs(created.Name, &corev1.PodLogOptions{Container: "fakephone"}).DoRaw(ctx)
	if err != nil {
		t.Fatalf("logs of %s: %v", created.Name, err)
	}
	res := fakephoneResult{Pod: done, Logs: string(raw), Succeeded: done.Status.Phase == corev1.PodSucceeded}
	t.Logf("fakephone pod %s %s (pod IP %s):\n%s", created.Name, done.Status.Phase, done.Status.PodIP, indent(res.Logs))
	if res.Succeeded {
		_ = pods.Delete(context.WithoutCancel(ctx), created.Name, metav1.DeleteOptions{})
	}
	return res
}

// pbxPod returns the running PBX pod.
func pbxPod(ctx context.Context, t *testing.T) *corev1.Pod {
	t.Helper()
	list, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=pbx",
	})
	if err != nil {
		t.Fatal(err)
	}
	var running []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			running = append(running, p)
		}
	}
	if len(running) != 1 {
		t.Fatalf("want exactly one running PBX pod, found %d of %d", len(running), len(list.Items))
	}
	return &running[0]
}

// asterisk runs one CLI command in the PBX pod's asterisk container
// (`asterisk -rx <command>`) and returns its output.
func asterisk(ctx context.Context, t *testing.T, cfg *envconf.Config, command string) string {
	t.Helper()
	pod := pbxPod(ctx, t)
	r, err := resources.New(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := r.ExecInPod(ctx, namespace, pod.Name, "asterisk", []string{"asterisk", "-rx", command}, &stdout, &stderr); err != nil {
		t.Fatalf("asterisk -rx %q: %v\n%s%s", command, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// mediaStats are the numbers fakephone's call reports.
type mediaStats struct {
	Sent, Received, ToneFrames int
	Source                     string
}

var mediaLine = regexp.MustCompile(`sent=(\d+) received=(\d+) pcmu=\d+ toneFrames=(\d+) maxRatio=\S+ firstTone=\S+ source=([^\s"]*)`)

// parseMedia finds the media summary in fakephone's output.
func parseMedia(logs string) (mediaStats, bool) {
	m := mediaLine.FindStringSubmatch(logs)
	if m == nil {
		return mediaStats{}, false
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	return mediaStats{Sent: atoi(m[1]), Received: atoi(m[2]), ToneFrames: atoi(m[3]), Source: m[4]}, true
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
