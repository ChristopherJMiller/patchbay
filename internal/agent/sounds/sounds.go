// Package sounds installs the PBX's sound prompts (docs/plans/release-a.md
// §2(c), W2.9).
//
// The Asterisk image ships no prompts, and recordings are never committed,
// so the patchbay image carries prompts generated at build time
// (hack/gen-sounds.sh: espeak-ng text-to-speech from text in the repo) under
// DefaultSource. At start the agent copies them into the sounds volume,
// mounted at compile.SoundsDir in the agent (read-write) and in Asterisk
// (read-only), where the compiled dialplan plays them by absolute path. The
// agent reports ready only once every prompt is there (Check), so Asterisk
// never answers 911 with a missing announcement.
package sounds

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// DefaultSource is where the image carries the generated prompts (the
// Dockerfile copies build/sounds/ there).
const DefaultSource = "/usr/share/patchbay/sounds"

// FileMode is every installed prompt's mode: Asterisk runs as another uid
// and reads them as "other".
const FileMode fs.FileMode = 0o644

// Prompts are the prompts the compiled dialplan plays.
var Prompts = []string{compile.SoundNoEmergency, compile.SoundNotAvailable}

// Formats are the encodings each prompt exists in, as file extensions:
// G.711 µ-law at 8 kHz and G.722 at 16 kHz, so neither kind of call
// transcodes (format_pcm plays both).
var Formats = []string{"ulaw", "g722"}

// Files lists every prompt file name: each prompt in each format.
func Files() []string {
	out := make([]string, 0, len(Prompts)*len(Formats))
	for _, p := range Prompts {
		for _, f := range Formats {
			out = append(out, p+"."+f)
		}
	}
	return out
}

// Install copies every prompt file from src into dst, each one written to a
// temporary file in dst and renamed into place with FileMode, so Asterisk
// never plays a half-written prompt and an agent restart (dst survives it)
// simply replaces them. It fails, naming the file, if one is missing or
// empty in src, and checks dst afterwards (Check).
func Install(src, dst string) error {
	var errs []error
	for _, name := range Files() {
		if err := install(filepath.Join(src, name), dst, name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sounds: installing the prompts from %s: %w", src, err)
	}
	return Check(dst)
}

func install(from, dst, name string) (err error) {
	in, err := os.Open(from) //nolint:gosec // G304: the path is the agent's own flag plus a fixed prompt name.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	switch {
	case err != nil:
		return err
	case !st.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", from)
	case st.Size() == 0:
		return fmt.Errorf("%s is empty", from)
	}

	tmp, err := os.CreateTemp(dst, "."+name+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copying %s: %w", from, err)
	}
	if err = tmp.Chmod(FileMode); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dst, name))
}

// Check reports an error, naming every problem, unless each prompt file is
// in dir as a non-empty regular file that others may read. A symlink does
// not count: the agent installs real files.
func Check(dir string) error {
	var errs []error
	for _, name := range Files() {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			errs = append(errs, fmt.Errorf("%s is missing", p))
		case err != nil:
			errs = append(errs, err)
		case !st.Mode().IsRegular():
			errs = append(errs, fmt.Errorf("%s is not a regular file", p))
		case st.Size() == 0:
			errs = append(errs, fmt.Errorf("%s is empty", p))
		case st.Mode().Perm()&0o004 == 0:
			errs = append(errs, fmt.Errorf("%s is not readable by Asterisk (mode %v)", p, st.Mode().Perm()))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sounds: %w", err)
	}
	return nil
}
