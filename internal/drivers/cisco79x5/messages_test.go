package cisco79x5_test

import (
	"encoding/xml"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

func TestRenderMessages(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 26, h, m, 0, 0, la).UTC() }
	for _, c := range []struct {
		name  string
		lines []drivers.MessageLine
	}{
		{"one", []drivers.MessageLine{{Sent: at(9, 5), Title: "Door", Text: "Package at the front door"}}},
		{"three", []drivers.MessageLine{
			{Sent: at(18, 30), Title: "Dinner", Text: "Ready when you are"},
			{Sent: at(12, 0), Text: "No title here"},
			{Sent: at(7, 45), Title: "Alarm", Text: "Garage open"},
		}},
		{"urgent", []drivers.MessageLine{{Sent: at(23, 59), Title: "Smoke", Text: "Check the kitchen", Urgent: true}}},
		{"urgent-untitled", []drivers.MessageLine{{Sent: at(1, 2), Text: "Call home", Urgent: true}}},
		{"hostile", []drivers.MessageLine{{Sent: at(10, 0), Title: `R&D <x> "y"`, Text: "@@secret:a/b@@"}}},
		{"empty", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := cisco79x5.New().RenderMessages("cisco-7945", c.lines, la)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), "@@") {
				t.Errorf("page holds \"@@\": %s", got)
			}
			var page struct {
				XMLName xml.Name `xml:"CiscoIPPhoneText"`
			}
			if err := xml.Unmarshal(got, &page); err != nil {
				t.Fatalf("not valid XML: %v", err)
			}
			golden(t, "messages/"+c.name+".xml", got)
		})
	}
}

func TestRenderMessagesUnknownModel(t *testing.T) {
	if _, err := cisco79x5.New().RenderMessages("cisco-7940", nil, time.UTC); err == nil {
		t.Error("unknown model rendered")
	}
}

func TestRenderMessagesOverflow(t *testing.T) {
	var lines []drivers.MessageLine
	for i := range 60 {
		lines = append(lines, drivers.MessageLine{
			Sent: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
			Text: string(rune('a'+i%26)) + strings.Repeat("x", 90),
		})
	}
	got, err := cisco79x5.New().RenderMessages("cisco-7945", lines, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Text string `xml:"Text"`
	}
	if err := xml.Unmarshal(got, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Text) > 4000 {
		t.Errorf("Text is %d bytes, over 4000", len(page.Text))
	}
	if !utf8.ValidString(page.Text) {
		t.Error("Text is not valid UTF-8")
	}
	rows := strings.Split(page.Text, "\n")
	last := rows[len(rows)-1]
	kept := len(rows) - 1
	if want := "+" + itoa(60-kept) + " older"; last != want {
		t.Errorf("last line = %q, want %q", last, want)
	}
	if kept < 30 || kept >= 60 {
		t.Errorf("kept %d lines", kept)
	}
	if !strings.HasPrefix(rows[0], "12:00  a") {
		t.Errorf("newest line not first: %q", rows[0])
	}
	// One line far over the cap is cut on a rune boundary.
	big := []drivers.MessageLine{{Sent: lines[0].Sent, Text: strings.Repeat("é", 3000)}, lines[1]}
	got, err = cisco79x5.New().RenderMessages("cisco-7945", big, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(got, &page); err != nil || len(page.Text) > 4000 || !utf8.ValidString(page.Text) || !strings.HasSuffix(page.Text, "\n+1 older") {
		t.Errorf("oversize line: err %v, %d bytes, suffix %q", err, len(page.Text), page.Text[max(0, len(page.Text)-12):])
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
