package streamstate_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/streamstate"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

const (
	kingSource  = "http://192.168.0.58:5004/auto/v5.1"
	kiroSource  = "http://192.168.0.58:5004/auto/v7.1"
	foxSource   = "http://192.168.0.58:5004/auto/v13.1"
	radioSource = "https://ice2.somafm.com/groovesalad-128-mp3"
)

var names = install.NewNames("patchbay")

// testSettings are the house's: the stream bridge on, one tuner pool.
func testSettings() compile.Settings {
	return compile.Settings{
		PBXAddress:      "192.168.0.240",
		SIPPort:         5060,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  8088,
		NTPServer:       "192.168.20.1",
		Timezone:        "America/Los_Angeles",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{"192.168.20.0/24", "192.168.0.0/24"},
		AMISecret:       names.AMIRef(),
		StreamBridge:    install.StreamBridgeAddress(names, "telephony", bridgeapi.DefaultAudioSocketPort),
		Streams: compile.StreamSettings{TunerPools: []compile.TunerPool{
			{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1},
		}},
	}
}

func newStream(name, number, source string) *v1alpha1.Stream {
	return &v1alpha1.Stream{
		ObjectMeta: metav1.ObjectMeta{Namespace: "telephony", Name: name, Generation: 1},
		Spec:       v1alpha1.StreamSpec{Number: v1alpha1.Number(number), Name: strings.ToUpper(name), Source: source},
	}
}

// house is the compiled document: king5, kiro7, fox13 on the tuner,
// groovesalad on the radio.
func house(t *testing.T, edit ...func(d *streams.Document)) streams.Document {
	t.Helper()
	d := streams.Document{
		Pools: []streams.Pool{{Name: "hdhomerun", Max: 1}},
		Streams: map[string]streams.Stream{
			streams.UUID("king5"):       {Name: "king5", Number: "705", Source: kingSource, Pool: "hdhomerun"},
			streams.UUID("kiro7"):       {Name: "kiro7", Number: "707", Source: kiroSource, Pool: "hdhomerun"},
			streams.UUID("fox13"):       {Name: "fox13", Number: "713", Source: foxSource, Pool: "hdhomerun"},
			streams.UUID("groovesalad"): {Name: "groovesalad", Number: "750", Source: radioSource},
		},
	}
	for _, e := range edit {
		e(&d)
	}
	sealed, err := d.Sealed()
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// loaded is the bridge's observations of a document, nobody listening.
func loaded(d streams.Document) bridgeapi.Observations {
	o := bridgeapi.Observations{
		GeneratedAt: t0,
		Bridge:      bridgeapi.BridgeStatus{StartedAt: t0.Add(-time.Hour), StreamsVersion: d.Version},
		Streams:     []bridgeapi.StreamObservations{},
		Pools:       []bridgeapi.PoolObservations{},
	}
	for id, s := range d.Streams {
		o.Streams = append(o.Streams, bridgeapi.StreamObservations{UUID: id, Name: s.Name, Number: s.Number, Source: s.Source, Pool: s.Pool})
	}
	for _, p := range d.Pools {
		o.Pools = append(o.Pools, bridgeapi.PoolObservations{Name: p.Name, Max: p.Max, Sessions: []bridgeapi.PoolSession{}})
	}
	return o
}

// stream is the bridge's observation of a stream, to edit.
func stream(o *bridgeapi.Observations, name string) *bridgeapi.StreamObservations {
	for i := range o.Streams {
		if o.Streams[i].Name == name {
			return &o.Streams[i]
		}
	}
	panic("no stream " + name)
}

// session puts a session of the given source (with its streams) in the
// hdhomerun pool.
func session(o *bridgeapi.Observations, source string, state bridgeapi.SessionState, listeners int, names ...string) {
	for i := range o.Pools {
		if o.Pools[i].Name == "hdhomerun" {
			o.Pools[i].Sessions = append(o.Pools[i].Sessions, bridgeapi.PoolSession{
				Source: source, Streams: names, State: state, Listeners: listeners,
			})
		}
	}
}

// input is king5 (705) on a reachable bridge that has loaded the house's
// document, polled 5 s ago.
func input(t *testing.T) streamstate.Input {
	t.Helper()
	d := house(t)
	return streamstate.Input{
		Stream:   newStream("king5", "705", kingSource),
		Settings: testSettings(),
		Compiled: d,
		Snapshot: streamstate.Snapshot{
			Document: loaded(d), PolledAt: t0.Add(-5 * time.Second), Reachable: true, Polls: 3,
		},
		StaleAfter: install.StaleAfter,
		Now:        t0,
	}
}

func ptime(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

func at(t time.Time) *time.Time { return &t }

// TestDecide is the rule table, row by row, with the boundaries
// (docs/plans/release-b.md RT.5).
func TestDecide(t *testing.T) {
	const (
		conflictMsg = "number 705 is held by Stream a-radio, which is older"
		failMsg     = "ffmpeg exited with status 8: Server returned 5XX Server Error reply"
	)
	type want struct {
		state   string
		status  metav1.ConditionStatus
		reason  string
		message string
	}
	compiledV, playsV := house(t).Version[:12], house(t).Version[:12]
	for _, tc := range []struct {
		name string
		edit func(in *streamstate.Input)
		want want
	}{
		{"1 a number conflict", func(in *streamstate.Input) {
			in.Claim = &compile.ClaimProblem{NumberConflict: true, Message: conflictMsg}
		}, want{streamstate.StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonNumberConflict, conflictMsg}},
		{"1 out of the plan", func(in *streamstate.Input) {
			in.Stream.Spec.Number = "305"
			in.Claim = &compile.ClaimProblem{NumberOutOfPlan: true, Message: "number 305 is not in the Stream range 7XX"}
		}, want{streamstate.StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan, "number 305 is not in the Stream range 7XX"}},
		{"2 an rtsp source", func(in *streamstate.Input) { in.Stream.Spec.Source = "rtsp://192.168.0.58/auto/v5.1" },
			want{streamstate.StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonInvalidSource, `the source's scheme is "rtsp"; only http and https are read`}},
		{"2 a playlist, even with the bridge down", func(in *streamstate.Input) {
			in.Stream.Spec.Source = "https://somafm.com/groovesalad130.pls"
			in.Snapshot.Reachable = false
		}, want{streamstate.StateInvalid, metav1.ConditionFalse, v1alpha1.ReasonInvalidSource, "the source is a playlist (.pls or .m3u); use one of the stream URLs it lists"}},
		{"3 no stream bridge", func(in *streamstate.Input) { in.Settings.StreamBridge = ""; in.Snapshot = streamstate.Snapshot{} },
			want{streamstate.StateUnavailable, metav1.ConditionFalse, v1alpha1.ReasonNoStreamBridge, `streams are disabled (streamBridge.enabled is false): dialling 705 plays "not available"`}},
		{"4 unreachable", func(in *streamstate.Input) {
			in.Snapshot.Reachable = false
			in.Snapshot.LastError = "bridgeclient: Get \"http://patchbay-streams.telephony.svc:9093/v1/observations\": connection refused"
		}, want{streamstate.StateUnknown, metav1.ConditionUnknown, v1alpha1.ReasonBridgeUnreachable,
			"cannot reach the stream bridge: bridgeclient: Get \"http://patchbay-streams.telephony.svc:9093/v1/observations\": connection refused"}},
		{"4 never reached", func(in *streamstate.Input) {
			in.Snapshot = streamstate.Snapshot{Polls: 1, LastError: "bridgeclient: connection refused"}
		}, want{streamstate.StateUnknown, metav1.ConditionUnknown, v1alpha1.ReasonBridgeUnreachable, "cannot reach the stream bridge: bridgeclient: connection refused"}},
		{"4 stale past StaleAfter", func(in *streamstate.Input) { in.Snapshot.PolledAt = t0.Add(-install.StaleAfter - time.Second) },
			want{streamstate.StateUnknown, metav1.ConditionUnknown, v1alpha1.ReasonBridgeUnreachable, "cannot reach the stream bridge: its last observations are 31s old"}},
		{"4 not stale at exactly StaleAfter", func(in *streamstate.Input) { in.Snapshot.PolledAt = t0.Add(-install.StaleAfter) },
			want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"5 not compiled yet", func(in *streamstate.Input) {
			in.Compiled = house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("king5")) })
		}, want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			"this stream is not compiled yet (compiled streams " + house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("king5")) }).Version[:12] + ")"}},
		{"5 a new source not compiled yet", func(in *streamstate.Input) { in.Stream.Spec.Source = "http://192.168.0.58:5004/auto/v5.2" },
			want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending, "this stream is not compiled yet (compiled streams " + compiledV + ")"}},
		{"5 the bridge has not loaded it", func(in *streamstate.Input) {
			old := house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("king5")) })
			in.Snapshot.Document = loaded(old)
		}, want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			"the stream bridge has not loaded this stream yet (it plays streams " +
				house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("king5")) }).Version[:12] + ", compiled " + compiledV + ")"}},
		{"5 the bridge has its old source", func(in *streamstate.Input) {
			stream(&in.Snapshot.Document, "king5").Source = "http://192.168.0.58:5004/auto/v5.9"
		}, want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			"the stream bridge has not loaded this stream yet (it plays streams " + playsV + ", compiled " + compiledV + ")"}},
		{"5 the bridge has another pool", func(in *streamstate.Input) { stream(&in.Snapshot.Document, "king5").Pool = "" },
			want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
				"the stream bridge has not loaded this stream yet (it plays streams " + playsV + ", compiled " + compiledV + ")"}},
		{"5 the bridge has loaded nothing", func(in *streamstate.Input) {
			in.Snapshot.Document = bridgeapi.Observations{Streams: []bridgeapi.StreamObservations{}, Pools: []bridgeapi.PoolObservations{}}
		}, want{streamstate.StatePending, metav1.ConditionFalse, v1alpha1.ReasonBridgePending,
			"the stream bridge has not loaded this stream yet (it plays streams (none), compiled " + compiledV + ")"}},
		{"6 two listeners", func(in *streamstate.Input) {
			stream(&in.Snapshot.Document, "king5").Listeners = 2
			session(&in.Snapshot.Document, kingSource, bridgeapi.SessionPlaying, 2, "king5")
		}, want{streamstate.StatePlaying, metav1.ConditionTrue, v1alpha1.ReasonPlaying, "2 listeners; tuner pool hdhomerun"}},
		{"6 one listener on the radio", func(in *streamstate.Input) {
			in.Stream = newStream("groovesalad", "750", radioSource)
			stream(&in.Snapshot.Document, "groovesalad").Listeners = 1
		}, want{streamstate.StatePlaying, metav1.ConditionTrue, v1alpha1.ReasonPlaying, "1 listener"}},
		{"6 playing beats an older failure", func(in *streamstate.Input) {
			o := stream(&in.Snapshot.Document, "king5")
			o.Listeners = 1
			o.LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute), Reason: bridgeapi.FailureSourceFailed, Message: failMsg}
		}, want{streamstate.StatePlaying, metav1.ConditionTrue, v1alpha1.ReasonPlaying, "1 listener; tuner pool hdhomerun"}},
		{"7 failed, never played", func(in *streamstate.Input) {
			stream(&in.Snapshot.Document, "king5").LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute), Reason: bridgeapi.FailureSourceFailed, Message: failMsg}
		}, want{streamstate.StateFailing, metav1.ConditionFalse, v1alpha1.ReasonSourceFailed, "2026-10-16T11:59:00Z: " + failMsg}},
		{"7 ended after it last played", func(in *streamstate.Input) {
			o := stream(&in.Snapshot.Document, "king5")
			o.LastPlayed = at(t0.Add(-2 * time.Minute))
			o.LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute), Reason: bridgeapi.FailureSourceEnded, Message: "no audio for 10s"}
		}, want{streamstate.StateFailing, metav1.ConditionFalse, v1alpha1.ReasonSourceFailed, "2026-10-16T11:59:00Z: no audio for 10s"}},
		{"7 a failure kept in status, the bridge restarted", func(in *streamstate.Input) {
			in.Stream.Status.LastFailure = &v1alpha1.StreamFailure{At: metav1.NewTime(t0.Add(-time.Hour)), Reason: v1alpha1.StreamFailureSourceFailed, Message: failMsg}
		}, want{streamstate.StateFailing, metav1.ConditionFalse, v1alpha1.ReasonSourceFailed, "2026-10-16T11:00:00Z: " + failMsg}},
		{"7 not failing: the failure is as old as the last play", func(in *streamstate.Input) {
			o := stream(&in.Snapshot.Document, "king5")
			o.LastPlayed = at(t0.Add(-time.Minute))
			o.LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute), Reason: bridgeapi.FailureSourceFailed, Message: failMsg}
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening; last played 2026-10-16T11:59:00Z"}},
		{"7 not failing: it played since", func(in *streamstate.Input) {
			o := stream(&in.Snapshot.Document, "king5")
			o.LastPlayed = at(t0.Add(-30 * time.Second))
			in.Stream.Status.LastFailure = &v1alpha1.StreamFailure{At: metav1.NewTime(t0.Add(-time.Minute)), Reason: v1alpha1.StreamFailureSourceFailed}
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening; last played 2026-10-16T11:59:30Z"}},
		{"7 not failing: a refusal is history", func(in *streamstate.Input) {
			stream(&in.Snapshot.Document, "king5").LastFailure = &bridgeapi.Failure{
				At: t0.Add(-time.Minute), Reason: bridgeapi.FailureTunerBusy, Message: "pool hdhomerun (1 at a time) is playing kiro7 (707)"}
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"8 busy", func(in *streamstate.Input) {
			stream(&in.Snapshot.Document, "kiro7").Listeners = 1
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionPlaying, 1, "kiro7")
		}, want{streamstate.StateBusy, metav1.ConditionFalse, v1alpha1.ReasonTunerBusy, "pool hdhomerun (1 at a time) is playing kiro7 (707)"}},
		{"8 busy beats an older failure", func(in *streamstate.Input) {
			o := stream(&in.Snapshot.Document, "king5")
			o.LastPlayed = at(t0.Add(-time.Minute))
			o.LastFailure = &bridgeapi.Failure{At: t0.Add(-2 * time.Minute), Reason: bridgeapi.FailureSourceFailed}
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionStarting, 1, "kiro7")
		}, want{streamstate.StateBusy, metav1.ConditionFalse, v1alpha1.ReasonTunerBusy, "pool hdhomerun (1 at a time) is playing kiro7 (707)"}},
		{"8 busy at a max of 2, holders sorted", func(in *streamstate.Input) {
			in.Compiled = house(t, func(d *streams.Document) { d.Pools[0].Max = 2 })
			in.Snapshot.Document = loaded(in.Compiled)
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionPlaying, 1, "kiro7")
			session(&in.Snapshot.Document, foxSource, bridgeapi.SessionPlaying, 3, "fox13", "fox13-hd")
		}, want{streamstate.StateBusy, metav1.ConditionFalse, v1alpha1.ReasonTunerBusy, "pool hdhomerun (2 at a time) is playing fox13 (713), fox13-hd, kiro7 (707)"}},
		{"8 not busy: its own source holds the tuner", func(in *streamstate.Input) {
			session(&in.Snapshot.Document, kingSource, bridgeapi.SessionPlaying, 1, "king5", "king5-again")
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"8 not busy: a draining session gives way", func(in *streamstate.Input) {
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionDraining, 0, "kiro7")
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"8 not busy: below max", func(in *streamstate.Input) {
			in.Compiled = house(t, func(d *streams.Document) { d.Pools[0].Max = 2 })
			in.Snapshot.Document = loaded(in.Compiled)
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionPlaying, 1, "kiro7")
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"8 a radio is never busy", func(in *streamstate.Input) {
			in.Stream = newStream("groovesalad", "750", radioSource)
			session(&in.Snapshot.Document, kiroSource, bridgeapi.SessionPlaying, 1, "kiro7")
		}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
		{"9 idle", func(*streamstate.Input) {}, want{streamstate.StateIdle, metav1.ConditionTrue, v1alpha1.ReasonIdle, "nobody is listening"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input(t)
			tc.edit(&in)
			v := streamstate.Decide(in)
			if v.Wait {
				t.Fatal("Wait")
			}
			if v.State != tc.want.state || v.Ready.Type != v1alpha1.ConditionReady || v.Ready.Status != tc.want.status ||
				v.Ready.Reason != tc.want.reason || v.Ready.Message != tc.want.message {
				t.Errorf("%s, Ready %s %s %q;\nwant %s, Ready %s %s %q", v.State, v.Ready.Status, v.Ready.Reason, v.Ready.Message,
					tc.want.state, tc.want.status, tc.want.reason, tc.want.message)
			}
			wantConflict := metav1.ConditionFalse
			if in.Claim != nil && in.Claim.NumberConflict {
				wantConflict = metav1.ConditionTrue
			}
			if v.NumberConflict.Type != v1alpha1.ConditionNumberConflict || v.NumberConflict.Status != wantConflict {
				t.Errorf("NumberConflict %+v, want %s", v.NumberConflict, wantConflict)
			}
			changes := 0
			for _, e := range v.Events {
				if e.Reason != install.EventReasonStateChanged {
					continue
				}
				changes++
				if e.Type != corev1.EventTypeNormal || e.Action != install.EventActionDecide || e.Note != "None -> "+tc.want.state+": "+tc.want.message {
					t.Errorf("StateChanged %+v", e)
				}
			}
			if changes != 1 {
				t.Errorf("events %+v, want one StateChanged", v.Events)
			}
		})
	}
}

// TestDecideWaits: until the poller's first attempt ends, a Stream that
// rows 1-3 do not settle gets no status.
func TestDecideWaits(t *testing.T) {
	in := input(t)
	in.Snapshot = streamstate.Snapshot{}
	if v := streamstate.Decide(in); !v.Wait {
		t.Errorf("before the first poll: %+v, want Wait", v)
	}
	in.Stream.Spec.Source = "rtsp://x/y"
	if v := streamstate.Decide(in); v.Wait || v.State != streamstate.StateInvalid {
		t.Errorf("an invalid source before the first poll: %+v, want Invalid", v)
	}
}

// TestDecideFolds: rows 5-9 carry the observation into the status fields
// (listeners, the compiled pool, the later lastPlayed); rows 1-4 keep the
// Stream's own.
func TestDecideFolds(t *testing.T) {
	in := input(t)
	in.Stream.Status = v1alpha1.StreamStatus{
		State: streamstate.StatePlaying, Listeners: 3, TunerPool: "old-pool",
		LastPlayed: ptime(t0.Add(-time.Hour)),
	}
	o := stream(&in.Snapshot.Document, "king5")
	o.Listeners = 2
	o.LastPlayed = at(t0.Add(-time.Minute + 300*time.Millisecond))
	v := streamstate.Decide(in)
	if v.Listeners != 2 || v.TunerPool != "hdhomerun" || v.LastPlayed == nil || !v.LastPlayed.Time.Equal(t0.Add(-time.Minute)) {
		t.Errorf("folded %d listeners, pool %q, last played %v; want 2, hdhomerun, %s (to the second)", v.Listeners, v.TunerPool, v.LastPlayed, t0.Add(-time.Minute))
	}
	if len(v.Events) != 0 {
		t.Errorf("Playing to Playing emitted %+v", v.Events)
	}

	// An older lastPlayed from the bridge never moves status back.
	o.LastPlayed = at(t0.Add(-2 * time.Hour))
	if v := streamstate.Decide(in); !v.LastPlayed.Time.Equal(t0.Add(-time.Hour)) {
		t.Errorf("last played %v, want status's %s", v.LastPlayed, t0.Add(-time.Hour))
	}

	// Unknown keeps everything as it was.
	in.Snapshot.Reachable = false
	v = streamstate.Decide(in)
	if v.State != streamstate.StateUnknown || v.Listeners != 3 || v.TunerPool != "old-pool" || !v.LastPlayed.Time.Equal(t0.Add(-time.Hour)) {
		t.Errorf("Unknown: %d listeners, pool %q, last played %v; want status's 3, old-pool, %s", v.Listeners, v.TunerPool, v.LastPlayed, t0.Add(-time.Hour))
	}

	// The bridge without this stream (Pending): no listeners.
	in.Snapshot.Reachable = true
	in.Snapshot.Document = loaded(house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("king5")) }))
	if v := streamstate.Decide(in); v.State != streamstate.StatePending || v.Listeners != 0 || v.TunerPool != "hdhomerun" {
		t.Errorf("Pending: %s, %d listeners, pool %q", v.State, v.Listeners, v.TunerPool)
	}

	// A radio has no pool.
	in = input(t)
	in.Stream = newStream("groovesalad", "750", radioSource)
	in.Stream.Status.TunerPool = "hdhomerun"
	if v := streamstate.Decide(in); v.TunerPool != "" {
		t.Errorf("a radio's pool %q, want none", v.TunerPool)
	}
}

// TestFold: a failure is copied into status once (its time to the second,
// as status keeps it), with one Event; an older or equal observation, as
// after a bridge restart, changes nothing.
func TestFold(t *testing.T) {
	in := input(t)
	o := stream(&in.Snapshot.Document, "king5")
	o.LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Minute + 700*time.Millisecond), Reason: bridgeapi.FailureSourceFailed,
		Message: "ffmpeg exited with status 8: Server returned 5XX Server Error reply"}
	v := streamstate.Decide(in)
	want := v1alpha1.StreamFailure{At: metav1.NewTime(t0.Add(-time.Minute)), Reason: v1alpha1.StreamFailureSourceFailed, Message: o.LastFailure.Message}
	if v.LastFailure == nil || !v.LastFailure.At.Equal(&want.At) || v.LastFailure.Reason != want.Reason || v.LastFailure.Message != want.Message {
		t.Fatalf("lastFailure %+v, want %+v", v.LastFailure, want)
	}
	if !hasEvent(v.Events, corev1.EventTypeWarning, install.EventReasonSourceFailed, "SourceFailed: "+want.Message) {
		t.Errorf("events %+v, want a SourceFailed Warning", v.Events)
	}

	// Written, read back (as the API server returns it), judged again on
	// the same observation: nothing new.
	in.Stream.Status = roundTrip(t, statusOf(v))
	again := streamstate.Decide(in)
	if len(again.Events) != 0 || !again.LastFailure.At.Equal(&want.At) {
		t.Errorf("the same failure again: events %+v, lastFailure %+v; want none and the same", again.Events, again.LastFailure)
	}

	// The same second, another message: still the same failure.
	o.LastFailure.Message = "something else"
	if v := streamstate.Decide(in); len(v.Events) != 0 || v.LastFailure.Message != want.Message {
		t.Errorf("an equal time: events %+v, message %q", v.Events, v.LastFailure.Message)
	}

	// The bridge restarted: it remembers an older failure, or none, and
	// nothing it played. Status stays.
	in.Stream.Status.LastPlayed = ptime(t0.Add(-30 * time.Second))
	o.LastFailure = &bridgeapi.Failure{At: t0.Add(-time.Hour), Reason: bridgeapi.FailureSourceEnded, Message: "old"}
	o.LastPlayed = nil
	in.Snapshot.Document.Bridge.StartedAt = t0.Add(-time.Second)
	v = streamstate.Decide(in)
	if len(failures(v.Events)) != 0 || v.LastFailure.Message != want.Message || !v.LastPlayed.Time.Equal(t0.Add(-30*time.Second)) || v.State != streamstate.StateIdle {
		t.Errorf("after a bridge restart: %s, events %+v, lastFailure %+v, lastPlayed %v; want Idle and status unchanged",
			v.State, v.Events, v.LastFailure, v.LastPlayed)
	}
	o.LastFailure = nil
	if v := streamstate.Decide(in); v.LastFailure == nil || v.LastFailure.Message != want.Message {
		t.Errorf("no failure on the bridge: lastFailure %+v, want status's", v.LastFailure)
	}

	// A refusal: CallRefused, Normal, the bridge's message.
	busy := "pool hdhomerun (1 at a time) is playing kiro7 (707)"
	o.LastFailure = &bridgeapi.Failure{At: t0, Reason: bridgeapi.FailureTunerBusy, Message: busy}
	v = streamstate.Decide(in)
	if !hasEvent(v.Events, corev1.EventTypeNormal, install.EventReasonCallRefused, busy) || v.LastFailure.Reason != v1alpha1.StreamFailureTunerBusy {
		t.Errorf("a refusal: events %+v, lastFailure %+v; want CallRefused", v.Events, v.LastFailure)
	}

	// SourceEnded: SourceFailed, Warning.
	o.LastFailure = &bridgeapi.Failure{At: t0.Add(time.Second), Reason: bridgeapi.FailureSourceEnded, Message: "no audio for 10s"}
	if v := streamstate.Decide(in); !hasEvent(v.Events, corev1.EventTypeWarning, install.EventReasonSourceFailed, "SourceEnded: no audio for 10s") {
		t.Errorf("SourceEnded: events %+v", v.Events)
	}

	// A reason this controller does not know (a newer bridge) is never
	// written: the CRD's enum would refuse the status.
	o.LastFailure = &bridgeapi.Failure{At: t0.Add(time.Second), Reason: "Melted", Message: "x"}
	if v := streamstate.Decide(in); len(failures(v.Events)) != 0 || v.LastFailure.Reason == "Melted" {
		t.Errorf("an unknown reason: events %+v, lastFailure %+v", v.Events, v.LastFailure)
	}

	// A message longer than the CRD allows is cut, printable.
	o.LastFailure = &bridgeapi.Failure{At: t0.Add(2 * time.Second), Reason: bridgeapi.FailureSourceFailed,
		Message: "\x1b[31m" + strings.Repeat("é", 3000)}
	v = streamstate.Decide(in)
	if len(v.LastFailure.Message) > 2048 || strings.ContainsRune(v.LastFailure.Message, 0x1b) {
		t.Errorf("message of %d bytes, holding an escape: %t", len(v.LastFailure.Message), strings.ContainsRune(v.LastFailure.Message, 0x1b))
	}
	for _, e := range v.Events {
		if len(e.Note) > install.MaxEventNote {
			t.Errorf("an Event note of %d bytes", len(e.Note))
		}
	}
	if len(v.Ready.Message) > install.MaxStatusMessage {
		t.Errorf("a Ready message of %d bytes", len(v.Ready.Message))
	}
}

// failures are the Events that are not StateChanged.
func failures(evs []streamstate.Event) []streamstate.Event {
	var out []streamstate.Event
	for _, e := range evs {
		if e.Reason != install.EventReasonStateChanged {
			out = append(out, e)
		}
	}
	return out
}

func hasEvent(evs []streamstate.Event, typ, reason, note string) bool {
	for _, e := range evs {
		if e.Type == typ && e.Reason == reason && e.Note == note && e.Action == install.EventActionPlay {
			return true
		}
	}
	return false
}

func statusOf(v streamstate.Verdict) v1alpha1.StreamStatus {
	return v1alpha1.StreamStatus{State: v.State, Listeners: v.Listeners, TunerPool: v.TunerPool, LastPlayed: v.LastPlayed, LastFailure: v.LastFailure}
}

// roundTrip is a status as the API server hands it back: through JSON.
func roundTrip(t *testing.T, s v1alpha1.StreamStatus) v1alpha1.StreamStatus {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out v1alpha1.StreamStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRefusalKeepsSourceFailure: a TunerBusy refusal never replaces a
// source failure that no call has played past. FOX 13's channel is gone
// (404, Failing); a call to 713 is refused while KING 5 holds the tuner;
// once the pool frees up, FOX 13 is still Failing with its 404, not Idle
// (its source was never tried again), and no CallRefused Event is sent
// (the bridge keeps the source failure too; refused_calls_total counts
// the refusal).
func TestRefusalKeepsSourceFailure(t *testing.T) {
	in := input(t)
	in.Stream = newStream("fox13", "713", foxSource)
	gone := v1alpha1.StreamFailure{
		At: metav1.NewTime(t0.Add(-10 * time.Minute)), Reason: v1alpha1.StreamFailureSourceFailed,
		Message: "ffmpeg exited with status 8: Server returned 404 Not Found",
	}
	in.Stream.Status = v1alpha1.StreamStatus{State: streamstate.StateFailing, TunerPool: "hdhomerun", LastFailure: &gone}
	refused := func(o *bridgeapi.Observations) {
		stream(o, "fox13").LastFailure = &bridgeapi.Failure{
			At: t0.Add(-time.Minute), Reason: bridgeapi.FailureTunerBusy, Message: "pool hdhomerun (1 at a time) is playing king5 (705)",
		}
	}
	for name, busy := range map[string]bool{"the pool still full": true, "the pool free again": false} {
		t.Run(name, func(t *testing.T) {
			in := in
			in.Snapshot.Document = loaded(in.Compiled)
			refused(&in.Snapshot.Document)
			if busy {
				session(&in.Snapshot.Document, kingSource, bridgeapi.SessionPlaying, 1, "king5")
			}
			v := streamstate.Decide(in)
			if v.State != streamstate.StateFailing || v.Ready.Reason != v1alpha1.ReasonSourceFailed ||
				v.LastFailure == nil || v.LastFailure.Reason != v1alpha1.StreamFailureSourceFailed || !strings.Contains(v.Ready.Message, "404") {
				t.Errorf("%s (%s: %s), lastFailure %+v; want Failing with the 404 kept", v.State, v.Ready.Reason, v.Ready.Message, v.LastFailure)
			}
			if evs := failures(v.Events); len(evs) != 0 {
				t.Errorf("events %+v, want none for a refusal of a failing stream", evs)
			}
		})
	}

	// Once a call has played it since, a refusal is its newest failure.
	refused(&in.Snapshot.Document)
	in.Stream.Status.LastPlayed = ptime(t0.Add(-5 * time.Minute))
	in.Stream.Status.State = streamstate.StateIdle
	v := streamstate.Decide(in)
	if v.LastFailure == nil || v.LastFailure.Reason != v1alpha1.StreamFailureTunerBusy || v.State != streamstate.StateIdle ||
		!hasEvent(v.Events, corev1.EventTypeNormal, install.EventReasonCallRefused, "pool hdhomerun (1 at a time) is playing king5 (705)") {
		t.Errorf("after a play: %s, lastFailure %+v, events %+v; want Idle, TunerBusy and CallRefused", v.State, v.LastFailure, v.Events)
	}
}
