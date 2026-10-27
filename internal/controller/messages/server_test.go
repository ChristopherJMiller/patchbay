package messages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/messages"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
)

const (
	ns    = "telephony"
	token = "s3cret-token-value"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

type env struct {
	c   client.Client
	srv *httptest.Server
	log *bytes.Buffer
}

func tokenSecret(tok string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "msg-token", Namespace: ns},
		Data:       map[string][]byte{"token": []byte(tok)},
	}
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Message{}).Build()
	logs := &bytes.Buffer{}
	log := funcr.New(func(prefix, args string) { logs.WriteString(prefix + args + "\n") }, funcr.Options{Verbosity: 9})
	s, err := messages.NewServer(messages.ServerConfig{
		Client: c, Namespace: ns, Names: install.NewNames("patchbay"),
		TokenSecret: types.NamespacedName{Namespace: ns, Name: "msg-token"}, TokenKey: "token",
		Addr: "127.0.0.1:0", Now: func() time.Time { return now }, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{c: c, srv: srv, log: logs}
}

func (e *env) do(t *testing.T, method, path, tok, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func (e *env) list(t *testing.T) []v1alpha1.Message {
	t.Helper()
	var l v1alpha1.MessageList
	if err := e.c.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func (e *env) house(t *testing.T) (*corev1.ConfigMap, bool) {
	t.Helper()
	var cm corev1.ConfigMap
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "patchbay-house"}, &cm)
	return &cm, err == nil
}

func TestMessagesCreates(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	code, body := e.do(t, "POST", messagesapi.PathMessages, token,
		`{"to":["desk","kitchen"],"title":"Hi","text":"Door open","urgent":true,"expires":"30m","key":"fridge-door"}`)
	if code != http.StatusCreated {
		t.Fatalf("code %d: %s", code, body)
	}
	var resp messagesapi.MessageResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Names) != 2 {
		t.Fatalf("response %q: %v", body, err)
	}
	items := e.list(t)
	if len(items) != 2 {
		t.Fatalf("%d messages", len(items))
	}
	phones := map[string]bool{}
	for _, m := range items {
		phones[string(m.Spec.Phone)] = true
		if !strings.HasPrefix(m.Name, "msg-") {
			t.Errorf("name %q", m.Name)
		}
		if !m.Spec.ExpiresAt.Time.Equal(now.Add(30 * time.Minute)) {
			t.Errorf("expiresAt %v", m.Spec.ExpiresAt)
		}
		if m.Spec.Text != "Door open" || m.Spec.Title != "Hi" || !m.Spec.Urgent || m.Spec.Key != "fridge-door" {
			t.Errorf("spec %+v", m.Spec)
		}
		for k, v := range install.ManagedLabels() {
			if m.Labels[k] != v {
				t.Errorf("label %s=%q", k, m.Labels[k])
			}
		}
	}
	if !phones["desk"] || !phones["kitchen"] {
		t.Errorf("phones %v", phones)
	}
}

func TestMessagesAuth(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	good := `{"to":["desk"],"text":"x"}`
	for name, tok := range map[string]string{"none": "", "wrong": "nope"} {
		if code, _ := e.do(t, "POST", messagesapi.PathMessages, tok, good); code != http.StatusUnauthorized {
			t.Errorf("%s: code %d", name, code)
		}
	}
	if n := len(e.list(t)); n != 0 {
		t.Fatalf("%d messages after rejected requests", n)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, good); code != http.StatusCreated {
		t.Fatalf("right token: %d", code)
	}
	// Rotation without a restart.
	sec := tokenSecret("rotated")
	var cur corev1.Secret
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(sec), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Data["token"] = []byte("rotated")
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, good); code != http.StatusUnauthorized {
		t.Errorf("old token after rotation: %d", code)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, "rotated", good); code != http.StatusCreated {
		t.Errorf("new token after rotation: %d", code)
	}
}

func TestMessagesNoSecret(t *testing.T) {
	e := newEnv(t)
	code, body := e.do(t, "POST", messagesapi.PathMessages, "anything", `{"to":["desk"],"text":"x"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "no messages token configured") {
		t.Fatalf("missing Secret: %d %s", code, body)
	}
	if len(e.list(t)) != 0 {
		t.Error("wrote with no token")
	}
	// A Secret without the key.
	sec := tokenSecret(token)
	sec.Data = map[string][]byte{"other": []byte("x")}
	e = newEnv(t, sec)
	if code, _ := e.do(t, "PUT", messagesapi.PathHouse, "x", `{"quiet":true}`); code != http.StatusServiceUnavailable {
		t.Errorf("missing key: %d", code)
	}
	if _, ok := e.house(t); ok {
		t.Error("wrote the house with no token")
	}
}

func TestWithdrawSetsSpec(t *testing.T) {
	mk := func(name, phone, key string, st v1alpha1.MessageState) *v1alpha1.Message {
		return &v1alpha1.Message{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.MessageSpec{Phone: v1alpha1.LocalName(phone), Text: "x", Key: key},
			Status:     v1alpha1.MessageStatus{State: st},
		}
	}
	e := newEnv(t, tokenSecret(token),
		mk("live", "desk", "fridge-door", v1alpha1.MessageStateShown),
		mk("old", "desk", "fridge-door", v1alpha1.MessageStateExpired),
		mk("other-key", "desk", "garage", v1alpha1.MessageStateShown),
		mk("other-phone", "kitchen", "fridge-door", v1alpha1.MessageStateHeld),
	)
	code, body := e.do(t, "POST", messagesapi.PathWithdraw, token, `{"key":"fridge-door","to":["desk"]}`)
	if code != 200 || strings.TrimSpace(body) != `{"withdrawn":1}` {
		t.Fatalf("%d %s", code, body)
	}
	got := map[string]bool{}
	for _, m := range e.list(t) {
		got[m.Name] = m.Spec.Withdrawn
	}
	if !got["live"] || got["old"] || got["other-key"] || got["other-phone"] {
		t.Errorf("withdrawn: %v", got)
	}
	// Every phone when To is empty.
	_, body = e.do(t, "POST", messagesapi.PathWithdraw, token, `{"key":"fridge-door"}`)
	if strings.TrimSpace(body) != `{"withdrawn":1}` {
		t.Errorf("all phones: %s", body)
	}
}

func TestHousePut(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	if code, body := e.do(t, "GET", messagesapi.PathHouse, token, ""); code != 200 {
		t.Fatalf("get: %d %s", code, body)
	} else if h, err := messagesapi.ParseHouse([]byte(body)); err != nil || h.Quiet || h.Away {
		t.Fatalf("empty house: %s %v", body, err)
	}
	if code, body := e.do(t, "PUT", messagesapi.PathHouse, token, `{"quiet":true}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	cm, ok := e.house(t)
	if !ok {
		t.Fatal("no ConfigMap")
	}
	h, err := messagesapi.ParseHouse([]byte(cm.Data[install.HouseKey]))
	if err != nil || !h.Quiet || h.Away || !h.UpdatedAt.Equal(now) {
		t.Fatalf("house %+v %v", h, err)
	}
	if cm.Labels[install.LabelManagedBy] != install.ManagedBy {
		t.Errorf("labels %v", cm.Labels)
	}
	if code, _ := e.do(t, "PUT", messagesapi.PathHouse, token, `{"away":true}`); code != 200 {
		t.Fatal(code)
	}
	cm, _ = e.house(t)
	h, _ = messagesapi.ParseHouse([]byte(cm.Data[install.HouseKey]))
	if !h.Quiet || !h.Away {
		t.Errorf("quiet not kept: %+v", h)
	}
	_, body := e.do(t, "GET", messagesapi.PathHouse, token, "")
	if g, _ := messagesapi.ParseHouse([]byte(body)); !g.Quiet || !g.Away {
		t.Errorf("get: %s", body)
	}
}

func TestHouseRejectsBadState(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	for _, body := range []string{`{"quiet":"unavailable"}`, `{"away":1}`, `{}`, ``} {
		if code, _ := e.do(t, "PUT", messagesapi.PathHouse, token, body); code != http.StatusBadRequest {
			t.Errorf("%q: %d", body, code)
		}
	}
	if _, ok := e.house(t); ok {
		t.Error("ConfigMap written")
	}
}

func TestServerLimits(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	big := `{"to":["desk"],"text":"` + strings.Repeat("a", messages.MaxBody) + `"}`
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("big: %d", code)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, `{"to":["desk"],"text":"x","bogus":1}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, `{"to":["desk"],"text":"x"} {}`); code != 400 {
		t.Errorf("trailing data: %d", code)
	}
	if code, _ := e.do(t, "POST", messagesapi.PathMessages, token, `{"to":[],"text":"x"}`); code != 400 {
		t.Errorf("invalid: %d", code)
	}
	if code, _ := e.do(t, "GET", messagesapi.PathMessages, token, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: %d", code)
	}
	if n := len(e.list(t)); n != 0 {
		t.Errorf("%d messages", n)
	}
}

func TestServerNeverLogsToken(t *testing.T) {
	e := newEnv(t, tokenSecret(token))
	var all strings.Builder
	for _, tok := range []string{token, "wrong", ""} {
		code, body := e.do(t, "POST", messagesapi.PathMessages, tok, `{"to":["desk"],"text":"x"}`)
		all.WriteString(body)
		_ = code
	}
	_, body := e.do(t, "POST", messagesapi.PathMessages, token+"x", `{"bad`)
	all.WriteString(body)
	all.WriteString(e.log.String())
	if strings.Contains(all.String(), token) {
		t.Error("the token appears in an answer or a log")
	}
}

func TestParseMessagesToken(t *testing.T) {
	for _, s := range []string{"a/b", "my-secret/token"} {
		if _, _, err := controller.ParseMessagesToken(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "a", "/b", "a/", "a/b/c", "A_B/k"} {
		if _, _, err := controller.ParseMessagesToken(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
