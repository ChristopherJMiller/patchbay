package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// NewEnv builds the real Env from a kubeconfig.
//
// A non-empty kubeconfig is that file and nothing else: a missing or broken
// file is an error naming it, never a fallback to another config. An empty
// one means client-go's default chain ($KUBECONFIG, else ~/.kube/config),
// but never the in-cluster config the deferred loader falls back to when
// that chain finds nothing, and never its migration of an old ~/.kube file.
//
// The namespace is f.Namespace, else the current context's, else
// "default". Kube is a controller-runtime client over the built-in types
// and v1alpha1; Agent forwards a local port to the PBX pod's agent
// (pods/portforward, see AgentOpener); Registry is builtin.Registry();
// Clock is the real clock. Nothing is contacted until a command runs.
func NewEnv(kubeconfig string, f ClientFlags, out, errOut io.Writer) (*Env, error) {
	raw, err := loadKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return newEnv(clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}), f, out, errOut)
}

// loadKubeconfig reads the raw kubeconfig: the explicit file, or the
// default chain without the in-cluster fallback.
func loadKubeconfig(path string) (*clientcmdapi.Config, error) {
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	if path == "" {
		rules = clientcmd.NewDefaultClientConfigLoadingRules()
		// Migration copies ~/.kube/.kubeconfig to ~/.kube/config: a client
		// command never writes a kubeconfig.
		rules.MigrationRules = nil
	}
	raw, err := rules.Load()
	switch {
	case err == nil:
		return raw, nil
	case path != "" && errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("kubeconfig %s does not exist", path)
	case path != "":
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	default:
		return nil, fmt.Errorf("loading the kubeconfig: %w", err)
	}
}

// newEnv builds the Env from a client config that is already loaded (no
// file is read here, so tests pass an in-memory one).
func newEnv(cc clientcmd.ClientConfig, f ClientFlags, out, errOut io.Writer) (*Env, error) {
	cfg, err := cc.ClientConfig()
	if clientcmd.IsEmptyConfig(err) {
		return nil, errors.New("no kubeconfig found: pass --kubeconfig, or set $KUBECONFIG")
	}
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	ns := f.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, fmt.Errorf("kubeconfig: namespace: %w", err)
		}
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	reg, err := builtin.Registry()
	if err != nil {
		return nil, err
	}
	return &Env{
		Kube:      kube,
		Namespace: ns,
		Instance:  f.Instance,
		Agent: &podAgentOpener{
			kube:      kube,
			namespace: ns,
			instance:  f.Instance,
			forward:   spdyForwarder(cfg, errOut),
		},
		Registry: reg,
		Clock:    clock.RealClock{},
		Out:      out,
		Err:      errOut,
	}, nil
}

// newScheme holds what the client commands read: the built-in types (Pods,
// Secrets) and v1alpha1. It is built here because cli must not import
// internal/controller.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("scheme: %w", err)
	}
	return s, nil
}
