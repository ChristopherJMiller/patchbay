package install_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// TestText: text that may hold what a device sent becomes valid UTF-8
// with no control character (C0, DEL, C1: a terminal escape or a
// carriage return could rewrite `patchbay status`), each a space, and at
// most limit bytes, cut on a rune boundary with an ellipsis.
func TestText(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		limit    int
		want     string
	}{
		{"plain", "running SIP45.9-4-2SR4-3S", 64, "running SIP45.9-4-2SR4-3S"},
		{"exactly the limit", "abcdef", 6, "abcdef"},
		{"cut", "abcdefghij", 6, "abc…"},
		{"cut before a multi-byte rune", "ab€€€", 8, "ab€…"},
		{"escape and carriage return", "ok\x1b[2J\rspoofed\n", 64, "ok [2J spoofed "},
		{"C1 and DEL", "a\u0085b\u009bc\x7fd", 64, "a b c d"},
		{"invalid UTF-8", "a\xffb", 64, "a�b"},
		{"tiny limit", "abcdef", 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := install.Text(tc.in, tc.limit)
			if got != tc.want {
				t.Errorf("Text(%q, %d) = %q; want %q", tc.in, tc.limit, got, tc.want)
			}
			if len(got) > tc.limit || !utf8.ValidString(got) || strings.IndexFunc(got, unicode.IsControl) >= 0 {
				t.Errorf("Text(%q, %d) = %q: longer than the limit, invalid UTF-8 or holding a control character", tc.in, tc.limit, got)
			}
		})
	}
}

// TestEventNote: an Event note fits events.k8s.io's 1024-byte limit,
// whatever an agent or a device put in it.
func TestEventNote(t *testing.T) {
	long := "config version 0123456789ab was not applied: " + strings.Repeat("endpoint phone-x is missing; ", 100)
	got := install.EventNote(long)
	if len(got) > install.MaxEventNote || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "config version 0123456789ab") {
		t.Errorf("EventNote of %d bytes: %d bytes, %q…", len(long), len(got), got[:40])
	}
	if install.MaxEventNote != 1024 {
		t.Errorf("MaxEventNote %d; events.k8s.io/v1 allows 1024", install.MaxEventNote)
	}
	if got := install.EventNote("unknown phone 0200E2E00001\r\x1b[1A"); got != "unknown phone 0200E2E00001  [1A" {
		t.Errorf("EventNote kept a control character: %q", got)
	}
}
