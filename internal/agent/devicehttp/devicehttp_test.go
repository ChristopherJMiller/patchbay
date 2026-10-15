package devicehttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

const xmlType = "text/xml; charset=utf-8"

func ev(file string, result api.FetchResult) sensor.FetchEvent {
	return sensor.FetchEvent{Transport: api.TransportHTTPDevice, File: file, Result: result, At: t0, Source: "127.0.0.1"}
}

func treeEvent(name, version string, result api.FetchResult) sensor.FetchEvent {
	e := ev(name, result)
	e.BundleVersion, e.BundlePath = version, "http/"+name
	return e
}

func checkEvent(t *testing.T, got, want sensor.FetchEvent) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event\n %+v\nwant\n %+v", got, want)
	}
}

func TestServesTheHTTPTree(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	writeVersion(t, f.live, v2, v1Files("v2"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)

	r := f.get(t, "/directory.xml")
	if r.code != http.StatusOK || r.body != "<CiscoIPPhoneDirectory>v1</CiscoIPPhoneDirectory>" ||
		r.header.Get("Content-Type") != xmlType || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("GET /directory.xml = %d %q %v", r.code, r.body, r.header)
	}
	checkEvent(t, f.rec.next(t), treeEvent("directory.xml", v1, api.FetchServed))

	if r := f.get(t, "/apps/menu.xml"); r.code != http.StatusOK || r.body != "<CiscoIPPhoneMenu>v1</CiscoIPPhoneMenu>" {
		t.Errorf("GET /apps/menu.xml = %d %q", r.code, r.body)
	}
	checkEvent(t, f.rec.next(t), treeEvent("apps/menu.xml", v1, api.FetchServed))
	if r := f.get(t, "/logo.bin"); r.code != http.StatusOK || r.header.Get("Content-Type") != "application/octet-stream" || r.body != "\x00\x01binaryv1" {
		t.Errorf("GET /logo.bin = %d %q %v", r.code, r.body, r.header)
	}
	f.rec.next(t)

	// A swap: the next request is served from the new version.
	setCurrent(t, f.live, v2)
	if r := f.get(t, "/directory.xml"); r.body != "<CiscoIPPhoneDirectory>v2</CiscoIPPhoneDirectory>" {
		t.Errorf("after the swap: %q", r.body)
	}
	checkEvent(t, f.rec.next(t), treeEvent("directory.xml", v2, api.FetchServed))

	// HEAD: the headers, no body, nothing recorded.
	if r := f.do(t, http.MethodHead, "/directory.xml"); r.code != http.StatusOK || r.body != "" ||
		r.header.Get("Content-Length") != strconv.Itoa(len("<CiscoIPPhoneDirectory>v2</CiscoIPPhoneDirectory>")) || r.header.Get("Content-Type") != xmlType {
		t.Errorf("HEAD /directory.xml = %d %q %v", r.code, r.body, r.header)
	}
	f.rec.none(t, f)
	// Anything but GET and HEAD: 405, nothing recorded.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, "PROPFIND"} {
		if r := f.do(t, m, "/directory.xml"); r.code != http.StatusMethodNotAllowed || r.header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /directory.xml = %d %v", m, r.code, r.header)
		}
	}
	f.rec.none(t, f)
}

// TestNothingElseFromTheLiveTree: only http/ is served, by path; no
// directory listing, no other tree, no escape.
func TestNothingElseFromTheLiveTree(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	// A symlink inside http/ that points into another tree.
	must(t, os.Symlink("../provisioning/softphone-den.xml", filepath.Join(f.live, "v"+v1, "http", "den.xml")))
	must(t, os.Symlink("/etc/passwd", filepath.Join(f.live, "v"+v1, "http", "passwd.xml")))

	secrets := []string{"tftp secret", "asterisk secret", "den document", "lab document", "root:", "softphone-den", "bundle"}
	for _, target := range []string{
		"/", "/apps", "/apps/", "/http/directory.xml",
		"/tftp/SEPD0C78915E8C8.cnf.xml", "/SEPD0C78915E8C8.cnf.xml",
		"/asterisk/pjsip.conf", "/pjsip.conf",
		"/provisioning/softphone-den.xml", "/softphone-den.xml", "/den.xml", "/passwd.xml",
		"/bundle.json", "/current/http/directory.xml", "/v" + v1 + "/http/directory.xml",
		"/../asterisk/pjsip.conf", "/%2e%2e/asterisk/pjsip.conf", "/..%2fprovisioning%2fsoftphone-den.xml",
		"/apps/../../asterisk/pjsip.conf", "/./directory.xml", "//directory.xml", "/apps//menu.xml",
		"/directory.xml/", "/apps\\menu.xml", "/directory.xml%00", "/DIRECTORY.XML",
	} {
		r := f.raw(t, http.MethodGet, target)
		if !strings.HasPrefix(r, "HTTP/1.1 404") && !strings.HasPrefix(r, "HTTP/1.1 400") && !strings.HasPrefix(r, "HTTP/1.1 503") {
			t.Errorf("GET %s:\n%s", target, r)
		}
		for _, s := range secrets {
			if strings.Contains(r, s) {
				t.Errorf("GET %s leaked %q:\n%s", target, s, r)
			}
		}
	}
	for _, e := range f.rec.all() {
		if e.Result == api.FetchServed {
			t.Errorf("served %+v", e)
		}
	}
}

func TestRecordsWhatWasAskedFor(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)

	f.get(t, "/missing.xml")
	checkEvent(t, f.rec.next(t), treeEvent("missing.xml", v1, api.FetchNotFound))
	f.get(t, "/apps")
	checkEvent(t, f.rec.next(t), treeEvent("apps", v1, api.FetchNotFound))
	// A name no bundle can hold: recorded quoted, without a bundle path.
	f.raw(t, http.MethodGet, "/../asterisk/pjsip.conf")
	checkEvent(t, f.rec.next(t), ev(`"../asterisk/pjsip.conf"`, api.FetchNotFound))
	f.raw(t, http.MethodGet, "/")
	checkEvent(t, f.rec.next(t), ev(`""`, api.FetchNotFound))
	long := strings.Repeat("a", 300) + ".xml"
	f.get(t, "/"+long)
	checkEvent(t, f.rec.next(t), ev(`"`+strings.Repeat("a", 199), api.FetchNotFound))
	f.get(t, "/caf%C3%A9.xml")
	checkEvent(t, f.rec.next(t), ev(`"caf\u00e9.xml"`, api.FetchNotFound))
}

func TestBeforeFirstApply(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/directory.xml", "/p/" + tokDen} {
		if r := f.get(t, p); r.code != http.StatusNotFound {
			t.Errorf("GET %s before the first apply = %d", p, r.code)
		}
	}
	checkEvent(t, f.rec.next(t), ev("directory.xml", api.FetchNotFound))
	checkEvent(t, f.rec.next(t), ev(sensor.ProvisioningUnknownFile, api.FetchNotFound))
	if s := f.stateFile(t); s != "" {
		t.Errorf("a state file before any spend: %s", s)
	}
}

func provisioned(device, file, bundle, version string) sensor.FetchEvent {
	e := ev(file, api.FetchServed)
	e.BundleVersion, e.BundlePath, e.Device, e.Version = bundle, file, device, version
	return e
}

func TestProvisioningOneUse(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	writeVersion(t, f.live, v2, v1Files("v2"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))
	// current moved on: the grant's own version still serves its document.
	setCurrent(t, f.live, v2)

	r := f.get(t, "/p/"+tokDen)
	if r.code != http.StatusOK || r.body != "<config>den document v1</config>" ||
		r.header.Get("Content-Type") != xmlType || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("first GET = %d %q %v", r.code, r.body, r.header)
	}
	checkEvent(t, f.rec.next(t), provisioned("softphone-den", denFile, v1, denV1))

	r = f.get(t, "/p/"+tokDen)
	if r.code != http.StatusNotFound || strings.Contains(r.body, "den document") {
		t.Errorf("second GET = %d %q, want 404", r.code, r.body)
	}
	spent := ev(denFile, api.FetchNotFound)
	spent.BundleVersion, spent.BundlePath, spent.Device = v1, denFile, "softphone-den"
	checkEvent(t, f.rec.next(t), spent)

	// The other token is its own.
	if r := f.get(t, "/p/"+tokLab); r.code != http.StatusOK || r.body != "<config>lab document v1</config>" {
		t.Errorf("lab = %d %q", r.code, r.body)
	}
	checkEvent(t, f.rec.next(t), provisioned("softphone-lab", labFile, v1, labV1))

	// HEAD never spends a token and never tells whether one is good.
	f.grants.set(v1, grant("softphone-den", tokDen2, denFile, v1, denV1))
	if r := f.do(t, http.MethodHead, "/p/"+tokDen2); r.code != http.StatusNotFound {
		t.Errorf("HEAD of a good token = %d, want the 404", r.code)
	}
	f.rec.none(t, f)
	if r := f.get(t, "/p/"+tokDen2); r.code != http.StatusOK {
		t.Errorf("GET after a HEAD = %d, want 200: HEAD spent it", r.code)
	}
	f.rec.next(t)
}

func TestProvisioningOneUseUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
	)
	for range 20 {
		wg.Go(func() {
			r := f.get(t, "/p/"+tokDen)
			mu.Lock()
			codes[r.code]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusNotFound] != 19 {
		t.Errorf("20 concurrent GETs of one token: %v, want one 200 and 19 404s", codes)
	}
}

func TestProvisioningDocumentMissing(t *testing.T) {
	f := newFixture(t)
	files := v1Files("v1")
	delete(files, denFile)
	writeVersion(t, f.live, v1, files, v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1))

	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusNotFound {
		t.Errorf("a missing document = %d, want 404", r.code)
	}
	missing := ev(denFile, api.FetchNotFound)
	missing.BundleVersion, missing.BundlePath, missing.Device = v1, denFile, "softphone-den"
	checkEvent(t, f.rec.next(t), missing)
	if s := f.stateFile(t); s != "" {
		t.Errorf("a missing document spent the token: %s", s)
	}
	// Once the document is there, the token still works.
	must(t, os.WriteFile(filepath.Join(f.live, "v"+v1, denFile), []byte("<config>late</config>"), 0o600))
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusOK || r.body != "<config>late</config>" {
		t.Errorf("then = %d %q", r.code, r.body)
	}
}

func TestIdentical404s(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusOK {
		t.Fatalf("spending: %d", r.code)
	}

	// The one 404, byte for byte but its Date, whatever the reason.
	want := f.raw(t, http.MethodGet, "/nothing-at-all.xml")
	head, body, _ := strings.Cut(want, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	slices.Sort(lines[1:])
	if lines[0] != "HTTP/1.1 404 Not Found" || body != "404 page not found\n" || !slices.Equal(lines[1:], []string{
		"Connection: close", "Content-Length: 19", "Content-Type: text/plain; charset=utf-8", "X-Content-Type-Options: nosniff",
	}) {
		t.Fatalf("the 404:\n%s", want)
	}
	for name, target := range map[string]string{
		"unknown path":         "/nothing.xml",
		"unknown token":        "/p/" + tokNone,
		"spent token":          "/p/" + tokDen,
		"token followed by /x": "/p/" + tokLab + "/x",
		"bare prefix":          "/p/",
		"a directory":          "/apps",
		"an invalid name":      "/a//b",
		"no prefix slash":      "/p",
	} {
		if got := f.raw(t, http.MethodGet, target); got != want {
			t.Errorf("%s (%s):\n%q\nwant\n%q", name, target, got, want)
		}
	}
	// The unspent lab token was not spent by the /x request.
	if r := f.get(t, "/p/"+tokLab); r.code != http.StatusOK {
		t.Errorf("lab after /p/<lab>/x = %d, want 200", r.code)
	}
	wantHead := strings.TrimSuffix(want, "404 page not found\n")
	for _, target := range []string{"/nothing.xml", "/p/" + tokNone, "/p/" + tokDen, "/p/" + tokLab, "/p/"} {
		if got := f.raw(t, http.MethodHead, target); got != wantHead {
			t.Errorf("HEAD %s:\n%q\nwant\n%q", target, got, wantHead)
		}
	}
}

func TestComparesEveryGrant(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1,
		grant("softphone-den", tokDen, denFile, v1, denV1),
		grant("softphone-lab", tokLab, labFile, v1, labV1),
		grant("softphone-other", "", "provisioning/softphone-other.xml", v1, denV1),
	)
	var (
		mu       sync.Mutex
		compared []int
	)
	restore := devicehttp.SetCompareHook(func(i int) {
		mu.Lock()
		defer mu.Unlock()
		compared = append(compared, i)
	})
	defer restore()
	for _, tok := range []string{tokDen, tokLab, tokNone, tokDen} {
		mu.Lock()
		compared = nil
		mu.Unlock()
		f.get(t, "/p/"+tok)
		mu.Lock()
		if !slices.Equal(compared, []int{0, 1, 2}) {
			t.Errorf("token %s...: compared grants %v, want all three in order", tok[:24], compared)
		}
		mu.Unlock()
	}
	if r := f.get(t, "/p/x"); r.code != http.StatusNotFound {
		t.Errorf("a short token matched: %d", r.code)
	}
}

func TestServedProvisioningCarriesVersion(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1))
	f.get(t, "/p/"+tokDen)
	got := f.rec.next(t)
	if got.Version != denV1 || got.Device != "softphone-den" || got.Result != api.FetchServed || got.File != denFile {
		t.Errorf("served event %+v, want the grant's device and version", got)
	}
	// The persisted spend holds the same event.
	var st struct {
		Spends []struct {
			TokenSHA256 string            `json:"tokenSHA256"`
			Event       sensor.FetchEvent `json:"event"`
		} `json:"spends"`
	}
	must(t, json.Unmarshal([]byte(f.stateFile(t)), &st))
	if len(st.Spends) != 1 || st.Spends[0].TokenSHA256 != digest(tokDen) || !reflect.DeepEqual(st.Spends[0].Event, got) {
		t.Errorf("state file %+v, want the spend of %s with event %+v", st, digest(tokDen), got)
	}
}

func TestSpendSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))
	f.get(t, "/p/"+tokDen)
	served := f.rec.next(t)
	f.now = t0.Add(time.Hour)
	f.get(t, "/p/"+tokLab)
	servedLab := f.rec.next(t)

	// The agent restarts before the controller polled: the new server
	// re-records both spends, as they were, and both tokens stay spent.
	f.start(t)
	checkEvent(t, f.rec.next(t), served)
	checkEvent(t, f.rec.next(t), servedLab)
	for _, tok := range []string{tokDen, tokLab} {
		if r := f.get(t, "/p/"+tok); r.code != http.StatusNotFound {
			t.Errorf("after a restart: %d, want 404", r.code)
		}
		if e := f.rec.next(t); e.Result != api.FetchNotFound || e.Device == "" {
			t.Errorf("after a restart: %+v", e)
		}
	}
}

// TestSpendFailsCleanly: a rewrite that fails after its temp file exists
// (here the rename, over a directory) serves nothing, spends nothing and
// leaves no temp file behind.
func TestSpendFailsCleanly(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1))
	blocker := filepath.Join(f.state, layout.SpentTokensFile)
	must(t, os.Mkdir(blocker, 0o700))
	must(t, os.WriteFile(filepath.Join(blocker, "x"), nil, 0o600))

	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusServiceUnavailable || strings.Contains(r.body, "den document") {
		t.Errorf("a failed rename: %d %q, want 503 and no document", r.code, r.body)
	}
	if e := f.rec.next(t); e.Result != api.FetchError {
		t.Errorf("event %+v, want an error", e)
	}
	entries, err := os.ReadDir(f.state)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != layout.SpentTokensFile {
		t.Errorf("the state dir holds %v, want only the blocker", entries)
	}
	must(t, os.RemoveAll(blocker))
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusOK {
		t.Errorf("then = %d, want 200: the failed spend spent the token", r.code)
	}
}

func TestSpendPersistedBeforeServing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1))
	must(t, os.Chmod(f.state, 0o500))
	t.Cleanup(func() { _ = os.Chmod(f.state, 0o700) })

	r := f.get(t, "/p/"+tokDen)
	if r.code != http.StatusServiceUnavailable || strings.Contains(r.body, "den document") || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("with a read-only state dir: %d %q %v, want 503 and no document", r.code, r.body, r.header)
	}
	e := f.rec.next(t)
	if e.Result != api.FetchError || e.Error == "" || e.Version != "" || e.Device != "softphone-den" || e.File != denFile {
		t.Errorf("event %+v, want an error with the device and no version", e)
	}
	entries, err := os.ReadDir(f.state)
	must(t, err)
	if len(entries) != 0 {
		t.Errorf("the state dir holds %v", entries)
	}

	// Nothing was spent: once the state dir is writable, the token works.
	must(t, os.Chmod(f.state, 0o700))
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusOK || r.body != "<config>den document v1</config>" {
		t.Errorf("then = %d %q, want the document", r.code, r.body)
	}
}

func TestPrune(t *testing.T) {
	f := newFixture(t)
	both := v1Devices
	writeVersion(t, f.live, v1, v1Files("v1"), both(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))
	f.get(t, "/p/"+tokDen)
	f.rec.next(t)
	hasSpend := func(tok string) bool { return strings.Contains(f.stateFile(t), digest(tok)) }

	// v2 changes something else: den's token is the same, so it stays spent.
	writeVersion(t, f.live, v2, v1Files("v2"), both(denV1, labV1))
	f.grants.set(v2, grant("softphone-den", tokDen, denFile, v2, denV1), grant("softphone-lab", tokLab, labFile, v2, labV1))
	f.s.Prune()
	if !hasSpend(tokDen) {
		t.Fatal("the same token across bundle versions lost its spend")
	}
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusNotFound {
		t.Errorf("the same token in v2 = %d, want 404", r.code)
	}
	f.rec.next(t)

	// v3 rotates den's token: the old spend goes, at the next provisioning
	// request (no explicit prune), and the new token serves once.
	writeVersion(t, f.live, v3, v1Files("v3"), both(denV2, labV1))
	f.grants.set(v3, grant("softphone-den", tokDen2, denFile, v3, denV2), grant("softphone-lab", tokLab, labFile, v3, labV1))
	if r := f.get(t, "/p/"+tokDen2); r.code != http.StatusOK || r.body != "<config>den document v3</config>" {
		t.Errorf("the rotated token = %d %q", r.code, r.body)
	}
	f.rec.next(t)
	if hasSpend(tokDen) || !hasSpend(tokDen2) {
		t.Errorf("after the rotation the state file is %s, want only the new spend", f.stateFile(t))
	}

	// An incomplete grant list (apply adopted v4 but has not resolved its
	// tokens yet) prunes nothing, nor does a version without an index.
	writeVersion(t, f.live, v4, v1Files("v4"), both(denV2, labV1))
	f.grants.set(v4)
	f.s.Prune()
	writeVersion(t, f.live, v5, v1Files("v5"), nil)
	f.grants.set(v5, grant("softphone-lab", tokLab, labFile, v5, labV1))
	f.s.Prune()
	if !hasSpend(tokDen2) {
		t.Error("an incomplete grant list or a missing index pruned a spend")
	}
	// Once v4's grants are complete it prunes, and keeps what is granted.
	f.grants.set(v4, grant("softphone-den", tokDen2, denFile, v4, denV2), grant("softphone-lab", tokLab, labFile, v4, labV1))
	f.s.Prune()
	if !hasSpend(tokDen2) {
		t.Error("a granted token lost its spend")
	}

	// den is removed: its spend goes.
	must(t, os.RemoveAll(filepath.Join(f.live, "v"+v5)))
	writeVersion(t, f.live, v5, v1Files("v5"), map[string]compile.BundleDevice{"softphone-lab": softphone("lab", labV1)})
	f.grants.set(v5, grant("softphone-lab", tokLab, labFile, v5, labV1))
	f.s.Prune()
	if f.stateFile(t) != `{"spends":[]}` {
		t.Errorf("after den was removed: %s", f.stateFile(t))
	}
	// A restart re-records nothing.
	f.start(t)
	f.rec.none(t, f)
}

func TestNoTokenAnywhere(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))

	var answers []string
	for _, target := range []string{
		"/p/" + tokDen, "/p/" + tokDen, "/p/" + tokNone, "/p/" + tokLab + "/x",
		// A token where it does not belong is never recorded either.
		"/" + tokLab, "//p/" + tokLab, "/P/" + tokLab, "/x/../p/" + tokLab, "/p%2F" + tokLab + "/y",
		"/a/" + tokLab + "/b.xml", "/p\\" + tokLab, "/directory.xml?t=" + tokLab, "/x\\" + tokLab,
	} {
		answers = append(answers, f.raw(t, http.MethodGet, target))
	}
	// The error paths: the state cannot be written, the document cannot
	// be read.
	if os.Geteuid() != 0 {
		must(t, os.Chmod(f.state, 0o500))
		answers = append(answers, f.raw(t, http.MethodGet, "/p/"+tokLab))
		must(t, os.Chmod(f.state, 0o700))
		must(t, os.Chmod(filepath.Join(f.live, "v"+v1, labFile), 0o000))
		answers = append(answers, f.raw(t, http.MethodGet, "/p/"+tokLab))
		must(t, os.Chmod(filepath.Join(f.live, "v"+v1, labFile), 0o600))
	}
	f.get(t, "/p/"+tokLab)
	time.Sleep(50 * time.Millisecond)
	before := f.rec.all()
	f.start(t) // re-records the spends

	events, err := json.Marshal(append(before, f.rec.all()...))
	must(t, err)
	for _, tok := range allTokens {
		for what, out := range map[string]string{
			"logs": f.logs.String(), "events": string(events), "state file": f.stateFile(t),
			"answers": strings.Join(answers, "\n"),
		} {
			if strings.Contains(out, tok) {
				t.Errorf("the token %s... is in the %s:\n%s", tok[:24], what, out)
			}
		}
	}
	if !strings.Contains(f.logs.String(), denFile) {
		t.Errorf("the logs do not name the bundle path:\n%s", f.logs.String())
	}
	if !strings.Contains(string(events), `"file":"p/"`) {
		t.Errorf("no misdirected token was recorded as p/: %s", events)
	}
}

func TestNew(t *testing.T) {
	ok := func(t *testing.T) devicehttp.Config {
		root := t.TempDir()
		return devicehttp.Config{
			LiveDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"),
			Grants: &grants{}, Log: logr.Discard(),
		}
	}
	t.Run("creates the state dir 0700", func(t *testing.T) {
		cfg := ok(t)
		if _, err := devicehttp.New(cfg); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(cfg.StateDir)
		must(t, err)
		if !fi.IsDir() || fi.Mode().Perm() != layout.StateDirMode {
			t.Errorf("state dir %v, want a 0700 directory", fi.Mode())
		}
	})
	t.Run("tightens an existing state dir and clears stale temp files", func(t *testing.T) {
		cfg := ok(t)
		must(t, os.Mkdir(cfg.StateDir, 0o755))
		stale := filepath.Join(cfg.StateDir, layout.SpentTokensFile+".tmp-123")
		must(t, os.WriteFile(stale, []byte("x"), 0o600))
		if _, err := devicehttp.New(cfg); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(cfg.StateDir)
		must(t, err)
		if fi.Mode().Perm() != layout.StateDirMode {
			t.Errorf("state dir %v, want 0700", fi.Mode())
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("a stale temp file survived: %v", err)
		}
	})
	t.Run("refuses missing config", func(t *testing.T) {
		cfg := ok(t)
		for name, c := range map[string]devicehttp.Config{
			"no live dir": {StateDir: cfg.StateDir, Grants: cfg.Grants},
			"no state":    {LiveDir: cfg.LiveDir, Grants: cfg.Grants},
			"no grants":   {LiveDir: cfg.LiveDir, StateDir: cfg.StateDir},
		} {
			if _, err := devicehttp.New(c); err == nil {
				t.Errorf("%s: New succeeded", name)
			}
		}
	})
	t.Run("a state dir that is a file", func(t *testing.T) {
		cfg := ok(t)
		must(t, os.WriteFile(cfg.StateDir, nil, 0o600))
		if _, err := devicehttp.New(cfg); err == nil {
			t.Error("New succeeded")
		}
	})

	good := provisioned("softphone-den", denFile, v1, denV1)
	goodJSON, err := json.Marshal(good)
	must(t, err)
	spend := func(digest string, e any) string {
		b, err := json.Marshal(e)
		must(t, err)
		return fmt.Sprintf(`{"tokenSHA256":%q,"event":%s}`, digest, b)
	}
	notServed := good
	notServed.Result = api.FetchNotFound
	noVersion := good
	noVersion.Version = ""
	otherTree := good
	otherTree.File, otherTree.BundlePath = "http/x.xml", "http/x.xml"
	tftp := good
	tftp.Transport = api.TransportTFTP
	for name, content := range map[string]string{
		"not JSON":         `{"spends":[`,
		"empty":            ``,
		"unknown field":    `{"spends":[],"tokens":["x"]}`,
		"trailing data":    `{"spends":[]}{}`,
		"a bad digest":     `{"spends":[` + spend("abc", good) + `]}`,
		"a token, not one": `{"spends":[` + spend(tokDen, good) + `]}`,
		"duplicate":        `{"spends":[` + spend(digest(tokDen), good) + `,` + spend(digest(tokDen), good) + `]}`,
		"invalid event":    `{"spends":[` + spend(digest(tokDen), sensor.FetchEvent{}) + `]}`,
		"not served":       `{"spends":[` + spend(digest(tokDen), notServed) + `]}`,
		"no version":       `{"spends":[` + spend(digest(tokDen), noVersion) + `]}`,
		"not provisioning": `{"spends":[` + spend(digest(tokDen), otherTree) + `]}`,
		"not device HTTP":  `{"spends":[` + spend(digest(tokDen), tftp) + `]}`,
		"unknown event field": `{"spends":[{"tokenSHA256":"` + digest(tokDen) + `","event":` +
			strings.Replace(string(goodJSON), "{", `{"token":"x",`, 1) + `}]}`,
	} {
		t.Run("corrupt: "+name, func(t *testing.T) {
			cfg := ok(t)
			must(t, os.Mkdir(cfg.StateDir, 0o700))
			must(t, os.WriteFile(filepath.Join(cfg.StateDir, layout.SpentTokensFile), []byte(content), 0o600))
			if _, err := devicehttp.New(cfg); err == nil {
				t.Error("New accepted a corrupt state file")
			} else if strings.Contains(err.Error(), tokDen) {
				t.Errorf("the error quotes the file: %v", err)
			}
		})
	}
	t.Run("a valid state file", func(t *testing.T) {
		cfg := ok(t)
		rec := newRecorder(t)
		cfg.Recorder = rec
		must(t, os.Mkdir(cfg.StateDir, 0o700))
		must(t, os.WriteFile(filepath.Join(cfg.StateDir, layout.SpentTokensFile),
			[]byte(`{"spends":[`+spend(digest(tokDen), good)+`]}`), 0o600))
		if _, err := devicehttp.New(cfg); err != nil {
			t.Fatal(err)
		}
		checkEvent(t, rec.next(t), good)
	})
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	f.grants.set(v1, grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1))
	f.s.SetPruneEvery(10 * time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, ln) }()
	url := "http://" + ln.Addr().String()
	resp, err := http.Get(url + "/p/" + tokDen)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("served on the listener: %d", resp.StatusCode)
	}

	// The prune also runs on its own, with no provisioning request.
	writeVersion(t, f.live, v2, v1Files("v2"), v1Devices(denV2, labV1))
	f.grants.set(v2, grant("softphone-den", tokDen2, denFile, v2, denV2), grant("softphone-lab", tokLab, labFile, v2, labV1))
	deadline := time.Now().Add(5 * time.Second)
	for strings.Contains(f.stateFile(t), digest(tokDen)) {
		if time.Now().After(deadline) {
			t.Fatal("the periodic prune never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}

	// Run listens on the address it is given.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel2() }()
	if err := f.s.Run(ctx2, "127.0.0.1:0"); err != nil {
		t.Errorf("Run = %v", err)
	}
	if err := f.s.Run(context.Background(), "192.0.2.300:8088"); err == nil {
		t.Error("Run on a bad address succeeded")
	}
}

func TestServerLimits(t *testing.T) {
	f := newFixture(t)
	srv := f.s.HTTPServer()
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 10*time.Second ||
		srv.WriteTimeout != 30*time.Second || srv.MaxHeaderBytes != 8<<10 || srv.Handler != f.s {
		t.Errorf("server limits: %+v", srv)
	}
}

// TestMalformedGrant: a grant apply should never produce is refused
// without serving, and without naming anything it holds.
func TestMalformedGrant(t *testing.T) {
	f := newFixture(t)
	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	for _, g := range []provision.Grant{
		grant("softphone-den", tokDen, "asterisk/pjsip.conf", v1, denV1),
		grant("softphone-den", tokDen, "provisioning/../asterisk/pjsip.conf", v1, denV1),
		grant("softphone-den", tokDen, denFile, "../v1", denV1),
		grant("softphone-den", tokDen, denFile, v1, "not-a-version"),
		grant("Softphone_Den", tokDen, denFile, v1, denV1),
	} {
		f.grants.set(v1, g)
		r := f.get(t, "/p/"+tokDen)
		if r.code == http.StatusOK || strings.Contains(r.body, "secret") || strings.Contains(r.body, "document") {
			t.Errorf("grant %v: %d %q", g, r.code, r.body)
		}
		if e := f.rec.next(t); e.Result == api.FetchServed {
			t.Errorf("grant %v: recorded %+v", g, e)
		}
	}
	if s := f.stateFile(t); s != "" {
		t.Errorf("a malformed grant spent its token: %s", s)
	}
}

// TestMatch: the first grant whose token is the request's, never a grant
// without a token, and -1 for no match.
func TestMatch(t *testing.T) {
	gs := []provision.Grant{{Device: "a", Token: ""}, {Device: "b", Token: tokDen}, {Device: "c", Token: tokLab}, {Device: "d", Token: tokDen}}
	for _, tc := range []struct {
		token string
		want  int
	}{{tokDen, 1}, {tokLab, 2}, {tokNone, -1}, {"", -1}, {tokDen[:10], -1}, {tokDen + "x", -1}} {
		if got := devicehttp.Match(tc.token, gs); got != tc.want {
			t.Errorf("Match(%.12q...) = %d, want %d", tc.token, got, tc.want)
		}
	}
	if got := devicehttp.Match(tokDen, nil); got != -1 {
		t.Errorf("Match with no grants = %d", got)
	}
}

// TestPrunesOnlyAgainstSettledGrants: spends are pruned against the
// settled grants, never the serving ones: a version swapped in unverified
// can be rolled back, and its predecessor's tokens granted again, so their
// spends must still be there.
func TestPrunesOnlyAgainstSettledGrants(t *testing.T) {
	f := newFixture(t)
	settled := &grants{}
	f.settled = settled
	f.start(t)
	hasSpend := func(tok string) bool { return strings.Contains(f.stateFile(t), digest(tok)) }

	writeVersion(t, f.live, v1, v1Files("v1"), v1Devices(denV1, labV1))
	setCurrent(t, f.live, v1)
	g1 := []provision.Grant{grant("softphone-den", tokDen, denFile, v1, denV1), grant("softphone-lab", tokLab, labFile, v1, labV1)}
	f.grants.set(v1, g1...)
	settled.set(v1, g1...)
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusOK {
		t.Fatalf("first fetch = %d", r.code)
	}
	f.rec.next(t)

	// v2, unverified, rotates den's token and serves; it is not settled.
	writeVersion(t, f.live, v2, v1Files("v2"), v1Devices(denV2, labV1))
	setCurrent(t, f.live, v2)
	f.grants.set(v2, grant("softphone-den", tokDen2, denFile, v2, denV2), grant("softphone-lab", tokLab, labFile, v2, labV1))
	settled.set("")
	f.get(t, "/p/"+tokNone) // a provisioning request prunes
	f.rec.next(t)
	f.s.Prune()
	if !hasSpend(tokDen) {
		t.Fatal("a spend was pruned against grants that are not settled")
	}

	// v2 fails verification: the rollback grants v1's tokens again, and
	// den's is still spent.
	setCurrent(t, f.live, v1)
	f.grants.set(v1, g1...)
	if r := f.get(t, "/p/"+tokDen); r.code != http.StatusNotFound {
		t.Errorf("den's spent token after the rollback = %d, want 404", r.code)
	}
	f.rec.next(t)

	// v3 rotates it and settles: the old spend goes.
	writeVersion(t, f.live, v3, v1Files("v3"), v1Devices(denV2, labV1))
	setCurrent(t, f.live, v3)
	g3 := []provision.Grant{grant("softphone-den", tokDen2, denFile, v3, denV2), grant("softphone-lab", tokLab, labFile, v3, labV1)}
	f.grants.set(v3, g3...)
	settled.set(v3, g3...)
	f.s.Prune()
	if hasSpend(tokDen) {
		t.Errorf("the settled grants did not prune: %s", f.stateFile(t))
	}

	// Without a settled source nothing is ever pruned.
	f.settled = nil
	f.start(t)
	if r := f.get(t, "/p/"+tokDen2); r.code != http.StatusOK {
		t.Fatalf("den's new token = %d", r.code)
	}
	f.rec.next(t)
	writeVersion(t, f.live, v4, v1Files("v4"), v1Devices(denV1, labV1))
	f.grants.set(v4, grant("softphone-den", tokDen, denFile, v4, denV1), grant("softphone-lab", tokLab, labFile, v4, labV1))
	f.s.Prune()
	if !hasSpend(tokDen2) {
		t.Error("a server without a settled source pruned")
	}
}
