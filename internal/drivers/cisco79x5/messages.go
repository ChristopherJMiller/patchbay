package cisco79x5

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// maxMessageText is the most bytes the page's Text holds: Cisco's
// CiscoIPPhoneText takes 4000 (IP Phone Services SDK). The cap is on the
// value before XML escaping, as UTF-8.
const maxMessageText = 4000

var _ drivers.MessageRenderer = (*Driver)(nil)

type textPage struct {
	XMLName  xml.Name      `xml:"CiscoIPPhoneText"`
	Title    string        `xml:"Title"`
	Prompt   string        `xml:"Prompt"`
	Text     string        `xml:"Text"`
	SoftKeys []textSoftKey `xml:"SoftKeyItem"`
}

type textSoftKey struct {
	Name     string `xml:"Name"`
	URL      string `xml:"URL"`
	Position int    `xml:"Position"`
}

// RenderMessages renders the page the phone opens when a message is pushed:
// a CiscoIPPhoneText whose Text is one line per message, newest first
// (lines is already in that order), "15:04  Title: text" in loc, with
// URGENT before the title of an urgent one. Title and text are free text a
// person typed, so they go through drivers.UserText. When the lines do not
// fit in maxMessageText bytes, the oldest are dropped and the last line says
// how many ("+N older"). With no lines the page says "No messages".
func (d *Driver) RenderMessages(model string, lines []drivers.MessageLine, loc *time.Location) ([]byte, error) {
	if _, ok := d.profile(model); !ok {
		return nil, fmt.Errorf("cisco79x5: %w: %q", drivers.ErrUnknownModel, model)
	}
	if loc == nil {
		loc = time.UTC
	}
	text := make([]string, len(lines))
	for i, l := range lines {
		var b strings.Builder
		b.WriteString(l.Sent.In(loc).Format("15:04"))
		b.WriteString("  ")
		if l.Urgent {
			b.WriteString("URGENT ")
		}
		if title := drivers.UserText(l.Title); title != "" {
			b.WriteString(title)
			b.WriteString(": ")
		}
		b.WriteString(drivers.UserText(l.Text))
		text[i] = b.String()
	}

	prompt := "No messages"
	switch len(lines) {
	case 0:
	case 1:
		prompt = "1 message"
	default:
		prompt = fmt.Sprintf("%d messages", len(lines))
	}
	page := textPage{
		Title:    "patchbay",
		Prompt:   prompt,
		Text:     fitMessages(text),
		SoftKeys: []textSoftKey{{Name: "Exit", URL: "SoftKey:Exit", Position: 1}},
	}
	body, err := xml.MarshalIndent(page, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cisco79x5: marshal message page: %w", err)
	}
	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.Write(body)
	out.WriteString("\n")
	return out.Bytes(), nil
}

// fitMessages joins the lines (newest first) with newlines, dropping the
// oldest until the text, with its "+N older" line, is within
// maxMessageText bytes.
func fitMessages(lines []string) string {
	n := len(lines)
	for keep := n; keep >= 1; keep-- {
		s := strings.Join(lines[:keep], "\n")
		if keep < n {
			s += fmt.Sprintf("\n+%d older", n-keep)
		}
		if len(s) <= maxMessageText {
			return s
		}
	}
	if n == 0 {
		return ""
	}
	// Even the newest line alone is too long: cut it.
	tail := ""
	if n > 1 {
		tail = fmt.Sprintf("\n+%d older", n-1)
	}
	return cutUTF8(lines[0], maxMessageText-len(tail)) + tail
}

// cutUTF8 cuts s to at most n bytes, on a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
