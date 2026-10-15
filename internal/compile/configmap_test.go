package compile_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestConfigMapKey(t *testing.T) {
	for _, tc := range []struct{ path, key string }{
		{"asterisk/pjsip.conf", "asterisk__pjsip.conf"},
		{"asterisk/pjsip_notify.conf", "asterisk__pjsip_notify.conf"},
		{"tftp/SEPD0C78915E8C8.cnf.xml", "tftp__SEPD0C78915E8C8.cnf.xml"},
		{"tftp/Desktops/320x212x16/List.xml", "tftp__Desktops__320x212x16__List.xml"},
		{"provisioning/softphone-chris-laptop.xml", "provisioning__softphone-chris-laptop.xml"},
	} {
		key, err := compile.ConfigMapKey(tc.path)
		if err != nil || key != tc.key {
			t.Errorf("ConfigMapKey(%q) = %q, %v; want %q", tc.path, key, err, tc.key)
		}
		back, err := compile.PathFromConfigMapKey(tc.key)
		if err != nil || back != tc.path {
			t.Errorf("PathFromConfigMapKey(%q) = %q, %v; want %q", tc.key, back, err, tc.path)
		}
	}
	if _, err := compile.ConfigMapKey("tftp/" + strings.Repeat("a", 260)); err == nil {
		t.Error("a key over 253 characters: want an error")
	}
	for _, key := range []string{compile.IndexKey, "pjsip.conf", "etc__pjsip.conf", "tftp___x", "tftp"} {
		if p, err := compile.PathFromConfigMapKey(key); err == nil {
			t.Errorf("PathFromConfigMapKey(%q) = %q, nil; want an error", key, p)
		}
	}
}

func TestConfigMapRoundTrip(t *testing.T) {
	in := goodBundle()
	in.Files["tftp/ring.raw"] = []byte{0xff, 0x7f, 0x00, 0x80} // not UTF-8
	in.Files["asterisk/empty.conf"] = []byte{}
	data, binaryData, err := in.ConfigMapData()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binaryData["tftp__ring.raw"]; !ok || data["tftp__ring.raw"] != "" {
		t.Errorf("a non-UTF-8 file should be in binaryData only")
	}
	if !strings.Contains(data[compile.IndexKey], `"version":"`+in.Version+`"`) {
		t.Errorf("index %s does not carry the version", data[compile.IndexKey])
	}
	out, err := compile.BundleFromConfigMap(data, binaryData)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the bundle:\n in %+v\nout %+v", in, out)
	}
}

func TestConfigMapRefuses(t *testing.T) {
	big := goodBundle()
	big.Files["tftp/big.xml"] = []byte(strings.Repeat("x", compile.MaxConfigMapBytes))
	if _, _, err := big.ConfigMapData(); !errors.Is(err, compile.ErrBundleTooLarge) {
		t.Errorf("an oversized bundle: err = %v, want ErrBundleTooLarge", err)
	}
	invalid := goodBundle()
	delete(invalid.Files, "asterisk/asterisk.conf")
	if _, _, err := invalid.ConfigMapData(); err == nil {
		t.Error("an invalid bundle: want an error")
	}
	unversioned := goodBundle()
	unversioned.Version = ""
	if _, _, err := unversioned.ConfigMapData(); err == nil {
		t.Error("a bundle without a version: want an error")
	}

	data, _, err := goodBundle().ConfigMapData()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(d map[string]string, bin map[string][]byte){
		"no index":         func(d map[string]string, _ map[string][]byte) { delete(d, compile.IndexKey) },
		"a corrupt index":  func(d map[string]string, _ map[string][]byte) { d[compile.IndexKey] = "{" },
		"a key not a path": func(d map[string]string, _ map[string][]byte) { d["notes.txt"] = "hi" },
		"a key in both data and binaryData": func(_ map[string]string, bin map[string][]byte) {
			bin["asterisk__asterisk.conf"] = []byte{0xff}
		},
		"a file the index needs is missing": func(d map[string]string, _ map[string][]byte) { delete(d, "tftp__dialplan.xml") },
	} {
		d := map[string]string{}
		for k, v := range data {
			d[k] = v
		}
		bin := map[string][]byte{}
		mutate(d, bin)
		if _, err := compile.BundleFromConfigMap(d, bin); err == nil {
			t.Errorf("%s: BundleFromConfigMap = nil error, want one", name)
		}
	}
}
