package streambridge

import "sync"

// frameQueue is one caller's queue: at most QueueFrames frames, the oldest
// dropped when a new one comes to a full queue, so a session never waits
// for a caller (docs/plans/release-b.md §2(l)). The frames are shared
// between callers and never written to.
type frameQueue struct {
	mu   sync.Mutex
	buf  [QueueFrames][]byte
	head int // the oldest frame
	n    int
}

// push queues f, dropping the oldest frame if the queue is full, and
// reports whether it dropped one.
func (q *frameQueue) push(f []byte) (dropped bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == len(q.buf) {
		q.buf[q.head] = nil
		q.head = (q.head + 1) % len(q.buf)
		q.n--
		dropped = true
	}
	q.buf[(q.head+q.n)%len(q.buf)] = f
	q.n++
	return dropped
}

// pop takes the oldest frame, if there is one.
func (q *frameQueue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return nil, false
	}
	f := q.buf[q.head]
	q.buf[q.head] = nil
	q.head = (q.head + 1) % len(q.buf)
	q.n--
	return f, true
}
