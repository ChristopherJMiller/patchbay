package sounds_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/sounds"
)

// hack/gen-sounds.sh renders what Install installs: these tests keep its
// voices and its layout in step with this package.

var genSounds = filepath.Join("..", "..", "..", "hack", "gen-sounds.sh")

// TestGenSoundsVoicesAreTheAllowlist: the script renders exactly Voices,
// in the same order.
func TestGenSoundsVoicesAreTheAllowlist(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), genSounds, "--list").Output()
	if err != nil {
		t.Fatalf("%s --list: %v", genSounds, err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, sounds.Voices) {
		t.Fatalf("%s --list = %v, want sounds.Voices %v", genSounds, got, sounds.Voices)
	}
}

// TestGenSoundsRefusesAnUnknownVoice: a voice outside the allowlist,
// alone or beside a known one, stops the script before it renders,
// builds or even looks for a tool (PATH holds only bash), naming the
// voice and the voices.
func TestGenSoundsRefusesAnUnknownVoice(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, args := range [][]string{{"lessac"}, {sounds.DefaultVoice, "lessac"}, {""}} {
		cmd := exec.CommandContext(t.Context(), bash, append([]string{genSounds, out}, args...)...)
		cmd.Env = []string{"PATH=" + filepath.Dir(bash)}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		bad := args[len(args)-1]
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("gen-sounds %v: %v, want exit status 1", args, err)
		}
		for _, want := range []string{`unknown voice "` + bad + `"`, "the voices are: " + strings.Join(sounds.Voices, " ")} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("gen-sounds %v said %q, want %q", args, stderr.String(), want)
			}
		}
	}
	if entries, _ := os.ReadDir(out); len(entries) > 0 {
		t.Errorf("a refused run left %v in the output", entries)
	}
}

// TestGenSoundsRendersWhatInstallTakes: the default voice, rendered by the
// script (espeak-ng and ffmpeg from the flake), installs, each file at
// least a second long (8000 bytes a second in both formats); the prompts of
// the layout before voices are cleared from the output.
func TestGenSoundsRendersWhatInstallTakes(t *testing.T) {
	for _, tool := range []string{"espeak-ng", "ffmpeg"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: run the tests inside `nix develop`", tool)
		}
	}
	out, dst := t.TempDir(), t.TempDir()
	stale := filepath.Join(out, sounds.Files()[0])
	if err := os.WriteFile(stale, []byte("before voices"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.CommandContext(t.Context(), genSounds, out, sounds.DefaultVoice).CombinedOutput(); err != nil {
		t.Fatalf("gen-sounds: %v\n%s", err, b)
	}
	if err := sounds.Install(out, sounds.DefaultVoice, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range sounds.Files() {
		if st, err := os.Stat(filepath.Join(dst, name)); err != nil || st.Size() < 8000 {
			t.Errorf("%s: %v, want at least a second of audio", name, err)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != sounds.DefaultVoice || !entries[0].IsDir() {
		t.Errorf("the output holds %v, want only the %s directory", entries, sounds.DefaultVoice)
	}
}
