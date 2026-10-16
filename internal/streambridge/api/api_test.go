package api_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// TestFailureReasons: the bridge's failure reasons are the Stream status
// reasons the controller copies them into, word for word.
func TestFailureReasons(t *testing.T) {
	for got, want := range map[api.FailureReason]v1alpha1.StreamFailureReason{
		api.FailureSourceFailed: v1alpha1.StreamFailureSourceFailed,
		api.FailureSourceEnded:  v1alpha1.StreamFailureSourceEnded,
		api.FailureTunerBusy:    v1alpha1.StreamFailureTunerBusy,
	} {
		if string(got) != string(want) {
			t.Errorf("api %q != v1alpha1 %q", got, want)
		}
	}
}

// TestJSON pins the document's field names (the controller, the e2e suite
// and docs/streams.md read them).
func TestJSON(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	doc := api.Observations{
		GeneratedAt: at,
		Bridge:      api.BridgeStatus{Version: "v0.2.0", StartedAt: at, StreamsVersion: "abc", StreamsLoadedAt: &at, Calls: 1},
		Streams: []api.StreamObservations{{
			UUID: "89fb6719-3e3d-5ef3-881a-b9f423a8a432", Name: "king5", Number: "705",
			Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun", Listeners: 1,
			Session:     &api.SessionObservations{State: api.SessionPlaying, StartedAt: at, FirstAudioAt: &at, Listeners: 1},
			LastPlayed:  &at,
			LastFailure: &api.Failure{At: at, Reason: api.FailureTunerBusy, Message: "pool hdhomerun (1 at a time) is playing kiro7"},
			Calls:       2,
		}},
		Pools: []api.PoolObservations{{Name: "hdhomerun", Max: 1, Sessions: []api.PoolSession{{
			Source: "http://192.168.0.58:5004/auto/v5.1", Streams: []string{"king5"}, State: api.SessionPlaying, Listeners: 1,
		}}}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"generatedAt"`, `"bridge"`, `"startedAt"`, `"streamsVersion"`, `"streamsLoadedAt"`, `"calls"`, `"unknownCalls"`, `"fullCalls"`,
		`"streams"`, `"uuid"`, `"name"`, `"number"`, `"source"`, `"pool"`, `"listeners"`, `"session"`, `"state":"Playing"`,
		`"firstAudioAt"`, `"lastPlayed"`, `"lastFailure"`, `"reason":"TunerBusy"`, `"message"`,
		`"pools"`, `"max"`, `"sessions"`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("no %s in %s", field, raw)
		}
	}
	var back api.Observations
	if err := json.Unmarshal(raw, &back); err != nil || back.Streams[0].LastFailure.Reason != api.FailureTunerBusy {
		t.Errorf("round trip: %+v %v", back, err)
	}
	if api.DefaultAudioSocketPort != 9092 || api.DefaultControlPort != 9093 {
		t.Error("ports")
	}
}
