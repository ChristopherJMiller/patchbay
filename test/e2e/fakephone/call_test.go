package main

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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
	// The fake counts a BYE after answering it, so the last one may still
	// be on its way into the count when the call returns.
	waitFor(t, 5*time.Second, "3 BYEs at the PBX", func() bool {
		pbx.mu.Lock()
		defer pbx.mu.Unlock()
		return pbx.byes == 3
	})
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	if len(pbx.invites) != 3 || pbx.invites[0].Number != "600" || pbx.invites[0].AuthUser != "phone-kitchen" {
		t.Errorf("the PBX saw %+v", pbx.invites)
	}
}

// e2eMediaLine is test/e2e's mediaLine (kube_test.go), copied: the call's
// media line must keep every field the suite reads.
var e2eMediaLine = regexp.MustCompile(`sent=(\d+) received=(\d+) pcmu=\d+ toneFrames=(\d+) maxRatio=\S+ firstTone=\S+ source=([^\s"]*) loud=(\d+) voice=(\d+)`)

// callEndLine is the end of the media line: answered=<d> endedBy=<side>,
// right after voice=N.
var callEndLine = regexp.MustCompile(`voice=\d+ answered=(\S+) endedBy=(local|remote)\n`)

// runCall runs `fakephone call 600` as kitchen against pbx with extra flags.
func runCall(t *testing.T, pbx *fakePBX, extra ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(append([]string{"call", "600", "--timeout", "10s",
		"--server", pbx.addr.String(), "--transport", "udp", "--user", "101",
		"--auth-user", "phone-kitchen", "--password", "not-a-secret-kitchen",
		"--local-ip", "127.0.0.1", "--sip-port", strconv.Itoa(freePort(t)),
		"--tail", "200ms", "--tone-hz", "1000", "--min-tone-frames", "20"}, extra...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// callEndOf parses the media line's answered and endedBy.
func callEndOf(t *testing.T, out string) (time.Duration, string) {
	t.Helper()
	if !e2eMediaLine.MatchString(out) {
		t.Fatalf("the media line lost a field the e2e suite reads: %q", out)
	}
	m := callEndLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no answered=… endedBy=… after voice=N: %q", out)
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatalf("answered=%s: %v", m[1], err)
	}
	return d, m[2]
}

// TestCallEndedByRemote: when the PBX hangs up first (its BYE), the phone
// stops sending at once, sends no BYE of its own, and says so: endedBy=remote
// and how long the call was up. --expect-ended-by=remote passes, local fails.
func TestCallEndedByRemote(t *testing.T) {
	pbx := startPBX(t, map[string]string{"phone-kitchen": "not-a-secret-kitchen"})
	pbx.mu.Lock()
	pbx.hangupAfter = 700 * time.Millisecond
	pbx.mu.Unlock()

	start := time.Now()
	out, err := runCall(t, pbx, "--duration", "5s", "--expect-ended-by", "remote")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a call the PBX ends: %v\n%s", err, out)
	}
	answered, by := callEndOf(t, out)
	if by != endedByRemote || answered < 600*time.Millisecond || answered > 1500*time.Millisecond {
		t.Errorf("answered=%s endedBy=%s, want about 700ms, remote", answered, by)
	}
	if took > 3*time.Second {
		t.Errorf("the call took %s: it did not stop at the PBX's BYE", took)
	}
	stats := e2eMediaLine.FindStringSubmatch(out)
	if sent, _ := strconv.Atoi(stats[1]); sent >= 250 || sent < 20 {
		t.Errorf("sent %d packets in a call the PBX ended after 0.7 s", sent)
	}

	_, err = runCall(t, pbx, "--duration", "5s", "--expect-ended-by", "local")
	if err == nil || !strings.Contains(err.Error(), "the call was ended by remote, want local") {
		t.Errorf("--expect-ended-by local, ended by the PBX: %v", err)
	}
	// The fake counts its BYE once the phone has answered it, so the last
	// may still be on its way into the count.
	waitFor(t, 5*time.Second, "the PBX's 2 BYEs", func() bool {
		pbx.mu.Lock()
		defer pbx.mu.Unlock()
		return pbx.byesSent == 2
	})
	pbx.mu.Lock()
	defer pbx.mu.Unlock()
	if pbx.byes != 0 {
		t.Errorf("the phone sent %d BYEs in calls the PBX ended", pbx.byes)
	}
}

// TestCallEndedByLocal: a call the phone ends after --duration says
// endedBy=local and its length; --expect-ended-by=remote fails it.
func TestCallEndedByLocal(t *testing.T) {
	pbx := startPBX(t, map[string]string{"phone-kitchen": "not-a-secret-kitchen"})
	out, err := runCall(t, pbx, "--duration", "1s", "--expect-ended-by", "local")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	answered, by := callEndOf(t, out)
	if by != endedByLocal || answered < time.Second || answered > 3*time.Second {
		t.Errorf("answered=%s endedBy=%s, want about 1.2s, local", answered, by)
	}
	if _, err := runCall(t, pbx, "--duration", "1s", "--expect-ended-by", "remote"); err == nil ||
		!strings.Contains(err.Error(), "the call was ended by local, want remote") {
		t.Errorf("--expect-ended-by remote, ended by the phone: %v", err)
	}
	if _, err := runCall(t, pbx, "--expect-ended-by", "pbx"); err == nil || !strings.Contains(err.Error(), "--expect-ended-by must be") {
		t.Errorf("--expect-ended-by pbx: %v", err)
	}
	waitFor(t, 5*time.Second, "2 BYEs at the PBX", func() bool {
		pbx.mu.Lock()
		defer pbx.mu.Unlock()
		return pbx.byes == 2
	})
}

// TestCallFirstToneWithin: --expect-first-tone-within passes when the
// --expect-hz tone comes soon enough after the first packet, and fails
// when it comes later or never.
func TestCallFirstToneWithin(t *testing.T) {
	pbx := startPBX(t, map[string]string{"phone-kitchen": "not-a-secret-kitchen"})
	pbx.setAnswerHz(600)
	if out, err := runCall(t, pbx, "--duration", "1s", "--expect-hz", "600", "--expect-first-tone-within", "1s"); err != nil {
		t.Fatalf("the far tone within 1 s: %v\n%s", err, out)
	}
	if _, err := runCall(t, pbx, "--duration", "1s", "--expect-hz", "600", "--expect-first-tone-within", "1ns"); err == nil ||
		!strings.Contains(err.Error(), "the 600 Hz tone first came after") {
		t.Errorf("within 1ns: %v", err)
	}
	if _, err := runCall(t, pbx, "--duration", "1s", "--expect-hz", "800", "--expect-tone=false", "--expect-first-tone-within", "1s"); err == nil ||
		!strings.Contains(err.Error(), "the 800 Hz tone never came") {
		t.Errorf("a tone that never comes: %v", err)
	}
}

func TestCheckCallEnd(t *testing.T) {
	mo := mediaOptions{ExpectHz: 1100}
	for _, tc := range []struct {
		res          mediaResult
		by, want     string
		within       time.Duration
		errSubstring string
	}{
		{mediaResult{}, endedByLocal, endedByAny, 0, ""},
		{mediaResult{}, endedByRemote, endedByRemote, 0, ""},
		{mediaResult{}, endedByLocal, endedByRemote, 0, "ended by local, want remote"},
		{mediaResult{FirstTone: time.Second}, endedByRemote, endedByAny, 3 * time.Second, ""},
		{mediaResult{FirstTone: 4 * time.Second}, endedByRemote, endedByAny, 3 * time.Second, "first came after 4s, want it within 3s"},
		{mediaResult{}, endedByRemote, endedByAny, 3 * time.Second, "the 1100 Hz tone never came"},
	} {
		err := checkCallEnd(tc.res, mo, tc.by, tc.want, tc.within)
		if (tc.errSubstring == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.errSubstring)) {
			t.Errorf("%+v ended by %s, want %s within %s: %v, want %q", tc.res, tc.by, tc.want, tc.within, err, tc.errSubstring)
		}
	}
}
