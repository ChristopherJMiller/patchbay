package sounds_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/sounds"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// source writes every prompt file into a new directory, each holding its
// own name, and returns the directory.
func source(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range sounds.Files() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("audio of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFiles(t *testing.T) {
	want := []string{
		compile.SoundNoEmergency + ".ulaw", compile.SoundNoEmergency + ".g722",
		compile.SoundNotAvailable + ".ulaw", compile.SoundNotAvailable + ".g722",
	}
	got := sounds.Files()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Files() = %v, want %v", got, want)
	}
}

// TestInstallCopiesBothFormats: every prompt, in both formats, lands in the
// destination byte for byte, mode 0644 whatever the umask and the source's
// mode, and nothing else is left there.
func TestInstallCopiesBothFormats(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	src, dst := source(t), t.TempDir()
	if err := sounds.Install(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range sounds.Files() {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte("audio of "+name)) {
			t.Errorf("%s = %q", name, got)
		}
		st, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode() != sounds.FileMode {
			t.Errorf("%s mode %v, want %v", name, st.Mode(), sounds.FileMode)
		}
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(sounds.Files()) {
		t.Errorf("destination holds %d entries, want exactly the %d prompts", len(entries), len(sounds.Files()))
	}
	if err := sounds.Check(dst); err != nil {
		t.Errorf("Check after Install: %v", err)
	}
}

// TestInstallReplaces: a second Install (an agent restart; the volume
// survives it) replaces what is there.
func TestInstallReplaces(t *testing.T) {
	src, dst := source(t), t.TempDir()
	for _, name := range sounds.Files() {
		if err := os.WriteFile(filepath.Join(dst, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := sounds.Install(src, dst); err != nil {
		t.Fatal(err)
	}
	name := sounds.Files()[0]
	if got, _ := os.ReadFile(filepath.Join(dst, name)); string(got) != "audio of "+name {
		t.Errorf("%s = %q after a second install", name, got)
	}
}

// TestInstallRefusesAMissingPrompt: a prompt missing or empty in the image
// fails Install, naming it, and installs the others anyway.
func TestInstallRefusesAMissingPrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(path string) error
		want   string
	}{
		{"missing", os.Remove, "no such file"},
		{"empty", func(p string) error { return os.WriteFile(p, nil, 0o600) }, "is empty"},
		{"a directory", func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Mkdir(p, 0o700)
		}, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := source(t), t.TempDir()
			bad := sounds.Files()[1]
			if err := tc.mutate(filepath.Join(src, bad)); err != nil {
				t.Fatal(err)
			}
			err := sounds.Install(src, dst)
			if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Install = %v, want an error naming %s (%s)", err, bad, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dst, sounds.Files()[0])); err != nil {
				t.Errorf("the other prompts were not installed: %v", err)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(dst, ".*")); len(leftovers) > 0 {
				t.Errorf("temporary files left behind: %v", leftovers)
			}
		})
	}
}

func TestInstallFailsOnAMissingDestination(t *testing.T) {
	if err := sounds.Install(source(t), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Install into a missing directory succeeded")
	}
}

// TestCheck: every way a prompt can be unusable is reported, each by name.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	if err := sounds.Install(source(t), dir); err != nil {
		t.Fatal(err)
	}
	files := sounds.Files()
	if err := os.Remove(filepath.Join(dir, files[0])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, files[1]), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, files[2]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, files[3])); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, files[2]), filepath.Join(dir, files[3])); err != nil {
		t.Fatal(err)
	}
	err := sounds.Check(dir)
	if err == nil {
		t.Fatal("Check passed a broken directory")
	}
	for _, want := range []string{
		files[0] + " is missing",
		files[1] + " is empty",
		files[2] + " is not readable by Asterisk",
		files[3] + " is not a regular file",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check = %v, want it to say %q", err, want)
		}
	}
}

func TestCheckEmptyDirectory(t *testing.T) {
	err := sounds.Check(t.TempDir())
	if err == nil || strings.Count(err.Error(), "is missing") != len(sounds.Files()) {
		t.Fatalf("Check of an empty directory = %v, want every prompt missing", err)
	}
}
