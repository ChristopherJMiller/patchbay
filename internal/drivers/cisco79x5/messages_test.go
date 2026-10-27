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
		// Ruling R21: what a 7945's Latin-1 font can show.
		{"emoji-title", []drivers.MessageLine{{Sent: at(6, 40), Title: "💧 Leak detected", Text: "Water under the sink"}}},
		{"typographic", []drivers.MessageLine{{Sent: at(17, 15), Text: "Mom’s “pie” – ready…"}}},
		{"latin1", []drivers.MessageLine{{Sent: at(8, 0), Title: "Café", Text: "café au lait"}}},
		{"all-emoji", []drivers.MessageLine{{Sent: at(20, 0), Title: "🎉", Text: "🎉🎊"}}},
		// A message living over 12 h shows its weekday (2026-10-26 is a
		// Monday); 12 h exactly, or no expiry known, the time alone.
		{"long-lived", []drivers.MessageLine{
			{Sent: at(9, 0), Expires: at(9, 0).Add(24 * time.Hour), Title: "Boiler", Text: "Service due"},
			{Sent: at(8, 30), Expires: at(8, 30).Add(12 * time.Hour), Text: "Twelve hours exactly"},
			{Sent: at(8, 0), Text: "No expiry known"},
		}},
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

// TestRenderMessagesScreenText (ruling R21): titles and texts as a 7945's
// Latin-1 font shows them, one line each: typographic punctuation as
// ASCII, other runes past U+00FF dropped, runs of spaces collapsed and
// trimmed, an empty title left out and an empty text "(no text)".
func TestRenderMessagesScreenText(t *testing.T) {
	sent := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		title, text, want string
	}{
		{"💧 Leak detected", "Water under the sink", "09:00  Leak detected: Water under the sink"},
		{"", "Mom’s “pie” – ready…", `09:00  Mom's "pie" - ready...`},
		{"", "café", "09:00  café"},
		{"Ünïcödé ÿ", "naïve — Æsir ½", "09:00  Ünïcödé ÿ: naïve - Æsir ½"},
		{"🎉", "🎉🎊", "09:00  (no text)"},
		{"  a   b  ", "  x   y  ", "09:00  a b: x y"},
		{"one—two", "Ⅻ o’clock 日本", "09:00  one-two: o'clock"},
		// A dropped rune never brings two "@" together.
		{"", "@💧@secret:a/b@💧@", "09:00  @secret:a/b@"},
	} {
		got, err := cisco79x5.New().RenderMessages("cisco-7945", []drivers.MessageLine{{Sent: sent, Title: c.title, Text: c.text}}, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Text string `xml:"Text"`
		}
		if err := xml.Unmarshal(got, &page); err != nil {
			t.Fatalf("%q/%q: not valid XML: %v", c.title, c.text, err)
		}
		if page.Text != c.want {
			t.Errorf("%q/%q renders %q, want %q", c.title, c.text, page.Text, c.want)
		}
		for _, r := range page.Text {
			if r > 0xFF {
				t.Errorf("%q/%q renders %q, past Latin-1", c.title, c.text, r)
			}
		}
	}
}

// TestRenderMessagesWeekday: the weekday is the message's own (in loc),
// decided by its lifetime alone, so a page renders the same whatever the
// day it is rendered on.
func TestRenderMessagesWeekday(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC) // Sunday 20:00 in Los Angeles
	for _, c := range []struct {
		life time.Duration
		want string
	}{
		{time.Hour, "20:00  x"},
		{12 * time.Hour, "20:00  x"},
		{12*time.Hour + time.Second, "Sun 20:00  x"},
		{24 * time.Hour, "Sun 20:00  x"},
		{-time.Hour, "20:00  x"},
	} {
		got, err := cisco79x5.New().RenderMessages("cisco-7945", []drivers.MessageLine{{Sent: sent, Expires: sent.Add(c.life), Text: "x"}}, la)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Text string `xml:"Text"`
		}
		if err := xml.Unmarshal(got, &page); err != nil || page.Text != c.want {
			t.Errorf("lifetime %s: %q (%v), want %q", c.life, page.Text, err, c.want)
		}
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
	// The cap is on the text as shown (ruling R21): two lines of 7500 bytes
	// each that show as 1500 both fit.
	emoji := []drivers.MessageLine{
		{Sent: lines[0].Sent, Text: strings.Repeat("💧x", 1500)},
		{Sent: lines[0].Sent, Text: strings.Repeat("💧y", 1500)},
	}
	if got, err = cisco79x5.New().RenderMessages("cisco-7945", emoji, time.UTC); err != nil {
		t.Fatal(err)
	}
	want := "12:00  " + strings.Repeat("x", 1500) + "\n12:00  " + strings.Repeat("y", 1500)
	if err := xml.Unmarshal(got, &page); err != nil || page.Text != want {
		t.Errorf("lines that fit once mapped: err %v, %d bytes, want %d", err, len(page.Text), len(want))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
