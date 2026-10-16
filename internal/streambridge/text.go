package streambridge

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ellipsis ends a message that was cut.
const ellipsis = "…"

// printable makes text from outside the bridge (ffmpeg's stderr, a
// source's answer, a decode error) safe to report: invalid UTF-8 becomes
// "?", every control, format or other non-printing character and every run
// of white space one space, and the ends are trimmed. A result longer than
// max bytes is cut at a character boundary and ends in "…", within max.
func printable(s string, max int) string {
	var b strings.Builder
	space := false
	for i, w := 0, 0; i < len(s); i += w {
		var r rune
		r, w = utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && w == 1:
			r = '?'
		case unicode.IsSpace(r) || !unicode.IsPrint(r):
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= max {
		return out
	}
	cut := max - len(ellipsis)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return strings.TrimRight(out[:cut], " ") + ellipsis
}
