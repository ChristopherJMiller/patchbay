package install_test

import (
	"context"
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

// sinkCounts counts the Event writes a fake clientset received.
func sinkCounts(cs *kubefake.Clientset) (creates, patches int) {
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "events" {
			continue
		}
		switch a.GetVerb() {
		case "create":
			creates++
		case "patch":
			patches++
		}
	}
	return creates, patches
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

	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		c, p := sinkCounts(cs)
		return c+p == 5, nil
	})
	creates, patches := sinkCounts(cs)
	if err != nil || creates != 4 || patches != 1 {
		t.Errorf("%d creates, %d patches (%v); want 4 Events and one series patch", creates, patches, err)
	}
	l, err := cs.EventsV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	notes := map[string]bool{}
	for _, e := range l.Items {
		notes[e.Note] = true
	}
	for _, want := range []string{
		install.UnknownPhoneMessage("0200E2E00001", "192.168.20.11"),
		install.UnknownPhoneMessage("0200E2E00002", "192.168.20.12"),
		"three: the ConfigMap was rewritten",
	} {
		if !notes[want] {
			t.Errorf("no Event with note %q in %v", want, notes)
		}
	}
}
