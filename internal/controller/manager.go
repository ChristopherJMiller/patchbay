// Package controller is the imperative shell around patchbay's pure core.
//
// The controller is the only component that decides and the only writer of
// resource status (design §5). Each reconcile observes (resources, agent
// observations), decides with pure functions (compile.Compile, a driver's
// Decide), then acts (ConfigMap, Secrets, status, agent actions) and
// requeues. The pieces live in its subpackages (docs/plans/release-a.md
// §6.2.3 has the map); this package builds the manager (NewManager) and
// wires them into it (Setup), tested against a real API server with
// envtest.
package controller

import (
	"errors"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// LeaderElectionID names the Lease the controller replicas of one install
// compete for: one per fullname, so two installs in one namespace never
// share a leader. The chart's Role names it (resourceNames).
func LeaderElectionID(fullname string) string {
	return fullname + install.LeaseSuffix
}

// Options configures the controller manager.
type Options struct {
	// Namespace is the PBX's namespace: the only namespace the controller
	// watches, and where its leader-election Lease lives. Required.
	Namespace string

	// MetricsAddr is the Prometheus metrics bind address; "0" disables it.
	MetricsAddr string

	// ProbeAddr is the /healthz and /readyz bind address; "0" disables it.
	ProbeAddr string

	// LeaderElection makes the manager hold a Lease before running
	// reconcilers, so only one replica ever decides.
	LeaderElection bool

	// Fullname is the chart's fullname, which every object of the install
	// is named after (install.NewNames). Required by Setup.
	Fullname string

	// SettingsFile is the settings.json the install's settings were read
	// from (the chart's <fullname>-settings ConfigMap, mounted). Setup is
	// given the settings themselves; this is for the log.
	SettingsFile string

	// AgentURL is the agent's control API. "" is the agent Service's
	// address on the default port (install.AgentURL).
	AgentURL string

	// StreamBridgeURL is the stream bridge's control API, which the Stream
	// reconciler polls for its observations. "" is the bridge Service's
	// address on the default control port (install.StreamBridgeURL) when
	// the settings name a stream bridge (compile.Settings.StreamBridge);
	// without one there is no bridge to poll and it is ignored.
	StreamBridgeURL string

	// MessagesAddr is the messages endpoint's bind address. "" (the default)
	// does not start it.
	MessagesAddr string

	// MessagesToken is the Secret holding the endpoint's bearer token, as
	// "<name>/<key>" (ParseMessagesToken). Required with MessagesAddr.
	MessagesToken string

	// Registerer takes the controller's own metrics. Nil is
	// controller-runtime's metrics.Registry, which the manager serves on
	// MetricsAddr; a process can register there only once.
	Registerer prometheus.Registerer
}

// skipNameValidation turns off controller-runtime's process-wide check that
// each controller name is used once. Only tests set it (export_test.go):
// a test process builds several managers and runs Setup in each (go test
// -count=N); the binary builds one.
var skipNameValidation bool

// NewScheme returns a scheme holding the built-in Kubernetes types and the
// patchbay v1alpha1 API.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// NewManager builds the controller-runtime manager: v1alpha1 scheme, a cache
// scoped to opts.Namespace, metrics, health probes and optional leader
// election (the Lease LeaderElectionID(opts.Fullname)). Setup registers the
// reconcilers.
func NewManager(cfg *rest.Config, opts Options) (ctrl.Manager, error) {
	if opts.Namespace == "" {
		return nil, errors.New("controller: namespace is required (set --namespace or POD_NAMESPACE)")
	}
	if opts.LeaderElection && opts.Fullname == "" {
		return nil, errors.New("controller: leader election needs the fullname, which names its Lease")
	}
	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: opts.MetricsAddr},
		HealthProbeBindAddress: opts.ProbeAddr,
		LeaderElection:         opts.LeaderElection,
		LeaderElectionID:       LeaderElectionID(opts.Fullname),
		// The binary exits as soon as the manager stops, so handing the
		// Lease back early is safe and speeds up failover on rollout.
		LeaderElectionReleaseOnCancel: true,
		LeaderElectionNamespace:       opts.Namespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{opts.Namespace: {}},
		},
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		return nil, err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, err
	}
	return mgr, nil
}

// ParseMessagesToken splits Options.MessagesToken, "<name>/<key>": a Secret
// name (a DNS-1123 subdomain) and a data key.
func ParseMessagesToken(s string) (name, key string, err error) {
	name, key, ok := strings.Cut(s, "/")
	if !ok || name == "" || key == "" || strings.Contains(key, "/") {
		return "", "", errors.New("must be <secret-name>/<key>")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", "", fmt.Errorf("secret name %q: %s", name, errs[0])
	}
	if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
		return "", "", fmt.Errorf("key %q: %s", key, errs[0])
	}
	return name, key, nil
}
