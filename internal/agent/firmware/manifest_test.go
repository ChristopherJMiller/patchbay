package firmware_test

import (
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// TestResolve: a device's request resolves to one load. A name that is not
// Shared belongs to exactly one load; a Shared name only to the one default
// load carrying it, never to another load and never when none or several
// default loads carry it.
func TestResolve(t *testing.T) {
	m := testManifest(t)
	for _, tc := range []struct {
		name     string
		wantLoad string // "" = not served
	}{
		{"a1.loads", "A1"},
		{"appsA2.sbn", "A2"}, // a non-default load's own file is served by name
		{"b1.sbn", "B1"},
		{"a3.loads", "A3"},             // pending: resolved, and Open refuses it
		{"term.default.loads", "A1"},   // Shared: from the default load, not A2
		{"pend.default.loads", "B1"},   // Shared, pending in its default load
		{"both.default.loads", ""},     // Shared by two default loads
		{"orphan.default.loads", ""},   // Shared by no default load
		{"gone", ""},                   // a default load in no manifest is no file
		{"unknown.sbn", ""},            // in no manifest
		{"apps45.9-4-2SR4-3.sbn", ""},  // another registry's file
		{"A2", ""},                     // a load is not a file
		{"", ""},                       // nothing
		{"appsa1.sbn", ""},             // names are exact: no case folding
		{"TERM.default.loads", ""},     // names are exact: no case folding
		{"appsA1.sbn ", ""},            // names are exact: no trimming
		{"term.default.loads\x00", ""}, // names are exact: no C strings
		{"A1/a1.loads", ""},            // a path, not a name
		{"A2/term.default.loads", ""},  // a path: Shared stays the default's
		{"/a1.loads", ""},              // a leading slash is the server's to strip
		{"./a1.loads", ""},             // never a path
		{"../A1/a1.loads", ""},         // never a path
		{".incoming/x", ""},            // never the upload area
		{"%2e%2e/a1.loads", ""},        // no unescaping here
		{"*.loads", ""},                // no globbing
	} {
		load, f, ok := m.Resolve(tc.name)
		switch {
		case tc.wantLoad == "" && ok:
			t.Errorf("Resolve(%q) = %s/%s, want not served", tc.name, load.Name, f.Name)
		case tc.wantLoad != "" && !ok:
			t.Errorf("Resolve(%q) = not served, want load %s", tc.name, tc.wantLoad)
		case ok && (load.Name != tc.wantLoad || f.Name != tc.name):
			t.Errorf("Resolve(%q) = %s/%s, want %s/%s", tc.name, load.Name, f.Name, tc.wantLoad, tc.name)
		}
	}

	// A Shared name resolves to the default load's own entry (its digest),
	// not another load's copy.
	_, f, _ := m.Resolve("term.default.loads")
	if want := digest(content("A1", "term.default.loads")); f.SHA256 != want || !f.Shared {
		t.Errorf("Resolve(term.default.loads) = %+v, want A1's entry (%s)", f, want)
	}
}

// TestLookup: an upload names its load, so every manifest file is a
// target, Shared or not, default load or not, pending or not.
func TestLookup(t *testing.T) {
	m := testManifest(t)
	for _, tc := range []struct {
		load, name string
		ok         bool
	}{
		{"A1", "a1.loads", true},
		{"A2", "term.default.loads", true},
		{"A2", "orphan.default.loads", true},
		{"A3", "a3.loads", true},
		{"B1", "both.default.loads", true},
		{"A1", "a2.loads", false}, // another load's file
		{"A9", "a1.loads", false}, // no such load
		{"A1", "", false},
		{"", "a1.loads", false},
		{"a1", "a1.loads", false}, // names are exact
		{".incoming", "a1.loads", false},
		{"A1/..", "a1.loads", false},
	} {
		load, f, ok := m.Lookup(tc.load, tc.name)
		if ok != tc.ok {
			t.Errorf("Lookup(%q, %q) ok = %v, want %v", tc.load, tc.name, ok, tc.ok)
			continue
		}
		if ok && (load.Name != tc.load || f.Name != tc.name) {
			t.Errorf("Lookup(%q, %q) = %s/%s", tc.load, tc.name, load.Name, f.Name)
		}
	}
	// A2's own copy of a Shared name, not the default's.
	_, f, _ := m.Lookup("A2", "term.default.loads")
	if want := digest(content("A2", "term.default.loads")); f.SHA256 != want {
		t.Errorf("Lookup(A2, term.default.loads) digest = %s, want A2's %s", f.SHA256, want)
	}
}

// TestManifestReturnsCopies: nothing a caller does to a returned load
// changes the manifest.
func TestManifestReturnsCopies(t *testing.T) {
	m := testManifest(t)
	load, _, _ := m.Lookup("A1", "a1.loads")
	load.Files[0].SHA256 = "x"
	load.Models[0] = "x"
	load2, f, _ := m.Resolve("a1.loads")
	if f.SHA256 == "x" || load2.Files[0].SHA256 == "x" || load2.Models[0] == "x" {
		t.Error("changing a returned load changed the manifest")
	}
}

// TestNewManifestRefuses: the store serves by name across every driver, so
// the drivers' manifests together must keep ValidateFirmware's rules, and a
// load name must be a plain directory name that is not the upload area.
func TestNewManifestRefuses(t *testing.T) {
	if _, err := firmware.NewManifest(nil); err == nil {
		t.Error("NewManifest(nil): want an error")
	}
	one := func(loads ...drivers.FirmwareLoad) fakeDriver {
		return fakeDriver{name: "one", defaults: map[string]string{"one-1": loads[0].Name}, loads: loads}
	}
	two := func(loads ...drivers.FirmwareLoad) fakeDriver {
		return fakeDriver{name: "two", defaults: map[string]string{"two-1": loads[0].Name}, loads: loads}
	}
	load := func(name string, files ...drivers.FirmwareFile) drivers.FirmwareLoad {
		return drivers.FirmwareLoad{Name: name, Models: []string{"m"}, Files: files}
	}
	for what, ds := range map[string][]drivers.Driver{
		"one load name in two drivers": {
			one(load("L", file("L", "x.sbn", false))), two(load("L", file("L", "y.sbn", false))),
		},
		"one file name in two drivers, not Shared": {
			one(load("L1", file("L1", "x.sbn", false))), two(load("L2", file("L2", "x.sbn", false))),
		},
		"a load name starting with a dot": {one(load(".incoming", file(".incoming", "x.sbn", false)))},
		"a load name with a slash":        {one(load("a/b", file("a/b", "x.sbn", false)))},
		"a load name with a backslash":    {one(load(`a\b`, file(`a\b`, "x.sbn", false)))},
		"a load name with a NUL":          {one(load("a\x00", file("a\x00", "x.sbn", false)))},
		"a load name with a newline":      {one(load("a\n", file("a\n", "x.sbn", false)))},
		"a file name with a NUL":          {one(load("L", file("L", "x\x00", false)))},
		"a file name with a control":      {one(load("L", file("L", "x\x7f", false)))},
	} {
		reg, err := drivers.NewRegistry(ds...)
		if err != nil {
			t.Fatalf("%s: the registry itself refused: %v", what, err)
		}
		if _, err := firmware.NewManifest(reg); err == nil {
			t.Errorf("%s: NewManifest = nil error, want a refusal", what)
		}
	}

	// Two drivers may share a Shared name: only the store's resolution
	// minds, and it refuses to serve it (TestResolve: both.default.loads).
	reg, err := drivers.NewRegistry(
		one(load("L1", file("L1", "s.default.loads", true))), two(load("L2", file("L2", "s.default.loads", true))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firmware.NewManifest(reg); err != nil {
		t.Errorf("a Shared name in two drivers: %v", err)
	}
}

// TestBuiltinManifest: the shipped registry builds a manifest, and its
// Shared files resolve to the 79x5's default load.
func TestBuiltinManifest(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	m, err := firmware.NewManifest(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"term45.default.loads", "term65.default.loads", "apps45.9-4-2SR4-3.sbn"} {
		load, _, ok := m.Resolve(name)
		if !ok || load.Name != "SIP45.9-4-2SR4-3S" {
			t.Errorf("Resolve(%s) = %s %v, want SIP45.9-4-2SR4-3S", name, load.Name, ok)
		}
	}
}
