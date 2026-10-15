// Package agentview is the controller's view of the PBX agent and its way
// of talking to it: what the leader-only poller publishes (Snapshot,
// Observer, Holder), whom it tells (Changes, SnapshotFunc), and the actions
// the device reconcilers send (Actor). docs/plans/release-a.md §6.2 is the
// specification.
//
// It is a contract between W3's stages: controller-core builds the poller
// and the HTTP client that implement it, controller-devices builds the
// reconcilers that consume it (tested against Holder and a fake Actor), and
// the integrator wires them in internal/controller. Change it deliberately.
//
// Rules:
//   - The poller is the only writer of the snapshot (Holder.Set). Everyone
//     else reads it with Observer.Snapshot and never modifies what it
//     returns: the document's slices are shared.
//   - Nothing is decided from a snapshot that has not Started: before the
//     poller's first attempt a device reconciler writes no status (it is
//     enqueued once that attempt ends, success or not).
//   - drivers.Observations are always built with Snapshot.For, so every
//     driver sees the agent the same way.
//   - An action counts as sent only when Actor returned no error and
//     Sent=true (ActionFailure).
package agentview

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Snapshot is what the controller knows of the agent after a poll.
type Snapshot struct {
	// Document is the last document GET /v1/observations returned
	// successfully; the zero value before the first success. When the
	// latest poll failed it is the previous good one (AgentReachable says
	// so).
	Document agentapi.Observations

	// PolledAt is when Document was fetched, by the controller's clock;
	// zero before the first success. Decide judges staleness from it.
	PolledAt time.Time

	// AgentReachable is whether the latest poll succeeded.
	AgentReachable bool

	// Polls counts the poll attempts that have ended since the poller
	// started, successful or not. Zero means the first is still running.
	Polls uint64

	// LastError is why the latest poll failed (it never holds the token);
	// empty after a success.
	LastError string
}

// Started reports whether the first poll attempt has ended, so the
// snapshot says something about the agent (even if only that it is
// unreachable).
func (s Snapshot) Started() bool { return s.Polls > 0 }

// Device returns the document's observations of one device, or nil when no
// sensor has seen it. The result is a copy of the entry; its slices are
// shared and must not be modified.
func (s Snapshot) Device(id string) *agentapi.DeviceObservations {
	for i := range s.Document.Devices {
		if s.Document.Devices[i].Device == id {
			d := s.Document.Devices[i]
			return &d
		}
	}
	return nil
}

// For assembles one device's drivers.Observations: the agent's health,
// the device's own observations, the firmware store, and the bundle
// version the controller compiled last (install.Compiled.Index.Version).
func (s Snapshot) For(deviceID, compiledVersion string) drivers.Observations {
	return drivers.Observations{
		AgentReachable:  s.AgentReachable,
		PolledAt:        s.PolledAt,
		Agent:           s.Document.Agent,
		Device:          s.Device(deviceID),
		Firmware:        s.Document.Firmware,
		CompiledVersion: compiledVersion,
	}
}

// Observer gives the latest snapshot. The poller implements it; tests use
// Holder.
type Observer interface {
	Snapshot() Snapshot
}

// Holder is a concurrency-safe Observer whose snapshot its owner sets: the
// poller publishes through one, and tests use one as a fake agent.
type Holder struct {
	mu sync.RWMutex
	s  Snapshot
}

// Snapshot returns the snapshot last set (the zero Snapshot, not Started,
// before that).
func (h *Holder) Snapshot() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.s
}

// Set replaces the snapshot.
func (h *Holder) Set(s Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.s = s
}

// SnapshotFunc is told about every successful poll, after the snapshot is
// published and the changed devices are enqueued. It runs on the poller's
// goroutine, so it must return quickly (record Events, never block on the
// network). Discovery is one (discovery.Reporter.Report).
type SnapshotFunc func(ctx context.Context, s Snapshot)

// Actor sends the agent's device actions: POST /v1/actions/resync and
// /v1/actions/restart with the bearer token (internal/controller/agentclient
// implements it). err is a failure to get an answer (agent unreachable, a
// non-200 status, a body that does not decode); an answer with Sent=false is
// returned as it came, with a nil error.
type Actor interface {
	Resync(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)
	Restart(ctx context.Context, req agentapi.ActionRequest) (agentapi.ActionResponse, error)
}

// ActionFailure is why an action did not go out, or "" when it did: the
// error, else the agent's Error when Sent is false.
func ActionFailure(resp agentapi.ActionResponse, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case resp.Sent:
		return ""
	case resp.Error != "":
		return resp.Error
	default:
		return "the agent did not send it"
	}
}

// DefaultChangesBuffer is the buffer of each Changes channel the
// integrator makes.
const DefaultChangesBuffer = 1024

// Changes carries "this device's observations changed, decide it again"
// from the poller to the device reconcilers: one channel per kind, each
// read by that kind's controller through source.Channel with
// handler.TypedEnqueueRequestForObject. The poller sends; the integrator
// makes one Changes and gives the same value to both.
type Changes struct {
	Phones     chan event.TypedGenericEvent[*v1alpha1.Phone]
	Softphones chan event.TypedGenericEvent[*v1alpha1.Softphone]
}

// NewChanges makes both channels with the given buffer.
func NewChanges(buffer int) Changes {
	return Changes{
		Phones:     make(chan event.TypedGenericEvent[*v1alpha1.Phone], buffer),
		Softphones: make(chan event.TypedGenericEvent[*v1alpha1.Softphone], buffer),
	}
}

// Notify asks for one device to be decided again: an event carrying only
// the resource's namespace and name (the reconciler reads the rest). It
// blocks until the event is queued or ctx ends, and fails for a kind other
// than Phone or Softphone.
func (c Changes) Notify(ctx context.Context, kind drivers.DeviceKind, namespace, name string) error {
	meta := metav1.ObjectMeta{Namespace: namespace, Name: name}
	switch kind {
	case drivers.KindPhone:
		return send(ctx, c.Phones, event.TypedGenericEvent[*v1alpha1.Phone]{Object: &v1alpha1.Phone{ObjectMeta: meta}})
	case drivers.KindSoftphone:
		return send(ctx, c.Softphones, event.TypedGenericEvent[*v1alpha1.Softphone]{Object: &v1alpha1.Softphone{ObjectMeta: meta}})
	}
	return fmt.Errorf("agentview: no device kind %q", kind)
}

func send[T any](ctx context.Context, ch chan<- T, v T) error {
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
