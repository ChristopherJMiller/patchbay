package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// TestRunCiscoLifecycle walks a 79x5 through what TestPhoneLifecycle
// needs: the config fetch, a load's .loads retried until it is served,
// its firmware, the .sgn of a factory reset, the web server down while it
// "boots" and up again with the new load, a signal, then REGISTER over TCP
// with the parsed credentials and the config's expiry, refreshed at half
// what the PBX granted (a grant of 2 s: under twice the 79x5's 5 s delta).
func TestRunCiscoLifecycle(t *testing.T) {
	const password = "not-a-secret-lifecycle"
	pbx, ts, opts := deskPhone(t, password)
	pbx.mu.Lock()
	pbx.maxGrant = 2 // refresh every second
	pbx.mu.Unlock()
	sep := deskSEP(t, pbx, password, "")
	ts.put("SIP45.9-4-2SR4-3S.loads", []byte("not a real load list"))
	ts.setServeAfter("SIP45.9-4-2SR4-3S.loads", 2)
	ts.put("apps45.9-4-2ES26.sbn", make([]byte, 3000))

	sc := ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "loads", Fetch: &script.Fetch{File: "{load}.loads",
			Retry: script.Retry{Every: dur(50 * time.Millisecond), Within: dur(5 * time.Second)}}},
		script.Step{Label: "firmware", Fetch: &script.Fetch{File: "apps45.9-4-2ES26.sbn"}},
		script.Step{Label: "sgn", Fetch: &script.Fetch{File: "SEP{mac}.cnf.xml.sgn", Expect: script.NotFound}},
		script.Step{Label: "down", SetWeb: &script.Web{Down: true}},
		script.Step{Label: "boot", WaitSignal: &script.WaitSignal{Name: "boot", Within: dur(10 * time.Second)}},
		script.Step{Label: "up", SetWeb: &script.Web{VersionID: "SIP45.9-4-2SR4-3S", AppLoadID: "jar45sip.9-4-2SR4-3.sbn"}},
		script.Step{Label: "register", Register: &script.Register{}},
	)
	sc.Web = &script.Web{VersionID: "SCCP45.9-4-2SR4-3S", AppLoadID: "jar45sccp.9-4-2SR4-3.sbn", ModelNumber: "CP-7945G"}
	h := startRun(t, sc, opts)

	h.atStep("boot")
	if code, _ := h.get(scrape.PathDeviceInformation); code != http.StatusServiceUnavailable {
		t.Fatalf("while booting the web answered %d, want 503", code)
	}
	if code := h.signal("boot"); code != http.StatusNoContent {
		t.Fatalf("signal: %d", code)
	}
	s := h.atStep("")

	// The fetches, in order, with their results.
	var got []string
	for _, f := range s.Fetches {
		got = append(got, f.File+" "+string(f.Result))
	}
	want := []string{
		"SEPD0C78915E8C8.cnf.xml served",
		"SIP45.9-4-2SR4-3S.loads notFound", "SIP45.9-4-2SR4-3S.loads notFound", "SIP45.9-4-2SR4-3S.loads served",
		"apps45.9-4-2ES26.sbn served",
		"SEPD0C78915E8C8.cnf.xml.sgn notFound",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fetches\n%q\nwant\n%q", got, want)
	}
	if f := s.Fetches[0]; f.Bytes != int64(len(sep)) || f.SHA256 != hex.EncodeToString(sha256Of(sep)) {
		t.Errorf("config fetch %+v", f)
	}
	if f := s.Fetches[4]; f.Bytes != 3000 || f.SHA256 == "" {
		t.Errorf("firmware fetch %+v", f)
	}
	if f := s.Fetches[5]; f.Error != "file not found" {
		t.Errorf("sgn fetch error %q", f.Error)
	}

	c := s.Config
	if c == nil || c.Source != "SEPD0C78915E8C8.cnf.xml" || c.User != "100" || c.AuthUser != "phone-desk" ||
		c.PasswordSHA256 != sha(password) || c.Proxy != pbx.addr.String() || c.Transport != "tcp" ||
		c.Expires != 600 || c.PhoneLabel != "Desk" || c.LoadInformation != "SIP45.9-4-2SR4-3S" {
		t.Fatalf("config %+v", c)
	}
	if s.Web.VersionID != "SIP45.9-4-2SR4-3S" || s.Web.Down {
		t.Errorf("web %+v", s.Web)
	}
	code, page := h.get(scrape.PathDeviceInformation)
	if info, err := scrape.ParseDeviceInformation(page); code != http.StatusOK || err != nil || info.VersionID != "SIP45.9-4-2SR4-3S" {
		t.Errorf("after boot: %d %+v %v", code, info, err)
	}

	// REGISTER over TCP as phone-desk asking for the config's 600 s; the
	// PBX grants 2, so the phone refreshes about every second, keeping its
	// Call-ID and counting up its CSeq.
	waitFor(t, 5*time.Second, "a refresh", func() bool { return len(pbx.okRegisters()) >= 3 })
	regs := pbx.okRegisters()
	for i, r := range regs {
		if r.Transport != "tcp" || r.AuthUser != "phone-desk" || r.Expires != 600 || r.Granted != 2 ||
			r.CallID != regs[0].CallID || !strings.HasPrefix(r.Contact, "sip:100@127.0.0.1:") {
			t.Fatalf("register %d: %+v", i, r)
		}
		if i > 0 {
			if r.CSeq <= regs[i-1].CSeq {
				t.Errorf("CSeq went %d -> %d", regs[i-1].CSeq, r.CSeq)
			}
			if gap := r.At.Sub(regs[i-1].At); gap < 700*time.Millisecond || gap > 1600*time.Millisecond {
				t.Errorf("refresh %d came after %s, want about 1s (half of 2s)", i, gap)
			}
		}
	}
	s = h.waitState(5*time.Second, "three registrations recorded", func(s script.State) bool { return len(s.Registrations) >= 3 })
	if !s.Registered {
		t.Fatalf("registered=%t registrations=%+v", s.Registered, s.Registrations)
	}
	if r := s.Registrations[0]; r.Status != 200 || r.Expires != 600 || r.Granted != 2 ||
		r.AuthUser != "phone-desk" || r.PasswordSHA256 != sha(password) {
		t.Errorf("registration record %+v", r)
	}
	if s.Step != len(sc.Steps) || !s.Done || s.Label != "" {
		t.Errorf("step %d label %q done %t", s.Step, s.Label, s.Done)
	}
	if !slices.Equal(s.Signals, []string{"boot"}) {
		t.Errorf("signals %q", s.Signals)
	}
	if err := h.wait(100 * time.Millisecond); !errors.Is(err, errStillRunning) {
		t.Errorf("a phone without exitWhenDone stopped by itself: %v", err)
	}
}

// TestNotifyRefetchesAndReregisters: a service-control NOTIFY makes the
// phone fetch its config again; with a new password in it, it REGISTERs
// with that password at once (not at its next refresh). Other NOTIFYs are
// answered and recorded, and refetch nothing.
func TestNotifyRefetchesAndReregisters(t *testing.T) {
	const (
		oldPassword = "not-a-secret-old"
		newPassword = "not-a-secret-new"
	)
	pbx, ts, opts := deskPhone(t, oldPassword)
	h := startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "other", WaitNotify: &script.WaitNotify{Event: "refer", Within: dur(10 * time.Second)}},
		script.Step{Label: "notify", WaitNotify: &script.WaitNotify{Within: dur(10 * time.Second)}},
		script.Step{Label: "end", WaitSignal: &script.WaitSignal{Name: "end"}},
	), opts)
	h.atStep("other")

	// Answered and recorded, nothing refetched: another action, another
	// event. A waitNotify ends only on its own event.
	if code := pbx.notify("service-control", "action=apply\r\n"); code != 200 {
		t.Fatalf("NOTIFY answered %d", code)
	}
	h.waitState(5*time.Second, "the first NOTIFY", func(s script.State) bool { return len(s.Notifies) == 1 })
	if s := h.state(); s.Label != "other" {
		t.Fatalf("a NOTIFY of another event ended the step: %q", s.Label)
	}
	if code := pbx.notify("refer", "SIP/2.0 200 OK"); code != 200 {
		t.Fatalf("NOTIFY answered %d", code)
	}
	if s := h.atStep("notify"); len(s.Fetches) != 1 || len(s.Notifies) != 2 {
		t.Fatalf("a NOTIFY that asks for no refetch refetched: %+v", s.Fetches)
	}

	ts.put("SEP"+deskMAC+".cnf.xml", deskSEP(t, pbx, newPassword, "DeskTwo"))
	pbx.setDeskPassword(newPassword)
	before := len(pbx.okRegisters())
	body := "action=restart\r\nRegisterCallId={abc}\r\nConfigVersionStamp={00000000-0000-0000-0000-000000000000}\r\n"
	sent := time.Now()
	if code := pbx.notify("service-control", body); code != 200 {
		t.Fatalf("NOTIFY answered %d", code)
	}
	s := h.atStep("end")
	n := s.Notifies[2]
	if n.Event != "service-control" || n.Body != body || !n.Refetched ||
		!slices.Equal(n.SubscriptionState, []string{"terminated", "active"}) {
		t.Fatalf("notify record %+v", n)
	}
	if s.Notifies[0].Refetched || s.Notifies[1].Event != "refer" || s.Notifies[1].Refetched {
		t.Errorf("other NOTIFYs %+v", s.Notifies[:2])
	}
	if len(s.Fetches) != 2 || s.Config.PhoneLabel != "DeskTwo" || s.Config.PasswordSHA256 != sha(newPassword) {
		t.Fatalf("after the refetch: fetches %+v config %+v", s.Fetches, s.Config)
	}
	waitFor(t, 3*time.Second, "a REGISTER with the new password", func() bool { return len(pbx.okRegisters()) > before })
	if r := pbx.okRegisters()[before]; r.At.Sub(sent) > 2*time.Second {
		t.Errorf("re-registered %s after the NOTIFY, want at once", r.At.Sub(sent))
	}
	s = h.waitState(5*time.Second, "the new registration recorded", func(s script.State) bool {
		last := s.Registrations[len(s.Registrations)-1]
		return last.PasswordSHA256 == sha(newPassword) && last.Status == 200
	})
	if !s.Registered {
		t.Error("not registered after the re-REGISTER")
	}

	// A check-sync refetches too; the config is unchanged, so no REGISTER.
	before = len(pbx.registerLog())
	if code := pbx.notify("check-sync", ""); code != 200 {
		t.Fatalf("NOTIFY answered %d", code)
	}
	s = h.waitState(5*time.Second, "the check-sync handled", func(s script.State) bool {
		return len(s.Notifies) == 4 && s.Notifies[3].Refetched
	})
	if len(s.Fetches) != 3 {
		t.Errorf("fetches after check-sync: %+v", s.Fetches)
	}
	time.Sleep(300 * time.Millisecond)
	if after := len(pbx.registerLog()); after != before {
		t.Errorf("an unchanged config re-registered (%d -> %d REGISTERs)", before, after)
	}
}

// TestStopRefreshingAndUnregister: stopRefreshing leaves the contact to
// lapse (no more REGISTERs; Registered turns false at the expiry);
// unregister sends Expires: 0 and stops refreshing.
func TestStopRefreshingAndUnregister(t *testing.T) {
	const password = "not-a-secret-refresh"
	pbx, _, opts := deskPhone(t, password)
	pbx.mu.Lock()
	pbx.maxGrant = 1
	pbx.mu.Unlock()
	h := startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "refreshing", Sleep: &script.Sleep{For: dur(1200 * time.Millisecond)}},
		script.Step{Label: "stop", StopRefreshing: &script.StopRefreshing{}},
		script.Step{Label: "silent", WaitSignal: &script.WaitSignal{Name: "again"}},
		script.Step{Label: "again", Register: &script.Register{}},
		script.Step{Label: "unregister", Unregister: &script.Unregister{}},
	), opts)

	h.atStep("silent")
	if n := len(pbx.okRegisters()); n < 3 {
		t.Fatalf("%d REGISTERs while refreshing for 1.2s at half of 1s, want at least 3", n)
	}
	stopped := len(pbx.registerLog())
	h.waitState(3*time.Second, "the registration to lapse", func(s script.State) bool { return !s.Registered })
	time.Sleep(600 * time.Millisecond)
	if n := len(pbx.registerLog()); n != stopped {
		t.Fatalf("%d REGISTERs after stopRefreshing", n-stopped)
	}

	h.signal("again")
	s := h.atStep("")
	last := s.Registrations[len(s.Registrations)-1]
	if last.Expires != 0 || last.Status != 200 || s.Registered {
		t.Fatalf("after unregister: %+v registered=%t", last, s.Registered)
	}
	regs := pbx.registerLog()
	if r := regs[len(regs)-1]; r.Expires != 0 || r.Status != 200 || r.AuthUser != "phone-desk" {
		t.Fatalf("the PBX's last REGISTER: %+v", r)
	}
	n := len(regs)
	time.Sleep(800 * time.Millisecond)
	if len(pbx.registerLog()) != n {
		t.Error("still refreshing after unregister")
	}
}

// TestRegisterExpectRefused: a register step expecting a refusal (an old
// password) passes on that status, fails on 200, and leaves the
// registration and its refresher alone.
func TestRegisterExpectRefused(t *testing.T) {
	const password = "not-a-secret-current"
	pbx, _, opts := deskPhone(t, password)
	h := startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
		script.Step{Label: "old", Register: &script.Register{Password: "not-a-secret-stale", Expect: 401}},
	), opts)
	s := h.atStep("")
	if len(s.Registrations) != 2 {
		t.Fatalf("registrations %+v", s.Registrations)
	}
	old := s.Registrations[1]
	if old.Status != 401 || old.PasswordSHA256 != sha("not-a-secret-stale") || !s.Registered {
		t.Fatalf("refused register %+v registered=%t", old, s.Registered)
	}
	// The refused one is the last REGISTER refused with credentials: the
	// refresher may have sent another of the registration's since.
	var refused *pbxRegister
	for _, r := range pbx.registerLog() {
		if r.Status == 401 && r.AuthUser != "" {
			refused = &r
		}
	}
	if refused == nil {
		t.Fatal("the PBX refused no REGISTER that carried credentials")
	}
	if refused.CallID == pbx.okRegisters()[0].CallID {
		t.Error("the refused REGISTER reused the registration's Call-ID")
	}

	// Expecting a refusal that does not come fails the step.
	sc := ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "wrong", Register: &script.Register{Expect: 403, NoRefresh: true}},
	)
	sc.ExitWhenDone = true
	h2 := startRun(t, sc, opts)
	if err := h2.wait(10 * time.Second); err == nil || !strings.Contains(err.Error(), "step 1 (wrong) register: REGISTER answered 200, want 403") {
		t.Fatalf("run: %v", err)
	}
}

// TestRunNeedsAConfig: steps that use the config fail before one was read.
func TestRunNeedsAConfig(t *testing.T) {
	for name, st := range map[string]script.Step{
		"register":   {Register: &script.Register{}},
		"unregister": {Unregister: &script.Unregister{}},
		"call":       {Call: &script.Call{Number: "600"}},
		"load":       {Fetch: &script.Fetch{File: "{load}.loads"}},
		"transfer":   {Transfer: &script.Transfer{To: "150"}},
	} {
		t.Run(name, func(t *testing.T) {
			st.Label = name
			sc := ciscoScript(st)
			sc.ExitWhenDone = true
			h := startRun(t, sc, runOptions{TFTPPort: 1})
			err := h.wait(5 * time.Second)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("step 0 (%s)", name)) {
				t.Fatalf("run: %v", err)
			}
		})
	}
}

// TestFetchConfigRetries: a config that is not there yet is fetched again
// until it is (or the retry window closes), and a served config that does
// not parse fails the step.
func TestFetchConfigRetries(t *testing.T) {
	const password = "not-a-secret-retry"
	_, ts, opts := deskPhone(t, password)
	ts.setServeAfter("SEP"+deskMAC+".cnf.xml", 2)
	sc := ciscoScript(script.Step{Label: "config", FetchConfig: &script.FetchConfig{
		Retry: script.Retry{Every: dur(50 * time.Millisecond), Within: dur(5 * time.Second)}}})
	sc.ExitWhenDone = true
	h := startRun(t, sc, opts)
	if err := h.wait(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if s := h.r.snapshot(); len(s.Fetches) != 3 || s.Fetches[2].Result != script.Served || s.Config == nil {
		t.Fatalf("fetches %+v", s.Fetches)
	}

	// Not found within the window: the step fails with what it got.
	sc.Steps[0].FetchConfig.Retry = script.Retry{Every: dur(50 * time.Millisecond), Within: dur(120 * time.Millisecond)}
	ts.remove("SEP" + deskMAC + ".cnf.xml")
	h = startRun(t, sc, opts)
	if err := h.wait(10 * time.Second); err == nil || !strings.Contains(err.Error(), "the fetch was notFound, want served") {
		t.Fatalf("run: %v", err)
	}

	// Served but not a SEP file.
	ts.put("SEP"+deskMAC+".cnf.xml", []byte("<device><nothing/></device>"))
	h = startRun(t, sc, opts)
	if err := h.wait(10 * time.Second); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("run: %v", err)
	}

	// Expecting notFound and getting it; Any takes it too.
	ts.remove("SEP" + deskMAC + ".cnf.xml")
	for _, expect := range []script.Result{script.NotFound, script.Any} {
		sc.Steps[0].FetchConfig = &script.FetchConfig{Expect: expect}
		h = startRun(t, sc, opts)
		if err := h.wait(10 * time.Second); err != nil {
			t.Fatalf("expect %s: %v", expect, err)
		}
	}
}

// TestFetchFailed: a TFTP server that does not answer is a Failed fetch
// with the reason, and Any accepts it.
func TestFetchFailed(t *testing.T) {
	sc := ciscoScript(script.Step{Label: "nothing", Fetch: &script.Fetch{File: "x.bin", Expect: script.Any}})
	sc.ExitWhenDone = true
	h := startRun(t, sc, runOptions{TFTPPort: freePort(t), TFTPTimeout: 100 * time.Millisecond})
	if err := h.wait(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	f := h.r.snapshot().Fetches
	if len(f) != 1 || f[0].Result != script.Failed || f[0].Error == "" {
		t.Fatalf("fetches %+v", f)
	}
}

func TestClassifyTFTP(t *testing.T) {
	for _, tc := range []struct {
		err    string
		result script.Result
		msg    string
	}{
		{"tftp get a from b: code: 1, message: file not found\x00", script.NotFound, "file not found"},
		{"tftp get a from b: code: 2, message: access violation", script.Failed, "TFTP error 2: access violation"},
		{"tftp get a from b: timeout", script.Failed, "tftp get a from b: timeout"},
	} {
		if r, m := classifyTFTP(fmt.Errorf("%s", tc.err)); r != tc.result || m != tc.msg {
			t.Errorf("%q: %s %q", tc.err, r, m)
		}
	}
}

func TestRetryDeadline(t *testing.T) {
	r := newRunner(ciscoScript(), runOptions{Out: &lockedBuffer{}})
	attempts := 0
	err := r.retry(context.Background(), "", script.Retry{Every: dur(10 * time.Millisecond), Within: dur(55 * time.Millisecond)},
		func() (script.Result, error) { attempts++; return script.NotFound, nil })
	if err == nil || attempts < 4 || attempts > 7 {
		t.Fatalf("%d attempts, %v", attempts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.retry(ctx, script.Served, script.Retry{Every: dur(time.Hour), Within: dur(2 * time.Hour)},
		func() (script.Result, error) { return script.Failed, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry: %v", err)
	}
}

// TestRegisteredLapsesWhenRefreshesFail: Registered is whether the phone
// holds a registration that has not lapsed; a refresher whose REGISTERs
// are refused does not keep it true, and keeps trying every RetryAfter.
func TestRegisteredLapsesWhenRefreshesFail(t *testing.T) {
	const password = "not-a-secret-lapse"
	pbx, _, opts := deskPhone(t, password)
	pbx.mu.Lock()
	pbx.maxGrant = 1
	pbx.mu.Unlock()
	opts.RetryAfter = 100 * time.Millisecond
	h := startRun(t, ciscoScript(
		script.Step{Label: "config", FetchConfig: &script.FetchConfig{}},
		script.Step{Label: "register", Register: &script.Register{}},
	), opts)
	h.atStep("")
	pbx.setDeskPassword("not-a-secret-changed")
	s := h.waitState(5*time.Second, "the registration to lapse", func(s script.State) bool { return !s.Registered })
	if last := s.Registrations[len(s.Registrations)-1]; last.Status != 401 {
		t.Fatalf("last registration %+v", last)
	}
	pbx.setDeskPassword(password)
	h.waitState(5*time.Second, "a refresh to succeed again", func(s script.State) bool { return s.Registered })
}

// TestRefreshAfter: a 79x5 refreshes 5 s before its registration expires
// (its SEP file's timerRegisterDelta), so the cisco mode does too; the
// linphone mode, and very short grants, at half.
func TestRefreshAfter(t *testing.T) {
	for _, tc := range []struct {
		mode    script.Mode
		granted int
		want    time.Duration
	}{
		{script.ModeCisco, 600, 595 * time.Second},
		{script.ModeCisco, 11, 6 * time.Second},
		{script.ModeCisco, 10, 5 * time.Second},
		{script.ModeCisco, 2, time.Second},
		{script.ModeLinphone, 600, 300 * time.Second},
		{script.ModeCisco, 0, 7 * time.Second},
	} {
		r := newRunner(script.Script{Mode: tc.mode}, runOptions{RetryAfter: 7 * time.Second})
		if got := r.refreshAfter(tc.granted); got != tc.want {
			t.Errorf("%s, granted %d s: refresh after %s, want %s", tc.mode, tc.granted, got, tc.want)
		}
	}
}
