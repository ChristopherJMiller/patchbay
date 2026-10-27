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

// weekdayPast is the lifetime past which a message's time carries its
// weekday ("Mon 09:00"): one that lives up to 24 h can still be on the
// screen the next day, when "09:00" alone would read as today's.
const weekdayPast = 12 * time.Hour

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
// URGENT before the title of an urgent one, and the weekday before the time
// of one that lives (Expires less Sent) over weekdayPast ("Mon 15:04"): the
// line depends only on the message, never on now, so a page is not pushed
// again when a day turns. Title and text are free text a
// person typed, so they go through drivers.UserText, then screenText: what
// is left of a title is no title when empty, and an empty text reads
// noText. When the lines do not fit in maxMessageText bytes, the oldest are
// dropped and the last line says how many ("+N older"). With no lines the
// page says "No messages".
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
		layout := "15:04"
		if l.Expires.Sub(l.Sent) > weekdayPast {
			layout = "Mon 15:04"
		}
		b.WriteString(l.Sent.In(loc).Format(layout))
		b.WriteString("  ")
		if l.Urgent {
			b.WriteString("URGENT ")
		}
		if title := screenText(drivers.UserText(l.Title)); title != "" {
			b.WriteString(title)
			b.WriteString(": ")
		}
		body := screenText(drivers.UserText(l.Text))
		if body == "" {
			body = noText
		}
		b.WriteString(body)
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

// noText is a message line's text when none of it can be shown.
const noText = "(no text)"

// asciiFor are the runes past Latin-1 that screenText keeps, as ASCII, and
// the no-break space, as a space.
var asciiFor = map[rune]string{
	'\u2018': "'", '\u2019': "'", // left and right single quotes
	'\u201C': `"`, '\u201D': `"`, // left and right double quotes
	'\u2013': "-", '\u2014': "-", // en and em dashes
	'\u2026': "...", // the ellipsis
	'\u00A0': " ",   // the no-break space
}

// screenText is s as a 7945 can show it (ruling R21: until the bench says
// otherwise, its English locale has a Latin-1 font, and a page it cannot
// render or parse would read Shown while blank): typographic quotes,
// dashes and the ellipsis become ASCII and a no-break space a space, every
// other rune past U+00FF (an emoji in "💧 Leak detected") is dropped, and
// each run of spaces left becomes one, none at either end. Dropping a rune
// can bring two "@" together, so the result goes through drivers.UserText
// again.
func screenText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false // a space is owed before the next character
	put := func(t string) {
		for _, r := range t {
			if r == ' ' {
				space = b.Len() > 0
				continue
			}
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	for _, r := range s {
		if t, ok := asciiFor[r]; ok {
			put(t)
		} else if r <= 0xFF {
			put(string(r))
		}
	}
	return drivers.UserText(b.String())
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
