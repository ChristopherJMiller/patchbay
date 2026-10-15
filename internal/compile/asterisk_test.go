package compile_test

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestCallerIDName(t *testing.T) {
	for _, tc := range []struct {
		label string
		max   int
		want  string
	}{
		{"Desk", 30, "Desk"},
		{`R&D; "Lab" <1>`, 30, `R&D; 'Lab' <1>`},
		{`C:\Users`, 30, "C:/Users"},
		{"line\nbreak\ttab", 30, "line break tab"},
		{"Küche über alles", 5, "Küche"},
		{"日本語のラベル", 3, "日本語"},
		{"no limit", 0, "no limit"},
	} {
		if got := compile.CallerIDName(tc.label, tc.max); got != tc.want {
			t.Errorf("CallerIDName(%q, %d) = %q, want %q", tc.label, tc.max, got, tc.want)
		}
	}
}

// TestAsteriskEscaping renders hostile labels into pjsip.conf and reads it
// back with a tokenizer that follows Asterisk's config parser: each label
// must come back as the caller ID name, whole, in its own option.
func TestAsteriskEscaping(t *testing.T) {
	reg := fakeRegistry()
	in, s := fakeHouse()
	labels := []struct{ label, callerid string }{ // the callerid Asterisk must read
		{`R&D; "Lab" <1> Küche`, `"R&D; 'Lab' <1> Küche" <100>`},
		{`back\slash;--block--;`, `"back/slash;--block--;" <101>`},
		{"two\nlines\n[evil]\ntype=aor", `"two lines [evil] type=aor" <102>`},
		{"a label well over thirty characters long", `"a label well over thirty chara" <103>`},
		{"日本語 ; ラベル", `"日本語 ; ラベル" <104>`},
	}
	want := map[string]string{} // endpoint -> callerid
	for i, l := range labels {
		n := 100 + i
		name := fmt.Sprintf("p%d", n)
		in.Phones = append(in.Phones, phone(name, 5, modelPhone, fmt.Sprintf("D0C78915A%03d", n), fmt.Sprint(n), l.label))
		want["phone-"+name] = l.callerid
	}
	in.Phones = in.Phones[1:] // desk held 100
	in.People = nil
	fakeSecrets(&in, s)
	b := compileOK(t, reg, in, s)
	objects, err := pjsipObjects(b.Files["asterisk/pjsip.conf"])
	if err != nil {
		t.Fatalf("pjsip.conf does not parse: %v\n%s", err, b.Files["asterisk/pjsip.conf"])
	}
	for id, callerid := range want {
		ep, ok := objects[[2]string{"endpoint", id}]
		if !ok {
			t.Errorf("no endpoint %s", id)
			continue
		}
		if got, ok := ep.one("callerid"); !ok || got != callerid {
			t.Errorf("%s: callerid %q, want %q", id, got, callerid)
		}
		if len(ep.Options) != 21+2 { // every endpoint option, contact_permit once per network
			t.Errorf("%s: %d options, want 23: a label leaked into another option:\n%v", id, len(ep.Options), ep.Options)
		}
	}
	for k := range objects {
		if strings.Contains(k[1], "evil") {
			t.Errorf("a label made a section: %v", k)
		}
	}
	if strings.Contains(string(b.Files["asterisk/extensions.conf"]), "Lab") {
		t.Error("a label reached extensions.conf")
	}
}

// ciscoDirectory is what TestDirectory reads back.
type ciscoDirectory struct {
	XMLName xml.Name `xml:"CiscoIPPhoneDirectory"`
	Title   string   `xml:"Title"`
	Entries []struct {
		Name      string `xml:"Name"`
		Telephone string `xml:"Telephone"`
	} `xml:"DirectoryEntry"`
}

// TestDirectory: sorted by number, names cut to 32 characters, at most 32
// entries with a comment for the rest, and well-formed XML.
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
	var dir ciscoDirectory
	raw := b.Files[compile.DirectoryFile]
	if err := xml.Unmarshal(raw, &dir); err != nil {
		t.Fatalf("directory.xml: %v\n%s", err, raw)
	}
	if dir.Title != "patchbay" || len(dir.Entries) != compile.DirectoryMaxEntries {
		t.Fatalf("title %q, %d entries; want patchbay, %d", dir.Title, len(dir.Entries), compile.DirectoryMaxEntries)
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
	raw = compileOK(t, reg, in2, s2).Files[compile.DirectoryFile]
	var dir2 ciscoDirectory
	if err := xml.Unmarshal(raw, &dir2); err != nil {
		t.Fatal(err)
	}
	if got := dir2.Entries[2].Name; got != `A "name" <&> well over thirty-tw` {
		t.Errorf("person entry name %q, want it cut to 32 characters", got)
	}
}

// TestDialplanTargets: a person rings its routed devices in ring order, each
// once, skipping missing and unrouted ones; a held stream number plays "not
// available".
func TestDialplanTargets(t *testing.T) {
	reg := fakeRegistry()
	in, s := fakeHouse()
	in.Phones = append(in.Phones, phone("late", 5, modelPhone, "D0C78915A001", "100", "Late"))
	in.People = append(in.People, person("all", 5, "302", "All",
		ringSoftphone("laptop"), ringPhone("attic"), ringPhone("late"), ringPhone("desk"), ringSoftphone("laptop")))
	in.Streams = append(in.Streams, stream("king5", 5, "705"), stream("late", 6, "705"))
	fakeSecrets(&in, s)
	b := compileOK(t, reg, in, s)
	ext := string(b.Files["asterisk/extensions.conf"])
	for _, want := range []string{
		"exten => 302,1,Dial(PJSIP/softphone-laptop&PJSIP/phone-desk,30)\n",
		"; Stream king5: no stream bridge in this release\nexten => 705,1,Goto(patchbay-unavailable,s,1)\n",
	} {
		if !strings.Contains(ext, want) {
			t.Errorf("extensions.conf lacks %q:\n%s", want, ext)
		}
	}
	if strings.Count(ext, "exten => 705,") != 1 || strings.Contains(ext, "Stream late") {
		t.Errorf("the conflicted stream is routed:\n%s", ext)
	}
	if _, err := parseConf([]byte(ext)); err != nil {
		t.Errorf("extensions.conf does not parse: %v", err)
	}
}
