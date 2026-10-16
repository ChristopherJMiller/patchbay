package streambridge

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/audiosocket"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// The AudioSocket server (docs/plans/release-b.md 6.1 BR.4, §7.1).
//
// Asterisk's Dial(AudioSocket/…/c(slin16)) connects, sends one KindUUID
// frame, then the caller's audio (discarded) and key presses (ignored),
// and closes the connection to end the call. The bridge sends one
// KindAudio frame every 20 ms, silence when it has nothing, and a
// KindHangup frame before it closes a call it ends or refuses, so the
// dialplan goes on to "not available".

// Socket hygiene, on the real clock (the bridge's own timings use
// Options.Clock).
const (
	// writeTimeout ends a call whose connection takes no audio for this
	// long (Asterisk stopped reading).
	writeTimeout = 5 * time.Second
	// drainTimeout is how long the bridge waits, after its hangup frame,
	// for Asterisk to close (it reads what Asterisk still sends, so the
	// close is a FIN and the hangup frame is not lost to a reset).
	drainTimeout = time.Second
	// lateFrames: a caller this many frames behind its schedule (a slow
	// connection) skips ahead instead of being sent a burst.
	lateFrames = 5
)

// endCallerError is a call Asterisk ended with an error frame.
const endCallerError endReason = "the caller sent an error frame"

// server serves AudioSocket connections.
type server struct {
	m     *manager
	clock clock.WithTicker
	log   logr.Logger
	wg    sync.WaitGroup
}

// serve accepts on ln until ctx ends (then it closes ln and waits for every
// call to finish its hangup) or ln fails.
func (srv *server) serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer srv.wg.Wait()
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return errors.New("stream-bridge: the AudioSocket listener closed")
			}
			// Out of file descriptors, or the like: wait, as net/http does.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			srv.log.Error(err, "accepting an AudioSocket connection; retrying", "in", delay.String())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
			continue
		}
		delay = 0
		srv.wg.Add(1)
		go func() {
			defer srv.wg.Done()
			srv.handle(ctx, conn)
		}()
	}
}

// handle is one connection: its UUID, then its call.
func (srv *server) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := srv.m.openCall(); err != nil {
		srv.log.Info("call refused", "remote", conn.RemoteAddr().String(), "reason", err.Error())
		hangup(conn)
		return
	}
	defer srv.m.closeCall()

	id, err := srv.readID(ctx, conn)
	if err != nil {
		srv.log.V(1).Info("connection closed before a call", "remote", conn.RemoteAddr().String(), "reason", err.Error())
		return
	}
	c, err := srv.m.join(id)
	if err != nil {
		if errors.Is(err, errUnknownStream) {
			srv.log.Info("call refused: an unknown stream", "uuid", id, "remote", conn.RemoteAddr().String())
		}
		hangup(conn)
		return
	}
	start := srv.clock.Now()
	why := srv.play(ctx, conn, c)
	srv.m.leave(c)
	srv.log.Info("call ended", "stream", c.name, "number", c.number,
		"duration", srv.clock.Since(start).Round(time.Millisecond).String(), "ended", string(why))
}

// readID reads the connection's first frame, which must be a KindUUID
// frame within IDTimeout, and returns the UUID in streams.FormatUUID's
// form.
func (srv *server) readID(ctx context.Context, conn net.Conn) (string, error) {
	timer := srv.clock.NewTimer(IDTimeout)
	var mu sync.Mutex
	read := false
	stop := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
		case <-ctx.Done():
		case <-stop:
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !read {
			_ = conn.Close()
		}
	}()
	f, err := audiosocket.ReadFrame(conn)
	mu.Lock()
	read = true
	mu.Unlock()
	close(stop)
	timer.Stop()
	if err != nil {
		return "", err
	}
	u, err := audiosocket.UUIDOf(f)
	if err != nil {
		return "", err
	}
	return streams.FormatUUID(u), nil
}

// play runs a joined call until the caller, the session or the bridge
// ends it, and says which.
func (srv *server) play(ctx context.Context, conn net.Conn, c *caller) endReason {
	var readerWhy endReason
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readerWhy = discard(conn)
	}()
	why := srv.pace(ctx, conn, c, readerDone, &readerWhy)
	switch why {
	case endSourceFailed, endSourceEnded, endStopping:
		// The reader goes on dropping what Asterisk sends until it closes.
		if sendHangup(conn) {
			<-readerDone
		}
	case endCallerHungUp, endCallerError, endWriteFailed:
	}
	_ = conn.Close()
	<-readerDone
	return why
}

// pace sends c its frames on schedule (docs/plans/release-b.md §2(l)):
// frame n is due n×20 ms after the call joined, on the bridge's clock; at
// each due time the oldest queued frame goes out, or silence if there is
// none. A caller more than lateFrames behind skips ahead.
func (srv *server) pace(ctx context.Context, conn net.Conn, c *caller, readerDone <-chan struct{}, readerWhy *endReason) endReason {
	silence := audiosocket.AppendFrame(nil, audiosocket.KindAudio, make([]byte, streams.FrameBytes))
	buf := make([]byte, 0, audiosocket.HeaderSize+streams.FrameBytes)
	timer := srv.clock.NewTimer(streams.FrameDuration)
	defer timer.Stop()
	t0 := srv.clock.Now()
	for n := 0; ; n++ {
		due := t0.Add(time.Duration(n) * streams.FrameDuration)
		if wait := due.Sub(srv.clock.Now()); wait > 0 {
			timer.Reset(wait)
			select {
			case <-timer.C():
			case <-c.ended:
				return c.why
			case <-readerDone:
				return *readerWhy
			case <-ctx.Done():
				return endStopping
			}
		} else if -wait > lateFrames*streams.FrameDuration {
			n = int(srv.clock.Since(t0) / streams.FrameDuration)
		}
		select {
		case <-c.ended:
			return c.why
		case <-readerDone:
			return *readerWhy
		case <-ctx.Done():
			return endStopping
		default:
		}
		out := silence
		if f, ok := c.queue.pop(); ok {
			out = audiosocket.AppendFrame(buf[:0], audiosocket.KindAudio, f)
		} else if c.sess.playing.Load() {
			srv.m.metrics.underruns.Inc()
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := conn.Write(out); err != nil {
			return endWriteFailed
		}
	}
}

// discard reads Asterisk's frames until the call ends: audio (the
// caller's microphone), key presses and unknown kinds are dropped; a
// hangup or error frame, or the connection closing, ends the call.
func discard(r io.Reader) endReason {
	var h [audiosocket.HeaderSize]byte
	buf := make([]byte, 4096)
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return endCallerHungUp
		}
		for n := int(binary.BigEndian.Uint16(h[1:])); n > 0; {
			k, err := io.ReadFull(r, buf[:min(n, len(buf))])
			if err != nil {
				return endCallerHungUp
			}
			n -= k
		}
		if k := audiosocket.Kind(h[0]); k == audiosocket.KindHangup {
			return endCallerHungUp
		} else if k == audiosocket.KindError {
			return endCallerError
		}
	}
}

// hangup refuses a connection: a KindHangup frame, the write side closed,
// then what Asterisk still sends read (and dropped) until it closes or
// drainTimeout passes. The caller closes conn.
func hangup(conn net.Conn) {
	if !sendHangup(conn) {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(conn, 1<<20))
}

// sendHangup writes a KindHangup frame and closes the write side, so
// Asterisk reads the frame, then the end of the stream, and closes; it
// gives Asterisk drainTimeout to do so. It reports whether the frame went
// out.
func sendHangup(conn net.Conn) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(drainTimeout))
	if err := audiosocket.WriteFrame(conn, audiosocket.Frame{Kind: audiosocket.KindHangup}); err != nil {
		return false
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(drainTimeout))
	return true
}
