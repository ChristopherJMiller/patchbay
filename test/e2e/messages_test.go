//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// TestDeskLoop's phone messages: what the CLI sends, what the messages
// endpoint is told, and what desk's fake phone takes (script.PushRecord).

// chimeItem is the Execute item that plays the chime ahead of a page with
// a new message (compile.ChimeFile, from the TFTP tree).
var chimeItem = "Play:" + path.Base(compile.ChimeFile)

// forgetMessages deletes every Message of the namespace and the house
// ConfigMap. A reused cluster keeps an earlier run's, which would crowd or
// hold this run's (an old message on desk's page, a quiet house).
func (h *harness) forgetMessages(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := h.kube.DeleteAllOf(ctx, &v1alpha1.Message{}, client.InNamespace(namespace)); err != nil {
		t.Fatalf("deleting earlier Messages: %v", err)
	}
	if err := h.cs.CoreV1().ConfigMaps(namespace).Delete(ctx, names.HouseConfigMap, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting %s: %v", names.HouseConfigMap, err)
	}
}

// message runs `patchbay phone message desk <text> [flags]` and returns
// the Message's name, which it prints.
func (h *harness) message(ctx context.Context, t *testing.T, text string, flags ...string) string {
	t.Helper()
	out, err := h.patchbay(ctx, append([]string{"phone", "message", deskName, text}, flags...)...)
	if err != nil {
		t.Fatalf("phone message %q: %v\n%s", text, err, out)
	}
	// Its one line on stdout; stderr (here too) may carry a client warning.
	var name string
	for line := range strings.SplitSeq(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "msg-") && !strings.ContainsAny(l, " \t") {
			name = l
		}
	}
	if name == "" {
		t.Fatalf("phone message %q printed %q, want the Message's name", text, out)
	}
	t.Logf("Message %s: %q %s", name, text, strings.Join(flags, " "))
	return name
}

// messageStatus reads a Message's status from the API server.
func (h *harness) messageStatus(ctx context.Context, t *testing.T, name string) v1alpha1.MessageStatus {
	t.Helper()
	var m v1alpha1.Message
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &m); err != nil {
		t.Fatalf("Message %s: %v", name, err)
	}
	return m.Status
}

// waitMessage polls a Message every second until cond holds of its
// status, for up to stateTimeout.
func (h *harness) waitMessage(ctx context.Context, t *testing.T, name, what string, cond func(v1alpha1.MessageStatus) bool) {
	t.Helper()
	var last v1alpha1.MessageStatus
	err := wait.PollUntilContextTimeout(ctx, time.Second, stateTimeout, true, func(ctx context.Context) (bool, error) {
		var m v1alpha1.Message
		if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &m); err != nil {
			return false, nil //nolint:nilerr // keep polling: the next read may succeed
		}
		last = m.Status
		return cond(m.Status), nil
	})
	if err != nil {
		t.Fatalf("Message %s %s: %v (state %q, reason %q: %q)", name, what, err, last.State, last.Reason, last.Message)
	}
}

// isShown is a Message wait condition: on the phone's screen.
func isShown(st v1alpha1.MessageStatus) bool {
	return st.State == v1alpha1.MessageStateShown && st.ShownAt != nil
}

// isHeld is a Message wait condition: Held for reason, never shown.
func isHeld(reason string) func(v1alpha1.MessageStatus) bool {
	return func(st v1alpha1.MessageStatus) bool {
		return st.State == v1alpha1.MessageStateHeld && st.Reason == reason && st.ShownAt == nil
	}
}

func (h *harness) pushCount(ctx context.Context, t *testing.T, pod string) int {
	t.Helper()
	s, err := h.phoneState(ctx, pod)
	if err != nil {
		t.Fatal(err)
	}
	return len(s.Pushes)
}

// pushedSince is a phone wait condition: one of the pushes after the first
// n matches.
func pushedSince(n int, match func(script.PushRecord) bool) func(script.State) (bool, string) {
	return func(s script.State) (bool, string) {
		since := s.Pushes[min(n, len(s.Pushes)):]
		return slices.ContainsFunc(since, match), fmt.Sprintf("%d pushes since: %+v", len(since), since)
	}
}

// messagePage matches a push desk took (its authenticationURL said
// AUTHORIZED, it answered Status 0) of its message page, with the chime
// first when chime and only then, whose fetched text holds each of has and
// none of lacks.
func messagePage(chime bool, has, lacks []string) func(script.PushRecord) bool {
	page := compile.MessagesPathPrefix + deskID + ".xml"
	return func(p script.PushRecord) bool {
		items := p.Items
		if chime {
			if len(items) != 2 || items[0] != chimeItem {
				return false
			}
			items = items[1:]
		}
		if !p.Authorized || p.Status != 0 || len(items) != 1 || !strings.Contains(items[0], page) || p.Fetched == "" {
			return false
		}
		for _, s := range has {
			if !strings.Contains(p.Fetched, s) {
				return false
			}
		}
		for _, s := range lacks {
			if strings.Contains(p.Fetched, s) {
				return false
			}
		}
		return true
	}
}

// messagesURL is p on the messages endpoint's Service, as Home Assistant
// reaches it from its own namespace.
func messagesURL(p string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d%s", names.MessagesService, namespace, messagesapi.DefaultPort, p)
}

// putHouse PUTs upd to the messages endpoint's house path from a tools pod
// (label names it), with the bearer token from messagesTokenSecret, as Home
// Assistant does; then GETs the house until it reads back as upd set it.
// The endpoint reads through the controller's cache, the one the Message
// reconciler decides from (the e2e install runs one controller), so a
// Message created after this is decided in the new house state.
func putHouse(ctx context.Context, t *testing.T, label string, upd messagesapi.HouseUpdate) {
	t.Helper()
	body, err := json.Marshal(upd)
	if err != nil {
		t.Fatal(err)
	}
	env := []corev1.EnvVar{secretEnv("TOKEN", messagesTokenSecret, messagesTokenKey)}
	const auth = "Authorization: Bearer $(TOKEN)"
	r := runTool(ctx, t, toolPod{name: label, env: env, args: []string{
		"http", "send", "PUT", messagesURL(messagesapi.PathHouse),
		"--header", auth, "--header", "Content-Type: application/json", "--data", string(body),
	}}, true)
	if !r.Succeeded {
		t.Fatalf("PUT %s %s was not answered 200", messagesapi.PathHouse, body)
	}
	var want []string
	if upd.Quiet != nil {
		want = append(want, fmt.Sprintf(`"quiet":%t`, *upd.Quiet))
	}
	if upd.Away != nil {
		want = append(want, fmt.Sprintf(`"away":%t`, *upd.Away))
	}
	var logs string
	err = wait.PollUntilContextTimeout(ctx, time.Second, stateTimeout, true, func(ctx context.Context) (bool, error) {
		r := runTool(ctx, t, toolPod{name: label + "-get", env: env, args: []string{
			"http", "get", "--header", auth, messagesURL(messagesapi.PathHouse),
		}}, true)
		logs = r.Logs
		for _, w := range want {
			if !strings.Contains(r.Logs, w) {
				return false, nil
			}
		}
		return r.Succeeded, nil
	})
	if err != nil {
		t.Fatalf("GET %s never read back %s: %v\n%s", messagesapi.PathHouse, body, err, indent(logs))
	}
}
