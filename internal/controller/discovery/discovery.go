// Package discovery reports phones that ask for config patchbay does not
// have (docs/plans/release-a.md W3.7, design §5 "Discovery"): for each MAC
// in the agent's UnknownDevices, one UnknownPhone Event regarding the
// pbx-config ConfigMap, "unknown phone <MAC> requested config from <IP>",
// once per MAC per agent process (Document.Agent.StartedAt: the agent's
// list starts empty when it restarts).
//
// Each Event names its MAC in its related object
// (install.UnknownPhoneRelated): client-go's events broadcaster would
// otherwise join every UnknownPhone on one pbx-config, within six minutes
// of the last, into one series with the first phone's note.
//
// The agent's list is whatever asks on the voice VLAN: a host requesting
// SEP<random MAC>.cnf.xml in a loop fills it with up to 64 new MACs every
// poll. So the Reporter emits at most ReportBurst Events at once and one
// per ReportEvery after; a MAC it holds back is not marked, and is
// reported on a later poll while the agent still lists it. It remembers
// only the MACs the agent still lists, so its memory is bounded by the
// agent's list (64), and a phone that drops out of the list and asks
// again later is reported again.
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

// The rate of UnknownPhone Events: a burst, then one per interval.
const (
	// ReportBurst is how many UnknownPhone Events may go out at once (a
	// box of new phones plugged in together).
	ReportBurst = 16
	// ReportEvery is how often one more may go out once the burst is
	// spent.
	ReportEvery = 30 * time.Second
)

// Config is what the Reporter needs; every field but Now and Log is
// required.
type Config struct {
	// Reader reads the pbx-config ConfigMap (the manager's cached client).
	Reader client.Reader
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Recorder emits the UnknownPhone Events.
	Recorder events.EventRecorder
	// Now is the clock of the rate limit; nil means time.Now.
	Now func() time.Time
	// Log is the Reporter's logger.
	Log logr.Logger
}

// Reporter turns the agent's unknown devices into Events.
type Reporter struct {
	cfg Config

	mu        sync.Mutex
	startedAt time.Time       // the agent process the set belongs to
	reported  map[string]bool // MACs reported for it that it still lists, upper case
	tokens    float64         // Events that may go out now, at most ReportBurst
	refilled  time.Time       // when tokens was last brought up to date
}

// New builds a Reporter.
func New(cfg Config) *Reporter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reporter{cfg: cfg, reported: map[string]bool{}, tokens: ReportBurst}
}

// Report emits an UnknownPhone Event for each unknown MAC not reported yet
// for this agent process, as the rate allows. While pbx-config does not
// exist (or cannot be read) nothing is reported and nothing is marked, so
// the MACs are reported on a later poll.
func (r *Reporter) Report(ctx context.Context, s agentview.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if started := s.Document.Agent.StartedAt; !started.Equal(r.startedAt) {
		r.startedAt, r.reported = started, map[string]bool{}
	}
	listed := map[string]bool{}
	for _, u := range s.Document.UnknownDevices {
		listed[strings.ToUpper(u.MAC)] = true
	}
	for mac := range r.reported {
		if !listed[mac] {
			delete(r.reported, mac)
		}
	}
	r.refill()

	var cm *corev1.ConfigMap
	held := 0
	for _, u := range s.Document.UnknownDevices {
		mac := strings.ToUpper(u.MAC)
		if mac == "" || r.reported[mac] {
			continue
		}
		if r.tokens < 1 {
			held++
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
		r.cfg.Recorder.Eventf(cm, install.UnknownPhoneRelated(r.cfg.Namespace, mac), corev1.EventTypeNormal,
			install.EventReasonUnknownPhone, install.EventActionDiscover, "%s", install.EventNote(install.UnknownPhoneMessage(mac, u.Source)))
		r.reported[mac] = true
		r.tokens--
	}
	if held > 0 {
		r.cfg.Log.V(1).Info("unknown phones held back by the Event rate limit; reported later while the agent lists them", "count", held)
	}
}

// refill brings the rate limit's tokens up to now.
func (r *Reporter) refill() {
	now := r.cfg.Now()
	if !r.refilled.IsZero() && now.After(r.refilled) {
		r.tokens = min(ReportBurst, r.tokens+float64(now.Sub(r.refilled))/float64(ReportEvery))
	}
	r.refilled = now
}
