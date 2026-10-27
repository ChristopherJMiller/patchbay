package devicehttp_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

const (
	authUser = "patchbay-push"
	authPass = "s3cret-Pa55-word"
)

type authFixture struct {
	ts   *httptest.Server
	rec  *recorder
	logs *logs
}

func newAuthFixture(t *testing.T, ok bool) *authFixture {
	t.Helper()
	root := t.TempDir()
	rec := newRecorder(t)
	l := &logs{}
	s, err := devicehttp.New(devicehttp.Config{
		LiveDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Grants: &grants{}, Recorder: rec,
		Credentials: func() (string, string, bool) { return authUser, authPass, ok },
		Log: funcr.New(func(prefix, args string) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.b.WriteString(prefix + " " + args + "\n")
		}, funcr.Options{Verbosity: 10}),
	})
	must(t, err)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &authFixture{ts: ts, rec: rec, logs: l}
}

func (f *authFixture) send(t *testing.T, method, target, body string, hdr map[string]string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.ts.URL+target, rd)
	must(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.ts.Client().Do(req)
	must(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	return response{resp.StatusCode, resp.Header, string(b)}
}

func wantAnswer(t *testing.T, r response, want string) {
	t.Helper()
	if r.code != http.StatusOK || r.body != want {
		t.Fatalf("got %d %q, want 200 %q", r.code, r.body, want)
	}
}

func TestAuthenticateQuery(t *testing.T) {
	f := newAuthFixture(t, true)
	q := url.Values{"UserID": {authUser}, "Password": {authPass}, "devicename": {"SEPD0C78915E8C8"}}
	wantAnswer(t, f.send(t, http.MethodGet, compile.AuthenticatePath+"?"+q.Encode(), "", nil), "AUTHORIZED")
}

func TestAuthenticateForm(t *testing.T) {
	f := newAuthFixture(t, true)
	q := url.Values{"UserID": {authUser}, "Password": {authPass}, "devicename": {"SEPD0C78915E8C8"}}
	wantAnswer(t, f.send(t, http.MethodPost, compile.AuthenticatePath, q.Encode(), nil), "AUTHORIZED")
}

func TestAuthenticateBasic(t *testing.T) {
	f := newAuthFixture(t, true)
	r := f.send(t, http.MethodGet, compile.AuthenticatePath, "", nil)
	wantAnswer(t, r, "UN-AUTHORIZED")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.ts.URL+compile.AuthenticatePath, nil)
	must(t, err)
	req.SetBasicAuth(authUser, authPass)
	resp, err := f.ts.Client().Do(req)
	must(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "AUTHORIZED" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
}

func TestAuthenticateCaseInsensitiveNames(t *testing.T) {
	f := newAuthFixture(t, true)
	q := url.Values{"userid": {authUser}, "password": {authPass}}
	wantAnswer(t, f.send(t, http.MethodGet, compile.AuthenticatePath+"?"+q.Encode(), "", nil), "AUTHORIZED")
}

func TestAuthenticateWrong(t *testing.T) {
	f := newAuthFixture(t, true)
	for name, q := range map[string]url.Values{
		"wrong password": {"UserID": {authUser}, "Password": {"nope"}},
		"wrong user":     {"UserID": {"someone"}, "Password": {authPass}},
		"no password":    {"UserID": {authUser}},
		"nothing":        {},
	} {
		t.Run(name, func(t *testing.T) {
			wantAnswer(t, f.send(t, http.MethodGet, compile.AuthenticatePath+"?"+q.Encode(), "", nil), "UN-AUTHORIZED")
		})
	}
	g := newAuthFixture(t, false)
	q := url.Values{"UserID": {authUser}, "Password": {authPass}}
	wantAnswer(t, g.send(t, http.MethodGet, compile.AuthenticatePath+"?"+q.Encode(), "", nil), "UN-AUTHORIZED")
}

func TestAuthenticateNeverLogsValues(t *testing.T) {
	f := newAuthFixture(t, true)
	q := url.Values{"UserID": {authUser}, "Password": {authPass}, "devicename": {"SEPD0C78915E8C8"}}
	f.send(t, http.MethodGet, compile.AuthenticatePath+"?"+q.Encode(), "", nil)
	hdr := map[string]string{"Authorization": "Basic " + "cGF0Y2hiYXktcHVzaDpzM2NyZXQtUGE1NS13b3Jk"}
	f.send(t, http.MethodPost, compile.AuthenticatePath, "zeta=1&alpha=2", hdr)

	out := f.logs.String()
	for _, secret := range []string{authPass, authUser, "cGF0Y2hi", "SEPD0C78915E8C8"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log holds a value %q:\n%s", secret, out)
		}
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "authenticate") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("want one line per request, got %d:\n%s", len(lines), out)
	}
	for _, want := range []string{`"method"="GET"`, `"Password,UserID,devicename"`, `"result"="AUTHORIZED"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line 1 lacks %s: %s", want, lines[0])
		}
	}
	for _, want := range []string{`"method"="POST"`, `alpha,zeta`, `Authorization: Basic`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("line 2 lacks %s: %s", want, lines[1])
		}
	}
}

func TestAuthenticateNotAFetch(t *testing.T) {
	f := newAuthFixture(t, true)
	wantAnswer(t, f.send(t, http.MethodGet, compile.AuthenticatePath+"?UserID=x", "", nil), "UN-AUTHORIZED")
	wantAnswer(t, f.send(t, http.MethodPost, compile.AuthenticatePath, "UserID=x", nil), "UN-AUTHORIZED")
	if got := f.rec.all(); len(got) != 0 {
		t.Fatalf("recorded fetches: %+v", got)
	}
}
