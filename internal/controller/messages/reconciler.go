package messages

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

const (
	// ControllerName is the Message reconciler's name in the manager.
	ControllerName = "message"
	// RequestName names the one request the reconciler takes per
	// namespace: every Message, Phone and house state change enqueues it,
	// and Decide judges every Message and phone at once.
	RequestName = "messages"
	// PushedMemory is how long the reconciler remembers a successful push
	// that the agent's observations do not show yet (PhoneView.PushedPage).
	PushedMemory = 30 * time.Second

	// conflictRetry is how soon the Messages are decided again after a
	// status write met a newer copy: the cache learns of a write a moment
	// after it succeeds.
	conflictRetry = time.Second
)

// Config configures the Message reconciler. Every field but Interval, Now
// and Log is required.
type Config struct {
	// Client is the manager's client: reads come from its cache.
	Client client.Client
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names (the house ConfigMap).
	Names install.Names
	// Settings are the install's settings: the signalling backend and the
	// timezone the page shows times in.
	Settings compile.Settings
	// Registry maps a Phone's model to its driver (its profile, its page).
	Registry *drivers.Registry
	// Observer gives the poller's latest snapshot of the agent.
	Observer agentview.Observer
	// Actor sends the pushes (agentclient.Client).
	Actor agentview.Actor
	// Recorder emits the Message Events.
	Recorder events.EventRecorder
	// Interval is how often the Messages are decided again while one is
	// not final (ruling R16: calls end and phones register without an
	// event this reconciler watches). 0: install.PollInterval.
	Interval time.Duration
	// Now is the controller's clock. Nil: time.Now.
	Now func() time.Time
	// Log is the reconciler's logger.
	Log logr.Logger
}

func (c Config) validate() error {
	var errs []error
	if c.Client == nil {
		errs = append(errs, errors.New("messages: no client"))
	}
	if c.Namespace == "" {
		errs = append(errs, errors.New("messages: no namespace"))
	}
	if err := c.Names.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("messages: %w", err))
	}
	if err := c.Settings.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("messages: %w", err))
	}
	if c.Registry == nil {
		errs = append(errs, errors.New("messages: no driver registry"))
	}
	if c.Observer == nil {
		errs = append(errs, errors.New("messages: no observer of the agent"))
	}
	if c.Actor == nil {
		errs = append(errs, errors.New("messages: no actor to push with"))
	}
	if c.Recorder == nil {
		errs = append(errs, errors.New("messages: no event recorder"))
	}
	if c.Interval < 0 {
		errs = append(errs, fmt.Errorf("messages: interval %s", c.Interval))
	}
	return errors.Join(errs...)
}

// Reconciler delivers Messages: it builds Decide's input from the cluster,
// the agent's snapshot and its memory of its own pushes, sends the pushes
// Decide asks for, and writes what Decide returns (statuses, Events,
// deletions). It sets no status of its own: after pushing it decides again
// with their outcome, and writes that verdict.
//
// Its memory (the last successful push and the last failure per phone) is
// in-process and disposable: a new leader starts from the agent's
// observations, at worst pushing one page again without the chime.
type Reconciler struct {
	cfg Config
	loc *time.Location
	log logr.Logger

	// mu is held through a whole Reconcile (there is one request, so
	// reconciles never overlap; the lock only makes that certain).
	mu       sync.Mutex
	mem      map[string]*phoneMemory // by Phone name
	houseBad string                  // the unreadable house.json last logged
}

// phoneMemory is what the reconciler remembers of its pushes to a phone.
type phoneMemory struct {
	pushed   string // the version pushed (PageCleared for a clear), "" for none
	pushedAt time.Time
	failure  string
	failedAt time.Time
}

// New checks the configuration, loads the settings' timezone and builds
// the reconciler.
func New(cfg Config) (*Reconciler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.Settings.Timezone)
	if err != nil {
		return nil, fmt.Errorf("messages: timezone %q: %w", cfg.Settings.Timezone, err)
	}
	if cfg.Interval == 0 {
		cfg.Interval = install.PollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reconciler{cfg: cfg, loc: loc, log: cfg.Log.WithName(ControllerName), mem: map[string]*phoneMemory{}}, nil
}

// Setup registers the reconciler, Named("message"), with the manager. It
// watches Messages and Phones for a new generation (so its own status
// writes never enqueue it; a Message's withdraw is a new generation) and
// the house ConfigMap, each mapped onto the namespace's one request
// (RequestName). It does not read the poller's agentview.Changes: that
// channel's receivers are the device reconcilers (ruling R16).
func Setup(mgr ctrl.Manager, cfg Config) error {
	r, err := New(cfg)
	if err != nil {
		return err
	}
	toRequest := handler.EnqueueRequestsFromMapFunc(r.request)
	house := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == cfg.Namespace && o.GetName() == cfg.Names.HouseConfigMap
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		Watches(&v1alpha1.Message{}, toRequest, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.Phone{}, toRequest, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, toRequest, house).
		Complete(r)
}

// request maps an object of the namespace onto its one request.
func (r *Reconciler) request(_ context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() != r.cfg.Namespace {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.cfg.Namespace, Name: RequestName}}}
}

// Reconcile decides every Message of the namespace and carries the verdict
// out: the pushes first, then (deciding again with their outcome) the
// status writes, the Events of the statuses written, and the deletions.
// Nothing is decided before the poller's first attempt has ended. It is
// decided again at the verdict's RequeueAfter, and every Interval while a
// Message is not final.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.cfg.Namespace {
		return ctrl.Result{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := r.cfg.Observer.Snapshot()
	if !snap.Started() {
		r.log.V(1).Info("not deciding: the agent has not been polled yet")
		return ctrl.Result{RequeueAfter: r.cfg.Interval}, nil
	}
	var (
		msgs   v1alpha1.MessageList
		phones v1alpha1.PhoneList
	)
	for _, l := range []client.ObjectList{&msgs, &phones} {
		if err := r.cfg.Client.List(ctx, l, client.InNamespace(r.cfg.Namespace)); err != nil {
			return ctrl.Result{}, fmt.Errorf("messages: listing %T: %w", l, err)
		}
	}
	house, err := r.house(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.cfg.Now()
	in := Input{Messages: msgs.Items, House: house, Phones: r.views(phones.Items, snap, now), Location: r.loc, Now: now}
	v := Decide(in)
	if len(v.Pushes) > 0 {
		r.push(ctx, v.Pushes)
		in.Phones = r.views(phones.Items, snap, now)
		v = Decide(in)
		for _, p := range v.Pushes {
			// Decide waits PushRetry after a failure and takes a success for
			// held, so a second verdict has nothing new to push.
			r.log.Info("a push left for the next decision", "phone", p.Phone, "version", p.PageVersion, "clear", p.Clear)
		}
	}
	conflict, err := r.apply(ctx, msgs.Items, v)
	if err != nil {
		return ctrl.Result{}, err // retried with the controller's backoff
	}
	after := v.RequeueAfter
	if pending(msgs.Items, v) {
		after = earliest(after, r.cfg.Interval)
	}
	if conflict {
		after = earliest(after, conflictRetry)
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// earliest is the shorter of two waits, 0 meaning none.
func earliest(a, b time.Duration) time.Duration {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// pending reports whether some Message is left in a state that is not
// final once the verdict is carried out.
func pending(msgs []v1alpha1.Message, v Verdict) bool {
	for i := range msgs {
		m := &msgs[i]
		st, ok := v.Status[m.Name]
		if !ok {
			st = m.Status
		}
		if !final(st.State) && !slices.Contains(v.Delete, m.Name) {
			return true
		}
	}
	return false
}

// house reads the house state from the house ConfigMap: the zero House
// (deliver) when it or its key does not exist. A house.json that does not
// parse counts as quiet hours, so only urgent Messages go out until it is
// fixed; that is logged once per content. r.mu is held.
func (r *Reconciler) house(ctx context.Context) (messagesapi.House, error) {
	var cm corev1.ConfigMap
	err := r.cfg.Client.Get(ctx, types.NamespacedName{Namespace: r.cfg.Namespace, Name: r.cfg.Names.HouseConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		return messagesapi.House{}, nil
	case err != nil:
		return messagesapi.House{}, fmt.Errorf("messages: reading the house ConfigMap: %w", err)
	}
	raw := cm.Data[install.HouseKey]
	if raw == "" {
		return messagesapi.House{}, nil
	}
	h, err := messagesapi.ParseHouse([]byte(raw))
	if err != nil {
		if r.houseBad != raw {
			r.houseBad = raw
			r.log.Error(err, "the house state does not parse; holding every Message that is not urgent as in quiet hours",
				"configMap", r.cfg.Names.HouseConfigMap, "key", install.HouseKey)
		}
		return messagesapi.House{Quiet: true}, nil
	}
	r.houseBad = ""
	return h, nil
}

// views builds each Phone's PhoneView from the cluster, the snapshot and
// the reconciler's memory, which it prunes: a remembered push is dropped
// once the agent's observations show it or PushedMemory passes, and the
// memory of a Phone that is gone is dropped. r.mu is held.
func (r *Reconciler) views(phones []v1alpha1.Phone, snap agentview.Snapshot, now time.Time) map[string]PhoneView {
	out := make(map[string]PhoneView, len(phones))
	for i := range phones {
		pv := phoneView(&phones[i], r.cfg.Registry, r.cfg.Settings.Signalling, snap, now)
		if m := r.mem[pv.Name]; m != nil {
			if m.pushed != "" && (now.Sub(m.pushedAt) >= PushedMemory || observed(m.pushed, pv.HeldPage)) {
				m.pushed = ""
			}
			pv.PushedPage = m.pushed
			pv.PushFailure, pv.PushFailedAt = m.failure, m.failedAt
		}
		out[pv.Name] = pv
	}
	for _, name := range slices.Collect(maps.Keys(r.mem)) {
		if _, ok := out[name]; !ok {
			delete(r.mem, name)
		}
	}
	return out
}

// observed reports whether the agent's acknowledged page shows the push
// remembered as pushed.
func observed(pushed, held string) bool {
	if pushed == PageCleared {
		return held == ""
	}
	return held == pushed
}

// phoneView is what Decide needs of one Phone, from the resource, its
// driver and the snapshot. Reachable needs the agent's latest poll to have
// succeeded, at most install.StaleAfter ago, and a registration that has
// not lapsed; person calls are the device's Calls less its StreamCalls.
func phoneView(p *v1alpha1.Phone, reg *drivers.Registry, signalling drivers.Signalling, snap agentview.Snapshot, now time.Time) PhoneView {
	dev := drivers.PhoneDevice(p)
	pv := PhoneView{Name: p.Name, DeviceID: dev.ID, Model: dev.Model, Exists: true, Push: drivers.Unsupported}
	if drv, err := reg.ForModel(dev.Model); err == nil {
		if prof, ok := drv.Profile(dev.Model); ok {
			pv.Push = prof.Supports(signalling, drivers.FeaturePushNotification)
		}
		if mr, ok := drv.(drivers.MessageRenderer); ok {
			pv.Renderer = mr
		}
	}
	if obs := snap.Device(dev.ID); obs != nil {
		pv.HeldPage = obs.Page
		pv.PersonCalls = max(0, obs.Calls-obs.StreamCalls)
		fresh := snap.AgentReachable && now.Sub(snap.PolledAt) <= install.StaleAfter
		pv.Reachable = fresh && obs.Registration != nil && obs.Registration.ExpiresAt.After(now)
	}
	return pv
}

// push sends the pushes and remembers each outcome. r.mu is held.
func (r *Reconciler) push(ctx context.Context, pushes []PagePush) {
	for _, p := range pushes {
		resp, err := r.cfg.Actor.Push(ctx, agentapi.PushRequest{
			Device: p.DeviceID, PageVersion: p.PageVersion, Page: string(p.Page), Beep: p.Beep, Clear: p.Clear,
		})
		at := r.cfg.Now()
		m := r.mem[p.Phone]
		if m == nil {
			m = &phoneMemory{}
			r.mem[p.Phone] = m
		}
		if failure := agentview.PushFailure(resp, err); failure != "" {
			m.failure, m.failedAt = failure, at
			r.log.Info("push not sent; trying again later", "phone", p.Phone, "device", p.DeviceID,
				"version", p.PageVersion, "clear", p.Clear, "reason", failure, "retryIn", PushRetry)
			continue
		}
		m.failure, m.failedAt = "", time.Time{}
		m.pushed, m.pushedAt = p.PageVersion, at
		if p.Clear {
			m.pushed = PageCleared
		}
		r.log.Info("pushed", "phone", p.Phone, "device", p.DeviceID, "version", p.PageVersion,
			"beep", p.Beep, "clear", p.Clear, "answer", resp.Phone)
	}
}

// apply writes the verdict's statuses, emits the Events of the statuses
// written (each regarding the Message as written, so a new note is a new
// series), and deletes the Messages it names. conflict is true when a
// write met a newer copy; that Message is decided again shortly.
func (r *Reconciler) apply(ctx context.Context, msgs []v1alpha1.Message, v Verdict) (conflict bool, err error) {
	byName := make(map[string]*v1alpha1.Message, len(msgs))
	for i := range msgs {
		byName[msgs[i].Name] = &msgs[i]
	}
	var errs []error
	written := map[string]*v1alpha1.Message{}
	for _, name := range slices.Sorted(maps.Keys(v.Status)) {
		m := byName[name].DeepCopy()
		m.Status = v.Status[name]
		switch err := r.cfg.Client.Status().Update(ctx, m); {
		case err == nil:
			written[name] = m
			r.log.V(1).Info("status written", "message", name, "phone", m.Spec.Phone, "state", m.Status.State, "reason", m.Status.Reason)
		case apierrors.IsNotFound(err):
		case apierrors.IsConflict(err):
			r.log.V(1).Info("the status write met a newer copy; deciding again once the cache has it", "message", name)
			conflict = true
		default:
			errs = append(errs, statusWriteError(name, err))
		}
	}
	for _, e := range v.Events {
		if m, ok := written[e.Message]; ok {
			r.cfg.Recorder.Eventf(m, nil, e.Type, e.Reason, e.Action, "%s", e.Note)
		}
	}
	for _, name := range v.Delete {
		m := byName[name]
		err := r.cfg.Client.Delete(ctx, m, client.Preconditions{UID: &m.UID})
		switch {
		case err == nil:
			r.log.Info("deleted a finished Message", "message", name, "state", m.Status.State, "finishedAt", m.Status.FinishedAt)
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			errs = append(errs, fmt.Errorf("messages: deleting Message %s: %w", name, err))
		}
	}
	return conflict, errors.Join(errs...)
}

// statusWriteError describes a failed status write without anything an Invalid
// error quotes from the object (the Message's text), and unwraps to the API
// error.
func statusWriteError(name string, err error) error {
	msg := err.Error()
	if apierrors.IsInvalid(err) {
		msg = "the API server refused it as invalid"
	}
	return &statusError{msg: fmt.Sprintf("messages: writing the status of Message %s: %s", name, msg), err: err}
}

type statusError struct {
	msg string
	err error
}

func (e *statusError) Error() string { return e.msg }
func (e *statusError) Unwrap() error { return e.err }
