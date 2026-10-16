package streambridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

func TestPrintable(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"  padded\t", "padded"},
		{"two\nlines\r\n", "two lines"},
		{"bell\a and escape\x1b[31m", "bell and escape [31m"},
		{"runs   of \t\t spaces", "runs of spaces"},
		{"bad \xff utf-8", "bad ? utf-8"},
		{"bidi \u202e override", "bidi override"},
		{"no-break\u00a0space", "no-break space"},
		{"café ✓", "café ✓"},
	} {
		if got := printable(c.in, 100); got != c.want {
			t.Errorf("printable(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPrintableBound(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	got := printable(long, api.MaxMessageBytes)
	if len(got) > api.MaxMessageBytes || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("%d bytes, suffix %q, valid %v", len(got), got[len(got)-3:], utf8.ValidString(got))
	}
	if exact := strings.Repeat("a", api.MaxMessageBytes); printable(exact, api.MaxMessageBytes) != exact {
		t.Error("a message of exactly the bound was cut")
	}
	if got := printable(strings.Repeat("a", 10), 2); got != "" && len(got) > 2 {
		t.Errorf("a bound below the ellipsis: %q", got)
	}
}
