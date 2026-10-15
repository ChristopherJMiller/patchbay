package cli

import (
	"io"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Seams for the external tests.

// NewScheme is the CLI's scheme.
var NewScheme = newScheme

// ForwardFunc is the port-forward seam of the production AgentOpener.
type ForwardFunc = forwardFunc

// NewPodAgentOpener is the production AgentOpener over a given client and
// forwarder.
func NewPodAgentOpener(kube client.Reader, namespace, instance string, forward ForwardFunc) AgentOpener {
	return &podAgentOpener{kube: kube, namespace: namespace, instance: instance, forward: forward}
}

// NewEnvFromClientConfig is NewEnv after the kubeconfig is loaded.
func NewEnvFromClientConfig(cc clientcmd.ClientConfig, f ClientFlags, out, errOut io.Writer) (*Env, error) {
	return newEnv(cc, f, out, errOut)
}

// SPDYForwarder is the production forwarder.
func SPDYForwarder(cfg *rest.Config, errOut io.Writer) ForwardFunc { return spdyForwarder(cfg, errOut) }

// OpenerOf reports the namespace and instance of an Env's production
// AgentOpener.
func OpenerOf(env *Env) (namespace, instance string, ok bool) {
	o, ok := env.Agent.(*podAgentOpener)
	if !ok {
		return "", "", false
	}
	return o.namespace, o.instance, true
}
