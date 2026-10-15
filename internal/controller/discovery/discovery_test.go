package discovery_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/discovery"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

const ns = "telephony"

var (
	names   = install.NewNames("pbx")
	started = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
)

// recorder keeps every Event with the object it regards.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	name := ""
	if o, ok := regarding.(client.Object); ok {
		name = fmt.Sprintf("%T %s/%s", o, o.GetNamespace(), o.GetName())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s %s %s [%s] %s", eventType, reason, action, name, fmt.Sprintf(note, args...)))
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

var _ events.EventRecorder = (*recorder)(nil)

func pbxConfig() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: ns}}
}

func snapshot(startedAt time.Time, unknown ...agentapi.UnknownDevice) agentview.Snapshot {
	doc := agentapi.Empty(startedAt.Add(time.Minute), agentapi.AgentStatus{StartedAt: startedAt, AsteriskUp: true})
	doc.UnknownDevices = unknown
	return agentview.Snapshot{Document: doc, PolledAt: startedAt.Add(time.Minute), AgentReachable: true, Polls: 1}
}

func unknown(mac, source string) agentapi.UnknownDevice {
	return agentapi.UnknownDevice{MAC: mac, File: "SEP" + mac + ".cnf.xml", Source: source, FirstSeen: started, LastSeen: started}
}

func event(mac, source string) string {
	return "Normal UnknownPhone Discover [*v1.ConfigMap telephony/pbx-pbx-config] unknown phone " + mac + " requested config from " + source
}

func newReporter(t *testing.T, c client.Client) (*discovery.Reporter, *recorder, *strings.Builder) {
	t.Helper()
	rec := &recorder{}
	var logs strings.Builder
	var mu sync.Mutex
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		logs.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})
	return discovery.New(discovery.Config{Reader: c, Namespace: ns, Names: names, Recorder: rec, Log: log}), rec, &logs
}

func fakeClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
}

// TestOncePerMAC: each unknown MAC is one Event on pbx-config, with design
// §5's note; the same snapshot again reports nothing; a MAC listed twice
// (or in another case) is one phone.
func TestOncePerMAC(t *testing.T) {
	r, rec, _ := newReporter(t, fakeClient(pbxConfig()))
	ctx := context.Background()
	s := snapshot(started,
		unknown("0200E2E1ABCD", "192.168.20.55"),
		unknown("0200E2E1BEEF", "192.168.20.56"),
		unknown("0200e2e1abcd", "192.168.20.57"),
		agentapi.UnknownDevice{})
	r.Report(ctx, s)
	if got, want := rec.take(), []string{event("0200E2E1ABCD", "192.168.20.55"), event("0200E2E1BEEF", "192.168.20.56")}; !slices.Equal(got, want) {
		t.Errorf("events:\n got %q\nwant %q", got, want)
	}
	r.Report(ctx, s)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("the same snapshot again: events %q; want none", got)
	}
	// A new MAC on a later poll is reported; the old ones are not.
	r.Report(ctx, snapshot(started, unknown("0200E2E1ABCD", "192.168.20.55"), unknown("0200E2E1F00D", "192.168.20.60")))
	if got, want := rec.take(), []string{event("0200E2E1F00D", "192.168.20.60")}; !slices.Equal(got, want) {
		t.Errorf("a new MAC: events %q; want %q", got, want)
	}
}

// TestNewAgentProcess: an agent restart (a new StartedAt) forgets what was
// reported, so a phone still asking is reported again.
func TestNewAgentProcess(t *testing.T) {
	r, rec, _ := newReporter(t, fakeClient(pbxConfig()))
	ctx := context.Background()
	r.Report(ctx, snapshot(started, unknown("0200E2E1ABCD", "192.168.20.55")))
	rec.take()
	later := started.Add(time.Hour)
	r.Report(ctx, snapshot(later, unknown("0200E2E1ABCD", "192.168.20.58")))
	if got, want := rec.take(), []string{event("0200E2E1ABCD", "192.168.20.58")}; !slices.Equal(got, want) {
		t.Errorf("after an agent restart: events %q; want %q", got, want)
	}
	r.Report(ctx, snapshot(later, unknown("0200E2E1ABCD", "192.168.20.58")))
	if got := rec.take(); len(got) != 0 {
		t.Errorf("again: events %q; want none", got)
	}
}

// TestNoConfigMap: before pbx-config exists nothing is reported and
// nothing is marked; once it exists, the phone is reported.
func TestNoConfigMap(t *testing.T) {
	c := fakeClient()
	r, rec, logs := newReporter(t, c)
	ctx := context.Background()
	s := snapshot(started, unknown("0200E2E1ABCD", "192.168.20.55"))
	r.Report(ctx, s)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("no pbx-config: events %q; want none", got)
	}
	if !strings.Contains(logs.String(), "has not been compiled") {
		t.Errorf("logs:\n%s", logs)
	}
	if err := c.Create(ctx, pbxConfig()); err != nil {
		t.Fatal(err)
	}
	r.Report(ctx, s)
	if got, want := rec.take(), []string{event("0200E2E1ABCD", "192.168.20.55")}; !slices.Equal(got, want) {
		t.Errorf("pbx-config written: events %q; want %q", got, want)
	}
}

// TestReadError: a ConfigMap read that fails is logged, nothing is marked,
// and the next poll reports the phone.
func TestReadError(t *testing.T) {
	failing := true
	c := interceptor.NewClient(fakeClient(pbxConfig()), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failing {
				return errors.New("cache not synced")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r, rec, logs := newReporter(t, c)
	ctx := context.Background()
	s := snapshot(started, unknown("0200E2E1ABCD", "192.168.20.55"))
	r.Report(ctx, s)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a failed read: events %q; want none", got)
	}
	if !strings.Contains(logs.String(), "cache not synced") {
		t.Errorf("logs:\n%s\nwant the read error", logs)
	}
	failing = false
	r.Report(ctx, s)
	if got := rec.take(); len(got) != 1 {
		t.Errorf("after the read recovers: events %q; want one", got)
	}
}

// TestNothingUnknown: a snapshot without unknown devices reads nothing.
func TestNothingUnknown(t *testing.T) {
	reads := 0
	c := interceptor.NewClient(fakeClient(pbxConfig()), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads++
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r, rec, _ := newReporter(t, c)
	r.Report(context.Background(), snapshot(started))
	r.Report(context.Background(), agentview.Snapshot{})
	if reads != 0 || len(rec.take()) != 0 {
		t.Errorf("%d reads; want none", reads)
	}
}

// TestEachMACItsOwnEvent: through client-go's real events broadcaster, two
// unknown phones reported on one pbx-config within minutes are two Events,
// each naming its MAC; without a related object per MAC the second would
// join the first's series and its note would be lost.
func TestEachMACItsOwnEvent(t *testing.T) {
	cs := kubefake.NewClientset()
	b := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.StartRecordingToSinkWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()
	cm := pbxConfig()
	cm.UID, cm.ResourceVersion = "uid-1", "7"
	r := discovery.New(discovery.Config{Reader: fakeClient(cm), Namespace: ns, Names: names,
		Recorder: b.NewRecorder(clientgoscheme.Scheme, install.EventRecorderName)})
	r.Report(ctx, snapshot(started, unknown("0200E2E00001", "192.168.20.11")))
	r.Report(ctx, snapshot(started, unknown("0200E2E00001", "192.168.20.11"), unknown("0200E2E00002", "192.168.20.12")))

	var notes []string
	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		l, err := cs.EventsV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		notes = notes[:0]
		for _, e := range l.Items {
			notes = append(notes, e.Note)
		}
		return len(notes) == 2, nil
	})
	slices.Sort(notes)
	want := []string{install.UnknownPhoneMessage("0200E2E00001", "192.168.20.11"), install.UnknownPhoneMessage("0200E2E00002", "192.168.20.12")}
	if err != nil || !slices.Equal(notes, want) {
		t.Errorf("Event notes %q (%v); want %q", notes, err, want)
	}
}

// TestFloodOfFreshMACs: a host requesting SEP<random MAC>.cnf.xml in a
// loop puts up to the agent's 64 new MACs in every poll. The Reporter
// emits at most ReportBurst Events at once and one per ReportEvery after,
// reports the MACs it held back on a later poll while the agent still
// lists them, and remembers only MACs the agent still lists, so its memory
// stays bounded.
func TestFloodOfFreshMACs(t *testing.T) {
	rec := &recorder{}
	now := started
	r := discovery.New(discovery.Config{Reader: fakeClient(pbxConfig()), Namespace: ns, Names: names, Recorder: rec,
		Now: func() time.Time { return now }})
	ctx := context.Background()
	batch := func(poll int) []agentapi.UnknownDevice {
		var out []agentapi.UnknownDevice
		for i := range 64 {
			out = append(out, unknown(fmt.Sprintf("02%04X%06X", poll, i), "192.168.20.66"))
		}
		return out
	}
	first := batch(0)
	r.Report(ctx, snapshot(started, first...))
	if n := len(rec.take()); n != discovery.ReportBurst {
		t.Fatalf("first poll of 64 fresh MACs: %d Events; want the burst, %d", n, discovery.ReportBurst)
	}
	// Five seconds on, the same 64: still limited, nothing more.
	now = now.Add(5 * time.Second)
	r.Report(ctx, snapshot(started, first...))
	if n := len(rec.take()); n != 0 {
		t.Errorf("five seconds on: %d Events; want none", n)
	}
	// One ReportEvery on, one held-back MAC is reported.
	now = now.Add(discovery.ReportEvery)
	r.Report(ctx, snapshot(started, first...))
	if got := rec.take(); len(got) != 1 || slices.Contains(got, event(first[0].MAC, first[0].Source)) {
		t.Errorf("one interval on: events %q; want one MAC not reported before", got)
	}
	// A day of fresh batches: Events stay at the rate, memory at the
	// agent's list.
	total := 0
	for poll := 1; poll <= 17280; poll++ {
		now = now.Add(5 * time.Second)
		r.Report(ctx, snapshot(started, batch(poll)...))
		total += len(rec.take())
	}
	if limit := int(17280*5*time.Second/discovery.ReportEvery) + discovery.ReportBurst; total > limit {
		t.Errorf("a day of floods: %d Events; want at most %d", total, limit)
	}
	if n := discovery.Remembered(r); n > 64 {
		t.Errorf("the Reporter remembers %d MACs; want at most the agent's 64", n)
	}
}
