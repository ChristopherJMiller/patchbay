//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

type ctxKey string

const crdNamespaceKey ctxKey = "crd-namespace"

// TestCRDs installs nothing itself: the CRDs come from the chart. It applies
// config/samples, reads them back typed, and checks that the API server
// enforces a CEL rule.
func TestCRDs(t *testing.T) {
	f := features.New("CRDs from the chart").
		Setup(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			suffix, err := randomHex(3)
			if err != nil {
				t.Fatal(err)
			}
			ns := "patchbay-e2e-crds-" + suffix
			if _, err := h.cs.CoreV1().Namespaces().Create(ctx,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			return context.WithValue(ctx, crdNamespaceKey, ns)
		}).
		Assess("the samples are accepted and read back", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(crdNamespaceKey).(string)
			r := typedResources(t, cfg)
			samples := os.DirFS(filepath.Join(h.root, "config", "samples"))
			if err := decoder.DecodeEachFile(ctx, samples, "patchbay_v1alpha1_*.yaml",
				decoder.CreateHandler(r), decoder.MutateNamespace(ns)); err != nil {
				t.Fatalf("apply config/samples: %v", err)
			}

			var phone v1alpha1.Phone
			if err := r.Get(ctx, "desk", ns, &phone); err != nil {
				t.Fatal(err)
			}
			if phone.Spec.Number != "100" || phone.Spec.MAC.Normalized() != "D0C78915E8C8" || phone.Spec.Model != v1alpha1.PhoneModelCisco7945 {
				t.Errorf("Phone desk read back as %+v", phone.Spec)
			}
			var soft v1alpha1.Softphone
			if err := r.Get(ctx, "chris-laptop", ns, &soft); err != nil {
				t.Fatal(err)
			}
			if soft.Spec.Number != "150" {
				t.Errorf("Softphone chris-laptop read back as %+v", soft.Spec)
			}
			var person v1alpha1.Person
			if err := r.Get(ctx, "chris", ns, &person); err != nil {
				t.Fatal(err)
			}
			if person.Spec.Number != "301" || len(person.Spec.Rings) != 2 {
				t.Errorf("Person chris read back as %+v", person.Spec)
			}
			var stream v1alpha1.Stream
			if err := r.Get(ctx, "king5", ns, &stream); err != nil {
				t.Fatal(err)
			}
			if stream.Spec.Number != "705" {
				t.Errorf("Stream king5 read back as %+v", stream.Spec)
			}
			return ctx
		}).
		Assess("a ring target naming both a phone and a softphone is rejected (CEL)", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(crdNamespaceKey).(string)
			bad := &v1alpha1.Person{
				ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: ns},
				Spec: v1alpha1.PersonSpec{
					Number: "302",
					Name:   "Both",
					Rings:  []v1alpha1.RingTarget{{Phone: "desk", Softphone: "chris-laptop"}},
				},
			}
			err := typedResources(t, cfg).Create(ctx, bad)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "exactly one of phone or softphone") {
				t.Fatalf("create a Person breaking the CEL rule: err = %v, want Invalid with the rule's message", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			if ns, ok := ctx.Value(crdNamespaceKey).(string); ok {
				if err := h.cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
					t.Logf("delete namespace %s: %v", ns, err)
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestPBXUp checks the PBX pod's shape, that Asterisk runs the static config,
// and that the agent answers through its Service from the pod network (the
// path the controller will use).
func TestPBXUp(t *testing.T) {
	f := features.New("PBX pod").
		Assess("runs on the host network of the pinned node, agent as a native sidecar", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			pod := pbxPod(ctx, t)
			if !pod.Spec.HostNetwork || pod.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
				t.Errorf("hostNetwork=%v dnsPolicy=%s", pod.Spec.HostNetwork, pod.Spec.DNSPolicy)
			}
			if pod.Spec.NodeName != h.nodeName || pod.Status.PodIP != h.nodeIP {
				t.Errorf("pod on %s with IP %s, want %s / %s", pod.Spec.NodeName, pod.Status.PodIP, h.nodeName, h.nodeIP)
			}
			ready := false
			for _, c := range pod.Status.Conditions {
				ready = ready || (c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue)
			}
			if !ready {
				t.Errorf("PBX pod %s is not Ready", pod.Name)
			}
			if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "agent" ||
				pod.Spec.InitContainers[0].RestartPolicy == nil ||
				*pod.Spec.InitContainers[0].RestartPolicy != corev1.ContainerRestartPolicyAlways {
				t.Errorf("the agent is not a native sidecar: %+v", pod.Spec.InitContainers)
			}
			for _, s := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				if !s.Ready || s.State.Running == nil {
					t.Errorf("container %s: ready=%v state=%+v", s.Name, s.Ready, s.State)
				}
				if s.RestartCount > 0 {
					t.Errorf("container %s restarted %d times", s.Name, s.RestartCount)
				}
			}
			return ctx
		}).
		Assess("Asterisk 22 runs the static config", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if v := asterisk(ctx, t, cfg, "core show version"); !strings.HasPrefix(v, "Asterisk 22.") {
				t.Errorf("core show version: %q", v)
			}
			transports := asterisk(ctx, t, cfg, "pjsip show transports")
			for _, want := range []string{
				fmt.Sprintf("transport-udp             udp      0      0  %s:%d", h.nodeIP, sipPort),
				fmt.Sprintf("transport-tcp             tcp      0      0  %s:%d", h.nodeIP, sipPort),
			} {
				if !strings.Contains(transports, want) {
					t.Errorf("pjsip show transports lacks %q:\n%s", want, transports)
				}
			}
			modules := asterisk(ctx, t, cfg, "module show")
			for _, m := range []string{
				"res_pjsip.so", "chan_pjsip.so", "res_pjsip_registrar.so", "res_pjsip_notify.so",
				"res_rtp_asterisk.so", "codec_ulaw.so", "codec_g722.so", "app_echo.so", "pbx_config.so",
				"res_audiosocket.so", "app_audiosocket.so",
			} {
				if !moduleRunning(modules, m) {
					t.Errorf("module %s is not Running", m)
				}
			}
			if ami := asterisk(ctx, t, cfg, "manager show settings"); !strings.Contains(ami, "127.0.0.1:5038") {
				t.Errorf("AMI is not bound to loopback only:\n%s", ami)
			}
			if httpStatus := asterisk(ctx, t, cfg, "http show status"); !strings.Contains(httpStatus, "Server Disabled") {
				t.Errorf("Asterisk's HTTP server is not disabled:\n%s", httpStatus)
			}
			if plan := asterisk(ctx, t, cfg, "dialplan show "+echoNumber+"@e2e"); !strings.Contains(plan, "Echo()") {
				t.Errorf("no echo extension:\n%s", plan)
			}
			return ctx
		}).
		Assess("the agent answers through its Service from the pod network", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			base := fmt.Sprintf("http://%s.%s.svc:%d", agentService, namespace, controlPort)
			if r := runFakephone(ctx, t, "readyz", nil, "http", "get", base+agentapi.PathReadyz); !r.Succeeded {
				t.Errorf("GET %s from a pod failed", agentapi.PathReadyz)
			}
			if r := runFakephone(ctx, t, "noauth", nil,
				"http", "get", "--expect-status", "401", base+agentapi.PathObservations); !r.Succeeded {
				t.Errorf("GET %s without the token was not refused with 401", agentapi.PathObservations)
			}
			token := []corev1.EnvVar{{Name: "AGENT_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: agentTokenSecret},
					Key:                  "token",
				},
			}}}
			r := runFakephone(ctx, t, "observations", token,
				"http", "get", "--header", "Authorization: Bearer $(AGENT_TOKEN)", base+agentapi.PathObservations)
			if !r.Succeeded {
				t.Fatalf("GET %s with the token failed", agentapi.PathObservations)
			}
			// The agent answers one line of JSON; it must be the wire type.
			var body string
			for line := range strings.SplitSeq(r.Logs, "\n") {
				if strings.HasPrefix(line, "{") {
					body = line
				}
			}
			var obs agentapi.Observations
			if err := json.Unmarshal([]byte(body), &obs); err != nil {
				t.Fatalf("observations are not an Observations document: %v\n%s", err, r.Logs)
			}
			if obs.Agent.StartedAt.IsZero() || obs.Devices == nil {
				t.Errorf("observations: %+v", obs)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestRegister registers fakephone, over TCP as the 79x5 does and over UDP,
// and checks that Asterisk holds the pod's contact.
func TestRegister(t *testing.T) {
	f := features.New("REGISTER")
	for _, transport := range []string{"tcp", "udp"} {
		f = f.Assess("over "+transport, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r := runFakephone(ctx, t, "register-"+transport, nil,
				"register", "--transport", transport, "--expires", "300")
			if !r.Succeeded {
				t.Fatalf("fakephone register over %s failed", transport)
			}
			want := fmt.Sprintf("sip:%s@%s:%d;transport=%s", sipUser, r.Pod.Status.PodIP, sipPort, transport)
			if r.Pod.Status.PodIP == "" || !strings.Contains(r.Logs, "contact=<"+want+">") {
				t.Fatalf("the registrar did not echo the contact %s", want)
			}
			// pjsip show contacts truncates long URIs; astdb holds the
			// contact in full.
			db := asterisk(ctx, t, cfg, "database show registrar")
			if !strings.Contains(db, `"uri":"`+want+`"`) {
				t.Errorf("Asterisk does not hold contact %s:\n%s", want, db)
			}
			if !strings.Contains(db, `"user_agent":"patchbay-fakephone"`) {
				t.Errorf("Asterisk did not record fakephone's User-Agent:\n%s", db)
			}
			if c := asterisk(ctx, t, cfg, "pjsip show contacts"); !strings.Contains(c, sipUser+"/sip:"+sipUser+"@"+r.Pod.Status.PodIP) {
				t.Errorf("pjsip show contacts:\n%s", c)
			}
			return ctx
		})
	}
	testenv.Test(t, f.Feature())
}

// TestEchoCall calls the echo test from a pod: SIP to the node's address,
// RTP between the pod network and the host network, and a 1 kHz tone that
// must come back.
func TestEchoCall(t *testing.T) {
	f := features.New("echo call").
		Assess("600 echoes a 1 kHz tone", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			r := runFakephone(ctx, t, "call-echo", nil, "call", echoNumber, "--duration", "3s")
			stats, ok := parseMedia(r.Logs)
			if !r.Succeeded || !ok {
				t.Fatalf("fakephone call %s failed", echoNumber)
			}
			// 3 s of 20 ms packets is 150; fakephone already insists on
			// 50 tone packets, so this restates the bar for the report.
			if stats.Sent != 150 || stats.ToneFrames < 50 {
				t.Errorf("media: %+v", stats)
			}
			if !strings.HasPrefix(stats.Source, h.nodeIP+":") {
				t.Errorf("RTP came back from %s, want the node %s", stats.Source, h.nodeIP)
			}
			t.Logf("echo media: sent %d, received %d, tone in %d, from %s", stats.Sent, stats.Received, stats.ToneFrames, stats.Source)
			return ctx
		}).
		Assess("an unrouted number is refused, and fakephone says so", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			r := runFakephone(ctx, t, "call-unrouted", nil, "call", "699", "--expect-tone=false")
			if r.Succeeded || !strings.Contains(r.Logs, "404") {
				t.Fatalf("calling an unrouted number: succeeded=%v, want a 404 failure", r.Succeeded)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// typedResources is a klient Resources that knows the patchbay types.
func typedResources(t *testing.T, cfg *envconf.Config) *resources.Resources {
	t.Helper()
	r, err := resources.New(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(r.GetScheme()); err != nil {
		t.Fatal(err)
	}
	return r
}

// moduleRunning reports whether `module show` lists name as Running.
func moduleRunning(moduleShow, name string) bool {
	for line := range strings.SplitSeq(moduleShow, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == name && strings.Contains(line, "Running") {
			return true
		}
	}
	return false
}
