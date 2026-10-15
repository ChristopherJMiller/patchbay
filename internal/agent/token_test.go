package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// kubeletVolume mimics how the kubelet writes a Secret volume: the visible
// file is a symlink through ..data, and an update renames a new timestamped
// directory into place, so the file gets a new inode.
type kubeletVolume struct {
	t   *testing.T
	dir string
	gen int
}

func newKubeletVolume(t *testing.T, token string) *kubeletVolume {
	v := &kubeletVolume{t: t, dir: t.TempDir()}
	v.write(token)
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(v.dir, "token")); err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *kubeletVolume) path() string { return filepath.Join(v.dir, "token") }

func (v *kubeletVolume) write(token string) {
	v.t.Helper()
	v.gen++
	gen := filepath.Join(v.dir, "..gen"+string(rune('0'+v.gen)))
	if err := os.Mkdir(gen, 0o755); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "token"), []byte(token+"\n"), 0o440); err != nil {
		v.t.Fatal(err)
	}
	tmp := filepath.Join(v.dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(gen), tmp); err != nil {
		v.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(v.dir, "..data")); err != nil {
		v.t.Fatal(err)
	}
}

func TestTokenFileFollowsARotation(t *testing.T) {
	vol := newKubeletVolume(t, "old-token")
	clk := &clock{t: pinned}
	f, err := NewTokenFile(vol.path(), time.Minute, clk.now, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Tokens(); !slices.Equal(got, []string{"old-token"}) {
		t.Fatalf("Tokens() = %v, want [old-token]", got)
	}

	vol.write("new-token")
	if got := f.Tokens(); !slices.Equal(got, []string{"new-token", "old-token"}) {
		t.Fatalf("right after the rotation Tokens() = %v, want [new-token old-token]", got)
	}
	clk.add(59 * time.Second)
	if got := f.Tokens(); !slices.Contains(got, "old-token") {
		t.Fatalf("within the grace period Tokens() = %v, want old-token still accepted", got)
	}
	clk.add(time.Second)
	if got := f.Tokens(); !slices.Equal(got, []string{"new-token"}) {
		t.Fatalf("after the grace period Tokens() = %v, want [new-token]", got)
	}
}

func TestTokenFileSeesAnInPlaceRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := NewTokenFile(path, 0, nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	// Same size, same inode: only the modification time tells.
	if err := os.WriteFile(path, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if got := f.Tokens(); got[0] != "bbbb" {
		t.Fatalf("Tokens() = %v, want bbbb first", got)
	}
}

func TestTokenFileKeepsTheLastGoodToken(t *testing.T) {
	vol := newKubeletVolume(t, "good-token")
	f, err := NewTokenFile(vol.path(), time.Minute, nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	vol.write("") // an empty Secret
	if got := f.Tokens(); !slices.Equal(got, []string{"good-token"}) {
		t.Fatalf("after an empty rewrite Tokens() = %v, want [good-token]", got)
	}
	if err := os.Remove(filepath.Join(vol.dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if got := f.Tokens(); !slices.Equal(got, []string{"good-token"}) {
		t.Fatalf("with the file gone Tokens() = %v, want [good-token]", got)
	}
}

func TestNewTokenFileFailsWithoutAToken(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewTokenFile(filepath.Join(dir, "missing"), 0, nil, logr.Discard()); err == nil {
		t.Error("a missing file: want an error")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTokenFile(empty, 0, nil, logr.Discard()); err == nil {
		t.Error("an empty file: want an error")
	}
}

func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// TestControlAPIFollowsTheTokenFile is the end-to-end property: after the
// Secret changes, the running server accepts the new token without a
// restart, and the old one only during the grace period.
func TestControlAPIFollowsTheTokenFile(t *testing.T) {
	vol := newKubeletVolume(t, "old-token")
	clk := &clock{t: pinned}
	f, err := NewTokenFile(vol.path(), time.Minute, clk.now, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Options{Tokens: f.Tokens, Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, h)

	if code, _ := do(t, ts, http.MethodGet, api.PathObservations, "new-token"); code != http.StatusUnauthorized {
		t.Fatalf("new token before the rotation: %d, want 401", code)
	}
	vol.write("new-token")
	if code, _ := do(t, ts, http.MethodGet, api.PathObservations, "new-token"); code != http.StatusOK {
		t.Fatalf("new token after the rotation: %d, want 200", code)
	}
	if code, _ := do(t, ts, http.MethodGet, api.PathObservations, "old-token"); code != http.StatusOK {
		t.Fatalf("old token within the grace period: %d, want 200", code)
	}
	clk.add(time.Minute)
	if code, _ := do(t, ts, http.MethodGet, api.PathObservations, "old-token"); code != http.StatusUnauthorized {
		t.Fatalf("old token after the grace period: %d, want 401", code)
	}
}

func TestEmptyTokensAreRefused(t *testing.T) {
	if _, err := NewHandler(Options{Tokens: StaticToken("")}); err == nil {
		t.Error("NewHandler with only an empty token: want an error")
	}
	if accepted("", []string{""}) {
		t.Error("an empty bearer token matched an empty token")
	}
}
