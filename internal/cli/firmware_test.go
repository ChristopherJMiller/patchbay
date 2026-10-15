package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// zipEntry is one entry of a generated zip. A name ending in "/" is a
// directory. With raw set, body is written as the entry's raw data under
// that header (zip.Writer.CreateRaw): how a test makes a header lie.
type zipEntry struct {
	name string
	body []byte
	raw  *zip.FileHeader
	mode fs.FileMode // non-zero: the entry's mode (a directory or symlink without a trailing slash)
}

// file is an entry holding a test load's genuine file.
func file(entry, load, name string) zipEntry {
	return zipEntry{name: entry, body: testFile(load, name)}
}

func writeZip(t *testing.T, entries ...zipEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		switch {
		case e.raw != nil:
			w, err := zw.CreateRaw(e.raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(e.body); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(e.name, "/"):
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Store}); err != nil {
				t.Fatal(err)
			}
		default:
			h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: testNow}
			if e.mode != 0 {
				h.SetMode(e.mode)
			}
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cmterm-test.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func shaOf(load, name string) string { return sha256Hex(testFile(load, name)) }

// corruptedA is load A's a.sbn with one byte changed.
func corruptedA() []byte {
	b := testFile(loadA, "a.sbn")
	b[10] ^= 0x20
	return b
}

func TestFirmwareLoadLayouts(t *testing.T) {
	aFiles := []zipEntry{
		file(loadA+".loads", loadA, loadA+".loads"),
		file("a.sbn", loadA, "a.sbn"),
		file(sharedName, loadA, sharedName),
	}
	uploadedA := "" +
		"TESTA.1-0-0S/TESTA.1-0-0S.loads: stored (201)\n" +
		"TESTA.1-0-0S/a.sbn: stored (201)\n" +
		"TESTA.1-0-0S/term45.default.loads: stored (201)\n"
	storedA := uploadedA + "load TESTA.1-0-0S: 3 of 3 files stored\n"
	putsA := []string{loadA + "/" + loadA + ".loads", loadA + "/a.sbn", loadA + "/" + sharedName}
	big := bytes.Repeat([]byte{0}, agentapi.MaxFirmwareFileSize+1)
	short := testFile(loadA, "a.sbn")

	for _, tc := range []struct {
		name     string
		entries  []zipEntry
		wantOut  string
		wantPuts []string
		wantErr  string // "" for success
	}{
		{
			name:     "flat",
			entries:  aFiles,
			wantOut:  storedA,
			wantPuts: putsA,
		},
		{
			name: "under a top-level folder",
			entries: []zipEntry{
				{name: "cmterm-testa/"},
				file("cmterm-testa/"+loadA+".loads", loadA, loadA+".loads"),
				file("cmterm-testa/a.sbn", loadA, "a.sbn"),
				file("cmterm-testa/"+sharedName, loadA, sharedName),
			},
			wantOut:  storedA,
			wantPuts: putsA,
		},
		{
			name: "extra files",
			entries: append(slices.Clone(aFiles),
				zipEntry{name: "readme.txt", body: []byte("read me")},
				zipEntry{name: "cmterm-testa.cop.sgn", body: []byte("a signed cop file")},
			),
			wantOut: "" +
				"cmterm-testa.cop.sgn: skipped: not in any firmware manifest\n" +
				"readme.txt: skipped: not in any firmware manifest\n" +
				storedA,
			wantPuts: putsA,
		},
		{
			name: "a name in the wrong case is never uploaded",
			entries: []zipEntry{
				{name: "A.SBN", body: testFile(loadA, "a.sbn")},
				{name: "Term45.Default.Loads", body: testFile(loadA, sharedName)},
				file("b.sbn", loadB, "b.sbn"),
			},
			wantOut: "" +
				"A.SBN: not uploaded: differs only in case from a.sbn (load TESTA.1-0-0S)\n" +
				"Term45.Default.Loads: not uploaded: differs only in case from term45.default.loads (loads TESTA.1-0-0S, TESTB.2-0-0S)\n" +
				"TESTB.2-0-0S/b.sbn: stored (201)\n" +
				"load TESTB.2-0-0S: 1 of 2 files stored\n",
			wantPuts: []string{loadB + "/b.sbn"},
			wantErr:  "2 files not stored",
		},
		{
			name: "another load's files too",
			entries: append(slices.Clone(aFiles),
				file("other/b.sbn", loadB, "b.sbn"),
				file("other/"+sharedName, loadB, sharedName),
			),
			wantOut: uploadedA +
				"TESTB.2-0-0S/b.sbn: stored (201)\n" +
				"TESTB.2-0-0S/term45.default.loads: stored (201)\n" +
				"load TESTA.1-0-0S: 3 of 3 files stored\n" +
				"load TESTB.2-0-0S: 2 of 2 files stored\n",
			wantPuts: append(slices.Clone(putsA), loadB+"/b.sbn", loadB+"/"+sharedName),
		},
		{
			name:    "the Shared name of load A goes to A only",
			entries: []zipEntry{file(sharedName, loadA, sharedName)},
			wantOut: "" +
				"TESTA.1-0-0S/term45.default.loads: stored (201)\n" +
				"load TESTA.1-0-0S: 1 of 3 files stored\n",
			wantPuts: []string{loadA + "/" + sharedName},
		},
		{
			name:    "the Shared name of load B goes to B only",
			entries: []zipEntry{file("SIP/"+sharedName, loadB, sharedName)},
			wantOut: "" +
				"TESTB.2-0-0S/term45.default.loads: stored (201)\n" +
				"load TESTB.2-0-0S: 1 of 2 files stored\n",
			wantPuts: []string{loadB + "/" + sharedName},
		},
		{
			name:    "a Shared name matching no load",
			entries: []zipEntry{{name: sharedName, body: testFile(loadP, sharedName)}},
			wantOut: sharedName + ": not uploaded: its SHA-256 " + shaOf(loadP, sharedName) +
				" is not the manifest's for loads TESTA.1-0-0S, TESTB.2-0-0S\n",
			wantErr: "1 file not stored",
		},
		{
			name:    "a pending digest",
			entries: []zipEntry{file("cmterm-testp/p.sbn", loadP, "p.sbn"), file("b.sbn", loadB, "b.sbn")},
			wantOut: "" +
				"cmterm-testp/p.sbn: not uploaded: the manifest has no SHA-256 for it yet (load TESTP.0-0-0S); its SHA-256 is " + shaOf(loadP, "p.sbn") + "\n" +
				"TESTB.2-0-0S/b.sbn: stored (201)\n" +
				"load TESTB.2-0-0S: 1 of 2 files stored\n",
			wantPuts: []string{loadB + "/b.sbn"},
			wantErr:  "1 file not stored",
		},
		{
			name: "a corrupted file",
			entries: []zipEntry{
				{name: "cmterm-testa/a.sbn", body: corruptedA()},
				file("cmterm-testa/"+loadA+".loads", loadA, loadA+".loads"),
			},
			wantOut: "" +
				"cmterm-testa/a.sbn: not uploaded: its SHA-256 " + sha256Hex(corruptedA()) + " is not the manifest's for load TESTA.1-0-0S\n" +
				"TESTA.1-0-0S/TESTA.1-0-0S.loads: stored (201)\n" +
				"load TESTA.1-0-0S: 1 of 3 files stored\n",
			wantPuts: []string{loadA + "/" + loadA + ".loads"},
			wantErr:  "1 file not stored",
		},
		{
			name:    "only a corrupted file: nothing to upload",
			entries: []zipEntry{{name: "cmterm-testa/readme.txt", body: []byte("hi")}, {name: "cmterm-testa/a.sbn", body: corruptedA()}},
			wantOut: "" +
				"cmterm-testa/a.sbn: not uploaded: its SHA-256 " + sha256Hex(corruptedA()) + " is not the manifest's for load TESTA.1-0-0S\n" +
				"cmterm-testa/readme.txt: skipped: not in any firmware manifest\n",
			wantErr: "1 file not stored",
		},
		{
			name:    "no match",
			entries: []zipEntry{{name: "readme.txt", body: []byte("hi")}, {name: "docs/"}},
			wantOut: "readme.txt: skipped: not in any firmware manifest\n",
			wantErr: "no file in ",
		},
		{
			name:    "an empty zip",
			wantErr: "no file in ",
		},
		{
			name: "a duplicate entry is uploaded once",
			entries: []zipEntry{
				file("a.sbn", loadA, "a.sbn"),
				file("copy/a.sbn", loadA, "a.sbn"),
			},
			wantOut: "" +
				"copy/a.sbn: skipped: TESTA.1-0-0S/a.sbn already comes from a.sbn\n" +
				"TESTA.1-0-0S/a.sbn: stored (201)\n" +
				"load TESTA.1-0-0S: 1 of 3 files stored\n",
			wantPuts: []string{loadA + "/a.sbn"},
		},
		{
			name: "a duplicate that differs is a failure",
			entries: []zipEntry{
				file("a.sbn", loadA, "a.sbn"),
				{name: "copy/a.sbn", body: corruptedA()},
			},
			wantOut: "" +
				"copy/a.sbn: not uploaded: its SHA-256 " + sha256Hex(corruptedA()) + " is not the manifest's for load TESTA.1-0-0S\n" +
				"TESTA.1-0-0S/a.sbn: stored (201)\n" +
				"load TESTA.1-0-0S: 1 of 3 files stored\n",
			wantPuts: []string{loadA + "/a.sbn"},
			wantErr:  "1 file not stored",
		},
		{
			name:    "an oversized entry",
			entries: []zipEntry{{name: "big/a.sbn", body: big}, file("b.sbn", loadB, "b.sbn")},
			wantOut: "" +
				"big/a.sbn: not uploaded: 33554433 bytes, over the agent's limit of 33554432\n" +
				"TESTB.2-0-0S/b.sbn: stored (201)\n" +
				"load TESTB.2-0-0S: 1 of 2 files stored\n",
			wantPuts: []string{loadB + "/b.sbn"},
			wantErr:  "1 file not stored",
		},
		{
			name: "a header declaring more than the limit is refused unread",
			entries: []zipEntry{{name: "a.sbn", body: short, raw: &zip.FileHeader{
				Name: "a.sbn", Method: zip.Store, CRC32: crc32.ChecksumIEEE(short),
				CompressedSize64: uint64(len(short)), UncompressedSize64: agentapi.MaxFirmwareFileSize + 1,
			}}},
			wantOut: "a.sbn: not uploaded: 33554433 bytes, over the agent's limit of 33554432\n",
			wantErr: "1 file not stored",
		},
		{
			name: "data longer than its header says",
			entries: []zipEntry{{name: "a.sbn", body: append(slices.Clone(short), "more"...), raw: &zip.FileHeader{
				Name: "a.sbn", Method: zip.Store, CRC32: crc32.ChecksumIEEE(short),
				CompressedSize64: uint64(len(short) + 4), UncompressedSize64: uint64(len(short)),
			}}},
			wantOut: "a.sbn: not uploaded: reading it: zip: not a valid zip file\n",
			wantErr: "1 file not stored",
		},
		{
			name: "a checksum that does not match its data",
			entries: []zipEntry{{name: "a.sbn", body: short, raw: &zip.FileHeader{
				Name: "a.sbn", Method: zip.Store, CRC32: crc32.ChecksumIEEE(short) + 1,
				CompressedSize64: uint64(len(short)), UncompressedSize64: uint64(len(short)),
			}}},
			wantOut: "a.sbn: not uploaded: reading it: zip: checksum error\n",
			wantErr: "1 file not stored",
		},
		{
			name: "a directory without a trailing slash is skipped, a symlink refused",
			entries: []zipEntry{
				{name: "cmterm/a.sbn", mode: fs.ModeDir | 0o755},
				{name: "link/a.sbn", body: []byte("../a.sbn"), mode: fs.ModeSymlink | 0o777},
				file(loadA+".loads", loadA, loadA+".loads"),
			},
			wantOut: "" +
				"link/a.sbn: not uploaded: not a regular file\n" +
				"TESTA.1-0-0S/TESTA.1-0-0S.loads: stored (201)\n" +
				"load TESTA.1-0-0S: 1 of 3 files stored\n",
			wantPuts: []string{loadA + "/" + loadA + ".loads"},
			wantErr:  "1 file not stored",
		},
		{
			name: "../ and \\ in names: the base name only",
			entries: []zipEntry{
				file("../../"+loadA+".loads", loadA, loadA+".loads"),
				file(`cmterm\firmware\a.sbn`, loadA, "a.sbn"),
				file("/abs/"+sharedName, loadA, sharedName),
				{name: `cmterm\`, body: []byte("not a file")},
			},
			wantOut:  storedA,
			wantPuts: putsA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			path := writeZip(t, tc.entries...)
			err := cli.FirmwareLoad(context.Background(), te.Env, path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error %v, want one containing %q", err, tc.wantErr)
			}
			if te.out.String() != tc.wantOut {
				t.Errorf("output:\n%s\nwant:\n%s", te.out, tc.wantOut)
			}
			if got := te.agent.putsSoFar(); !slices.Equal(got, tc.wantPuts) {
				t.Errorf("uploaded %v, want %v", got, tc.wantPuts)
			}
			opens, _ := te.opener.counts()
			if wantOpens := min(len(tc.wantPuts), 1); opens != wantOpens {
				t.Errorf("opened the agent %d times, want %d", opens, wantOpens)
			}
			if te.agent.unauthorized != 0 {
				t.Errorf("%d requests without the token", te.agent.unauthorized)
			}
		})
	}
}

// Two loads with the same digest for one Shared name both get it.
func TestFirmwareLoadEqualDigests(t *testing.T) {
	same := testFile("any", "same.loads")
	sameFile := drivers.FirmwareFile{Name: "same.loads", SHA256: sha256Hex(same), Size: int64(len(same)), Shared: true}
	reg, err := drivers.NewRegistry(fakeDriver{
		name: "twins", models: []string{"twin"},
		loads: []drivers.FirmwareLoad{
			{Name: "TWIN.1", Source: "z", Models: []string{"twin"}, Files: []drivers.FirmwareFile{sameFile}},
			{Name: "TWIN.2", Source: "z", Models: []string{"twin"}, Files: []drivers.FirmwareFile{sameFile}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	te := newTestEnv(t)
	te.Registry = reg
	te.agent.reg = reg
	if err := cli.FirmwareLoad(context.Background(), te.Env, writeZip(t, zipEntry{name: "same.loads", body: same})); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"TWIN.1/same.loads: stored (201)\n" +
		"TWIN.2/same.loads: stored (201)\n" +
		"load TWIN.1: 1 of 1 files stored\n" +
		"load TWIN.2: 1 of 1 files stored\n"
	if te.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", te.out, want)
	}
}

func TestFirmwareLoadAgentFailures(t *testing.T) {
	te := newTestEnv(t)
	te.agent.answer[loadA+"/a.sbn"] = http.StatusUnprocessableEntity
	te.agent.answer[loadA+"/"+loadA+".loads"] = http.StatusPreconditionFailed
	te.agent.answer[loadB+"/b.sbn"] = http.StatusInternalServerError
	path := writeZip(t,
		file(loadA+".loads", loadA, loadA+".loads"),
		file("a.sbn", loadA, "a.sbn"),
		file(sharedName, loadA, sharedName),
		file("b.sbn", loadB, "b.sbn"),
	)
	err := cli.FirmwareLoad(context.Background(), te.Env, path)
	if err == nil || !strings.Contains(err.Error(), "3 files not stored") {
		t.Errorf("error %v, want 3 files not stored", err)
	}
	want := "" +
		"TESTA.1-0-0S/TESTA.1-0-0S.loads: refused by the agent (412): injected 412\n" +
		"TESTA.1-0-0S/a.sbn: refused by the agent (422): injected 422\n" +
		"TESTA.1-0-0S/term45.default.loads: stored (201)\n" +
		"TESTB.2-0-0S/b.sbn: refused by the agent (500): injected 500\n" +
		"load TESTA.1-0-0S: 1 of 3 files stored\n" +
		"load TESTB.2-0-0S: 0 of 2 files stored\n"
	if te.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", te.out, want)
	}
	if got := len(te.agent.putsSoFar()); got != 4 {
		t.Errorf("%d uploads, want all 4 tried", got)
	}
}

// Uploads are idempotent: a second run reports what is there already.
func TestFirmwareLoadAgain(t *testing.T) {
	te := newTestEnv(t)
	path := writeZip(t, file("a.sbn", loadA, "a.sbn"))
	for range 2 {
		if err := cli.FirmwareLoad(context.Background(), te.Env, path); err != nil {
			t.Fatal(err)
		}
	}
	want := "" +
		"TESTA.1-0-0S/a.sbn: stored (201)\nload TESTA.1-0-0S: 1 of 3 files stored\n" +
		"TESTA.1-0-0S/a.sbn: already stored (200)\nload TESTA.1-0-0S: 1 of 3 files stored\n"
	if te.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", te.out, want)
	}
}

func TestFirmwareLoadAgentUnreachable(t *testing.T) {
	te := newTestEnv(t)
	te.opener.err = errors.New("found 0 running PBX pods in namespace telephony")
	err := cli.FirmwareLoad(context.Background(), te.Env, writeZip(t, file("a.sbn", loadA, "a.sbn")))
	if err == nil || !strings.Contains(err.Error(), "found 0 running PBX pods") {
		t.Errorf("error %v, want the opener's", err)
	}
}

// The connection drops mid-upload: that file failed, the next is still
// tried.
func TestFirmwareLoadConnectionDrops(t *testing.T) {
	te := newTestEnv(t)
	te.agent.answer[loadA+"/"+loadA+".loads"] = dropConnection
	err := cli.FirmwareLoad(context.Background(), te.Env, writeZip(t,
		file(loadA+".loads", loadA, loadA+".loads"),
		file("a.sbn", loadA, "a.sbn"),
	))
	if err == nil || !strings.Contains(err.Error(), "1 file not stored") {
		t.Errorf("error %v, want 1 file not stored", err)
	}
	out := te.out.String()
	if !strings.HasPrefix(out, "TESTA.1-0-0S/TESTA.1-0-0S.loads: not uploaded: ") ||
		!strings.Contains(out, "TESTA.1-0-0S/a.sbn: stored (201)\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestFirmwareLoadBadZip(t *testing.T) {
	te := newTestEnv(t)
	dir := t.TempDir()
	notZip := filepath.Join(dir, "firmware.zip")
	if err := os.WriteFile(notZip, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notZip, filepath.Join(dir, "missing.zip")} {
		if err := cli.FirmwareLoad(context.Background(), te.Env, path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("FirmwareLoad(%s) = %v, want an error naming it", path, err)
		}
	}
}

// A cancelled context ends the uploads.
func TestFirmwareLoadCancelled(t *testing.T) {
	te := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cli.FirmwareLoad(ctx, te.Env, writeZip(t, file("a.sbn", loadA, "a.sbn")))
	if err == nil || !strings.Contains(te.out.String(), "TESTA.1-0-0S/a.sbn: not uploaded: ") || !strings.Contains(te.out.String(), "context canceled") {
		t.Errorf("error %v, output %q: want a.sbn not uploaded, context canceled", err, te.out)
	}
}

// With GODEBUG=zipinsecurepath=0, Go's zip reader reports ../ and absolute
// names as ErrInsecurePath; the CLI never extracts, so it reads on.
func TestFirmwareLoadInsecurePaths(t *testing.T) {
	t.Setenv("GODEBUG", "zipinsecurepath=0")
	te := newTestEnv(t)
	path := writeZip(t, file("../../a.sbn", loadA, "a.sbn"), file("/abs/b.sbn", loadB, "b.sbn"))
	if err := cli.FirmwareLoad(context.Background(), te.Env, path); err != nil {
		t.Fatal(err)
	}
	if got, want := te.agent.putsSoFar(), []string{loadA + "/a.sbn", loadB + "/b.sbn"}; !slices.Equal(got, want) {
		t.Errorf("uploaded %v, want %v", got, want)
	}
}
