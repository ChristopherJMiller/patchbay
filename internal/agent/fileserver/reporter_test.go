package fileserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

var pinned = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// event is a valid event, numbered by its file name.
func event(i int) sensor.FetchEvent {
	return sensor.FetchEvent{
		Transport: api.TransportTFTP, File: fmt.Sprintf("f%05d", i), Result: api.FetchServed,
		At: pinned, Source: "192.168.20.100",
	}
}

// agentSocket is the agent's end of the fetch log: it accepts connections
// on a unix socket and reads events from each, in order.
type agentSocket struct {
	t      *testing.T
	path   string
	ln     net.Listener
	mu     sync.Mutex
	conns  []net.Conn
	events chan sensor.FetchEvent
}

func socketPath(t *testing.T) string {
	t.Helper()
	// A unix socket path must stay under ~108 bytes; t.TempDir() can be
	// long, so use a short directory of our own.
	dir, err := os.MkdirTemp("", "fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "fetches.sock")
}

func listenAgent(t *testing.T, path string) *agentSocket {
	t.Helper()
	_ = os.Remove(path)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	a := &agentSocket{t: t, path: path, ln: ln, events: make(chan sensor.FetchEvent, 4096)}
	go a.accept()
	t.Cleanup(a.close)
	return a
}

func (a *agentSocket) accept() {
	for {
		c, err := a.ln.Accept()
		if err != nil {
			return
		}
		a.mu.Lock()
		a.conns = append(a.conns, c)
		a.mu.Unlock()
		go func() {
			r := sensor.NewFetchEventReader(c)
			for {
				e, err := r.Next()
				if err != nil {
					return
				}
				a.events <- e
			}
		}()
	}
}

// close stops listening and drops every connection, as an agent that
// exits does.
func (a *agentSocket) close() {
	a.ln.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.conns {
		c.Close()
	}
	a.conns = nil
}

func (a *agentSocket) connections() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// next waits for the next event.
func (a *agentSocket) next() sensor.FetchEvent {
	a.t.Helper()
	select {
	case e := <-a.events:
		return e
	case <-time.After(10 * time.Second):
		a.t.Fatal("no event arrived")
		return sensor.FetchEvent{}
	}
}

func (a *agentSocket) none(within time.Duration) {
	a.t.Helper()
	select {
	case e := <-a.events:
		a.t.Errorf("unexpected event %s", e.File)
	case <-time.After(within):
	}
}

func runReporter(t *testing.T, r *Reporter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v, want nil once cancelled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return once cancelled")
		}
	})
}

func newTestReporter(path string, log logr.Logger) *Reporter {
	r := NewReporter(path, log)
	r.retry = 20 * time.Millisecond
	return r
}

// TestReporter: events arrive in order; when the agent restarts they keep
// coming on a new connection, none lost that was recorded after it was
// gone; while the agent is away the queue keeps the newest 1024 and counts
// (and logs) what it dropped.
func TestReporter(t *testing.T) {
	path := socketPath(t)
	agent := listenAgent(t, path)
	var logMu sync.Mutex
	var logged strings.Builder
	log := funcr.New(func(prefix, args string) {
		logMu.Lock()
		defer logMu.Unlock()
		logged.WriteString(prefix + args + "\n")
	}, funcr.Options{})
	r := newTestReporter(path, log)
	runReporter(t, r)

	for i := range 100 {
		r.RecordFetch(event(i))
	}
	for i := range 100 {
		if e := agent.next(); e.File != event(i).File {
			t.Fatalf("event %d is %s: order not kept", i, e.File)
		}
	}

	// The agent restarts: a new listener on the same path.
	agent.close()
	agent = listenAgent(t, path)
	for i := 100; i < 110; i++ {
		r.RecordFetch(event(i))
	}
	for i := 100; i < 110; i++ {
		if e := agent.next(); e.File != event(i).File {
			t.Fatalf("after the restart, event %d is %s", i, e.File)
		}
	}

	// The agent is away: the queue overflows and drops the oldest.
	agent.close()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// Nothing can be delivered now, so of the reporterQueue+10 events
	// recorded the oldest 10 are dropped, and the newest reporterQueue
	// arrive in order once the agent is back.
	for i := 110; i < 110+reporterQueue+10; i++ {
		r.RecordFetch(event(i))
	}
	// The sender may hold the first of them, popped for a write to the
	// closed agent, while the rest arrive: the queue then has room for one
	// more, and the tenth drop happens only when the failed write puts
	// that event back (unpop). So wait for the count to settle; it must
	// never pass 10.
	deadline := time.Now().Add(5 * time.Second)
	for r.Dropped() < 10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := r.Dropped(); got != 10 {
		t.Errorf("Dropped = %d, want 10", got)
	}
	agent = listenAgent(t, path)
	for i := 120; i < 110+reporterQueue+10; i++ {
		if e := agent.next(); e.File != event(i).File {
			t.Fatalf("after the overflow, event %d is %s; want the newest %d in order", i, e.File, reporterQueue)
		}
	}
	agent.none(100 * time.Millisecond)
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logged.String(), `"dropped"=10`) {
		t.Errorf("the drops were not logged:\n%s", logged.String())
	}
}

// TestReporterNeverBlocks: an agent that accepts and never reads, or no
// agent at all, never slows RecordFetch: it is on a phone's transfer path.
func TestReporterNeverBlocks(t *testing.T) {
	path := socketPath(t)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	accepting := make(chan struct{})
	go func() { // accept, never read
		defer close(accepting)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	defer func() {
		ln.Close()
		<-accepting
		for _, c := range held {
			c.Close()
		}
	}()
	r := newTestReporter(path, logr.Discard())
	r.writeTimeout = 50 * time.Millisecond
	runReporter(t, r)
	start := time.Now()
	for i := range 20000 {
		r.RecordFetch(event(i))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("20000 RecordFetch calls took %v", d)
	}
	if r.Dropped() == 0 {
		t.Error("nothing dropped although the agent never reads")
	}
}

// TestReporterDropsInvalidEvents: an invalid event is never queued (the
// agent would end the connection on it).
func TestReporterDropsInvalidEvents(t *testing.T) {
	path := socketPath(t)
	agent := listenAgent(t, path)
	r := newTestReporter(path, logr.Discard())
	runReporter(t, r)
	bad := event(1)
	bad.Source = "not an address"
	r.RecordFetch(bad)
	r.RecordFetch(event(2))
	if e := agent.next(); e.File != event(2).File {
		t.Errorf("got %s, want only the valid event", e.File)
	}
	agent.none(50 * time.Millisecond)
	if agent.connections() != 1 {
		t.Errorf("%d connections, want 1: the invalid event must not end one", agent.connections())
	}
}

// TestReporterRunEndsWithContext: Run returns once its context ends,
// connected or not.
func TestReporterRunEndsWithContext(t *testing.T) {
	r := newTestReporter(filepath.Join(t.TempDir(), "absent.sock"), logr.Discard())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
