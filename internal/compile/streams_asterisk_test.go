//go:build asterisk

package compile_test

// TestRealAsteriskStreams (make asterisk-check, docs/plans/release-b.md
// RT.1): the streams golden in the chart's Asterisk, with a stand-in for
// the stream bridge. This test binary runs inside the container a second
// way, as an AudioSocket server (audioSocketEnv), which records each
// connection's first frame and what it sent into a tmpfs, and, by UUID,
// plays a tone for a while and then hangs up, or hangs up at once. Then,
// as the desk phone, the SIP client calls the streams and the test reads
// the records and Asterisk's log.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

const (
	// audioSocketEnv selects the AudioSocket server mode; its value is the
	// directory the records go in.
	audioSocketEnv = "PATCHBAY_CHECK_AUDIOSOCKET"
	// recordDir is a tmpfs in every check container (startAsterisk).
	recordDir = "/tmp/patchbay-check"
	// checkBridge is where the stand-in bridge listens, inside the
	// container: the streams golden's streamBridge is replaced by it, as
	// checkSettings replaces the PBX address.
	checkBridge = "127.0.0.1:9092"
)

// The AudioSocket frame (internal/streambridge/audiosocket's contract,
// docs/plans/release-b.md §7.1): a kind byte, a big-endian 16-bit payload
// length, the payload. Repeated here so this check builds without the
// bridge's code.
const (
	kindHangup   = 0x00
	kindUUID     = 0x01
	kindSlin16   = 0x12
	headerSize   = 3
	frameTimeout = 5 * time.Second
)

// audioSocketRecord is what the server saw and did on one connection.
type audioSocketRecord struct {
	FirstKind    int     `json:"firstKind"`
	FirstPayload string  `json:"firstPayload"` // hex
	Played       string  `json:"played"`       // how long it was asked to play, or ""
	SentFrames   int     `json:"sentFrames"`   // audio frames written
	WriteError   string  `json:"writeError,omitempty"`
	HangupSent   bool    `json:"hangupSent"`
	PeerClosed   bool    `json:"peerClosed"` // Asterisk closed the connection
	Seconds      float64 `json:"seconds"`    // from the first frame to the close
	// Received counts Asterisk's frames after the first, by "kind/size"
	// (e.g. "0x12/640": the caller's audio at 16 kHz).
	Received map[string]int `json:"received"`
}

// runAudioSocketServer is the stand-in bridge: it listens on
// AUDIOSOCKET_LISTEN, writes <dir>/ready, serves AUDIOSOCKET_CONNECTIONS
// connections (each in turn may overlap the next), writes <dir>/done and
// exits. AUDIOSOCKET_PLAY lists <uuid>=<duration> pairs, comma
// separated: a call on such a UUID hears a 1 kHz tone, 640-byte slin16
// frames paced at 20 ms, for that long, then a hangup frame; any other
// call gets the hangup frame at once. It never runs longer than 3 min.
func runAudioSocketServer(dir string) int {
	fail := func(err error) int {
		fmt.Println("error:", err)
		return 2
	}
	plays := map[string]time.Duration{}
	for p := range strings.SplitSeq(os.Getenv("AUDIOSOCKET_PLAY"), ",") {
		if p == "" {
			continue
		}
		id, d, ok := strings.Cut(p, "=")
		dur, err := time.ParseDuration(d)
		if !ok || err != nil {
			return fail(fmt.Errorf("AUDIOSOCKET_PLAY: %q", p))
		}
		plays[id] = dur
	}
	n, err := strconv.Atoi(os.Getenv("AUDIOSOCKET_CONNECTIONS"))
	if err != nil {
		return fail(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", os.Getenv("AUDIOSOCKET_LISTEN"))
	if err != nil {
		return fail(err)
	}
	if err := writeAtomic(filepath.Join(dir, "ready"), []byte("ready\n")); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	var wg sync.WaitGroup
	for i := range n {
		_ = ln.(*net.TCPListener).SetDeadline(deadline)
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveCheckConnection(conn, dir, i, plays)
		}()
	}
	_ = ln.Close()
	wg.Wait()
	if err := writeAtomic(filepath.Join(dir, "done"), []byte("done\n")); err != nil {
		return fail(err)
	}
	return 0
}

// serveCheckConnection serves one call and writes <dir>/<uuid>.json (or
// conn-<i>.json when the first frame names none).
func serveCheckConnection(conn net.Conn, dir string, i int, plays map[string]time.Duration) {
	defer func() { _ = conn.Close() }()
	var mu sync.Mutex
	rec := audioSocketRecord{Received: map[string]int{}}
	name := fmt.Sprintf("conn-%d", i)
	save := func() {
		mu.Lock()
		raw, _ := json.Marshal(rec)
		mu.Unlock()
		_ = writeAtomic(filepath.Join(dir, name+".json"), raw)
	}

	_ = conn.SetReadDeadline(time.Now().Add(frameTimeout))
	kind, payload, err := readCheckFrame(conn)
	if err != nil {
		rec.WriteError = "reading the first frame: " + err.Error()
		save()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	start := time.Now()
	rec.FirstKind, rec.FirstPayload = int(kind), hex.EncodeToString(payload)
	if kind == kindUUID && len(payload) == 16 {
		name = streams.FormatUUID([16]byte(payload))
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			k, p, err := readCheckFrame(conn)
			if err != nil {
				mu.Lock()
				rec.PeerClosed = errors.Is(err, io.EOF)
				mu.Unlock()
				return
			}
			mu.Lock()
			rec.Received[fmt.Sprintf("0x%02x/%d", k, len(p))]++
			mu.Unlock()
		}
	}()

	if d := plays[name]; d > 0 {
		rec.Played = d.String()
		tone := newTone(1000)
		next := time.Now()
	play:
		for range int(d / streams.FrameDuration) {
			select {
			case <-closed:
				break play
			default:
			}
			frame := appendCheckFrame(nil, kindSlin16, tone.next())
			if _, err := conn.Write(frame); err != nil {
				mu.Lock()
				rec.WriteError = err.Error()
				mu.Unlock()
				break
			}
			mu.Lock()
			rec.SentFrames++
			mu.Unlock()
			next = next.Add(streams.FrameDuration)
			time.Sleep(time.Until(next))
		}
	}
	select {
	case <-closed:
	default:
		if _, err := conn.Write(appendCheckFrame(nil, kindHangup, nil)); err == nil {
			mu.Lock()
			rec.HangupSent = true
			mu.Unlock()
		}
	}
	select { // Asterisk closes the connection once it has the hangup
	case <-closed:
	case <-time.After(frameTimeout):
	}
	_ = conn.Close()
	<-closed
	mu.Lock()
	rec.Seconds = time.Since(start).Seconds()
	mu.Unlock()
	save()
}

// readCheckFrame reads one AudioSocket frame.
func readCheckFrame(r io.Reader) (byte, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(h[1:]))
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// appendCheckFrame appends one AudioSocket frame to dst.
func appendCheckFrame(dst []byte, kind byte, payload []byte) []byte {
	dst = append(dst, kind, 0, 0)
	binary.BigEndian.PutUint16(dst[len(dst)-2:], uint16(len(payload)))
	return append(dst, payload...)
}

// tone is a sine at 16 kHz, signed 16-bit little-endian, its phase kept
// from frame to frame.
type tone struct {
	hz float64
	n  int
}

func newTone(hz float64) *tone { return &tone{hz: hz} }

func (t *tone) next() []byte {
	out := make([]byte, streams.FrameBytes)
	for i := range streams.FrameSamples {
		v := int16(8000 * math.Sin(2*math.Pi*t.hz*float64(t.n)/streams.SampleRate))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
		t.n++
	}
	return out
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// audioSocket starts the stand-in bridge in the container, detached, and
// waits until it listens.
func (c asteriskContainer) audioSocket(env map[string]string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := []string{"exec", "--detach", "-e", audioSocketEnv + "=" + recordDir}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	if _, err := docker(ctx, append(args, c.name, clientDir+"/client")...); err != nil {
		c.t.Fatal(err)
	}
	c.waitFile("ready", 15*time.Second)
}

// waitFile waits for a file in recordDir and returns it.
func (c asteriskContainer) waitFile(name string, timeout time.Duration) []byte {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := docker(ctx, "exec", c.name, "cat", recordDir+"/"+name)
		cancel()
		if err == nil {
			return []byte(out)
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("no %s/%s after %s: %v", recordDir, name, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// record is the stand-in bridge's record of a call on a stream.
func (c asteriskContainer) record(name string) audioSocketRecord {
	c.t.Helper()
	return c.recordWithin(name, 30*time.Second)
}

// recordWithin is the record of a call on a stream, waited for up to
// timeout (the stand-in writes it when the call ends).
func (c asteriskContainer) recordWithin(name string, timeout time.Duration) audioSocketRecord {
	c.t.Helper()
	raw := c.waitFile(streams.UUID(name)+".json", timeout)
	var rec audioSocketRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		c.t.Fatalf("the record of %s: %v\n%s", name, err, raw)
	}
	return rec
}

// logs is the container's log so far.
func (c asteriskContainer) logs() string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := docker(ctx, "logs", c.name)
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

// uuidHex is a stream's UUID as the ID frame's payload, in hex.
func uuidHex(t *testing.T, name string) string {
	t.Helper()
	u, err := streams.ParseUUID(streams.UUID(name))
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(u[:])
}

// heldCall is the client's line for a call held with SIP_HOLD.
var heldCall = regexp.MustCompile(`^status=(\d+) endedBy=(local|remote) held=([0-9.]+)$`)

// TestRealAsteriskStreams boots the streams golden with the stand-in
// bridge's address and the shortest call limit (10 s), then, as the desk
// phone:
//   - 705 (king5): answered; the bridge got an ID frame with king5's UUID
//     and wrote 2 s of 16 kHz audio without a write error, then hung up,
//     and the dialplan went on to "not available";
//   - 707 (kiro7): the bridge hangs up at once; the call is still answered
//     and goes on to "not available";
//   - 704 (komo4): the bridge plays for 20 s; Asterisk ends the call at the
//     10 s limit (TIMEOUT(absolute)), closing the bridge's connection, and
//     never reaches "not available";
//   - 713 (fox13): the phone blind-transfers the call to the echo test 2 s
//     in (a REFER) and hangs up; the bridge plays for 40 s, and Asterisk
//     still closes its connection at the 10 s limit (Dial's S(), the
//     limit on the AudioSocket leg, which the transfer keeps);
//   - 750 (groovesalad), with nothing listening: answered, "not
//     available";
//
// and `dialplan show 705@patchbay-devices` is the compiled extension.
func TestRealAsteriskStreams(t *testing.T) {
	env := newCheckEnv(t)
	in, s := loadGolden(t, "streams")
	s = checkSettings(s, true)
	s.StreamBridge = checkBridge
	s.Streams.CallLimit = metav1.Duration{Duration: compile.MinCallLimit}
	b, err := compile.Compile(builtinRegistry(t), in, s)
	if err != nil {
		t.Fatal(err)
	}
	c := startAsterisk(t, env, fmt.Sprintf("patchbay-asterisk-check-%d-streams-calls", os.Getpid()), b, in.Secrets)

	// The extension Asterisk holds for 705 is the compiled one, step for
	// step.
	steps := compiledSteps(t, b, "705")
	if !slices.Equal(steps, []string{
		"Answer()",
		"Set(TIMEOUT(absolute)=10)",
		"Dial(AudioSocket/" + checkBridge + "/" + streams.UUID("king5") + "/c(slin16),,gS(10))",
		"Goto(patchbay-unavailable,s,1)",
	}) {
		t.Fatalf("705 compiles to %q", steps)
	}
	shown := c.cli("dialplan show 705@" + compile.DevicesContext)
	for i, step := range steps {
		if !regexp.MustCompile(`(?m)\b` + strconv.Itoa(i+1) + `\. ` + regexp.QuoteMeta(step) + `\s`).MatchString(shown) {
			t.Errorf("dialplan show 705 lacks step %d %s:\n%s", i+1, step, shown)
		}
	}

	c.audioSocket(map[string]string{
		"AUDIOSOCKET_LISTEN": checkBridge,
		"AUDIOSOCKET_PLAY": streams.UUID("king5") + "=2s," + streams.UUID("komo4") + "=20s," +
			streams.UUID("fox13") + "=40s",
		"AUDIOSOCKET_CONNECTIONS": "4",
	})
	id := drivers.DeviceID(drivers.KindPhone, "desk")
	port := s.SIPPort + 300
	call := func(number string, hold time.Duration) (status, endedBy string, held float64) {
		t.Helper()
		port += 10
		out := c.sip("call", streamCallEnv(t, b, s, in.Secrets, id, port, number, hold))
		m := heldCall.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("calling %s: %s", number, out)
		}
		held, _ = strconv.ParseFloat(m[3], 64)
		return m[1], m[2], held
	}
	unavailableAfterDial := func(number string) bool {
		return strings.Contains(c.logs(), "Executing ["+number+"@"+compile.DevicesContext+":4] Goto(")
	}

	t.Run("plays", func(t *testing.T) {
		// The check container has no prompts, so "not available" is the
		// SIT tone and a second of silence, then the PBX hangs up.
		if status, _, held := call("705", 8*time.Second); status != "200" || held < 2 {
			t.Errorf("705: status %s, up %.1f s; want 200, up through the bridge's 2 s of audio", status, held)
		}
		rec := c.record("king5")
		t.Logf("the bridge's record of king5: %+v", rec)
		if rec.FirstKind != kindUUID || rec.FirstPayload != uuidHex(t, "king5") {
			t.Errorf("the bridge's first frame: kind %#x payload %s; want an ID frame with king5's UUID %s", rec.FirstKind, rec.FirstPayload, uuidHex(t, "king5"))
		}
		if rec.SentFrames != 100 || rec.WriteError != "" || !rec.HangupSent {
			t.Errorf("the bridge sent %d frames (error %q, hangup %t); want 2 s (100 frames) without an error, then a hangup",
				rec.SentFrames, rec.WriteError, rec.HangupSent)
		}
		for k := range rec.Received {
			if k != fmt.Sprintf("0x%02x/%d", kindSlin16, streams.FrameBytes) && !strings.HasPrefix(k, "0x03/") {
				t.Errorf("Asterisk sent the bridge %s frames; want only 16 kHz audio (0x12/640) and DTMF", k)
			}
		}
		if !unavailableAfterDial("705") {
			t.Errorf("after the bridge hung up, 705 did not go on to \"not available\"")
		}
	})

	t.Run("refused", func(t *testing.T) {
		if status, _, _ := call("707", 3*time.Second); status != "200" {
			t.Errorf("707: status %s; want 200", status)
		}
		rec := c.record("kiro7")
		t.Logf("the bridge's record of kiro7: %+v", rec)
		if rec.FirstKind != kindUUID || rec.FirstPayload != uuidHex(t, "kiro7") || rec.SentFrames != 0 || !rec.HangupSent {
			t.Errorf("record %+v; want kiro7's ID frame, then only a hangup", rec)
		}
		if !unavailableAfterDial("707") {
			t.Errorf("after the bridge refused it, 707 did not go on to \"not available\"")
		}
	})

	t.Run("time limit", func(t *testing.T) {
		status, by, held := call("704", 16*time.Second)
		if status != "200" || by != "remote" || held < 9.5 || held > 11.5 {
			t.Errorf("704: status %s, ended by %s after %.1f s; want 200, ended by the PBX at the 10 s limit", status, by, held)
		}
		rec := c.record("komo4")
		t.Logf("the bridge's record of komo4: %+v", rec)
		if !rec.PeerClosed || rec.HangupSent || rec.Seconds < 9.5 || rec.Seconds > 12 || rec.SentFrames < 450 || rec.SentFrames > 560 {
			t.Errorf("record %+v; want Asterisk to close the bridge's connection at about 10 s, mid-tone", rec)
		}
		if unavailableAfterDial("704") {
			t.Errorf("the time limit fell through to \"not available\"; it should end the call")
		}
	})

	t.Run("transferred", func(t *testing.T) {
		// The desk phone blind-transfers its 713 call to the echo test 2 s
		// in, by a REFER as the 7945's transfer key sends, and hangs up 2 s
		// later. The stream's AudioSocket leg then plays into Echo() with
		// nobody on the line, and must still end at the 10 s limit (the
		// stand-in would play for 40 s).
		port += 10
		env := streamCallEnv(t, b, s, in.Secrets, id, port, "713", 4*time.Second)
		env["SIP_REFER_TO"], env["SIP_REFER_AFTER"] = "600", "2s"
		out := c.sip("call", env)
		m := regexp.MustCompile(`^status=(\d+) endedBy=(local|remote) held=([0-9.]+) refer=(\d+)$`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("calling 713: %s", out)
		}
		if m[1] != "200" || m[4] != "202" {
			t.Errorf("713: %s; want 200, then the REFER taken (202)", out)
		}
		rec := c.recordWithin("fox13", 60*time.Second)
		t.Logf("the bridge's record of fox13: %+v", rec)
		if !rec.PeerClosed || rec.HangupSent || rec.Seconds < 9.5 || rec.Seconds > 12 {
			t.Errorf("record %+v; want Asterisk to close the transferred call's connection at about 10 s, mid-tone", rec)
		}
		if !strings.Contains(c.logs(), "Executing [600@"+compile.DevicesContext+":2] Echo(\"AudioSocket/") {
			t.Errorf("the stream's AudioSocket leg never reached the echo test")
		}
	})

	t.Run("no bridge", func(t *testing.T) {
		c.waitFile("done", 30*time.Second) // the stand-in has served its four calls and exited
		if status, _, _ := call("750", 3*time.Second); status != "200" {
			t.Errorf("750: status %s; want 200", status)
		}
		if !unavailableAfterDial("750") {
			t.Errorf("with no bridge listening, 750 did not go on to \"not available\"")
		}
	})
}

// compiledSteps are the steps of one extension of patchbay-devices in the
// bundle's extensions.conf.
func compiledSteps(t *testing.T, b compile.Bundle, number string) []string {
	t.Helper()
	var steps []string
	in := false
	for line := range bytes.Lines(b.Files["asterisk/extensions.conf"]) {
		l := strings.TrimRight(string(line), "\n")
		switch {
		case strings.HasPrefix(l, "exten => "+number+",1,"):
			in = true
			steps = append(steps, strings.TrimPrefix(l, "exten => "+number+",1,"))
		case in && strings.HasPrefix(l, " same => n,"):
			steps = append(steps, strings.TrimPrefix(l, " same => n,"))
		default:
			in = false
		}
	}
	return steps
}

// streamCallEnv is the SIP client's environment for a call from a
// rendered device to number, held for hold.
func streamCallEnv(t *testing.T, b compile.Bundle, s compile.Settings, secrets map[compile.SecretRef][]byte,
	id string, port int, number string, hold time.Duration,
) map[string]string {
	t.Helper()
	objects, err := pjsipObjects(b.Files["asterisk/pjsip.conf"])
	if err != nil {
		t.Fatal(err)
	}
	transport, _ := objects[[2]string{"endpoint", id}].one("transport")
	return map[string]string{
		"SIP_SERVER": fmt.Sprintf("%s:%d", s.PBXAddress, s.SIPPort), "SIP_TRANSPORT": strings.TrimPrefix(transport, "transport-"),
		"SIP_USER": b.Devices[id].Number, "SIP_AUTH_USER": id, "SIP_PASSWORD": string(secrets[compile.PasswordRef(id)]),
		"SIP_PORT": strconv.Itoa(port), "SIP_EXPIRES": "3600", "SIP_TARGET": number, "SIP_HOLD": hold.String(),
	}
}
