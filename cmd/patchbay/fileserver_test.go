package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFileserverRequiresItsAddresses(t *testing.T) {
	_, err := run(t, context.Background(), "fileserver")
	if err == nil || !strings.Contains(err.Error(), "--tftp-listen") {
		t.Fatalf("error = %v, want the missing listen addresses", err)
	}
}

func TestFileserverNeverServesAsRoot(t *testing.T) {
	_, err := run(t, context.Background(), "fileserver",
		"--tftp-listen", "127.0.0.1:0", "--http-listen", "127.0.0.1:0", "--uid", "0", "--gid", "0")
	if err == nil || !strings.Contains(err.Error(), "never serves as root") {
		t.Fatalf("error = %v, want a refusal to serve as root", err)
	}
}

// TestFileserverExitsWhenTheDropFails: without the privilege to drop to
// another uid (the test runs unprivileged), the file server binds, fails
// to drop and exits with that error rather than serve.
func TestFileserverExitsWhenTheDropFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the drop would succeed and change this test process's uid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := run(t, ctx, "fileserver", "--live-dir", t.TempDir(), "--firmware-dir", t.TempDir(),
		"--tftp-listen", "127.0.0.1:0", "--http-listen", "127.0.0.1:0", "--fetch-socket", "/nonexistent/fetches.sock")
	if err == nil || !strings.Contains(err.Error(), "dropping privileges") {
		t.Fatalf("error = %v, want the failed privilege drop", err)
	}
}
