//go:build e2e

package e2e

// The release B harness kit (docs/plans/release-b.md §6.3 PK.6): the
// stream bridge's image, the fake stream sources (`fakephone source`, a
// fake HDHomeRun and a fake Icecast station in the cluster), and helpers to
// read them and the bridge, wait for a Stream and call one. The stream
// tests themselves are the integrator's (§6.4); nothing here runs alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// streamBridgeImage is the bridge's image as `make e2e-images` builds it
// (the Makefile's E2E_STREAM_BRIDGE_IMG); test/e2e/values.yaml runs it by
// this tag, never pulled.
const streamBridgeImage = "ghcr.io/christopherjmiller/patchbay-stream-bridge:e2e"

// The fake sources: one Deployment of the tools image running `fakephone
// source` with its defaults (2 tuners, a 1.5 s lock), behind two Services.
const (
	fakeSourcesName  = "fake-sources"
	fakeTunerService = "fake-tuner"
	fakeRadioService = "fake-radio"
	// fakeSourcesLabel selects the fake sources' pod (not fakephoneSelector:
	// the harness deletes those pods at install).
	fakeSourcesLabel = "app.kubernetes.io/name"
)

// fakeTunerHost is the fake tuner's in-cluster host as the site Streams'
// sources write it, and as test/e2e/values.yaml's tuner pool lists it.
const fakeTunerHost = fakeTunerService + "." + namespace + ".svc"

// fakeSourcesObjects are the fake sources' Deployment and Services.
func fakeSourcesObjects() (*appsv1.Deployment, []*corev1.Service) {
	labels := map[string]string{fakeSourcesLabel: fakeSourcesName}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: fakeSourcesName, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			// One process holds the tuners: never two during a rollout.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr.To(false),
					EnableServiceLinks:            ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To[int64](2),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "source",
						Image:           toolsImage,
						ImagePullPolicy: corev1.PullNever,
						Args:            []string{"source"},
						Ports: []corev1.ContainerPort{
							{Name: "tuner", ContainerPort: script.DefaultTunerPort, Protocol: corev1.ProtocolTCP},
							{Name: "radio", ContainerPort: script.DefaultRadioPort, Protocol: corev1.ProtocolTCP},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: script.SourcePathStatus, Port: intstr.FromString("tuner"),
							}},
							PeriodSeconds: 2,
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	svc := func(name, port string, number int32) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports: []corev1.ServicePort{{
					Name: port, Port: number, TargetPort: intstr.FromString(port), Protocol: corev1.ProtocolTCP,
				}},
			},
		}
	}
	return d, []*corev1.Service{
		svc(fakeTunerService, "tuner", script.DefaultTunerPort),
		svc(fakeRadioService, "radio", script.DefaultRadioPort),
	}
}

// deployFakeSources creates the fake sources (Deployment fake-sources,
// Services fake-tuner on DefaultTunerPort and fake-radio on
// DefaultRadioPort), or puts an existing one's spec back: a reused cluster
// keeps the last run's. It does not wait; fakeSourcesReady does.
func deployFakeSources(ctx context.Context) error {
	d, svcs := fakeSourcesObjects()
	deps := h.cs.AppsV1().Deployments(namespace)
	cur, err := deps.Get(ctx, d.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := deps.Create(ctx, d, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating Deployment %s: %w", d.Name, err)
		}
	case err != nil:
		return err
	default:
		cur.Labels, cur.Spec = d.Labels, d.Spec
		if _, err := deps.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating Deployment %s: %w", d.Name, err)
		}
	}
	services := h.cs.CoreV1().Services(namespace)
	for _, s := range svcs {
		cur, err := services.Get(ctx, s.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if _, err := services.Create(ctx, s, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("creating Service %s: %w", s.Name, err)
			}
		case err != nil:
			return err
		default:
			// The cluster IP stays the Service's own.
			cur.Labels, cur.Spec.Selector, cur.Spec.Ports = s.Labels, s.Spec.Selector, s.Spec.Ports
			if _, err := services.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("updating Service %s: %w", s.Name, err)
			}
		}
	}
	return nil
}

// fakeSourcesReady waits until the fake sources' Deployment has rolled out
// and its pod is ready.
func fakeSourcesReady(ctx context.Context, timeout time.Duration) error {
	why := "not read yet"
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		d, err := h.cs.AppsV1().Deployments(namespace).Get(ctx, fakeSourcesName, metav1.GetOptions{})
		if err != nil {
			why = err.Error()
			return false, nil //nolint:nilerr // not created or not readable yet: keep polling
		}
		var ok bool
		ok, why = rolledOut(d)
		return ok, nil
	})
	if err != nil {
		return fmt.Errorf("the fake sources did not roll out within %s: %s (%w)", timeout, why, err)
	}
	return nil
}

// proxyGet GETs path from a Service's port through the API server
// (services/proxy), returning the status and body.
func proxyGet(ctx context.Context, service string, port int, path string) (int, []byte, error) {
	return proxyStatus(h.cs.CoreV1().Services(namespace).ProxyGet("http", service, strconv.Itoa(port), path, nil).DoRaw(ctx))
}

// fakeSourceStatus reads one fake source listener's status (fake-tuner on
// DefaultTunerPort, fake-radio on DefaultRadioPort) through services/proxy.
func fakeSourceStatus(ctx context.Context, t *testing.T, service string, port int) script.SourceStatus {
	t.Helper()
	st, err := tryFakeSourceStatus(ctx, service, port)
	if err != nil {
		t.Fatalf("the fake source %s: %v", service, err)
	}
	return st
}

// tryFakeSourceStatus is fakeSourceStatus for any goroutine.
func tryFakeSourceStatus(ctx context.Context, service string, port int) (script.SourceStatus, error) {
	var st script.SourceStatus
	code, body, err := proxyGet(ctx, service, port, script.SourcePathStatus)
	if err != nil {
		return st, err
	}
	if code != http.StatusOK {
		return st, fmt.Errorf("GET %s: %d %s", script.SourcePathStatus, code, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("its status: %w: %s", err, body)
	}
	return st, nil
}

// occupyTuners has the fake tuner hold n tuners as a DVR recording would
// (0 frees them) and returns its status. While open streams hold the
// tuners asked for (a bridge session in its grace), the fake refuses (409),
// and this asks again every second for up to 20 s.
func occupyTuners(ctx context.Context, t *testing.T, n int) script.SourceStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body, err := proxyStatus(h.cs.CoreV1().RESTClient().Post().
			Namespace(namespace).Resource("services").
			Name("http:"+fakeTunerService+":"+strconv.Itoa(script.DefaultTunerPort)).
			SubResource("proxy").Suffix(script.SourcePathOccupy).
			Param(script.SourceParamTuners, strconv.Itoa(n)).
			DoRaw(ctx))
		if err != nil {
			t.Fatalf("occupying %d tuners: %v", n, err)
		}
		var st script.SourceStatus
		if code == http.StatusOK || code == http.StatusConflict {
			if err := json.Unmarshal(body, &st); err != nil {
				t.Fatalf("occupying %d tuners: %v: %s", n, err, body)
			}
		}
		switch {
		case code == http.StatusOK:
			t.Logf("fake tuner: %d tuners occupied (%+v)", n, st)
			return st
		case code == http.StatusConflict && time.Now().Before(deadline):
			t.Logf("fake tuner: %d tuners are not free yet (%+v); asking again", n, st)
		default:
			t.Fatalf("occupying %d tuners: %d %s", n, code, strings.TrimSpace(string(body)))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("occupying %d tuners: %v", n, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// bridgeObservations reads the stream bridge's observations
// (<fullname>-streams:DefaultControlPort, PathObservations) through
// services/proxy, which needs the bridge Ready.
func bridgeObservations(ctx context.Context, t *testing.T) bridgeapi.Observations {
	t.Helper()
	obs, err := tryBridgeObservations(ctx)
	if err != nil {
		t.Fatalf("the stream bridge's observations: %v", err)
	}
	return obs
}

// tryBridgeObservations is bridgeObservations for any goroutine.
func tryBridgeObservations(ctx context.Context) (bridgeapi.Observations, error) {
	var obs bridgeapi.Observations
	code, body, err := proxyGet(ctx, names.StreamBridge, bridgeapi.DefaultControlPort, bridgeapi.PathObservations)
	if err != nil {
		return obs, err
	}
	if code != http.StatusOK {
		return obs, fmt.Errorf("GET %s: %d %s", bridgeapi.PathObservations, code, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &obs); err != nil {
		return obs, fmt.Errorf("decoding: %w: %s", err, body)
	}
	return obs, nil
}

// streamObservation is one stream's entry in obs, by name, or nil.
func streamObservation(obs bridgeapi.Observations, name string) *bridgeapi.StreamObservations {
	for i := range obs.Streams {
		if obs.Streams[i].Name == name {
			return &obs.Streams[i]
		}
	}
	return nil
}

// poolObservation is one pool's entry in obs, by name, or nil.
func poolObservation(obs bridgeapi.Observations, name string) *bridgeapi.PoolObservations {
	for i := range obs.Pools {
		if obs.Pools[i].Name == name {
			return &obs.Pools[i]
		}
	}
	return nil
}

// waitStream polls Stream name every second until ok holds, for up to
// timeout, and returns it; on a timeout the test fails with ok's last
// reason and the Stream's status.
func waitStream(ctx context.Context, t *testing.T, name, what string, timeout time.Duration,
	ok func(*v1alpha1.Stream) (bool, string),
) *v1alpha1.Stream {
	t.Helper()
	var (
		s   *v1alpha1.Stream
		why = "not read yet"
	)
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		cur := &v1alpha1.Stream{}
		if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cur); err != nil {
			why = err.Error()
			return false, nil //nolint:nilerr // not created or not readable yet: keep polling
		}
		s = cur
		done, reason := ok(cur)
		why = reason
		return done, nil
	})
	if err != nil {
		t.Fatalf("Stream %s: %s: not within %s: %s\n%s", name, what, timeout, why, streamSummary(s))
	}
	t.Logf("Stream %s: %s (%s)", name, what, streamSummary(s))
	return s
}

// streamReady is a waitStream condition: Ready has status and reason (and,
// when state is set, status.state is it).
func streamReady(status metav1.ConditionStatus, reason, state string) func(*v1alpha1.Stream) (bool, string) {
	return func(s *v1alpha1.Stream) (bool, string) {
		c := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady)
		if c == nil {
			return false, "no Ready condition"
		}
		if c.ObservedGeneration != s.Generation {
			return false, fmt.Sprintf("Ready is for generation %d, the Stream is at %d", c.ObservedGeneration, s.Generation)
		}
		if c.Status != status || c.Reason != reason || (state != "" && s.Status.State != state) {
			return false, fmt.Sprintf("state %s, Ready %s %s: %s", s.Status.State, c.Status, c.Reason, c.Message)
		}
		return true, ""
	}
}

// streamSummary is one line of a Stream's status, for logs.
func streamSummary(s *v1alpha1.Stream) string {
	if s == nil {
		return "(no Stream)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): state %s, %d listeners", s.Name, s.Spec.Number, s.Status.State, s.Status.Listeners)
	if s.Status.TunerPool != "" {
		fmt.Fprintf(&b, ", pool %s", s.Status.TunerPool)
	}
	if c := meta.FindStatusCondition(s.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		fmt.Fprintf(&b, ", Ready %s %s: %s", c.Status, c.Reason, c.Message)
	}
	if lp := s.Status.LastPlayed; lp != nil {
		fmt.Fprintf(&b, ", last played %s", lp.UTC().Format(time.RFC3339))
	}
	if lf := s.Status.LastFailure; lf != nil {
		fmt.Fprintf(&b, ", last failure %s %s: %s", lf.At.UTC().Format(time.RFC3339), lf.Reason, lf.Message)
	}
	return b.String()
}

// streamCall is one one-shot call to a stream: what the caller expects.
type streamCall struct {
	// Number is dialled; Duration is how long the caller stays (the PBX
	// may end the call first).
	Number   string
	Duration time.Duration
	// ExpectHz, when set, is the stream's tone, which at least
	// MinToneFrames 20 ms packets (default 50) must carry.
	ExpectHz      float64
	MinToneFrames int
	// FirstToneWithin, when set, is how soon after the first packet sent
	// the tone must come.
	FirstToneWithin time.Duration
	// ExpectPrompt: the call plays "not available" (≥ 2 s not silent, of
	// which ≥ 1 s is not the SIT tone), as TestPrompts checks.
	ExpectPrompt bool
	// EndedBy, when set, is who must end the call: "remote" (the PBX: the
	// time limit, a refusal) or "local" (the caller, after Duration).
	EndedBy string
}

// callerHz is the tone the stream callers send: a multiple of 50 Hz away
// from every stream tone and from the SIT tone.
const callerHz = 300

// args is the call's `fakephone call` command line.
func (c streamCall) args() []string {
	args := []string{
		"call", c.Number,
		"--duration", c.Duration.String(),
		// The command's deadline: the call, the setup and the hang-up.
		"--timeout", (c.Duration + 30*time.Second).String(),
		"--tone-hz", strconv.Itoa(callerHz),
	}
	switch {
	case c.ExpectPrompt:
		args = append(args, "--expect-tone=false", "--expect-audio", "2s", "--min-voice", "1s")
	case c.ExpectHz != 0:
		frames := c.MinToneFrames
		if frames == 0 {
			frames = 50
		}
		args = append(args, "--expect-hz", strconv.FormatFloat(c.ExpectHz, 'f', -1, 64),
			"--min-tone-frames", strconv.Itoa(frames))
	default:
		args = append(args, "--expect-tone=false")
	}
	if c.FirstToneWithin > 0 {
		args = append(args, "--expect-first-tone-within", c.FirstToneWithin.String())
	}
	if c.EndedBy != "" {
		args = append(args, "--expect-ended-by", c.EndedBy)
	}
	return args
}

// startStreamCall starts a one-shot call as Phone kitchen, sending
// callerHz, and returns its tools pod (pod names it: "call-705-a"). Start
// several, then wait for each with waitStreamCall, for calls at once.
func startStreamCall(ctx context.Context, t *testing.T, pod string, c streamCall) string {
	t.Helper()
	return startTool(ctx, t, toolPod{name: pod, args: c.args(), env: kitchenEnv()})
}

// waitStreamCall waits for a call startStreamCall started; a call that
// failed what it expected fails the test (its pod is kept).
func waitStreamCall(ctx context.Context, t *testing.T, name string, c streamCall) fakephoneResult {
	t.Helper()
	r := waitTool(ctx, t, name, true)
	if !r.Succeeded {
		t.Errorf("the call to %s (%s) failed", c.Number, name)
	}
	return r
}

// callStream is one one-shot call to a stream as Phone kitchen, sending
// callerHz: startStreamCall, then waitStreamCall.
func callStream(ctx context.Context, t *testing.T, pod string, c streamCall) fakephoneResult {
	t.Helper()
	return waitStreamCall(ctx, t, startStreamCall(ctx, t, pod, c), c)
}

// callEndLine is the end of fakephone call's media line (after voice=N):
// how long the call was up, and which side ended it.
var callEndLine = regexp.MustCompile(`answered=(\S+) endedBy=(local|remote)\b`)

// parseCallEnd finds how long a call was up and who ended it in
// fakephone's output.
func parseCallEnd(logs string) (answered time.Duration, endedBy string, ok bool) {
	m := callEndLine.FindStringSubmatch(logs)
	if m == nil {
		return 0, "", false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return 0, "", false
	}
	return d, m[2], true
}

// firstToneLine is the media line's firstTone: how soon the tone came.
var firstToneLine = regexp.MustCompile(`firstTone=(\S+) `)

// parseFirstTone finds how soon after the first packet sent the expected
// tone first came (0: never).
func parseFirstTone(logs string) (time.Duration, bool) {
	m := firstToneLine.FindStringSubmatch(logs)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	return d, err == nil
}

// The site Streams (docs/plans/release-b.md §6.4 I.5): two on the fake
// tuner (its pool, test/e2e/values.yaml's "tuner"), two on the fake radio
// (no pool). Each plays a tone of its own, a multiple of 50 Hz away from
// the others, callerHz and the SIT tone.
const (
	tunerAName, tunerANumber, tunerAHz = "tuner-a", "705", 1100
	tunerBName, tunerBNumber, tunerBHz = "tuner-b", "707", 1300
	radioAName, radioANumber, radioAHz = "radio-a", "750", 500
	radioBName, radioBNumber, radioBHz = "radio-b", "751", 700
	// e2eTunerPool is test/e2e/values.yaml's tuner pool.
	e2eTunerPool = "tuner"
)

// e2eStreams are the site Streams, as the harness writes them.
func e2eStreams() []v1alpha1.Stream {
	tuner := func(channel string, hz int) string {
		return fmt.Sprintf("http://%s:%d/auto/v%s?%s=%d", fakeTunerHost, script.DefaultTunerPort, channel, script.SourceParamHz, hz)
	}
	radio := func(mount string, hz int) string {
		return fmt.Sprintf("http://%s.%s.svc:%d/%s?%s=%d", fakeRadioService, namespace, script.DefaultRadioPort, mount, script.SourceParamHz, hz)
	}
	stream := func(name, number, display, source string) v1alpha1.Stream {
		return v1alpha1.Stream{
			TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Stream"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       v1alpha1.StreamSpec{Number: v1alpha1.Number(number), Name: display, Source: source},
		}
	}
	return []v1alpha1.Stream{
		stream(tunerAName, tunerANumber, "Tuner A", tuner("5.1", tunerAHz)),
		stream(tunerBName, tunerBNumber, "Tuner B", tuner("7.1", tunerBHz)),
		stream(radioAName, radioANumber, "Radio A", radio("radio-a", radioAHz)),
		stream(radioBName, radioBNumber, "Radio B", radio("radio-b", radioBHz)),
	}
}

// proxyStatus reads a services/proxy answer (DoRaw's): its status and
// body. A status the backend or the proxy answered (4xx, 5xx: client-go
// makes those a StatusError carrying the code) is not an error; failing to
// reach the API server is.
func proxyStatus(body []byte, err error) (int, []byte, error) {
	if err == nil {
		return http.StatusOK, body, nil
	}
	var se apierrors.APIStatus
	if errors.As(err, &se) && se.Status().Code != 0 {
		return int(se.Status().Code), body, nil
	}
	return 0, body, err
}

// TestStreamKit checks the kit against itself and the e2e values, without
// the cluster: the site Streams' sources are ones the CRD and the bridge
// accept, the tuner ones are on test/e2e/values.yaml's tuner pool's host
// and the radio ones on no pool's, every tone is its own; the bridge image
// is the values' tag, never pulled; the time limit is the 20 s the stream
// tests time; and a call's command line and media line read as fakephone
// writes them.
func TestStreamKit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(h.root, "test", "e2e", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		StreamBridge struct {
			Enabled *bool `json:"enabled"`
			Image   struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				PullPolicy string `json:"pullPolicy"`
			} `json:"image"`
		} `json:"streamBridge"`
		Streams struct {
			CallLimit  string `json:"callLimit"`
			TunerPools []struct {
				Name  string   `json:"name"`
				Hosts []string `json:"hosts"`
				Max   int      `json:"max"`
			} `json:"tunerPools"`
		} `json:"streams"`
	}
	// Only these keys: the rest are release A's.
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	img := values.StreamBridge.Image
	repo, tag, _ := strings.Cut(streamBridgeImage, ":")
	if (img.Repository != "" && img.Repository != repo) || img.Tag != tag || img.PullPolicy != string(corev1.PullNever) {
		t.Errorf("values streamBridge.image %+v, want %s (Never pulled)", img, streamBridgeImage)
	}
	if e := values.StreamBridge.Enabled; e != nil && !*e {
		t.Error("values turn the stream bridge off")
	}
	if values.Streams.CallLimit != "20s" {
		t.Errorf("values streams.callLimit %q, want 20s (TestStreamRadio times it)", values.Streams.CallLimit)
	}
	pools := values.Streams.TunerPools
	if len(pools) != 1 || pools[0].Name != e2eTunerPool || pools[0].Max != 1 || !slices.Equal(pools[0].Hosts, []string{fakeTunerHost}) {
		t.Errorf("values streams.tunerPools %+v, want [{%s [%s] 1}]", pools, e2eTunerPool, fakeTunerHost)
	}

	numbers, tones := map[string]bool{}, map[int]bool{callerHz: true}
	for _, f := range []int{950, 1400, 1800} { // the SIT tone
		tones[f] = true
	}
	for _, s := range e2eStreams() {
		if err := streams.CheckSource(s.Spec.Source); err != nil {
			t.Errorf("Stream %s: %v", s.Name, err)
		}
		u, err := url.Parse(s.Spec.Source)
		if err != nil {
			t.Fatal(err)
		}
		inPool := u.Hostname() == fakeTunerHost
		if wantPool := strings.HasPrefix(s.Name, "tuner-"); inPool != wantPool {
			t.Errorf("Stream %s on %s: in the tuner pool %t, want %t", s.Name, u.Hostname(), inPool, wantPool)
		}
		if !strings.HasPrefix(string(s.Spec.Number), "7") || len(s.Spec.Number) != 3 || numbers[string(s.Spec.Number)] {
			t.Errorf("Stream %s's number %s is not a 7XX of its own", s.Name, s.Spec.Number)
		}
		numbers[string(s.Spec.Number)] = true
		hz, err := strconv.Atoi(u.Query().Get(script.SourceParamHz))
		if err != nil || hz%50 != 0 || tones[hz] {
			t.Errorf("Stream %s's tone %q is not a multiple of 50 Hz of its own", s.Name, u.Query().Get(script.SourceParamHz))
		}
		tones[hz] = true
	}

	args := streamCall{Number: "705", Duration: 8 * time.Second, ExpectHz: 1100, FirstToneWithin: 3 * time.Second, EndedBy: "local"}.args()
	want := []string{"call", "705", "--duration", "8s", "--timeout", "38s", "--tone-hz", "300",
		"--expect-hz", "1100", "--min-tone-frames", "50", "--expect-first-tone-within", "3s", "--expect-ended-by", "local"}
	if !slices.Equal(args, want) {
		t.Errorf("a tuner call runs\n  %v\nwant\n  %v", args, want)
	}
	if args := (streamCall{Number: "707", Duration: 6 * time.Second, ExpectPrompt: true, EndedBy: "remote"}).args(); !slices.Contains(args, "--expect-audio") ||
		!slices.Contains(args, "--expect-tone=false") || slices.Contains(args, "--expect-hz") {
		t.Errorf("a refused call runs %v", args)
	}

	// fakephone call's media line, as it writes it (test/e2e/fakephone
	// TestCallEndedByRemote pins the format there).
	line := "call 751: sent=1000 received=1001 pcmu=1001 toneFrames=990 maxRatio=0.998 firstTone=1.62s source=172.18.0.2:10010 loud=1000 voice=1000 answered=20.004s endedBy=remote\n"
	stats, ok := parseMedia(line)
	answered, by, endOK := parseCallEnd(line)
	first, firstOK := parseFirstTone(line)
	if !ok || stats.ToneFrames != 990 || !endOK || answered != 20004*time.Millisecond || by != "remote" || !firstOK || first != 1620*time.Millisecond {
		t.Errorf("the media line read as %+v (%t), answered %s by %s (%t), first tone %s (%t)", stats, ok, answered, by, endOK, first, firstOK)
	}
}
