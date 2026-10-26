package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The contacts a Linphone downloads (misc/contacts-vcard-list), read as
// liblinphone reads them with belcard (linphone-sdk 79aefb33, 2026-10-26),
// so a file Linphone would refuse fails here too:
//
//   - bctoolbox Utils::unfold: lines end in CRLF, or all in a bare LF; a
//     line break followed by a space or tab is removed;
//   - belcard/src/vcard_rules: a list of one or more cards, each
//     BEGIN:VCARD, then at once VERSION:4.0, then at least one property,
//     then END:VCARD; an FN value is TEXT-CHAR (line 281): "\\", "\,", "\n",
//     whitespace, non-ASCII, and printable ASCII but a bare "," or "\";
//   - belcard_unescape_string undoes "\\" and "\,";
//   - liblinphone Vcard::getSipAddresses: the addresses are the IMPP values
//     that start with sip: (or sips:).

// vCardTextValue is belcard's TEXT-CHAR, the grammar of an FN value.
var vCardTextValue = regexp.MustCompile(`^(?:\\\\|\\,|\\[nN]|[\t ]|[^\x00-\x7F]|[\x21-\x2B\x2D-\x5B\x5D-\x7E])*$`)

// vCardName is a property name, with an optional group.
var vCardName = regexp.MustCompile(`^(?:[A-Za-z0-9-]+\.)?[A-Za-z0-9-]+$`)

// parseVCards reads a vCard 4.0 list into its contacts, in file order.
func parseVCards(b []byte) ([]script.Contact, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("the contacts are not UTF-8")
	}
	text := string(b)
	eol := "\r\n"
	if !strings.Contains(text, eol) {
		eol = "\n"
	}
	var lines []string // unfolded
	for _, l := range strings.Split(text, eol) {
		if l != "" && (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var out []script.Contact
	for i := 0; i < len(lines); {
		if i+1 >= len(lines) || lines[i] != "BEGIN:VCARD" || lines[i+1] != "VERSION:4.0" {
			return nil, fmt.Errorf("card %d does not open BEGIN:VCARD, VERSION:4.0", len(out)+1)
		}
		var c script.Contact
		props := 0
		j := i + 2
		for ; j < len(lines) && lines[j] != "END:VCARD"; j++ {
			nameParams, value, ok := strings.Cut(lines[j], ":")
			name, _, _ := strings.Cut(nameParams, ";")
			if !ok || !vCardName.MatchString(name) {
				return nil, fmt.Errorf("card %d: %q is not a property", len(out)+1, lines[j])
			}
			props++
			switch strings.ToUpper(name) {
			case "FN":
				if !vCardTextValue.MatchString(value) {
					return nil, fmt.Errorf("card %d: FN %q is not vCard text", len(out)+1, value)
				}
				c.Name = strings.ReplaceAll(strings.ReplaceAll(value, `\\`, `\`), `\,`, `,`)
			case "IMPP":
				if c.URI == "" && (strings.HasPrefix(value, "sip:") || strings.HasPrefix(value, "sips:")) {
					c.URI = value
				}
			}
		}
		if j == len(lines) {
			return nil, fmt.Errorf("card %d has no END:VCARD", len(out)+1)
		}
		if props == 0 {
			return nil, fmt.Errorf("card %d has no property", len(out)+1)
		}
		out = append(out, c)
		i = j + 1
	}
	if len(out) == 0 {
		return nil, errors.New("the contacts hold no card")
	}
	return out, nil
}
