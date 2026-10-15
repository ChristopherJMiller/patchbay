package cli_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

const tokenSecret = "patchbay-agent-token"

// pbxPod is a PBX pod as the chart makes it: the labels, the agent init
// container with its control port, the agent-token volume.
func pbxPod(name, instance string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			Labels: map[string]string{
				cli.LabelPartOf: cli.PartOf, cli.LabelComponent: cli.ComponentPBX, cli.LabelInstance: instance,
				"app.kubernetes.io/name": "patchbay",
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "asterisk-modules"},
				{Name: cli.AgentContainer, Ports: []corev1.ContainerPort{
					{Name: "device-http", ContainerPort: 8088},
					{Name: cli.ControlPortName, ContainerPort: agentapi.DefaultPort},
				}},
			},
			Containers: []corev1.Container{{Name: "asterisk"}},
			Volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: cli.AgentTokenVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: tokenSecret}}},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func tokenSecretObj(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecret, Namespace: testNamespace},
		Data:       map[string][]byte{install.AgentTokenKey: []byte(value)},
	}
}

// fakeForward forwards to an httptest.Server and records what it was asked.
type fakeForward struct {
	addr string
	err  error

	mu      sync.Mutex
	calls   []string // "<namespace>/<pod>:<port>"
	stopped int
}

func (f *fakeForward) forward(_ context.Context, namespace, pod string, port int32) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, namespace+"/"+pod+":"+strconv.Itoa(int(port)))
	if f.err != nil {
		return "", nil, f.err
	}
	return f.addr, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}, nil
}

func TestOpenAgent(t *testing.T) {
	scheme, err := cli.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	terminating := pbxPod("pbx-old", "home", corev1.PodRunning)
	terminating.DeletionTimestamp = &metav1.Time{Time: testNow}
	terminating.Finalizers = []string{"test/keep"}
	controller := pbxPod("controller", "home", corev1.PodRunning)
	controller.Labels[cli.LabelComponent] = "controller"
	noAgent := pbxPod("pbx", "home", corev1.PodRunning)
	noAgent.Spec.InitContainers = noAgent.Spec.InitContainers[:1]
	noPort := pbxPod("pbx", "home", corev1.PodRunning)
	noPort.Spec.InitContainers[1].Ports = noPort.Spec.InitContainers[1].Ports[:1]
	noVolume := pbxPod("pbx", "home", corev1.PodRunning)
	noVolume.Spec.Volumes = noVolume.Spec.Volumes[:1]

	const token = "not-a-secret-control-api-token"
	for _, tc := range []struct {
		name       string
		objs       []client.Object
		instance   string
		forwardErr error
		wantPod    string // the pod forwarded to; empty: an error
		wantErr    []string
	}{
		{
			name:    "one running pod",
			objs:    []client.Object{pbxPod("pbx", "home", corev1.PodRunning), controller, tokenSecretObj(token + "\n")},
			wantPod: "pbx",
		},
		{
			name: "pending and terminating pods are not running",
			objs: []client.Object{
				pbxPod("pbx-new", "home", corev1.PodPending), terminating, pbxPod("pbx", "home", corev1.PodRunning),
				tokenSecretObj(token),
			},
			wantPod: "pbx",
		},
		{
			name:    "no pod",
			objs:    []client.Object{controller, tokenSecretObj(token)},
			wantErr: []string{"found 0 running PBX pods", "namespace " + testNamespace, "--instance"},
		},
		{
			name:     "no pod of the instance",
			objs:     []client.Object{pbxPod("pbx", "home", corev1.PodRunning), tokenSecretObj(token)},
			instance: "office",
			wantErr:  []string{"found 0 running PBX pods", "--instance office"},
		},
		{
			name: "two installs",
			objs: []client.Object{
				pbxPod("pbx-b", "office", corev1.PodRunning), pbxPod("pbx-a", "home", corev1.PodRunning), tokenSecretObj(token),
			},
			wantErr: []string{"found 2 running PBX pods", "pbx-a, pbx-b", "--instance"},
		},
		{
			name: "two installs, --instance",
			objs: []client.Object{
				pbxPod("pbx-b", "office", corev1.PodRunning), pbxPod("pbx-a", "home", corev1.PodRunning), tokenSecretObj(token),
			},
			instance: "office",
			wantPod:  "pbx-b",
		},
		{
			name:    "no agent container",
			objs:    []client.Object{noAgent, tokenSecretObj(token)},
			wantErr: []string{"pod pbx has no agent init container"},
		},
		{
			name:    "no control port",
			objs:    []client.Object{noPort, tokenSecretObj(token)},
			wantErr: []string{"no port named control"},
		},
		{
			name:    "no token volume",
			objs:    []client.Object{noVolume, tokenSecretObj(token)},
			wantErr: []string{"no agent-token Secret volume"},
		},
		{
			name:    "no token Secret",
			objs:    []client.Object{pbxPod("pbx", "home", corev1.PodRunning)},
			wantErr: []string{"agent token Secret " + tokenSecret},
		},
		{
			name:    "empty token",
			objs:    []client.Object{pbxPod("pbx", "home", corev1.PodRunning), tokenSecretObj(" \n")},
			wantErr: []string{"Secret " + tokenSecret + " has no token"},
		},
		{
			name:       "forward fails",
			objs:       []client.Object{pbxPod("pbx", "home", corev1.PodRunning), tokenSecretObj(token)},
			forwardErr: errors.New("port-forward to pod pbx: upgrade refused"),
			wantErr:    []string{"upgrade refused"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			fwd := &fakeForward{addr: addr, err: tc.forwardErr}
			opener := cli.NewPodAgentOpener(kube, testNamespace, tc.instance, fwd.forward)
			conn, err := opener.OpenAgent(context.Background())
			if tc.wantPod == "" {
				if err == nil {
					_ = conn.Close()
					t.Fatal("no error")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if strings.Contains(err.Error(), token) {
					t.Errorf("the error carries the token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{testNamespace + "/" + tc.wantPod + ":9440"}; strings.Join(fwd.calls, " ") != strings.Join(want, " ") {
				t.Errorf("forwarded %v, want %v", fwd.calls, want)
			}
			if conn.BaseURL != "http://"+addr {
				t.Errorf("BaseURL %q, want http://%s", conn.BaseURL, addr)
			}
			if conn.Token != token {
				t.Errorf("token is not the Secret's value, trimmed")
			}
			req, _ := http.NewRequest(http.MethodGet, conn.BaseURL+"/redirect", nil)
			req.Header.Set("Authorization", "Bearer "+conn.Token)
			res, err := conn.HTTP.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusFound {
				t.Errorf("a redirect was followed (status %d)", res.StatusCode)
			}
			if gotAuth != "Bearer "+token {
				t.Errorf("the server saw Authorization %q", gotAuth)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			if fwd.stopped != 1 {
				t.Errorf("forward stopped %d times, want once", fwd.stopped)
			}
		})
	}
}

// The production forwarder fails cleanly when the API server refuses the
// upgrade, and gives up on its own when the server never answers.
func TestSPDYForwarder(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer srv.Close()
		forward := cli.SPDYForwarder(&rest.Config{Host: srv.URL}, io.Discard)
		addr, stop, err := forward(context.Background(), testNamespace, "pbx", agentapi.DefaultPort)
		if err == nil {
			stop()
			t.Fatalf("forwarded to %s through a server that refuses", addr)
		}
		if !strings.Contains(err.Error(), "port-forward to pod pbx") {
			t.Errorf("error %q does not name the pod", err)
		}
	})
	t.Run("hangs", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		forward := cli.SPDYForwarder(&rest.Config{Host: srv.URL}, io.Discard)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err := forward(ctx, testNamespace, "pbx", agentapi.DefaultPort)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v, want the context's deadline", err)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("returned after %s", waited)
		}
	})
}
