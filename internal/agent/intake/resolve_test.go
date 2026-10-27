package intake_test

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

var (
	desk   = drivers.DeviceID(drivers.KindPhone, "desk")
	laptop = drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
)

func TestResolvePushPassword(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	r, err := intake.Resolve(b, in.SecretData(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if r.PushPassword != string(in.Secrets[intaketest.PushRef]) || r.PushPassword == "" {
		t.Errorf("PushPassword = %q", r.PushPassword)
	}
	if r.Bundle.Push.Username != compile.PushUsername {
		t.Errorf("Push.Username = %q", r.Bundle.Push.Username)
	}
}

func TestResolve(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	data := in.SecretData(t, b)
	data["some-other_key"] = []byte("ignored")
	original := maps.Clone(b.Files)

	r, err := intake.Resolve(b, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(r.Files["asterisk/pjsip.conf"], in.Secrets[compile.PasswordRef(desk)]) ||
		!bytes.Contains(r.Files["asterisk/pjsip.conf"], in.Secrets[compile.PasswordRef(laptop)]) {
		t.Error("pjsip.conf lacks a device password")
	}
	if !bytes.Contains(r.Files["asterisk/manager.conf"], in.Secrets[intaketest.AMIRef]) {
		t.Error("manager.conf lacks the AMI password")
	}
	if !bytes.Contains(r.Files["tftp/SEPD0C78915E8C8.cnf.xml"], in.Secrets[compile.PasswordRef(desk)]) {
		t.Error("the SEP file lacks the desk's password")
	}
	for p, content := range r.Files {
		if bytes.Contains(content, []byte("@@secret:")) {
			t.Errorf("%s still holds a placeholder", p)
		}
	}
	if len(r.Files) != len(b.Files) {
		t.Errorf("%d files resolved, the bundle has %d", len(r.Files), len(b.Files))
	}
	// Files without placeholders are byte for byte the bundle's.
	if !bytes.Equal(r.Files["asterisk/asterisk.conf"], b.Files["asterisk/asterisk.conf"]) {
		t.Error("asterisk.conf changed")
	}
	if r.AMIPassword != string(in.Secrets[intaketest.AMIRef]) {
		t.Errorf("AMIPassword = %q", r.AMIPassword)
	}
	wantTokens := map[string]string{laptop: string(in.Secrets[compile.ProvisioningTokenRef(laptop)])}
	if !maps.Equal(r.Tokens, wantTokens) {
		t.Errorf("Tokens = %v, want %v", r.Tokens, wantTokens)
	}
	// The bundle is as decoded: placeholders intact, nothing changed.
	if !maps.EqualFunc(b.Files, original, bytes.Equal) || r.Bundle.Version != b.Version {
		t.Error("Resolve changed the bundle")
	}
	if !bytes.Contains(r.Bundle.Files["asterisk/manager.conf"], []byte(intaketest.AMIRef.Placeholder())) {
		t.Error("Resolved.Bundle lost its placeholders")
	}
}

func TestResolveRefusesTheWholeBundle(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	for name, tc := range map[string]struct {
		drop  []compile.SecretRef
		empty []compile.SecretRef
		want  string
	}{
		"the AMI password": {
			drop: []compile.SecretRef{intaketest.AMIRef},
			want: "unresolved placeholder patchbay-ami/password",
		},
		// SecretRefs order: the desk's password before the laptop's token.
		"the first of two": {
			drop: []compile.SecretRef{compile.ProvisioningTokenRef(laptop), compile.PasswordRef(desk)},
			want: "unresolved placeholder patchbay-phone-desk/sip-password",
		},
		"a token": {
			drop: []compile.SecretRef{compile.ProvisioningTokenRef(laptop)},
			want: "unresolved placeholder patchbay-softphone-chris-laptop/provisioning-token",
		},
		"an empty value": {
			empty: []compile.SecretRef{compile.PasswordRef(laptop)},
			want:  "unresolved placeholder patchbay-softphone-chris-laptop/sip-password",
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := in.SecretData(t, b)
			for _, r := range tc.drop {
				key, _ := r.ProjectedKey()
				delete(data, key)
			}
			for _, r := range tc.empty {
				key, _ := r.ProjectedKey()
				data[key] = []byte{}
			}
			r, err := intake.Resolve(b, data)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Resolve = %v, want exactly %q", err, tc.want)
			}
			if r.Files != nil || r.AMIPassword != "" || r.Tokens != nil {
				t.Errorf("a refused bundle came with content: %+v", r)
			}
			for _, v := range in.Values() {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the error holds a secret value: %v", err)
				}
			}
		})
	}
}

// TestResolveIsOnePass: a value that looks like a placeholder is written
// as given, never resolved in turn.
func TestResolveIsOnePass(t *testing.T) {
	in := intaketest.House()
	in.Secrets[compile.PasswordRef(desk)] = []byte(intaketest.AMIRef.Placeholder())
	b := in.Compile(t)
	r, err := intake.Resolve(b, in.SecretData(t, b))
	if err != nil {
		t.Fatal(err)
	}
	sep := r.Files["tftp/SEPD0C78915E8C8.cnf.xml"]
	if !bytes.Contains(sep, []byte(intaketest.AMIRef.Placeholder())) || bytes.Contains(sep, in.Secrets[intaketest.AMIRef]) {
		t.Error("a value holding a placeholder was resolved again")
	}
}

// TestResolveLeavesOtherBytesAlone: a file that is not UTF-8 is untouched,
// and placeholders in it are still resolved.
func TestResolveLeavesOtherBytesAlone(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	raw := []byte{0xff, 0xfe, 0x00, '@', '@', 0x80, 0xc3}
	b.Files["tftp/ring.raw"] = raw
	b.Files["tftp/mixed.bin"] = append([]byte{0xff, 0x00}, []byte(intaketest.AMIRef.Placeholder()+"\xfe")...)
	r, err := intake.Resolve(b, in.SecretData(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Files["tftp/ring.raw"], raw) {
		t.Errorf("ring.raw = %x, want %x", r.Files["tftp/ring.raw"], raw)
	}
	want := append([]byte{0xff, 0x00}, append(in.Secrets[intaketest.AMIRef], 0xfe)...)
	if !bytes.Equal(r.Files["tftp/mixed.bin"], want) {
		t.Errorf("mixed.bin = %q, want %q", r.Files["tftp/mixed.bin"], want)
	}
	raw[0] = 0 // the resolved file is a copy
	if r.Files["tftp/ring.raw"][0] != 0xff {
		t.Error("Resolved.Files shares memory with the bundle")
	}
}

func TestResolveRefusesAMalformedPlaceholder(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	data := in.SecretData(t, b)
	b.Files["tftp/bad.xml"] = []byte("@@secret:Not_A_Name/x@@")
	if _, err := intake.Resolve(b, data); err == nil {
		t.Error("a malformed placeholder was accepted")
	}
}
