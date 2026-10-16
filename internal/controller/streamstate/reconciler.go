package streamstate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// ControllerName is the Stream reconciler's name in the manager.
const ControllerName = "stream"

// Source fetches the stream bridge's observations: bridgeclient.Client.
// It must honour ctx.
type Source interface {
	Observations(ctx context.Context) (bridgeapi.Observations, error)
}

// Config configures the Stream reconciler and its poller. Client,
// Namespace, Names, Settings and Recorder are required.
type Config struct {
	// Client is the manager's client: reads come from its cache.
	Client client.Client
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Settings are the install's settings (the number plan, the stream
	// bridge).
	Settings compile.Settings
	// Bridge fetches the bridge's observations (bridgeclient.Client); nil
	// when Settings.StreamBridge is "". There is a poller only with both: a
	// Bridge is ignored when the settings name no stream bridge, and
	// without a Bridge while they name one, every routed Stream is Unknown,
	// saying the controller does not poll it.
	Bridge Source
	// Interval is the time between polls. 0: install.PollInterval.
	Interval time.Duration
	// Recorder emits StateChanged, SourceFailed and CallRefused.
	Recorder events.EventRecorder
	// Metrics records the bridge's reachability. Nil: none.
	Metrics *metrics.Metrics
	// Now is the controller's clock. Nil: time.Now.
	Now func() time.Time
	// Log is the reconciler's and the poller's logger.
	Log logr.Logger
}

func (c Config) validate() error {
	var errs []error
	if c.Client == nil {
		errs = append(errs, errors.New("streamstate: no client"))
	}
	if c.Namespace == "" {
		errs = append(errs, errors.New("streamstate: no namespace"))
	}
	if err := c.Names.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("streamstate: %w", err))
	}
	if err := c.Settings.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("streamstate: %w", err))
	}
	if c.Recorder == nil {
		errs = append(errs, errors.New("streamstate: no event recorder"))
	}
	if c.Interval < 0 {
		errs = append(errs, fmt.Errorf("streamstate: interval %s", c.Interval))
	}
	return errors.Join(errs...)
}

// noPoller is the snapshot of an install whose settings name a stream
// bridge that the controller was given no way to poll.
var noPoller = Snapshot{Polls: 1, LastError: "the controller does not poll it (no stream bridge URL)"}

// Reconciler writes Streams' status.
type Reconciler struct {
	cfg    Config
	poller *Poller // nil without a Bridge or a stream bridge
	log    logr.Logger
}

// New checks the configuration and builds the reconciler and, when the
// settings name a stream bridge and a Bridge is given, its poller.
func New(cfg Config) (*Reconciler, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Interval == 0 {
		cfg.Interval = install.PollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Reconciler{cfg: cfg, log: cfg.Log.WithName(ControllerName)}
	if cfg.Bridge != nil && cfg.Settings.StreamBridge != "" {
		r.poller = newPoller(cfg, r.log.WithName("poller"))
	}
	return r, nil
}

// Poller is the reconciler's poller, or nil when there is none.
func (r *Reconciler) Poller() *Poller { return r.poller }

// snapshot is the poller's latest snapshot.
func (r *Reconciler) snapshot() Snapshot {
	if r.poller == nil {
		return noPoller
	}
	return r.poller.Snapshot()
}

// Setup registers the poller (a leader-only Runnable) and the reconciler,
// Named("stream"), with the manager. The reconciler watches Streams for a
// new generation (so its own status writes never enqueue one), the
// pbx-config and <fullname>-streams ConfigMaps (every Stream: a number
// taken or freed, a stream compiled), and the poller's channel (the
// Streams whose observations changed).
func Setup(mgr ctrl.Manager, cfg Config) error {
	r, err := New(cfg)
	if err != nil {
		return err
	}
	compiled := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == cfg.Namespace && (o.GetName() == cfg.Names.ConfigMap || o.GetName() == cfg.Names.StreamsConfigMap)
	}))
	b := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&v1alpha1.Stream{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.all), compiled)
	if r.poller != nil {
		if err := mgr.Add(r.poller); err != nil {
			return fmt.Errorf("streamstate: adding the poller: %w", err)
		}
		b = b.WatchesRawSource(source.Channel(r.poller.changes, &handler.TypedEnqueueRequestForObject[*v1alpha1.Stream]{}))
	}
	return b.Complete(r)
}

// all enqueues every Stream.
func (r *Reconciler) all(ctx context.Context, _ client.Object) []reconcile.Request {
	var l v1alpha1.StreamList
	if err := r.cfg.Client.List(ctx, &l, client.InNamespace(r.cfg.Namespace)); err != nil {
		r.log.Error(err, "listing Streams to judge them again")
		return nil
	}
	out := make([]reconcile.Request, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
	}
	return out
}

// conflictRetry is how soon a Stream whose status write met a newer copy
// is judged again: the cache learns of a write a moment after it succeeds.
const conflictRetry = time.Second

// Reconcile judges one Stream and writes its status when it changed, then
// emits the verdict's Events. Nothing is written while <fullname>-streams
// does not exist (its creation enqueues every Stream), nor before the
// poller's first attempt has ended (Decide's Wait).
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var st v1alpha1.Stream
	if err := r.cfg.Client.Get(ctx, req.NamespacedName, &st); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	doc, err := install.ReadStreams(ctx, r.cfg.Client, r.cfg.Namespace, r.cfg.Names)
	switch {
	case errors.Is(err, install.ErrStreamsNotCompiled):
		r.log.V(1).Info("not judging: the streams have not been compiled yet", "stream", st.Name)
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	in, err := listInputs(ctx, r.cfg.Client, r.cfg.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	claims := compile.ResolveClaims(in, r.cfg.Settings.NumberPlan)
	var claim *compile.ClaimProblem
	if p, ok := claims.Problems[compile.ResourceRef{Kind: compile.ResourceStream, Name: st.Name}]; ok {
		claim = &p
	}
	v := Decide(Input{
		Stream: &st, Claim: claim, Settings: r.cfg.Settings, Compiled: doc,
		Snapshot: r.snapshot(), StaleAfter: install.StaleAfter, Now: r.cfg.Now(),
	})
	if v.Wait {
		r.log.V(1).Info("not judging: the stream bridge has not been polled yet", "stream", st.Name)
		return ctrl.Result{}, nil
	}
	after := r.apply(st.Status, v, st.Generation)
	if equality.Semantic.DeepEqual(st.Status, after) {
		return ctrl.Result{}, nil
	}
	st.Status = after
	if err := r.cfg.Client.Status().Update(ctx, &st); err != nil {
		if apierrors.IsConflict(err) {
			r.log.V(1).Info("the status write met a newer copy; judging again once the cache has it", "stream", st.Name)
			return ctrl.Result{RequeueAfter: conflictRetry}, nil
		}
		return ctrl.Result{}, writeError(st.Name, err)
	}
	for _, e := range v.Events {
		r.cfg.Recorder.Eventf(&st, nil, e.Type, e.Reason, e.Action, "%s", e.Note)
	}
	return ctrl.Result{}, nil
}

// apply is the status with the verdict: state, observedGeneration, Ready
// then NumberConflict with the generation and now as transition time
// (meta.SetStatusCondition keeps it unless the status changes), and the
// folded fields.
func (r *Reconciler) apply(status v1alpha1.StreamStatus, v Verdict, generation int64) v1alpha1.StreamStatus {
	out := *status.DeepCopy()
	at := metav1.NewTime(r.cfg.Now().UTC().Truncate(time.Second))
	out.State = v.State
	out.ObservedGeneration = generation
	for _, c := range []metav1.Condition{v.Ready, v.NumberConflict} {
		c.ObservedGeneration = generation
		c.LastTransitionTime = at
		meta.SetStatusCondition(&out.Conditions, c)
	}
	out.Listeners, out.TunerPool = v.Listeners, v.TunerPool
	out.LastPlayed, out.LastFailure = v.LastPlayed, v.LastFailure
	return out
}

// listInputs lists every resource that claims a number, from the cache.
func listInputs(ctx context.Context, c client.Reader, namespace string) (compile.Inputs, error) {
	var (
		phones     v1alpha1.PhoneList
		softphones v1alpha1.SoftphoneList
		people     v1alpha1.PersonList
		list       v1alpha1.StreamList
	)
	for _, l := range []client.ObjectList{&phones, &softphones, &people, &list} {
		if err := c.List(ctx, l, client.InNamespace(namespace)); err != nil {
			return compile.Inputs{}, fmt.Errorf("streamstate: listing %T: %w", l, err)
		}
	}
	return compile.Inputs{Phones: phones.Items, Softphones: softphones.Items, People: people.Items, Streams: list.Items}, nil
}

// writeError describes a failed status write without anything an Invalid
// error quotes from the object, and unwraps to the API error.
func writeError(name string, err error) error {
	msg := err.Error()
	if apierrors.IsInvalid(err) {
		msg = "the API server refused it as invalid"
	}
	return &statusError{msg: fmt.Sprintf("streamstate: writing the status of Stream %s: %s", name, msg), err: err}
}

type statusError struct {
	msg string
	err error
}

func (e *statusError) Error() string { return e.msg }
func (e *statusError) Unwrap() error { return e.err }
