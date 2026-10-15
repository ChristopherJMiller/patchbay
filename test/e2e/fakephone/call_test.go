package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestCallExpectTone: `call --expect-hz` counts the far phone's tone, not
// the echo of its own: a PBX answering with 600 Hz passes a 1000 Hz caller
// that expects 600, and fails one that expects its own tone back.
func TestCallExpectTone(t *testing.T) {
	pbx := startPBX(t, map[string]string{"phone-kitchen": "not-a-secret-kitchen"})
	pbx.setAnswerHz(600)
	run := func(extra ...string) (string, error) {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append([]string{"call", "600", "--timeout", "10s",
			"--server", pbx.addr.String(), "--transport", "udp", "--user", "101",
			"--auth-user", "phone-kitchen", "--password", "not-a-secret-kitchen",
			"--local-ip", "127.0.0.1", "--sip-port", strconv.Itoa(freePort(t)),
			"--duration", "1s", "--tail", "200ms", "--tone-hz", "1000", "--min-tone-frames", "20"}, extra...))
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}

	out, err := run("--expect-hz", "600")
	if err != nil {
		t.Fatalf("expecting the far tone: %v\n%s", err, out)
	}
	if !strings.Contains(out, "call 600: sent=") {
		t.Errorf("output %q", out)
	}
	if _, err := run(); err == nil || !strings.Contains(err.Error(), "the 1000 Hz tone came back in") {
		t.Fatalf("expecting the echo of 1000 Hz from a 600 Hz phone: %v", err)
	}
	if _, err := run("--expect-hz", "800"); err == nil || !strings.Contains(err.Error(), "the 800 Hz tone") {
		t.Fatalf("expecting 800 Hz from a 600 Hz phone: %v", err)
	}
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	if len(pbx.invites) != 3 || pbx.invites[0].Number != "600" || pbx.invites[0].AuthUser != "phone-kitchen" {
		t.Errorf("the PBX saw %+v", pbx.invites)
	}
	if pbx.byes != 3 {
		t.Errorf("the PBX saw %d BYEs, want 3", pbx.byes)
	}
}
