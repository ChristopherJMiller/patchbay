package streamstate_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/streamstate"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// TestStreamStatusEnvtest runs Setup in a manager against a real API
// server, with a compiled <fullname>-streams and a fake bridge whose
// document takes each shape in turn: Idle, Playing, Busy, Failing,
// Unknown and Pending are written, the conditions carry the generation,
// and the CRD (its enum, its bounds) refuses nothing the controller
// writes; the SourceFailed and CallRefused Events reach the API server.
func TestStreamStatusEnvtest(t *testing.T) {
	ctrltest.Need(t, env)
	ctx := context.Background()
	namespace := ctrltest.Namespace(t, env)
	for _, s := range []*v1alpha1.Stream{
		newStream("king5", "705", kingSource), newStream("kiro7", "707", kiroSource), newStream("groovesalad", "750", radioSource),
	} {
		s.Namespace, s.Generation = namespace, 0
		if err := env.Client.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	d := house(t, func(d *streams.Document) { delete(d.Streams, streams.UUID("fox13")) })
	cm, err := install.StreamsConfigMap(names, namespace, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	b := &bridge{}
	b.set(loaded(d), nil)

	mgr := ctrltest.NewManager(t, env, namespace)
	if err := streamstate.Setup(mgr, streamstate.Config{
		Client: mgr.GetClient(), Namespace: namespace, Names: names, Settings: testSettings(), Bridge: b,
		Interval: 100 * time.Millisecond, Recorder: mgr.GetEventRecorder(install.EventRecorderName),
	}); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)

	waitFor := func(name, state, reason string, check func(*v1alpha1.Stream) bool) *v1alpha1.Stream {
		t.Helper()
		var s v1alpha1.Stream
		err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			if err := env.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s); err != nil {
				return false, err
			}
			return s.Status.State == state && ready(&s).Reason == reason && (check == nil || check(&s)), nil
		})
		if err != nil {
			t.Fatalf("%s never became %s (%s): %+v", name, state, reason, s.Status)
		}
		for _, c := range s.Status.Conditions {
			if c.ObservedGeneration != s.Generation {
				t.Errorf("%s: condition %s at generation %d, want %d", name, c.Type, c.ObservedGeneration, s.Generation)
			}
		}
		if s.Status.ObservedGeneration != s.Generation {
			t.Errorf("%s: observedGeneration %d, want %d", name, s.Status.ObservedGeneration, s.Generation)
		}
		return &s
	}

	for _, n := range []string{"king5", "kiro7", "groovesalad"} {
		waitFor(n, streamstate.StateIdle, v1alpha1.ReasonIdle, nil)
	}

	// king5 plays to two callers: Playing, and kiro7 on the same tuner Busy;
	// kiro7's refused call is in its lastFailure.
	busy := "pool hdhomerun (1 at a time) is playing king5 (705)"
	b.edit(func(o *bridgeapi.Observations) {
		k := stream(o, "king5")
		k.Listeners, k.LastPlayed = 2, at(time.Now())
		session(o, kingSource, bridgeapi.SessionPlaying, 2, "king5")
		stream(o, "kiro7").LastFailure = &bridgeapi.Failure{At: time.Now(), Reason: bridgeapi.FailureTunerBusy, Message: busy}
	})
	waitFor("king5", streamstate.StatePlaying, v1alpha1.ReasonPlaying, func(s *v1alpha1.Stream) bool {
		return s.Status.Listeners == 2 && s.Status.TunerPool == "hdhomerun" && s.Status.LastPlayed != nil
	})
	waitFor("kiro7", streamstate.StateBusy, v1alpha1.ReasonTunerBusy, func(s *v1alpha1.Stream) bool {
		return ready(s).Message == busy && s.Status.LastFailure != nil && s.Status.LastFailure.Reason == v1alpha1.StreamFailureTunerBusy
	})

	// The tuner fails under king5: Failing; kiro7 is Idle again, its
	// refusal kept.
	failed := "ffmpeg exited with status 8: Server returned 5XX Server Error reply " + strings.Repeat("x", 600)
	b.edit(func(o *bridgeapi.Observations) {
		k := stream(o, "king5")
		k.Listeners = 0
		k.LastFailure = &bridgeapi.Failure{At: time.Now().Add(2 * time.Second), Reason: bridgeapi.FailureSourceFailed, Message: failed}
		o.Pools[0].Sessions = []bridgeapi.PoolSession{}
	})
	waitFor("king5", streamstate.StateFailing, v1alpha1.ReasonSourceFailed, func(s *v1alpha1.Stream) bool {
		return s.Status.Listeners == 0 && s.Status.LastFailure != nil && s.Status.LastFailure.Message == failed
	})
	waitFor("kiro7", streamstate.StateIdle, v1alpha1.ReasonIdle, func(s *v1alpha1.Stream) bool {
		return s.Status.LastFailure != nil && s.Status.LastFailure.Reason == v1alpha1.StreamFailureTunerBusy
	})

	// The bridge goes away: every Stream Unknown, its history kept.
	b.set(bridgeapi.Observations{}, context.DeadlineExceeded)
	for _, n := range []string{"king5", "kiro7", "groovesalad"} {
		waitFor(n, streamstate.StateUnknown, v1alpha1.ReasonBridgeUnreachable, func(s *v1alpha1.Stream) bool {
			return ready(s).Status == metav1.ConditionUnknown
		})
	}
	if s := waitFor("king5", streamstate.StateUnknown, v1alpha1.ReasonBridgeUnreachable, nil); s.Status.LastFailure == nil || s.Status.LastPlayed == nil {
		t.Errorf("king5 lost its history: %+v", s.Status)
	}

	// It comes back from a restart, with a streams.json that lacks
	// groovesalad: groovesalad Pending, king5 still Failing (its failure
	// is in status), kiro7 Idle.
	restarted := loaded(house(t, func(d *streams.Document) {
		delete(d.Streams, streams.UUID("fox13"))
		delete(d.Streams, streams.UUID("groovesalad"))
	}))
	b.set(restarted, nil)
	waitFor("groovesalad", streamstate.StatePending, v1alpha1.ReasonBridgePending, nil)
	waitFor("king5", streamstate.StateFailing, v1alpha1.ReasonSourceFailed, nil)
	waitFor("kiro7", streamstate.StateIdle, v1alpha1.ReasonIdle, nil)

	// A new generation is judged and recorded.
	var g v1alpha1.Stream
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "groovesalad"}, &g); err != nil {
		t.Fatal(err)
	}
	g.Spec.Name = "Groove Salad Classic"
	if err := env.Client.Update(ctx, &g); err != nil {
		t.Fatal(err)
	}
	waitFor("groovesalad", streamstate.StatePending, v1alpha1.ReasonBridgePending, func(s *v1alpha1.Stream) bool { return s.Status.ObservedGeneration == 2 })

	// The failure Events reached the API server, once each.
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var list corev1.EventList
		if err := env.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		var got []string
		for _, e := range list.Items {
			if e.Reason == install.EventReasonSourceFailed || e.Reason == install.EventReasonCallRefused {
				got = append(got, e.InvolvedObject.Name+" "+e.Type+" "+e.Reason)
			}
		}
		slices.Sort(got)
		return slices.Equal(got, []string{"king5 Warning SourceFailed", "kiro7 Normal CallRefused"}), nil
	})
	if err != nil {
		t.Errorf("the failure Events: %v", err)
	}
}
