package cli_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// isolate points $KUBECONFIG at a file that does not exist and $HOME at an
// empty temporary directory, so client-go's default loading rules can never
// reach a real kubeconfig (plan 6.2.3 rule 7). It returns the directory.
//
// $KUBECONFIG must stay non-empty: client-go computes ~/.kube/config
// (clientcmd.RecommendedHomeFile) from $HOME once, at package init, so
// only $KUBECONFIG keeps NewEnv("") away from the real file.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(dir, "no-such-kubeconfig"))
	t.Setenv("HOME", dir)
	return dir
}

func TestNewEnvNeverFallsBack(t *testing.T) {
	dir := isolate(t)
	// What makes client-go's deferred loader try the in-cluster config.
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "1")

	missing := filepath.Join(dir, "missing", "kubeconfig")
	if _, err := cli.NewEnv(missing, cli.ClientFlags{}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("NewEnv(%s) = %v, want an error naming the file", missing, err)
	}
	// A path that exists but is not a kubeconfig is that path's error too.
	if _, err := cli.NewEnv(dir, cli.ClientFlags{}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("NewEnv(%s) = %v, want an error naming it", dir, err)
	}
	// No --kubeconfig, and nothing at $KUBECONFIG or ~/.kube/config: an
	// error, not the in-cluster config.
	if _, err := cli.NewEnv("", cli.ClientFlags{}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Errorf(`NewEnv("") = %v, want "no kubeconfig found"`, err)
	}
	// $KUBECONFIG naming the temporary directory itself: an error too.
	t.Setenv("KUBECONFIG", dir)
	if _, err := cli.NewEnv("", cli.ClientFlags{}, io.Discard, io.Discard); err == nil {
		t.Error(`NewEnv("") with $KUBECONFIG a directory: no error`)
	}
	// Loading never writes under $HOME (client-go's ~/.kube migration).
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("$HOME holds %v after NewEnv (%v), want nothing", entries, err)
	}
}

// newEnv is tested on an in-memory config: no test loads a kubeconfig file.
func TestNewEnvFromClientConfig(t *testing.T) {
	isolate(t)
	raw := func(namespace string) clientcmdapi.Config {
		c := clientcmdapi.NewConfig()
		c.Clusters["kind"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:1"}
		c.AuthInfos["user"] = &clientcmdapi.AuthInfo{Token: "not-a-secret-kube-token"}
		c.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "kind", AuthInfo: "user", Namespace: namespace}
		c.CurrentContext = "ctx"
		return *c
	}
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, contextNS string
		flags           cli.ClientFlags
		wantNS          string
	}{
		{"context namespace", "telephony", cli.ClientFlags{}, "telephony"},
		{"flag wins", "telephony", cli.ClientFlags{Namespace: "pbx", Instance: "home"}, "pbx"},
		{"default", "", cli.ClientFlags{}, "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			cc := clientcmd.NewDefaultClientConfig(raw(tc.contextNS), &clientcmd.ConfigOverrides{})
			env, err := cli.NewEnvFromClientConfig(cc, tc.flags, &out, &errOut)
			if err != nil {
				t.Fatal(err)
			}
			if env.Namespace != tc.wantNS {
				t.Errorf("namespace %q, want %q", env.Namespace, tc.wantNS)
			}
			if env.Kube == nil {
				t.Error("no Kube client")
			}
			if ns, inst, ok := cli.OpenerOf(env); !ok || ns != tc.wantNS || inst != tc.flags.Instance {
				t.Errorf("agent opener (%q, %q, %v), want the port-forward opener in %q for instance %q", ns, inst, ok, tc.wantNS, tc.flags.Instance)
			}
			if env.Registry == nil || !slices.Equal(env.Registry.Models(), reg.Models()) {
				t.Errorf("registry is not builtin.Registry()")
			}
			if _, ok := env.Clock.(clock.RealClock); !ok {
				t.Errorf("clock %T, want clock.RealClock", env.Clock)
			}
			if env.Out != &out || env.Err != &errOut {
				t.Error("Out and Err are not the writers given")
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		cc := clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), &clientcmd.ConfigOverrides{})
		if _, err := cli.NewEnvFromClientConfig(cc, cli.ClientFlags{}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
			t.Errorf("empty config: %v, want no kubeconfig found", err)
		}
	})
	t.Run("broken context", func(t *testing.T) {
		c := raw("x")
		c.CurrentContext = "nope"
		cc := clientcmd.NewDefaultClientConfig(c, &clientcmd.ConfigOverrides{})
		if _, err := cli.NewEnvFromClientConfig(cc, cli.ClientFlags{}, io.Discard, io.Discard); err == nil {
			t.Error("a current context that does not exist: no error")
		}
	})
}
