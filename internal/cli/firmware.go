package cli

import (
	"archive/zip"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// uploadTimeout bounds one file's upload through the port-forward (the
// largest 79x5 file is about 4.6 MB).
const uploadTimeout = 5 * time.Minute

// FirmwareLoad checks the files of a firmware zip against every driver's
// manifest (env.Registry) locally, then uploads each match to the agent
// (PUT api.FirmwarePath through env.Agent), which verifies it again before
// storing it (design §6, plan 6.2.7, Review Focus 3).
//
// The zip is read in place, never extracted. Entries are taken in name
// order; directories are skipped. An entry is known by its base name (after
// the last "/" or "\"), compared case-sensitively with every file of every
// load:
//   - a name no manifest has is skipped, unread; one that equals a manifest
//     name only ignoring case is a failure, never uploaded;
//   - a manifest name is read (at most api.MaxFirmwareFileSize bytes, by its
//     header and by what it holds) and hashed. It goes to every load that
//     pins that SHA-256 for the name, so a Shared name reaches only the load
//     it belongs to. With none, it is a failure that names the file and its
//     digest: pending when the manifest has no digest for it yet, else a
//     mismatch;
//   - a (load, name) an earlier entry already provides is not uploaded
//     again.
//
// The agent is opened once, and only when something matched; uploads go in
// (load, name) order and each prints its answer, then each load touched
// prints how many of its files this run stored. It returns an error when no
// entry is in any manifest, or when any file failed, locally or at the
// agent.
func FirmwareLoad(ctx context.Context, env *Env, zipPath string) error {
	if env.Registry == nil {
		return errors.New("firmware load: no driver registry")
	}
	zr, err := zip.OpenReader(zipPath)
	// ErrInsecurePath (GODEBUG=zipinsecurepath=0) is for extracting: names
	// here are only matched by their base name.
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return fmt.Errorf("opening %s: %w", zipPath, err)
	}
	defer func() { _ = zr.Close() }()

	man := newManifest(env.Registry)
	p := &printer{w: env.Out}
	plan := matchEntries(p, man, zr.File)
	if !plan.anyManifestName {
		if p.err != nil {
			return p.err
		}
		return fmt.Errorf("no file in %s is in a firmware manifest", zipPath)
	}
	failures := plan.failures
	if len(plan.uploads) > 0 {
		n, err := uploadAll(ctx, env, p, man, plan.uploads)
		if err != nil {
			return err
		}
		failures += n
	}
	if p.err != nil {
		return p.err
	}
	if failures > 0 {
		return fmt.Errorf("%s: %d %s not stored (see above)", zipPath, failures, plural(failures, "file", "files"))
	}
	return nil
}

// loadFile is one file of one load: what the agent stores, and what an
// upload is addressed to.
type loadFile struct{ load, name string }

// candidate is a load that carries a file name.
type candidate struct {
	load string
	file drivers.FirmwareFile
}

// manifest is every driver's firmware, indexed for matching.
type manifest struct {
	byName  map[string][]candidate // manifest file name -> the loads carrying it, in registry order
	byFold  map[string][]string    // lower-case name -> the manifest names, sorted
	inLoads map[string]int         // load -> its number of files
}

func newManifest(reg *drivers.Registry) manifest {
	m := manifest{byName: map[string][]candidate{}, byFold: map[string][]string{}, inLoads: map[string]int{}}
	for _, d := range reg.Drivers() {
		for _, l := range d.Firmware() {
			m.inLoads[l.Name] += len(l.Files)
			for _, f := range l.Files {
				if _, seen := m.byName[f.Name]; !seen {
					fold := strings.ToLower(f.Name)
					m.byFold[fold] = append(m.byFold[fold], f.Name)
					slices.Sort(m.byFold[fold])
				}
				m.byName[f.Name] = append(m.byName[f.Name], candidate{load: l.Name, file: f})
			}
		}
	}
	return m
}

// loadsOf names the loads of candidates: "load X" or "loads X, Y".
func loadsOf(cs []candidate) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.load
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return plural(len(names), "load", "loads") + " " + strings.Join(names, ", ")
}

// upload is one match to send: the entry and its size, as read.
type upload struct {
	loadFile
	entry *zip.File
	size  int64
}

// entryPlan is the local half of FirmwareLoad.
type entryPlan struct {
	uploads         []upload
	failures        int
	anyManifestName bool
}

// matchEntries checks every entry against the manifest, printing a line
// for each one skipped or refused.
func matchEntries(p *printer, man manifest, entries []*zip.File) entryPlan {
	var plan entryPlan
	from := map[loadFile]string{} // -> the entry that provides it
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b *zip.File) int { return cmp.Compare(a.Name, b.Name) })
	fail := func(entry, format string, args ...any) {
		plan.failures++
		p.printf("%s: not uploaded: %s\n", entry, fmt.Sprintf(format, args...))
	}
	for _, f := range sorted {
		base := baseName(f.Name)
		if f.FileInfo().IsDir() || base == "" {
			continue
		}
		cands := man.byName[base]
		if len(cands) == 0 {
			if names := man.byFold[strings.ToLower(base)]; len(names) > 0 {
				fail(f.Name, "differs only in case from %s (%s)", names[0], loadsOf(man.byName[names[0]]))
			} else {
				p.printf("%s: skipped: not in any firmware manifest\n", f.Name)
			}
			continue
		}
		plan.anyManifestName = true
		if !f.Mode().IsRegular() {
			fail(f.Name, "not a regular file")
			continue
		}
		if f.UncompressedSize64 > agentapi.MaxFirmwareFileSize {
			fail(f.Name, "%d bytes, over the agent's limit of %d", f.UncompressedSize64, agentapi.MaxFirmwareFileSize)
			continue
		}
		sum, size, err := hashEntry(f)
		if err != nil {
			fail(f.Name, "%v", err)
			continue
		}
		var hits, pinned, pending []candidate
		for _, c := range cands {
			switch {
			case c.file.Pending():
				pending = append(pending, c)
			case c.file.SHA256 == sum:
				hits = append(hits, c)
				pinned = append(pinned, c)
			default:
				pinned = append(pinned, c)
			}
		}
		switch {
		case len(hits) == 0 && len(pinned) == 0:
			fail(f.Name, "the manifest has no SHA-256 for it yet (%s); its SHA-256 is %s", loadsOf(pending), sum)
			continue
		case len(hits) == 0 && len(pending) == 0:
			fail(f.Name, "its SHA-256 %s is not the manifest's for %s", sum, loadsOf(pinned))
			continue
		case len(hits) == 0:
			fail(f.Name, "its SHA-256 %s is not the manifest's for %s, and the manifest has no SHA-256 for it yet in %s",
				sum, loadsOf(pinned), loadsOf(pending))
			continue
		}
		for _, h := range hits {
			k := loadFile{load: h.load, name: base}
			if earlier, dup := from[k]; dup {
				p.printf("%s: skipped: %s/%s already comes from %s\n", f.Name, k.load, k.name, earlier)
				continue
			}
			from[k] = f.Name
			plan.uploads = append(plan.uploads, upload{loadFile: k, entry: f, size: size})
		}
	}
	return plan
}

// baseName is an entry's name after its last "/" or "\": zips made on
// Windows use either.
func baseName(entry string) string {
	if i := strings.LastIndexAny(entry, `/\`); i >= 0 {
		return entry[i+1:]
	}
	return entry
}

// hashEntry reads an entry once, at most MaxFirmwareFileSize+1 bytes, and
// returns its SHA-256 and size. The zip reader also refuses data longer or
// shorter than the header says, and a CRC-32 that does not match.
func hashEntry(f *zip.File) (sum string, size int64, err error) {
	rc, err := f.Open()
	if err != nil {
		return "", 0, fmt.Errorf("reading it: %w", err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	size, err = io.Copy(h, io.LimitReader(rc, agentapi.MaxFirmwareFileSize+1))
	if err != nil {
		return "", 0, fmt.Errorf("reading it: %w", err)
	}
	if size > agentapi.MaxFirmwareFileSize {
		return "", 0, fmt.Errorf("over the agent's limit of %d bytes", agentapi.MaxFirmwareFileSize)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// uploadAll opens the agent once and PUTs every match in (load, name)
// order, then prints per load how many files this run stored. It returns
// how many uploads failed; an error only when the agent cannot be opened.
func uploadAll(ctx context.Context, env *Env, p *printer, man manifest, uploads []upload) (int, error) {
	if env.Agent == nil {
		return 0, errors.New("firmware load: no way to reach the agent")
	}
	conn, err := env.Agent.OpenAgent(ctx)
	if err != nil {
		return 0, fmt.Errorf("reaching the agent: %w", err)
	}
	defer func() { _ = conn.Close() }()
	slices.SortFunc(uploads, func(a, b upload) int {
		return cmp.Or(cmp.Compare(a.load, b.load), cmp.Compare(a.name, b.name))
	})
	failures := 0
	stored := map[string]int{}
	var loads []string
	for _, u := range uploads {
		if !slices.Contains(loads, u.load) {
			loads = append(loads, u.load)
		}
		code, msg, err := put(ctx, conn, u)
		switch {
		case err != nil:
			failures++
			p.printf("%s/%s: not uploaded: %v\n", u.load, u.name, err)
		case code == http.StatusCreated:
			stored[u.load]++
			p.printf("%s/%s: stored (201)\n", u.load, u.name)
		case code == http.StatusOK:
			stored[u.load]++
			p.printf("%s/%s: already stored (200)\n", u.load, u.name)
		default:
			failures++
			p.printf("%s/%s: refused by the agent (%d): %s\n", u.load, u.name, code, msg)
		}
	}
	for _, l := range loads {
		p.printf("load %s: %d of %d files stored\n", l, stored[l], man.inLoads[l])
	}
	return failures, nil
}

// put streams one entry to the agent, again from the zip, with its size as
// the Content-Length. The agent verifies the SHA-256 itself.
func put(ctx context.Context, conn *AgentConn, u upload) (code int, msg string, err error) {
	rc, err := u.entry.Open()
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = rc.Close() }()
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	res, err := agentRequest(ctx, conn, http.MethodPut, agentapi.FirmwarePath(u.load, u.name), rc, u.size)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusCreated || res.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxAnswerBody))
		return res.StatusCode, "", nil
	}
	return res.StatusCode, answerError(res), nil
}

// plural picks the word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
