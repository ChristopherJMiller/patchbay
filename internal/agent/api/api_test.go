package api_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

func TestEmptyMarshalsListsAsArrays(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	got, err := json.Marshal(api.Empty(at, api.AgentStatus{StartedAt: at}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"generatedAt":"2026-10-14T12:00:00Z",` +
		`"agent":{"startedAt":"2026-10-14T12:00:00Z","asteriskUp":false},` +
		`"devices":[],"unknownDevices":[],"firmware":{"files":[]}}`
	if string(got) != want {
		t.Fatalf("empty document:\n got %s\nwant %s", got, want)
	}
}

func TestObservationsRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	in := api.Observations{
		GeneratedAt: at,
		Agent: api.AgentStatus{
			StartedAt: at, AppliedVersion: "7c1e", LastApplyVersion: "9f00", LastApplyError: "verify: endpoint phone-desk missing",
			LastApplyAt: &at, AsteriskUp: true, AsteriskVersion: "22.6.0",
		},
		Devices: []api.DeviceObservations{{
			Device: "phone-desk",
			MAC:    "D0C78915E8C8",
			Fetches: []api.Fetch{{
				File: "SEPD0C78915E8C8.cnf.xml", Transport: api.TransportTFTP,
				Result: api.FetchServed, At: at, Source: "192.168.20.100", Version: "7c1e",
			}},
			Registration: &api.Registration{
				Contact: "sip:100@192.168.20.100:5060;transport=tcp", Address: "192.168.20.100",
				UserAgent: "Cisco-CP7945G/9.4.2", CallID: "00112233-4455", ExpiresAt: at.Add(time.Hour),
				Qualify: api.QualifyUnknown,
			},
			Web: &api.WebScrape{
				At: at, Address: "192.168.20.100",
				DeviceInformation:    &api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S", AppLoadID: "jar45sip.9-4-2SR4-3.sbn"},
				NetworkConfiguration: &api.NetworkConfiguration{CallManager1: "192.168.0.240  Active"},
				DeviceLog:            []string{"Registered 100"},
			},
			Calls: 1, StreamCalls: 1, Page: "abc",
		}},
		UnknownDevices: []api.UnknownDevice{{
			MAC: "D0C78915EBB5", File: "SEPD0C78915EBB5.cnf.xml", Source: "192.168.20.101",
			FirstSeen: at, LastSeen: at,
		}},
		Firmware: api.FirmwareStore{Files: []api.FirmwareFileStatus{{Load: "SIP45.9-4-2SR4-3S", Name: "term45.default.loads", Present: true}}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out api.Observations
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the document:\n in %+v\nout %+v", in, out)
	}
}

func TestFirmwarePath(t *testing.T) {
	if got, want := api.FirmwarePath("SIP45.9-4-2SR4-3S", "apps45.9-4-2SR4-3.sbn"),
		"/v1/firmware/SIP45.9-4-2SR4-3S/apps45.9-4-2SR4-3.sbn"; got != want {
		t.Fatalf("FirmwarePath = %q, want %q", got, want)
	}
}

func TestFirmwareUploadResponseJSON(t *testing.T) {
	got, err := json.Marshal(api.FirmwareUploadResponse{
		Load: "SIP45.9-4-2SR4-3S", Name: "term45.default.loads", Size: 673,
		Error: "the manifest has no SHA-256 for term45.default.loads yet",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"load":"SIP45.9-4-2SR4-3S","name":"term45.default.loads","size":673,"stored":false,` +
		`"error":"the manifest has no SHA-256 for term45.default.loads yet"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestPushJSONRoundTrip(t *testing.T) {
	req := api.PushRequest{
		Device: "phone-desk", PageVersion: "abc", Page: "<CiscoIPPhoneText/>", Beep: true, Clear: true,
		Execute: []string{"Key:Services"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"device":"phone-desk","pageVersion":"abc","page":"\u003cCiscoIPPhoneText/\u003e","beep":true,"clear":true,"execute":["Key:Services"]}`
	if string(raw) != want {
		t.Fatalf("request:\n got %s\nwant %s", raw, want)
	}
	var back api.PushRequest
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(req, back) {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	resp := api.PushResponse{Sent: true, Error: "e", Phone: "status 0"}
	raw, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var rback api.PushResponse
	if err := json.Unmarshal(raw, &rback); err != nil || rback != resp {
		t.Fatalf("response round trip: %+v, %v", rback, err)
	}
	if string(raw) != `{"sent":true,"error":"e","phone":"status 0"}` {
		t.Fatalf("response: %s", raw)
	}
}
