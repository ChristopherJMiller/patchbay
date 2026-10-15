package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// StaticToken accepts one fixed token. It is for tests and for a token given
// in the environment, which cannot change while the process runs.
func StaticToken(tok string) func() []string {
	return func() []string { return []string{tok} }
}

// DefaultTokenGrace is how long a TokenFile keeps accepting the token it
// just replaced.
const DefaultTokenGrace = 2 * time.Minute

// TokenFile is the control API token read from a file: in the PBX pod, the
// token Secret's volume. The controller regenerates a lost Secret (design
// §4, "a lost Secret heals itself") and the kubelet then replaces the file in
// place, so TokenFile checks the file on every call and re-reads it when it
// changed; the agent never needs a restart, which would drop calls.
//
// Two windows remain, and are the controller's to ride out:
//   - after a change TokenFile still accepts the token it replaced for its
//     grace period, so a controller that has not seen the new Secret yet is
//     not locked out;
//   - the kubelet refreshes Secret volumes only every minute or so, so for
//     that long after a rotation the agent knows only the old token. The
//     controller should retry a 401 with the token it used before the
//     rotation.
//
// If the file cannot be read, or is empty, TokenFile keeps the last good
// token and tries again on the next call.
type TokenFile struct {
	path  string
	grace time.Duration
	now   func() time.Time
	log   logr.Logger

	mu        sync.Mutex
	info      os.FileInfo // of the file the current token came from
	current   string
	previous  string
	replaced  time.Time // when previous stopped being current
	lastError string    // the last re-read failure, logged once
}

// NewTokenFile reads the token from path. grace <= 0 means
// DefaultTokenGrace; now nil means time.Now.
func NewTokenFile(path string, grace time.Duration, now func() time.Time, log logr.Logger) (*TokenFile, error) {
	if grace <= 0 {
		grace = DefaultTokenGrace
	}
	if now == nil {
		now = time.Now
	}
	f := &TokenFile{path: path, grace: grace, now: now, log: log}
	info, tok, err := readToken(path)
	if err != nil {
		return nil, err
	}
	f.info, f.current = info, tok
	return f, nil
}

// Tokens returns the tokens accepted now: the current one, and during a
// rotation's grace period the one it replaced.
func (f *TokenFile) Tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh()
	if f.previous != "" && f.now().Sub(f.replaced) < f.grace {
		return []string{f.current, f.previous}
	}
	return []string{f.current}
}

// refresh re-reads the file if it is not the one the current token came
// from. f.mu is held.
func (f *TokenFile) refresh() {
	info, err := os.Stat(f.path)
	if err == nil && sameFile(f.info, info) {
		return
	}
	var tok string
	if err == nil {
		info, tok, err = readToken(f.path)
	}
	if err != nil {
		if msg := err.Error(); msg != f.lastError {
			f.log.Error(err, "re-reading the control API token; keeping the last good one")
			f.lastError = msg
		}
		return
	}
	f.lastError = ""
	f.info = info
	if tok == f.current {
		return
	}
	f.previous, f.current, f.replaced = f.current, tok, f.now()
	f.log.Info("control API token changed; the previous one is accepted for the grace period", "grace", f.grace)
}

// sameFile reports whether b is still the file a describes. The kubelet
// swaps a Secret volume's files by renaming a new directory into place (a new
// inode); an in-place rewrite changes the size or modification time.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func readToken(path string) (os.FileInfo, string, error) {
	// Stat before reading: if the file changes in between, the next call
	// sees a file other than info and reads it again.
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading the token file: %w", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is an operator-supplied flag.
	if err != nil {
		return nil, "", fmt.Errorf("reading the token file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return nil, "", errors.New("the token file " + path + " is empty")
	}
	return info, tok, nil
}
