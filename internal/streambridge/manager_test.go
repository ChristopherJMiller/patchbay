package streambridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// sessionState is the state of source's session, "" without one.
func sessionState(m *manager, source string) api.SessionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[source]; s != nil {
		return s.state
	}
	return ""
}

// settled reports whether every stopped session's decoder has ended.
func settled(m *manager) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.stopping {
		if len(l) > 0 {
			return false
		}
	}
	return true
}

func TestOneSessionPerSource(t *testing.T) {
	src := newFakeSource()
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, king5Spare))
	a, b := join(t, m, king5), join(t, m, king5Spare)
	if a.sess != b.sess {
		t.Fatal("two streams with one source have two sessions")
	}
	eventually(t, "playing", func() bool { return sessionState(m, srcKing) == api.SessionPlaying })
	if n := src.total(); n != 1 {
		t.Errorf("%d decoders started, want 1", n)
	}
	o := streamObs(t, m, "king5")
	if o.Listeners != 1 || o.Session == nil || o.Session.Listeners != 2 || o.Session.State != api.SessionPlaying {
		t.Errorf("king5: %+v, session %+v", o, o.Session)
	}
	if pools := m.observations().Pools; len(pools) != 1 || len(pools[0].Sessions) != 1 ||
		!slices.Equal(pools[0].Sessions[0].Streams, []string{"king5", "king5-spare"}) || pools[0].Sessions[0].Listeners != 2 {
		t.Errorf("pools: %+v", pools)
	}
}

func TestFanOut(t *testing.T) {
	src := newFakeSource().set(srcRadio, numbered(20))
	m, _ := testManager(t, src, nil, doc(t, nil, groovesalad))
	callers := []*caller{join(t, m, groovesalad), join(t, m, groovesalad), join(t, m, groovesalad)}
	for i, c := range callers {
		got := popN(t, c, 20)
		for n, f := range got {
			if frameNumber(f) != n {
				t.Fatalf("caller %d: frame %d is %d", i, n, frameNumber(f))
			}
		}
	}
}

func TestSlowCallerDropsOldest(t *testing.T) {
	feed := make(chan []byte)
	src := newFakeSource().set(srcRadio, fed(feed))
	m, reg := testManager(t, src, nil, doc(t, nil, groovesalad))
	fast1, fast2, slow := join(t, m, groovesalad), join(t, m, groovesalad), join(t, m, groovesalad)
	const sent = QueueFrames + 30
	var got1, got2 []int
	for i := range sent {
		feed <- numberedFrame(i)
		eventually(t, "the fast callers", func() bool {
			for _, p := range []struct {
				c   *caller
				got *[]int
			}{{fast1, &got1}, {fast2, &got2}} {
				for f, ok := p.c.queue.pop(); ok; f, ok = p.c.queue.pop() {
					*p.got = append(*p.got, frameNumber(f))
				}
			}
			return len(got1) == i+1 && len(got2) == i+1
		})
	}
	for _, got := range [][]int{got1, got2} {
		for n, f := range got {
			if f != n {
				t.Fatalf("a fast caller's frame %d is %d", n, f)
			}
		}
	}
	var queued []int
	for f, ok := slow.queue.pop(); ok; f, ok = slow.queue.pop() {
		queued = append(queued, frameNumber(f))
	}
	if len(queued) != QueueFrames || queued[0] != sent-QueueFrames || queued[len(queued)-1] != sent-1 {
		t.Errorf("the slow caller holds %d frames, %v…, want the newest %d", len(queued), queued[:min(3, len(queued))], QueueFrames)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_frames_dropped_total", ""); v != sent-QueueFrames {
		t.Errorf("frames dropped %v, want %d", v, sent-QueueFrames)
	}
}

func TestGrace(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource()
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5))
	c := join(t, m, king5)
	ctx := src.decoderCtx(t, srcKing)
	eventually(t, "playing", func() bool { return sessionState(m, srcKing) == api.SessionPlaying })

	m.leave(c)
	if st := sessionState(m, srcKing); st != api.SessionDraining {
		t.Fatalf("after the last caller left: %q, want Draining", st)
	}
	clk.Step(Grace - time.Millisecond)
	never(t, "the decoder stopped before Grace", func() bool { return done(ctx) })
	c = join(t, m, king5) // a join at Grace - 1 ms keeps it
	if st := sessionState(m, srcKing); st != api.SessionPlaying {
		t.Fatalf("after a join while Draining: %q, want Playing", st)
	}
	clk.Step(time.Millisecond)
	never(t, "the decoder stopped although a caller joined", func() bool { return done(ctx) })

	m.leave(c)
	clk.Step(Grace - time.Millisecond)
	never(t, "the decoder stopped before Grace after the second leave", func() bool { return done(ctx) })
	clk.Step(time.Millisecond)
	eventually(t, "the decoder to stop at Grace", func() bool { return done(ctx) })
	if st := sessionState(m, srcKing); st != "" {
		t.Errorf("session %q after Grace, want none", st)
	}
	if src.started(srcKing) != 1 {
		t.Errorf("%d decoders", src.started(srcKing))
	}
	if o := streamObs(t, m, "king5"); o.LastFailure != nil || o.Session != nil {
		t.Errorf("after Grace: %+v", o)
	}
}

func TestDrainingBeforeAudioGoesBackToStarting(t *testing.T) {
	src := newFakeSource().set(srcKing, silent())
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5))
	m.leave(join(t, m, king5))
	join(t, m, king5)
	if st := sessionState(m, srcKing); st != api.SessionStarting {
		t.Errorf("%q, want Starting", st)
	}
}

// failure is a stream's LastFailure, waiting for one.
func failure(t *testing.T, m *manager, name string) api.Failure {
	t.Helper()
	var f *api.Failure
	eventually(t, name+"'s failure", func() bool { f = streamObs(t, m, name).LastFailure; return f != nil })
	return *f
}

func TestStartFails(t *testing.T) {
	src := newFakeSource().failStart(srcKing, errors.New("starting ffmpeg: fork/exec /usr/bin/ffmpeg: no such file or directory"))
	m, reg := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5))
	c := join(t, m, king5)
	eventually(t, "the call to end", func() bool { return ended(c) })
	f := failure(t, m, "king5")
	if f.Reason != api.FailureSourceFailed || !strings.Contains(f.Message, "no such file") || !f.At.Equal(epoch) {
		t.Errorf("failure %+v", f)
	}
	if st := sessionState(m, srcKing); st != "" {
		t.Errorf("session %q", st)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_session_starts_total", "result=failed"); v != 1 {
		t.Errorf("failed starts %v", v)
	}
}

func TestExitBeforeAudio(t *testing.T) {
	why := errors.New("ffmpeg exited with status 8: Server returned 5XX Server Error reply")
	src := newFakeSource().set(srcKing, failAfter(0, why))
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5))
	c := join(t, m, king5)
	eventually(t, "the call to end", func() bool { return ended(c) })
	if f := failure(t, m, "king5"); f.Reason != api.FailureSourceFailed || f.Message != why.Error() {
		t.Errorf("failure %+v", f)
	}
	if c.why != endSourceFailed {
		t.Errorf("the call ended %q", c.why)
	}
}

func TestStartTimeout(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource().set(srcKing, silent())
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5))
	c := join(t, m, king5)
	ctx := src.decoderCtx(t, srcKing)
	clk.Step(StartTimeout - time.Millisecond)
	never(t, "the session failed before StartTimeout", func() bool { return ended(c) })
	clk.Step(time.Millisecond)
	eventually(t, "the call to end", func() bool { return ended(c) })
	f := failure(t, m, "king5")
	if f.Reason != api.FailureSourceFailed || f.Message != "no audio came from the source within 15s" {
		t.Errorf("failure %+v", f)
	}
	eventually(t, "the decoder to stop", func() bool { return done(ctx) })
}

func TestStall(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource().set(srcKing, numbered(3))
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5))
	c := join(t, m, king5)
	popN(t, c, 3)
	clk.Step(StallTimeout - time.Millisecond)
	never(t, "the session failed before StallTimeout", func() bool { return ended(c) })
	clk.Step(time.Millisecond)
	eventually(t, "the call to end", func() bool { return ended(c) })
	if f := failure(t, m, "king5"); f.Reason != api.FailureSourceEnded || f.Message != "no audio came from the source for 10s" {
		t.Errorf("failure %+v", f)
	}
}

func TestEndsWhilePlaying(t *testing.T) {
	feed := make(chan []byte)
	src := newFakeSource().set(srcKing, fed(feed))
	m, reg := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, king5Spare))
	a, b := join(t, m, king5), join(t, m, king5Spare)
	feed <- numberedFrame(0)
	popN(t, a, 1)
	close(feed)
	eventually(t, "both calls to end", func() bool { return ended(a) && ended(b) })
	for _, name := range []string{"king5", "king5-spare"} {
		if f := failure(t, m, name); f.Reason != api.FailureSourceEnded || f.Message != "the feed ended" {
			t.Errorf("%s: %+v", name, f)
		}
	}
	if a.why != endSourceEnded {
		t.Errorf("ended %q", a.why)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_session_starts_total", "result=played"); v != 1 {
		t.Errorf("played starts %v", v)
	}
	if o := streamObs(t, m, "king5"); o.Listeners != 0 || o.Session != nil {
		t.Errorf("after the end: %+v", o)
	}
}

func TestPoolLimit(t *testing.T) {
	src := newFakeSource()
	m, reg := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	holder := join(t, m, king5)
	popN(t, holder, 1)
	_, err := m.join(streams.UUID("kiro7"))
	var busy *busyError
	if !errors.As(err, &busy) {
		t.Fatalf("a second source in a max-1 pool: %v, want TunerBusy", err)
	}
	want := "pool hdhomerun (1 at a time) is playing king5 (705)"
	if f := failure(t, m, "kiro7"); f.Reason != api.FailureTunerBusy || f.Message != want || busy.Error() != want {
		t.Errorf("failure %+v, error %q", f, busy)
	}
	if src.started(srcKiro) != 0 {
		t.Error("the refused source started")
	}
	popN(t, holder, 10) // the holder plays on
	if ended(holder) || streamObs(t, m, "king5").LastFailure != nil {
		t.Error("the holder was disturbed")
	}
	if o := streamObs(t, m, "kiro7"); o.Calls != 1 || o.Listeners != 0 {
		t.Errorf("kiro7: %+v", o)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_refused_calls_total", "reason=tuner_busy"); v != 1 {
		t.Errorf("refused tuner_busy %v", v)
	}
}

func TestPoolLimitNamesEveryHolder(t *testing.T) {
	pool := streams.Pool{Name: "hdhomerun", Max: 2}
	m, _ := testManager(t, newFakeSource(), nil, doc(t, []streams.Pool{pool}, king5, king5Spare, kiro7, komo4))
	join(t, m, kiro7)
	join(t, m, king5Spare)
	join(t, m, king5)
	join(t, m, king5)
	_, err := m.join(streams.UUID("komo4"))
	if want := "pool hdhomerun (2 at a time) is playing king5 (705), king5-spare (715), kiro7 (707)"; err == nil || err.Error() != want {
		t.Errorf("%v, want %q", err, want)
	}
}

func TestPoolPreemptsDraining(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource()
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	c := join(t, m, king5)
	kingCtx := src.decoderCtx(t, srcKing)
	popN(t, c, 1)
	m.leave(c)

	next := join(t, m, kiro7)
	eventually(t, "the Draining session to stop", func() bool { return done(kingCtx) })
	eventually(t, "its decoder to end", func() bool { return settled(m) })
	if st := sessionState(m, srcKiro); st != api.SessionStarting {
		t.Errorf("the new session is %q, want Starting while the tuner settles", st)
	}
	clk.Step(PreemptSettle - time.Millisecond)
	never(t, "the new source started before PreemptSettle", func() bool { return src.started(srcKiro) > 0 })
	clk.Step(time.Millisecond)
	stepUntil(t, clk, "the new source to start", func() bool { return src.started(srcKiro) == 1 })
	popN(t, next, 1)
	if o := streamObs(t, m, "king5"); o.LastFailure != nil {
		t.Errorf("the preempted stream has a failure: %+v", o.LastFailure)
	}
}

func TestPoolWaitsForAStoppingDecoder(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource()
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	m.leave(join(t, m, king5))
	src.decoderCtx(t, srcKing)
	clk.Step(Grace) // king5's decoder is stopped
	eventually(t, "its decoder to end", func() bool { return settled(m) && sessionState(m, srcKing) == "" })
	join(t, m, kiro7)
	never(t, "a pool source started within PreemptSettle of the last one's end", func() bool { return src.started(srcKiro) > 0 })
	clk.Step(PreemptSettle)
	stepUntil(t, clk, "the new source to start", func() bool { return src.started(srcKiro) == 1 })

	// Radio has no pool and never waits.
	m.setDocument(doc(t, []streams.Pool{hdhomerun}, king5, kiro7, groovesalad))
	join(t, m, groovesalad)
	eventually(t, "radio to start", func() bool { return src.started(srcRadio) == 1 })
}

func TestPoolRace(t *testing.T) {
	src := newFakeSource()
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	var wg sync.WaitGroup
	var mu sync.Mutex
	joined := map[string]int{}
	refused := map[string]int{}
	for i := range 100 {
		s := []streams.Stream{king5, kiro7}[i%2]
		wg.Go(func() {
			_, err := m.join(streams.UUID(s.Name))
			mu.Lock()
			defer mu.Unlock()
			var busy *busyError
			switch {
			case err == nil:
				joined[s.Name]++
			case errors.As(err, &busy):
				refused[s.Name]++
			default:
				t.Errorf("join: %v", err)
			}
		})
	}
	wg.Wait()
	eventually(t, "a decoder", func() bool { return src.total() == 1 })
	never(t, "a second decoder", func() bool { return src.total() > 1 })
	won := src.starts[0]
	winner, loser := "king5", "kiro7"
	if won == srcKiro {
		winner, loser = loser, winner
	}
	if joined[winner] != 50 || refused[winner] != 0 || joined[loser] != 0 || refused[loser] != 50 {
		t.Errorf("joined %v, refused %v", joined, refused)
	}
}

func TestRadioHasNoLimit(t *testing.T) {
	src := newFakeSource()
	var list []streams.Stream
	for i := range MaxSessions + 1 {
		list = append(list, streams.Stream{Name: "radio-" + string(rune('a'+i)), Number: "75" + string(rune('0'+i)), Source: srcRadio + "?n=" + string(rune('a'+i))})
	}
	m, reg := testManager(t, src, nil, doc(t, nil, list...))
	for _, s := range list[:MaxSessions] {
		join(t, m, s)
	}
	eventually(t, "every radio playing", func() bool { return src.total() == MaxSessions })
	if _, err := m.join(streams.UUID(list[MaxSessions].Name)); !errors.Is(err, errBridgeFull) {
		t.Errorf("a session over MaxSessions: %v", err)
	}
	if o := m.observations(); o.Bridge.FullCalls != 1 {
		t.Errorf("full calls %d", o.Bridge.FullCalls)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_sessions", "pool=none"); v != MaxSessions {
		t.Errorf("sessions without a pool %v", v)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_refused_calls_total", "reason=bridge_full"); v != 1 {
		t.Errorf("refused bridge_full %v", v)
	}
}

func TestUnknownStream(t *testing.T) {
	m, reg := testManager(t, newFakeSource(), nil, doc(t, nil, groovesalad))
	if _, err := m.join(streams.UUID("nope")); !errors.Is(err, errUnknownStream) {
		t.Errorf("%v", err)
	}
	if m.observations().Bridge.UnknownCalls != 1 || gauge(t, reg, "patchbay_stream_bridge_refused_calls_total", "reason=unknown_stream") != 1 {
		t.Error("the unknown call was not counted")
	}
}

func TestReloadKeepsCalls(t *testing.T) {
	src := newFakeSource()
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, kiro7))
	c := join(t, m, king5)
	popN(t, c, 1)
	// king5 is gone, kiro7 stays: king5's call plays on, its session still
	// holds the pool's tuner, and the stream has no observations.
	m.setDocument(doc(t, []streams.Pool{hdhomerun}, kiro7))
	popN(t, c, 5)
	if ended(c) {
		t.Fatal("a reload ended a call")
	}
	if _, err := m.join(streams.UUID("king5")); !errors.Is(err, errUnknownStream) {
		t.Errorf("a call on a removed stream: %v", err)
	}
	_, err := m.join(streams.UUID("kiro7"))
	if want := "pool hdhomerun (1 at a time) is playing king5 (705)"; err == nil || err.Error() != want {
		t.Errorf("%v, want %q", err, want)
	}
	for _, s := range m.observations().Streams {
		if s.Name == "king5" {
			t.Errorf("a removed stream is observed: %+v", s)
		}
	}
	// A changed source applies to new calls only.
	moved := kiro7
	moved.Source = srcKomo
	m.setDocument(doc(t, []streams.Pool{hdhomerun}, moved))
	m.leave(c)
	if st := sessionState(m, srcKing); st != api.SessionDraining {
		t.Errorf("king5's session after its last caller left: %q", st)
	}
}

func TestLastPlayed(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	feed := make(chan []byte)
	src := newFakeSource().set(srcKing, fed(feed))
	m, _ := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5, king5Spare))
	a := join(t, m, king5)
	if o := streamObs(t, m, "king5"); o.LastPlayed != nil || o.Session == nil || o.Session.State != api.SessionStarting {
		t.Fatalf("before any audio: %+v", o)
	}
	clk.Step(2 * time.Second)
	feed <- numberedFrame(0)
	popN(t, a, 1)
	first := epoch.Add(2 * time.Second)
	if o := streamObs(t, m, "king5"); o.LastPlayed == nil || !o.LastPlayed.Equal(first) || o.Session.FirstAudioAt == nil || !o.Session.FirstAudioAt.Equal(first) {
		t.Errorf("after the first audio: %+v %+v", o, o.Session)
	}
	if o := streamObs(t, m, "king5-spare"); o.LastPlayed != nil {
		t.Errorf("king5-spare played without a caller: %v", o.LastPlayed)
	}
	clk.Step(time.Second)
	join(t, m, king5Spare)
	if o := streamObs(t, m, "king5-spare"); o.LastPlayed == nil || !o.LastPlayed.Equal(first.Add(time.Second)) {
		t.Errorf("joining a playing session: %+v", o)
	}
}

func TestStopAll(t *testing.T) {
	src := newFakeSource()
	m, _ := testManager(t, src, nil, doc(t, []streams.Pool{hdhomerun}, king5, groovesalad))
	a, b := join(t, m, king5), join(t, m, groovesalad)
	ctxs := []context.Context{src.decoderCtx(t, srcKing), src.decoderCtx(t, srcRadio)}
	m.stopAll()
	for _, c := range []*caller{a, b} {
		if !ended(c) || c.why != endStopping {
			t.Errorf("a call after stopAll: ended %v, %q", ended(c), c.why)
		}
	}
	for _, ctx := range ctxs {
		if !done(ctx) {
			t.Error("a decoder runs after stopAll")
		}
	}
	if _, err := m.join(streams.UUID("king5")); !errors.Is(err, errStopping) {
		t.Errorf("a join after stopAll: %v", err)
	}
	m.stopAll() // twice is fine
}

// TestTunerBusyKeepsSourceFailure: a TunerBusy refusal never replaces a
// source failure no call has played past, so a stream whose source is
// broken stays Failing in status after the pool frees up (its source was
// never tried again); the refusal is still counted. After a play, a
// refusal is the newest failure as before.
func TestTunerBusyKeepsSourceFailure(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	src := newFakeSource().failStart(srcKiro, errors.New("ffmpeg exited with status 8: Server returned 404 Not Found"))
	m, reg := testManager(t, src, clk, doc(t, []streams.Pool{hdhomerun}, king5, kiro7, komo4))
	broken := join(t, m, kiro7)
	eventually(t, "kiro7's source to fail", func() bool { return ended(broken) })
	if f := failure(t, m, "kiro7"); f.Reason != api.FailureSourceFailed {
		t.Fatalf("kiro7's failure %+v, want SourceFailed", f)
	}

	clk.Step(time.Minute)
	holder := join(t, m, king5)
	popN(t, holder, 1)
	for _, s := range []streams.Stream{kiro7, komo4} {
		var busy *busyError
		if _, err := m.join(streams.UUID(s.Name)); !errors.As(err, &busy) {
			t.Fatalf("%s while king5 holds the pool: %v, want TunerBusy", s.Name, err)
		}
	}
	if f := failure(t, m, "kiro7"); f.Reason != api.FailureSourceFailed || !strings.Contains(f.Message, "404") {
		t.Errorf("kiro7's failure after a refusal: %+v, want its source's 404 kept", f)
	}
	if f := failure(t, m, "komo4"); f.Reason != api.FailureTunerBusy {
		t.Errorf("komo4's failure %+v, want TunerBusy", f)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_refused_calls_total", "reason=tuner_busy"); v != 2 {
		t.Errorf("refused tuner_busy %v, want both refusals", v)
	}

	// Once the stream has played since its failure, a refusal is recorded.
	m.mu.Lock()
	played := clk.Now()
	m.mem(streams.UUID("kiro7")).lastPlayed = &played
	m.mu.Unlock()
	clk.Step(time.Second)
	if _, err := m.join(streams.UUID("kiro7")); err == nil {
		t.Fatal("kiro7 joined a full pool")
	}
	if f := failure(t, m, "kiro7"); f.Reason != api.FailureTunerBusy {
		t.Errorf("a refusal after a play: %+v, want TunerBusy", f)
	}
}
