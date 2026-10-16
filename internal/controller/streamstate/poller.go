package streamstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// ChangesBuffer is the buffer of the poller's channel to the reconciler.
const ChangesBuffer = 1024

// Poller is the stream bridge's poller: a leader-only manager.Runnable
// that fetches the bridge's observations every Interval, keeps what it saw
// as a Snapshot, and enqueues the Streams to judge again.
//
// Each attempt publishes a snapshot:
//   - success: {document, now, reachable, Polls+1, ""};
//   - failure: {the previous document, its PolledAt, unreachable, Polls+1,
//     the error}, so a Stream judged after a failure keeps the last good
//     document and can tell how old it is.
//
// Then it enqueues, through its channel: every Stream on the first attempt
// and whenever the bridge's fingerprint changes (reachability, its start
// time, the streams.json version it plays, the pools' observations), since
// each of those can change any Stream's state; otherwise each Stream whose
// own observation changed, appeared or disappeared, by name. If the
// Streams cannot be listed or the enqueue cannot finish, the fingerprints
// stay as they were, so the next attempt enqueues the same Streams again.
//
// Each attempt is bounded by twice the interval (the client's own timeout
// is shorter), and so is the enqueue; polls never queue up behind a slow
// one.
type Poller struct {
	cfg     Config
	log     logr.Logger
	changes chan event.TypedGenericEvent[*v1alpha1.Stream]

	snapMu sync.RWMutex
	snap   Snapshot

	mu        sync.Mutex // one attempt at a time
	attempted bool       // an attempt's enqueue has completed
	bridgeFP  string
	streamFPs map[string]string // by Stream name
	lastLog   string            // the outcome logged last
}

func newPoller(cfg Config, log logr.Logger) *Poller {
	return &Poller{
		cfg: cfg, log: log, streamFPs: map[string]string{},
		changes: make(chan event.TypedGenericEvent[*v1alpha1.Stream], ChangesBuffer),
	}
}

// Changes is the channel the poller sends the Streams to judge again on
// (Setup reads it with source.Channel).
func (p *Poller) Changes() <-chan event.TypedGenericEvent[*v1alpha1.Stream] { return p.changes }

// Snapshot is the latest snapshot (the zero Snapshot, not Started, before
// the first attempt ends).
func (p *Poller) Snapshot() Snapshot {
	p.snapMu.RLock()
	defer p.snapMu.RUnlock()
	return p.snap
}

func (p *Poller) set(s Snapshot) {
	p.snapMu.Lock()
	defer p.snapMu.Unlock()
	p.snap = s
}

// Start polls at once, then every Interval, until ctx ends; it returns nil.
func (p *Poller) Start(ctx context.Context) error {
	p.Poll(ctx)
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.Poll(ctx)
		}
	}
}

// NeedLeaderElection is true: only the leader polls and enqueues.
func (p *Poller) NeedLeaderElection() bool { return true }

// Poll makes one attempt: fetch, publish, enqueue. An attempt cut short
// because ctx ended publishes nothing.
func (p *Poller) Poll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	limit := 2 * p.cfg.Interval
	pctx, cancel := context.WithTimeout(ctx, limit)
	doc, err := p.cfg.Bridge.Observations(pctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	prev := p.Snapshot()
	next := Snapshot{Polls: prev.Polls + 1}
	if err == nil {
		next.Document, next.PolledAt, next.Reachable = doc, p.cfg.Now(), true
	} else {
		next.Document, next.PolledAt = prev.Document, prev.PolledAt
		next.LastError = install.Text(err.Error(), install.MaxStatusMessage)
	}
	p.set(next)
	p.cfg.Metrics.ObserveBridge(next.Reachable)
	p.logOutcome(next)

	ectx, cancel := context.WithTimeout(ctx, limit)
	p.enqueue(ectx, next)
	cancel()
}

// enqueue sends the Streams to judge again, and remembers the
// fingerprints once they are all sent.
func (p *Poller) enqueue(ctx context.Context, s Snapshot) {
	bridgeFP := fingerprint(struct {
		Reachable      bool
		StartedAt      time.Time
		StreamsVersion string
		Pools          []bridgeapi.PoolObservations
	}{s.Reachable, s.Document.Bridge.StartedAt, s.Document.Bridge.StreamsVersion, s.Document.Pools})
	streamFPs := make(map[string]string, len(s.Document.Streams))
	for _, o := range s.Document.Streams {
		streamFPs[o.Name] = fingerprint(o)
	}
	commit := func() { p.attempted, p.bridgeFP, p.streamFPs = true, bridgeFP, streamFPs }

	var names []string
	if !p.attempted || bridgeFP != p.bridgeFP {
		var l v1alpha1.StreamList
		if err := p.cfg.Client.List(ctx, &l, client.InNamespace(p.cfg.Namespace)); err != nil {
			p.log.Error(err, "listing Streams; they are enqueued on the next poll")
			return
		}
		for i := range l.Items {
			names = append(names, l.Items[i].Name)
		}
	} else {
		names = changed(p.streamFPs, streamFPs)
	}
	for _, n := range names {
		ev := event.TypedGenericEvent[*v1alpha1.Stream]{Object: &v1alpha1.Stream{
			ObjectMeta: metav1.ObjectMeta{Namespace: p.cfg.Namespace, Name: n},
		}}
		select {
		case p.changes <- ev:
		case <-ctx.Done():
			p.log.Error(ctx.Err(), "enqueueing a Stream; the changed Streams are enqueued on the next poll", "stream", n)
			return
		}
	}
	commit()
}

// changed lists the names whose fingerprint differs between before and
// after: changed, appeared or disappeared.
func changed(before, after map[string]string) []string {
	var out []string
	for n, fp := range after {
		if before[n] != fp {
			out = append(out, n)
		}
	}
	for n := range before {
		if _, ok := after[n]; !ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// fingerprint is the SHA-256 of v's JSON ("" if it does not encode, which
// never matches a previous fingerprint).
func fingerprint(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// logOutcome logs a failure when it is new (not every attempt) and the
// recovery from one.
func (p *Poller) logOutcome(s Snapshot) {
	switch {
	case s.Reachable && p.lastLog != "":
		p.log.Info("the stream bridge answers again")
		p.lastLog = ""
	case !s.Reachable && s.LastError != p.lastLog:
		p.log.Info("polling the stream bridge failed; Streams are Unknown until it answers",
			"error", s.LastError, "lastSuccess", s.PolledAt)
		p.lastLog = s.LastError
	}
}
