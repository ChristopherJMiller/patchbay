package compile_test

// The release B settings contract (docs/plans/release-b.md §4): the field
// names the chart renders into settings.json, and the defaults the
// dialplan's time limit uses. compile's own tests are the routing stage's;
// this file is the contract's.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestStreamSettingsJSON(t *testing.T) {
	s := compile.Settings{
		StreamBridge: "patchbay-streams.telephony.svc:9092",
		Streams: compile.StreamSettings{
			CallLimit:  metav1.Duration{Duration: 2 * time.Hour},
			TunerPools: []compile.TunerPool{{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1}},
		},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"streamBridge":"patchbay-streams.telephony.svc:9092"`,
		`"streams":{"callLimit":"2h0m0s","tunerPools":[{"name":"hdhomerun","hosts":["192.168.0.58"],"max":1}]}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("settings JSON has no %s:\n%s", want, raw)
		}
	}
	var back compile.Settings
	if err := json.Unmarshal([]byte(`{"streams":{"callLimit":"90m","tunerPools":[{"name":"a","hosts":["h"],"max":2}]}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Streams.Limit() != 90*time.Minute || !slices.Equal(back.Streams.TunerPools[0].Hosts, []string{"h"}) {
		t.Errorf("parsed %+v", back.Streams)
	}

	// Release A's settings (no streams) still encode without the field.
	raw, err = json.Marshal(compile.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["streams"]; ok {
		t.Errorf("empty stream settings are written: %s", raw)
	}
	if (compile.StreamSettings{}).Limit() != compile.DefaultCallLimit || compile.DefaultCallLimit != 2*time.Hour {
		t.Error("the default call limit is not 2 h")
	}
	if compile.MinCallLimit > compile.DefaultCallLimit || compile.DefaultCallLimit > compile.MaxCallLimit {
		t.Error("the call limit bounds do not hold the default")
	}
}

// TestStreamModules: the modules a stream call needs are loaded, and the
// 8 kHz AudioSocket application is not (plan §2(a)).
func TestStreamModules(t *testing.T) {
	mods := compile.Modules()
	for _, m := range []string{"res_audiosocket.so", "chan_audiosocket.so", "func_timeout.so", "codec_resample.so", "app_dial.so"} {
		if !slices.Contains(mods, m) {
			t.Errorf("%s is not loaded", m)
		}
	}
	if slices.Contains(mods, "app_audiosocket.so") {
		t.Error("app_audiosocket.so is loaded; streams use the channel driver")
	}
}
