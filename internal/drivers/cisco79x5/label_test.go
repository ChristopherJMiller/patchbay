package cisco79x5_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

func TestScreenLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Desk", "Desk"},
		{"Front Door", "FrontDoor"},
		{"abcdefghijkl", "abcdefghijkl"},                   // 12: unchanged
		{"abcdefghijklm", "abcdefghijkl"},                  // 13: cut
		{strings.Repeat("x", 31), strings.Repeat("x", 12)}, // 31
		{"Upstairs Office Phone", "UpstairsOffi"},
		{"Küche Oben", "KücheOben"},
		{"ÄÖÜäöüßÄÖÜäöüß", "ÄÖÜäöüßÄÖÜäö"}, // multi-byte runes are never split
		{"電話 電話 電話 電話 電話 電話 電話", "電話電話電話電話電話電話"},
		{"   ", ""},                // all spaces
		{"a\u00a0b\u2003c", "abc"}, // any Unicode space
		{`R&D; "Lab" <1> Küche`, `R&D;"Lab"<1>`}, // XML metacharacters kept: Render escapes them
		{"", ""},
	} {
		got := cisco79x5.ScreenLabel(c.in)
		if got != c.want {
			t.Errorf("ScreenLabel(%q) = %q, want %q", c.in, got, c.want)
		}
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 12 || strings.ContainsAny(got, " \u00a0") {
			t.Errorf("ScreenLabel(%q) = %q breaks the phoneLabel rules", c.in, got)
		}
	}
}

func TestLineLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Desk", "Desk"},
		{"Front Door", "Front Door"},
		{strings.Repeat("x", 30), strings.Repeat("x", 30)}, // 30: unchanged
		{strings.Repeat("x", 31), strings.Repeat("x", 30)}, // 31: cut
		{"Upstairs Office Phone", "Upstairs Office Phone"},
		{strings.Repeat("ü", 31), strings.Repeat("ü", 30)},
		{`R&D; "Lab" <1> Küche`, `R&D; "Lab" <1> Küche`},
		{"   ", "   "},
	} {
		got := cisco79x5.LineLabel(c.in)
		if got != c.want {
			t.Errorf("LineLabel(%q) = %q, want %q", c.in, got, c.want)
		}
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 30 {
			t.Errorf("LineLabel(%q) = %q is over 30 runes or invalid", c.in, got)
		}
	}
}
