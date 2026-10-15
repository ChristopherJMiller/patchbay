package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAMI answers one connection like Asterisk's manager: a banner,
// Challenge (AuthType MD5), Login by MD5 challenge-response only (password
// "pw"; a plaintext Secret is refused, so the test proves none is sent),
// ModuleLoad (Error for app_system.so), Logoff, and an event without an
// ActionID before every response.
func fakeAMI(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		w := bufio.NewWriter(conn)
		_, _ = w.WriteString("Asterisk Call Manager/11.0.0\r\n")
		_ = w.Flush()
		r := bufio.NewReader(conn)
		for {
			req := map[string]string{}
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					break
				}
				k, v, _ := strings.Cut(line, ": ")
				req[k] = v
			}
			resp, msg, extra := "Success", "", ""
			switch req["Action"] {
			case "Challenge":
				if req["AuthType"] != "MD5" {
					resp, msg = "Error", "Must specify AuthType"
				} else {
					extra = "Challenge: 424242\r\n"
				}
			case "Login":
				sum := md5.Sum([]byte("424242pw"))
				if req["AuthType"] != "MD5" || req["Key"] != hex.EncodeToString(sum[:]) || req["Secret"] != "" || req["Events"] != "off" {
					resp, msg = "Error", "Authentication failed"
				}
			case "ModuleLoad":
				if req["Module"] == "app_system.so" {
					resp, msg = "Error", "Could not load module."
				}
			}
			_, _ = w.WriteString("Event: FullyBooted\r\nStatus: Fully Booted\r\n\r\n")
			_, _ = w.WriteString("Response: " + resp + "\r\nActionID: " + req["ActionID"] + "\r\n" + extra + "Message: " + msg + "\r\n\r\n")
			_ = w.Flush()
		}
	}()
	return ln.Addr().String()
}

func TestAMI(t *testing.T) {
	addr := fakeAMI(t)
	t.Setenv("FAKEPHONE_AMI_SECRET", "pw")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ami", "--addr", addr,
		"ModuleLoad Module=app_system.so LoadType=load", "ModuleLoad Module=app_echo.so LoadType=reload"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "action=0 ModuleLoad Module=app_system.so LoadType=load response=Error message=\"Could not load module.\"\n" +
		"action=1 ModuleLoad Module=app_echo.so LoadType=reload response=Success message=\"\"\n"
	if out.String() != want {
		t.Fatalf("ami printed\n%s\nwant\n%s", out.String(), want)
	}
}

func TestAMILoginRefused(t *testing.T) {
	addr := fakeAMI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := dialAMI(ctx, addr, "patchbay-agent", "wrong"); err == nil || !strings.Contains(err.Error(), "login refused") {
		t.Fatalf("login with a wrong secret: %v", err)
	}
}

func TestAMINeedsASecret(t *testing.T) {
	t.Setenv("FAKEPHONE_AMI_SECRET", "")
	root := newRootCmd()
	root.SetArgs([]string{"ami", "--addr", "127.0.0.1:1", "Ping"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "AMI_SECRET") {
		t.Fatalf("ami without a secret: %v", err)
	}
}

func TestParseAMIAction(t *testing.T) {
	action, fields, err := parseAMIAction("ModuleLoad Module=../x/app_system.so LoadType=load")
	if err != nil || action != "ModuleLoad" || len(fields) != 2 || fields[0] != [2]string{"Module", "../x/app_system.so"} {
		t.Fatalf("parse: %q %v %v", action, fields, err)
	}
	for _, bad := range []string{"", "ModuleLoad Module", "ModuleLoad =x"} {
		if _, _, err := parseAMIAction(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestFindProcesses finds this test process by its own command line.
func TestFindProcesses(t *testing.T) {
	found, err := findProcesses("/proc", []string{filepath.Base(os.Args[0])})
	if err != nil {
		t.Fatal(err)
	}
	var mine string
	for pid, status := range found {
		if pid == strconv.Itoa(os.Getpid()) {
			mine = status
		}
	}
	if !strings.Contains(mine, "Uid:") || !strings.Contains(mine, "CapEff:") || strings.Contains(mine, "VmRSS") {
		t.Fatalf("own status:\n%s", mine)
	}
	if found, _ := findProcesses("/proc", []string{"no-such-binary-anywhere"}); len(found) != 0 {
		t.Errorf("matched %v", found)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(ok); got != "read "+ok+": ok bytes=5" {
		t.Errorf("readable file: %q", got)
	}
	if got := readFile(filepath.Join(dir, "missing")); !strings.Contains(got, "error: ") {
		t.Errorf("missing file: %q", got)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.WriteFile(locked, []byte("secret"), 0o000); err != nil {
			t.Fatal(err)
		}
		if got := readFile(locked); !strings.Contains(got, "permission denied") || strings.Contains(got, "secret") {
			t.Errorf("unreadable file: %q", got)
		}
	}
}

// tone returns one 20 ms µ-law frame of a sine at freq.
func tone(freq float64) []byte {
	g := &toneGenerator{freq: freq, amplitude: 0.5}
	return g.nextFrame()
}

func TestClassifyULaw(t *testing.T) {
	silence := bytes.Repeat([]byte{0xFF}, samplesPerFrame)
	for name, tc := range map[string]struct {
		frame     []byte
		loud, sit bool
	}{
		"silence":   {silence, false, false},
		"SIT 950":   {tone(950), true, true},
		"SIT 1400":  {tone(1400), true, true},
		"SIT 1800":  {tone(1800), true, true},
		"voice-ish": {voiceish(), true, false},
	} {
		if loud, sit := classifyULaw(tc.frame); loud != tc.loud || sit != tc.sit {
			t.Errorf("%s: loud=%v sit=%v, want %v %v", name, loud, sit, tc.loud, tc.sit)
		}
	}
}

// voiceish is a frame with energy spread over several frequencies, none of
// them the SIT tone's.
func voiceish() []byte {
	out := make([]byte, samplesPerFrame)
	for i := range out {
		t := float64(i) / sampleRate
		v := 0.2 * math.MaxInt16 * (math.Sin(2*math.Pi*300*t) + math.Sin(2*math.Pi*620*t) + math.Sin(2*math.Pi*2500*t))
		out[i] = linearToULaw(int16(v))
	}
	return out
}

func TestCheckMedia(t *testing.T) {
	mo := mediaOptions{ToneHz: 1000}
	for name, tc := range map[string]struct {
		res              mediaResult
		tone             bool
		audio, voice     time.Duration
		wantErrSubstring string
	}{
		"nothing back":      {mediaResult{}, false, 0, 0, "no RTP"},
		"echo ok":           {mediaResult{Received: 150, ToneFrames: 140}, true, 0, 0, ""},
		"echo without tone": {mediaResult{Received: 150, ToneFrames: 3}, true, 0, 0, "tone came back in 3"},
		"prompt ok":         {mediaResult{Received: 200, Loud: 150, Voice: 100}, false, 2 * time.Second, time.Second, ""},
		"prompt too short":  {mediaResult{Received: 200, Loud: 60, Voice: 50}, false, 2 * time.Second, time.Second, "1.2s of non-silent"},
		"SIT tone only":     {mediaResult{Received: 200, Loud: 150, Voice: 0}, false, 2 * time.Second, time.Second, "not the SIT tone"},
	} {
		err := checkMedia(tc.res, mo, tc.tone, 50, tc.audio, tc.voice)
		switch {
		case tc.wantErrSubstring == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.wantErrSubstring != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSubstring)):
			t.Errorf("%s: %v, want %q", name, err, tc.wantErrSubstring)
		}
	}
}
