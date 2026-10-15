package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sounds"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

const (
	testNamespace = "telephony"
	testConfigMap = "patchbay-pbx-config"
	testSecret    = "patchbay-pbx-secrets"
	testToken     = "not-a-secret-agent-token"
)

func TestAgentRequiresItsFlags(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv(agentTokenEnv, "test-token")
	_, err := run(t, context.Background(), "agent", "--listen", "127.0.0.1:0")
	if err == nil {
		t.Fatal("agent with no namespace, bundle or device HTTP address started")
	}
	for _, want := range []string{"--namespace", "--bundle-configmap", "--secrets", "--device-http-listen", "--sip-bind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// agentArgs are every flag the agent requires.
var agentArgs = []string{
	"agent", "--listen", "127.0.0.1:0", "--namespace", testNamespace,
	"--bundle-configmap", testConfigMap, "--secrets", testSecret, "--device-http-listen", "127.0.0.1:0",
	"--sip-bind", "192.168.0.240:5060",
}

func TestAgentRequiresToken(t *testing.T) {
	t.Setenv(agentTokenEnv, "")
	_, err := run(t, context.Background(), agentArgs...)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("error = %v, want a token error", err)
	}
}

// TestAgentNeedsAKubeconfig: with every flag right, the agent goes for the
// Kubernetes API, and run's --kubeconfig does not exist.
func TestAgentNeedsAKubeconfig(t *testing.T) {
	t.Setenv(agentTokenEnv, "test-token")
	_, err := run(t, context.Background(), agentArgs...)
	if err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("error = %v, want the missing kubeconfig", err)
	}
}

// freeAddr returns a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// logBuffer collects the agent's log lines from every goroutine.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) write(prefix, args string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(prefix + " " + args + "\n")
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// agentFixture is one runAgent against a fake API server holding the
// compiled house bundle, with AMI down (nothing listens on its port).
type agentFixture struct {
	t      *testing.T
	opts   agentOptions
	bundle compile.Bundle
	logs   *logBuffer
	values []string // every secret value of the install
	cancel context.CancelFunc
	done   chan error
}

// newAgentFixture starts the agent. Without sounds the image's prompts are
// missing; change, if not nil, edits the options first.
func newAgentFixture(t *testing.T, withSounds bool, change func(*agentOptions)) *agentFixture {
	t.Helper()
	// A short directory: a unix socket's path is limited to 108 bytes.
	sock, err := os.MkdirTemp("", "pb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	persistent, src := t.TempDir(), t.TempDir()
	if withSounds {
		for _, name := range sounds.Files() {
			if err := os.WriteFile(filepath.Join(src, name), []byte("audio "+name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	house := intaketest.House()
	b := house.Compile(t)
	cs := fake.NewClientset(
		intaketest.ConfigMap(t, testNamespace, testConfigMap, b),
		intaketest.Secret(testNamespace, testSecret, "1", house.SecretData(t, b)),
	)
	f := &agentFixture{
		t: t,
		opts: agentOptions{
			listen: freeAddr(t), namespace: testNamespace, bundleConfigMap: testConfigMap, secrets: testSecret,
			liveDir: t.TempDir(), firmwareDir: filepath.Join(persistent, "firmware"),
			stateDir: filepath.Join(persistent, "state"), soundsDir: t.TempDir(), soundsSrc: src,
			fetchSocket: filepath.Join(sock, "fetches.sock"), fsGroup: -1,
			deviceHTTPListen: freeAddr(t), amiAddress: freeAddr(t), sipBind: "192.168.0.240:5060",
		},
		bundle: b, logs: &logBuffer{}, values: house.Values(), done: make(chan error, 1),
	}
	if change != nil {
		change(&f.opts)
	}
	log := funcr.New(f.logs.write, funcr.Options{Verbosity: 2})
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		f.done <- runAgent(ctx, f.opts, func() []string { return []string{testToken} }, cs, log)
	}()
	t.Cleanup(func() {
		cancel()
		if _, err := f.wait(); err != nil {
			t.Error(err)
		}
	})
	return f
}

// wait returns what runAgent returned, at most 30 s from now; it can be
// called more than once.
func (f *agentFixture) wait() (runErr, timeout error) {
	select {
	case err := <-f.done:
		f.done <- err
		return err, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("the agent did not stop within 30 s; logs:\n%s", f.logs)
	}
}

// do sends a request to the control API and returns the status and body.
func (f *agentFixture) do(method, path string, body io.Reader, token bool) (int, string) {
	f.t.Helper()
	return httpDo(f.t, method, "http://"+f.opts.listen+path, body, token)
}

func httpDo(t *testing.T, method, url string, body io.Reader, token bool) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// eventually polls cond every 50 ms for up to 10 s.
func (f *agentFixture) eventually(what string, cond func() (bool, string)) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s: never happened; last: %s\nlogs:\n%s", what, last, f.logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *agentFixture) observations() api.Observations {
	f.t.Helper()
	code, body := f.do(http.MethodGet, api.PathObservations, nil, true)
	if code != http.StatusOK {
		f.t.Fatalf("GET %s: %d %s", api.PathObservations, code, body)
	}
	var obs api.Observations
	if err := json.Unmarshal([]byte(body), &obs); err != nil {
		f.t.Fatal(err)
	}
	return obs
}

// TestAgentWiring runs the whole agent with Asterisk down: the first version
// goes live (unverified) and the agent becomes ready; the control API,
// device HTTP, the fetch socket, the firmware upload and /metrics each reach
// the component behind them; it stops cleanly when its context ends, and no
// secret reaches its log.
func TestAgentWiring(t *testing.T) {
	f := newAgentFixture(t, true, nil)
	f.eventually("ready", func() (bool, string) {
		code, body := f.do(http.MethodGet, api.PathReadyz, nil, false)
		return code == http.StatusOK, fmt.Sprint(code, " ", body)
	})

	obs := f.observations()
	if obs.Agent.AppliedVersion != f.bundle.Version || obs.Agent.AsteriskUp {
		t.Errorf("agent status %+v, want version %s applied with Asterisk down", obs.Agent, f.bundle.Version)
	}
	if code, _ := f.do(http.MethodGet, api.PathObservations, nil, false); code != http.StatusUnauthorized {
		t.Errorf("observations without the token: %d, want 401", code)
	}
	for _, name := range sounds.Files() {
		if _, err := os.Stat(filepath.Join(f.opts.soundsDir, name)); err != nil {
			t.Errorf("prompt %s not installed: %v", name, err)
		}
	}

	// Device HTTP serves the live http/ tree.
	code, dir := httpDo(t, http.MethodGet, "http://"+f.opts.deviceHTTPListen+"/directory.xml", nil, false)
	if want := string(f.bundle.Files[compile.DirectoryFile]); code != http.StatusOK || dir != want {
		t.Errorf("GET /directory.xml: %d %q", code, dir)
	}

	// A fetch the file server reports over the socket is attributed to the
	// phone whose SEP file it was, with its version.
	desk := f.bundle.Devices[drivers.DeviceID(drivers.KindPhone, "desk")]
	sep := "SEP" + desk.MAC + ".cnf.xml"
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", f.opts.fetchSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err := sensor.WriteFetchEvent(conn, sensor.FetchEvent{
		Transport: api.TransportTFTP, File: sep, Result: api.FetchServed, At: time.Now().UTC(), Source: "192.168.20.50",
		BundleVersion: f.bundle.Version, BundlePath: "tftp/" + sep,
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	f.eventually("the socket's fetch in the observations", func() (bool, string) {
		devices := f.observations().Devices
		for _, d := range devices {
			for _, fe := range d.Fetches {
				if d.Device == "phone-desk" && fe.File == sep && fe.Version == desk.Version {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%+v", devices)
	})

	// The firmware upload reaches the store: Cisco's load is pending.
	path := api.FirmwarePath(cisco79x5.DefaultLoad, "apps45.9-4-2SR4-3.sbn")
	if code, body := f.do(http.MethodPut, path, strings.NewReader("x"), true); code != http.StatusPreconditionFailed {
		t.Errorf("PUT %s: %d %s, want 412", path, code, body)
	}

	code, metricsBody := f.do(http.MethodGet, api.PathMetrics, nil, false)
	if want := `patchbay_agent_applied_version_info{version="` + f.bundle.Version + `"} 1`; code != http.StatusOK ||
		!strings.Contains(metricsBody, want) {
		t.Errorf("GET /metrics: %d, want %s in\n%s", code, want, metricsBody)
	}

	f.cancel()
	runErr, timeout := f.wait()
	if timeout != nil {
		t.Fatal(timeout)
	}
	if runErr != nil {
		t.Fatalf("the agent stopped with %v, want a clean shutdown", runErr)
	}
	logs := f.logs.String()
	for _, v := range append(f.values, testToken) {
		if strings.Contains(logs, v) {
			t.Errorf("a secret value is in the agent's log")
		}
	}
}

// TestAgentNotReadyWithoutSounds: a prompt missing from the image keeps the
// agent unready, and /readyz says why, even with config live.
func TestAgentNotReadyWithoutSounds(t *testing.T) {
	f := newAgentFixture(t, false, nil)
	f.eventually("config live", func() (bool, string) {
		code, body := f.do(http.MethodGet, api.PathObservations, nil, true)
		var obs api.Observations
		if code != http.StatusOK || json.Unmarshal([]byte(body), &obs) != nil {
			return false, fmt.Sprint(code, " ", body)
		}
		return obs.Agent.AppliedVersion == f.bundle.Version, fmt.Sprintf("%+v", obs.Agent)
	})
	code, body := f.do(http.MethodGet, api.PathReadyz, nil, false)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, compile.SoundNoEmergency) {
		t.Fatalf("/readyz = %d %q, want 503 naming the missing prompt", code, body)
	}
}

// TestAgentStopsWhenAComponentFails: a component that cannot run (device
// HTTP on an address in use) stops the whole agent with its error.
func TestAgentStopsWhenAComponentFails(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	f := newAgentFixture(t, true, func(o *agentOptions) { o.deviceHTTPListen = ln.Addr().String() })
	runErr, timeout := f.wait()
	if timeout != nil {
		t.Fatal(timeout)
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "device HTTP") {
		t.Fatalf("the agent stopped with %v, want device HTTP's error", runErr)
	}
}

// TestAgentRefusesAnotherSIPBind: in a pod whose Asterisk binds SIP on
// another port than the bundle was compiled for (pbx.ports.sip changed and
// the controller has not recompiled yet), the agent applies nothing and
// stays unready, so Asterisk never boots on transports the pod is not
// configured for; the observations say why.
func TestAgentRefusesAnotherSIPBind(t *testing.T) {
	f := newAgentFixture(t, true, func(o *agentOptions) { o.sipBind = "192.168.0.240:5070" })
	f.eventually("the bundle refused", func() (bool, string) {
		code, body := f.do(http.MethodGet, api.PathObservations, nil, true)
		var obs api.Observations
		if code != http.StatusOK || json.Unmarshal([]byte(body), &obs) != nil {
			return false, fmt.Sprint(code, " ", body)
		}
		a := obs.Agent
		return a.LastApplyVersion == f.bundle.Version && strings.Contains(a.LastApplyError, "192.168.0.240:5070"), fmt.Sprintf("%+v", a)
	})
	if a := f.observations().Agent; a.AppliedVersion != "" {
		t.Errorf("applied %s in a pod for another SIP port", a.AppliedVersion)
	}
	if code, body := f.do(http.MethodGet, api.PathReadyz, nil, false); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d %q, want 503", code, body)
	}
}

func TestAgentSIPBindMustBeAnAddress(t *testing.T) {
	t.Setenv(agentTokenEnv, "test-token")
	for _, bad := range []string{"192.168.0.240", "pbx.example:5060", "192.168.0.240:0", "192.168.0.240:sip"} {
		args := slices.Clone(agentArgs)
		args[slices.Index(args, "--sip-bind")+1] = bad
		if _, err := run(t, context.Background(), args...); err == nil || !strings.Contains(err.Error(), "--sip-bind") {
			t.Errorf("--sip-bind %q: %v, want an error naming the flag", bad, err)
		}
	}
}
