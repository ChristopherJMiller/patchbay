// Package devices holds the Phone and Softphone reconcilers
// (docs/plans/release-a.md W3.5, §6.2.6): thin shells around each driver's
// pure Decide. A reconcile observes (the resource, the compiled index read
// back from the cluster with install.ReadCompiled, the number table from
// compile.ResolveClaims, the poller's snapshot through agentview.Observer),
// decides (drivers.Driver.Decide), acts (resync and restart through
// agentview.Actor; rotateToken by writing the device Secret), and writes
// status only when it changed.
//
// No device logic lives here: every state, condition, action, memory field
// and human action comes from the driver's Decision. The shell adds only
// what the contracts give it to add: ObservedGeneration and
// LastTransitionTime on conditions, the softphone's provisioning URL from
// the compiled token, what a failed action keeps (6.2.1), and Events.
//
// Gating (6.2.1): no status is written before the poller's first attempt
// has ended, while the bundle has not been compiled, or while the device is
// not in the compiled index. Each of those ends with an event that enqueues
// the device again (the poller's first snapshot enqueues every device; the
// pbx-config watch enqueues every device of the kind).
//
// Secrets: the reconcilers never log, print or put in an Event or an error
// a secret value, a provisioning token or a provisioning URL.
package devices

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ActionRetryAfter is when a device whose action did not go out is decided
// again (6.2.1, "What a failed action records"), unless its Decision asks
// for sooner.
const ActionRetryAfter = 30 * time.Second

// Config is what both device reconcilers need. Every field but Rand, Now
// and Log is required.
type Config struct {
	// Client is the manager's client: reads come from its cache.
	Client client.Client
	// Namespace is the PBX's namespace, the only one the controller
	// watches.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Settings are the install's settings (the number plan, the PBX
	// address, the device HTTP port).
	Settings compile.Settings
	// Registry maps a model to its driver.
	Registry *drivers.Registry
	// Observer gives the poller's latest snapshot of the agent.
	Observer agentview.Observer
	// Actor sends resync and restart to the agent.
	Actor agentview.Actor
	// Changes are the poller's "decide this device again" channels (Setup
	// only).
	Changes agentview.Changes
	// Recorder emits the devices' Events.
	Recorder events.EventRecorder
	// Rand is the randomness of rotateToken; nil means crypto/rand.Reader.
	Rand io.Reader
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the reconcilers' logger.
	Log logr.Logger
}

// validate checks what both reconcilers need.
func (c Config) validate() error {
	var errs []error
	if c.Client == nil {
		errs = append(errs, errors.New("devices: no client"))
	}
	if c.Namespace == "" {
		errs = append(errs, errors.New("devices: no namespace"))
	}
	if err := c.Names.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("devices: %w", err))
	}
	if err := c.Settings.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("devices: %w", err))
	}
	if c.Registry == nil {
		errs = append(errs, errors.New("devices: no driver registry"))
	}
	if c.Observer == nil {
		errs = append(errs, errors.New("devices: no observer"))
	}
	if c.Actor == nil {
		errs = append(errs, errors.New("devices: no actor"))
	}
	if c.Recorder == nil {
		errs = append(errs, errors.New("devices: no event recorder"))
	}
	return errors.Join(errs...)
}

// engine is what the Phone and Softphone reconcilers share: everything but
// reading the resource and writing its kind's status.
type engine struct {
	writes *writes
	cfg    Config
	now    func() time.Time
	rand   io.Reader
	log    logr.Logger
}

func newEngine(cfg Config, controller string) (engine, error) {
	if err := cfg.validate(); err != nil {
		return engine{}, err
	}
	e := engine{cfg: cfg, now: cfg.Now, rand: cfg.Rand, log: cfg.Log.WithName(controller), writes: &writes{before: map[types.NamespacedName]string{}}}
	if e.now == nil {
		e.now = time.Now
	}
	if e.rand == nil {
		e.rand = rand.Reader
	}
	return e, nil
}

// view is what one device is decided from, besides the device itself.
type view struct {
	snap     agentview.Snapshot
	compiled install.Compiled
	entry    compile.BundleDevice
	claims   compile.Claims
}

// observe reads what a device is decided from (§6.2.6 steps 2 to 4). ok is
// false when the device must not be decided now (6.2.1, "Gating"): no
// status is written then.
func (e *engine) observe(ctx context.Context, id string) (v view, ok bool, err error) {
	log := e.log.WithValues("device", id)
	v.snap = e.cfg.Observer.Snapshot()
	if !v.snap.Started() {
		log.V(1).Info("not deciding: the first poll of the agent has not ended")
		return view{}, false, nil
	}
	v.compiled, err = install.ReadCompiled(ctx, e.cfg.Client, e.cfg.Namespace, e.cfg.Names)
	switch {
	case errors.Is(err, install.ErrNotCompiled):
		log.V(1).Info("not deciding: the bundle has not been compiled yet")
		return view{}, false, nil
	case err != nil:
		return view{}, false, err
	}
	if v.entry, ok = v.compiled.Device(id); !ok {
		log.V(1).Info("not deciding: the device is not in the compiled bundle yet", "version", v.compiled.Index.Version)
		return view{}, false, nil
	}
	in, err := listInputs(ctx, e.cfg.Client, e.cfg.Namespace)
	if err != nil {
		return view{}, false, err
	}
	v.claims = compile.ResolveClaims(in, e.cfg.Settings.NumberPlan)
	return v, true, nil
}

// listInputs lists every resource that claims a number, from the cache:
// what compile.ResolveClaims takes.
func listInputs(ctx context.Context, c client.Reader, namespace string) (compile.Inputs, error) {
	var (
		phones     v1alpha1.PhoneList
		softphones v1alpha1.SoftphoneList
		people     v1alpha1.PersonList
		streams    v1alpha1.StreamList
	)
	for _, l := range []client.ObjectList{&phones, &softphones, &people, &streams} {
		if err := c.List(ctx, l, client.InNamespace(namespace)); err != nil {
			return compile.Inputs{}, fmt.Errorf("devices: listing %T: %w", l, err)
		}
	}
	return compile.Inputs{Phones: phones.Items, Softphones: softphones.Items, People: people.Items, Streams: streams.Items}, nil
}

// decide completes the device from the view (claims, its compiled version)
// and runs its driver's Decide (§6.2.6 steps 5 and 6). ok is false when no
// driver serves its model: the CRD's enum admits only served models, so
// that is a build without the driver, logged and not decided.
func (e *engine) decide(dev *drivers.Device, v view) (d drivers.Decision, now time.Time, ok bool) {
	v.claims.Apply(dev)
	dev.ConfigVersion = v.entry.Version
	drv, err := e.cfg.Registry.ForModel(dev.Model)
	if err != nil {
		e.log.Error(err, "not deciding: no driver serves the device's model", "device", dev.ID)
		return drivers.Decision{}, time.Time{}, false
	}
	now = e.now()
	return drv.Decide(*dev, v.snap.For(dev.ID, v.compiled.Index.Version), install.DecideSettings(e.cfg.Settings), now), now, true
}

// act takes the Decision's actions in order (§6.2.6 step 7) and reports
// whether one failed. It stops at the first failure: the rest of the
// Decision's actions are not attempted, and the caller keeps the previous
// lastAction and handledRequest (6.2.1).
func (e *engine) act(ctx context.Context, obj runtime.Object, dev drivers.Device, actions []drivers.Action) (failed bool) {
	for _, a := range actions {
		failure := e.send(ctx, dev, a)
		if failure != "" {
			e.log.Info("action not sent", "device", dev.ID, "action", a.Kind, "version", a.Version, "reason", failure)
			e.event(obj, corev1.EventTypeWarning, install.EventReasonActionFailed, eventAction(a.Kind), failedNote(a, failure))
			return true
		}
		e.log.Info("action sent", "device", dev.ID, "action", a.Kind, "version", a.Version)
		e.event(obj, corev1.EventTypeNormal, install.EventReasonActionSent, eventAction(a.Kind), sentNote(a))
	}
	return false
}

// send takes one action; it returns why it did not go out, or "".
func (e *engine) send(ctx context.Context, dev drivers.Device, a drivers.Action) string {
	req := agentapi.ActionRequest{Device: dev.ID, Version: a.Version}
	switch a.Kind {
	case v1alpha1.ActionResync:
		resp, err := e.cfg.Actor.Resync(ctx, req)
		return agentview.ActionFailure(resp, err)
	case v1alpha1.ActionRestart:
		resp, err := e.cfg.Actor.Restart(ctx, req)
		return agentview.ActionFailure(resp, err)
	case v1alpha1.ActionRotateToken:
		return e.rotateToken(ctx, dev.ID)
	}
	return fmt.Sprintf("the controller cannot take action %q", a.Kind)
}

// rotateToken writes a new provisioning token into the device's existing
// Secret (6.2.1, "Secret writers": the bundle reconciler creates it; a
// missing Secret is a failed action). It returns why it failed, or "";
// neither the token nor the Secret's data ever reaches the result.
func (e *engine) rotateToken(ctx context.Context, id string) string {
	name := compile.DeviceSecretName(id)
	var s corev1.Secret
	if err := e.cfg.Client.Get(ctx, client.ObjectKey{Namespace: e.cfg.Namespace, Name: name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "Secret " + name + " does not exist"
		}
		return "reading Secret " + name + ": " + apiFailure(err)
	}
	token, err := install.GenerateProvisioningToken(e.rand)
	if err != nil {
		return "generating a provisioning token: " + err.Error()
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[compile.SecretKeyProvisioningToken] = []byte(token)
	if err := e.cfg.Client.Update(ctx, &s); err != nil {
		return "writing Secret " + name + ": " + apiFailure(err)
	}
	return ""
}

// apiFailure describes an API error without anything it may quote from the
// object: an Invalid error's causes can quote field values (a Secret's, or
// a status' provisioning URL).
func apiFailure(err error) string {
	if apierrors.IsInvalid(err) {
		return "the API server refused it as invalid"
	}
	return err.Error()
}

// writeError is the error of a failed status write: its message never
// quotes the object (apiFailure), and it unwraps to the API error, so
// apierrors.IsConflict and friends still see it.
type writeError struct {
	msg string
	err error
}

func (w *writeError) Error() string { return w.msg }
func (w *writeError) Unwrap() error { return w.err }

func statusWriteError(kind, name string, err error) error {
	return &writeError{msg: fmt.Sprintf("devices: writing the status of %s %s: %s", kind, name, apiFailure(err)), err: err}
}

// memory is the status memory to write: the Decision's, except that a
// failed action keeps the previous lastAction and handledRequest (6.2.1),
// and that the values observed from the device (the load ID its web page
// reports, its address) are bounded (install.Text, MaxStatusValue).
func memory(dev drivers.Device, d drivers.Decision, failed bool) drivers.Memory {
	m := d.StatusPatch
	m.Firmware = install.Text(m.Firmware, install.MaxStatusValue)
	m.Address = install.Text(m.Address, install.MaxStatusValue)
	if failed {
		m.LastAction = dev.Memory.LastAction.DeepCopy()
		m.HandledRequest = dev.Memory.HandledRequest
	}
	return m
}

// result is what to requeue after (§6.2.6 step 11): the Decision's, or
// ActionRetryAfter after a failed action unless the Decision asks for
// sooner.
func result(d drivers.Decision, failed bool) ctrl.Result {
	after := d.RequeueAfter
	if failed && (after <= 0 || after > ActionRetryAfter) {
		after = ActionRetryAfter
	}
	return ctrl.Result{RequeueAfter: after}
}

// setConditions sets the Decision's conditions (§6.2.6 step 8): each gets
// the resource's generation and now, in whole seconds, as its transition
// time, which meta.SetStatusCondition keeps unless the status changes, and
// a message bounded by install.Text (it can quote a device's web page or
// SIP user agent, or the agent's apply error). A condition type the
// Decision does not list keeps its value.
func setConditions(conditions *[]metav1.Condition, from []metav1.Condition, generation int64, now time.Time) {
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	for _, c := range from {
		c.Message = install.Text(c.Message, install.MaxStatusMessage)
		c.ObservedGeneration = generation
		c.LastTransitionTime = at
		meta.SetStatusCondition(conditions, c)
	}
}

// summary is what the Events after a status write compare.
type summary struct {
	state          string
	handledRequest string
	conditions     []metav1.Condition
}

// written emits the Events of a status write (§6.2.6 step 10):
// StateChanged when the state changed; RequestIgnored when handledRequest
// advanced to the device's request without the action that answers it;
// ApplyFailed when ConfigCurrent became False with reason ApplyFailed.
func (e *engine) written(obj runtime.Object, dev drivers.Device, d drivers.Decision, answers v1alpha1.ActionKind,
	annotation, bundleVersion string, before, after summary) {
	if before.state != after.state {
		old := before.state
		if old == "" {
			old = "None"
		}
		ready := ""
		if c := meta.FindStatusCondition(after.conditions, v1alpha1.ConditionReady); c != nil {
			ready = c.Message
		}
		e.event(obj, corev1.EventTypeNormal, install.EventReasonStateChanged, install.EventActionDecide,
			fmt.Sprintf("%s -> %s: %s", old, after.state, ready))
	}
	if after.handledRequest != before.handledRequest && after.handledRequest == dev.Request && !holds(d.Actions, answers) {
		e.event(obj, corev1.EventTypeNormal, install.EventReasonRequestIgnored, install.EventActionDecide,
			fmt.Sprintf("%s request %s ignored: the device is %s", annotation, dev.Request, after.state))
	}
	if c := meta.FindStatusCondition(after.conditions, v1alpha1.ConditionConfigCurrent); applyFailed(c) &&
		!applyFailed(meta.FindStatusCondition(before.conditions, v1alpha1.ConditionConfigCurrent)) {
		e.event(obj, corev1.EventTypeWarning, install.EventReasonApplyFailed, install.EventActionDecide,
			fmt.Sprintf("config version %s was not applied: %s", short(bundleVersion), c.Message))
	}
}

// applyFailed reports a ConfigCurrent condition that is False with reason
// ApplyFailed.
func applyFailed(c *metav1.Condition) bool {
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.ReasonApplyFailed
}

// holds reports whether the actions include one of the kind.
func holds(actions []drivers.Action, kind v1alpha1.ActionKind) bool {
	for _, a := range actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// event records an Event regarding obj. The note is passed as an argument,
// never as the format, so a '%' in an agent's message stays as it is, and
// bounded (install.EventNote), so the API server never refuses it.
func (e *engine) event(obj runtime.Object, eventType, reason, action, note string) {
	e.cfg.Recorder.Eventf(obj, nil, eventType, reason, action, "%s", install.EventNote(note))
}

// humanAction is the Decision's human action as status holds it: bounded
// like a condition message.
func humanAction(d drivers.Decision) string {
	return install.Text(d.HumanAction, install.MaxStatusMessage)
}

// eventAction is the Event action of a device action.
func eventAction(k v1alpha1.ActionKind) string {
	switch k {
	case v1alpha1.ActionResync:
		return install.EventActionResync
	case v1alpha1.ActionRestart:
		return install.EventActionRestart
	case v1alpha1.ActionRotateToken:
		return install.EventActionRotateToken
	}
	return install.EventActionDecide
}

// sentNote is an ActionSent Event's note (install events.go).
func sentNote(a drivers.Action) string {
	switch a.Kind {
	case v1alpha1.ActionResync:
		return "resync sent for version " + short(a.Version)
	case v1alpha1.ActionRestart:
		return "restart sent"
	case v1alpha1.ActionRotateToken:
		return "provisioning token rotated"
	}
	return string(a.Kind) + " sent"
}

// failedNote is an ActionFailed Event's note (install events.go): "<kind>
// for version <v[:12]> not sent: <reason>", without the version part for
// an action that is not about one (rotateToken).
func failedNote(a drivers.Action, failure string) string {
	if a.Version == "" {
		return fmt.Sprintf("%s not sent: %s", a.Kind, failure)
	}
	return fmt.Sprintf("%s for version %s not sent: %s", a.Kind, short(a.Version), failure)
}

// short is a version as notes show it: its first 12 characters.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// staleRetry is how soon a device is decided again when the cache still
// serves the copy this reconciler wrote the status of (writes).
const staleRetry = 250 * time.Millisecond

// writes guards against deciding on a cached copy older than the
// reconciler's own last status write. The manager's client reads from an
// informer cache, which learns of a write a moment after it succeeds; a
// reconcile in that moment (an event queued during the one that wrote)
// would decide on the old status and repeat an action the write recorded,
// such as a resync. For each resource it remembers the resourceVersion it
// read before its last successful status write: a cached copy with exactly
// that version predates the write, so the reconcile waits for the cache
// (staleRetry) instead of deciding. Equality only: resourceVersions are
// opaque.
type writes struct {
	mu     sync.Mutex
	before map[types.NamespacedName]string
}

// stale reports whether the copy read (its resourceVersion) predates this
// reconciler's last status write of the resource; a newer copy clears the
// mark.
func (w *writes) stale(key types.NamespacedName, resourceVersion string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	before, ok := w.before[key]
	if !ok {
		return false
	}
	if before == resourceVersion {
		return true
	}
	delete(w.before, key)
	return false
}

// wrote records a successful status write of a copy read at
// resourceVersion.
func (w *writes) wrote(key types.NamespacedName, resourceVersion string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.before[key] = resourceVersion
}

// forget drops a deleted resource.
func (w *writes) forget(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.before, key)
}

// read gets the resource for a reconcile. ok is false when there is
// nothing to decide now: it is gone, or the cache has not caught up with
// the last status write (then res says when to look again).
func (e *engine) read(ctx context.Context, key types.NamespacedName, obj client.Object) (res ctrl.Result, ok bool, err error) {
	if err := e.cfg.Client.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			e.writes.forget(key)
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, false, err
	}
	if e.writes.stale(key, obj.GetResourceVersion()) {
		e.log.V(1).Info("not deciding: the cache has not seen the last status write yet", "name", key.Name)
		return ctrl.Result{RequeueAfter: staleRetry}, false, nil
	}
	return ctrl.Result{}, true, nil
}
