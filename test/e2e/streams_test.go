//go:build e2e

package e2e

// The release B stream tests (docs/plans/release-b.md §6.4 I.6): the stream
// bridge as the chart runs it, against the fake stream sources
// (`fakephone source`: a fake HDHomeRun with two tuners and a 1.5 s lock,
// and a fake Icecast station), with the site Streams the harness writes at
// install (tuner-a 705 and tuner-b 707 on the fake tuner, whose pool
// "tuner" plays one at a time; radio-a 750 and radio-b 751 on the radio)
// and the e2e time limit of 20 s. Every call is a one-shot `fakephone call`
// as kitchen, which hears the stream's tone (G.711: the narrowband half of
// what the bridge sends at 16 kHz).
//
// TestStreamBridgeUp and TestStreamBridgeDown are sequential (Down stops
// the bridge); the rest run in parallel with release A's parallel tests.

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// streamBridgeSelector selects the bridge's pod.
const streamBridgeSelector = "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=stream-bridge"

// e2eCallLimit is test/e2e/values.yaml's streams.callLimit (TestStreamKit
// pins it).
const e2eCallLimit = 20 * time.Second

// The fake sources' stream paths (SourceStatus counts by path).
const (
	tunerAPath = "/auto/v5.1"
	tunerBPath = "/auto/v7.1"
)

// streamBridgePod returns the running bridge pod.
func streamBridgePod(ctx context.Context, t *testing.T) *corev1.Pod {
	t.Helper()
	list, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: streamBridgeSelector})
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
		t.Fatalf("want exactly one running stream bridge pod, found %d of %d", len(running), len(list.Items))
	}
	return &running[0]
}

// execInBridge runs a command in the bridge pod's container (Alpine: it
// has a shell and cat, unlike the distroless patchbay image) and returns
// its standard output.
func execInBridge(ctx context.Context, t *testing.T, cfg *envconf.Config, command ...string) string {
	const container = "stream-bridge"
	t.Helper()
	pod := streamBridgePod(ctx, t)
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

// scaleStreamBridge scales <fullname>-streams and waits: for 0, until its
// pods are gone; for 1, until it has rolled out and its pod is Ready.
func (h *harness) scaleStreamBridge(ctx context.Context, t *testing.T, replicas int) {
	t.Helper()
	if _, err := h.kubectl(ctx, "scale", "deployment/"+names.StreamBridge, "--namespace", namespace,
		fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := wait.PollUntilContextTimeout(ctx, time.Second, rolloutTimeout, true, func(ctx context.Context) (bool, error) {
		if replicas == 0 {
			pods, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: streamBridgeSelector})
			return err == nil && len(pods.Items) == 0, nil
		}
		d, err := h.cs.AppsV1().Deployments(namespace).Get(ctx, names.StreamBridge, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr // keep polling
		}
		done, _ := rolledOut(d)
		return done && d.Status.ReadyReplicas == int32(replicas), nil
	})
	if err != nil {
		t.Fatalf("scaling the stream bridge to %d: %v", replicas, err)
	}
	t.Logf("stream bridge scaled to %d in %s", replicas, time.Since(start).Round(100*time.Millisecond))
}

// watchFor polls every 500 ms until each condition has held at least
// once, or timeout, and returns the names of those that never did with
// their last reasons. Conditions that must hold while calls are up are
// checked this way: each one's window is short, and a sequence of waits
// could miss one while waiting on another.
func watchFor(ctx context.Context, timeout time.Duration, conds map[string]func(context.Context) (bool, string)) []string {
	seen := map[string]bool{}
	last := map[string]string{}
	deadline := time.Now().Add(timeout)
	for {
		for name, cond := range conds {
			if seen[name] {
				continue
			}
			ok, why := cond(ctx)
			seen[name], last[name] = ok, why
		}
		if len(seen) == len(conds) && !slices.Contains(mapValues(seen), false) {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			var missing []string
			for name := range conds {
				if !seen[name] {
					missing = append(missing, name+" (last: "+last[name]+")")
				}
			}
			slices.Sort(missing)
			return missing
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func mapValues(m map[string]bool) []bool {
	out := make([]bool, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// getStream reads one Stream.
func getStream(ctx context.Context, name string) (*v1alpha1.Stream, error) {
	s := &v1alpha1.Stream{}
	err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s)
	return s, err
}

// streamPlaying is a watchFor condition: Stream name is Playing (Ready
// True Playing) with listeners listeners.
func streamPlaying(name string, listeners int32) func(context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		s, err := getStream(ctx, name)
		if err != nil {
			return false, err.Error()
		}
		ok, why := streamReady(metav1.ConditionTrue, v1alpha1.ReasonPlaying, "Playing")(s)
		if ok && s.Status.Listeners != listeners {
			ok, why = false, fmt.Sprintf("%d listeners", s.Status.Listeners)
		}
		return ok, why
	}
}

// waitEvent waits up to timeout for an events.k8s.io Event with reason
// about Stream name since since, and returns its note.
func waitEvent(ctx context.Context, t *testing.T, since metav1.Time, reason, name string, timeout time.Duration) string {
	t.Helper()
	prefix := "Stream/" + name + ": "
	var note string
	var seen []string
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		seen = h.eventsSince(ctx, t, since, reason)
		for _, e := range seen {
			if n, ok := strings.CutPrefix(e, prefix); ok {
				note = n
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("no %s Event on Stream %s within %s (%s Events since: %v)", reason, name, timeout, reason, seen)
	}
	t.Logf("%s Event on Stream %s: %s", reason, name, note)
	return note
}

// waitFakeSource polls a fake source listener until ok holds, for up to
// timeout, and returns its status.
func waitFakeSource(ctx context.Context, t *testing.T, service string, port int, what string, timeout time.Duration,
	ok func(script.SourceStatus) bool,
) script.SourceStatus {
	t.Helper()
	start := time.Now()
	var st script.SourceStatus
	var last error
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		st, last = tryFakeSourceStatus(ctx, service, port)
		return last == nil && ok(st), nil
	})
	if err != nil {
		t.Fatalf("%s %s: not within %s (%+v, %v)", service, what, timeout, st, last)
	}
	t.Logf("%s %s after %s (%+v)", service, what, time.Since(start).Round(100*time.Millisecond), st)
	return st
}

// waitBridge polls the bridge's observations until ok holds, for up to
// timeout.
func waitBridge(ctx context.Context, t *testing.T, what string, timeout time.Duration,
	ok func(bridgeapi.Observations) (bool, string),
) bridgeapi.Observations {
	t.Helper()
	var obs bridgeapi.Observations
	why := "not read yet"
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		o, err := tryBridgeObservations(ctx)
		if err != nil {
			why = err.Error()
			return false, nil //nolint:nilerr // the bridge is starting: keep polling
		}
		obs = o
		var done bool
		done, why = ok(o)
		return done, nil
	})
	if err != nil {
		t.Fatalf("the stream bridge: %s: not within %s: %s", what, timeout, why)
	}
	return obs
}

// logCall logs how a call went: when its tone first came, how long it was
// up and who ended it.
func logCall(t *testing.T, number string, r fakephoneResult) (answered time.Duration, endedBy string) {
	t.Helper()
	first, _ := parseFirstTone(r.Logs)
	answered, endedBy, ok := parseCallEnd(r.Logs)
	if !ok {
		t.Errorf("the call to %s printed no answered=/endedBy=", number)
	}
	stats, _ := parseMedia(r.Logs)
	t.Logf("call %s: first tone after %s, %d tone packets, up %s, ended by %s", number, first, stats.ToneFrames, answered, endedBy)
	return answered, endedBy
}

// TestStreamBridgeUp checks the bridge as the chart runs it: one replica,
// Recreate, Ready, with no API token; PID 1 is patchbay as 65532 with no
// capabilities on a read-only root; it reports the ffmpeg it runs and the
// streams.json the controller compiled; and every site Stream is decided
// Idle, the tuner ones in the e2e pool.
func TestStreamBridgeUp(t *testing.T) {
	f := features.New("stream bridge").
		Assess("one replica, Recreate, Ready, no service account token, no restarts", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			d, err := h.cs.AppsV1().Deployments(namespace).Get(ctx, names.StreamBridge, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
				t.Errorf("Deployment %s: replicas %v, strategy %s", d.Name, d.Spec.Replicas, d.Spec.Strategy.Type)
			}
			pod := streamBridgePod(ctx, t)
			ready := false
			for _, c := range pod.Status.Conditions {
				ready = ready || (c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue)
			}
			if !ready {
				t.Errorf("stream bridge pod %s is not Ready", pod.Name)
			}
			if a := pod.Spec.AutomountServiceAccountToken; a == nil || *a {
				t.Errorf("the bridge pod automounts a service account token (%v)", a)
			}
			for _, c := range pod.Spec.Containers {
				for _, m := range c.VolumeMounts {
					if strings.HasPrefix(m.MountPath, "/var/run/secrets") {
						t.Errorf("container %s mounts %s at %s", c.Name, m.Name, m.MountPath)
					}
				}
			}
			for _, v := range pod.Spec.Volumes {
				if v.Projected != nil || v.Secret != nil {
					t.Errorf("the bridge pod has a Secret or projected volume %s", v.Name)
				}
			}
			reused := pod.CreationTimestamp.Time.Before(h.started)
			for _, s := range pod.Status.ContainerStatuses {
				if !s.Ready || s.State.Running == nil {
					t.Errorf("container %s: ready=%v state=%+v", s.Name, s.Ready, s.State)
				}
				switch {
				case s.RestartCount > 0 && reused:
					t.Logf("container %s restarted %d times, in a pod a previous run started", s.Name, s.RestartCount)
				case s.RestartCount > 0:
					t.Errorf("container %s restarted %d times", s.Name, s.RestartCount)
				}
			}
			return ctx
		}).
		Assess("PID 1 is patchbay as 65532 with no capabilities on a read-only root", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			st := procStatus(execInBridge(ctx, t, cfg, "cat", "/proc/1/status"))
			if st["Name"] != "patchbay" {
				t.Errorf("PID 1 is %q, not patchbay", st["Name"])
			}
			wantStatus(t, "stream bridge", st, map[string]string{
				"Uid": "65532 65532 65532 65532", "Gid": "65532 65532 65532 65532",
				"CapInh": capsNone, "CapPrm": capsNone, "CapEff": capsNone, "CapBnd": capsNone, "CapAmb": capsNone,
				"NoNewPrivs": "1",
			})
			opts := rootMountOptions(execInBridge(ctx, t, cfg, "cat", "/proc/1/mounts"))
			if !slices.Contains(opts, "ro") {
				t.Errorf("the bridge's root filesystem is mounted %v, want ro", opts)
			}
			return ctx
		}).
		Assess("reports its ffmpeg and plays the compiled streams.json", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			version, _, _ := strings.Cut(execInBridge(ctx, t, cfg, streambridge.DefaultFFmpeg, "-version"), "\n")
			version = strings.TrimSpace(version)
			if !strings.HasPrefix(version, "ffmpeg version ") {
				t.Errorf("ffmpeg -version printed %q", version)
			}
			start := time.Now()
			var compiled streams.Document
			obs := waitBridge(ctx, t, "the compiled streams loaded", 120*time.Second, func(o bridgeapi.Observations) (bool, string) {
				doc, err := install.ReadStreams(ctx, h.kube, namespace, names)
				if err != nil {
					return false, err.Error()
				}
				compiled = doc
				return o.Bridge.StreamsVersion == doc.Version && o.Bridge.StreamsError == "",
					fmt.Sprintf("the bridge plays %.12s (%q), compiled %.12s", o.Bridge.StreamsVersion, o.Bridge.StreamsError, doc.Version)
			})
			t.Logf("the bridge plays streams %.12s (%s); ffmpeg %q", obs.Bridge.StreamsVersion, time.Since(start).Round(time.Second), obs.Bridge.FFmpeg)
			if obs.Bridge.FFmpeg != version {
				t.Errorf("the bridge reports ffmpeg %q, the image runs %q", obs.Bridge.FFmpeg, version)
			}
			for _, s := range e2eStreams() {
				c, ok := compiled.Streams[streams.UUID(s.Name)]
				if !ok || c.Source != s.Spec.Source {
					t.Errorf("Stream %s is not compiled into %s as written: %+v", s.Name, names.StreamsConfigMap, c)
				}
			}
			if !slices.Equal(compiled.Pools, []streams.Pool{{Name: e2eTunerPool, Max: 1}}) {
				t.Errorf("compiled pools %+v, want the e2e values' %s (1)", compiled.Pools, e2eTunerPool)
			}
			return ctx
		}).
		Assess("every site Stream is Idle, the tuner ones in the pool", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			for _, s := range e2eStreams() {
				got := waitStream(ctx, t, s.Name, "Idle", 30*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
				want := ""
				if strings.HasPrefix(s.Name, "tuner-") {
					want = e2eTunerPool
				}
				if got.Status.TunerPool != want {
					t.Errorf("Stream %s: tunerPool %q, want %q", s.Name, got.Status.TunerPool, want)
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestStreamBridgeDown: with the bridge stopped, a stream call is still
// answered and plays "not available" (Asterisk's Dial finds no bridge and
// the dialplan continues), the Stream reads Unknown (BridgeUnreachable),
// and once the bridge is back the Stream is Idle again with the history
// its status kept. Not parallel: the other stream tests need the bridge.
func TestStreamBridgeDown(t *testing.T) {
	f := features.New("stream bridge down").
		Assess("a call plays the prompt, the Stream is Unknown, then Idle again", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			before := waitStream(ctx, t, radioAName, "Idle before", 30*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			h.scaleStreamBridge(ctx, t, 0)
			restored := false
			defer func() {
				if !restored {
					h.scaleStreamBridge(context.WithoutCancel(ctx), t, 1)
				}
			}()
			down := time.Now()
			call := streamCall{Number: radioANumber, Duration: 8 * time.Second, ExpectPrompt: true, EndedBy: "remote"}
			pod := startStreamCall(ctx, t, "call-750-down", call)
			waitStream(ctx, t, radioAName, "Unknown", 15*time.Second,
				streamReady(metav1.ConditionUnknown, v1alpha1.ReasonBridgeUnreachable, "Unknown"))
			t.Logf("radio-a Unknown %s after the bridge was gone", time.Since(down).Round(100*time.Millisecond))
			logCall(t, radioANumber, waitStreamCall(ctx, t, pod, call))

			h.scaleStreamBridge(ctx, t, 1)
			restored = true
			up := time.Now()
			after := waitStream(ctx, t, radioAName, "Idle again", 60*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			t.Logf("radio-a Idle %s after the bridge was back", time.Since(up).Round(100*time.Millisecond))
			if !equality.Semantic.DeepEqual(after.Status.LastPlayed, before.Status.LastPlayed) ||
				!equality.Semantic.DeepEqual(after.Status.LastFailure, before.Status.LastFailure) {
				t.Errorf("radio-a's history changed across the bridge restart: before %s; after %s", streamSummary(before), streamSummary(after))
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestStreamRadio: two stations play at once (no pool, no limit), each
// within 3 s; the Streams show one listener each while they play and Idle
// after; the fake radio's streams close within 10 s of the hang-ups (the
// bridge's 5 s grace); and a call left up is ended by the PBX at the time
// limit, having heard the station until then.
func TestStreamRadio(t *testing.T) {
	t.Parallel()
	f := features.New("internet radio").
		Assess("two stations at once, then Idle and closed", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			before := fakeSourceStatus(ctx, t, fakeRadioService, script.DefaultRadioPort)
			a := streamCall{Number: radioANumber, Duration: 10 * time.Second, ExpectHz: radioAHz, MinToneFrames: 200,
				FirstToneWithin: 3 * time.Second, EndedBy: "local"}
			b := streamCall{Number: radioBNumber, Duration: 10 * time.Second, ExpectHz: radioBHz, MinToneFrames: 200,
				FirstToneWithin: 3 * time.Second, EndedBy: "local"}
			pa := startStreamCall(ctx, t, "call-750", a)
			pb := startStreamCall(ctx, t, "call-751", b)
			missing := watchFor(ctx, 25*time.Second, map[string]func(context.Context) (bool, string){
				"the bridge plays both at once": func(ctx context.Context) (bool, string) {
					obs, err := tryBridgeObservations(ctx)
					if err != nil {
						return false, err.Error()
					}
					ra, rb := streamObservation(obs, radioAName), streamObservation(obs, radioBName)
					if ra == nil || rb == nil {
						return false, "a radio stream is not loaded"
					}
					return ra.Listeners == 1 && rb.Listeners == 1, fmt.Sprintf("listeners %d and %d", ra.Listeners, rb.Listeners)
				},
				"radio-a Playing, 1 listener": streamPlaying(radioAName, 1),
				"radio-b Playing, 1 listener": streamPlaying(radioBName, 1),
			})
			for _, m := range missing {
				t.Errorf("while both calls were up: never %s", m)
			}
			logCall(t, radioANumber, waitStreamCall(ctx, t, pa, a))
			logCall(t, radioBNumber, waitStreamCall(ctx, t, pb, b))
			after := waitFakeSource(ctx, t, fakeRadioService, script.DefaultRadioPort, "closed both stations", 10*time.Second, func(s script.SourceStatus) bool {
				return s.Open["/"+radioAName] == 0 && s.Open["/"+radioBName] == 0
			})
			for _, path := range []string{"/" + radioAName, "/" + radioBName} {
				if n := after.Opened[path] - before.Opened[path]; n != 1 {
					t.Errorf("the fake radio opened %s %d times for one call, want once", path, n)
				}
			}
			for _, name := range []string{radioAName, radioBName} {
				waitStream(ctx, t, name, "Idle after", 15*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			}
			return ctx
		}).
		Assess("the PBX ends a call at the time limit", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			long := streamCall{Number: radioBNumber, Duration: 2 * e2eCallLimit, ExpectHz: radioBHz,
				MinToneFrames: int((e2eCallLimit - 4*time.Second) / (20 * time.Millisecond)), FirstToneWithin: 3 * time.Second, EndedBy: "remote"}
			answered, by := logCall(t, radioBNumber, callStream(ctx, t, "call-751-long", long))
			if by != "remote" || answered < e2eCallLimit-2*time.Second || answered > e2eCallLimit+2*time.Second {
				t.Errorf("the call was up %s and ended by %s, want the PBX to end it at %s ± 2 s", answered, by, e2eCallLimit)
			}
			logBridgeResources(ctx, t, cfg, "after the radio tests")
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestStreamTuner is the tuner policy (§1.2) against the fake HDHomeRun,
// whose pool "tuner" plays one source at a time: two callers of one
// channel share one tuner and one session, which is freed within 10 s of
// the last hang-up; a second channel while one plays is refused ("not
// available", Busy, a CallRefused Event) without the tuner ever being
// asked; and with the DVR holding both tuners a call hears "not
// available" and the Stream is Failing with the tuner's 5XX, until a call
// plays it again.
func TestStreamTuner(t *testing.T) {
	t.Parallel()
	f := features.New("tuner").
		Assess("shared: two callers of 705 use one tuner", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			before := fakeSourceStatus(ctx, t, fakeTunerService, script.DefaultTunerPort)
			c := streamCall{Number: tunerANumber, Duration: 12 * time.Second, ExpectHz: tunerAHz, FirstToneWithin: 3 * time.Second, EndedBy: "local"}
			p1 := startStreamCall(ctx, t, "call-705-a", c)
			p2 := startStreamCall(ctx, t, "call-705-b", c)
			missing := watchFor(ctx, 25*time.Second, map[string]func(context.Context) (bool, string){
				"one session with 2 listeners in the pool, on one tuner": func(ctx context.Context) (bool, string) {
					obs, err := tryBridgeObservations(ctx)
					if err != nil {
						return false, err.Error()
					}
					pool := poolObservation(obs, e2eTunerPool)
					if pool == nil {
						return false, "no pool " + e2eTunerPool
					}
					if len(pool.Sessions) != 1 || pool.Sessions[0].Listeners != 2 {
						return false, fmt.Sprintf("sessions %+v", pool.Sessions)
					}
					st, err := tryFakeSourceStatus(ctx, fakeTunerService, script.DefaultTunerPort)
					if err != nil {
						return false, err.Error()
					}
					return st.OpenTotal() == 1 && st.Open[tunerAPath] == 1, fmt.Sprintf("fake tuner %+v", st)
				},
				"tuner-a Playing, 2 listeners": streamPlaying(tunerAName, 2),
			})
			for _, m := range missing {
				t.Errorf("while both calls were up: never %s", m)
			}
			logCall(t, tunerANumber, waitStreamCall(ctx, t, p1, c))
			logCall(t, tunerANumber, waitStreamCall(ctx, t, p2, c))
			after := waitFakeSource(ctx, t, fakeTunerService, script.DefaultTunerPort, "freed the tuner", 10*time.Second, func(s script.SourceStatus) bool {
				return s.OpenTotal() == 0
			})
			if n := after.Opened[tunerAPath] - before.Opened[tunerAPath]; n != 1 {
				t.Errorf("the fake tuner opened %s %d times for two callers, want once", tunerAPath, n)
			}
			if pool := poolObservation(bridgeObservations(ctx, t), e2eTunerPool); pool == nil || len(pool.Sessions) != 0 {
				t.Errorf("the bridge's pool %s after the tuner was freed: %+v", e2eTunerPool, pool)
			}
			waitStream(ctx, t, tunerAName, "Idle after", 15*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			return ctx
		}).
		Assess("limit: 707 while 705 plays is refused, Busy, and never reaches the tuner", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			before := fakeSourceStatus(ctx, t, fakeTunerService, script.DefaultTunerPort)
			since := metav1.Now()
			long := streamCall{Number: tunerANumber, Duration: 16 * time.Second, ExpectHz: tunerAHz, EndedBy: "local"}
			pa := startStreamCall(ctx, t, "call-705-long", long)
			waitBridge(ctx, t, "tuner-a playing", 20*time.Second, func(o bridgeapi.Observations) (bool, string) {
				s := streamObservation(o, tunerAName)
				if s == nil || s.Session == nil {
					return false, "no session"
				}
				return s.Session.State == bridgeapi.SessionPlaying, string(s.Session.State)
			})
			refused := streamCall{Number: tunerBNumber, Duration: 10 * time.Second, ExpectPrompt: true, EndedBy: "remote"}
			pb := startStreamCall(ctx, t, "call-707", refused)
			busy := waitStream(ctx, t, tunerBName, "Busy while 705 plays", 12*time.Second,
				streamReady(metav1.ConditionFalse, v1alpha1.ReasonTunerBusy, "Busy"))
			if c := meta.FindStatusCondition(busy.Status.Conditions, v1alpha1.ConditionReady); !strings.Contains(c.Message, tunerAName+" ("+tunerANumber+")") {
				t.Errorf("Busy does not name %s (%s): %s", tunerAName, tunerANumber, c.Message)
			}
			logCall(t, tunerBNumber, waitStreamCall(ctx, t, pb, refused))
			refusal := waitStream(ctx, t, tunerBName, "the refusal kept", 15*time.Second, func(s *v1alpha1.Stream) (bool, string) {
				f := s.Status.LastFailure
				if f == nil || f.Reason != v1alpha1.StreamFailureTunerBusy || f.At.Before(&since) {
					return false, "no TunerBusy failure since the test began"
				}
				return true, ""
			})
			if m := refusal.Status.LastFailure.Message; !strings.Contains(m, tunerAName+" ("+tunerANumber+")") {
				t.Errorf("the refusal does not name %s: %s", tunerAName, m)
			}
			if note := waitEvent(ctx, t, since, install.EventReasonCallRefused, tunerBName, 15*time.Second); !strings.Contains(note, tunerAName+" ("+tunerANumber+")") {
				t.Errorf("the CallRefused Event does not name %s: %s", tunerAName, note)
			}
			logCall(t, tunerANumber, waitStreamCall(ctx, t, pa, long))
			idle := waitStream(ctx, t, tunerBName, "Idle once 705 ended", 15*time.Second,
				streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			if f := idle.Status.LastFailure; f == nil || f.Reason != v1alpha1.StreamFailureTunerBusy {
				t.Errorf("tuner-b lost its refusal: %s", streamSummary(idle))
			}
			after := fakeSourceStatus(ctx, t, fakeTunerService, script.DefaultTunerPort)
			if after.Opened[tunerBPath] != before.Opened[tunerBPath] {
				t.Errorf("the fake tuner opened %s (%d times before, %d after): the pool did not refuse it first",
					tunerBPath, before.Opened[tunerBPath], after.Opened[tunerBPath])
			}
			return ctx
		}).
		Assess("refused: with the DVR on both tuners, 705 is Failing until it plays again", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			before := occupyTuners(ctx, t, 2)
			freed := false
			defer func() {
				if !freed {
					occupyTuners(context.WithoutCancel(ctx), t, 0)
				}
			}()
			since := metav1.Now()
			refused := streamCall{Number: tunerANumber, Duration: 10 * time.Second, ExpectPrompt: true, EndedBy: "remote"}
			logCall(t, tunerANumber, callStream(ctx, t, "call-705-refused", refused))
			failing := waitStream(ctx, t, tunerAName, "Failing", 20*time.Second,
				streamReady(metav1.ConditionFalse, v1alpha1.ReasonSourceFailed, "Failing"))
			// ffmpeg at -loglevel error names the class, not the code:
			// "Server returned 5XX Server Error reply" (docs/streams.md).
			if c := meta.FindStatusCondition(failing.Status.Conditions, v1alpha1.ConditionReady); !strings.Contains(c.Message, "5XX") {
				t.Errorf("Failing does not give the tuner's 5XX: %s", c.Message)
			}
			if f := failing.Status.LastFailure; f == nil || f.Reason != v1alpha1.StreamFailureSourceFailed || f.At.Before(&since) {
				t.Errorf("tuner-a's lastFailure: %s", streamSummary(failing))
			}
			if note := waitEvent(ctx, t, since, install.EventReasonSourceFailed, tunerAName, 15*time.Second); !strings.Contains(note, "5XX") {
				t.Errorf("the SourceFailed Event does not give the tuner's 5XX: %s", note)
			}
			if after := fakeSourceStatus(ctx, t, fakeTunerService, script.DefaultTunerPort); after.Refused <= before.Refused {
				t.Errorf("the fake tuner refused nothing (%d before, %d after)", before.Refused, after.Refused)
			}
			occupyTuners(ctx, t, 0)
			freed = true
			again := streamCall{Number: tunerANumber, Duration: 5 * time.Second, ExpectHz: tunerAHz, EndedBy: "local"}
			logCall(t, tunerANumber, callStream(ctx, t, "call-705-again", again))
			waitStream(ctx, t, tunerAName, "Idle, played after the failure", 20*time.Second, func(s *v1alpha1.Stream) (bool, string) {
				if ok, why := streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle")(s); !ok {
					return false, why
				}
				lp, lf := s.Status.LastPlayed, s.Status.LastFailure
				return lp != nil && lf != nil && lf.At.Before(lp), "lastPlayed is not later than lastFailure"
			})
			logBridgeResources(ctx, t, cfg, "after the tuner tests")
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestStreamAdded: a Stream created on a running install is Pending until
// the bridge has its streams.json (the kubelet's sync of the mounted
// ConfigMap, measured), then Idle and plays; once deleted it leaves
// <fullname>-streams, and its number plays "not available" when the agent
// has applied the bundle without it.
func TestStreamAdded(t *testing.T) {
	t.Parallel()
	const (
		addedName, addedNumber, addedHz = "added", "759", 600
	)
	added := &v1alpha1.Stream{
		ObjectMeta: metav1.ObjectMeta{Name: addedName, Namespace: namespace},
		Spec: v1alpha1.StreamSpec{Number: addedNumber, Name: "Added",
			Source: fmt.Sprintf("http://%s.%s.svc:%d/%s?%s=%d", fakeRadioService, namespace, script.DefaultRadioPort, addedName, script.SourceParamHz, addedHz)},
	}
	id := streams.UUID(addedName)
	f := features.New("a Stream added and deleted").
		Assess("Pending, then Idle, then plays", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.recreate(ctx, t, added.DeepCopy())
			start := time.Now()
			waitStream(ctx, t, addedName, "Pending", 30*time.Second, streamReady(metav1.ConditionFalse, v1alpha1.ReasonBridgePending, "Pending"))
			waitStream(ctx, t, addedName, "Idle", 150*time.Second, streamReady(metav1.ConditionTrue, v1alpha1.ReasonIdle, "Idle"))
			t.Logf("Stream %s Idle %s after it was created (the kubelet's sync of %s)", addedName, time.Since(start).Round(time.Second), names.StreamsConfigMap)
			c := streamCall{Number: addedNumber, Duration: 6 * time.Second, ExpectHz: addedHz, FirstToneWithin: 3 * time.Second, EndedBy: "local"}
			logCall(t, addedNumber, callStream(ctx, t, "call-759", c))
			return ctx
		}).
		Assess("deleted: gone from streams.json, then not available", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h.deleteAndWait(ctx, t, added.DeepCopy())
			why := ""
			err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				doc, err := install.ReadStreams(ctx, h.kube, namespace, names)
				if err != nil {
					why = err.Error()
					return false, nil //nolint:nilerr // keep polling
				}
				_, still := doc.Streams[id]
				b, err := h.readCompiled(ctx)
				if err != nil {
					why = err.Error()
					return false, nil //nolint:nilerr // between the two writes: keep polling
				}
				routed := strings.Contains(string(b.Files[compile.TreeAsterisk+"/extensions.conf"]), "exten => "+addedNumber+",")
				why = fmt.Sprintf("in streams.json: %t; routed in bundle %.12s: %t", still, b.Version, routed)
				return !still && !routed, nil
			})
			if err != nil {
				t.Fatalf("Stream %s is still compiled: %v (%s)", addedName, err, why)
			}
			b := h.waitApplied(ctx, t, 2*time.Minute)
			if strings.Contains(string(b.Files[compile.TreeAsterisk+"/extensions.conf"]), "exten => "+addedNumber+",") {
				t.Fatalf("the applied bundle %.12s still routes %s", b.Version, addedNumber)
			}
			c := streamCall{Number: addedNumber, Duration: 10 * time.Second, ExpectPrompt: true, EndedBy: "remote"}
			logCall(t, addedNumber, callStream(ctx, t, "call-759-gone", c))
			logBridgeResources(ctx, t, cfg, "after the added Stream")
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.deleteAndWait(ctx, t, added.DeepCopy())
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// logBridgeResources logs the bridge container's memory (now and its
// peak) and the CPU it has used, from its cgroup, beside the chart's
// requests (plan PK.4: 50m and 96Mi, a 512Mi limit). It never fails the
// test.
func logBridgeResources(ctx context.Context, t *testing.T, cfg *envconf.Config, when string) {
	t.Helper()
	list, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: streamBridgeSelector})
	if err != nil || len(list.Items) != 1 {
		t.Logf("bridge resources %s: no single bridge pod (%v)", when, err)
		return
	}
	pod := list.Items[0]
	r, err := resources.New(cfg.Client().RESTConfig())
	if err != nil {
		t.Logf("bridge resources %s: %v", when, err)
		return
	}
	var stdout, stderr bytes.Buffer
	cmd := []string{"cat", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.peak", "/sys/fs/cgroup/cpu.stat"}
	if err := r.ExecInPod(ctx, namespace, pod.Name, "stream-bridge", cmd, &stdout, &stderr); err != nil {
		t.Logf("bridge resources %s: %v: %s", when, err, stderr.String())
		return
	}
	lines := strings.Fields(stdout.String())
	if len(lines) < 4 {
		t.Logf("bridge resources %s: %q", when, stdout.String())
		return
	}
	mib := func(s string) string {
		var n float64
		if _, err := fmt.Sscan(s, &n); err != nil {
			return s
		}
		return fmt.Sprintf("%.1f MiB", n/(1<<20))
	}
	cpu := "?"
	for i := 2; i+1 < len(lines); i++ {
		if lines[i] == "usage_usec" {
			var us float64
			if _, err := fmt.Sscan(lines[i+1], &us); err == nil {
				up := time.Since(pod.CreationTimestamp.Time)
				cpu = fmt.Sprintf("%.1f s over %s (%.0fm average)", us/1e6, up.Round(time.Second), us/1e3/up.Seconds())
			}
		}
	}
	t.Logf("bridge resources %s: memory %s now, %s peak; CPU %s", when, mib(lines[0]), mib(lines[1]), cpu)
}
