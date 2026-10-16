package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// TestWebPagesParse: the three pages are built from scrape's page types,
// so scrape's parsers read back exactly the scripted Web; before any Web
// and while it is down they answer 503; a linphone phone has none.
func TestWebPagesParse(t *testing.T) {
	web := script.Web{
		VersionID: "SIP45.9-4-2SR4-3S", AppLoadID: "jar45sip.9-4-2SR4-3.sbn", BootLoadID: "tnp45.8-3-1-21a.bin",
		HardwareRevision: "1.0", SerialNumber: "FCH1234ABCD", ModelNumber: "CP-7945G",
		AdminVLAN: "20", CallManager1: "192.168.0.240  Active",
		Log: []string{"Trust List update failed", "TFTP Timeout : SEPD0C78915E8C8.cnf.xml"},
	}
	sc := ciscoScript(script.Step{Label: "wait", WaitSignal: &script.WaitSignal{Name: "down"}},
		script.Step{Label: "down", SetWeb: &script.Web{Down: true}})
	sc.Web = &web
	h := startRun(t, sc, runOptions{TFTPPort: 1})

	code, b := h.get(scrape.PathDeviceInformation)
	info, err := scrape.ParseDeviceInformation(b)
	if code != http.StatusOK || err != nil {
		t.Fatalf("%s: %d %v\n%s", scrape.PathDeviceInformation, code, err, b)
	}
	if info.VersionID != web.VersionID || info.AppLoadID != web.AppLoadID || info.BootLoadID != web.BootLoadID ||
		info.HardwareRevision != web.HardwareRevision || info.SerialNumber != web.SerialNumber || info.ModelNumber != web.ModelNumber {
		t.Errorf("device information %+v", info)
	}
	code, b = h.get(scrape.PathNetworkConfiguration)
	netcfg, err := scrape.ParseNetworkConfiguration(b)
	if code != http.StatusOK || err != nil || netcfg.AdminVLAN != "20" || netcfg.CallManager1 != web.CallManager1 {
		t.Errorf("network configuration: %d %+v %v", code, netcfg, err)
	}
	code, b = h.get(scrape.PathDeviceLog)
	lines, err := scrape.ParseDeviceLog(b)
	if code != http.StatusOK || err != nil || !slices.Equal(lines, web.Log) {
		t.Errorf("device log: %d %q %v", code, lines, err)
	}

	h.signal("down")
	h.atStep("")
	for _, path := range []string{scrape.PathDeviceInformation, scrape.PathNetworkConfiguration, scrape.PathDeviceLog} {
		if code, _ := h.get(path); code != http.StatusServiceUnavailable {
			t.Errorf("%s while down: %d", path, code)
		}
	}

	// No Web yet: 503.
	h = startRun(t, ciscoScript(), runOptions{TFTPPort: 1})
	if code, _ := h.get(scrape.PathDeviceInformation); code != http.StatusServiceUnavailable {
		t.Errorf("before any web: %d", code)
	}
	// Linphone has no pages.
	h = startRun(t, linphoneScript("http://127.0.0.1:1/p/not-a-secret-token"), runOptions{})
	if code, _ := h.get(scrape.PathDeviceInformation); code != http.StatusNotFound {
		t.Errorf("linphone: %d", code)
	}
}

// TestWebPagesAsTheReal7945: with NetworkConfigurationBreaksOff and
// EmptyDeviceLog the pages are what a real CP-7945G on SIP 9.4(2)SR4-3
// serves (2026-10-15): /NetworkConfigurationX ends mid-element where the
// phone's renderer throws, with the scripted fields before the break, and
// /DeviceLogX is a 200 with nothing in it. scrape's parsers read them so.
func TestWebPagesAsTheReal7945(t *testing.T) {
	web := script.Web{
		VersionID: "*SIP45.9-4-2SR4-3S*", AppLoadID: "jar45sip.9-4-2SR4-3.sbn", ModelNumber: "CP-7945G",
		AdminVLAN: "", CallManager1: "192.168.0.240   Active",
		NetworkConfigurationBreaksOff: true, EmptyDeviceLog: true,
	}
	sc := ciscoScript()
	sc.Web = &web
	h := startRun(t, sc, runOptions{TFTPPort: 1})

	code, b := h.get(scrape.PathDeviceInformation)
	info, err := scrape.ParseDeviceInformation(b)
	if code != http.StatusOK || err != nil || info.VersionID != web.VersionID {
		t.Errorf("%s: %d %+v %v", scrape.PathDeviceInformation, code, info, err)
	}
	code, b = h.get(scrape.PathNetworkConfiguration)
	if !bytes.HasSuffix(b, []byte("<EnergywisePowerLevel>java.lang.NullPointerException")) {
		t.Errorf("%s does not end as the phone's does:\n%s", scrape.PathNetworkConfiguration, b)
	}
	netcfg, err := scrape.ParseNetworkConfiguration(b)
	if code != http.StatusOK || !errors.Is(err, scrape.ErrIncomplete) || netcfg == nil || netcfg.CallManager1 != web.CallManager1 {
		t.Errorf("%s: %d %+v %v", scrape.PathNetworkConfiguration, code, netcfg, err)
	}
	code, b = h.get(scrape.PathDeviceLog)
	lines, err := scrape.ParseDeviceLog(b)
	if code != http.StatusOK || len(b) != 0 || err != nil || lines != nil {
		t.Errorf("%s: %d %q %q %v", scrape.PathDeviceLog, code, b, lines, err)
	}
}

// TestSignalBeforeStep: a signal sent before its step runs is remembered;
// each signal releases one wait; bad names and methods are refused.
func TestSignalBeforeStep(t *testing.T) {
	sc := ciscoScript(
		script.Step{Label: "early", Sleep: &script.Sleep{For: dur(300 * time.Millisecond)}},
		script.Step{Label: "go", WaitSignal: &script.WaitSignal{Name: "go", Within: dur(5 * time.Second)}},
		script.Step{Label: "again", WaitSignal: &script.WaitSignal{Name: "go", Within: dur(200 * time.Millisecond)}},
	)
	sc.ExitWhenDone = true
	h := startRun(t, sc, runOptions{TFTPPort: 1})
	if s := h.state(); s.Label != "early" {
		t.Fatalf("label %q", s.Label)
	}
	if code := h.signal("go"); code != http.StatusNoContent {
		t.Fatalf("signal: %d", code)
	}
	if code := h.signal("Not_A_Label"); code != http.StatusBadRequest {
		t.Errorf("a bad signal name: %d", code)
	}
	if code, _ := h.get(script.SignalPath("go")); code != http.StatusMethodNotAllowed {
		t.Errorf("GET a signal: %d", code)
	}
	err := h.wait(10 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "step 2 (again) waitSignal: no signal go within 200ms") {
		t.Fatalf("one signal released two waits, or the first was lost: %v", err)
	}
	if s := h.r.snapshot(); !slices.Equal(s.Signals, []string{"go"}) {
		t.Errorf("signals %q", s.Signals)
	}
}

// TestStateJSON: State starts with every list empty, never null; it never
// holds a password (only its SHA-256) or the provisioning URL.
func TestStateJSON(t *testing.T) {
	const token = "not-a-secret-state-token"
	h := startRun(t, linphoneScript("http://127.0.0.1:1/p/"+token), runOptions{})
	h.atStep("")
	_, raw := h.get(script.PathState)
	for _, want := range []string{`"mode":"linphone"`, `"step":0`, `"done":true`, `"registered":false`,
		`"fetches":[]`, `"registrations":[]`, `"notifies":[]`, `"calls":[]`, `"signals":[]`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("state lacks %s:\n%s", want, raw)
		}
	}
	if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte("http://")) {
		t.Errorf("state holds the provisioning URL:\n%s", raw)
	}

	const password = "not-a-secret-state-password"
	_, _, opts := deskPhone(t, password)
	h = startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
	), opts)
	h.atStep("")
	_, raw = h.get(script.PathState)
	if bytes.Contains(raw, []byte(password)) || !bytes.Contains(raw, []byte(sha(password))) {
		t.Errorf("state holds the password or lacks its digest:\n%s", raw)
	}
	var s script.State
	if err := json.Unmarshal(raw, &s); err != nil || s.Config == nil || !s.Registered {
		t.Fatalf("state %+v %v", s, err)
	}
}

func linphoneScript(url string, steps ...script.Step) script.Script {
	return script.Script{Mode: script.ModeLinphone, Server: "127.0.0.1", ProvisioningURL: url, Steps: steps}
}

// provisioningServer serves doc once at /p/<token>, as device HTTP does
// (then 404: the token is spent), and counts the GETs and their agents.
type provisioningServer struct {
	*httptest.Server
	mu     sync.Mutex
	gets   int
	agents []string
}

func startProvisioning(t *testing.T, token string, doc []byte) *provisioningServer {
	t.Helper()
	ps := &provisioningServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		ps.gets++
		ps.agents = append(ps.agents, r.UserAgent())
		first := ps.gets == 1
		ps.mu.Unlock()
		if r.URL.Path != "/p/"+token || !first {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(ps.Close)
	return ps
}

// laptopLP is the softphone chris-laptop golden pointed at pbx.
func laptopLP(t *testing.T, pbx *fakePBX, password string) []byte {
	t.Helper()
	doc := string(resolve(readGolden(t, filepath.Join(softphoneGoldens,
		"chris-laptop/provisioning/softphone-chris-laptop.xml")), password))
	doc = strings.ReplaceAll(doc, "192.168.0.240:5060", pbx.addr.String())
	return []byte(strings.ReplaceAll(doc, "192.168.0.240", "127.0.0.1"))
}

// TestRunLinphone: GET the provisioning URL once, parse the lpconfig,
// REGISTER over UDP with its credentials; a second GET of the same URL is
// recorded as notFound.
func TestRunLinphone(t *testing.T) {
	const (
		password = "not-a-secret-laptop"
		token    = "not-a-secret-provisioning-token"
	)
	pbx := startPBX(t, map[string]string{"softphone-chris-laptop": password})
	doc := laptopLP(t, pbx, password)
	ps := startProvisioning(t, token, doc)
	sc := linphoneScript(ps.URL+"/p/"+token,
		script.Step{Label: "provision", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "spent", FetchConfig: &script.FetchConfig{Expect: script.NotFound}},
	)
	h := startRun(t, sc, runOptions{})
	s := h.atStep("")

	if len(s.Fetches) != 2 || s.Fetches[0].File != "provisioning" || s.Fetches[0].Result != script.Served ||
		s.Fetches[0].Bytes != int64(len(doc)) || s.Fetches[1].Result != script.NotFound || s.Fetches[1].Error != "HTTP 404" {
		t.Fatalf("fetches %+v", s.Fetches)
	}
	c := s.Config
	if c == nil || c.Source != "provisioning" || c.User != "150" || c.AuthUser != "softphone-chris-laptop" ||
		c.Transport != "udp" || c.Proxy != pbx.addr.String() || !c.ConfigURICleared || c.PasswordSHA256 != sha(password) {
		t.Fatalf("config %+v", c)
	}
	regs := pbx.okRegisters()
	if len(regs) != 1 || regs[0].Transport != "udp" || regs[0].AuthUser != "softphone-chris-laptop" ||
		!strings.HasPrefix(regs[0].Contact, "sip:150@127.0.0.1:") || regs[0].Expires != 600 {
		t.Fatalf("the PBX saw %+v", regs)
	}
	if !s.Registered {
		t.Error("not registered")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.gets != 2 || ps.agents[0] != userAgentLinphone {
		t.Errorf("GETs %d, agents %q", ps.gets, ps.agents)
	}
}

// TestStepFailureExits: as a process, a failed step exits 1 and says
// which step; the JSON log has the failure; a finished exitWhenDone
// script exits 0; no script is an error.
func TestStepFailureExits(t *testing.T) {
	run := func(scriptJSON string) (int, string, string) {
		cmd := exec.CommandContext(context.Background(), os.Args[0], "run", "--local-ip", "127.0.0.1",
			"--sip-port", strconv.Itoa(freePort(t)))
		cmd.Env = append(os.Environ(), "FAKEPHONE_TEST_MAIN=1", script.EnvScript+"="+scriptJSON)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // the process's own error
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, stdout.String(), stderr.String()
	}
	encode := func(sc script.Script) string {
		sc.WebListen = "127.0.0.1:0"
		s, err := sc.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	code, stdout, stderr := run(encode(ciscoScript(script.Step{Label: "wait",
		WaitSignal: &script.WaitSignal{Name: "never", Within: dur(50 * time.Millisecond)}})))
	if code != 1 || !strings.Contains(stderr, "fakephone: step 0 (wait) waitSignal: no signal never within 50ms") {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"msg":"step"`) || !strings.Contains(stdout, `"phase":"failed"`) {
		t.Errorf("stdout lacks the failed step line:\n%s", stdout)
	}

	sc := ciscoScript(script.Step{Label: "nap", Sleep: &script.Sleep{For: dur(10 * time.Millisecond)}})
	sc.ExitWhenDone = true
	if code, stdout, stderr := run(encode(sc)); code != 0 || !strings.Contains(stdout, `"msg":"done"`) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if code, _, stderr := run(""); code != 1 || !strings.Contains(stderr, "no script") {
		t.Fatalf("no script: exit %d %s", code, stderr)
	}
	if code, _, stderr := run(`{"mode":"cisco"}`); code != 1 || !strings.Contains(stderr, "script:") {
		t.Fatalf("a bad script: exit %d %s", code, stderr)
	}
}

// TestReadScript: --script FILE wins over $FAKEPHONE_SCRIPT.
func TestReadScript(t *testing.T) {
	t.Setenv(script.EnvScript, `{"from":"env"}`)
	if b, err := readScript(""); err != nil || string(b) != `{"from":"env"}` {
		t.Fatalf("env: %s %v", b, err)
	}
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(`{"from":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readScript(path); err != nil || string(b) != `{"from":"file"}` {
		t.Fatalf("file: %s %v", b, err)
	}
	t.Setenv(script.EnvScript, "")
	if _, err := readScript(""); err == nil {
		t.Fatal("no script read")
	}
}

// TestRunNeverLogsSecrets: across a cisco phone's life (config, register,
// a NOTIFY with a new password, a call) and a linphone phone's (a served,
// a spent and an unreachable provisioning URL), no log line, sipgo trace,
// error or State holds a password, the provisioning token or its URL.
func TestRunNeverLogsSecrets(t *testing.T) {
	const (
		password    = "not-a-secret-logged-1"
		newPassword = "not-a-secret-logged-2"
		lpPassword  = "not-a-secret-logged-3"
		token       = "not-a-secret-logged-token"
	)
	secrets := []string{password, newPassword, lpPassword, token, "/p/"}

	pbx, ts, opts := deskPhone(t, password)
	cisco := startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "notify", WaitNotify: &script.WaitNotify{Within: dur(10 * time.Second)}},
		script.Step{Label: "call", Call: &script.Call{Number: "600", Hold: dur(200 * time.Millisecond)}},
		script.Step{Label: "stale", Register: &script.Register{Password: password, Expect: 401}},
	), opts)
	cisco.atStep("notify")
	ts.put("SEP"+deskMAC+".cnf.xml", deskSEP(t, pbx, newPassword, ""))
	pbx.setDeskPassword(newPassword)
	pbx.notify("service-control", "action=restart\r\n")
	cisco.atStep("")

	lpPBX := startPBX(t, map[string]string{"softphone-chris-laptop": lpPassword})
	ps := startProvisioning(t, token, laptopLP(t, lpPBX, lpPassword))
	lp := startRun(t, linphoneScript(ps.URL+"/p/"+token,
		script.Step{Label: "provision", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "spent", FetchConfig: &script.FetchConfig{Expect: script.NotFound}},
	), runOptions{})
	lp.atStep("")
	sc := linphoneScript("http://127.0.0.1:1/p/"+token, script.Step{Label: "down", FetchConfig: &script.FetchConfig{}})
	sc.ExitWhenDone = true
	down := startRun(t, sc, runOptions{})
	runErr := down.wait(10 * time.Second)
	if runErr == nil {
		t.Fatal("a fetch from a closed port succeeded")
	}

	texts := map[string]string{"run error": runErr.Error(), "sipgo": sipLog.String()}
	for name, h := range map[string]*runHarness{"cisco": cisco, "linphone": lp, "down": down} {
		raw, err := json.Marshal(h.r.snapshot()) // "down" has exited: no web server
		if err != nil {
			t.Fatal(err)
		}
		texts[name+" state"] = string(raw)
		texts[name+" log"] = h.out.String()
		texts[name+" SIP log"] = h.log.String()
	}
	if !strings.Contains(texts["cisco log"], `"msg":"registration"`) || !strings.Contains(texts["down state"], "connection refused") {
		t.Fatalf("the logs are not what this test thinks:\n%s\n%s", texts["cisco log"], texts["down state"])
	}
	for name, text := range texts {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s holds %q:\n%s", name, s, text)
			}
		}
	}
}
