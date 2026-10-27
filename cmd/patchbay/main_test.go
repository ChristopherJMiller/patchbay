package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// run executes the CLI with args and returns its output and error. Every run
// gets a --kubeconfig that does not exist, and $KUBECONFIG and $HOME in a
// temporary directory, so no test can ever reach a real kubeconfig
// (client-go fixes ~/.kube/config from $HOME at start, so only a non-empty
// $KUBECONFIG keeps its default chain off it).
func run(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(tmp, "no-such-kubeconfig"))
	t.Setenv("HOME", tmp)
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--kubeconfig", missingKubeconfig(t)))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// missingKubeconfig is the --kubeconfig every run passes.
func missingKubeconfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("HOME"), "missing")
}

func TestVersion(t *testing.T) {
	out, err := run(t, context.Background(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "patchbay "+version.Version) {
		t.Fatalf("version printed %q", out)
	}
}

// TestClientCommandsLoadOnlyTheGivenKubeconfig: each client command builds
// its client from --kubeconfig alone, so with a file that does not exist
// it fails naming that file, before any request (cli.NewEnv never falls
// back to $KUBECONFIG, ~/.kube/config or the in-cluster config when a
// file is named).
func TestClientCommandsLoadOnlyTheGivenKubeconfig(t *testing.T) {
	zip := filepath.Join(t.TempDir(), "cmterm.zip")
	for _, args := range [][]string{
		{"status"},
		{"status", "-n", "telephony"},
		{"status", "--namespace", "telephony", "--instance", "patchbay"},
		{"firmware", "load", zip},
		{"softphone", "qr", "chris-laptop"},
		{"softphone", "qr", "chris-laptop", "--reissue", "-n", "telephony"},
		{"phone", "resync", "desk", "--instance", "patchbay"},
		{"phone", "test-push", "desk", "--text", "hi", "--beep"},
		{"phone", "test-push", "desk", "--clear"},
		{"phone", "test-push", "desk", "--execute", "Key:Messages", "--execute", "Play:Chime.raw"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := run(t, context.Background(), args...)
			if err == nil || !strings.Contains(err.Error(), "kubeconfig "+missingKubeconfig(t)+" does not exist") {
				t.Fatalf("error = %v, want the missing --kubeconfig named", err)
			}
		})
	}
}

func TestClientCommandsCheckArgs(t *testing.T) {
	for _, args := range [][]string{
		{"phone", "resync"},
		{"phone", "test-push"},
		{"phone", "test-push", "a", "b"},
		{"phone", "test-push", "desk", "--clear", "--text", "x"},
		{"phone", "test-push", "desk", "--execute", "Key:Messages", "--beep"},
		{"softphone", "qr"},
		{"firmware", "load"},
		{"status", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := run(t, context.Background(), args...)
			if err == nil || strings.Contains(err.Error(), "kubeconfig") {
				t.Fatalf("error = %v, want an argument error", err)
			}
		})
	}
}

// TestModelsNeedsNoCluster: models prints the built-in drivers' profiles
// without loading any kubeconfig.
func TestModelsNeedsNoCluster(t *testing.T) {
	out, err := run(t, context.Background(), "models")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cisco-7945", "softphone"} {
		if !strings.Contains(out, want) {
			t.Errorf("models does not mention %s:\n%s", want, out)
		}
	}
}

func TestControllerRequiresNamespaceAndFullname(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	_, err := run(t, context.Background(), "controller")
	if err == nil || !strings.Contains(err.Error(), "namespace") || !strings.Contains(err.Error(), "--fullname") {
		t.Fatalf("error = %v, want the namespace and --fullname named", err)
	}
	_, err = run(t, context.Background(), "controller", "--namespace", "telephony",
		"--fullname", strings.Repeat("p", install.MaxFullnameLength+1))
	if err == nil || !strings.Contains(err.Error(), "--fullname") {
		t.Fatalf("error = %v, want --fullname refused", err)
	}
}

// TestControllerReadsItsSettings: the settings file is read and checked
// before any kubeconfig: a missing file, settings that do not parse,
// settings of another install, and settings a driver cannot render with
// (drivers.SettingsChecker) each stop it, naming why.
func TestControllerReadsItsSettings(t *testing.T) {
	dir := t.TempDir()
	good, err := os.ReadFile(filepath.Join("..", "..", "internal", "controller", "settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, file, fullname, want string
	}{
		{"missing", filepath.Join(dir, "nothing.json"), "patchbay", "nothing.json"},
		{"not settings", write("bad.json", `{"pbxAddress": "192.0.2.10", "surprise": 1}`), "patchbay", "surprise"},
		{"another install's", write("other.json", string(good)), "other", "other-ami"},
		{"a time zone the 79x5 cannot name", write("tz.json", strings.Replace(string(good), `"timezone": "UTC"`, `"timezone": "America/Vancouver"`, 1)),
			"patchbay", "America/Vancouver"},
		{"good, then the kubeconfig", write("good.json", string(good)), "patchbay", "kubeconfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, context.Background(), "controller", "--namespace", "telephony",
				"--fullname", tc.fullname, "--settings-file", tc.file)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
