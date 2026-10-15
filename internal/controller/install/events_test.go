package install_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// stored is what a fake clientset holds of the Events: how many, and the
// series count of each (1 for an Event without a series), by note. The
// broadcaster records each Event in a goroutine of its own, so a series
// patch can race its Event's create and be tried again: the actions are
// not a measure, what is stored is.
func stored(ctx context.Context, cs *kubefake.Clientset) (map[string]int32, error) {
	l, err := cs.EventsV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]int32{}
	for _, e := range l.Items {
		out[e.Note] = 1
		if e.Series != nil {
			out[e.Note] = e.Series.Count
		}
	}
	return out, nil
}

// waitStored waits until the stored Events are exactly want (note ->
// count), where a note given as "one|two" may be either.
func waitStored(ctx context.Context, t *testing.T, cs *kubefake.Clientset, want map[string]int32) {
	t.Helper()
	var got map[string]int32
	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		if got, err = stored(ctx, cs); err != nil {
			return false, err
		}
		if len(got) != len(want) {
			return false, nil
		}
		for note, n := range want {
			match := false
			for _, alt := range strings.Split(note, "|") {
				if c, ok := got[alt]; ok && c == n {
					match = true
				}
			}
			if !match {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("stored Events (note: count) %v, want %v: %v", got, want, err)
	}
}

// TestEventSeries pins the client-go behaviour the Event doc (events.go)
// relies on, through the real events broadcaster: two Events that differ
// only in their note are one series (one create, one patch of its count;
// the second note is lost), while a different related object (each
// UnknownPhone's MAC) or a new resourceVersion of the regarded object (a
// device after a status write) is an Event of its own.
func TestEventSeries(t *testing.T) {
	cs := kubefake.NewClientset()
	b := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.StartRecordingToSinkWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()
	rec := b.NewRecorder(clientgoscheme.Scheme, install.EventRecorderName)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: ns, UID: "uid-1", ResourceVersion: "7"}}
	emit := func(regarding *corev1.ConfigMap, related runtime.Object, note string) {
		rec.Eventf(regarding, related, corev1.EventTypeNormal, install.EventReasonUnknownPhone, install.EventActionDiscover, "%s", note)
	}
	emit(cm, nil, "one")
	emit(cm, nil, "two: joins one's series")
	emit(cm, install.UnknownPhoneRelated(ns, "0200E2E00001"), install.UnknownPhoneMessage("0200E2E00001", "192.168.20.11"))
	emit(cm, install.UnknownPhoneRelated(ns, "0200E2E00002"), install.UnknownPhoneMessage("0200E2E00002", "192.168.20.12"))
	newer := cm.DeepCopy()
	newer.ResourceVersion = "8"
	emit(newer, nil, "three: the ConfigMap was rewritten")

	// "one" and "two" are one Event of count 2, holding whichever note was
	// recorded first.
	waitStored(ctx, t, cs, map[string]int32{
		"one|two: joins one's series":                                2,
		install.UnknownPhoneMessage("0200E2E00001", "192.168.20.11"): 1,
		install.UnknownPhoneMessage("0200E2E00002", "192.168.20.12"): 1,
		"three: the ConfigMap was rewritten":                         1,
	})
}

// TestCompileFailedSeries: each distinct compile error is an Event of its
// own (CompileFailedRelated names it), while the same error repeated by
// the 5 min retry joins its own series; without it every error on an
// unchanged pbx-config joined the first one's series, which kept its note.
func TestCompileFailedSeries(t *testing.T) {
	cs := kubefake.NewClientset()
	b := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.StartRecordingToSinkWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()
	rec := b.NewRecorder(clientgoscheme.Scheme, install.EventRecorderName)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.ConfigMap, Namespace: ns, UID: "uid-1", ResourceVersion: "7"}}
	for _, note := range []string{"compile: Phone desk: first error", "compile: Phone desk: first error", "compile: a second error"} {
		rec.Eventf(cm, install.CompileFailedRelated(cm, note), corev1.EventTypeWarning, install.EventReasonCompileFailed,
			install.EventActionCompile, "%s", note)
	}
	waitStored(ctx, t, cs, map[string]int32{"compile: Phone desk: first error": 2, "compile: a second error": 1})
	if r := install.CompileFailedRelated(cm, "x"); r.Kind != "ConfigMap" || r.Name != cm.Name || r.Namespace != ns ||
		r.FieldPath == install.CompileFailedRelated(cm, "y").FieldPath {
		t.Errorf("related %+v: want pbx-config, a field path per error", r)
	}
}
