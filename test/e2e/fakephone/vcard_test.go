package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// TestParseVCards: every contacts file compile renders parses as Linphone
// reads it, names unescaped.
func TestParseVCards(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(compileGoldens, "*/want/http/directory/contacts.vcf"))
	if err != nil || len(files) < 5 {
		t.Fatalf("goldens %v %v", files, err)
	}
	for _, f := range files {
		cards, err := parseVCards(readGolden(t, f))
		if err != nil || len(cards) == 0 {
			t.Errorf("%s: %v", f, err)
		}
		for _, c := range cards {
			if c.Name == "" || !strings.HasPrefix(c.URI, "sip:") {
				t.Errorf("%s: card %+v", f, c)
			}
		}
	}
	cards, err := parseVCards(readGolden(t, filepath.Join(compileGoldens, "hostile-labels/want/http/directory/contacts.vcf")))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(cards))
	for i, c := range cards {
		names[i] = c.Name
	}
	if !slices.Contains(names, "A label of sixty-four characters, cut for every phone's screen!!") ||
		!slices.Contains(names, `Mom & Dad's \ ;-- --; @secret:patchbay-ami/password@`) {
		t.Errorf("names %q: want the comma and the backslash back", names)
	}

	// Folded lines, and LF line ends.
	folded := "BEGIN:VCARD\nVERSION:4.0\nFN:Groove\n  Salad\nIMPP:sip:750@pbx\nEND:VCARD\n"
	if got, err := parseVCards([]byte(folded)); err != nil || !slices.Equal(got, []script.Contact{{Name: "Groove Salad", URI: "sip:750@pbx"}}) {
		t.Errorf("folded: %+v %v", got, err)
	}
}

// TestParseVCardsRefuses: what belcard refuses, Linphone imports nothing
// from.
func TestParseVCardsRefuses(t *testing.T) {
	card := func(lines ...string) []byte {
		return []byte(strings.Join(lines, "\r\n") + "\r\n")
	}
	for name, b := range map[string][]byte{
		"empty":              nil,
		"no VERSION":         card("BEGIN:VCARD", "FN:x", "END:VCARD"),
		"VERSION not second": card("BEGIN:VCARD", "FN:x", "VERSION:4.0", "END:VCARD"),
		"vCard 3":            card("BEGIN:VCARD", "VERSION:3.0", "FN:x", "END:VCARD"),
		"no property":        card("BEGIN:VCARD", "VERSION:4.0", "END:VCARD"),
		"no END":             card("BEGIN:VCARD", "VERSION:4.0", "FN:x"),
		"bare comma":         card("BEGIN:VCARD", "VERSION:4.0", "FN:a, b", "END:VCARD"),
		"escaped semicolon":  card("BEGIN:VCARD", "VERSION:4.0", `FN:a\; b`, "END:VCARD"),
		"bare backslash":     card("BEGIN:VCARD", "VERSION:4.0", `FN:a \ b`, "END:VCARD"),
		"not a property":     card("BEGIN:VCARD", "VERSION:4.0", "FN x", "END:VCARD"),
		"not UTF-8":          card("BEGIN:VCARD", "VERSION:4.0", "FN:\xff", "END:VCARD"),
	} {
		if got, err := parseVCards(b); err == nil {
			t.Errorf("%s: parsed %+v", name, got)
		}
	}
}
