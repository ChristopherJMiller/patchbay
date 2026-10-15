package compile_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// update rewrites the golden files: go test ./internal/compile -run Golden -update
var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden/*/want")

// goldenDir holds one directory per case: input.yaml, and want/ with every
// bundle file plus bundle.json (the index, version included).
const goldenDir = "testdata/golden"

// goldenInput is a case's input.yaml. Settings start as houseSettings and
// the file's settings override them field by field.
type goldenInput struct {
	Settings   compile.Settings     `json:"settings"`
	Phones     []v1alpha1.Phone     `json:"phones"`
	Softphones []v1alpha1.Softphone `json:"softphones"`
	People     []v1alpha1.Person    `json:"people"`
	Streams    []v1alpha1.Stream    `json:"streams"`
	Secrets    []struct {
		Name  string `json:"name"`
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"secrets"`
}

// loadGolden reads a case's input.
func loadGolden(t *testing.T, name string) (compile.Inputs, compile.Settings) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(goldenDir, name, "input.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	in := goldenInput{Settings: houseSettings()}
	if err := yaml.UnmarshalStrict(raw, &in); err != nil {
		t.Fatalf("%s/input.yaml: %v", name, err)
	}
	inputs := compile.Inputs{
		Phones: in.Phones, Softphones: in.Softphones, People: in.People, Streams: in.Streams,
		Secrets: map[compile.SecretRef][]byte{},
	}
	for _, s := range in.Secrets {
		inputs.Secrets[compile.SecretRef{Name: s.Name, Key: s.Key}] = []byte(s.Value)
	}
	return inputs, in.Settings
}

func goldenCases(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) < 8 {
		t.Fatalf("only %d golden cases in %s", len(names), goldenDir)
	}
	return names
}

func builtinRegistry(t *testing.T) *drivers.Registry {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestGolden compiles each case with the built-in drivers and compares every
// file and the index with want/, then runs the cross-checks on the bundle.
func TestGolden(t *testing.T) {
	reg := builtinRegistry(t)
	for _, name := range goldenCases(t) {
		t.Run(name, func(t *testing.T) {
			in, s := loadGolden(t, name)
			b, err := compile.Compile(reg, in, s)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			want := filepath.Join(goldenDir, name, "want")
			got := goldenFiles(t, b)
			if *update {
				if err := os.RemoveAll(want); err != nil {
					t.Fatal(err)
				}
				for p, content := range got {
					writeFile(t, filepath.Join(want, filepath.FromSlash(p)), content)
				}
			}
			compareTree(t, want, got)
			crossCheck(t, name, in, b)
		})
	}
}

// goldenFiles is the bundle as golden files: every file at its path, and
// bundle.json.
func goldenFiles(t *testing.T, b compile.Bundle) map[string][]byte {
	t.Helper()
	index, err := json.MarshalIndent(compile.Index{Version: b.Version, Devices: b.Devices, AMI: b.AMI}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out := maps.Clone(b.Files)
	out["bundle.json"] = append(index, '\n')
	return out
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// compareTree checks that dir holds exactly the files in got, byte for byte.
func compareTree(t *testing.T, dir string, got map[string][]byte) {
	t.Helper()
	have := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		have[filepath.ToSlash(rel)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", dir, err)
	}
	for _, p := range slices.Sorted(maps.Keys(got)) {
		w, ok := have[p]
		switch {
		case !ok:
			t.Errorf("%s: not in %s (run with -update)", p, dir)
		case !bytes.Equal(w, got[p]):
			t.Errorf("%s differs from %s (run with -update and review the diff):\n%s", p, dir, firstDiff(w, got[p]))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(have)) {
		if _, ok := got[p]; !ok {
			t.Errorf("%s/%s: no longer in the bundle (run with -update)", dir, p)
		}
	}
}

// firstDiff shows the first differing line.
func firstDiff(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + strconv.Itoa(i+1) + ":\n want " + strconv.Quote(wl) + "\n  got " + strconv.Quote(gl)
		}
	}
	return "(lengths differ)"
}

// sepFile is what the cross-checks read from a SEP<MAC>.cnf.xml.
type sepFile struct {
	Lines []struct {
		Name     string `xml:"name"`
		AuthName string `xml:"authName"`
	} `xml:"sipProfile>sipLines>line"`
	TimerRegisterExpires int `xml:"sipProfile>sipStack>timerRegisterExpires"`
}

// lpConfig is what the cross-checks read from a Linphone document.
type lpConfig struct {
	Sections []struct {
		Name    string `xml:"name,attr"`
		Entries []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"entry"`
	} `xml:"section"`
}

func (c lpConfig) get(section, entry string) string {
	for _, s := range c.Sections {
		if s.Name != section {
			continue
		}
		for _, e := range s.Entries {
			if e.Name == entry {
				return e.Value
			}
		}
	}
	return ""
}

// crossCheck asserts what must hold of every compiled bundle, beyond its
// golden bytes (docs/plans/release-a.md §4 T4).
func crossCheck(t *testing.T, name string, in compile.Inputs, b compile.Bundle) {
	t.Helper()
	if err := b.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// The ConfigMap encoding round-trips, and the house fits easily.
	data, binary, err := b.ConfigMapData()
	if err != nil {
		t.Fatalf("ConfigMapData: %v", err)
	}
	back, err := compile.BundleFromConfigMap(data, binary)
	if err != nil {
		t.Fatalf("BundleFromConfigMap: %v", err)
	}
	if !reflect.DeepEqual(back, b) {
		t.Errorf("the ConfigMap round trip changed the bundle")
	}
	if name == "house" {
		size := 0
		for k, v := range data {
			size += len(k) + len(v)
		}
		if size >= 64<<10 {
			t.Errorf("house encodes to %d bytes, want under 64 KiB", size)
		}
	}

	// No secret value anywhere; every placeholder is a rendered device's
	// password or the AMI secret; nothing in http/ has one.
	allowed := map[compile.SecretRef]bool{b.AMI.Password: true}
	for id, d := range b.Devices {
		if d.Version != "" {
			allowed[compile.PasswordRef(id)] = true
		}
	}
	for _, p := range slices.Sorted(maps.Keys(b.Files)) {
		content := b.Files[p]
		for ref, v := range in.Secrets {
			if bytes.Contains(content, v) {
				t.Errorf("%s holds the value of secret %s/%s", p, ref.Name, ref.Key)
			}
		}
		for _, m := range compile.PlaceholderPattern.FindAllStringSubmatch(string(content), -1) {
			if ref := (compile.SecretRef{Name: m[1], Key: m[2]}); !allowed[ref] {
				t.Errorf("%s references %s/%s, which is neither a rendered device's password nor the AMI secret", p, ref.Name, ref.Key)
			}
		}
		if strings.HasPrefix(p, compile.TreeHTTP+"/") && compile.PlaceholderPattern.Match(content) {
			t.Errorf("%s is served to anyone but holds a placeholder", p)
		}
	}

	// Every rendered device has its endpoint, auth and AOR, and its number
	// in the dialplan; nothing else has an endpoint.
	objects, err := pjsipObjects(b.Files["asterisk/pjsip.conf"])
	if err != nil {
		t.Fatalf("pjsip.conf: %v", err)
	}
	extensions, err := parseConf(b.Files["asterisk/extensions.conf"])
	if err != nil {
		t.Fatalf("extensions.conf: %v", err)
	}
	dialled := map[string]bool{}
	for _, s := range extensions {
		for _, e := range s.get("exten") {
			dialled[s.Name+"/"+strings.Split(e, ",")[0]] = true
		}
	}
	endpoints := 0
	for k := range objects {
		if k[0] == "endpoint" {
			endpoints++
		}
	}
	rendered := 0
	for _, id := range slices.Sorted(maps.Keys(b.Devices)) {
		d := b.Devices[id]
		if d.Version == "" {
			if _, ok := objects[[2]string{"endpoint", id}]; ok {
				t.Errorf("%s is not rendered but has an endpoint", id)
			}
			continue
		}
		rendered++
		ep, okE := objects[[2]string{"endpoint", id}]
		auth, okA := objects[[2]string{"auth", id}]
		aor, okR := objects[[2]string{"aor", d.Number}]
		if !okE || !okA || !okR {
			t.Errorf("%s: endpoint %t, auth %t, aor %s %t", id, okE, okA, d.Number, okR)
			continue
		}
		if v, _ := ep.one("aors"); v != d.Number {
			t.Errorf("%s: aors = %q, want %q", id, v, d.Number)
		}
		if v, _ := ep.one("auth"); v != id {
			t.Errorf("%s: auth = %q, want %q", id, v, id)
		}
		if !dialled[compile.DevicesContext+"/"+d.Number] {
			t.Errorf("%s: number %s is not in %s", id, d.Number, compile.DevicesContext)
		}
		authUser, _ := auth.one("username")
		maxExp, _ := aor.one("maximum_expiration")
		defExp, _ := aor.one("default_expiration")
		if maxExp != defExp {
			t.Errorf("AOR %s: default_expiration %s != maximum_expiration %s", d.Number, defExp, maxExp)
		}
		switch d.Kind {
		case drivers.KindPhone:
			var sep sepFile
			if err := xml.Unmarshal(b.Files[d.ConfigFile], &sep); err != nil {
				t.Fatalf("%s: %v", d.ConfigFile, err)
			}
			if len(sep.Lines) != 1 {
				t.Fatalf("%s: %d lines, want 1", d.ConfigFile, len(sep.Lines))
			}
			if sep.Lines[0].AuthName != authUser {
				t.Errorf("%s: authName %q != auth username %q", d.ConfigFile, sep.Lines[0].AuthName, authUser)
			}
			if sep.Lines[0].Name != aor.Name {
				t.Errorf("%s: line name %q != AOR %q", d.ConfigFile, sep.Lines[0].Name, aor.Name)
			}
			if got := strconv.Itoa(sep.TimerRegisterExpires); got != maxExp {
				t.Errorf("%s: timerRegisterExpires %s != AOR maximum_expiration %s", d.ConfigFile, got, maxExp)
			}
		case drivers.KindSoftphone:
			var lp lpConfig
			if err := xml.Unmarshal(b.Files[d.ConfigFile], &lp); err != nil {
				t.Fatalf("%s: %v", d.ConfigFile, err)
			}
			if v := lp.get("auth_info_0", "userid"); v != authUser {
				t.Errorf("%s: userid %q != auth username %q", d.ConfigFile, v, authUser)
			}
			if v := lp.get("auth_info_0", "username"); v != aor.Name {
				t.Errorf("%s: username %q != AOR %q", d.ConfigFile, v, aor.Name)
			}
			if v := lp.get("proxy_0", "reg_expires"); v != maxExp {
				t.Errorf("%s: reg_expires %s != AOR maximum_expiration %s", d.ConfigFile, v, maxExp)
			}
		}
	}
	if endpoints != rendered {
		t.Errorf("pjsip.conf has %d endpoints for %d rendered devices", endpoints, rendered)
	}
}

// TestVersionsBuiltin checks the same with the real drivers on the house:
// a softphone's document holds no label, so relabelling it moves no device
// version (no QR code to scan again), while a phone's label is on its
// screen.
func TestVersionsBuiltin(t *testing.T) {
	reg := builtinRegistry(t)
	in, s := loadGolden(t, "house")
	before := compileOK(t, reg, in, s)
	for _, tc := range []struct {
		name    string
		edit    func(in *compile.Inputs, s *compile.Settings)
		devices []string
	}{
		{"desk's label", func(in *compile.Inputs, _ *compile.Settings) { in.Phones[0].Spec.Label = "Study" }, []string{"phone-desk"}},
		{"the laptop's label", func(in *compile.Inputs, _ *compile.Settings) { in.Softphones[0].Spec.Label = "Laptop" }, nil},
		{"the time zone", func(_ *compile.Inputs, s *compile.Settings) { s.Timezone = "America/Chicago" }, []string{"phone-desk"}},
		{"the SIP port", func(_ *compile.Inputs, s *compile.Settings) { s.SIPPort = 5070 }, []string{"phone-desk", "softphone-chris-laptop"}},
		{"the token", func(in *compile.Inputs, _ *compile.Settings) {
			in.Secrets[compile.ProvisioningTokenRef(drivers.DeviceID(drivers.KindSoftphone, "chris-laptop"))] = []byte("not-a-secret-token-0009")
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, s := loadGolden(t, "house")
			tc.edit(&in, &s)
			after := compileOK(t, reg, in, s)
			if after.Version == before.Version {
				t.Error("the bundle version did not change")
			}
			if got := changed(before, after); !slices.Equal(got, tc.devices) {
				t.Errorf("device versions changed for %v, want %v", got, tc.devices)
			}
		})
	}
}
