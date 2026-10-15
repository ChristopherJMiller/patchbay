package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// run executes the CLI with args and returns its stdout and error. Every run
// gets a --kubeconfig that does not exist, so no test can ever fall back to
// a real kubeconfig.
func run(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--kubeconfig", filepath.Join(t.TempDir(), "missing")))
	err := root.ExecuteContext(ctx)
	return out.String(), err
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

func TestStubsExitNonZero(t *testing.T) {
	for _, args := range [][]string{
		{"status"},
		{"models"},
		{"firmware", "load", "cmterm.zip"},
		{"softphone", "qr", "chris-laptop"},
		{"softphone", "qr", "chris-laptop", "--reissue"},
		{"phone", "resync", "desk"},
		{"stream-bridge"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := run(t, context.Background(), args...)
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("error = %v, want errNotImplemented", err)
			}
		})
	}
}

func TestStubsCheckArgs(t *testing.T) {
	if _, err := run(t, context.Background(), "phone", "resync"); err == nil || errors.Is(err, errNotImplemented) {
		t.Fatalf("phone resync with no name: error = %v, want an argument error", err)
	}
}

func TestControllerRequiresNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	_, err := run(t, context.Background(), "controller")
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("error = %v, want a namespace error", err)
	}
}

func TestAgentRequiresToken(t *testing.T) {
	t.Setenv(agentTokenEnv, "")
	_, err := run(t, context.Background(), "agent", "--listen", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error = %v, want a token error", err)
	}
}

func TestAgentRunsUntilCancelled(t *testing.T) {
	t.Setenv(agentTokenEnv, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := run(t, ctx, "agent", "--listen", "127.0.0.1:0"); err != nil {
		t.Fatalf("agent stopped with %v, want a clean shutdown", err)
	}
}
