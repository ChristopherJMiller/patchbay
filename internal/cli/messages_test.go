package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

func listMessages(t *testing.T, te *testEnv) []v1alpha1.Message {
	t.Helper()
	var l v1alpha1.MessageList
	if err := te.Kube.List(context.Background(), &l, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestPhoneMessage(t *testing.T) {
	ctx := context.Background()
	te := newTestEnv(t)
	err := cli.PhoneMessage(ctx, te.Env, "desk", "Dinner is ready", cli.MessageOpts{
		Title: "Kitchen", Urgent: true, Expires: cli.DefaultMessageExpires, Key: "dinner",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := listMessages(t, te)
	if len(got) != 1 {
		t.Fatalf("%d messages, want 1", len(got))
	}
	m := got[0]
	if want := m.Name + "\n"; te.out.String() != want || !strings.HasPrefix(m.Name, "msg-") {
		t.Errorf("printed %q for %q", te.out, m.Name)
	}
	if m.Spec.Phone != "desk" || m.Spec.Text != "Dinner is ready" || m.Spec.Title != "Kitchen" || !m.Spec.Urgent || m.Spec.Key != "dinner" {
		t.Errorf("spec = %+v", m.Spec)
	}
	if want := testNow.Add(time.Hour); !m.Spec.ExpiresAt.Time.Equal(want.Truncate(time.Second)) && !m.Spec.ExpiresAt.Time.Equal(want) {
		t.Errorf("expiresAt = %s, want %s", m.Spec.ExpiresAt, want)
	}
	for k, v := range install.ManagedLabels() {
		if m.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, m.Labels[k], v)
		}
	}
}

func TestPhoneMessageExpiresZeroAllowed(t *testing.T) {
	te := newTestEnv(t)
	if err := cli.PhoneMessage(context.Background(), te.Env, "desk", "now or never", cli.MessageOpts{}); err != nil {
		t.Fatal(err)
	}
	m := listMessages(t, te)[0]
	if !m.Spec.ExpiresAt.Time.Equal(testNow.Truncate(time.Second)) && !m.Spec.ExpiresAt.Time.Equal(testNow) {
		t.Errorf("expiresAt = %s, want now (%s)", m.Spec.ExpiresAt, testNow)
	}
}

// Everything the endpoint would refuse is refused before any API call.
func TestPhoneMessageRefusals(t *testing.T) {
	long := strings.Repeat("x", 257)
	for _, tc := range []struct {
		name, phone, text string
		o                 cli.MessageOpts
		want              string
	}{
		{"text too long", "desk", long, cli.MessageOpts{}, "text"},
		{"text empty", "desk", "", cli.MessageOpts{}, "text"},
		{"control in text", "desk", "a\x1b[2Jb", cli.MessageOpts{}, "control"},
		{"title too long", "desk", "hi", cli.MessageOpts{Title: strings.Repeat("t", 33)}, "title"},
		{"control in title", "desk", "hi", cli.MessageOpts{Title: "a\nb"}, "title"},
		{"expires too long", "desk", "hi", cli.MessageOpts{Expires: 24*time.Hour + time.Second}, "expires"},
		{"expires negative", "desk", "hi", cli.MessageOpts{Expires: -time.Second}, "expires"},
		{"key not a label", "desk", "hi", cli.MessageOpts{Key: "Not A Label"}, "key"},
		{"phone not a name", "Desk Phone", "hi", cli.MessageOpts{}, "phone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			calls := 0
			te.Kube = interceptKube(t, te, &calls)
			err := cli.PhoneMessage(context.Background(), te.Env, tc.phone, tc.text, tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
			if calls != 0 {
				t.Errorf("%d API calls before refusing", calls)
			}
			if te.out.Len() != 0 {
				t.Errorf("printed %q", te.out)
			}
		})
	}
}

// interceptKube counts every call to the fake client.
func interceptKube(t *testing.T, te *testEnv, calls *int) client.Client {
	t.Helper()
	count := func() { *calls++ }
	return interceptor.NewClient(te.Kube.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			count()
			return c.Create(ctx, o, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			count()
			return c.List(ctx, l, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			count()
			return c.Get(ctx, k, o, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			count()
			return c.Update(ctx, o, opts...)
		},
	})
}

func testMessage(name, phone, key string, state v1alpha1.MessageState, reason string, age time.Duration) *v1alpha1.Message {
	return &v1alpha1.Message{
		ObjectMeta: objectMeta(name, testNow.Add(-age).Truncate(time.Second)),
		Spec: v1alpha1.MessageSpec{
			Phone: v1alpha1.LocalName(phone), Text: "secret text " + name, Key: key,
			ExpiresAt: metav1.NewTime(testNow.Add(time.Hour - age).Truncate(time.Second)),
			Urgent:    name == "msg-urgent",
		},
		Status: v1alpha1.MessageStatus{State: state, Reason: reason},
	}
}

func TestPhoneWithdraw(t *testing.T) {
	ctx := context.Background()
	te := newTestEnv(t,
		testMessage("msg-a", "desk", "dinner", v1alpha1.MessageStateHeld, v1alpha1.ReasonAway, time.Minute),
		testMessage("msg-b", "desk", "dinner", v1alpha1.MessageStateShown, "", 2*time.Minute),
		testMessage("msg-done", "desk", "dinner", v1alpha1.MessageStateExpired, "", 3*time.Minute),
		testMessage("msg-other-key", "desk", "other", v1alpha1.MessageStateHeld, "", time.Minute),
		testMessage("msg-other-phone", "kitchen", "dinner", v1alpha1.MessageStateHeld, "", time.Minute),
	)
	if err := cli.PhoneWithdraw(ctx, te.Env, "desk", "dinner"); err != nil {
		t.Fatal(err)
	}
	if want := "withdrew 2 messages with key dinner from desk\n"; te.out.String() != want {
		t.Errorf("output %q, want %q", te.out, want)
	}
	for _, m := range listMessages(t, te) {
		want := m.Name == "msg-a" || m.Name == "msg-b"
		if m.Spec.Withdrawn != want {
			t.Errorf("%s withdrawn = %v, want %v", m.Name, m.Spec.Withdrawn, want)
		}
	}
	// Again: nothing left to withdraw, and that is not an error.
	te.out.Reset()
	if err := cli.PhoneWithdraw(ctx, te.Env, "desk", "dinner"); err != nil {
		t.Fatal(err)
	}
	if want := "withdrew 0 messages with key dinner from desk\n"; te.out.String() != want {
		t.Errorf("output %q, want %q", te.out, want)
	}
}

func TestPhoneWithdrawRefusals(t *testing.T) {
	for _, key := range []string{"", "Bad Key"} {
		te := newTestEnv(t)
		calls := 0
		te.Kube = interceptKube(t, te, &calls)
		if err := cli.PhoneWithdraw(context.Background(), te.Env, "desk", key); err == nil {
			t.Errorf("key %q: no error", key)
		}
		if calls != 0 {
			t.Errorf("key %q: %d API calls", key, calls)
		}
	}
}

func TestMessagesTable(t *testing.T) {
	te := newTestEnv(t,
		testMessage("msg-old-done", "desk", "", v1alpha1.MessageStateExpired, "", 5*time.Hour),
		testMessage("msg-new-done", "desk", "dinner", v1alpha1.MessageStateReplaced, "", 4*time.Hour),
		testMessage("msg-old", "desk", "", v1alpha1.MessageStateShown, "", 2*time.Hour),
		testMessage("msg-urgent", "kitchen", "alarm", v1alpha1.MessageStateHeld, v1alpha1.ReasonInCall, time.Minute),
		testMessage("msg-pending", "desk", "", "", "", 30*time.Minute),
	)
	if err := cli.Messages(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	golden(t, "messages.golden", te.out.Bytes())
	if strings.Contains(te.out.String(), "secret text") {
		t.Error("the table printed message text")
	}
}

func TestMessagesEmptyAndEscapes(t *testing.T) {
	te := newTestEnv(t)
	if err := cli.Messages(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if want := "No messages in namespace telephony.\n"; te.out.String() != want {
		t.Errorf("output %q, want %q", te.out, want)
	}
	te = newTestEnv(t, testMessage("msg-x", "desk", "k", v1alpha1.MessageStateHeld, "Bad\x1b[2JReason", time.Minute))
	if err := cli.Messages(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(te.out.String(), 0x1b) {
		t.Errorf("escape reached the terminal: %q", te.out)
	}
}
