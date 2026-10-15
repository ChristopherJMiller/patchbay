package compile_test

import (
	"bytes"
	"maps"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// TestBoot: a bundle's Boot is the part of its Asterisk config that only a
// restart loads (asterisk.conf, modules.conf, the PJSIP transports), and
// BootFor gives what this release compiles for a PBX address and SIP port.
func TestBoot(t *testing.T) {
	in, s := fakeHouse()
	b := compileOK(t, fakeRegistry(), in, s)
	got, err := compile.BootOf(b.Files)
	if err != nil {
		t.Fatal(err)
	}
	if want := compile.BootFor("192.168.0.240", 5060); got != want {
		t.Errorf("BootOf(house) = %+v, BootFor(the house's address and port) = %+v", got, want)
	}
	if got.SIPBind != "192.168.0.240:5060" || len(got.Digest) != 64 {
		t.Errorf("Boot = %+v", got)
	}

	// Anything reloadable changes nothing: endpoints, the ACL, RTP ports.
	in.Phones = in.Phones[:0]
	s.AllowedNetworks = []string{"10.0.0.0/8"}
	s.RTPPorts.Last = 10199
	if other, err := compile.BootOf(compileOK(t, fakeRegistry(), in, s).Files); err != nil || other != got {
		t.Errorf("reloadable changes moved the Boot: %+v, %v", other, err)
	}

	// The SIP port or the address does.
	for _, other := range []compile.Boot{compile.BootFor("192.168.0.240", 5070), compile.BootFor("192.168.0.241", 5060)} {
		if other.Digest == got.Digest {
			t.Errorf("%+v has the house's digest", other)
		}
	}
	s = houseSettings()
	s.SIPPort = 5070
	if b2, err := compile.BootOf(compileOK(t, fakeRegistry(), in, s).Files); err != nil || b2 != compile.BootFor("192.168.0.240", 5070) {
		t.Errorf("BootOf(port 5070) = %+v, %v; want BootFor's", b2, err)
	}

	// So does any change to asterisk.conf, modules.conf or a transport.
	for _, edit := range []struct{ file, from, to string }{
		{"asterisk/modules.conf", "load => ", "load => res_extra.so\nload => "},
		{"asterisk/asterisk.conf", "hideconnect = yes", "hideconnect = no"},
		{"asterisk/pjsip.conf", "protocol = tcp", "protocol = tcp\nlocal_net = 10.0.0.0/8"},
	} {
		files := maps.Clone(b.Files)
		files[edit.file] = bytes.Replace(files[edit.file], []byte(edit.from), []byte(edit.to), 1)
		if bytes.Equal(files[edit.file], b.Files[edit.file]) {
			t.Fatalf("%s holds no %q", edit.file, edit.from)
		}
		if other, err := compile.BootOf(files); err != nil || other.Digest == got.Digest {
			t.Errorf("editing %s: Boot %+v, %v; want a new digest", edit.file, other, err)
		}
	}

	// A bundle without one of the files, or without transports, has none.
	for _, drop := range []string{"asterisk/asterisk.conf", "asterisk/modules.conf", "asterisk/pjsip.conf"} {
		files := maps.Clone(b.Files)
		delete(files, drop)
		if _, err := compile.BootOf(files); err == nil {
			t.Errorf("BootOf without %s succeeded", drop)
		}
	}
	files := maps.Clone(b.Files)
	files["asterisk/pjsip.conf"] = []byte("[global]\ntype = global\n")
	if _, err := compile.BootOf(files); err == nil {
		t.Error("BootOf a pjsip.conf without transports succeeded")
	}
}

// TestBootIgnoresComments: what Asterisk does not read does not count.
func TestBootIgnoresComments(t *testing.T) {
	in, s := fakeHouse()
	b := compileOK(t, fakeRegistry(), in, s)
	want, err := compile.BootOf(b.Files)
	if err != nil {
		t.Fatal(err)
	}
	files := maps.Clone(b.Files)
	for _, f := range []string{"asterisk/asterisk.conf", "asterisk/modules.conf", "asterisk/pjsip.conf"} {
		files[f] = append([]byte("; a new comment\n\n"), bytes.ReplaceAll(files[f], []byte("\n"), []byte("\n  \n"))...)
	}
	if got, err := compile.BootOf(files); err != nil || got != want {
		t.Errorf("comments and blank lines moved the Boot: %+v, %v", got, err)
	}
}
