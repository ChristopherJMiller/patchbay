package cli

import (
	"context"
	"io"
	"net/http"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Env is everything a client command reaches outside the process. NewEnv
// builds the real one from a kubeconfig; tests fill it with fakes (the
// controller-runtime fake client, an httptest.Server behind an
// AgentOpener, a fake clock, buffers), so no test needs a cluster.
//
// The commands read only through Env: never os.Stdout, time.Now, a
// kubeconfig or the network directly.
type Env struct {
	// Kube reads Phones, Softphones, People, Streams, Pods and Secrets and
	// patches the request annotations, all in Namespace. Its scheme holds
	// the built-in types and v1alpha1 (built here: cli must not import
	// internal/controller).
	Kube client.Client

	// Namespace is the PBX's namespace.
	Namespace string

	// Agent opens the agent's control API: a pods/portforward to the PBX
	// pod in production, an httptest.Server in tests. Commands that do not
	// talk to the agent never call it.
	Agent AgentOpener

	// Registry is the driver registry: builtin.Registry() in production (an
	// e2e-tagged binary has the e2e test load), a registry of test drivers
	// with generated manifests in tests.
	Registry *drivers.Registry

	// Clock gives the request annotations' times and paces the --reissue
	// wait (k8s.io/utils/clock; tests use clock/testing.FakeClock).
	Clock clock.Clock

	// Out receives the command's result; Err its warnings and progress.
	Out, Err io.Writer
}

// ClientFlags are the flags every client command shares.
type ClientFlags struct {
	// Namespace is -n/--namespace: the PBX's namespace. Empty means the
	// kubeconfig context's namespace, else "default".
	Namespace string
	// Instance is --instance: the app.kubernetes.io/instance label of the
	// PBX pod, needed only when the namespace holds more than one install.
	Instance string
}

// AgentOpener opens the agent's control API.
type AgentOpener interface {
	// OpenAgent finds the PBX pod (labels LabelPartOf=PartOf and
	// LabelComponent=ComponentPBX, and the instance when given; exactly one
	// Running pod), reads the control API token from the Secret its
	// AgentTokenVolume mounts (key install.AgentTokenKey), and forwards a
	// local port to the AgentContainer's ControlPortName port. The caller
	// closes the result.
	OpenAgent(ctx context.Context) (*AgentConn, error)
}

// AgentConn is an open way to the agent's control API. Every request
// carries "Authorization: Bearer " + Token. String and GoString redact the
// token, so printing an AgentConn never shows it.
type AgentConn struct {
	// BaseURL is the control API's base, e.g. http://127.0.0.1:41234.
	BaseURL string
	// Token is the control API bearer token.
	Token string
	// HTTP makes the requests: no timeout of its own (a firmware file can
	// take a while through a port-forward); each request carries its
	// context.
	HTTP *http.Client
	// Close ends the port-forward. Never nil.
	Close func() error
}

// String describes the connection without its token.
func (c AgentConn) String() string { return "agent at " + c.BaseURL + " (token redacted)" }

// GoString is String, so %#v redacts the token too.
func (c AgentConn) GoString() string { return c.String() }

// How the CLI finds the agent in the chart's PBX pod (the chart's names;
// test/chart keeps them in step).
const (
	// LabelPartOf and PartOf select patchbay's pods.
	LabelPartOf = "app.kubernetes.io/part-of"
	PartOf      = "patchbay"
	// LabelComponent and ComponentPBX select the PBX pod.
	LabelComponent = "app.kubernetes.io/component"
	ComponentPBX   = "pbx"
	// LabelInstance selects one install (ClientFlags.Instance).
	LabelInstance = "app.kubernetes.io/instance"
	// AgentContainer is the agent's container (a native sidecar: an init
	// container with restartPolicy Always).
	AgentContainer = "agent"
	// ControlPortName is the agent container's control API port.
	ControlPortName = "control"
	// AgentTokenVolume is the pod volume holding the control API token's
	// Secret.
	AgentTokenVolume = "agent-token"
)
