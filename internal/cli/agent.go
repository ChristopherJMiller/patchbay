package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

// forwardOpenTimeout bounds how long opening a port-forward may take.
const forwardOpenTimeout = 30 * time.Second

// forwardFunc forwards a local port on 127.0.0.1 to port in a pod and
// returns the local address (host:port) and a function that ends the
// forward and waits for it to end. The production one is a pods/portforward
// through the API server (spdyForwarder); tests forward to an
// httptest.Server.
type forwardFunc func(ctx context.Context, namespace, pod string, port int32) (addr string, stop func(), err error)

// podAgentOpener is the production AgentOpener: it finds the PBX pod, reads
// the control API token from the Secret the pod mounts, and port-forwards
// to the agent's control port.
type podAgentOpener struct {
	kube      client.Reader
	namespace string
	instance  string
	forward   forwardFunc
}

// OpenAgent implements AgentOpener (see its doc).
func (o *podAgentOpener) OpenAgent(ctx context.Context) (*AgentConn, error) {
	pod, err := o.pbxPod(ctx)
	if err != nil {
		return nil, err
	}
	port, err := controlPort(pod)
	if err != nil {
		return nil, err
	}
	token, err := o.token(ctx, pod)
	if err != nil {
		return nil, err
	}
	addr, stop, err := o.forward(ctx, o.namespace, pod.Name, port)
	if err != nil {
		return nil, err
	}
	// A transport of its own, so Close drops its connections with the
	// forward. No proxy: the forward is on loopback.
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &AgentConn{
		BaseURL: "http://" + addr,
		Token:   token,
		HTTP: &http.Client{
			Transport: transport,
			// The control API never redirects; never follow one with the
			// token.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Close: sync.OnceValue(func() error {
			transport.CloseIdleConnections()
			stop()
			return nil
		}),
	}, nil
}

// pbxPod finds the one Running PBX pod of the install.
func (o *podAgentOpener) pbxPod(ctx context.Context) (*corev1.Pod, error) {
	sel := client.MatchingLabels{LabelPartOf: PartOf, LabelComponent: ComponentPBX}
	where := "namespace " + o.namespace
	if o.instance != "" {
		sel[LabelInstance] = o.instance
		where += " with --instance " + o.instance
	}
	var pods corev1.PodList
	if err := o.kube.List(ctx, &pods, client.InNamespace(o.namespace), sel); err != nil {
		return nil, fmt.Errorf("listing the PBX pods in %s: %w", where, err)
	}
	var running []corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	switch len(running) {
	case 1:
		return &running[0], nil
	case 0:
		return nil, fmt.Errorf("found 0 running PBX pods in %s: is patchbay installed and running there (-n/--namespace, --instance)?", where)
	default:
		names := make([]string, len(running))
		for i, p := range running {
			names[i] = p.Name
		}
		sort.Strings(names)
		return nil, fmt.Errorf("found %d running PBX pods in %s (%s): pass --instance <release> to choose one install",
			len(running), where, strings.Join(names, ", "))
	}
}

// controlPort is the agent init container's control API port.
func controlPort(pod *corev1.Pod) (int32, error) {
	for _, c := range pod.Spec.InitContainers {
		if c.Name != AgentContainer {
			continue
		}
		for _, p := range c.Ports {
			if p.Name == ControlPortName {
				return p.ContainerPort, nil
			}
		}
		return 0, fmt.Errorf("pod %s: the %s container has no port named %s", pod.Name, AgentContainer, ControlPortName)
	}
	return 0, fmt.Errorf("pod %s has no %s init container", pod.Name, AgentContainer)
}

// token reads the control API token from the Secret the pod's
// AgentTokenVolume mounts. Errors name the Secret and key, never the value.
func (o *podAgentOpener) token(ctx context.Context, pod *corev1.Pod) (string, error) {
	var secretName string
	for _, v := range pod.Spec.Volumes {
		if v.Name == AgentTokenVolume && v.Secret != nil {
			secretName = v.Secret.SecretName
		}
	}
	if secretName == "" {
		return "", fmt.Errorf("pod %s has no %s Secret volume", pod.Name, AgentTokenVolume)
	}
	var s corev1.Secret
	if err := o.kube.Get(ctx, client.ObjectKey{Namespace: o.namespace, Name: secretName}, &s); err != nil {
		return "", fmt.Errorf("reading the agent token Secret %s: %w", secretName, err)
	}
	token := strings.TrimSpace(string(s.Data[install.AgentTokenKey]))
	if token == "" {
		return "", fmt.Errorf("the agent token Secret %s has no %s", secretName, install.AgentTokenKey)
	}
	return token, nil
}

// spdyForwarder forwards through the API server's pods/portforward, as
// kubectl port-forward does, on 127.0.0.1 and a free local port. Its
// connection errors go to errOut.
func spdyForwarder(cfg *rest.Config, errOut io.Writer) forwardFunc {
	return func(ctx context.Context, namespace, pod string, port int32) (string, func(), error) {
		fail := func(err error) (string, func(), error) {
			return "", nil, fmt.Errorf("port-forward to pod %s: %w", pod, err)
		}
		transport, upgrader, err := spdy.RoundTripperFor(cfg)
		if err != nil {
			return fail(err)
		}
		core, err := corev1client.NewForConfig(cfg)
		if err != nil {
			return fail(err)
		}
		url := core.RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
		stopCh, ready := make(chan struct{}), make(chan struct{})
		fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)},
			stopCh, ready, io.Discard, errOut)
		if err != nil {
			return fail(err)
		}
		done := make(chan struct{})
		var fwErr error
		go func() {
			fwErr = fw.ForwardPorts()
			close(done)
		}()
		closeStop := sync.OnceFunc(func() { close(stopCh) })
		stop := func() {
			closeStop()
			<-done
		}
		ctx, cancel := context.WithTimeout(ctx, forwardOpenTimeout)
		defer cancel()
		select {
		case <-ready:
		case <-done:
			return fail(fwErr)
		case <-ctx.Done():
			// ForwardPorts may still be dialling (the dial has no
			// context): it ends on its own once the dial returns and sees
			// the stop channel closed. Do not wait for it.
			closeStop()
			return fail(ctx.Err())
		}
		ports, err := fw.GetPorts()
		if err == nil && len(ports) != 1 {
			err = fmt.Errorf("%d local ports, want 1", len(ports))
		}
		if err != nil {
			stop()
			return fail(err)
		}
		return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), stop, nil
	}
}
