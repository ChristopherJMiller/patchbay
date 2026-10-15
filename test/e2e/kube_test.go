//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// fakephoneResult is one finished tools pod.
type fakephoneResult struct {
	Pod       *corev1.Pod
	Logs      string
	Succeeded bool
}

// toolPod is one run of the tools image (fakephone and its probes).
type toolPod struct {
	name string
	args []string
	env  []corev1.EnvVar
	// hostNetwork puts the pod on the node's network (AMI is on its
	// loopback; a source outside the allowed networks); hostPID lets it
	// read other containers' /proc entries.
	hostNetwork, hostPID bool
	// runAsUser/runAsGroup replace the image's nonroot user when set.
	runAsUser, runAsGroup *int64
	volumes               []corev1.Volume
	mounts                []corev1.VolumeMount
}

// startTool creates a one-shot tools pod on the pod network (unless
// hostNetwork) and returns its name without waiting.
func startTool(ctx context.Context, t *testing.T, p toolPod) string {
	t.Helper()
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsUser:                p.runAsUser,
		RunAsGroup:               p.runAsGroup,
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "fakephone-" + p.name + "-",
			Namespace:    namespace,
			Labels:       map[string]string{"app.kubernetes.io/name": "fakephone"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         ptr.To[int64](150),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			HostNetwork:                   p.hostNetwork,
			HostPID:                       p.hostPID,
			Volumes:                       p.volumes,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "fakephone",
				Image:           toolsImage,
				ImagePullPolicy: corev1.PullNever,
				Args:            p.args,
				Env:             p.env,
				SecurityContext: sc,
				VolumeMounts:    p.mounts,
			}},
		},
	}
	created, err := h.cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create tools pod: %v", err)
	}
	t.Logf("tools pod %s: %s", created.Name, strings.Join(p.args, " "))
	return created.Name
}

// waitTool waits for a tools pod to finish and returns its logs. wantSuccess
// is the outcome the caller expects: a pod that ends that way is deleted,
// and one that does not is kept for the diagnostics.
func waitTool(ctx context.Context, t *testing.T, name string, wantSuccess bool) fakephoneResult {
	t.Helper()
	pods := h.cs.CoreV1().Pods(namespace)
	var done *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
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
		t.Fatalf("tools pod %s did not finish: %v (phase %s)", name, err, phase)
	}
	raw, err := pods.GetLogs(name, &corev1.PodLogOptions{Container: "fakephone"}).DoRaw(ctx)
	if err != nil {
		t.Fatalf("logs of %s: %v", name, err)
	}
	res := fakephoneResult{Pod: done, Logs: string(raw), Succeeded: done.Status.Phase == corev1.PodSucceeded}
	t.Logf("tools pod %s %s (pod IP %s):\n%s", name, done.Status.Phase, done.Status.PodIP, indent(res.Logs))
	if res.Succeeded == wantSuccess {
		_ = pods.Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
	}
	return res
}

// waitLog waits until a running tools pod's log contains want.
func waitLog(ctx context.Context, t *testing.T, name, want string, timeout time.Duration) {
	t.Helper()
	pods := h.cs.CoreV1().Pods(namespace)
	var logs string
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		raw, err := pods.GetLogs(name, &corev1.PodLogOptions{Container: "fakephone"}).DoRaw(ctx)
		if err != nil {
			return false, nil //nolint:nilerr // the container has not started yet: keep polling
		}
		logs = string(raw)
		return strings.Contains(logs, want), nil
	})
	if err != nil {
		t.Fatalf("tools pod %s never logged %q: %v\n%s", name, want, err, indent(logs))
	}
}

func runTool(ctx context.Context, t *testing.T, p toolPod, wantSuccess bool) fakephoneResult {
	t.Helper()
	return waitTool(ctx, t, startTool(ctx, t, p), wantSuccess)
}

// secretEnv is an environment variable from a Secret key.
func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key},
	}}
}

// deviceEnv is fakephone's environment as one compiled device: its number
// as the SIP user, its ID as the digest username, its password from its
// Secret, the PBX's SIP and TFTP addresses, and the pod's own IP.
func deviceEnv(id, number string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "FAKEPHONE_SERVER", Value: fmt.Sprintf("%s:%d", h.nodeIP, sipPort)},
		{Name: "FAKEPHONE_TFTP_SERVER", Value: fmt.Sprintf("%s:%d", h.nodeIP, tftpPort)},
		{Name: "FAKEPHONE_USER", Value: number},
		{Name: "FAKEPHONE_AUTH_USER", Value: id},
		secretEnv("FAKEPHONE_PASSWORD", compile.DeviceSecretName(id), compile.SecretKeySIPPassword),
		{Name: "FAKEPHONE_LOCAL_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}},
	}
}

// runFakephone runs fakephone with args as Phone desk on the pod network
// (like a phone, it reaches the PBX at the node's address from elsewhere),
// waits for it to finish and returns its logs.
func runFakephone(ctx context.Context, t *testing.T, name string, wantSuccess bool, args ...string) fakephoneResult {
	t.Helper()
	return runTool(ctx, t, toolPod{name: name, args: args, env: deviceEnv(deskID, deskNumber)}, wantSuccess)
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

// asteriskConf is the compiled asterisk.conf, through the current symlink:
// the remote console needs it too (the chart's probes pass it).
var asteriskConf = compile.AsteriskConfigDir + "/asterisk.conf"

// asterisk runs one CLI command in the PBX pod's asterisk container
// (`asterisk -C <conf> -rx <command>`) and returns its output.
func asterisk(ctx context.Context, t *testing.T, cfg *envconf.Config, command string) string {
	t.Helper()
	return execInAsterisk(ctx, t, cfg, "asterisk", "-C", asteriskConf, "-rx", command)
}

// execInAsterisk runs a command in the PBX pod's asterisk container (the
// only one with a shell and tools: the patchbay image is distroless) and
// returns its standard output.
func execInAsterisk(ctx context.Context, t *testing.T, cfg *envconf.Config, command ...string) string {
	const container = "asterisk"
	t.Helper()
	pod := pbxPod(ctx, t)
	r, err := resources.New(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := r.ExecInPod(ctx, namespace, pod.Name, container, command, &stdout, &stderr); err != nil {
		t.Fatalf("exec %q in %s: %v\n%s%s", command, container, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// procStatus parses /proc/<pid>/status into its fields.
func procStatus(status string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	return out
}

// rootMountOptions returns the mount options of / from /proc/<pid>/mounts.
func rootMountOptions(mounts string) []string {
	for line := range strings.SplitSeq(mounts, "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[1] == "/" {
			return strings.Split(f[3], ",")
		}
	}
	return nil
}

// mediaStats are the numbers fakephone's call reports.
type mediaStats struct {
	Sent, Received, ToneFrames int
	Source                     string
	Loud, Voice                int
}

var mediaLine = regexp.MustCompile(`sent=(\d+) received=(\d+) pcmu=\d+ toneFrames=(\d+) maxRatio=\S+ firstTone=\S+ source=([^\s"]*) loud=(\d+) voice=(\d+)`)

// parseMedia finds the media summary in fakephone's output.
func parseMedia(logs string) (mediaStats, bool) {
	m := mediaLine.FindStringSubmatch(logs)
	if m == nil {
		return mediaStats{}, false
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	return mediaStats{
		Sent: atoi(m[1]), Received: atoi(m[2]), ToneFrames: atoi(m[3]), Source: m[4],
		Loud: atoi(m[5]), Voice: atoi(m[6]),
	}, true
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

// agentForward is a port-forward from the harness to the agent's control
// port in the PBX pod, through the API server (pods/portforward): the path
// `patchbay firmware load` and `status` take (docs/plans/release-a.md W3.9).
type agentForward struct {
	mu   sync.Mutex
	pod  string
	addr string // 127.0.0.1:<local port>
	stop chan struct{}
	done chan struct{}
}

func (f *agentForward) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeLocked()
}

func (f *agentForward) closeLocked() {
	if f.stop != nil {
		close(f.stop)
		<-f.done
	}
	f.pod, f.addr, f.stop, f.done = "", "", nil, nil
}

// agentAddr returns a local address forwarded to the agent's control port,
// forwarding anew when the PBX pod changed or the last forward ended.
func (h *harness) agentAddr(ctx context.Context, t *testing.T, fresh bool) string {
	t.Helper()
	pod := pbxPod(ctx, t).Name
	f := &h.agentFwd
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addr != "" && f.pod == pod && !fresh {
		select {
		case <-f.done:
		default:
			return f.addr
		}
	}
	f.closeLocked()
	transport, upgrader, err := spdy.RoundTripperFor(h.rest)
	if err != nil {
		t.Fatal(err)
	}
	url := h.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", controlPort)},
		stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var fwErr error
	go func() { fwErr = fw.ForwardPorts(); close(done) }()
	select {
	case <-ready:
	case <-done:
		t.Fatalf("port-forward to %s: %v", pod, fwErr)
	case <-time.After(30 * time.Second):
		close(stop)
		t.Fatalf("port-forward to %s: not ready within 30 s", pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) != 1 {
		close(stop)
		t.Fatalf("port-forward to %s: ports %v, %v", pod, ports, err)
	}
	f.pod, f.addr, f.stop, f.done = pod, fmt.Sprintf("127.0.0.1:%d", ports[0].Local), stop, done
	return f.addr
}

// agentDo sends one request to the agent's control API (with the token)
// through the port-forward and returns the status and body. A request
// that fails to connect (the agent restarting) is retried on a fresh
// forward for up to a minute.
func (h *harness) agentDo(ctx context.Context, t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	var last error
	for attempt := range 30 {
		addr := h.agentAddr(ctx, t, attempt > 0)
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, method, "http://"+addr+path, bytes.NewReader(body))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.agentToken)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			b, rerr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			cancel()
			if rerr == nil {
				return res.StatusCode, b
			}
			err = rerr
		} else {
			cancel()
		}
		last = err
		select {
		case <-ctx.Done():
			t.Fatalf("%s %s: %v (%v)", method, path, ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("%s %s through the port-forward: %v", method, path, last)
	return 0, nil
}

// observations fetches GET /v1/observations.
func (h *harness) observations(ctx context.Context, t *testing.T) agentapi.Observations {
	t.Helper()
	code, body := h.agentDo(ctx, t, http.MethodGet, agentapi.PathObservations, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", agentapi.PathObservations, code, body)
	}
	var obs agentapi.Observations
	if err := json.Unmarshal(body, &obs); err != nil {
		t.Fatalf("observations: %v\n%s", err, body)
	}
	return obs
}

// eventually polls the observations every 2 s until cond holds, for up to
// timeout, and returns the document that satisfied it.
func (h *harness) eventually(ctx context.Context, t *testing.T, what string, timeout time.Duration,
	cond func(agentapi.Observations) (bool, string),
) agentapi.Observations {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		obs := h.observations(ctx, t)
		ok, why := cond(obs)
		if ok {
			return obs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s: %s", what, timeout, why)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v (%s)", what, ctx.Err(), why)
		case <-time.After(2 * time.Second):
		}
	}
}

// device returns one device's observations, or nil.
func device(obs agentapi.Observations, id string) *agentapi.DeviceObservations {
	for i := range obs.Devices {
		if obs.Devices[i].Device == id {
			return &obs.Devices[i]
		}
	}
	return nil
}
