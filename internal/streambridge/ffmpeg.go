package streambridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// FFmpegSource is the production Source: ffmpeg with FFmpegArgs, one
// process per decoder.
type FFmpegSource struct {
	path, caFile string
	log          logr.Logger
}

// NewFFmpegSource runs the ffmpeg at path, verifying https sources against
// caFile ("" is the system's trust store).
func NewFFmpegSource(path, caFile string, log logr.Logger) *FFmpegSource {
	return &FFmpegSource{path: path, caFile: caFile, log: log}
}

// stderrTail is how much of ffmpeg's standard error a decoder keeps: its
// last lines say why it stopped.
const stderrTail = 4 << 10

// versionTimeout bounds `ffmpeg -version`.
const versionTimeout = 10 * time.Second

// Start runs ffmpeg on source (which must pass streams.CheckSource) and
// returns once the process runs; its standard output is the decoder's
// audio. When ctx ends, ffmpeg gets SIGTERM, and SIGKILL StopTimeout
// later; its standard error is kept to its last 4 KiB for Wait's reason.
func (s *FFmpegSource) Start(ctx context.Context, source string) (Decoder, error) {
	if err := streams.CheckSource(source); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.path, FFmpegArgs(source, s.caFile)...) //nolint:gosec // G204: the operator's ffmpeg, with FFmpegArgs (a checked source, no shell).
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = StopTimeout
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New(printable("starting ffmpeg: "+err.Error(), api.MaxMessageBytes))
	}
	s.log.V(1).Info("ffmpeg started", "source", source, "pid", cmd.Process.Pid)
	return &ffmpegDecoder{cmd: cmd, out: out, ctx: ctx, stderr: stderr}, nil
}

// Version runs `ffmpeg -version` and returns its first line ("ffmpeg
// version 8.1.2 Copyright …"), for readiness and BridgeStatus.FFmpeg.
func (s *FFmpegSource) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.path, "-version").Output() //nolint:gosec // G204: the operator's ffmpeg, a fixed argument.
	if err != nil {
		return "", errors.New(printable("ffmpeg -version: "+err.Error(), api.MaxMessageBytes))
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if first = printable(first, api.MaxMessageBytes); first == "" {
		return "", errors.New("ffmpeg -version printed nothing")
	}
	return first, nil
}

// ffmpegDecoder is one ffmpeg process.
type ffmpegDecoder struct {
	cmd    *exec.Cmd
	out    io.Reader
	ctx    context.Context
	stderr *tailBuffer

	once sync.Once
	err  error
}

// Read reads ffmpeg's standard output: the decoded PCM.
func (d *ffmpegDecoder) Read(p []byte) (int, error) { return d.out.Read(p) }

// Wait waits for ffmpeg to exit (call it once Read has failed: it closes
// the output) and says why: nil if its ctx ended, otherwise how it exited
// and its last error line (exitMessage, ffmpegReason).
func (d *ffmpegDecoder) Wait() error {
	d.once.Do(func() {
		err := d.cmd.Wait()
		if d.ctx.Err() != nil {
			return
		}
		code, signal := 0, ""
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signal = ws.Signal().String()
			} else {
				code = exit.ExitCode()
			}
		default:
			d.err = errors.New(printable("ffmpeg: "+err.Error(), api.MaxMessageBytes))
			return
		}
		d.err = errors.New(exitMessage(code, signal, ffmpegReason(d.stderr.String())))
	})
	return d.err
}

// exitMessage is a decoder's end for a person: "ffmpeg exited with status
// 8: Server returned 5XX Server Error reply", or "ffmpeg was killed by
// <signal>". ffmpeg exits with status 0 when its source closes (an Icecast
// server going away, a stall past -rw_timeout), so status 0 is a failure
// too, and reads "the source ended", with ffmpeg's last line after it in
// parentheses: a live source (chunked, or no length) that closes makes
// ffmpeg 9.0 log "Error during demuxing: Input/output error", which would
// otherwise read like a source that is not audio. At most
// api.MaxMessageBytes.
func exitMessage(code int, signal, reason string) string {
	msg := "ffmpeg exited with status " + strconv.Itoa(code)
	switch {
	case signal != "":
		msg = "ffmpeg was killed by " + signal
	case code == 0 && reason == "":
		reason = "the source ended"
	case code == 0:
		reason = "the source ended (" + reason + ")"
	}
	if reason != "" {
		msg += ": " + reason
	}
	return printable(msg, api.MaxMessageBytes)
}

// ffmpegPrefix is the context ffmpeg puts before a component's message:
// "[mpegts @ 0x55555560bdc0] ", "[in#0/mpegts @ 0x…] ".
var ffmpegPrefix = regexp.MustCompile(`^(\[[^\]]* @ 0x[0-9a-fA-F]+\] ?)+`)

// Lines ffmpeg (9.0) wraps an input's failure in.
const (
	openFailed  = "Error opening input: "
	openFailedN = "Error opening input files: "
	openFile    = "Error opening input file "
)

// ffmpegReason is why ffmpeg stopped, from the tail of its standard error,
// printable and at most api.MaxMessageBytes: its last non-empty line, with
// ffmpeg's "[component @ 0x…]" prefixes removed. When ffmpeg could not
// open the source, the last line is its generic wrapper ("Error opening
// input files: Input/output error"), so the reason is instead the line
// ffmpeg logged just before its "Error opening input: …" (the cause:
// "Failed to resolve hostname …", "Peer certificate failed verification"),
// or the wrapped error itself if there is none. Two kinds of failure are
// said in the bridge's words, with ffmpeg's in parentheses: "Format not on
// whitelist" and "Invalid data found when processing input" (and an
// "Input/output error" opening the source with no cause before it, which is
// how ffmpeg 9.0 ends on a short web page: it reads the whole answer
// without recognising it) become "the source is not an audio stream the
// bridge reads (…)". An HTTP error keeps ffmpeg's line ("Server returned
// 5XX Server Error reply": at -loglevel error ffmpeg names the class, not
// the code).
func ffmpegReason(stderr string) string {
	var lines []string
	for _, raw := range strings.FieldsFunc(stderr, func(r rune) bool { return r == '\n' || r == '\r' }) {
		l := strings.TrimSpace(ffmpegPrefix.ReplaceAllString(printable(raw, len(raw)), ""))
		if l == "" || strings.HasPrefix(l, openFile) || strings.HasPrefix(l, "Error opening output file ") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return ""
	}
	reason := strings.TrimPrefix(lines[len(lines)-1], openFailedN)
	unreadable := notAudio(reason)
	for i := len(lines) - 1; i >= 0; i-- {
		wrapped, ok := strings.CutPrefix(lines[i], openFailed)
		if !ok {
			continue
		}
		reason, unreadable = wrapped, notAudio(wrapped)
		if i > 0 && !strings.HasPrefix(lines[i-1], "Error opening input") {
			reason = lines[i-1]
			unreadable = unreadable || notAudio(reason)
		} else if wrapped == "Input/output error" {
			unreadable = true
		}
		break
	}
	if unreadable {
		reason = "the source is not an audio stream the bridge reads (" + reason + ")"
	}
	return printable(reason, api.MaxMessageBytes)
}

// notAudio reports whether ffmpeg's words mean the source is no format
// the bridge reads.
func notAudio(s string) bool {
	return strings.Contains(s, "Format not on whitelist") || strings.Contains(s, "Invalid data found when processing input")
}

// tailBuffer keeps the last stderrTail bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= stderrTail {
		b.buf = append(b.buf[:0], p[len(p)-stderrTail:]...)
		return n, nil
	}
	if over := len(b.buf) + len(p) - stderrTail; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.buf))
}
