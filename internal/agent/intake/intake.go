// Package intake is the agent's bundle intake (W2.1): it watches the
// pbx-config ConfigMap and the projected pbx-secrets Secret, decodes the
// bundle, resolves its placeholders, and hands the result to apply, or
// refuses the whole version.
//
// The agent's Role names exactly these two objects, so each is watched by
// its own informer with a metadata.name field selector (RBAC lets a
// resourceNames rule list and watch only that way). One worker evaluates
// the latest pair: a decode, validation or resolve failure is refused
// (Applier.Refuse, so LastApplyVersion and LastApplyError show it); a
// resolved bundle goes to Applier.Apply.
//
// Retries (docs/plans/release-a.md §5.2.1): an attempt is keyed by the
// bundle version and the Secret's resourceVersion. A pair that failed is
// tried again when either changes, or RetryAfter after it failed; the
// pair last applied is never applied again (an informer resync, or a
// ConfigMap rewritten with the same version, does nothing).
//
// No secret value reaches a log line or an error: the Secret object is
// never logged, and Resolve's errors name keys only.
package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Config configures an Intake.
type Config struct {
	// Client reads the two objects.
	Client kubernetes.Interface
	// Namespace is the PBX namespace.
	Namespace string
	// ConfigMap is <fullname>-pbx-config.
	ConfigMap string
	// Secret is <fullname>-pbx-secrets.
	Secret string
	// Applier applies or records each version.
	Applier Applier
	// RetryAfter is how long a failed pair waits before it is tried again
	// unchanged; 0 means 5 minutes.
	RetryAfter time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives what the intake does.
	Log logr.Logger
}

// Applier is what the intake hands bundles to (apply.Applier).
type Applier interface {
	// Apply writes, swaps, reloads and verifies a resolved version. An
	// error means it is not live (refused or rolled back).
	Apply(ctx context.Context, r Resolved) error
	// Refuse records a version that never reached Apply.
	Refuse(version string, reason error)
}

// DefaultRetryAfter is Config.RetryAfter's default.
const DefaultRetryAfter = 5 * time.Minute

// Intake watches the two objects and feeds the applier.
type Intake struct {
	cfg Config

	// after is the retry timer; tests replace it.
	after func(time.Duration) <-chan time.Time
	// synced, when set, is called after each evaluation (tests).
	synced func()

	kick    chan struct{}
	cms     cache.Store
	secrets cache.Store

	// Worker state.
	applied  pair
	failed   pair
	failedAt time.Time
	waiting  string // what the intake last said it waits for
}

// pair keys an attempt: the bundle (its version, or the ConfigMap's
// resourceVersion when no valid version can be read) and the Secret's
// resourceVersion.
type pair struct {
	bundle string
	secret string
}

// New checks the configuration. It does no I/O.
func New(cfg Config) (*Intake, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("intake: no Kubernetes client")
	case cfg.Namespace == "" || cfg.ConfigMap == "" || cfg.Secret == "":
		return nil, errors.New("intake: the namespace, ConfigMap and Secret names are required")
	case cfg.Applier == nil:
		return nil, errors.New("intake: no applier")
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = DefaultRetryAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Intake{cfg: cfg, after: time.After, kick: make(chan struct{}, 1)}, nil
}

// Run watches until ctx is done, then returns nil.
func (i *Intake) Run(ctx context.Context) error {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { i.poke() },
		UpdateFunc: func(any, any) { i.poke() },
		DeleteFunc: func(any) { i.poke() },
	}
	byName := func(name string) informers.SharedInformerOption {
		return informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
		})
	}
	cmFactory := informers.NewSharedInformerFactoryWithOptions(i.cfg.Client, 0,
		informers.WithNamespace(i.cfg.Namespace), byName(i.cfg.ConfigMap))
	secretFactory := informers.NewSharedInformerFactoryWithOptions(i.cfg.Client, 0,
		informers.WithNamespace(i.cfg.Namespace), byName(i.cfg.Secret))
	cmInformer := cmFactory.Core().V1().ConfigMaps().Informer()
	secretInformer := secretFactory.Core().V1().Secrets().Informer()
	if _, err := cmInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("intake: watching the ConfigMap: %w", err)
	}
	if _, err := secretInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("intake: watching the Secret: %w", err)
	}
	i.cms, i.secrets = cmInformer.GetStore(), secretInformer.GetStore()

	stop := ctx.Done()
	cmFactory.Start(stop)
	secretFactory.Start(stop)
	defer cmFactory.Shutdown()
	defer secretFactory.Shutdown()
	if !cache.WaitForCacheSync(stop, cmInformer.HasSynced, secretInformer.HasSynced) {
		return nil // ctx is done
	}
	i.cfg.Log.Info("watching the bundle", "namespace", i.cfg.Namespace, "configMap", i.cfg.ConfigMap, "secret", i.cfg.Secret)

	i.poke()
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-i.kick:
		case <-retry:
		}
		retry = nil
		if wait := i.sync(ctx); wait > 0 {
			retry = i.after(wait)
		}
		if i.synced != nil {
			i.synced()
		}
	}
}

func (i *Intake) poke() {
	select {
	case i.kick <- struct{}{}:
	default:
	}
}

// sync evaluates the latest objects once. It returns how long until a
// failed pair may be tried again, or 0 when nothing waits.
func (i *Intake) sync(ctx context.Context) time.Duration {
	cm, ok := get[*corev1.ConfigMap](i.cms, i.cfg.Namespace, i.cfg.ConfigMap)
	if !ok {
		i.wait("ConfigMap " + i.cfg.ConfigMap)
		return 0
	}
	secret, ok := get[*corev1.Secret](i.secrets, i.cfg.Namespace, i.cfg.Secret)
	if !ok {
		i.wait("Secret " + i.cfg.Secret)
		return 0
	}
	i.waiting = ""

	version, b, decodeErr := decode(cm)
	key := pair{bundle: version, secret: secret.ResourceVersion}
	if version == "" {
		key.bundle = "configmap@" + cm.ResourceVersion
	}
	switch key {
	case i.applied:
		i.cfg.Log.V(1).Info("bundle already applied", "version", version)
		return 0
	case i.failed:
		if wait := i.failedAt.Add(i.cfg.RetryAfter).Sub(i.cfg.Now()); wait > 0 {
			i.cfg.Log.V(1).Info("failed bundle waits to be retried", "version", version, "in", wait.String())
			return wait
		}
	}

	err := decodeErr
	if err != nil {
		i.cfg.Log.Info("bundle refused: it does not decode", "version", version, "error", err.Error())
		i.cfg.Applier.Refuse(version, err)
	} else {
		var r Resolved
		if r, err = Resolve(b, secret.Data); err != nil {
			i.cfg.Log.Info("bundle refused: its secrets do not resolve", "version", version, "error", err.Error())
			i.cfg.Applier.Refuse(version, err)
		} else {
			i.cfg.Log.Info("applying bundle", "version", version)
			if err = i.cfg.Applier.Apply(ctx, r); err != nil {
				i.cfg.Log.Info("bundle not applied", "version", version, "error", err.Error())
			}
		}
	}
	if err != nil {
		i.failed, i.failedAt = key, i.cfg.Now()
		return i.cfg.RetryAfter
	}
	i.applied, i.failed = key, pair{}
	return 0
}

// wait logs once that an object is missing, until it appears.
func (i *Intake) wait(what string) {
	if i.waiting != what {
		i.cfg.Log.Info("waiting for "+what, "namespace", i.cfg.Namespace)
		i.waiting = what
	}
}

func get[T any](s cache.Store, namespace, name string) (T, bool) {
	var zero T
	obj, ok, err := s.GetByKey(namespace + "/" + name)
	if err != nil || !ok {
		return zero, false
	}
	t, ok := obj.(T)
	return t, ok
}

// decode reads the bundle. version is the bundle's version when it can
// be read and is valid, else "" (then any bundle error is the reason).
func decode(cm *corev1.ConfigMap) (string, compile.Bundle, error) {
	b, err := compile.BundleFromConfigMap(cm.Data, cm.BinaryData)
	if err != nil {
		// The version, if the index can be read at all, so the refusal
		// names it.
		var idx struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(cm.Data[compile.IndexKey]), &idx) == nil && layout.ValidVersion(idx.Version) {
			return idx.Version, compile.Bundle{}, err
		}
		return "", compile.Bundle{}, err
	}
	if !layout.ValidVersion(b.Version) {
		return "", compile.Bundle{}, fmt.Errorf("bundle version %.80q is not 64 lower-case hex digits", b.Version)
	}
	return b.Version, b, nil
}
