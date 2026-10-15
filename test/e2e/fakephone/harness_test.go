package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pin/tftp/v3"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The test phone's MAC: the desk golden's SEP file names it.
const (
	deskMAC      = "D0C78915E8C8"
	deskNumber   = "100"        // the desk golden's line
	deskAuthUser = "phone-desk" // and its authName
)

// runHarness runs one script in-process on loopback and reads it through
// its own HTTP server, as the e2e harness does through pods/proxy.
type runHarness struct {
	t    *testing.T
	r    *runner
	web  string // http://127.0.0.1:port
	out  *lockedBuffer
	log  *lockedBuffer
	errc chan error
	stop context.CancelFunc

	doneOnce sync.Once
	err      error
}

// startRun starts sc (on 127.0.0.1, a free SIP port and web port unless
// set) and returns once it listens.
func startRun(t *testing.T, sc script.Script, opts runOptions) *runHarness {
	t.Helper()
	if sc.WebListen == "" {
		sc.WebListen = "127.0.0.1:0"
	}
	if opts.LocalIP == "" {
		opts.LocalIP = "127.0.0.1"
	}
	if opts.SIPPort == 0 {
		opts.SIPPort = freePort(t)
	}
	h := &runHarness{t: t, out: &lockedBuffer{}, log: &lockedBuffer{}, errc: make(chan error, 1)}
	opts.Out = h.out
	opts.Log = slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	started := make(chan *runner, 1)
	opts.Started = func(r *runner) { started <- r }
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() { h.errc <- runScript(ctx, sc, opts) }()
	// Started runs before the first step, so when the run has already ended
	// (a step that fails at once) started holds the runner too: take it,
	// and keep the error for wait.
	select {
	case h.r = <-started:
	case err := <-h.errc:
		select {
		case h.r = <-started:
			h.errc <- err
		default:
			cancel()
			t.Fatalf("run did not start: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("run did not start within 5s")
	}
	h.web = "http://" + h.r.webAddr.String()
	t.Cleanup(func() {
		cancel()
		if err := h.wait(5 * time.Second); err != nil && !errors.Is(err, errStillRunning) {
			t.Logf("run ended with: %v", err)
		}
	})
	return h
}

var errStillRunning = errors.New("still running")

// wait waits up to d for the run to end and returns its error.
func (h *runHarness) wait(d time.Duration) error {
	select {
	case err := <-h.errc:
		h.doneOnce.Do(func() { h.err = err })
		h.errc <- err // keep it for the next caller
		return err
	case <-time.After(d):
		return errStillRunning
	}
}

// get GETs path on the phone's web server.
func (h *runHarness) get(path string) (int, []byte) {
	h.t.Helper()
	res, err := http.Get(h.web + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.StatusCode, b
}

// state is GET /fakephone/state.
func (h *runHarness) state() script.State {
	h.t.Helper()
	code, b := h.get(script.PathState)
	if code != http.StatusOK {
		h.t.Fatalf("state: %d %s", code, b)
	}
	var s script.State
	if err := json.Unmarshal(b, &s); err != nil {
		h.t.Fatalf("state: %v\n%s", err, b)
	}
	return s
}

// signal is POST /fakephone/signal/<name>; it returns the status.
func (h *runHarness) signal(name string) int {
	h.t.Helper()
	res, err := http.Post(h.web+script.SignalPath(name), "text/plain", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// waitState polls the State until cond holds, failing after d.
func (h *runHarness) waitState(d time.Duration, what string, cond func(script.State) bool) script.State {
	h.t.Helper()
	var s script.State
	waitFor(h.t, d, what, func() bool {
		s = h.state()
		return cond(s)
	})
	return s
}

// atStep waits until the script runs the step labelled label (or is past
// every step, for "").
func (h *runHarness) atStep(label string) script.State {
	h.t.Helper()
	return h.waitState(10*time.Second, "step "+label, func(s script.State) bool {
		if label == "" {
			return s.Done
		}
		return s.Label == label
	})
}

// tftpServer serves files from memory on 127.0.0.1 (pin/tftp, as the
// agent's file server uses). A missing file is "file not found" (code 1).
type tftpServer struct {
	port int

	mu         sync.Mutex
	files      map[string][]byte
	serveAfter map[string]int // a file is "not found" for its first n requests
	requests   []string
}

func startTFTP(t *testing.T) *tftpServer {
	t.Helper()
	ts := &tftpServer{files: map[string][]byte{}, serveAfter: map[string]int{}}
	s := tftp.NewServer(func(name string, rf io.ReaderFrom) error {
		ts.mu.Lock()
		ts.requests = append(ts.requests, name)
		n := 0
		for _, r := range ts.requests {
			if r == name {
				n++
			}
		}
		data, ok := ts.files[name]
		if n <= ts.serveAfter[name] {
			ok = false
		}
		ts.mu.Unlock()
		if !ok {
			return errors.New("file not found")
		}
		_, err := rf.ReadFrom(bytes.NewReader(data))
		return err
	}, nil)
	s.SetTimeout(2 * time.Second)
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts.port = conn.LocalAddr().(*net.UDPAddr).Port
	go func() { _ = s.Serve(conn) }()
	t.Cleanup(s.Shutdown)
	return ts
}

func (ts *tftpServer) put(name string, data []byte) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.files[name] = data
}

// remove makes name "file not found".
func (ts *tftpServer) remove(name string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.files, name)
}

// setServeAfter makes name "not found" for its first n requests.
func (ts *tftpServer) setServeAfter(name string, n int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.serveAfter[name] = n
}

// deskSEP is the cisco79x5 desk golden pointed at pbx (127.0.0.1 and its
// port) with password resolved, and the label replaced when set.
func deskSEP(t *testing.T, pbx *fakePBX, password, label string) []byte {
	t.Helper()
	doc := string(resolve(readGolden(t, filepath.Join(ciscoGoldens, "desk/tftp/SEP"+deskMAC+".cnf.xml")), password))
	doc = strings.ReplaceAll(doc, "192.168.0.240", "127.0.0.1")
	doc = strings.Replace(doc, "<sipPort>5060</sipPort>", "<sipPort>"+strconv.Itoa(int(pbx.addr.Port()))+"</sipPort>", 1)
	if label != "" {
		doc = strings.Replace(doc, "<phoneLabel>Desk</phoneLabel>", "<phoneLabel>"+label+"</phoneLabel>", 1)
	}
	return []byte(doc)
}

// ciscoScript is a cisco phone of MAC deskMAC served by ts.
func ciscoScript(steps ...script.Step) script.Script {
	return script.Script{Mode: script.ModeCisco, Server: "127.0.0.1", MAC: deskMAC, Steps: steps}
}

// deskPhone is a PBX that knows phone-desk's password, a TFTP server
// serving its SEP file, and the options that point a run at them.
func deskPhone(t *testing.T, password string) (*fakePBX, *tftpServer, runOptions) {
	t.Helper()
	pbx := startPBX(t, map[string]string{"phone-desk": password})
	ts := startTFTP(t)
	ts.put("SEP"+deskMAC+".cnf.xml", deskSEP(t, pbx, password, ""))
	return pbx, ts, runOptions{TFTPPort: ts.port}
}

func dur(d time.Duration) script.Duration { return script.Duration(d) }
