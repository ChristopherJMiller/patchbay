// Package controller is the imperative shell around patchbay's pure core.
//
// The controller is the only component that decides and the only writer of
// resource status (design §5). Each reconcile observes (resources, agent
// observations), decides with pure functions (compile.Compile, a driver's
// Decide), then acts (ConfigMap, Secrets, status, agent actions) and
// requeues. Reconcilers land here, tested against a real API server with
// envtest; this file only builds the manager they will run in.
package controller

import (
	"errors"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// LeaderElectionID names the Lease the controller replicas compete for.
const LeaderElectionID = "patchbay-controller.patchbay.chrismiller.xyz"

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
}

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
// election. No reconcilers are registered yet.
func NewManager(cfg *rest.Config, opts Options) (ctrl.Manager, error) {
	if opts.Namespace == "" {
		return nil, errors.New("controller: namespace is required (set --namespace or POD_NAMESPACE)")
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
		LeaderElectionID:       LeaderElectionID,
		// The binary exits as soon as the manager stops, so handing the
		// Lease back early is safe and speeds up failover on rollout.
		LeaderElectionReleaseOnCancel: true,
		LeaderElectionNamespace:       opts.Namespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{opts.Namespace: {}},
		},
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
