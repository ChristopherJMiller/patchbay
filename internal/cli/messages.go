package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
)

// The limits come from messagesapi, the endpoint's wire contract: that
// package holds only types and validation (no controller implementation)
// and mirrors the Message CRD, so the CLI shares it rather than keeping a
// second copy to drift.

// DefaultMessageExpires is how long a message lives when --expires is not
// given. MessageOpts.Expires is taken as given: zero means 0s (expires at
// once, shown only if the phone is free right now), so a caller that wants
// the default passes this.
const DefaultMessageExpires = messagesapi.DefaultExpires

// MessageOpts are the flags of `patchbay phone message`.
type MessageOpts struct {
	// Title is the heading ("" for none).
	Title string
	// Urgent shows the message in quiet hours and away; a call in progress
	// holds it like any other.
	Urgent bool
	// Expires is how long the message stays worth showing, 0s to 24h.
	Expires time.Duration
	// Key makes a newer message with the same key replace this one.
	Key string
}

// PhoneMessage puts a text on a desk phone: it creates a Message for the
// controller to decide and deliver, as the in-cluster endpoint does for
// Home Assistant, and prints its name. The text, title, expiry and key are
// checked against the endpoint's limits before any API call. The phone is
// not looked up: a Message for a Phone that does not exist is Failed
// (PhoneMissing) by the controller.
func PhoneMessage(ctx context.Context, env *Env, phone, text string, o MessageOpts) error {
	req := messagesapi.MessageRequest{
		To: []string{phone}, Title: o.Title, Text: text, Urgent: o.Urgent,
		Expires: o.Expires.String(), Key: o.Key,
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("phone message: %w", err)
	}
	msg := &v1alpha1.Message{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "msg-",
			Namespace:    env.Namespace,
			Labels:       install.ManagedLabels(),
		},
		Spec: v1alpha1.MessageSpec{
			Phone: v1alpha1.LocalName(phone), Title: o.Title, Text: text, Urgent: o.Urgent,
			ExpiresAt: metav1.NewTime(env.Clock.Now().Add(o.Expires)), Key: o.Key,
		},
	}
	if err := env.Kube.Create(ctx, msg); err != nil {
		return fmt.Errorf("phone message: creating the message: %w", err)
	}
	p := &printer{w: env.Out}
	p.printf("%s\n", msg.Name)
	return p.err
}

// PhoneWithdraw takes back the messages for a phone with a key that are
// not finished: it sets spec.withdrawn, and the controller clears the
// screen. It prints how many it withdrew (none is not an error).
func PhoneWithdraw(ctx context.Context, env *Env, phone, key string) error {
	req := messagesapi.WithdrawRequest{Key: key, To: []string{phone}}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("phone withdraw: %w", err)
	}
	var list v1alpha1.MessageList
	if err := env.Kube.List(ctx, &list, client.InNamespace(env.Namespace)); err != nil {
		return fmt.Errorf("phone withdraw: listing messages in namespace %s: %w", env.Namespace, err)
	}
	n := 0
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.Key != key || string(m.Spec.Phone) != phone || finalMessage(m.Status.State) {
			continue
		}
		set := false
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var cur v1alpha1.Message
			if err := env.Kube.Get(ctx, client.ObjectKeyFromObject(m), &cur); err != nil {
				return err
			}
			if cur.Spec.Withdrawn || finalMessage(cur.Status.State) {
				set = false
				return nil
			}
			cur.Spec.Withdrawn = true
			set = true
			return env.Kube.Update(ctx, &cur)
		})
		if apierrors.IsNotFound(err) {
			continue // gone since the list
		}
		if err != nil {
			return fmt.Errorf("phone withdraw: withdrawing %s: %w", m.Name, err)
		}
		if set {
			n++
		}
	}
	p := &printer{w: env.Out}
	noun := "messages"
	if n == 1 {
		noun = "message"
	}
	p.printf("withdrew %d %s with key %s from %s\n", n, noun, printable(key), printable(phone))
	return p.err
}

// finalMessage reports a state a Message never leaves.
func finalMessage(st v1alpha1.MessageState) bool {
	switch st {
	case v1alpha1.MessageStateExpired, v1alpha1.MessageStateWithdrawn,
		v1alpha1.MessageStateReplaced, v1alpha1.MessageStateFailed:
		return true
	case v1alpha1.MessageStateHeld, v1alpha1.MessageStateShown:
		return false
	}
	return false
}

// sortedMessages lists the namespace's Messages: unfinished first, then
// finished; newest first within each, name breaking a tie.
func sortedMessages(ctx context.Context, kube client.Reader, namespace string) ([]v1alpha1.Message, error) {
	var list v1alpha1.MessageList
	if err := kube.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing Messages in namespace %s: %w", namespace, err)
	}
	items := list.Items
	slices.SortFunc(items, func(a, b v1alpha1.Message) int {
		if fa, fb := finalMessage(a.Status.State), finalMessage(b.Status.State); fa != fb {
			if fa {
				return 1
			}
			return -1
		}
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return items, nil
}

// Messages prints every Message in the namespace as a table (name, phone,
// state, reason, key, urgent, sent, expires), unfinished ones first, newest
// first, finished ones after. Times are RFC 3339 UTC, as in status. The
// text is not shown: it can be private, and the name finds it with kubectl.
func Messages(ctx context.Context, env *Env) error {
	items, err := sortedMessages(ctx, env.Kube, env.Namespace)
	if err != nil {
		return err
	}
	p := &printer{w: env.Out}
	if len(items) == 0 {
		p.printf("No messages in namespace %s.\n", printable(env.Namespace))
		return p.err
	}
	tw := tabwriter.NewWriter(env.Out, 0, 8, 2, ' ', 0)
	row := func(cols ...string) {
		for i, c := range cols {
			if i > 0 {
				_, _ = tw.Write([]byte("\t"))
			}
			_, _ = tw.Write([]byte(c))
		}
		_, _ = tw.Write([]byte("\n"))
	}
	row("NAME", "PHONE", "STATE", "REASON", "KEY", "URGENT", "SENT", "EXPIRES")
	for _, m := range items {
		row(printable(m.Name), printable(string(m.Spec.Phone)), printable(displayState(string(m.Status.State))),
			dash(printable(m.Status.Reason)), dash(printable(m.Spec.Key)), strconv.FormatBool(m.Spec.Urgent),
			m.CreationTimestamp.UTC().Format(time.RFC3339), m.Spec.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return p.err
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// heldMessages are the unfinished Messages the controller is holding back,
// for status.
func heldMessages(ctx context.Context, kube client.Reader, namespace string) ([]v1alpha1.Message, error) {
	items, err := sortedMessages(ctx, kube, namespace)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(items, func(m v1alpha1.Message) bool {
		return m.Status.State != v1alpha1.MessageStateHeld
	}), nil
}
