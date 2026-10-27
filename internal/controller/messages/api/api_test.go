package api_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
)

func ptr[T any](v T) *T { return &v }

// TestRoundTrip: every type survives JSON, and the field names are pinned
// (Home Assistant's rest_command and the e2e suite write them).
func TestRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		in   any
		out  any
		json string
	}{
		"MessageRequest": {
			messagesapi.MessageRequest{To: []string{"desk"}, Title: "Door", Text: "Front door open", Urgent: true, Expires: "30m", Key: "door"},
			&messagesapi.MessageRequest{},
			`{"to":["desk"],"title":"Door","text":"Front door open","urgent":true,"expires":"30m","key":"door"}`,
		},
		"MessageResponse": {
			messagesapi.MessageResponse{Names: []string{"desk-abc"}}, &messagesapi.MessageResponse{}, `{"names":["desk-abc"]}`,
		},
		"WithdrawRequest": {
			messagesapi.WithdrawRequest{Key: "door", To: []string{"desk"}}, &messagesapi.WithdrawRequest{}, `{"key":"door","to":["desk"]}`,
		},
		"WithdrawResponse": {
			messagesapi.WithdrawResponse{Withdrawn: 2}, &messagesapi.WithdrawResponse{}, `{"withdrawn":2}`,
		},
		"HouseUpdate": {
			messagesapi.HouseUpdate{Quiet: ptr(true), Away: ptr(false)}, &messagesapi.HouseUpdate{}, `{"quiet":true,"away":false}`,
		},
		"House": {
			messagesapi.House{Quiet: true, Away: false, UpdatedAt: at}, &messagesapi.House{},
			`{"quiet":true,"away":false,"updatedAt":"2026-10-26T12:00:00Z"}`,
		},
		"Error": {messagesapi.Error{Error: "nope"}, &messagesapi.Error{}, `{"error":"nope"}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != c.json {
				t.Fatalf("JSON = %s, want %s", raw, c.json)
			}
			if err := json.Unmarshal(raw, c.out); err != nil {
				t.Fatal(err)
			}
			back, _ := json.Marshal(c.out)
			if string(back) != c.json {
				t.Errorf("round trip = %s, want %s", back, c.json)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	ok := messagesapi.MessageRequest{To: []string{"desk"}, Text: "hi"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("minimal request: %v", err)
	}
	for _, e := range []string{"", "0s", "1h", "90m", "24h"} {
		r := ok
		r.Expires = e
		if err := r.Validate(); err != nil {
			t.Errorf("expires %q: %v", e, err)
		}
	}
	full := messagesapi.MessageRequest{To: []string{"desk", "a.b"}, Title: strings.Repeat("é", 32), Text: strings.Repeat("é", 256), Key: "door-1"}
	if err := full.Validate(); err != nil {
		t.Errorf("limits: %v", err)
	}
	for name, mut := range map[string]func(*messagesapi.MessageRequest){
		"no to":           func(r *messagesapi.MessageRequest) { r.To = nil },
		"empty to entry":  func(r *messagesapi.MessageRequest) { r.To = []string{""} },
		"bad to":          func(r *messagesapi.MessageRequest) { r.To = []string{"Desk"} },
		"no text":         func(r *messagesapi.MessageRequest) { r.Text = "" },
		"long text":       func(r *messagesapi.MessageRequest) { r.Text = strings.Repeat("a", 257) },
		"control in text": func(r *messagesapi.MessageRequest) { r.Text = "a\nb" },
		"c1 in text":      func(r *messagesapi.MessageRequest) { r.Text = "a\u0085b" },
		"long title":      func(r *messagesapi.MessageRequest) { r.Title = strings.Repeat("a", 33) },
		"control title":   func(r *messagesapi.MessageRequest) { r.Title = "a\tb" },
		"negative":        func(r *messagesapi.MessageRequest) { r.Expires = "-1h" },
		"too long":        func(r *messagesapi.MessageRequest) { r.Expires = "25h" },
		"just over":       func(r *messagesapi.MessageRequest) { r.Expires = "24h1s" },
		"not a duration":  func(r *messagesapi.MessageRequest) { r.Expires = "x" },
		"key slash":       func(r *messagesapi.MessageRequest) { r.Key = "a/b" },
		"key dot":         func(r *messagesapi.MessageRequest) { r.Key = "a.b" },
	} {
		r := ok
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate = nil, want an error", name)
		}
	}
}

func TestExpiresDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"": time.Hour, "0s": 0, "30m": 30 * time.Minute, "24h": 24 * time.Hour} {
		r := messagesapi.MessageRequest{To: []string{"desk"}, Text: "x", Expires: in}
		got, err := r.ExpiresIn()
		if err != nil || got != want {
			t.Errorf("ExpiresIn(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestWithdrawValidate(t *testing.T) {
	for _, r := range []messagesapi.WithdrawRequest{{Key: "door"}, {Key: "door", To: []string{"desk"}}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, r := range []messagesapi.WithdrawRequest{{}, {Key: "A"}, {Key: "a/b"}, {Key: "door", To: []string{"Bad"}}} {
		if err := r.Validate(); err == nil {
			t.Errorf("%+v: Validate = nil, want an error", r)
		}
	}
}

func TestHouseUpdateValidate(t *testing.T) {
	if err := (messagesapi.HouseUpdate{}).Validate(); err == nil {
		t.Error("HouseUpdate{} validated")
	}
	for _, u := range []messagesapi.HouseUpdate{{Quiet: ptr(false)}, {Away: ptr(true)}, {Quiet: ptr(true), Away: ptr(true)}} {
		if err := u.Validate(); err != nil {
			t.Errorf("%+v: %v", u, err)
		}
	}
}

func TestParseHouse(t *testing.T) {
	h, err := messagesapi.ParseHouse([]byte(`{"quiet":true,"away":false,"updatedAt":"2026-10-26T12:00:00Z"}` + "\n"))
	if err != nil || !h.Quiet || h.Away || h.UpdatedAt.IsZero() {
		t.Errorf("ParseHouse = %+v, %v", h, err)
	}
	for _, bad := range []string{``, `{"quiet":true,"loud":1}`, `{"quiet":true} {}`, `{"quiet":true}x`, `[]`, `{"quiet":"yes"}`} {
		if _, err := messagesapi.ParseHouse([]byte(bad)); err == nil {
			t.Errorf("ParseHouse(%q) = nil error", bad)
		}
	}
}
