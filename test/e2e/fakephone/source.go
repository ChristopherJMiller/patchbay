package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// `fakephone source`: the e2e suite's stream sources (script/source.go is
// the contract): a fake HDHomeRun with a few tuners and a fake Icecast
// station, each streaming a tone as a 48 kHz stereo WAV at real time, so
// the stream bridge's ffmpeg has a real network source to decode and
// resample, and no test reaches a tuner or the internet.
//
// Beyond the contract: a stream sends its first sourceBurst (4 s) of
// audio at once, then paces it at real time, as Icecast bursts on connect
// (the contract says only "paced at real time"; without the burst,
// ffmpeg's probe of the WAV holds the first audio back 2.7 s, see
// sourceBurst). POST SourcePathOccupy with more tuners than are free (open
// streams hold theirs) is 409 Conflict, with the status as the body, as a
// DVR cannot take a tuner the bridge holds; any other method on a path is
// 405, an unusable hz 400, and SourcePathOccupy on the radio listener 404.

const (
	// sourceChunk is one write of a stream: 20 ms, like a call's packet.
	sourceChunk = 20 * time.Millisecond
	// sourceFramesPerChunk is a chunk in sample frames (one sample per
	// channel), sourceBytesPerFrame a frame in bytes (16-bit samples).
	sourceFramesPerChunk = script.SourceSampleRate * int(sourceChunk/time.Millisecond) / 1000
	sourceBytesPerFrame  = script.SourceChannels * 2
	// sourceBurst is how much audio a stream sends at once before it is
	// paced, as Icecast bursts on connect: ffmpeg reads 512 KiB of a PCM
	// WAV's data before it decodes any (probing it for S/PDIF or
	// mislabelled MP3; ffmpeg 8.1 and 9.0, with FFmpegArgs' limits),
	// which at this WAV's 192 kB/s would take 2.7 s; a tuner's MPEG-TS
	// (megabytes a second) and an Icecast mount fill ffmpeg's probe in a
	// fraction of a second. 4 s (768 kB) covers the probe with room.
	sourceBurst       = 4 * time.Second
	sourceBurstChunks = int(sourceBurst / sourceChunk)
	// sourceAmplitude is the tone's level, of full scale.
	sourceAmplitude = 0.5
	// maxSourceHz keeps the tone below the WAV's Nyquist frequency.
	maxSourceHz = script.SourceSampleRate / 2
	// wavDataSize is the data chunk's size in the header: the largest
	// whole number of frames whose RIFF size still fits 32 bits, as a
	// stream of unknown length must claim.
	wavDataSize = (math.MaxUint32 - 36) / sourceBytesPerFrame * sourceBytesPerFrame
)

// sourceOptions configures runSource.
type sourceOptions struct {
	// TunerListen and RadioListen are the listeners' addresses; "" turns
	// that listener off (at least one must be on).
	TunerListen, RadioListen string
	// Tuners is how many streams the tuner listener serves at once.
	Tuners int
	// LockDelay is how long the tuner listener waits after the response
	// header before the first audio, as a tuner locks.
	LockDelay time.Duration
	Log       *slog.Logger
	// Started, when set, is called with the bound listeners (nil for one
	// turned off) once both serve.
	Started func(tuner, radio net.Addr)
}

func newSourceCmd(rf *rootFlags) *cobra.Command {
	o := sourceOptions{}
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Serve fake stream sources (an HDHomeRun, an Icecast station) until stopped",
		Long: `Serve the e2e suite's stream sources until SIGTERM (--timeout does not apply):

  --tuner-listen  a fake HDHomeRun with --tuners tuners: each open stream
                  holds one until its client disconnects, and with none free
                  (open streams plus those POST /fake/occupy holds) a request
                  is 503 with X-HDHomeRun-Error: 805 All Tuners In Use; the
                  audio starts --lock-delay after the response header.
  --radio-listen  a fake Icecast station: no tuners, no limit, no delay.

GET of any path but /fake/status (that listener's counters, as JSON) streams
a tone: a WAV header (48 kHz, stereo, 16-bit), then its first 4 s at once (as
Icecast bursts), then the rest at real time in 20 ms writes; the tone's
frequency is ?hz= (default 1000).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.Log = rf.logger()
			return runSource(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.TunerListen, "tuner-listen", ":"+strconv.Itoa(script.DefaultTunerPort), "the fake tuner's address; empty: none")
	f.IntVar(&o.Tuners, "tuners", 2, "the fake tuner's tuners")
	f.DurationVar(&o.LockDelay, "lock-delay", 1500*time.Millisecond, "the fake tuner's delay before the first audio")
	f.StringVar(&o.RadioListen, "radio-listen", ":"+strconv.Itoa(script.DefaultRadioPort), "the fake radio's address; empty: none")
	return cmd
}

// runSource serves the listeners o names until ctx ends, then stops them
// (open streams end) and returns nil; it fails at once on bad options or a
// listener that cannot bind.
func runSource(ctx context.Context, o sourceOptions) error {
	switch {
	case o.TunerListen == "" && o.RadioListen == "":
		return errors.New("source: --tuner-listen and --radio-listen are both empty")
	case o.Tuners < 0:
		return fmt.Errorf("source: --tuners %d is negative", o.Tuners)
	case o.LockDelay < 0:
		return fmt.Errorf("source: --lock-delay %s is negative", o.LockDelay)
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type served struct {
		srv *http.Server
		ln  net.Listener
	}
	var all []served
	addrs := map[string]net.Addr{}
	var lc net.ListenConfig
	for _, l := range []struct {
		name, addr string
		src        *fakeSource
	}{
		{"tuner", o.TunerListen, &fakeSource{name: "tuner", limited: true, tuners: o.Tuners, lockDelay: o.LockDelay}},
		{"radio", o.RadioListen, &fakeSource{name: "radio"}},
	} {
		if l.addr == "" {
			continue
		}
		ln, err := lc.Listen(ctx, "tcp", l.addr)
		if err != nil {
			for _, s := range all {
				_ = s.ln.Close()
			}
			return fmt.Errorf("source: the %s listener: %w", l.name, err)
		}
		l.src.log = log.With("source", l.name)
		l.src.open, l.src.opened = map[string]int{}, map[string]uint64{}
		srv := &http.Server{
			Handler:           l.src,
			ReadHeaderTimeout: 5 * time.Second,
			// A stream ends when the source stops: its request context is
			// this one's.
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		all = append(all, served{srv, ln})
		addrs[l.name] = ln.Addr()
		log.Info("serving", "source", l.name, "addr", ln.Addr().String(), "tuners", l.src.tuners, "lockDelay", l.src.lockDelay)
	}

	errc := make(chan error, len(all))
	for _, s := range all {
		go func() {
			if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
				return
			}
			errc <- nil
		}()
	}
	if o.Started != nil {
		o.Started(addrs["tuner"], addrs["radio"])
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	cancel()
	stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	for _, s := range all {
		_ = s.srv.Shutdown(stopCtx)
	}
	if serveErr != nil {
		return fmt.Errorf("source: %w", serveErr)
	}
	return nil
}

// fakeSource is one listener.
type fakeSource struct {
	name      string
	limited   bool // a tuner: at most tuners streams, and the occupied ones
	tuners    int
	lockDelay time.Duration
	log       *slog.Logger

	mu       sync.Mutex
	occupied int
	open     map[string]int
	opened   map[string]uint64
	refused  uint64
}

// status is the listener's script.SourceStatus (maps copied, never nil).
func (s *fakeSource) status() script.SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *fakeSource) statusLocked() script.SourceStatus {
	st := script.SourceStatus{
		Occupied: s.occupied, Refused: s.refused,
		Open: maps.Clone(s.open), Opened: maps.Clone(s.opened),
	}
	if s.limited {
		st.Tuners = s.tuners
	}
	return st
}

func (s *fakeSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case script.SourcePathStatus:
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeStatus(w, http.StatusOK, s.status())
	case script.SourcePathOccupy:
		s.occupy(w, r)
	default:
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		s.stream(w, r)
	}
}

func writeStatus(w http.ResponseWriter, code int, st script.SourceStatus) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(st)
}

// occupy is POST SourcePathOccupy: hold ?tuners=n tuners, as a DVR does.
func (s *fakeSource) occupy(w http.ResponseWriter, r *http.Request) {
	if !s.limited {
		http.Error(w, "the radio has no tuners", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get(script.SourceParamTuners))
	if err != nil || n < 0 || n > s.tuners {
		http.Error(w, fmt.Sprintf("%s must be a number from 0 to %d", script.SourceParamTuners, s.tuners), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	st := s.statusLocked()
	if n > s.tuners-st.OpenTotal() {
		s.mu.Unlock()
		s.log.Info("occupy refused", "tuners", n, "open", st.OpenTotal())
		writeStatus(w, http.StatusConflict, st)
		return
	}
	s.occupied = n
	st = s.statusLocked()
	s.mu.Unlock()
	s.log.Info("occupied", "tuners", n)
	writeStatus(w, http.StatusOK, st)
}

// stream serves a tone until the client goes or the source stops.
func (s *fakeSource) stream(w http.ResponseWriter, r *http.Request) {
	hz := float64(script.DefaultSourceHz)
	if v := r.URL.Query().Get(script.SourceParamHz); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f >= maxSourceHz || math.IsNaN(f) {
			http.Error(w, fmt.Sprintf("%s must be a frequency from 0 to %d Hz", script.SourceParamHz, maxSourceHz), http.StatusBadRequest)
			return
		}
		hz = f
	}
	path := r.URL.Path
	if !s.acquire(path) {
		s.log.Info("refused", "path", path, "from", r.RemoteAddr)
		w.Header().Set(script.HDHomeRunErrorHeader, script.HDHomeRunAllTunersInUse)
		http.Error(w, script.HDHomeRunAllTunersInUse, http.StatusServiceUnavailable)
		return
	}
	start := time.Now()
	s.log.Info("open", "path", path, "hz", hz, "from", r.RemoteAddr)
	defer func() {
		s.release(path)
		s.log.Info("closed", "path", path, "after", time.Since(start).Round(time.Millisecond).String(), "from", r.RemoteAddr)
	}()

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	ctx := r.Context()
	if s.limited && s.lockDelay > 0 {
		t := time.NewTimer(s.lockDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}

	tone := &stereoTone{freq: hz}
	buf := wavHeader(make([]byte, 0, 44+sourceBurstChunks*sourceFramesPerChunk*sourceBytesPerFrame))
	for range sourceBurstChunks {
		buf = tone.appendChunk(buf)
	}
	if _, err := w.Write(buf); err != nil {
		return
	}
	buf = buf[:0]
	next := time.Now()
	for {
		buf = tone.appendChunk(buf)
		if _, err := w.Write(buf); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		buf = buf[:0]
		next = next.Add(sourceChunk)
		// A client that fell a second behind (a stalled reader) resumes
		// from now rather than receive a burst.
		if now := time.Now(); now.Sub(next) > time.Second {
			next = now
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// acquire counts a new stream of path, unless a tuner listener has no
// tuner free.
func (s *fakeSource) acquire(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limited {
		used := s.occupied
		for _, n := range s.open {
			used += n
		}
		if used >= s.tuners {
			s.refused++
			return false
		}
	}
	s.open[path]++
	s.opened[path]++
	return true
}

func (s *fakeSource) release(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[path]--; s.open[path] <= 0 {
		delete(s.open, path)
	}
}

// wavHeader appends a canonical 44-byte WAV header for an endless stream of
// script.SourceSampleRate, script.SourceChannels, 16-bit PCM.
func wavHeader(dst []byte) []byte {
	le := binary.LittleEndian
	dst = append(dst, "RIFF"...)
	dst = le.AppendUint32(dst, 36+wavDataSize)
	dst = append(dst, "WAVEfmt "...)
	dst = le.AppendUint32(dst, 16)
	dst = le.AppendUint16(dst, 1) // PCM
	dst = le.AppendUint16(dst, script.SourceChannels)
	dst = le.AppendUint32(dst, script.SourceSampleRate)
	dst = le.AppendUint32(dst, script.SourceSampleRate*sourceBytesPerFrame)
	dst = le.AppendUint16(dst, sourceBytesPerFrame)
	dst = le.AppendUint16(dst, 16)
	dst = append(dst, "data"...)
	return le.AppendUint32(dst, wavDataSize)
}

// stereoTone is a sine wave, the same on both channels, whose phase runs
// on across chunks.
type stereoTone struct {
	freq  float64
	phase float64 // radians, in [0, 2π)
}

// appendChunk appends sourceFramesPerChunk frames of the tone to dst.
func (g *stereoTone) appendChunk(dst []byte) []byte {
	step := 2 * math.Pi * g.freq / script.SourceSampleRate
	for range sourceFramesPerChunk {
		v := int16(math.Round(sourceAmplitude * math.MaxInt16 * math.Sin(g.phase)))
		for range script.SourceChannels {
			dst = binary.LittleEndian.AppendUint16(dst, uint16(v))
		}
		if g.phase += step; g.phase >= 2*math.Pi {
			g.phase -= 2 * math.Pi
		}
	}
	return dst
}
