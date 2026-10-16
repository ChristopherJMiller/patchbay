package script_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// lifecycle is the kind of script TestPhoneLifecycle runs.
func lifecycle() script.Script {
	return script.Script{
		Mode:   script.ModeCisco,
		Server: "172.18.0.2",
		MAC:    "0200E2E00011",
		Web:    &script.Web{VersionID: "SCCP45.9-4-2SR4-3S", AppLoadID: "jar45sccp.9-4-2SR4-3.sbn"},
		Answer: &script.Answer{DetectHz: []float64{600, 800}},
		Steps: []script.Step{
			{Label: "config", FetchConfig: &script.FetchConfig{}},
			{Label: "wait for the load", Fetch: &script.Fetch{
				File: "{load}.loads", Retry: script.Retry{Every: script.Duration(3 * time.Second), Within: script.Duration(3 * time.Minute)},
			}},
			{Fetch: &script.Fetch{File: "appsE2E.0-0-1.sbn"}},
			{SetWeb: &script.Web{Down: true}},
			{WaitSignal: &script.WaitSignal{Name: "boot"}},
			{SetWeb: &script.Web{VersionID: "E2E45.0-0-1S"}},
			{FetchConfig: &script.FetchConfig{}},
			{WaitSignal: &script.WaitSignal{Name: "register", Within: script.Duration(5 * time.Minute)}},
			{Register: &script.Register{}},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	s := lifecycle()
	enc, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc, `"within":"3m0s"`) || !strings.Contains(enc, `"fetchConfig":{}`) {
		t.Errorf("encoding: %s", enc)
	}
	back, err := script.Parse([]byte(enc))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, s)
	}
}

func TestParseIsStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":   `{"mode":"cisco","server":"10.0.0.1","mac":"0200E2E00011","steps":[],"extra":1}`,
		"trailing data":   `{"mode":"cisco","server":"10.0.0.1","mac":"0200E2E00011","steps":[]} {}`,
		"number duration": `{"mode":"cisco","server":"10.0.0.1","mac":"0200E2E00011","steps":[{"sleep":{"for":5}}]}`,
		"invalid":         `{"mode":"sccp","server":"10.0.0.1","steps":[]}`,
	} {
		if _, err := script.Parse([]byte(doc)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := lifecycle()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	linphone := script.Script{
		Mode: script.ModeLinphone, Server: "172.18.0.2",
		ProvisioningURL: "http://172.18.0.2:8088/p/not-a-secret-token",
		Steps:           []script.Step{{FetchConfig: &script.FetchConfig{}}, {Register: &script.Register{}}},
	}
	if err := linphone.Validate(); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*script.Script){
		"bad mode":         func(s *script.Script) { s.Mode = "sccp" },
		"server not an IP": func(s *script.Script) { s.Server = "pbx.local" },
		"lower-case MAC":   func(s *script.Script) { s.MAC = "0200e2e00011" },
		"cisco with URL":   func(s *script.Script) { s.ProvisioningURL = "http://x/p/y" },
		"tone out of band": func(s *script.Script) { s.ToneHz = 5000 },
		"no action":        func(s *script.Script) { s.Steps = []script.Step{{Label: "nothing"}} },
		"two actions": func(s *script.Script) {
			s.Steps = []script.Step{{Hangup: &script.Hangup{}, Unregister: &script.Unregister{}}}
		},
		"path in a fetch": func(s *script.Script) { s.Steps = []script.Step{{Fetch: &script.Fetch{File: "../x"}}} },
		"retry without step": func(s *script.Script) {
			s.Steps = []script.Step{{FetchConfig: &script.FetchConfig{Retry: script.Retry{Within: 5}}}}
		},
		"bad expect": func(s *script.Script) {
			s.Steps = []script.Step{{Fetch: &script.Fetch{File: "x", Expect: script.Failed}}}
		},
		"bad number": func(s *script.Script) { s.Steps = []script.Step{{Call: &script.Call{Number: "1-00"}}} },
		"open call held": func(s *script.Script) {
			s.Steps = []script.Step{{Call: &script.Call{Number: "600", Open: true, Hold: 5}}}
		},
		"bad signal":        func(s *script.Script) { s.Steps = []script.Step{{WaitSignal: &script.WaitSignal{Name: "Boot"}}} },
		"zero sleep":        func(s *script.Script) { s.Steps = []script.Step{{Sleep: &script.Sleep{}}} },
		"answer status 200": func(s *script.Script) { s.Answer = &script.Answer{Status: 200} },
	} {
		s := lifecycle()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	l := linphone
	l.Steps = []script.Step{{SetWeb: &script.Web{}}}
	if err := l.Validate(); err == nil {
		t.Error("linphone with web pages: valid")
	}
	l = linphone
	l.ProvisioningURL = "ftp://x/p/y"
	if err := l.Validate(); err == nil || strings.Contains(err.Error(), "ftp://") {
		t.Errorf("a bad provisioning URL: %v (and it must not be quoted)", err)
	}
}

// TestWebAsTheReal7945: a Web can ask for the pages a real CP-7945G on SIP
// 9.4(2)SR4-3 serves (a /NetworkConfigurationX that breaks off, an empty
// /DeviceLogX), at start or in a SetWeb; an empty log with lines is
// refused.
func TestWebAsTheReal7945(t *testing.T) {
	real7945 := script.Web{
		VersionID: "*SIP45.9-4-2SR4-3S*", AppLoadID: "jar45sip.9-4-2SR4-3.sbn",
		NetworkConfigurationBreaksOff: true, EmptyDeviceLog: true,
	}
	s := lifecycle()
	s.Web = &real7945
	s.Steps = append(s.Steps, script.Step{SetWeb: &real7945})
	enc, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc, `"networkConfigurationBreaksOff":true`) || !strings.Contains(enc, `"emptyDeviceLog":true`) {
		t.Errorf("encoding: %s", enc)
	}
	back, err := script.Parse([]byte(enc))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, s)
	}

	withLog := real7945
	withLog.Log = []string{"Trust List Update Failed"}
	s = lifecycle()
	s.Web = &withLog
	if err := s.Validate(); err == nil {
		t.Error("an empty device log with lines, at start: valid")
	}
	s = lifecycle()
	s.Steps = append(s.Steps, script.Step{SetWeb: &withLog})
	if err := s.Validate(); err == nil {
		t.Error("an empty device log with lines, in a step: valid")
	}
}

func TestExpand(t *testing.T) {
	if got := script.Expand("SEP{mac}.cnf.xml.sgn", "0200E2E00011", "E2E45.0-0-1S"); got != "SEP0200E2E00011.cnf.xml.sgn" {
		t.Errorf("Expand mac = %q", got)
	}
	if got := script.Expand("{load}.loads", "0200E2E00011", "E2E45.0-0-1S"); got != "E2E45.0-0-1S.loads" {
		t.Errorf("Expand load = %q", got)
	}
	if script.SignalPath("boot") != "/fakephone/signal/boot" || script.HzKey(1000) != "1000" || script.HzKey(437.5) != "437.5" {
		t.Error("SignalPath or HzKey")
	}
}

// The State's JSON is what the harness reads: its keys are part of the
// contract.
func TestStateJSON(t *testing.T) {
	st := script.NewState(script.ModeCisco)
	st.Config = &script.Config{Source: "SEP0200E2E00011.cnf.xml", PasswordSHA256: "ab"}
	st.Calls = []script.CallRecord{{Direction: "in", ToneFrames: map[string]int{script.HzKey(600): 50}}}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"step":0`, `"done":false`, `"registered":false`, `"passwordSHA256":"ab"`,
		`"toneFrames":{"600":50}`, `"fetches":[]`, `"signals":[]`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("State JSON lacks %s: %s", key, b)
		}
	}
	if strings.Contains(string(b), `"end"`) {
		t.Errorf("a zero End is written: %s", b)
	}
}
