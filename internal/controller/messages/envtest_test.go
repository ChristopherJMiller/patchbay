package messages_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/messages"
)

// TestMessagesEnvtest runs Setup in a manager against a real API server,
// with a fake agent (a Holder whose desk is registered, and an Actor that
// acknowledges each page it is sent, as the agent's next poll reports it):
// a Message for desk is pushed once, with the chime, and Shown; when the
// agent forgets the page (a restart) the same page goes again without the
// chime; the house ConfigMap's quiet holds a non-urgent Message while an
// urgent one is pushed; a withdrawn Message is Withdrawn and deleted a day
// later, when the rest expire and the screen is cleared. The CRD (its enum,
// its bounds) refuses nothing the reconciler writes, and its Events reach
// the API server.
func TestMessagesEnvtest(t *testing.T) {
	ctrltest.Need(t, kube)
	ctx := context.Background()
	namespace := ctrltest.Namespace(t, kube)
	if err := kube.Client.Create(ctx, deskPhone(namespace)); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	holder := &agentview.Holder{}
	holder.Set(registered(clk.Now()))
	act := newActor()
	act.ack, act.now = holder, clk.Now

	mgr := ctrltest.NewManager(t, kube, namespace)
	if err := messages.Setup(mgr, messages.Config{
		Client: mgr.GetClient(), Namespace: namespace, Names: names, Settings: testSettings(t), Registry: registry(t),
		Observer: holder, Actor: act, Recorder: mgr.GetEventRecorder(install.EventRecorderName),
		Interval: 100 * time.Millisecond, Now: clk.Now,
	}); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)

	create := func(name, text string, urgent bool) {
		t.Helper()
		m := &v1alpha1.Message{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: v1alpha1.MessageSpec{
				Phone: "desk", Title: "House", Text: text, Urgent: urgent, ExpiresAt: metav1.NewTime(clk.Now().Add(time.Hour)),
			},
		}
		if err := kube.Client.Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(name string, state v1alpha1.MessageState, reason string) v1alpha1.Message {
		t.Helper()
		var m v1alpha1.Message
		err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			if err := kube.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &m); err != nil {
				return false, err
			}
			return m.Status.State == state && m.Status.Reason == reason, nil
		})
		if err != nil {
			t.Fatalf("%s never became %s %s: %+v", name, state, reason, m.Status)
		}
		if m.Status.ObservedGeneration != m.Generation {
			t.Errorf("%s: observedGeneration %d, want %d", name, m.Status.ObservedGeneration, m.Generation)
		}
		return m
	}
	// pushed waits for n more pushes and returns them.
	var got []agentapi.PushRequest
	pushed := func(n int) []agentapi.PushRequest {
		t.Helper()
		err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
			got = append(got, act.take()...)
			return len(got) >= n, nil
		})
		if err != nil || len(got) != n {
			t.Fatalf("pushes %+v, want %d", got, n)
		}
		out := got
		got = nil
		return out
	}
	// none checks that nothing more is pushed for a while (20 polls).
	none := func(what string) {
		t.Helper()
		time.Sleep(20 * 100 * time.Millisecond)
		if p := act.take(); len(p) != 0 {
			t.Errorf("%s: pushed %+v", what, p)
		}
	}

	// A Message for desk: pushed once with the chime, then Shown.
	create("dinner", "Dinner is ready", false)
	p := pushed(1)[0]
	if !p.Beep || p.Clear || p.Device != "phone-desk" || !strings.Contains(p.Page, "Dinner is ready") {
		t.Fatalf("the first push %+v, want desk's page with the chime", p)
	}
	if m := waitFor("dinner", v1alpha1.MessageStateShown, ""); m.Status.ShownAt == nil {
		t.Errorf("dinner: no shownAt: %+v", m.Status)
	}
	none("dinner Shown")

	// The agent restarts: it holds no page. The same page goes again,
	// without the chime, once the push the reconciler remembers is older
	// than PushedMemory.
	clk.add(messages.PushedMemory)
	holder.Set(registered(clk.Now()))
	if again := pushed(1)[0]; again.Beep || again.PageVersion != p.PageVersion {
		t.Errorf("after the agent restarted: %+v, want %s without the chime", again, p.PageVersion)
	}
	none("re-sent")

	// Quiet hours: the non-urgent one waits; the urgent one goes out, on a
	// page with dinner (still Shown) and without the one that waits.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: names.HouseConfigMap, Namespace: namespace},
		Data:       map[string]string{install.HouseKey: `{"quiet":true,"away":false,"updatedAt":"2026-10-26T22:00:00Z"}`},
	}
	if err := kube.Client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	// The house ConfigMap is in the reconciler's cache before the next
	// Message arrives.
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var c corev1.ConfigMap
		err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(cm), &c)
		return err == nil, client.IgnoreNotFound(err)
	})
	if err != nil {
		t.Fatalf("the house ConfigMap never reached the cache: %v", err)
	}
	create("laundry", "The laundry is done", false)
	waitFor("laundry", v1alpha1.MessageStateHeld, v1alpha1.ReasonQuietHours)
	create("leak", "Water under the sink", true)
	p = pushed(1)[0]
	if !p.Beep || !strings.Contains(p.Page, "Water under the sink") || !strings.Contains(p.Page, "Dinner is ready") ||
		strings.Contains(p.Page, "laundry") {
		t.Errorf("the urgent push %+v, want leak and dinner with the chime", p)
	}
	waitFor("leak", v1alpha1.MessageStateShown, "")
	waitFor("laundry", v1alpha1.MessageStateHeld, v1alpha1.ReasonQuietHours)

	// dinner is withdrawn: Withdrawn, and the page is pushed without it
	// and without the chime.
	var dinner v1alpha1.Message
	if err := kube.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "dinner"}, &dinner); err != nil {
		t.Fatal(err)
	}
	dinner.Spec.Withdrawn = true
	if err := kube.Client.Update(ctx, &dinner); err != nil {
		t.Fatal(err)
	}
	if m := waitFor("dinner", v1alpha1.MessageStateWithdrawn, ""); m.Status.FinishedAt == nil || m.Status.ObservedGeneration != 2 {
		t.Errorf("dinner withdrawn: %+v", m.Status)
	}
	if p = pushed(1)[0]; p.Beep || strings.Contains(p.Page, "Dinner is ready") || !strings.Contains(p.Page, "Water under the sink") {
		t.Errorf("the push after the withdraw %+v, want leak alone without the chime", p)
	}

	// A day on: dinner (finished a day ago) is deleted; laundry and leak
	// expire, and the screen is cleared.
	clk.add(25 * time.Hour)
	holder.Set(registered(clk.Now(), func(d *agentapi.DeviceObservations) { d.Page = p.PageVersion }))
	err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var m v1alpha1.Message
		err := kube.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "dinner"}, &m)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
	if err != nil {
		t.Errorf("dinner was not deleted a day after it finished: %v", err)
	}
	waitFor("laundry", v1alpha1.MessageStateExpired, "")
	waitFor("leak", v1alpha1.MessageStateExpired, "")
	if p = pushed(1)[0]; !p.Clear || p.Beep || p.Page != "" {
		t.Errorf("the last push %+v, want a clear", p)
	}
	none("cleared")

	// The Events reached the API server, regarding their Messages.
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var list eventsv1.EventList
		if err := kube.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return false, err
		}
		var seen []string
		for _, e := range list.Items {
			if e.Regarding.Kind == "Message" {
				seen = append(seen, fmt.Sprintf("%s %s %s", e.Regarding.Name, e.Type, e.Reason))
			}
		}
		for _, want := range []string{
			"dinner Normal " + install.EventReasonMessageShown,
			"laundry Normal " + install.EventReasonMessageHeld,
			"leak Normal " + install.EventReasonMessageShown,
			"dinner Normal " + install.EventReasonMessageWithdrawn,
			"laundry Normal " + install.EventReasonMessageExpired,
		} {
			if !slices.Contains(seen, want) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("the Message Events: %v", err)
	}
}
