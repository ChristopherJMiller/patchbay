// Package poller is the controller's observation poller (docs/plans/
// release-a.md W3.4, §6.2.5): a leader-only manager.Runnable that fetches
// the agent's observations every PollInterval, publishes what it saw as an
// agentview.Snapshot, and enqueues the devices whose observations changed.
//
// Each attempt publishes a snapshot:
//   - success: {document, now, reachable, Polls+1, ""};
//   - failure: {the previous document, its PolledAt, unreachable, Polls+1,
//     the error}, so a device decided after a failure still sees the last
//     good document, and how old it is.
//
// Then it enqueues, through agentview.Changes:
//   - every compiled device on the first attempt, and whenever the agent's
//     fingerprint (reachability, the agent's status, the firmware store)
//     changes: each of those changes every device's decision;
//   - otherwise each device whose own observations (their JSON) changed,
//     appeared or disappeared.
//
// Devices are named through the compiled index (install.ReadCompiled): an
// ID the index does not hold is skipped; while nothing is compiled nothing
// is enqueued (the device reconcilers' pbx-config watch enqueues them once
// it exists). If the index cannot be read, or the enqueue cannot finish,
// the poller keeps its previous fingerprints, so the next attempt enqueues
// the same devices again.
//
// After a success only, each OnSnapshot function runs, in order
// (discovery).
//
// Nothing can wedge the loop for long: each attempt is bounded by twice the
// interval (the agent client's own timeout is shorter), and so is the
// enqueue. Polls never queue up behind a slow one. The fingerprints are the
// poller's memory, and disposable.
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
)

// Source fetches the agent's observations: agentclient.Client. It must
// honour ctx.
type Source interface {
	Observations(ctx context.Context) (agentapi.Observations, error)
}

// Config configures a Poller.
type Config struct {
	// Source fetches the observations.
	Source Source
	// Reader reads the compiled index (install.ReadCompiled): the
	// manager's cached client.
	Reader client.Reader
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Interval is the time between attempts. 0: install.PollInterval.
	Interval time.Duration
	// Holder is where the snapshot is published; the device reconcilers
	// and the metrics read it.
	Holder *agentview.Holder
	// Changes is where devices to decide again are sent.
	Changes agentview.Changes
	// OnSnapshot runs after each successful attempt, in order.
	OnSnapshot []agentview.SnapshotFunc
	// Metrics observes every snapshot (apply failures). Nil: none.
	Metrics *metrics.Metrics
	// Now is the controller's clock. Nil: time.Now.
	Now func() time.Time
	// Log is the poller's logger.
	Log logr.Logger
}

// Poller polls the agent.
type Poller struct {
	cfg Config

	mu        sync.Mutex // one attempt at a time
	attempted bool       // an attempt's enqueue has completed
	agentFP   string
	deviceFPs map[string]string
	lastLog   string // the outcome logged last
}

// New checks the configuration and returns a Poller.
func New(cfg Config) (*Poller, error) {
	var errs []error
	if cfg.Source == nil {
		errs = append(errs, errors.New("poller: no source"))
	}
	if cfg.Reader == nil {
		errs = append(errs, errors.New("poller: no reader"))
	}
	if cfg.Namespace == "" {
		errs = append(errs, errors.New("poller: no namespace"))
	}
	if err := cfg.Names.Validate(); err != nil {
		errs = append(errs, err)
	}
	if cfg.Holder == nil {
		errs = append(errs, errors.New("poller: no holder"))
	}
	if cfg.Changes.Phones == nil || cfg.Changes.Softphones == nil {
		errs = append(errs, errors.New("poller: no Changes channels (agentview.NewChanges)"))
	}
	if cfg.Interval < 0 {
		errs = append(errs, fmt.Errorf("poller: interval %s", cfg.Interval))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if cfg.Interval == 0 {
		cfg.Interval = install.PollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Poller{cfg: cfg, deviceFPs: map[string]string{}}, nil
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

// NeedLeaderElection is true: only the leader polls, enqueues and reports.
func (p *Poller) NeedLeaderElection() bool { return true }

// Snapshot is the latest snapshot (agentview.Observer).
func (p *Poller) Snapshot() agentview.Snapshot { return p.cfg.Holder.Snapshot() }

// Poll makes one attempt: fetch, publish, enqueue, and after a success
// tell the OnSnapshot functions. An attempt cut short because ctx ended
// publishes nothing.
func (p *Poller) Poll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	limit := 2 * p.cfg.Interval
	pctx, cancel := context.WithTimeout(ctx, limit)
	doc, err := p.cfg.Source.Observations(pctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	prev := p.cfg.Holder.Snapshot()
	next := agentview.Snapshot{Polls: prev.Polls + 1}
	if err == nil {
		next.Document, next.PolledAt, next.AgentReachable = doc, p.cfg.Now(), true
	} else {
		next.Document, next.PolledAt, next.LastError = prev.Document, prev.PolledAt, err.Error()
	}
	p.cfg.Holder.Set(next)
	p.cfg.Metrics.ObserveSnapshot(next)
	p.logOutcome(next)

	ectx, cancel := context.WithTimeout(ctx, limit)
	p.enqueue(ectx, next)
	cancel()

	if err != nil {
		return
	}
	for _, f := range p.cfg.OnSnapshot {
		f(ctx, next)
	}
}

// enqueue sends the devices to decide again, and remembers the
// fingerprints once they are all sent.
func (p *Poller) enqueue(ctx context.Context, s agentview.Snapshot) {
	agentFP := fingerprint(struct {
		Reachable bool
		Agent     agentapi.AgentStatus
		Firmware  agentapi.FirmwareStore
	}{s.AgentReachable, s.Document.Agent, s.Document.Firmware})
	deviceFPs := make(map[string]string, len(s.Document.Devices))
	for _, d := range s.Document.Devices {
		deviceFPs[d.Device] = fingerprint(d)
	}
	commit := func() {
		p.attempted, p.agentFP, p.deviceFPs = true, agentFP, deviceFPs
	}

	all := !p.attempted || agentFP != p.agentFP
	var ids []string
	if !all {
		ids = changed(p.deviceFPs, deviceFPs)
		if len(ids) == 0 {
			commit()
			return
		}
	}

	compiled, err := install.ReadCompiled(ctx, p.cfg.Reader, p.cfg.Namespace, p.cfg.Names)
	switch {
	case errors.Is(err, install.ErrNotCompiled):
		p.cfg.Log.V(1).Info("nothing compiled yet; no device to enqueue")
		commit()
		return
	case err != nil:
		p.cfg.Log.Error(err, "reading the compiled index; the changed devices are enqueued on the next poll")
		return
	}
	if all {
		ids = slices.Sorted(maps.Keys(compiled.Index.Devices))
	}
	for _, id := range ids {
		d, ok := compiled.Device(id)
		if !ok {
			continue
		}
		if err := p.cfg.Changes.Notify(ctx, d.Kind, p.cfg.Namespace, d.Name); err != nil {
			p.cfg.Log.Error(err, "enqueueing a device; the changed devices are enqueued on the next poll", "device", id)
			return
		}
	}
	commit()
}

// changed lists the IDs whose fingerprint differs between before and
// after: changed, appeared or disappeared.
func changed(before, after map[string]string) []string {
	var ids []string
	for id, fp := range after {
		if before[id] != fp {
			ids = append(ids, id)
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
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
func (p *Poller) logOutcome(s agentview.Snapshot) {
	switch {
	case s.AgentReachable && p.lastLog != "":
		p.cfg.Log.Info("the agent answers again")
		p.lastLog = ""
	case !s.AgentReachable && s.LastError != p.lastLog:
		p.cfg.Log.Info("polling the agent failed; devices keep the last good observations until they are stale",
			"error", s.LastError, "lastSuccess", s.PolledAt)
		p.lastLog = s.LastError
	}
}
