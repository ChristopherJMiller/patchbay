package controller_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/controller"
)

// The envtest suite: one real kube-apiserver + etcd (binaries from the nix
// flake via KUBEBUILDER_ASSETS) shared by every test in this package, with
// the generated CRDs installed straight from the chart.
var (
	testCfg    *rest.Config
	testClient client.Client
)

// crdDir is where `make generate` writes the CRDs. They must stay plain YAML
// (no Helm templating) so envtest can install them as they are.
var crdDir = filepath.Join("..", "..", "charts", "patchbay", "templates", "crds")

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		// Unit tests still run; envtest tests skip themselves via needEnvtest.
		os.Exit(m.Run())
	}
	os.Exit(runWithEnvtest(m))
}

func runWithEnvtest(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run inside `nix develop` (or pass -short to skip envtest)")
		return 1
	}
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
	}()

	scheme, err := controller.NewScheme()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		return 1
	}
	testCfg, testClient = cfg, c
	return m.Run()
}

// needEnvtest skips the calling test when the suite runs with -short.
func needEnvtest(t *testing.T) {
	t.Helper()
	if testClient == nil {
		t.Skip("envtest not running (-short)")
	}
}

// newNamespace creates a fresh namespace for one test.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pb-test-"}}
	if err := testClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}
