package fileserver

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// reporterQueue is how many events the Reporter holds while the agent is
// away or slow.
const reporterQueue = 1024

// Reporter is the file server's end of the fetch log: a sensor.FetchRecorder
// that sends each event to the agent over the unix socket layout.FetchSocket
// (sensor.WriteFetchEvent, one JSON line per event).
//
// RecordFetch never waits: a transfer to a phone must never stall on the
// agent. Events wait in a queue of reporterQueue, the oldest dropped first
// when it is full, and the drops are counted and logged. Run connects,
// reconnects about once a second while the agent is unreachable, and
// writes each event with a 1 s deadline; an event whose write failed went
// out at most in part, which the agent refuses, so it is sent again on the
// next connection. An event the agent received but had not stored when it
// restarted is lost, like the rest of its memory.
type Reporter struct {
	socket       string
	log          logr.Logger
	retry        time.Duration // between connection attempts
	writeTimeout time.Duration // per event

	mu      sync.Mutex
	queue   [reporterQueue]sensor.FetchEvent
	head, n int
	dropped uint64
	wake    chan struct{}
}

// NewReporter is a reporter to the socket at path. It does no I/O until
// Run.
func NewReporter(socket string, log logr.Logger) *Reporter {
	return &Reporter{
		socket: socket, log: log,
		retry: time.Second, writeTimeout: time.Second,
		wake: make(chan struct{}, 1),
	}
}

// RecordFetch queues e for the agent. An invalid event is dropped and
// logged: the agent would end the connection on it.
func (r *Reporter) RecordFetch(e sensor.FetchEvent) {
	if err := e.Validate(); err != nil {
		r.log.Error(err, "fetch log: dropping an invalid event", "file", e.File)
		return
	}
	r.mu.Lock()
	if r.n == reporterQueue {
		r.head = (r.head + 1) % reporterQueue
		r.n--
		r.dropped++
	}
	r.queue[(r.head+r.n)%reporterQueue] = e
	r.n++
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Dropped is how many events the queue has dropped.
func (r *Reporter) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// pop takes the oldest event.
func (r *Reporter) pop() (sensor.FetchEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return sensor.FetchEvent{}, false
	}
	e := r.queue[r.head]
	r.queue[r.head] = sensor.FetchEvent{}
	r.head = (r.head + 1) % reporterQueue
	r.n--
	return e, true
}

// unpop puts back an event pop took, as the oldest again; if the queue
// filled meanwhile it is the oldest, so it is the one dropped.
func (r *Reporter) unpop(e sensor.FetchEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == reporterQueue {
		r.dropped++
		return
	}
	r.head = (r.head - 1 + reporterQueue) % reporterQueue
	r.queue[r.head] = e
	r.n++
}

// Run sends queued events to the agent until ctx ends; it returns nil
// then.
func (r *Reporter) Run(ctx context.Context) error {
	var logged uint64 // drops already logged
	logDrops := func() {
		if d := r.Dropped(); d != logged {
			r.log.Info("fetch log: dropped events while the agent was unreachable or slow", "dropped", d)
			logged = d
		}
	}
	defer logDrops()
	unreachable := false
	for {
		conn, err := (&net.Dialer{Timeout: r.retry}).DialContext(ctx, "unix", r.socket)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !unreachable {
				r.log.Info("fetch log: the agent is unreachable; queueing events", "error", err.Error())
				unreachable = true
			}
			if !sleep(ctx, r.retry) {
				return nil
			}
			continue
		}
		if unreachable {
			r.log.Info("fetch log: connected to the agent")
			unreachable = false
		}
		err = r.pump(ctx, conn, logDrops)
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		r.log.Info("fetch log: lost the agent; reconnecting", "error", err.Error())
	}
}

// pump writes events to conn until a write fails or ctx ends.
func (r *Reporter) pump(ctx context.Context, conn net.Conn, idle func()) error {
	for {
		e, ok := r.pop()
		if !ok {
			idle()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.wake:
				continue
			}
		}
		if err := conn.SetWriteDeadline(time.Now().Add(r.writeTimeout)); err != nil {
			r.unpop(e)
			return err
		}
		if err := sensor.WriteFetchEvent(conn, e); err != nil {
			r.unpop(e)
			return err
		}
	}
}

// sleep waits d, or until ctx ends (false).
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
