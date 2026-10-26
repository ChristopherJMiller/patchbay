package compile_test

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// contact is one card of the softphones' contacts, as Linphone reads it.
type contact struct{ Name, URI string }

// vCardText is belcard's TEXT-CHAR, the grammar of an FN value
// (belcard/src/vcard_rules line 281, linphone-sdk 79aefb33): the escapes
// "\\", "\," and "\n", whitespace, any non-ASCII character, and printable
// ASCII but a bare comma or backslash.
var vCardText = regexp.MustCompile(`^(?:\\\\|\\,|\\[nN]|[\t ]|[^\x00-\x7F]|[\x21-\x2B\x2D-\x5B\x5D-\x7E])*$`)

// checkContacts reads ContactsFile the way Linphone does, and fails on
// anything belcard would refuse or RFC 6350 forbids:
//   - CRLF line endings, no line over 75 octets, valid UTF-8;
//   - lines unfolded where a line break is followed by a space or tab
//     (bctoolbox Utils::unfold);
//   - each card BEGIN:VCARD, then at once VERSION:4.0, then exactly one FN
//     whose value is belcard text and one IMPP, then END:VCARD;
//   - the IMPP a sip: URI naming the install's PBX (with its SIP port
//     unless that is 5060), which is all Linphone dials.
//
// It returns the cards in file order, names unescaped as belcard does.
func checkContacts(t *testing.T, s compile.Settings, b compile.Bundle) []contact {
	t.Helper()
	raw, ok := b.Files[compile.ContactsFile]
	if !ok {
		// Only an install with no extension and no stream has none.
		if bytes.Contains(b.Files[compile.ExtensionsFile], []byte("<DirectoryEntry>")) {
			t.Errorf("no %s in the bundle, though the directory has entries", compile.ContactsFile)
		}
		return nil
	}
	if len(raw) == 0 {
		t.Errorf("%s is empty", compile.ContactsFile)
		return nil
	}
	file := string(raw)
	if !utf8.ValidString(file) || !strings.HasSuffix(file, "\r\n") {
		t.Errorf("%s is not UTF-8 ending in CRLF:\n%q", compile.ContactsFile, file)
		return nil
	}
	var lines []string // unfolded
	for _, l := range strings.Split(strings.TrimSuffix(file, "\r\n"), "\r\n") {
		if strings.ContainsAny(l, "\r\n") || len(l) > 75 || l == "" {
			t.Errorf("%s: line %q: a bare CR or LF, empty, or over 75 octets", compile.ContactsFile, l)
		}
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			if len(lines) == 0 {
				t.Fatalf("%s starts with a continuation line", compile.ContactsFile)
			}
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	host := s.PBXAddress
	if s.SIPPort != 5060 {
		host = fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort)
	}
	impp := regexp.MustCompile(`^sip:[0-9]+@` + regexp.QuoteMeta(host) + `$`)
	var out []contact
	for i := 0; i < len(lines); {
		if i+1 >= len(lines) || lines[i] != "BEGIN:VCARD" || lines[i+1] != "VERSION:4.0" {
			t.Fatalf("%s: a card must open BEGIN:VCARD, VERSION:4.0; at line %d: %q", compile.ContactsFile, i, lines[i:])
		}
		var c contact
		j := i + 2
		for ; j < len(lines) && lines[j] != "END:VCARD"; j++ {
			name, value, _ := strings.Cut(lines[j], ":")
			switch name {
			case "FN":
				if c.Name != "" || !vCardText.MatchString(value) {
					t.Errorf("%s: FN %q: a second FN, or not belcard text", compile.ContactsFile, value)
				}
				c.Name = strings.ReplaceAll(strings.ReplaceAll(value, `\\`, `\`), `\,`, `,`) // belcard_unescape_string
			case "IMPP":
				if c.URI != "" || !impp.MatchString(value) {
					t.Errorf("%s: IMPP %q: a second IMPP, or not a sip: URI on %s", compile.ContactsFile, value, host)
				}
				c.URI = value
			default:
				t.Errorf("%s: unexpected line %q", compile.ContactsFile, lines[j])
			}
		}
		if j == len(lines) || c.Name == "" || c.URI == "" {
			t.Fatalf("%s: a card without END:VCARD, an FN or an IMPP: %q", compile.ContactsFile, lines[i:j])
		}
		out = append(out, c)
		i = j + 1
	}
	return out
}

func uris(cs []contact) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.URI
	}
	return out
}

// TestContacts: the softphones' contacts are the Extensions directory, then
// the Streams one, each by number, every name whole and every card dialing
// its number on the PBX.
func TestContacts(t *testing.T) {
	reg := fakeRegistry()

	t.Run("house", func(t *testing.T) {
		in, s := fakeHouse()
		b := compileOK(t, reg, in, s)
		want := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Desk\r\nIMPP:sip:100@192.168.0.240\r\nEND:VCARD\r\n" +
			"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Chris (laptop)\r\nIMPP:sip:150@192.168.0.240\r\nEND:VCARD\r\n" +
			"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Chris\r\nIMPP:sip:301@192.168.0.240\r\nEND:VCARD\r\n"
		if got := string(b.Files[compile.ContactsFile]); got != want {
			t.Errorf("contacts:\n%q\nwant\n%q", got, want)
		}
		checkContacts(t, s, b)
	})

	t.Run("escaped and folded", func(t *testing.T) {
		in, s := fakeHouse()
		name := `Mom, Dad \ "the" kids; <Küche> ` + strings.Repeat("ü", 20)
		in.People[0].Spec.Name = name
		b := compileOK(t, reg, in, s)
		got := checkContacts(t, s, b)
		if len(got) != 3 || got[2] != (contact{name, "sip:301@192.168.0.240"}) {
			t.Errorf("contacts %+v, want the person's whole name back", got)
		}
		raw := string(b.Files[compile.ContactsFile])
		if !strings.Contains(raw, `FN:Mom\, Dad \\ "the" kids; <Küche> `) || !strings.Contains(raw, "\r\n ") {
			t.Errorf("the name is not escaped and folded:\n%s", raw)
		}
	})

	t.Run("streams", func(t *testing.T) {
		king := sourced("king5", 3, "705", "http://192.168.0.58:5004/auto/v5.1")
		king.Spec.Name = `KING <5> & "News" @@x@@`
		radio := sourced("groovesalad", 3, "750", "https://ice2.somafm.com/groovesalad-128-mp3")
		radio.Spec.Name = "Groove Salad, the SomaFM station of forty characters"
		list := []v1alpha1.Stream{radio, king,
			sourced("playlist", 4, "751", "https://somafm.com/groovesalad130.pls"), // refused
		}
		in, s := streamHouse(list...)
		b := compileOK(t, reg, in, s)
		got := checkContacts(t, s, b)
		want := []contact{
			{"Desk", "sip:100@192.168.0.240"}, {"Chris (laptop)", "sip:150@192.168.0.240"}, {"Chris", "sip:301@192.168.0.240"},
			{`KING <5> & "News" @x@`, "sip:705@192.168.0.240"},
			{"Groove Salad, the SomaFM station of forty characters", "sip:750@192.168.0.240"},
		}
		if !slices.Equal(got, want) {
			t.Errorf("contacts\n%+v\nwant\n%+v", got, want)
		}

		in, s = streamHouse(list...)
		s.StreamBridge = ""
		b = compileOK(t, reg, in, s)
		if got := uris(checkContacts(t, s, b)); len(got) != 3 {
			t.Errorf("without a bridge: %v, want the extensions alone", got)
		}
	})

	t.Run("SIP port", func(t *testing.T) {
		in, s := fakeHouse()
		s.SIPPort = 5070
		b := compileOK(t, reg, in, s)
		if got := uris(checkContacts(t, s, b)); got[0] != "sip:100@192.168.0.240:5070" {
			t.Errorf("contacts %v, want the port in each URI", got)
		}
	})

	t.Run("no 32-entry limit", func(t *testing.T) {
		in, s := fakeHouse()
		for i := range 34 {
			n := 110 + i
			in.Phones = append(in.Phones, phone(fmt.Sprintf("p%d", n), 5, modelPhone, fmt.Sprintf("D0C78915A%03d", n), fmt.Sprint(n), fmt.Sprintf("Phone %d", n)))
		}
		fakeSecrets(&in, s)
		b := compileOK(t, reg, in, s)
		if got := checkContacts(t, s, b); len(got) != 37 || got[36].URI != "sip:301@192.168.0.240" {
			t.Errorf("%d contacts, last %+v; want all 37, 301 last", len(got), got[len(got)-1])
		}
	})
}
