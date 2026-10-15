package fileserver

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pin/tftp/v3"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

const sepName = "SEPD0C78915E8C8.cnf.xml"

// TestTFTPServesTheLiveVersion: a phone gets the current version's file,
// and the event says which version, which bundle path, from whom and when.
func TestTFTPServesTheLiveVersion(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())

	// Before the first apply: nothing, and no version to report.
	if _, err := tftpGet(t, e.tftp, sepName, 0); err == nil {
		t.Error("a fetch before any version was served")
	}
	if ev := e.rec.next(); ev != (sensor.FetchEvent{Transport: api.TransportTFTP, File: sepName, Result: api.FetchNotFound, At: pinned, Source: "127.0.0.1"}) {
		t.Errorf("before any version: %+v", ev)
	}

	sepA, sepB := generated("A", 1500), generated("B", 700)
	listA := generated("list A", 100)
	e.live.version(vA, map[string][]byte{sepName: sepA, "Desktops/320x212x12/List.xml": listA})
	e.live.version(vB, map[string][]byte{sepName: sepB})
	e.live.swap(vA)

	served := func(name, file, version string, want []byte) {
		t.Helper()
		got, err := tftpGet(t, e.tftp, name, 0)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v, %d bytes; want the %d bytes of %s", name, err, len(got), len(want), version[:1])
		}
		ev := e.rec.next()
		wantEv := sensor.FetchEvent{
			Transport: api.TransportTFTP, File: file, Result: api.FetchServed, At: pinned, Source: "127.0.0.1",
			BundleVersion: version, BundlePath: "tftp/" + file,
		}
		if ev != wantEv {
			t.Errorf("%s: event\n %+v\nwant\n %+v", name, ev, wantEv)
		}
	}
	served(sepName, sepName, vA, sepA)
	served("/"+sepName, sepName, vA, sepA) // one leading slash is stripped
	served("Desktops/320x212x12/List.xml", "Desktops/320x212x12/List.xml", vA, listA)

	e.live.swap(vB)
	served(sepName, sepName, vB, sepB)
	if _, err := tftpGet(t, e.tftp, "Desktops/320x212x12/List.xml", 0); err == nil {
		t.Error("a file of the old version was served")
	}
	if ev := e.rec.next(); ev.Result != api.FetchNotFound || ev.BundleVersion != vB || ev.BundlePath != "tftp/Desktops/320x212x12/List.xml" {
		t.Errorf("missing in the live version: %+v", ev)
	}

	// Not files: a directory, the version's index, another tree.
	e.live.swap(vA)
	for _, name := range []string{"Desktops", "Desktops/320x212x12", "bundle.json", "pjsip.conf", "directory.xml"} {
		if _, err := tftpGet(t, e.tftp, name, 0); err == nil {
			t.Errorf("%s was served", name)
		}
		if ev := e.rec.next(); ev.Result != api.FetchNotFound {
			t.Errorf("%s: %+v, want notFound", name, ev)
		}
	}
}

// TestHTTP6970Parity: HTTP :6970 answers every name with the bytes TFTP
// sends and reports the same event, but for the transport.
func TestHTTP6970Parity(t *testing.T) {
	store := newFakeStore(t, map[string][]byte{"apps.sbn": generated("firmware", 3000)})
	e := startEnv(t, store, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 1500), "Desktops/320x212x12/List.xml": generated("list", 80)})
	e.live.swap(vA)

	for _, name := range []string{
		sepName, "Desktops/320x212x12/List.xml", "apps.sbn", "missing.xml",
		`a\b`, "../asterisk/pjsip.conf", "x/", "", strings.Repeat("y", 300),
	} {
		tftpBytes, tftpErr := tftpGet(t, e.tftp, name, 0)
		tftpEv := e.rec.next()
		resp, httpBytes := httpGet(t, e.http, "/"+name)
		httpEv := e.rec.next()

		if (tftpErr == nil) != (resp.StatusCode == http.StatusOK) || (tftpErr == nil && !bytes.Equal(tftpBytes, httpBytes)) {
			t.Errorf("%q: TFTP %v (%d bytes), HTTP %d (%d bytes)", name, tftpErr, len(tftpBytes), resp.StatusCode, len(httpBytes))
		}
		if tftpEv.Transport != api.TransportTFTP || httpEv.Transport != api.TransportHTTP6970 {
			t.Errorf("%q: transports %s, %s", name, tftpEv.Transport, httpEv.Transport)
		}
		tftpEv.Transport, httpEv.Transport = "", ""
		if tftpEv != httpEv {
			t.Errorf("%q: events differ:\n TFTP %+v\n HTTP %+v", name, tftpEv, httpEv)
		}
		switch {
		case resp.StatusCode == http.StatusOK && strings.HasSuffix(name, ".xml"):
			if ct := resp.Header.Get("Content-Type"); ct != "text/xml; charset=utf-8" {
				t.Errorf("%q: Content-Type %q", name, ct)
			}
		case resp.StatusCode == http.StatusOK:
			if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("%q: Content-Type %q", name, ct)
			}
		case resp.StatusCode == http.StatusNotFound:
			if string(httpBytes) != notFoundBody || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("%q: 404 body %q, %q", name, httpBytes, resp.Header.Get("Content-Type"))
			}
		default:
			t.Errorf("%q: HTTP %d", name, resp.StatusCode)
		}
	}

	// HEAD is answered like GET, and not reported.
	resp, body := httpRaw(t, e.http, http.MethodHead, "/"+sepName)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.ContentLength != 1500 {
		t.Errorf("HEAD: %d, %d bytes, Content-Length %d", resp.StatusCode, len(body), resp.ContentLength)
	}
	resp, _ = httpRaw(t, e.http, http.MethodHead, "/missing.xml")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of a missing file: %d", resp.StatusCode)
	}
	// Any other method is 405, and not reported.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, "PROPFIND"} {
		resp, _ := httpRaw(t, e.http, m, "/"+sepName, "Content-Length: 0")
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q", m, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	e.rec.quiet(100 * time.Millisecond)

	// Range and conditional requests get the whole file.
	resp, body = httpRaw(t, e.http, http.MethodGet, "/"+sepName, "Range: bytes=0-9", "If-Modified-Since: Mon, 02 Jan 2040 15:04:05 GMT")
	if resp.StatusCode != http.StatusOK || len(body) != 1500 || resp.Header.Get("Last-Modified") != "" {
		t.Errorf("a ranged, conditional GET: %d, %d bytes, Last-Modified %q", resp.StatusCode, len(body), resp.Header.Get("Last-Modified"))
	}
	if ev := e.rec.next(); ev.Result != api.FetchServed {
		t.Errorf("a ranged GET: %+v", ev)
	}
}

// TestSwapMidTransfer: a transfer reads the version current when it
// started, to its end, even if apply swaps and prunes that version
// meanwhile, and reports that version.
func TestSwapMidTransfer(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	bigA, bigB := generated("big A", 4<<20), generated("big B", 4<<20)
	e.live.version(vA, map[string][]byte{"big.bin": bigA})
	e.live.version(vB, map[string][]byte{"big.bin": bigB})
	e.live.swap(vA)
	swapAndPrune := func() {
		e.live.swap(vB)
		if err := os.RemoveAll(filepath.Join(e.live.dir, "v"+vA)); err != nil {
			t.Fatal(err)
		}
	}

	// TFTP: swap after the first block.
	got := rawFetch(t, e.tftp, "big.bin", swapAndPrune)
	if !bytes.Equal(got, bigA) {
		t.Errorf("TFTP: %d bytes, not version A's", len(got))
	}
	if ev := e.rec.next(); ev.Result != api.FetchServed || ev.BundleVersion != vA {
		t.Errorf("TFTP: %+v, want served from version A", ev)
	}

	// HTTP: swap after the first 64 KiB.
	e.live.version(vA, map[string][]byte{"big.bin": bigA})
	e.live.swap(vA)
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", e.http)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET /big.bin HTTP/1.1\r\nHost: pbx\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	swapAndPrune()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(first, rest...), bigA) {
		t.Error("HTTP: not version A's bytes")
	}
	if ev := e.rec.next(); ev.Result != api.FetchServed || ev.BundleVersion != vA {
		t.Errorf("HTTP: %+v, want served from version A", ev)
	}

	// The next request gets the new version.
	if got, err := tftpGet(t, e.tftp, "big.bin", 1428); err != nil || !bytes.Equal(got, bigB) {
		t.Errorf("after the swap: %v", err)
	}
	if ev := e.rec.next(); ev.BundleVersion != vB {
		t.Errorf("after the swap: %+v", ev)
	}
}

// TestFirmwareFirst: the firmware store answers first; the tftp/ tree only
// when the store says ErrNotFound; a store failure is an error, never a
// fall back to the tree.
func TestFirmwareFirst(t *testing.T) {
	fw := generated("firmware copy", 2000)
	store := newFakeStore(t, map[string][]byte{sepName: fw, "apps.sbn": generated("apps", 600)})
	// As Disk.Open fails: its errors wrap a PathError, whose path the event
	// must not carry.
	store.errs["broken.sbn"] = fmt.Errorf("firmware: %w", &fs.PathError{Op: "openat", Path: "/var/lib/patchbay/persistent/firmware/L/broken.sbn", Err: syscall.EIO})
	e := startEnv(t, store, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("bundle copy", 900), "broken.sbn": generated("tree", 10), "only-tree.xml": generated("tree only", 10)})
	e.live.swap(vA)

	got, err := tftpGet(t, e.tftp, "/"+sepName, 0)
	if err != nil || !bytes.Equal(got, fw) {
		t.Errorf("a name in both: %v; want the firmware store's bytes", err)
	}
	if ev := e.rec.next(); ev != (sensor.FetchEvent{Transport: api.TransportTFTP, File: sepName, Result: api.FetchServed, At: pinned, Source: "127.0.0.1"}) {
		t.Errorf("firmware: %+v, want no bundle version or path", ev)
	}

	if _, err := tftpGet(t, e.tftp, "broken.sbn", 0); err == nil {
		t.Error("a store failure fell back to the tree")
	}
	ev := e.rec.next()
	if ev.Result != api.FetchError || ev.BundleVersion != "" || ev.Error != "firmware store: input/output error" {
		t.Errorf("a store failure: %+v", ev)
	}

	if got, err := tftpGet(t, e.tftp, "only-tree.xml", 0); err != nil || string(got) != string(generated("tree only", 10)) {
		t.Errorf("a name only in the tree: %v", err)
	}
	if ev := e.rec.next(); ev.BundleVersion != vA {
		t.Errorf("tree: %+v", ev)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if want := []string{sepName, "broken.sbn", "only-tree.xml"}; strings.Join(store.opens, ",") != strings.Join(want, ",") {
		t.Errorf("the store was asked for %q, want %q (each name as requested, without its slash)", store.opens, want)
	}
}

// TestNoPathEscapes: nothing a device asks for reaches a file outside the
// live tftp/ tree, by "..", by an absolute path, by encoding, by case or
// through a symlink, even one that stays inside the tree.
func TestNoPathEscapes(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 100)})
	e.live.swap(vA)
	pjsip := filepath.Join(e.live.dir, "v"+vA, "asterisk", "pjsip.conf")
	for link, target := range map[string]string{
		"link.xml":   "../asterisk/pjsip.conf",
		"abs.xml":    pjsip,
		"inlink.xml": sepName, // inside the tree, still not a regular file
		"sub":        "../asterisk",
		"cur":        "../../current",
	} {
		if err := os.Symlink(target, e.live.tftpPath(vA, link)); err != nil {
			t.Fatal(err)
		}
	}

	names := []string{
		"../asterisk/pjsip.conf", "/../asterisk/pjsip.conf", "tftp/../asterisk/pjsip.conf",
		"asterisk/pjsip.conf", "../provisioning/softphone-den.xml", "../bundle.json",
		"../../current/asterisk/pjsip.conf", "../v" + vA + "/asterisk/pjsip.conf",
		pjsip, "/" + pjsip, "//etc/passwd", "/etc/passwd", `..\asterisk\pjsip.conf`,
		"link.xml", "abs.xml", "inlink.xml", "sub/pjsip.conf", "cur/asterisk/pjsip.conf",
		"SEPd0c78915e8c8.cnf.xml", "SEPD0C78915E8C8.CNF.XML", "./" + sepName, sepName + "/.",
	}
	for _, name := range names {
		got, err := tftpGet(t, e.tftp, name, 0)
		if err == nil || bytes.Contains(got, []byte(secret)) {
			t.Errorf("TFTP %q was served (%d bytes)", name, len(got))
		}
		if ev := e.rec.next(); ev.Result != api.FetchNotFound {
			t.Errorf("TFTP %q: %+v, want notFound", name, ev)
		}
	}
	targets := []string{
		"/%2e%2e/asterisk/pjsip.conf", "/%2E%2E%2Fasterisk%2Fpjsip.conf", "/tftp/..%2fasterisk/pjsip.conf",
		"/sub%2fpjsip.conf", "/..%5casterisk%5cpjsip.conf", "/./" + sepName, "/link.xml", "/inlink.xml",
		"http://pbx/../asterisk/pjsip.conf",
	}
	for _, name := range names {
		targets = append(targets, "/"+strings.TrimPrefix(name, "/"))
	}
	for _, target := range targets {
		resp, body := httpGet(t, e.http, target)
		if resp.StatusCode != http.StatusNotFound || bytes.Contains(body, []byte(secret)) {
			t.Errorf("HTTP %q: %d (%d bytes), want 404", target, resp.StatusCode, len(body))
		}
		if ev := e.rec.next(); ev.Result != api.FetchNotFound {
			t.Errorf("HTTP %q: %+v, want notFound", target, ev)
		}
	}
}

// TestOneEventPerRequest: served, notFound and a failed transfer are each
// reported exactly once; a HEAD, a 405 and a TFTP write request not at all.
func TestOneEventPerRequest(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 1500), "big.bin": generated("big", 8<<20)})
	e.live.swap(vA)

	if _, err := tftpGet(t, e.tftp, sepName, 0); err != nil {
		t.Fatal(err)
	}
	_, _ = tftpGet(t, e.tftp, "missing", 0)
	httpGet(t, e.http, "/"+sepName)
	httpGet(t, e.http, "/missing")
	httpRaw(t, e.http, http.MethodHead, "/"+sepName)
	httpRaw(t, e.http, http.MethodPost, "/"+sepName, "Content-Length: 0")
	evs := e.rec.wait(4)
	results := map[string]int{}
	for _, ev := range evs {
		results[string(ev.Transport)+" "+string(ev.Result)]++
	}
	for _, k := range []string{"tftp served", "tftp notFound", "http6970 served", "http6970 notFound"} {
		if results[k] != 1 {
			t.Errorf("%s: %d events, want 1 (all: %v)", k, results[k], results)
		}
	}
	e.rec.take()

	// A TFTP write request: refused, not reported.
	c := udpClient(t)
	wrq := rrq(sepName)
	wrq[1] = opWRQ
	send(t, c, e.tftp, wrq)
	if p, _ := recv(t, c, 2*time.Second); opcode(p) != opERROR {
		t.Errorf("WRQ answered %v, want an error packet", p)
	}

	// A client that stops acknowledging: one error event, once the server
	// gives up.
	send(t, c, e.tftp, rrq(sepName))
	if p, _ := recv(t, c, 2*time.Second); opcode(p) != opDATA {
		t.Fatalf("got %v, want block 1", p)
	}
	ev := e.rec.next()
	if ev.Result != api.FetchError || ev.Error == "" || ev.File != sepName || ev.BundleVersion != vA {
		t.Errorf("a client that stopped acking: %+v", ev)
	}

	// An HTTP client that goes away mid-body: one error event.
	hc, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", e.http)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(hc, "GET /big.bin HTTP/1.1\r\nHost: pbx\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(hc, make([]byte, 1<<10)); err != nil {
		t.Fatal(err)
	}
	if tc, ok := hc.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // reset, not a polite close
	}
	hc.Close()
	if ev := e.rec.next(); ev.Result != api.FetchError || ev.Transport != api.TransportHTTP6970 {
		t.Errorf("an HTTP client that went away: %+v", ev)
	}
	e.rec.quiet(800 * time.Millisecond)
}

// TestInvalidNameIsReportedQuoted: a name that can only be garbage is
// never looked up, and is reported Go-quoted (cut to 200 bytes), so the
// event is valid and shows exactly what was asked.
func TestInvalidNameIsReportedQuoted(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 100)})
	e.live.swap(vA)
	long := strings.Repeat("x", 300)
	for _, tc := range []struct{ name, file string }{
		{`a\b`, `"a\\b"`},
		{"\x01x", `"\x01x"`},
		{"x\x7f", `"x\x7f"`},
		{"\xff.xml", `"\xff.xml"`},
		{"a//b", `"a//b"`},
		{"./x", `"./x"`},
		{"x/..", `"x/.."`},
		{"/", `""`},
		{"//x", `"/x"`},
		{"caf\u00e9/\u202e.xml", `"caf\u00e9/\u202e.xml"`}, // a bidi override is no control character
		{long, (`"` + long)[:200]},
		{strings.Repeat("z", 255), strings.Repeat("z", 255)}, // the longest name looked up
		{strings.Repeat("z", 256), (`"` + strings.Repeat("z", 256))[:200]},
	} {
		if _, err := tftpGet(t, e.tftp, tc.name, 0); err == nil {
			t.Errorf("%q was served", tc.name)
		}
		ev := e.rec.next()
		quoted := strings.HasPrefix(tc.file, `"`)
		if ev.File != tc.file || ev.Result != api.FetchNotFound || (ev.BundleVersion == "") != quoted {
			t.Errorf("%q: event %+v, want File %s, notFound", tc.name, ev, tc.file)
		}
		if err := ev.Validate(); err != nil {
			t.Errorf("%q: %v", tc.name, err)
		}
	}
	// HTTP: what TFTP cannot carry (a NUL ends a TFTP name).
	for target, file := range map[string]string{"/%00": `"\x00"`, "/a%00b.xml": `"a\x00b.xml"`, "/%0d%0a": `"\r\n"`} {
		resp, _ := httpGet(t, e.http, target)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", target, resp.StatusCode)
		}
		if ev := e.rec.next(); ev.File != file || ev.Result != api.FetchNotFound {
			t.Errorf("%s: %+v, want File %s", target, ev, file)
		}
	}
}

// TestReplySource: a server on every address answers from the address the
// request came to, not one the routing table picks.
func TestReplySource(t *testing.T) {
	r := newRecorder(t)
	l := newLive(t)
	l.version(vA, map[string][]byte{sepName: generated("sep", 100)})
	l.swap(vA)
	server, _ := serveOn(t, "0.0.0.0", Config{LiveDir: l.dir, Firmware: newFakeStore(t, nil), Recorder: r, Now: func() time.Time { return pinned }}, fastTFTP())
	for _, to := range []string{"127.0.0.2", "127.0.0.1", "127.0.0.3"} {
		c := udpClient(t)
		send(t, c, &net.UDPAddr{IP: net.ParseIP(to), Port: server.Port}, rrq(sepName))
		p, from := recv(t, c, 2*time.Second)
		if opcode(p) != opDATA {
			t.Fatalf("to %s: got %v", to, p)
		}
		if from.IP.String() != to {
			t.Errorf("a request to %s was answered from %s", to, from.IP)
		}
		send(t, c, from, ackPacket(1))
		if ev := r.next(); ev.Source != "127.0.0.1" || ev.Result != api.FetchServed {
			t.Errorf("to %s: %+v", to, ev)
		}
	}
}

// TestTFTPRobustness: malformed and hostile packets, many transfers at
// once and the block size and transfer size options, without a crash.
func TestTFTPRobustness(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	file := generated("robust", 20000)
	e.live.version(vA, map[string][]byte{"f.bin": file})
	e.live.swap(vA)

	c := udpClient(t)
	for _, p := range [][]byte{
		{}, {0}, {0, opRRQ}, {0, opRRQ, 'x'}, {0, opRRQ, 'x', 'y', 'z'}, {0, 9, 'j', 'u', 'n', 'k'},
		ackPacket(1), {0, opDATA, 0, 1, 'x'}, {0, opERROR, 0, 1, 'x', 0}, {0, opOACK, 'b', 0, '1', 0},
		append([]byte{0, opRRQ}, bytes.Repeat([]byte{'x'}, 2000)...), // over a datagram, no NUL
		bytes.Repeat([]byte{0xff}, 70000/2),
	} {
		send(t, c, e.tftp, p)
	}
	// Requests that parse, with hostile options: each served once.
	for _, p := range [][]byte{
		rrq("f.bin", "blksize", "99999999999999999999"),
		rrq("f.bin", "blksize", "abc", "tsize", strings.Repeat("9", 300)),
		rrq("f.bin", "blksize", "0000000000000000000000000000000000000000512"),
		rrq("f.bin", "tsize"),
		rrq("f.bin", "windowsize", "65535", "timeout", "0"),
	} {
		got := rawFetchPacket(t, e.tftp, p)
		if !bytes.Equal(got, file) {
			t.Errorf("%q: %d bytes, want the file", p[:min(len(p), 40)], len(got))
		}
		if ev := e.rec.next(); ev.Result != api.FetchServed {
			t.Errorf("%q: %+v", p[:min(len(p), 40)], ev)
		}
	}
	e.rec.quiet(100 * time.Millisecond)

	// Block sizes, and the transfer size a phone may ask for.
	for _, blksize := range []int{512, 1428, 8192, 65464} {
		if got, err := tftpGet(t, e.tftp, "f.bin", blksize); err != nil || !bytes.Equal(got, file) {
			t.Errorf("blksize %d: %v", blksize, err)
		}
		e.rec.next()
	}
	tc, err := tftp.NewClient(e.tftp.String())
	if err != nil {
		t.Fatal(err)
	}
	tc.RequestTSize(true)
	wt, err := tc.Receive("f.bin", "octet")
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := wt.(tftp.IncomingTransfer).Size(); !ok || n != int64(len(file)) {
		t.Errorf("tsize = %d %v, want %d", n, ok, len(file))
	}
	if _, err := wt.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
	e.rec.next()

	// Many at once.
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			got, err := tftpGet(t, e.tftp, "f.bin", []int{0, 1428}[i%2])
			if err == nil && !bytes.Equal(got, file) {
				err = fmt.Errorf("transfer %d: wrong bytes", i)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for _, ev := range e.rec.wait(n) {
		if ev.Result != api.FetchServed {
			t.Errorf("concurrent: %+v", ev)
		}
	}
	e.rec.take()

	// Still serving.
	if got, err := tftpGet(t, e.tftp, "f.bin", 0); err != nil || !bytes.Equal(got, file) {
		t.Errorf("after all that: %v", err)
	}
}

// rawFetchPacket sends a hand-made request and reads the file to its end,
// acknowledging an OACK with block 0, whatever block size was agreed.
func rawFetchPacket(t *testing.T, server *net.UDPAddr, req []byte) []byte {
	t.Helper()
	c := udpClient(t)
	send(t, c, server, req)
	blksize := 512
	var out []byte
	var want uint16 = 1
	for {
		p, from := recv(t, c, 5*time.Second)
		switch opcode(p) {
		case opOACK:
			opts := bytes.Split(p[2:], []byte{0})
			for i := 0; i+1 < len(opts); i += 2 {
				if string(opts[i]) == "blksize" {
					if _, err := fmt.Sscan(string(opts[i+1]), &blksize); err != nil {
						t.Fatalf("OACK blksize %q", opts[i+1])
					}
				}
			}
			send(t, c, from, ackPacket(0))
		case opDATA:
			if got := uint16(p[2])<<8 | uint16(p[3]); got != want {
				t.Fatalf("block %d, want %d", got, want)
			}
			out = append(out, p[4:]...)
			send(t, c, from, ackPacket(want))
			if len(p)-4 < blksize {
				return out
			}
			want++
		default:
			t.Fatalf("got %v", p)
		}
	}
}

// TestTFTPBusy: past the cap on transfers in flight, a request is refused
// at once (and reported), and served again once a slot is free.
func TestTFTPBusy(t *testing.T) {
	o := fastTFTP()
	o.transfers = 2
	e := startEnv(t, nil, o)
	e.live.version(vA, map[string][]byte{"f.bin": generated("busy", 5000)})
	e.live.swap(vA)

	// Two clients that never acknowledge hold both slots.
	for range 2 {
		c := udpClient(t)
		send(t, c, e.tftp, rrq("f.bin"))
		if p, _ := recv(t, c, 2*time.Second); opcode(p) != opDATA {
			t.Fatalf("got %v", p)
		}
	}
	c := udpClient(t)
	send(t, c, e.tftp, rrq("f.bin"))
	p, _ := recv(t, c, 2*time.Second)
	if opcode(p) != opERROR || !bytes.Contains(p, []byte("busy")) {
		t.Errorf("the third request got %q, want a busy error", p)
	}
	if ev := e.rec.next(); ev.Result != api.FetchError || !strings.Contains(ev.Error, "busy") || ev.File != "f.bin" {
		t.Errorf("busy: %+v", ev)
	}
	// The two held transfers fail; then a fetch is served.
	e.rec.wait(2)
	e.rec.take()
	if _, err := tftpGet(t, e.tftp, "f.bin", 0); err != nil {
		t.Errorf("after the slots freed: %v", err)
	}
}

// TestUnreadableFileIsAnError: a tftp/ file the server may not read is the
// server's failure, not the phone's: an error, never served.
func TestUnreadableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 100)})
	e.live.swap(vA)
	if err := os.Chmod(e.live.tftpPath(vA, sepName), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := tftpGet(t, e.tftp, sepName, 0); err == nil {
		t.Error("an unreadable file was served")
	}
	ev := e.rec.next()
	if ev.Result != api.FetchError || !strings.Contains(ev.Error, "permission denied") || strings.Contains(ev.Error, e.live.dir) {
		t.Errorf("unreadable: %+v", ev)
	}
}

// TestClientErrorTextIsSanitized: a client that aborts a transfer chooses
// its error packet's text; what reaches the event is printable ASCII.
func TestClientErrorTextIsSanitized(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{"f.bin": generated("f", 2000)})
	e.live.swap(vA)
	c := udpClient(t)
	send(t, c, e.tftp, rrq("f.bin"))
	p, from := recv(t, c, 2*time.Second)
	if opcode(p) != opDATA {
		t.Fatalf("got %v", p)
	}
	send(t, c, from, append([]byte{0, opERROR, 0, 0}, "go away\x1b[31m\r\n\xff\x00"...))
	ev := e.rec.next() // the recorder fails the test on an invalid event
	if ev.Result != api.FetchError || !strings.Contains(ev.Error, "go away?[31m???") {
		t.Errorf("an aborted transfer: %+v", ev)
	}
}

// TestFIFOInTheTreeNeverBlocks: a FIFO where a file should be is refused
// without being opened (an open would wait for a writer, holding the
// transfer and its slot forever).
func TestFIFOInTheTreeNeverBlocks(t *testing.T) {
	e := startEnv(t, nil, fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 10)})
	e.live.swap(vA)
	fifo := e.live.tftpPath(vA, "fifo.xml")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // unblock a wrongly blocked opener, so serve can stop
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	if _, err := tftpGet(t, e.tftp, "fifo.xml", 0); err == nil {
		t.Error("a FIFO was served")
	}
	if ev := e.rec.next(); ev.Result != api.FetchNotFound {
		t.Errorf("a FIFO: %+v", ev)
	}
}
