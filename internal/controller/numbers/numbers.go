// Package numbers holds the Person and Stream reconcilers
// (docs/plans/release-a.md W3.6, §6.2.6). Persons and Streams have no
// driver: their status comes from the number table (compile.ResolveClaims,
// the same function Compile routes with, over every resource) and, for a
// Person, from which of its ring targets the compiled bundle routes.
//
// Each writes two conditions, in order Ready and NumberConflict, and a
// display state:
//   - Invalid: an older resource holds the number (NumberConflict True,
//     Ready False NumberConflict), or the number is outside the kind's range
//     of the plan (Ready False NumberNotInPlan);
//   - Ready (Person): every ring target exists and is routed;
//   - Incomplete (Person): some ring targets are missing or not routed; it
//     still rings the others (Ready False MissingRingTargets);
//   - Unavailable: a Person none of whose targets is routed, or a Stream
//     (release A has no stream bridge): dialling it plays "not available".
//
// No status is written while the bundle has not been compiled (6.2.1,
// "Gating"); a new bundle enqueues every Person and Stream, and status is
// written only when it changed.
package numbers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Display states (status.state) of Persons and Streams.
const (
	StateReady       = "Ready"
	StateInvalid     = "Invalid"
	StateIncomplete  = "Incomplete"
	StateUnavailable = "Unavailable"
)

// Config is what both reconcilers need. Every field but Now and Log is
// required.
type Config struct {
	// Client is the manager's client: reads come from its cache.
	Client client.Client
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Settings are the install's settings (the number plan).
	Settings compile.Settings
	// Recorder emits StateChanged Events.
	Recorder events.EventRecorder
	// Now is the clock of condition transition times; nil means time.Now.
	Now func() time.Time
	// Log is the reconcilers' logger.
	Log logr.Logger
}

func (c Config) validate() error {
	var errs []error
	if c.Client == nil {
		errs = append(errs, errors.New("numbers: no client"))
	}
	if c.Namespace == "" {
		errs = append(errs, errors.New("numbers: no namespace"))
	}
	if err := c.Names.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("numbers: %w", err))
	}
	if err := c.Settings.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("numbers: %w", err))
	}
	if c.Recorder == nil {
		errs = append(errs, errors.New("numbers: no event recorder"))
	}
	return errors.Join(errs...)
}

// base is what both reconcilers share.
type base struct {
	cfg Config
	now func() time.Time
	log logr.Logger
}

func newBase(cfg Config, controller string) (base, error) {
	if err := cfg.validate(); err != nil {
		return base{}, err
	}
	b := base{cfg: cfg, now: cfg.Now, log: cfg.Log.WithName(controller)}
	if b.now == nil {
		b.now = time.Now
	}
	return b, nil
}

// view is what a Person or Stream is judged from.
type view struct {
	index  compile.Index
	inputs compile.Inputs
	claims compile.Claims
}

// observe reads the compiled index and every resource; ok is false while
// the bundle has not been compiled (no status then).
func (b *base) observe(ctx context.Context) (v view, ok bool, err error) {
	c, err := install.ReadCompiled(ctx, b.cfg.Client, b.cfg.Namespace, b.cfg.Names)
	switch {
	case errors.Is(err, install.ErrNotCompiled):
		b.log.V(1).Info("not judging: the bundle has not been compiled yet")
		return view{}, false, nil
	case err != nil:
		return view{}, false, err
	}
	v.index = c.Index
	if v.inputs, err = listInputs(ctx, b.cfg.Client, b.cfg.Namespace); err != nil {
		return view{}, false, err
	}
	v.claims = compile.ResolveClaims(v.inputs, b.cfg.Settings.NumberPlan)
	return v, true, nil
}

// listInputs lists every resource that claims a number, from the cache.
func listInputs(ctx context.Context, c client.Reader, namespace string) (compile.Inputs, error) {
	var (
		phones     v1alpha1.PhoneList
		softphones v1alpha1.SoftphoneList
		people     v1alpha1.PersonList
		streams    v1alpha1.StreamList
	)
	for _, l := range []client.ObjectList{&phones, &softphones, &people, &streams} {
		if err := c.List(ctx, l, client.InNamespace(namespace)); err != nil {
			return compile.Inputs{}, fmt.Errorf("numbers: listing %T: %w", l, err)
		}
	}
	return compile.Inputs{Phones: phones.Items, Softphones: softphones.Items, People: people.Items, Streams: streams.Items}, nil
}

// verdict is a Person's or Stream's judged status.
type verdict struct {
	state    string
	ready    metav1.Condition
	conflict metav1.Condition
}

func ready(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionReady, Status: status, Reason: reason, Message: message}
}

func numberConflict(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionNumberConflict, Status: status, Reason: reason, Message: message}
}

// noConflict is NumberConflict False.
var noConflict = numberConflict(metav1.ConditionFalse, v1alpha1.ReasonNoConflict, "")

// claimVerdict judges a resource that holds no number; ok is false when it
// holds its number.
func claimVerdict(claims compile.Claims, ref compile.ResourceRef) (v verdict, ok bool) {
	p, problem := claims.Problems[ref]
	switch {
	case !problem:
		return verdict{}, false
	case p.NumberConflict:
		return verdict{
			state:    StateInvalid,
			ready:    ready(metav1.ConditionFalse, v1alpha1.ReasonNumberConflict, p.Message),
			conflict: numberConflict(metav1.ConditionTrue, v1alpha1.ReasonNumberConflict, p.Message),
		}, true
	default: // out of the plan: the only other problem a Person or Stream can have
		return verdict{
			state:    StateInvalid,
			ready:    ready(metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan, p.Message),
			conflict: noConflict,
		}, true
	}
}

// notAvailable is what dialling a number with nothing behind it does.
func notAvailable(number string) string {
	return fmt.Sprintf(`dialling %s plays "not available"`, number)
}

// apply sets the verdict on a status (state, observedGeneration, Ready then
// NumberConflict with the generation and now as transition time, which
// meta.SetStatusCondition keeps unless the status changes).
func (b *base) apply(state *string, observed *int64, conditions *[]metav1.Condition, v verdict, generation int64) {
	at := metav1.NewTime(b.now().UTC().Truncate(time.Second))
	*state = v.state
	*observed = generation
	for _, c := range []metav1.Condition{v.ready, v.conflict} {
		c.ObservedGeneration = generation
		c.LastTransitionTime = at
		meta.SetStatusCondition(conditions, c)
	}
}

// written emits StateChanged after a status write that changed the state.
// Its note is bounded (install.EventNote): sixteen long ring targets make
// a Ready message longer than events.k8s.io's 1024 bytes.
func (b *base) written(obj runtime.Object, before, after string, v verdict) {
	if before == after {
		return
	}
	if before == "" {
		before = "None"
	}
	b.cfg.Recorder.Eventf(obj, nil, corev1.EventTypeNormal, install.EventReasonStateChanged, install.EventActionDecide,
		"%s", install.EventNote(fmt.Sprintf("%s -> %s: %s", before, after, v.ready.Message)))
}

// conflictRetry is how soon a resource whose status write met a conflict
// is judged again. The reconcilers read from the informer cache, which
// learns of a write a moment after it succeeds; a reconcile in that moment
// writes from a copy the API server has moved past. That is not an error
// (it would be logged as one and back off): a moment later the cache has
// caught up.
const conflictRetry = time.Second

// writeFailed is the result of a failed status write: a conflict is a
// quick requeue, anything else the error (writeError).
func (b *base) writeFailed(kind, name string, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		b.log.V(1).Info("the status write met a newer copy; judging again once the cache has it", "kind", kind, "name", name)
		return ctrl.Result{RequeueAfter: conflictRetry}, nil
	}
	return ctrl.Result{}, writeError(kind, name, err)
}

// statusChanged reports whether a status differs from its copy.
func statusChanged(before, after any) bool { return !equality.Semantic.DeepEqual(before, after) }

// writeError describes a failed status write without anything an Invalid
// error quotes from the object, and unwraps to the API error.
func writeError(kind, name string, err error) error {
	msg := err.Error()
	if apierrors.IsInvalid(err) {
		msg = "the API server refused it as invalid"
	}
	return &statusError{msg: fmt.Sprintf("numbers: writing the status of %s %s: %s", kind, name, msg), err: err}
}

type statusError struct {
	msg string
	err error
}

func (e *statusError) Error() string { return e.msg }
func (e *statusError) Unwrap() error { return e.err }

// PersonReconciler judges Persons (W3.6).
type PersonReconciler struct{ base }

// NewPersonReconciler checks the config and builds the reconciler.
func NewPersonReconciler(cfg Config) (*PersonReconciler, error) {
	b, err := newBase(cfg, personController)
	if err != nil {
		return nil, err
	}
	return &PersonReconciler{b}, nil
}

// Reconcile judges one Person and writes its status when it changed.
func (r *PersonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Person
	if err := r.cfg.Client.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	v, ok, err := r.observe(ctx)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}
	verdict, invalid := claimVerdict(v.claims, compile.ResourceRef{Kind: compile.ResourcePerson, Name: p.Name})
	if !invalid {
		verdict = personRouting(&p, v)
	}
	before := p.Status.DeepCopy()
	after := p.Status.DeepCopy()
	r.apply(&after.State, &after.ObservedGeneration, &after.Conditions, verdict, p.Generation)
	if !statusChanged(before, after) {
		return ctrl.Result{}, nil
	}
	p.Status = *after
	if err := r.cfg.Client.Status().Update(ctx, &p); err != nil {
		return r.writeFailed("Person", p.Name, err)
	}
	r.written(&p, before.State, after.State, verdict)
	return ctrl.Result{}, nil
}

// personRouting judges a Person that holds its number by its ring
// targets: each must exist and be routed (its compiled index entry has a
// number). Targets are named as "phone desk", "softphone laptop", each
// once, in ring order.
func personRouting(p *v1alpha1.Person, v view) verdict {
	exists := map[string]bool{}
	for i := range v.inputs.Phones {
		exists["phone "+v.inputs.Phones[i].Name] = true
	}
	for i := range v.inputs.Softphones {
		exists["softphone "+v.inputs.Softphones[i].Name] = true
	}
	var routed, missing []string
	seen := map[string]bool{}
	for _, t := range p.Spec.Rings {
		kind, name := drivers.KindPhone, string(t.Phone)
		if t.Softphone != "" {
			kind, name = drivers.KindSoftphone, string(t.Softphone)
		}
		target := strings.ToLower(string(kind)) + " " + name
		if seen[target] {
			continue
		}
		seen[target] = true
		switch {
		case !exists[target]:
			missing = append(missing, target+" (does not exist)")
		case v.index.Devices[drivers.DeviceID(kind, name)].Number == "":
			missing = append(missing, target+" (not routed)")
		default:
			routed = append(routed, target)
		}
	}
	rings := "rings " + strings.Join(routed, ", ")
	not := "not " + strings.Join(missing, ", ")
	switch {
	case len(missing) == 0:
		return verdict{state: StateReady, ready: ready(metav1.ConditionTrue, v1alpha1.ReasonRouted, rings), conflict: noConflict}
	case len(routed) > 0:
		return verdict{state: StateIncomplete,
			ready:    ready(metav1.ConditionFalse, v1alpha1.ReasonMissingRingTargets, rings+"; "+not),
			conflict: noConflict}
	default:
		return verdict{state: StateUnavailable,
			ready:    ready(metav1.ConditionFalse, v1alpha1.ReasonMissingRingTargets, not+": "+notAvailable(string(p.Spec.Number))),
			conflict: noConflict}
	}
}

// StreamReconciler judges Streams (W3.6).
type StreamReconciler struct{ base }

// NewStreamReconciler checks the config and builds the reconciler.
func NewStreamReconciler(cfg Config) (*StreamReconciler, error) {
	b, err := newBase(cfg, streamController)
	if err != nil {
		return nil, err
	}
	return &StreamReconciler{b}, nil
}

// Reconcile judges one Stream and writes its status when it changed. In
// release A every Stream that holds its number is Unavailable: there is no
// stream bridge.
func (r *StreamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s v1alpha1.Stream
	if err := r.cfg.Client.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	v, ok, err := r.observe(ctx)
	if err != nil || !ok {
		return ctrl.Result{}, err
	}
	verdict, invalid := claimVerdict(v.claims, compile.ResourceRef{Kind: compile.ResourceStream, Name: s.Name})
	if !invalid {
		verdict = noBridge(string(s.Spec.Number))
	}
	before := s.Status.DeepCopy()
	after := s.Status.DeepCopy()
	r.apply(&after.State, &after.ObservedGeneration, &after.Conditions, verdict, s.Generation)
	if !statusChanged(before, after) {
		return ctrl.Result{}, nil
	}
	s.Status = *after
	if err := r.cfg.Client.Status().Update(ctx, &s); err != nil {
		return r.writeFailed("Stream", s.Name, err)
	}
	r.written(&s, before.State, after.State, verdict)
	return ctrl.Result{}, nil
}

// noBridge is a Stream that holds its number in release A.
func noBridge(number string) verdict {
	return verdict{
		state:    StateUnavailable,
		ready:    ready(metav1.ConditionFalse, v1alpha1.ReasonNoStreamBridge, "release A has no stream bridge: "+notAvailable(number)),
		conflict: noConflict,
	}
}
