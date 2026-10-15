// Package discovery reports phones that ask for config patchbay does not
// have (docs/plans/release-a.md W3.7, design §5 "Discovery"): for each MAC
// in the agent's UnknownDevices, one UnknownPhone Event regarding the
// pbx-config ConfigMap, "unknown phone <MAC> requested config from <IP>",
// once per MAC per agent process (Document.Agent.StartedAt: the agent's
// list starts empty when it restarts).
//
// The Reporter is an agentview.SnapshotFunc: the poller calls it after each
// successful poll, on its own goroutine, so it never blocks (the Event
// recorder is asynchronous; the ConfigMap is read from the cache). It keeps
// what it reported in memory only, so a restarted controller reports each
// unknown phone again, once (plan §11). The live list is the agent's
// document, which `patchbay status` reads.
package discovery

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// Config is what the Reporter needs; every field but Log is required.
type Config struct {
	// Reader reads the pbx-config ConfigMap (the manager's cached client).
	Reader client.Reader
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Recorder emits the UnknownPhone Events.
	Recorder events.EventRecorder
	// Log is the Reporter's logger.
	Log logr.Logger
}

// Reporter turns the agent's unknown devices into Events.
type Reporter struct {
	cfg Config

	mu        sync.Mutex
	startedAt time.Time       // the agent process the set belongs to
	reported  map[string]bool // MACs reported for it, upper case
}

// New builds a Reporter.
func New(cfg Config) *Reporter {
	return &Reporter{cfg: cfg, reported: map[string]bool{}}
}

// Report emits an UnknownPhone Event for each unknown MAC not reported yet
// for this agent process. While pbx-config does not exist (or cannot be
// read) nothing is reported and nothing is marked, so the MACs are
// reported on a later poll.
func (r *Reporter) Report(ctx context.Context, s agentview.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if started := s.Document.Agent.StartedAt; !started.Equal(r.startedAt) {
		r.startedAt, r.reported = started, map[string]bool{}
	}
	var cm *corev1.ConfigMap
	for _, u := range s.Document.UnknownDevices {
		mac := strings.ToUpper(u.MAC)
		if mac == "" || r.reported[mac] {
			continue
		}
		if cm == nil {
			var got corev1.ConfigMap
			err := r.cfg.Reader.Get(ctx, client.ObjectKey{Namespace: r.cfg.Namespace, Name: r.cfg.Names.ConfigMap}, &got)
			switch {
			case apierrors.IsNotFound(err):
				r.cfg.Log.V(1).Info("not reporting unknown phones yet: the bundle has not been compiled", "configMap", r.cfg.Names.ConfigMap)
				return
			case err != nil:
				r.cfg.Log.Error(err, "not reporting unknown phones: reading the ConfigMap they are reported on", "configMap", r.cfg.Names.ConfigMap)
				return
			}
			cm = &got
		}
		r.cfg.Log.Info("unknown phone", "mac", mac, "source", u.Source, "file", u.File)
		r.cfg.Recorder.Eventf(cm, nil, corev1.EventTypeNormal, install.EventReasonUnknownPhone, install.EventActionDiscover,
			"%s", install.UnknownPhoneMessage(mac, u.Source))
		r.reported[mac] = true
	}
}
