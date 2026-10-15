// Package ctrltest is the envtest harness the controller packages' tests
// share (docs/plans/release-a.md §6.2): one real kube-apiserver and etcd per
// test package (binaries from the flake, KUBEBUILDER_ASSETS), with the
// chart's CRDs installed, a namespace per test, and a manager scoped to it.
// Import it from _test.go files only.
//
// A package's TestMain:
//
//	var env *ctrltest.Env
//
//	func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }
//
// and each test that needs the API server starts with ctrltest.Need(t, env).
// With -short (make test-unit) no API server starts and those tests skip;
// tests that use the controller-runtime fake client run either way.
//
// envtest has no kubelet and no controllers: no namespace is ever deleted,
// no garbage collector honours ownerReferences, and no Pod runs. Tests
// assert the objects the code writes, not what Kubernetes would do next.
package ctrltest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// Env is a running envtest API server.
type Env struct {
	// Config reaches the API server as an administrator.
	Config *rest.Config
	// Client is a direct (uncached) client with Scheme.
	Client client.Client
	// Scheme holds the built-in types and the patchbay API.
	Scheme *k8sruntime.Scheme
}

// NewScheme is the scheme the controller runs with: the built-in
// Kubernetes types and patchbay v1alpha1 (as controller.NewScheme, which
// the controller's own packages must not import).
func NewScheme() (*k8sruntime.Scheme, error) {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// CRDDir is the chart's CRD directory (charts/patchbay/templates/crds),
// found from this file's place in the repository.
func CRDDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("ctrltest: cannot locate the repository")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "charts", "patchbay", "templates", "crds")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("ctrltest: the chart's CRDs: %w", err)
	}
	return filepath.Clean(dir), nil
}

// Main runs a test package: without -short it starts envtest, sets *env,
// runs the tests and stops envtest; with -short it runs the tests with
// *env nil. It returns the exit code for os.Exit.
func Main(m *testing.M, env **Env) int {
	flag.Parse()
	if testing.Short() {
		return m.Run()
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run inside `nix develop` (or pass -short to skip envtest)")
		return 1
	}
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	dir, err := CRDDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	te := &envtest.Environment{CRDDirectoryPaths: []string{dir}, ErrorIfCRDPathMissing: true}
	cfg, err := te.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := te.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
	}()
	s, err := NewScheme()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		return 1
	}
	*env = &Env{Config: cfg, Client: c, Scheme: s}
	return m.Run()
}

// Need skips the test when envtest is not running (-short).
func Need(t testing.TB, env *Env) {
	t.Helper()
	if env == nil {
		t.Skip("envtest not running (-short)")
	}
}

// Namespace creates a fresh namespace for one test and returns its name.
func Namespace(t testing.TB, env *Env) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pb-test-"}}
	if err := env.Client.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating a namespace: %v", err)
	}
	return ns.Name
}

// NewManager builds a manager for one test: the scheme, a cache scoped to
// namespace, no metrics or probe listeners, no leader election, and
// controller name validation off (each test registers the same controller
// names again).
func NewManager(t testing.TB, env *Env, namespace string) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Config, ctrl.Options{
		Scheme:                 env.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	return mgr
}

// StartManager runs mgr until the test ends, waits for its cache to sync,
// and fails the test if it stops with an error.
func StartManager(t testing.TB, mgr ctrl.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager stopped with an error: %v", err)
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the manager's cache did not sync")
	}
}
