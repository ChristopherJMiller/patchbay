package firmware

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Manifest is every registered driver's firmware manifest, indexed the two
// ways the store needs: by (load, name) for an upload, which names its
// load, and by name alone for a device, which does not.
//
// The registry checks each driver's manifest on its own; TFTP serves by
// name across all of them, so NewManifest also checks them together.
type Manifest struct {
	// loads is every load of every driver, in registry, load and file
	// order: the order Status lists them in.
	loads  []drivers.FirmwareLoad
	byLoad map[string]int // load name -> index in loads
	// byName is how a device's request resolves: every name that is not
	// Shared, and every Shared name that exactly one default load carries.
	byName map[string]ref
	// unserved lists the Shared names no default load carries, or several
	// do: no device is ever served them. Sorted.
	unserved []string
}

// ref is one file of one load: indexes into Manifest.loads.
type ref struct{ load, file int }

// NewManifest indexes the firmware manifests of every driver in reg.
//
// A load is a default load when it is the profile DefaultFirmware of one
// of its driver's models and that driver's manifest has it. A Shared name
// (Cisco's term45.default.loads) resolves to the one default load that
// carries it; when none or several do, the name is not served at all,
// rather than served from a load a factory-reset phone was not meant to
// boot.
//
// It fails when the manifests together break drivers.ValidateFirmware (a
// load name in two drivers, a file name in two drivers that is not Shared
// in both) or a load or file name is not a plain name: load names are
// directories in the store, and none may be IncomingDir or start with a
// dot.
func NewManifest(reg *drivers.Registry) (*Manifest, error) {
	if reg == nil {
		return nil, errors.New("firmware: no driver registry")
	}
	var all []drivers.FirmwareLoad
	defaults := map[string]bool{} // load name -> is a default load
	for _, d := range reg.Drivers() {
		loads := d.Firmware()
		for _, model := range d.Models() {
			p, ok := d.Profile(model)
			if !ok || p.Provisioning.DefaultFirmware == "" {
				continue
			}
			if _, ok := drivers.LoadByName(loads, p.Provisioning.DefaultFirmware); ok {
				defaults[p.Provisioning.DefaultFirmware] = true
			}
		}
		all = append(all, cloneLoads(loads)...)
	}
	if err := drivers.ValidateFirmware(all); err != nil {
		return nil, fmt.Errorf("firmware: the drivers' manifests together: %w", err)
	}
	var errs []error
	for _, l := range all {
		if err := plainName(l.Name); err != nil || strings.HasPrefix(l.Name, ".") {
			errs = append(errs, fmt.Errorf("firmware: load %q is not a plain name that does not start with a dot", l.Name))
		}
		for _, f := range l.Files {
			if err := plainName(f.Name); err != nil {
				errs = append(errs, fmt.Errorf("firmware: load %s: file %q: %w", l.Name, f.Name, err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	m := &Manifest{loads: all, byLoad: map[string]int{}, byName: map[string]ref{}}
	shared := map[string][]ref{} // Shared name -> the default loads' entries
	sharedNames := map[string]bool{}
	for li, l := range all {
		m.byLoad[l.Name] = li
		for fi, f := range l.Files {
			switch {
			case !f.Shared:
				m.byName[f.Name] = ref{li, fi}
			case defaults[l.Name]:
				shared[f.Name] = append(shared[f.Name], ref{li, fi})
				sharedNames[f.Name] = true
			default:
				sharedNames[f.Name] = true
			}
		}
	}
	for name := range sharedNames {
		if refs := shared[name]; len(refs) == 1 {
			m.byName[name] = refs[0]
		} else {
			m.unserved = append(m.unserved, name)
		}
	}
	slices.Sort(m.unserved)
	return m, nil
}

// plainName refuses anything but a single, printable file name.
func plainName(s string) error {
	switch {
	case s == "" || s == "." || s == "..":
		return errors.New("empty, \".\" or \"..\"")
	case strings.ContainsAny(s, "/\\"):
		return errors.New("holds a slash")
	case !utf8.ValidString(s):
		return errors.New("not UTF-8")
	case strings.ContainsFunc(s, unicode.IsControl):
		return errors.New("holds a control character")
	}
	return nil
}

// Lookup returns the file name of load: an upload's target. Every manifest
// file is one, Shared or pending or not.
func (m *Manifest) Lookup(load, name string) (drivers.FirmwareLoad, drivers.FirmwareFile, bool) {
	li, ok := m.byLoad[load]
	if !ok {
		return drivers.FirmwareLoad{}, drivers.FirmwareFile{}, false
	}
	for fi, f := range m.loads[li].Files {
		if f.Name == name {
			return m.get(ref{li, fi})
		}
	}
	return drivers.FirmwareLoad{}, drivers.FirmwareFile{}, false
}

// Resolve returns the load and file a device's request for name is served
// from: the one load carrying it, or, for a Shared name, the one default
// load carrying it. ok is false for a name in no manifest and for a Shared
// name that no default load, or several, carry. A pending file resolves;
// the store then refuses to serve it.
func (m *Manifest) Resolve(name string) (drivers.FirmwareLoad, drivers.FirmwareFile, bool) {
	r, ok := m.byName[name]
	if !ok {
		return drivers.FirmwareLoad{}, drivers.FirmwareFile{}, false
	}
	return m.get(r)
}

func (m *Manifest) get(r ref) (drivers.FirmwareLoad, drivers.FirmwareFile, bool) {
	l := cloneLoads(m.loads[r.load : r.load+1])[0]
	return l, l.Files[r.file], true
}

// each calls f for every file of every load, in registry, load and file
// order.
func (m *Manifest) each(f func(load drivers.FirmwareLoad, file drivers.FirmwareFile)) {
	for _, l := range m.loads {
		for _, file := range l.Files {
			f(l, file)
		}
	}
}

// size is how many files the manifests hold.
func (m *Manifest) size() int {
	n := 0
	for _, l := range m.loads {
		n += len(l.Files)
	}
	return n
}

func cloneLoads(loads []drivers.FirmwareLoad) []drivers.FirmwareLoad {
	out := make([]drivers.FirmwareLoad, len(loads))
	for i, l := range loads {
		l.Models = slices.Clone(l.Models)
		l.Files = slices.Clone(l.Files)
		out[i] = l
	}
	return out
}
