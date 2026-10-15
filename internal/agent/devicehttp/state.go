package devicehttp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// The spent-token file (layout.StateDir/layout.SpentTokensFile).
//
// It is device HTTP's only durable state: one record per spent token, the
// token's SHA-256 (never the token) and the served fetch event, in the
// order they were spent. It is rewritten whole on every change (a temp
// file in the same directory, fsync, rename over the file, fsync the
// directory), so a reader sees the old file or the new one, never half.
//
// Device HTTP writes a spend before it sends the document. So a crash
// between the two loses at worst that one document (the controller rotates
// the token on the re-recorded fetch, and the person scans a fresh QR);
// it can never make a token usable twice, which an in-memory record
// forgotten by an agent restart would.
type stateFile struct {
	Spends []spendRecord `json:"spends"`
}

type spendRecord struct {
	// TokenSHA256 is the lower-case hex SHA-256 of the spent token.
	TokenSHA256 string `json:"tokenSHA256"`
	// Event is the served fetch, exactly as recorded when it was spent.
	Event sensor.FetchEvent `json:"event"`
}

// tokenDigest is a token's SHA-256.
type tokenDigest [sha256.Size]byte

func digestOf(token string) tokenDigest { return sha256.Sum256([]byte(token)) }

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// stateTempPattern names the temp files a rewrite uses; New removes any a
// crash left behind.
const stateTempPattern = layout.SpentTokensFile + ".tmp-*"

// ensureStateDir creates dir (mode layout.StateDirMode) if it is missing
// and gives an existing one that mode.
func ensureStateDir(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, layout.StateDirMode); err != nil {
			return fmt.Errorf("devicehttp: creating the state directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("devicehttp: the state directory: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("devicehttp: the state directory %s is not a directory", dir)
	}
	if err := os.Chmod(dir, layout.StateDirMode); err != nil {
		return fmt.Errorf("devicehttp: the state directory: %w", err)
	}
	stale, err := filepath.Glob(filepath.Join(dir, stateTempPattern))
	if err != nil {
		return fmt.Errorf("devicehttp: the state directory: %w", err)
	}
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("devicehttp: removing a stale temp file: %w", err)
		}
	}
	return nil
}

// loadState reads the spent-token file; a missing file is no spends.
// Anything it cannot fully trust is an error: failing open would make
// every spent token usable again.
func loadState(dir string) ([]spendRecord, error) {
	file := filepath.Join(dir, layout.SpentTokensFile)
	b, err := os.ReadFile(file) //nolint:gosec // G304: the agent's own state directory.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("devicehttp: reading %s: %w", file, err)
	}
	corrupt := func(format string, args ...any) error {
		return fmt.Errorf("devicehttp: %s is corrupt (remove it only if every provisioning token it spent has been rotated): "+format,
			append([]any{file}, args...)...)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var st stateFile
	if err := dec.Decode(&st); err != nil {
		return nil, corrupt("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, corrupt("data after the document")
	}
	seen := map[string]bool{}
	for i, r := range st.Spends {
		if !digestPattern.MatchString(r.TokenSHA256) {
			return nil, corrupt("spend %d: the token digest is not a lower-case hex SHA-256", i)
		}
		if seen[r.TokenSHA256] {
			return nil, corrupt("spend %d: a token spent twice", i)
		}
		seen[r.TokenSHA256] = true
		if err := checkSpendEvent(r.Event); err != nil {
			return nil, corrupt("spend %d: %v", i, err)
		}
	}
	return st.Spends, nil
}

// checkSpendEvent accepts only what device HTTP records for a spend: a
// valid served http8088 fetch of a provisioning document, with its device
// and version.
func checkSpendEvent(e sensor.FetchEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	tree, _ := compile.TreeOf(e.File)
	switch {
	case e.Transport != api.TransportHTTPDevice:
		return fmt.Errorf("transport %s, not %s", e.Transport, api.TransportHTTPDevice)
	case e.Result != api.FetchServed:
		return fmt.Errorf("result %s, not %s", e.Result, api.FetchServed)
	case tree != compile.TreeProvisioning || e.BundlePath != e.File:
		return fmt.Errorf("file %q is not a provisioning document", e.File)
	case e.Device == "" || e.Version == "":
		return errors.New("no device or version")
	}
	return nil
}

// writeState replaces the spent-token file with recs.
func writeState(dir string, recs []spendRecord) error {
	if recs == nil {
		recs = []spendRecord{}
	}
	data, err := json.Marshal(stateFile{Spends: recs})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, stateTempPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, layout.SpentTokensFile)); err != nil {
		return err
	}
	renamed = true
	d, err := os.Open(dir) //nolint:gosec // G304: the agent's own state directory, opened to fsync it.
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func hexDigest(d tokenDigest) string { return hex.EncodeToString(d[:]) }

// parseDigest is the inverse of hexDigest.
func parseDigest(s string) (tokenDigest, bool) {
	var d tokenDigest
	if !digestPattern.MatchString(s) {
		return d, false
	}
	_, err := hex.Decode(d[:], []byte(s))
	return d, err == nil
}
