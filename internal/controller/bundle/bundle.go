// Package bundle is the install's compile reconciler (docs/plans/
// release-a.md W3.3, §6.2.5): it turns every resource in the namespace into
// the compiled bundle the agent applies, written as pbx-secrets, then
// pbx-config, and the streams the stream bridge plays, written before both
// as <fullname>-streams (release B, docs/plans/release-b.md RT.2).
//
// There is one reconcile key for the whole install, {namespace,
// <fullname>-pbx-config}; every watch maps to it, and one worker runs it.
// A reconcile:
//
//  1. lists Phones, Softphones, People and Streams (cache);
//  2. secrets.Manager.Ensure: every Secret the install needs, and the
//     values Compile reads;
//  3. compile.Compile, then install.StreamsConfigMap, install.ProjectedSecret
//     and install.ConfigMap;
//  4. writes <fullname>-streams, then pbx-secrets, then pbx-config, each
//     only when it differs (<fullname>-streams: data, streams-version,
//     labels; pbx-secrets: data, bundle-version, labels; pbx-config: data,
//     binary data, bundle-version, labels), so the bridge's file holds a
//     new stream before the dialplan routes its number, the agent never
//     sees a ConfigMap ahead of the Secret holding its values, and an
//     edited or deleted object is written back. A change only
//     <fullname>-streams sees (a source, a pool's max) rewrites only it,
//     with no ConfigWritten Event: the PBX is not reloaded.
//
// A compile or encoding error (compile.Compile, compile.ErrBundleTooLarge,
// the three builders), or a Secret secrets.Manager refuses
// (secrets.ErrInvalid), needs a human: the previous bundle stays, a
// CompileFailed Warning Event goes to pbx-config (or, while it does not
// exist, the settings ConfigMap; or only the log), the compile-failures
// metric counts it, and the reconcile ends without an error, to run again
// after CompileRetryAfter or on the next change. A write that meets cache
// lag (a conflict, or a create of an object that exists) runs again after
// StaleRetry. Any other error (an API call) is returned, so the reconcile
// is retried with backoff.
//
// Neither Events, logs nor errors hold a secret value: compile errors name
// Secrets and keys only.
package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/secrets"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ControllerName is the controller's name in the manager (logs, metrics).
const ControllerName = "bundle"

// CompileRetryAfter is when a reconcile whose compile failed runs again
// without a change.
const CompileRetryAfter = 5 * time.Minute

// StaleRetry is how soon a reconcile whose write met cache lag runs again.
// The manager's client reads from an informer cache, which learns of a
// write a moment after it succeeds; a reconcile in that moment writes from
// a stale copy (an update conflicts, a create finds the object there).
// That is not an error, which would be logged as one and back off.
const StaleRetry = time.Second

// Config configures the Reconciler.
type Config struct {
	// Client is the manager's client: cached reads, writes to the API
	// server.
	Client client.Client
	// Namespace is the PBX's namespace.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Settings are the install settings; they must pass
	// install.CheckSettings.
	Settings compile.Settings
	// Registry is the driver registry Compile renders with.
	Registry *drivers.Registry
	// Secrets creates the install's Secrets and gives their values.
	Secrets *secrets.Manager
	// Recorder emits ConfigWritten and CompileFailed.
	Recorder events.EventRecorder
	// Metrics counts compile failures and records the version. Nil: none.
	Metrics *metrics.Metrics
	// Log is the reconciler's logger.
	Log logr.Logger
}

// Reconciler compiles the install into pbx-secrets and pbx-config.
type Reconciler struct {
	cfg Config
}

// New checks the configuration and returns a Reconciler.
func New(cfg Config) (*Reconciler, error) {
	var errs []error
	if cfg.Client == nil {
		errs = append(errs, errors.New("bundle: no client"))
	}
	if cfg.Namespace == "" {
		errs = append(errs, errors.New("bundle: no namespace"))
	}
	if err := cfg.Names.Validate(); err != nil {
		errs = append(errs, err)
	} else if err := install.CheckSettings(cfg.Settings, cfg.Names); err != nil {
		errs = append(errs, err)
	}
	if cfg.Registry == nil {
		errs = append(errs, errors.New("bundle: no driver registry"))
	}
	if cfg.Secrets == nil {
		errs = append(errs, errors.New("bundle: no secrets manager"))
	}
	if cfg.Recorder == nil {
		errs = append(errs, errors.New("bundle: no event recorder"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Reconciler{cfg: cfg}, nil
}

// Key is the one reconcile key of the install.
func (r *Reconciler) Key() reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Namespace: r.cfg.Namespace, Name: r.cfg.Names.ConfigMap}}
}

// Reconcile compiles the install and writes the bundle. The request is
// ignored: every request is the install.
func (r *Reconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	in := compile.Inputs{}
	var (
		phones     v1alpha1.PhoneList
		softphones v1alpha1.SoftphoneList
		people     v1alpha1.PersonList
		streams    v1alpha1.StreamList
	)
	for _, l := range []client.ObjectList{&phones, &softphones, &people, &streams} {
		if err := r.cfg.Client.List(ctx, l, client.InNamespace(r.cfg.Namespace)); err != nil {
			return ctrl.Result{}, fmt.Errorf("bundle: listing %T: %w", l, err)
		}
	}
	in.Phones, in.Softphones, in.People, in.Streams = phones.Items, softphones.Items, people.Items, streams.Items

	values, err := r.cfg.Secrets.Ensure(ctx, in.Phones, in.Softphones)
	switch {
	case errors.Is(err, secrets.ErrInvalid):
		return r.compileFailed(ctx, err)
	case err != nil:
		return r.writeFailed(err)
	}
	in.Secrets = values

	b, err := compile.Compile(r.cfg.Registry, in, r.cfg.Settings)
	if err != nil {
		return r.compileFailed(ctx, err)
	}
	streamsCM, err := install.StreamsConfigMap(r.cfg.Names, r.cfg.Namespace, b.Streams)
	if err != nil {
		return r.compileFailed(ctx, err)
	}
	sec, err := install.ProjectedSecret(r.cfg.Names, r.cfg.Namespace, b, values)
	if err != nil {
		return r.compileFailed(ctx, err)
	}
	cm, err := install.ConfigMap(r.cfg.Names, r.cfg.Namespace, b)
	if err != nil {
		return r.compileFailed(ctx, err)
	}

	_, wroteStreams, err := r.writeConfigMap(ctx, streamsCM, install.AnnotationStreamsVersion)
	if err != nil {
		return r.writeFailed(err)
	}
	if wroteStreams {
		r.cfg.Log.Info("streams written", "version", b.Streams.Version, "streams", len(b.Streams.Streams), "pools", len(b.Streams.Pools))
	}
	wroteSecret, err := r.writeSecret(ctx, sec)
	if err != nil {
		return r.writeFailed(err)
	}
	current, wroteConfigMap, err := r.writeConfigMap(ctx, cm, install.AnnotationBundleVersion)
	if err != nil {
		return r.writeFailed(err)
	}
	r.cfg.Metrics.ConfigWritten(b.Version)
	if !wroteSecret && !wroteConfigMap {
		return ctrl.Result{}, nil
	}
	r.cfg.Log.Info("bundle written", "version", b.Version, "secret", wroteSecret, "configMap", wroteConfigMap,
		"files", len(b.Files), "devices", len(b.Devices), "streams", len(b.Streams.Streams))
	r.cfg.Recorder.Eventf(current, nil, corev1.EventTypeNormal, install.EventReasonConfigWritten, install.EventActionCompile,
		"version %s written: %d files, %d devices, %d streams", short(b.Version), len(b.Files), len(b.Devices), len(b.Streams.Streams))
	return ctrl.Result{}, nil
}

// writeFailed is the result of a failed write: a conflict or an object
// that already exists is cache lag, a quick requeue (StaleRetry); any
// other API error is returned, so the reconcile backs off.
func (r *Reconciler) writeFailed(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		r.cfg.Log.V(1).Info("a write met a newer copy than the cache's; compiling again once the cache has it", "error", err.Error())
		return ctrl.Result{RequeueAfter: StaleRetry}, nil
	}
	return ctrl.Result{}, err
}

// compileFailed reports a failure only a human can fix and keeps the
// previous bundle.
func (r *Reconciler) compileFailed(ctx context.Context, cause error) (ctrl.Result, error) {
	r.cfg.Metrics.CompileFailed()
	r.cfg.Log.Error(cause, "compiling the bundle failed; the previous bundle stays")
	if target := r.eventTarget(ctx); target != nil {
		note := install.EventNote(cause.Error())
		r.cfg.Recorder.Eventf(target, install.CompileFailedRelated(target, note), corev1.EventTypeWarning,
			install.EventReasonCompileFailed, install.EventActionCompile, "%s", note)
	}
	return ctrl.Result{RequeueAfter: CompileRetryAfter}, nil
}

// eventTarget is pbx-config, or the settings ConfigMap while it does not
// exist, or nil.
func (r *Reconciler) eventTarget(ctx context.Context) *corev1.ConfigMap {
	for _, name := range []string{r.cfg.Names.ConfigMap, r.cfg.Names.Settings} {
		var cm corev1.ConfigMap
		err := r.cfg.Client.Get(ctx, client.ObjectKey{Namespace: r.cfg.Namespace, Name: name}, &cm)
		switch {
		case err == nil:
			return &cm
		case !apierrors.IsNotFound(err):
			r.cfg.Log.Error(err, "reading the CompileFailed Event's target", "configMap", name)
			return nil
		}
	}
	r.cfg.Log.Info("neither pbx-config nor the settings ConfigMap exists; CompileFailed is only logged")
	return nil
}

// writeSecret creates or updates pbx-secrets unless it already holds
// exactly want's data, version and labels.
func (r *Reconciler) writeSecret(ctx context.Context, want *corev1.Secret) (bool, error) {
	var cur corev1.Secret
	err := r.cfg.Client.Get(ctx, client.ObjectKeyFromObject(want), &cur)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.cfg.Client.Create(ctx, want); err != nil {
			return false, fmt.Errorf("bundle: creating Secret %s: %w", want.Name, err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("bundle: reading Secret %s: %w", want.Name, err)
	}
	if maps.EqualFunc(cur.Data, want.Data, bytes.Equal) && len(cur.StringData) == 0 && sameMeta(&cur.ObjectMeta, &want.ObjectMeta, install.AnnotationBundleVersion) {
		return false, nil
	}
	upd := cur.DeepCopy()
	upd.Data, upd.StringData = want.Data, nil
	setMeta(&upd.ObjectMeta, &want.ObjectMeta, install.AnnotationBundleVersion)
	if err := r.cfg.Client.Update(ctx, upd); err != nil {
		return false, fmt.Errorf("bundle: updating Secret %s: %w", want.Name, err)
	}
	return true, nil
}

// writeConfigMap creates or updates a ConfigMap (pbx-config, or
// <fullname>-streams) unless it already holds exactly want's data, binary
// data, labels and version (the annotation named version). It returns the
// object as it now is.
func (r *Reconciler) writeConfigMap(ctx context.Context, want *corev1.ConfigMap, version string) (*corev1.ConfigMap, bool, error) {
	var cur corev1.ConfigMap
	err := r.cfg.Client.Get(ctx, client.ObjectKeyFromObject(want), &cur)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.cfg.Client.Create(ctx, want); err != nil {
			return nil, false, fmt.Errorf("bundle: creating ConfigMap %s: %w", want.Name, err)
		}
		return want, true, nil
	case err != nil:
		return nil, false, fmt.Errorf("bundle: reading ConfigMap %s: %w", want.Name, err)
	}
	if maps.Equal(cur.Data, want.Data) && maps.EqualFunc(cur.BinaryData, want.BinaryData, bytes.Equal) &&
		sameMeta(&cur.ObjectMeta, &want.ObjectMeta, version) {
		return &cur, false, nil
	}
	upd := cur.DeepCopy()
	upd.Data, upd.BinaryData = want.Data, want.BinaryData
	setMeta(&upd.ObjectMeta, &want.ObjectMeta, version)
	if err := r.cfg.Client.Update(ctx, upd); err != nil {
		return nil, false, fmt.Errorf("bundle: updating ConfigMap %s: %w", want.Name, err)
	}
	return upd, true, nil
}

// sameMeta reports whether cur carries want's labels and its version
// annotation (install.AnnotationBundleVersion or AnnotationStreamsVersion).
func sameMeta(cur, want *metav1.ObjectMeta, version string) bool {
	for k, v := range want.Labels {
		if cur.Labels[k] != v {
			return false
		}
	}
	return cur.Annotations[version] == want.Annotations[version]
}

// setMeta gives cur want's labels and version annotation, keeping others.
func setMeta(cur, want *metav1.ObjectMeta, version string) {
	if cur.Labels == nil {
		cur.Labels = map[string]string{}
	}
	maps.Copy(cur.Labels, want.Labels)
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[version] = want.Annotations[version]
}

// SetupWithManager registers the reconciler: watches on every resource
// kind (spec changes, creates and deletes), the Secrets the bundle depends
// on, pbx-config and <fullname>-streams themselves, and one request when
// the controller starts, so a
// fresh install with no resources still gets its agent token and a bundle.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{r.Key()}
	})
	spec := builder.WithPredicates(predicate.GenerationChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Watches(&v1alpha1.Phone{}, toKey, spec).
		Watches(&v1alpha1.Softphone{}, toKey, spec).
		Watches(&v1alpha1.Person{}, toKey, spec).
		Watches(&v1alpha1.Stream{}, toKey, spec).
		Watches(&corev1.Secret{}, toKey, builder.WithPredicates(predicate.NewPredicateFuncs(r.WatchesSecret))).
		Watches(&corev1.ConfigMap{}, toKey, builder.WithPredicates(predicate.NewPredicateFuncs(r.WatchesConfigMap))).
		WatchesRawSource(source.Func(func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			q.Add(r.Key())
			return nil
		})).
		Complete(r)
}

// WatchesSecret reports whether a change to this Secret can change the
// bundle: one owned by a Phone or Softphone (a device Secret), the AMI
// password, the push password, the agent token, or pbx-secrets itself (an edit is written
// back).
func (r *Reconciler) WatchesSecret(o client.Object) bool {
	if o.GetNamespace() != r.cfg.Namespace {
		return false
	}
	switch o.GetName() {
	case r.cfg.Names.AMISecret, r.cfg.Names.PushSecret, r.cfg.Names.AgentTokenSecret, r.cfg.Names.ProjectedSecret:
		return true
	}
	for _, ref := range o.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != v1alpha1.GroupVersion.Group {
			continue
		}
		if ref.Kind == string(drivers.KindPhone) || ref.Kind == string(drivers.KindSoftphone) {
			return true
		}
	}
	return false
}

// WatchesConfigMap reports whether this is pbx-config or <fullname>-streams
// (an edit or a delete is written back).
func (r *Reconciler) WatchesConfigMap(o client.Object) bool {
	if o.GetNamespace() != r.cfg.Namespace {
		return false
	}
	return o.GetName() == r.cfg.Names.ConfigMap || o.GetName() == r.cfg.Names.StreamsConfigMap
}

// short is a version's first 12 digits, as Events show it.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}
