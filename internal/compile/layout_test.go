package compile_test

import (
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestTreeOf(t *testing.T) {
	for _, tc := range []struct {
		path, tree string
	}{
		{"asterisk/pjsip.conf", compile.TreeAsterisk},
		{"asterisk/pjsip_notify.conf", compile.TreeAsterisk},
		{"tftp/SEPD0C78915E8C8.cnf.xml", compile.TreeTFTP},
		{"tftp/Desktops/320x212x16/List.xml", compile.TreeTFTP},
		{"http/directory.xml", compile.TreeHTTP},
		{"provisioning/softphone-chris-laptop.xml", compile.TreeProvisioning},
	} {
		got, err := compile.TreeOf(tc.path)
		if err != nil || got != tc.tree {
			t.Errorf("TreeOf(%q) = %q, %v; want %q", tc.path, got, err, tc.tree)
		}
	}
	for _, bad := range []string{
		"",
		"pjsip.conf",                  // no tree
		"asterisk",                    // the tree itself, not a file in it
		"asterisk/",                   // ditto, unclean
		"/asterisk/pjsip.conf",        // absolute
		"etc/asterisk/pjsip.conf",     // not a tree
		"tftp/../asterisk/pjsip.conf", // escapes its tree
		"../tftp/x",
		"./tftp/x",
		"tftp//x",
		`tftp\x`,
		"tftp/ring tone.raw", // not a portable name
		"tftp/Ringlist~.xml", // ditto
		"tftp/a__b.xml",      // "__" would not survive ConfigMapKey
		"tftp/_a.xml",        // nor would a leading or trailing "_"
		"tftp/a_/b.xml",
		"tftp/sépia.xml",
	} {
		if tree, err := compile.TreeOf(bad); err == nil {
			t.Errorf("TreeOf(%q) = %q, nil; want an error", bad, tree)
		}
	}
}

// goodBundle is a small bundle that keeps every rule Validate checks: one
// rendered phone, one rendered softphone and one phone left out (Invalid).
func goodBundle() compile.Bundle {
	desk := drivers.DeviceID(drivers.KindPhone, "desk")
	laptop := drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
	kitchen := drivers.DeviceID(drivers.KindPhone, "kitchen")
	deskPW := compile.PasswordRef(desk).Placeholder()
	laptopPW := compile.PasswordRef(laptop).Placeholder()
	token := compile.ProvisioningTokenRef(laptop)
	return compile.Bundle{
		Files: map[string][]byte{
			"asterisk/asterisk.conf":                  []byte("[directories]\n"),
			"asterisk/pjsip.conf":                     []byte("password = " + deskPW + "\n"),
			"tftp/SEPD0C78915E8C8.cnf.xml":            []byte("<authPassword>" + deskPW + "</authPassword>"),
			"tftp/dialplan.xml":                       []byte("<DIALTEMPLATE/>"),
			"http/directory.xml":                      []byte("<CiscoIPPhoneDirectory/>"),
			"provisioning/softphone-chris-laptop.xml": []byte(laptopPW),
		},
		Version: strings.Repeat("a", 64),
		Devices: map[string]compile.BundleDevice{
			desk: {
				Kind: drivers.KindPhone, Name: "desk", Number: "100", MAC: "D0C78915E8C8",
				Version:    strings.Repeat("b", 64),
				ConfigFile: "tftp/SEPD0C78915E8C8.cnf.xml",
				Files:      []string{"tftp/SEPD0C78915E8C8.cnf.xml", "tftp/dialplan.xml"},
			},
			laptop: {
				Kind: drivers.KindSoftphone, Name: "chris-laptop", Number: "150",
				Version:           strings.Repeat("c", 64),
				ConfigFile:        "provisioning/softphone-chris-laptop.xml",
				Files:             []string{"provisioning/softphone-chris-laptop.xml"},
				ProvisioningToken: &token,
			},
			// Lost number 100 to desk: in the index for attribution, not rendered.
			kitchen: {Kind: drivers.KindPhone, Name: "kitchen", MAC: "D0C78915EBB5"},
		},
		AMI: compile.AMIAccount{
			Username: compile.AMIUsername,
			Password: compile.SecretRef{Name: "patchbay-ami", Key: "password"},
		},
	}
}

func TestBundleValidate(t *testing.T) {
	if err := goodBundle().Validate(); err != nil {
		t.Fatalf("a well-formed bundle: %v", err)
	}

	desk := drivers.DeviceID(drivers.KindPhone, "desk")
	laptop := drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
	kitchen := drivers.DeviceID(drivers.KindPhone, "kitchen")
	device := func(b *compile.Bundle, id string, mut func(*compile.BundleDevice)) {
		d := b.Devices[id]
		mut(&d)
		b.Devices[id] = d
	}
	pw := compile.PasswordRef(desk).Placeholder()
	for name, mutate := range map[string]func(*compile.Bundle){
		"no asterisk.conf":         func(b *compile.Bundle) { delete(b.Files, "asterisk/asterisk.conf") },
		"a path outside the trees": func(b *compile.Bundle) { b.Files["pjsip.conf"] = nil },
		"a placeholder in http/":   func(b *compile.Bundle) { b.Files["http/directory.xml"] = []byte(pw) },
		"an escape from tftp/":     func(b *compile.Bundle) { b.Files["tftp/../asterisk/manager.conf"] = nil },
		"no AMI account":           func(b *compile.Bundle) { b.AMI = compile.AMIAccount{} },
		"an ID that is not DeviceID": func(b *compile.Bundle) {
			b.Devices["desk"] = b.Devices[desk]
			delete(b.Devices, desk)
		},
		"an unknown kind": func(b *compile.Bundle) {
			device(b, desk, func(d *compile.BundleDevice) { d.Kind = "Person" })
		},
		"a missing file": func(b *compile.Bundle) { delete(b.Files, "tftp/dialplan.xml") },
		"a config file not among its files": func(b *compile.Bundle) {
			device(b, desk, func(d *compile.BundleDevice) { d.Files = []string{"tftp/dialplan.xml"} })
		},
		"a phone config file outside tftp/": func(b *compile.Bundle) {
			device(b, desk, func(d *compile.BundleDevice) { d.ConfigFile = "http/directory.xml" })
		},
		"unsorted files": func(b *compile.Bundle) {
			device(b, desk, func(d *compile.BundleDevice) { d.Files = []string{d.Files[1], d.Files[0]} })
		},
		"files without a version": func(b *compile.Bundle) {
			device(b, kitchen, func(d *compile.BundleDevice) { d.Files = []string{"tftp/dialplan.xml"} })
		},
		"a softphone without its token": func(b *compile.Bundle) {
			device(b, laptop, func(d *compile.BundleDevice) { d.ProvisioningToken = nil })
		},
		"a phone with a token": func(b *compile.Bundle) {
			device(b, desk, func(d *compile.BundleDevice) { d.ProvisioningToken = &compile.SecretRef{Name: "x", Key: "y"} })
		},
		"two devices with one MAC": func(b *compile.Bundle) {
			device(b, kitchen, func(d *compile.BundleDevice) { d.MAC = "D0C78915E8C8" })
		},
		"two devices with one number": func(b *compile.Bundle) {
			device(b, kitchen, func(d *compile.BundleDevice) { d.Number = "150" })
		},
	} {
		b := goodBundle()
		mutate(&b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestLiveLayout(t *testing.T) {
	if compile.AsteriskConfigDir != "/var/lib/patchbay/config/current/asterisk" {
		t.Fatalf("AsteriskConfigDir = %q", compile.AsteriskConfigDir)
	}
	if compile.SoundsDir != "/var/lib/patchbay/sounds" {
		t.Fatalf("SoundsDir = %q", compile.SoundsDir)
	}
	// Phones in the field hold the directory's URL (/directory.xml) from
	// their SEP files: it never moves.
	if compile.DirectoryFile != "http/directory.xml" {
		t.Fatalf("DirectoryFile = %q", compile.DirectoryFile)
	}
	for name, p := range map[string]string{
		"DirectoryFile": compile.DirectoryFile, "ServicesFile": compile.ServicesFile,
		"ExtensionsFile": compile.ExtensionsFile, "StreamsFile": compile.StreamsFile,
	} {
		if tree, err := compile.TreeOf(p); err != nil || tree != compile.TreeHTTP {
			t.Errorf("%s %q: tree %q, %v; want %s", name, p, tree, err, compile.TreeHTTP)
		}
	}
}
