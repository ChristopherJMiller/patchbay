package compile_test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Cisco's object limits (IP Phone Services SDK), as the tests check them.
const (
	menuMaxItems   = 100
	menuMaxName    = 64
	menuMaxURL     = 256
	ciscoMaxTitle  = 32
	directoryMaxNm = 32
)

// ciscoMenu is what the tests read back from a CiscoIPPhoneMenu.
type ciscoMenu struct {
	XMLName xml.Name `xml:"CiscoIPPhoneMenu"`
	Title   string   `xml:"Title"`
	Items   []struct {
		Name string `xml:"Name"`
		URL  string `xml:"URL"`
	} `xml:"MenuItem"`
}

// ciscoDirectory is what the tests read back from a CiscoIPPhoneDirectory.
type ciscoDirectory struct {
	XMLName xml.Name `xml:"CiscoIPPhoneDirectory"`
	Title   string   `xml:"Title"`
	Entries []struct {
		Name      string `xml:"Name"`
		Telephone string `xml:"Telephone"`
	} `xml:"DirectoryEntry"`
}

// numbers is a directory's Telephone column.
func (d ciscoDirectory) numbers() []string {
	out := make([]string, len(d.Entries))
	for i, e := range d.Entries {
		out[i] = e.Telephone
	}
	return out
}

// checkMenus checks what every bundle's http/ tree must be, and returns the
// directories the menu opens, by item name:
//   - DirectoryFile (the 79x5's directoryURL) and ServicesFile (its
//     servicesURL) are the same CiscoIPPhoneMenu, titled patchbay, UTF-8
//     with the declaration saying so, with 1 to 100 named items;
//   - every item's URL is absolute on the install's device HTTP, within
//     Cisco's 256 characters, and opens a CiscoIPPhoneDirectory that is in
//     the bundle's http/ tree, of at most 32 entries whose names are at
//     most 32 characters;
//   - Extensions is always there, Streams exactly when StreamsFile is, and
//     nothing in http/ is out of the menu's reach but the softphones'
//     ContactsFile, which holds the same entries (checkContacts).
func checkMenus(t *testing.T, s compile.Settings, b compile.Bundle) map[string]ciscoDirectory {
	t.Helper()
	base := fmt.Sprintf("http://%s:%d/", s.PBXAddress, s.DeviceHTTPPort)
	dirs := map[string]ciscoDirectory{}
	reached := map[string]bool{compile.DirectoryFile: true, compile.ServicesFile: true, compile.ContactsFile: true}
	checkContacts(t, s, b)
	for _, file := range []string{compile.DirectoryFile, compile.ServicesFile} {
		raw, ok := b.Files[file]
		if !ok {
			t.Errorf("no %s in the bundle", file)
			continue
		}
		var m ciscoMenu
		if err := xml.Unmarshal(raw, &m); err != nil {
			t.Errorf("%s is not a CiscoIPPhoneMenu: %v\n%s", file, err, raw)
			continue
		}
		if !utf8.Valid(raw) || !bytes.HasPrefix(raw, []byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n")) {
			t.Errorf("%s is not UTF-8 with an XML declaration saying so", file)
		}
		if m.Title != "patchbay" || len(m.Items) == 0 || len(m.Items) > menuMaxItems {
			t.Errorf("%s: title %q, %d items; want patchbay, 1 to %d", file, m.Title, len(m.Items), menuMaxItems)
		}
		for _, it := range m.Items {
			rel, abs := strings.CutPrefix(it.URL, base)
			if it.Name == "" || utf8.RuneCountInString(it.Name) > menuMaxName || !abs || len(it.URL) > menuMaxURL {
				t.Errorf("%s: item %q opens %q: want a name of 1 to %d characters and an absolute URL under %s of at most %d", file, it.Name, it.URL, menuMaxName, base, menuMaxURL)
				continue
			}
			target := compile.TreeHTTP + "/" + rel
			content, ok := b.Files[target]
			if !ok {
				t.Errorf("%s: item %q opens %s, which is not in the bundle", file, it.Name, target)
				continue
			}
			reached[target] = true
			var d ciscoDirectory
			if err := xml.Unmarshal(content, &d); err != nil {
				t.Errorf("%s is not a CiscoIPPhoneDirectory: %v\n%s", target, err, content)
				continue
			}
			if !bytes.HasPrefix(content, []byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n")) {
				t.Errorf("%s has no XML declaration", target)
			}
			if d.Title == "" || utf8.RuneCountInString(d.Title) > ciscoMaxTitle || len(d.Entries) > compile.DirectoryMaxEntries {
				t.Errorf("%s: title %q, %d entries; want a title of 1 to %d characters, at most %d entries", target, d.Title, len(d.Entries), ciscoMaxTitle, compile.DirectoryMaxEntries)
			}
			for _, e := range d.Entries {
				if utf8.RuneCountInString(e.Name) > directoryMaxNm || e.Telephone == "" {
					t.Errorf("%s: entry %+v: name over %d characters or no number", target, e, directoryMaxNm)
				}
			}
			dirs[it.Name] = d
		}
	}
	if !bytes.Equal(b.Files[compile.DirectoryFile], b.Files[compile.ServicesFile]) {
		t.Errorf("the Directories and Services menus differ")
	}
	if _, ok := dirs["Extensions"]; !ok {
		t.Errorf("the menu has no Extensions item")
	}
	_, streams := b.Files[compile.StreamsFile]
	if _, ok := dirs["Streams"]; ok != streams {
		t.Errorf("the menu's Streams item is there: %t; %s is: %t", ok, compile.StreamsFile, streams)
	}
	for _, p := range slices.Sorted(maps.Keys(b.Files)) {
		if strings.HasPrefix(p, compile.TreeHTTP+"/") && !reached[p] {
			t.Errorf("%s is in http/ but out of the menu's reach", p)
		}
	}
	return dirs
}

// TestDirectory: the Directories menu opens Extensions, every routed device
// and person sorted by number, names cut to 32 characters, at most 32
// entries with a comment for the rest; with no Stream there is no Streams
// item and no streams.xml.
func TestDirectory(t *testing.T) {
	reg := fakeRegistry()
	in, s := fakeHouse()
	in.People[0].Spec.Name = `A "name" <&> well over thirty-two characters`
	for i := range 34 {
		n := 110 + i
		in.Phones = append(in.Phones, phone(fmt.Sprintf("p%d", n), 5, modelPhone, fmt.Sprintf("D0C78915A%03d", n), fmt.Sprint(n), fmt.Sprintf("Phone %d", n)))
	}
	fakeSecrets(&in, s)
	b := compileOK(t, reg, in, s)
	dirs := checkMenus(t, s, b)
	if len(dirs) != 1 {
		t.Errorf("the menu opens %v, want Extensions alone (no Stream)", slices.Sorted(maps.Keys(dirs)))
	}
	if _, ok := b.Files[compile.StreamsFile]; ok {
		t.Errorf("%s is rendered with no Stream", compile.StreamsFile)
	}
	if want := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<CiscoIPPhoneMenu>\n  <Title>patchbay</Title>\n" +
		"  <MenuItem>\n    <Name>Extensions</Name>\n    <URL>http://192.168.0.240:8088/directory/extensions.xml</URL>\n  </MenuItem>\n" +
		"</CiscoIPPhoneMenu>\n"; string(b.Files[compile.DirectoryFile]) != want {
		t.Errorf("directory.xml:\n%s\nwant\n%s", b.Files[compile.DirectoryFile], want)
	}

	dir := dirs["Extensions"]
	raw := b.Files[compile.ExtensionsFile]
	if dir.Title != "Extensions" || len(dir.Entries) != compile.DirectoryMaxEntries {
		t.Fatalf("title %q, %d entries; want Extensions, %d", dir.Title, len(dir.Entries), compile.DirectoryMaxEntries)
	}
	if e := dir.Entries[0]; e.Name != "Desk" || e.Telephone != "100" {
		t.Errorf("first entry %+v", e)
	}
	if e := dir.Entries[1]; e.Telephone != "110" {
		t.Errorf("second entry %+v, want 110 (sorted by number)", e)
	}
	if !strings.Contains(string(raw), "<!-- 5 more entries dropped: a CiscoIPPhoneDirectory holds at most 32 -->") {
		t.Errorf("no comment for the dropped entries:\n%s", raw)
	}
	// 37 entries (100, 110-143, 150, 301): the last five are dropped.
	if strings.Contains(string(raw), "<Telephone>301</Telephone>") {
		t.Error("301 should have been dropped")
	}

	in2, s2 := fakeHouse()
	in2.People[0].Spec.Name = `A "name" <&> well over thirty-two characters`
	b2 := compileOK(t, reg, in2, s2)
	dir2 := checkMenus(t, s2, b2)["Extensions"]
	if got := dir2.numbers(); !slices.Equal(got, []string{"100", "150", "301"}) {
		t.Errorf("numbers %v, want 100 150 301", got)
	}
	if got := dir2.Entries[2].Name; got != `A "name" <&> well over thirty-tw` {
		t.Errorf("person entry name %q, want it cut to 32 characters", got)
	}
	if !strings.Contains(string(b2.Files[compile.ExtensionsFile]), "<Name>A &#34;name&#34; &lt;&amp;&gt; well over thirty-tw</Name>") {
		t.Errorf("the person's name is not escaped:\n%s", b2.Files[compile.ExtensionsFile])
	}
}

// TestStreamDirectory: the Streams item, and streams.xml, list every Stream
// whose number dials the stream bridge, by number, names neutralized
// (drivers.UserText), escaped and cut to 32 characters, at most 32; a
// Stream with a refused source, one that lost its number, and every Stream
// of an install without a bridge (each plays "not available") are left
// out, and with none left there is no Streams item.
func TestStreamDirectory(t *testing.T) {
	reg := fakeRegistry()
	king := sourced("king5", 3, "705", "http://192.168.0.58:5004/auto/v5.1")
	king.Spec.Name = `KING <5> & "News" @@x@@`
	radio := sourced("groovesalad", 3, "750", "https://ice2.somafm.com/groovesalad-128-mp3")
	radio.Spec.Name = "Groove Salad, the SomaFM station of forty characters"
	list := []v1alpha1.Stream{
		radio,
		king,
		sourced("kiro7", 3, "707", "http://192.168.0.58:5004/auto/v7.1"),
		sourced("late", 4, "705", "http://192.168.0.58:5004/auto/v5.1"),        // 705 is king5's
		sourced("playlist", 4, "751", "https://somafm.com/groovesalad130.pls"), // refused
	}

	in, s := streamHouse(list...)
	b := compileOK(t, reg, in, s)
	dirs := checkMenus(t, s, b)
	dir, ok := dirs["Streams"]
	if !ok {
		t.Fatalf("no Streams item:\n%s", b.Files[compile.DirectoryFile])
	}
	if dir.Title != "Streams" || !slices.Equal(dir.numbers(), []string{"705", "707", "750"}) {
		t.Errorf("title %q, numbers %v; want Streams, 705 707 750", dir.Title, dir.numbers())
	}
	if got := dir.Entries[0].Name; got != `KING <5> & "News" @x@` {
		t.Errorf("king5's entry %q, want its runs of @ collapsed", got)
	}
	if got := dir.Entries[1].Name; got != "kiro7" {
		t.Errorf("kiro7's entry %q", got)
	}
	if got := dir.Entries[2].Name; got != "Groove Salad, the SomaFM station" {
		t.Errorf("the radio's entry %q, want it cut to 32 characters", got)
	}
	raw := string(b.Files[compile.StreamsFile])
	if !strings.Contains(raw, "<Name>KING &lt;5&gt; &amp; &#34;News&#34; @x@</Name>") {
		t.Errorf("king5's name is not escaped:\n%s", raw)
	}
	if want := "  <MenuItem>\n    <Name>Streams</Name>\n    <URL>http://192.168.0.240:8088/directory/streams.xml</URL>\n  </MenuItem>\n"; !strings.Contains(string(b.Files[compile.DirectoryFile]), want) {
		t.Errorf("directory.xml lacks\n%s\nin\n%s", want, b.Files[compile.DirectoryFile])
	}
	if got := checkMenus(t, s, b)["Extensions"].numbers(); !slices.Equal(got, []string{"100", "150", "301"}) {
		t.Errorf("Extensions %v: the streams must not be listed there", got)
	}

	t.Run("no bridge", func(t *testing.T) {
		in, s := streamHouse(list...)
		s.StreamBridge = ""
		b := compileOK(t, reg, in, s)
		if dirs := checkMenus(t, s, b); len(dirs) != 1 {
			t.Errorf("the menu opens %v, want Extensions alone", slices.Sorted(maps.Keys(dirs)))
		}
		if _, ok := b.Files[compile.StreamsFile]; ok {
			t.Errorf("%s is rendered, but every stream plays \"not available\"", compile.StreamsFile)
		}
	})

	t.Run("only refused", func(t *testing.T) {
		in, s := streamHouse(sourced("playlist", 4, "751", "https://somafm.com/groovesalad130.pls"))
		b := compileOK(t, reg, in, s)
		if dirs := checkMenus(t, s, b); len(dirs) != 1 {
			t.Errorf("the menu opens %v, want Extensions alone", slices.Sorted(maps.Keys(dirs)))
		}
	})

	t.Run("over 32", func(t *testing.T) {
		var many []v1alpha1.Stream
		for i := range 34 {
			many = append(many, sourced(fmt.Sprintf("s%d", 733-i), 3, fmt.Sprint(733-i), "http://192.168.0.58:5004/auto/v5.1"))
		}
		in, s := streamHouse(many...)
		b := compileOK(t, reg, in, s)
		dir := checkMenus(t, s, b)["Streams"]
		if len(dir.Entries) != compile.DirectoryMaxEntries || dir.Entries[0].Telephone != "700" || dir.Entries[31].Telephone != "731" {
			t.Errorf("%d entries, %v; want 700-731", len(dir.Entries), dir.numbers())
		}
		if raw := string(b.Files[compile.StreamsFile]); !strings.Contains(raw, "<!-- 2 more entries dropped: a CiscoIPPhoneDirectory holds at most 32 -->") {
			t.Errorf("no comment for the dropped entries:\n%s", raw)
		}
	})
}
