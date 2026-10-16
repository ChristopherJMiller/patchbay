package streambridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// The session manager (docs/plans/release-b.md 6.1 BR.3): one session per
// source URL, shared by every caller of every stream with that source; at
// most a pool's Max sessions of its sources at once; a session's decoder
// stopped Grace after its last caller leaves.
//
// One mutex (manager.mu) guards the bookkeeping. It is never held while
// writing to a socket or reading a decoder: a session's reader takes it
// only to hand a frame to its callers' queues, which never block.

// Why the bridge refuses a call (the server sends a hangup frame).
var (
	errUnknownStream = errors.New("the stream is not in the bridge's streams.json")
	errBridgeFull    = errors.New("the bridge is at its limit of calls or sessions")
	errStopping      = errors.New("the bridge is stopping")
)

// busyError refuses a call because its stream's pool runs its Max
// sessions of other sources, each with callers; its text is the stream's
// TunerBusy failure.
type busyError struct{ msg string }

func (e *busyError) Error() string { return e.msg }

// endReason is how a call ended, for the bridge's log.
type endReason string

const (
	endCallerHungUp endReason = "the caller hung up"
	endWriteFailed  endReason = "writing to the caller failed"
	endSourceFailed endReason = "the source failed to start"
	endSourceEnded  endReason = "the source stopped"
	endStopping     endReason = "the bridge stopped"
)

// manager is the bridge's sessions, calls and memory.
type manager struct {
	source  Source
	clock   clock.WithTicker
	log     logr.Logger
	metrics *metrics
	started time.Time

	// wg counts the session and grace goroutines (stopAll waits for them).
	wg sync.WaitGroup

	mu         sync.Mutex
	doc        streams.Document
	pools      map[string]int // the document's pools: name to Max
	loadedAt   *time.Time
	streamsErr string
	ffmpeg     string
	ffmpegErr  string
	sessions   map[string]*session   // running sessions by source URL
	stopping   map[string][]*session // pool: stopped sessions whose decoder has not ended yet
	freedAt    map[string]time.Time  // pool: when a decoder of it last ended
	memory     map[string]*memory    // stream UUID: what the controller copies into status
	listeners  map[string]int        // stream UUID: open calls
	calls      int                   // open AudioSocket connections
	unknown    uint64
	full       uint64
	closed     bool
}

// memory is what the bridge remembers of a loaded stream since it started.
type memory struct {
	lastPlayed  *time.Time
	lastFailure *api.Failure
	calls       uint64
}

// sourceFailing reports whether the stream's last failure is its source's
// (SourceFailed or SourceEnded) and no call has played it since. A
// TunerBusy refusal then leaves that failure in place (and is counted only
// in refused_calls_total), so the stream stays Failing in status: the pool
// freeing up does not make a broken source playable.
func (mem *memory) sourceFailing() bool {
	f := mem.lastFailure
	if f == nil || f.Reason == api.FailureTunerBusy {
		return false
	}
	return mem.lastPlayed == nil || f.At.After(*mem.lastPlayed)
}

// session is one source playing: one decoder.
type session struct {
	source, pool string
	ctx          context.Context // the decoder's; cancel stops it
	cancel       context.CancelFunc
	waitFor      []*session    // the pool's stopped sessions whose decoders must end first
	done         chan struct{} // closed once the decoder has ended
	firstAudio   chan struct{} // closed at the first frame
	playing      atomic.Bool   // the first frame has come (the pacer's underrun count)

	// Guarded by manager.mu.
	state        api.SessionState
	startedAt    time.Time
	firstAudioAt *time.Time
	lastFrameAt  time.Time
	decoderRan   bool
	callers      map[*caller]struct{}
	streams      map[string]struct{} // the UUIDs of the streams that had a caller in it
	graceStop    chan struct{}       // closed to cancel the grace timer; nil unless Draining
	drainedAt    time.Time
	stopped      bool
}

// caller is one call on a stream, in one session.
type caller struct {
	uuid, name, number string
	sess               *session
	queue              frameQueue
	// limit is the call's time limit (streams.json's callLimitSeconds when
	// it joined; zero: none): the server ends a call CallLimitSlack past
	// it, if Asterisk has not.
	limit time.Duration
	// ended is closed when the bridge ends the call (its session stopped
	// or failed, or the bridge is stopping); why says which, set first.
	ended chan struct{}
	why   endReason
	left  bool // guarded by manager.mu
}

func newManager(src Source, clk clock.WithTicker, log logr.Logger, started time.Time) *manager {
	return &manager{
		source: src, clock: clk, log: log, metrics: newMetrics(), started: started,
		doc:      streams.Document{Pools: []streams.Pool{}, Streams: map[string]streams.Stream{}},
		pools:    map[string]int{},
		sessions: map[string]*session{}, stopping: map[string][]*session{}, freedAt: map[string]time.Time{},
		memory: map[string]*memory{}, listeners: map[string]int{},
	}
}

// register puts the bridge's metrics on reg.
func (m *manager) register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		m.metrics.starts, m.metrics.ends, m.metrics.refused, m.metrics.limitEnds, m.metrics.dropped, m.metrics.underruns,
		m.metrics.loadFailures, gauges{m},
	} {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("stream-bridge: registering metrics: %w", err)
		}
	}
	return nil
}

// setDocument makes d the streams the bridge plays from. Calls already
// playing keep their sessions (BR.3 rule 7); what the bridge remembers of
// a stream that is gone is forgotten.
func (m *manager) setDocument(d streams.Document) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Version != m.doc.Version || m.loadedAt == nil {
		now := m.clock.Now()
		m.loadedAt = &now
	}
	m.doc = d
	m.pools = map[string]int{}
	for _, p := range d.Pools {
		m.pools[p.Name] = p.Max
	}
	m.streamsErr = ""
	for id := range m.memory {
		if _, ok := d.Streams[id]; !ok {
			delete(m.memory, id)
		}
	}
}

// setStreamsError reports why the newest streams.json could not be loaded.
func (m *manager) setStreamsError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamsErr = msg
}

// setFFmpeg records the decoder's version line, or why it could not be
// had.
func (m *manager) setFFmpeg(v, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ffmpeg, m.ffmpegErr = v, errMsg
}

// status is what readiness needs: whether a streams.json is loaded, why
// the newest is not, the decoder's version line and why it is missing.
func (m *manager) status() (loaded bool, streamsErr, ffmpeg, ffmpegErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doc.Version != "", m.streamsErr, m.ffmpeg, m.ffmpegErr
}

// openCall counts a new AudioSocket connection, unless the bridge holds
// MaxCalls or is stopping.
func (m *manager) openCall() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed:
		return errStopping
	case m.calls >= MaxCalls:
		m.full++
		m.metrics.refused.WithLabelValues(refusedFull).Inc()
		return errBridgeFull
	}
	m.calls++
	return nil
}

// closeCall counts a connection closed.
func (m *manager) closeCall() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls--
}

// join joins a call on the stream with UUID id to its source's session,
// starting one if none runs (BR.3 rules 1-3), or refuses it:
// errUnknownStream, a *busyError, errBridgeFull or errStopping.
func (m *manager) join(id string) (*caller, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errStopping
	}
	now := m.clock.Now()
	st, ok := m.doc.Streams[id]
	if !ok {
		m.unknown++
		m.metrics.refused.WithLabelValues(refusedUnknown).Inc()
		return nil, errUnknownStream
	}
	mem := m.mem(id)
	mem.calls++
	c := &caller{
		uuid: id, name: st.Name, number: st.Number, ended: make(chan struct{}),
		limit: time.Duration(m.doc.CallLimitSeconds) * time.Second,
	}
	if s := m.sessions[st.Source]; s != nil {
		m.attach(s, c, now)
		return c, nil
	}

	var preempt *session
	if st.Pool != "" {
		var held []*session
		for _, s := range m.sessions {
			if s.pool == st.Pool {
				held = append(held, s)
			}
		}
		if max := m.pools[st.Pool]; len(held) >= max {
			for _, s := range held {
				if s.state == api.SessionDraining && (preempt == nil || s.drainedAt.Before(preempt.drainedAt)) {
					preempt = s
				}
			}
			if preempt == nil {
				msg := busyMessage(st.Pool, max, held)
				if !mem.sourceFailing() { // a refusal never hides that the source is broken
					mem.lastFailure = &api.Failure{At: now, Reason: api.FailureTunerBusy, Message: msg}
				}
				m.metrics.refused.WithLabelValues(refusedTunerBusy).Inc()
				m.log.Info("call refused: the tuner pool is busy", "stream", st.Name, "number", st.Number, "pool", st.Pool, "message", msg)
				return nil, &busyError{msg: msg}
			}
		}
	}
	if n := len(m.sessions); n >= MaxSessions && (preempt == nil || n-1 >= MaxSessions) {
		m.full++
		m.metrics.refused.WithLabelValues(refusedFull).Inc()
		m.log.Info("call refused: the bridge runs its most sessions", "stream", st.Name, "number", st.Number, "sessions", n)
		return nil, errBridgeFull
	}
	if preempt != nil {
		m.log.Info("session stopped", "source", preempt.source, "pool", preempt.pool, "reason", "its tuner is needed for "+st.Source)
		m.stopFor(preempt, endStopping, endedPreempted)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		source: st.Source, pool: st.Pool, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), firstAudio: make(chan struct{}),
		state: api.SessionStarting, startedAt: now,
		callers: map[*caller]struct{}{}, streams: map[string]struct{}{},
	}
	if st.Pool != "" {
		s.waitFor = slices.Clone(m.stopping[st.Pool])
	}
	m.sessions[st.Source] = s
	m.attach(s, c, now)
	m.wg.Add(1)
	go m.run(s)
	return c, nil
}

// busyMessage is a TunerBusy failure's text: "pool hdhomerun (1 at a
// time) is playing kiro7 (707)", naming the streams the pool's sessions'
// callers are on, sorted.
func busyMessage(pool string, max int, held []*session) string {
	var names []string
	for _, s := range held {
		for c := range s.callers {
			names = append(names, c.name+" ("+c.number+")")
		}
		if len(s.callers) == 0 {
			names = append(names, s.source)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return printable(fmt.Sprintf("pool %s (%d at a time) is playing %s", pool, max, strings.Join(names, ", ")), api.MaxMessageBytes)
}

// attach puts c in s; a Draining session plays on.
func (m *manager) attach(s *session, c *caller, now time.Time) {
	c.sess = s
	s.callers[c] = struct{}{}
	s.streams[c.uuid] = struct{}{}
	m.listeners[c.uuid]++
	if s.state == api.SessionDraining {
		close(s.graceStop)
		s.graceStop = nil
		s.state = api.SessionStarting
		if s.firstAudioAt != nil {
			s.state = api.SessionPlaying
		}
	}
	if s.firstAudioAt != nil {
		m.mem(c.uuid).lastPlayed = &now
	}
}

// detach takes c out of its session.
func (m *manager) detach(c *caller) {
	delete(c.sess.callers, c)
	if m.listeners[c.uuid]--; m.listeners[c.uuid] <= 0 {
		delete(m.listeners, c.uuid)
	}
}

// leave ends a call the caller (or the connection) ended. The session's
// last caller leaving starts its grace (BR.3 rule 6).
func (m *manager) leave(c *caller) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.left {
		return
	}
	c.left = true
	m.detach(c)
	if s := c.sess; !s.stopped && len(s.callers) == 0 {
		m.drain(s)
	}
}

// drain makes s Draining and stops it Grace from now unless a caller joins.
func (m *manager) drain(s *session) {
	s.state = api.SessionDraining
	s.drainedAt = m.clock.Now()
	stop := make(chan struct{})
	s.graceStop = stop
	t := m.clock.NewTimer(Grace)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer t.Stop()
		select {
		case <-t.C():
			m.mu.Lock()
			defer m.mu.Unlock()
			if s.graceStop == stop && !s.stopped {
				m.log.Info("session stopped", "source", s.source, "pool", s.pool, "reason", "no caller for "+Grace.String())
				m.stopFor(s, endStopping, endedIdle)
			}
		case <-stop:
		}
	}()
}

// stopFor stops s, counting why it ended (session_ends_total) the first
// time.
func (m *manager) stopFor(s *session, why endReason, cause string) {
	if s.stopped {
		return
	}
	m.metrics.ends.WithLabelValues(cause).Inc()
	m.stop(s, why)
}

// stop stops s: no new caller joins it, its decoder's context ends, and
// every caller still in it is ended with why.
func (m *manager) stop(s *session, why endReason) {
	if s.stopped {
		return
	}
	s.stopped = true
	if m.sessions[s.source] == s {
		delete(m.sessions, s.source)
	}
	if s.graceStop != nil {
		close(s.graceStop)
		s.graceStop = nil
	}
	s.cancel()
	if s.pool != "" {
		m.stopping[s.pool] = append(m.stopping[s.pool], s)
	}
	for c := range s.callers {
		m.detach(c)
		c.left, c.why = true, why
		close(c.ended)
	}
}

// fail ends s with a failure (BR.3 rule 5): its callers are sent away and
// each stream that had a caller in it remembers the failure, unless s was
// Draining (nobody heard it).
func (m *manager) fail(s *session, reason api.FailureReason, msg string) {
	if s.stopped {
		return
	}
	if s.state != api.SessionDraining {
		for id := range s.streams {
			if _, loaded := m.doc.Streams[id]; loaded {
				m.mem(id).lastFailure = &api.Failure{At: m.clock.Now(), Reason: reason, Message: msg}
			}
		}
	}
	if reason == api.FailureSourceFailed {
		m.metrics.starts.WithLabelValues(resultFailed).Inc()
	}
	m.log.Info("session failed", "source", s.source, "pool", s.pool, "reason", reason, "message", msg,
		"state", s.state, "callers", len(s.callers))
	why, cause := endSourceEnded, endedSourceEnded
	if reason == api.FailureSourceFailed {
		why, cause = endSourceFailed, endedSourceFailed
	}
	m.stopFor(s, why, cause)
}

// run is a session's life: wait for the pool's tuner to settle, start the
// decoder, hand its frames to the callers, watch it start and stall, and
// tell the manager when it ends.
func (m *manager) run(s *session) {
	defer m.wg.Done()
	defer m.finish(s)
	if !m.settle(s) {
		return
	}
	watchdog := m.clock.NewTimer(StartTimeout)
	defer watchdog.Stop()
	dec, err := m.source.Start(s.ctx, s.source)
	if err != nil {
		m.mu.Lock()
		if s.ctx.Err() == nil {
			m.fail(s, api.FailureSourceFailed, printable(err.Error(), api.MaxMessageBytes))
		}
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	s.decoderRan, s.startedAt = true, m.clock.Now()
	m.mu.Unlock()
	m.log.Info("session started", "source", s.source, "pool", s.pool)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		m.read(s, dec)
	}()
	firstAudio := s.firstAudio
loop:
	for {
		select {
		case <-firstAudio:
			firstAudio = nil
			watchdog.Reset(m.untilStall(s))
		case <-watchdog.C():
			next, ok := m.watch(s)
			if !ok {
				break loop
			}
			watchdog.Reset(next)
		case <-readDone:
			m.ended(s, dec.Wait())
			return
		case <-s.ctx.Done():
			break loop
		}
	}
	<-readDone
	_ = dec.Wait()
}

// settle waits, for a pool's session, until the pool's stopped decoders
// have ended and PreemptSettle has passed since the last of them did, so
// the tuner device has freed its tuner. It reports false if s was stopped
// meanwhile.
func (m *manager) settle(s *session) bool {
	if s.pool == "" {
		return true
	}
	for _, w := range s.waitFor {
		select {
		case <-w.done:
		case <-s.ctx.Done():
			return false
		}
	}
	m.mu.Lock()
	freed := m.freedAt[s.pool]
	m.mu.Unlock()
	if freed.IsZero() {
		return true
	}
	d := freed.Add(PreemptSettle).Sub(m.clock.Now())
	if d <= 0 {
		return true
	}
	t := m.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return s.ctx.Err() == nil
	case <-s.ctx.Done():
		return false
	}
}

// read hands the decoder's frames to the session until the decoder ends.
// After the session stops it keeps reading (and dropping) to the end, so
// the decoder is never blocked writing while it is being stopped.
func (m *manager) read(s *session, dec Decoder) {
	for {
		f := make([]byte, streams.FrameBytes)
		if _, err := io.ReadFull(dec, f); err != nil {
			return
		}
		m.deliver(s, f)
	}
}

// deliver queues one frame for each of s's callers (BR.3 rule 4). The
// first makes s Playing.
func (m *manager) deliver(s *session, f []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.stopped {
		return
	}
	now := m.clock.Now()
	s.lastFrameAt = now
	if s.firstAudioAt == nil {
		s.firstAudioAt = &now
		s.playing.Store(true)
		if s.state == api.SessionStarting {
			s.state = api.SessionPlaying
		}
		close(s.firstAudio)
		for c := range s.callers {
			m.mem(c.uuid).lastPlayed = &now
		}
		m.metrics.starts.WithLabelValues(resultPlayed).Inc()
		m.log.Info("session playing", "source", s.source, "pool", s.pool, "after", now.Sub(s.startedAt).String())
	}
	for c := range s.callers {
		if c.queue.push(f) {
			m.metrics.dropped.Inc()
		}
	}
}

// untilStall is how long until s has had no frame for StallTimeout.
func (m *manager) untilStall(s *session) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.lastFrameAt.Add(StallTimeout).Sub(m.clock.Now())
}

// watch checks that s has had audio within StartTimeout of its start and
// no gap of StallTimeout since, failing it otherwise. It reports when to
// look again, and false once s is over.
func (m *manager) watch(s *session) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case s.stopped:
		return 0, false
	case s.firstAudioAt == nil:
		m.fail(s, api.FailureSourceFailed, "no audio came from the source within "+StartTimeout.String())
		return 0, false
	}
	idle := m.clock.Since(s.lastFrameAt)
	if idle >= StallTimeout {
		m.fail(s, api.FailureSourceEnded, "no audio came from the source for "+StallTimeout.String())
		return 0, false
	}
	return StallTimeout - idle, true
}

// ended is the decoder ending by itself (or because s was stopped): a
// failure before any audio is SourceFailed, after it SourceEnded.
func (m *manager) ended(s *session, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.stopped || s.ctx.Err() != nil {
		return
	}
	msg := "the source ended"
	if err != nil {
		msg = printable(err.Error(), api.MaxMessageBytes)
	}
	reason := api.FailureSourceEnded
	if s.firstAudioAt == nil {
		reason = api.FailureSourceFailed
	}
	m.fail(s, reason, msg)
}

// finish records that s's decoder has ended (and so freed its tuner).
func (m *manager) finish(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop(s, endStopping)
	if s.pool != "" {
		m.stopping[s.pool] = slices.DeleteFunc(m.stopping[s.pool], func(x *session) bool { return x == s })
		if len(m.stopping[s.pool]) == 0 {
			delete(m.stopping, s.pool)
		}
		if s.decoderRan {
			m.freedAt[s.pool] = m.clock.Now()
		}
	}
	close(s.done)
}

// stopAll stops every session and refuses new calls, then waits for every
// decoder to end.
func (m *manager) stopAll() {
	m.mu.Lock()
	m.closed = true
	for _, s := range m.sessions {
		m.stopFor(s, endStopping, endedStopping)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// mem is what the bridge remembers of the stream with UUID id.
func (m *manager) mem(id string) *memory {
	mem := m.memory[id]
	if mem == nil {
		mem = &memory{}
		m.memory[id] = mem
	}
	return mem
}

// observations is the control API's document (BR.6).
func (m *manager) observations() api.Observations {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := api.Observations{
		GeneratedAt: m.clock.Now(),
		Bridge: api.BridgeStatus{
			Version: version.Version, StartedAt: m.started, FFmpeg: m.ffmpeg,
			StreamsVersion: m.doc.Version, StreamsLoadedAt: copyTime(m.loadedAt), StreamsError: m.streamsErr,
			Calls: m.calls, UnknownCalls: m.unknown, FullCalls: m.full,
		},
		Streams: []api.StreamObservations{},
		Pools:   []api.PoolObservations{},
	}
	ids := slices.SortedFunc(maps.Keys(m.doc.Streams), func(a, b string) int {
		return cmp.Compare(m.doc.Streams[a].Name, m.doc.Streams[b].Name)
	})
	bySource := map[string][]string{}
	for _, id := range ids {
		st := m.doc.Streams[id]
		bySource[st.Source] = append(bySource[st.Source], st.Name)
		so := api.StreamObservations{
			UUID: id, Name: st.Name, Number: st.Number, Source: st.Source, Pool: st.Pool,
			Listeners: m.listeners[id],
		}
		if s := m.sessions[st.Source]; s != nil {
			so.Session = &api.SessionObservations{
				State: s.state, StartedAt: s.startedAt, FirstAudioAt: copyTime(s.firstAudioAt), Listeners: len(s.callers),
			}
		}
		if mem := m.memory[id]; mem != nil {
			so.LastPlayed, so.Calls = copyTime(mem.lastPlayed), mem.calls
			if mem.lastFailure != nil {
				f := *mem.lastFailure
				so.LastFailure = &f
			}
		}
		o.Streams = append(o.Streams, so)
	}
	sources := slices.Sorted(maps.Keys(m.sessions))
	for _, p := range m.doc.Pools {
		po := api.PoolObservations{Name: p.Name, Max: p.Max, Sessions: []api.PoolSession{}}
		for _, src := range sources {
			if s := m.sessions[src]; s.pool == p.Name {
				po.Sessions = append(po.Sessions, api.PoolSession{
					Source: src, Streams: append([]string{}, bySource[src]...), State: s.state, Listeners: len(s.callers),
				})
			}
		}
		o.Pools = append(o.Pools, po)
	}
	return o
}

// gaugeValues are the gauges' values: open calls per loaded stream name,
// sessions per pool (every loaded pool, and noPool), the loaded version.
func (m *manager) gaugeValues() (calls, sessions map[string]int, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls = map[string]int{}
	for id, st := range m.doc.Streams {
		calls[st.Name] = m.listeners[id]
	}
	sessions = map[string]int{noPool: 0}
	for _, p := range m.doc.Pools {
		sessions[p.Name] = 0
	}
	for _, s := range m.sessions {
		pool := cmp.Or(s.pool, noPool)
		sessions[pool]++
	}
	return calls, sessions, m.doc.Version
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
