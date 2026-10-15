package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// golden compares got with testdata/<name>, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (run with -update to accept):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// The test registry's loads (plan 6.2.7): TESTA and TESTB each carry their
// own Shared term45.default.loads, and TESTP's only file is pending.
const (
	loadA      = "TESTA.1-0-0S"
	loadB      = "TESTB.2-0-0S"
	loadP      = "TESTP.0-0-0S"
	sharedName = "term45.default.loads"
)

// testFile is the generated content of one file of a test load: different
// for every (load, name), so the two Shared files differ.
func testFile(load, name string) []byte {
	return bytes.Repeat([]byte(fmt.Sprintf("patchbay cli test firmware %s/%s\n", load, name)), 50)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func manifestFile(load, name string, shared bool) drivers.FirmwareFile {
	b := testFile(load, name)
	return drivers.FirmwareFile{Name: name, SHA256: sha256Hex(b), Size: int64(len(b)), Shared: shared}
}

// fakeDriver is a driver with a generated manifest. Only Name, Models,
// Profile and Firmware matter to the CLI.
type fakeDriver struct {
	name   string
	models []string
	loads  []drivers.FirmwareLoad
}

func (d fakeDriver) Name() string     { return d.name }
func (d fakeDriver) Models() []string { return d.models }
func (d fakeDriver) Profile(model string) (drivers.Profile, bool) {
	for _, m := range d.models {
		if m == model {
			return testProfile(model), true
		}
	}
	return drivers.Profile{}, false
}
func (fakeDriver) Render(drivers.Device, drivers.RenderContext) (drivers.Files, error) {
	return nil, errors.New("not rendered in CLI tests")
}
func (fakeDriver) Decide(drivers.Device, drivers.Observations, drivers.DecideSettings, time.Time) drivers.Decision {
	return drivers.Decision{}
}
func (d fakeDriver) Firmware() []drivers.FirmwareLoad { return d.loads }

// testProfile is a profile with every field set, so the models golden shows
// each one; test-p is a software model with the zero hardware.
func testProfile(model string) drivers.Profile {
	p := drivers.Profile{
		Model: model,
		Hardware: drivers.Hardware{
			LineKeys:           2,
			Softkeys:           4,
			Display:            drivers.Size{Width: 320, Height: 240},
			ServiceArea:        drivers.Size{Width: 298, Height: 156},
			Wallpaper:          drivers.Size{Width: 320, Height: 212},
			WallpaperThumbnail: drivers.Size{Width: 80, Height: 53},
			RingtoneFormat:     "raw µ-law, 8 kHz",
			Codecs:             []string{"g722", "ulaw", "alaw"},
		},
		Provisioning: drivers.Provisioning{
			ConfigFile:      "SEP{MAC}.cnf.xml",
			DefaultFirmware: loadA,
			ScreenLabelMax:  12,
			LineLabelMax:    30,
			Password:        drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
			ResyncByNotify:  true,
		},
		Endpoint: drivers.EndpointOptions{
			Transport:          drivers.SIPTransportTCP,
			Codecs:             []string{"g722", "ulaw"},
			SendRPID:           true,
			TrustIDOutbound:    true,
			RegistrationExpiry: 600 * time.Second,
		},
		Features: map[drivers.Signalling]map[drivers.Feature]drivers.Support{
			drivers.SignallingPJSIP: {
				drivers.FeatureHold:     drivers.Supported,
				drivers.FeatureWideband: drivers.Supported,
				drivers.FeatureTransfer: drivers.Untested,
				drivers.FeatureBLF:      drivers.Unsupported,
			},
		},
	}
	switch model {
	case "test-b":
		p.Provisioning.DefaultFirmware = loadB
	case "test-p":
		p.Hardware = drivers.Hardware{Codecs: []string{"ulaw"}}
		p.Provisioning = drivers.Provisioning{
			ConfigFile:      "provisioning/{ID}.xml",
			DefaultFirmware: loadP,
			LineLabelMax:    64,
			Password:        drivers.PasswordPolicy{Length: 24, Alphabet: "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"},
		}
		p.Endpoint = drivers.EndpointOptions{
			Transport:          drivers.SIPTransportUDP,
			Codecs:             []string{"ulaw"},
			ForceRport:         true,
			RewriteContact:     true,
			RTPSymmetric:       true,
			SendPAI:            true,
			QualifyFrequency:   60 * time.Second,
			RegistrationExpiry: 300 * time.Second,
		}
		p.Features = nil
	}
	return p
}

func testRegistry(t *testing.T) *drivers.Registry {
	t.Helper()
	ab := fakeDriver{
		name:   "testab",
		models: []string{"test-b", "test-a"}, // Models sorts them
		loads: []drivers.FirmwareLoad{
			{
				Name: loadA, Source: "generated test zip A", Models: []string{"test-a"},
				Files: []drivers.FirmwareFile{
					manifestFile(loadA, "a.sbn", false),
					manifestFile(loadA, loadA+".loads", false),
					manifestFile(loadA, sharedName, true),
				},
			},
			{
				Name: loadB, Source: "generated test zip B", Models: []string{"test-b"},
				Files: []drivers.FirmwareFile{
					manifestFile(loadB, "b.sbn", false),
					manifestFile(loadB, sharedName, true),
				},
			},
		},
	}
	p := fakeDriver{
		name:   "testp",
		models: []string{"test-p"},
		loads: []drivers.FirmwareLoad{{
			Name: loadP, Source: "a zip nobody has yet", Models: []string{"test-p"},
			Files: []drivers.FirmwareFile{{Name: "p.sbn", SHA256: drivers.SHA256Pending, Size: 1234}},
		}},
	}
	reg, err := drivers.NewRegistry(ab, p)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// testAgentToken is the fake agent's bearer token: it must never appear
// in any output.
const testAgentToken = "not-a-secret-agent-token-for-cli-tests"

// dropConnection, as a fakeAgent answer, hangs up instead of answering.
const dropConnection = -1

// fakeAgent is the agent's control API: GET /v1/observations, and PUT
// /v1/firmware/<load>/<name> checked against the test registry's manifest
// as the real agent checks it (404, 412, 422, 200, 201). It checks the
// bearer token and the Content-Length.
type fakeAgent struct {
	t   *testing.T
	reg *drivers.Registry

	mu           sync.Mutex
	observations agentapi.Observations
	stored       map[string]string // "<load>/<name>" -> SHA-256
	puts         []string          // "<load>/<name>", in request order
	answer       map[string]int    // "<load>/<name>" -> forced status
	unauthorized int
}

func newFakeAgent(t *testing.T, reg *drivers.Registry) *fakeAgent {
	return &fakeAgent{t: t, reg: reg, stored: map[string]string{}, answer: map[string]int{}}
}

func (a *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testAgentToken {
		a.unauthorized++
		writeJSON(w, http.StatusUnauthorized, agentapi.Error{Error: "unauthorized"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == agentapi.PathObservations:
		writeJSON(w, http.StatusOK, a.observations)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, agentapi.PathFirmware+"/"):
		a.upload(w, r)
	default:
		writeJSON(w, http.StatusNotFound, agentapi.Error{Error: "no such endpoint"})
	}
}

func (a *fakeAgent) upload(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, agentapi.PathFirmware+"/")
	load, name, _ := strings.Cut(key, "/")
	a.puts = append(a.puts, key)
	if r.ContentLength < 0 {
		a.t.Errorf("PUT %s without a Content-Length", key)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, agentapi.MaxFirmwareFileSize+1))
	if err != nil {
		a.t.Errorf("PUT %s: reading the body: %v", key, err)
		return
	}
	if int64(len(body)) != r.ContentLength {
		a.t.Errorf("PUT %s: Content-Length %d, body %d bytes", key, r.ContentLength, len(body))
	}
	sum := sha256Hex(body)
	resp := agentapi.FirmwareUploadResponse{Load: load, Name: name, SHA256: sum, Size: int64(len(body))}
	if code, ok := a.answer[key]; ok {
		if code == dropConnection {
			panic(http.ErrAbortHandler)
		}
		resp.Error = fmt.Sprintf("injected %d", code)
		writeJSON(w, code, resp)
		return
	}
	var file *drivers.FirmwareFile
	for _, d := range a.reg.Drivers() {
		if l, ok := drivers.LoadByName(d.Firmware(), load); ok {
			for _, f := range l.Files {
				if f.Name == name {
					file = &f
				}
			}
		}
	}
	switch {
	case file == nil:
		resp.Error = "not in the manifest"
		writeJSON(w, http.StatusNotFound, resp)
	case file.Pending():
		resp.Error = "the manifest's digest is pending"
		writeJSON(w, http.StatusPreconditionFailed, resp)
	case file.SHA256 != sum:
		resp.Error = "digest mismatch"
		writeJSON(w, http.StatusUnprocessableEntity, resp)
	case a.stored[key] == sum:
		resp.Stored = true
		writeJSON(w, http.StatusOK, resp)
	default:
		a.stored[key] = sum
		resp.Stored = true
		writeJSON(w, http.StatusCreated, resp)
	}
}

func (a *fakeAgent) putsSoFar() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.puts...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeOpener is an AgentOpener in front of an httptest.Server: what the
// port-forward opener gives the commands in production.
type fakeOpener struct {
	srv   *httptest.Server
	token string
	err   error

	mu            sync.Mutex
	opens, closes int
}

func (o *fakeOpener) OpenAgent(context.Context) (*cli.AgentConn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, o.err
	}
	o.opens++
	return &cli.AgentConn{
		BaseURL: o.srv.URL,
		Token:   o.token,
		HTTP:    o.srv.Client(),
		Close: func() error {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.closes++
			return nil
		},
	}, nil
}

func (o *fakeOpener) counts() (opens, closes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens, o.closes
}

// testNamespace is where the fake client's objects live.
const testNamespace = "telephony"

// testEnv is an Env over fakes: the controller-runtime fake client, a fake
// agent behind a fake opener, a fake clock, buffers.
type testEnv struct {
	*cli.Env
	agent  *fakeAgent
	opener *fakeOpener
	clock  *clocktesting.FakeClock
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

var testNow = time.Date(2026, 10, 15, 9, 30, 0, 123456789, time.UTC)

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	scheme, err := cli.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Phone{}, &v1alpha1.Softphone{}, &v1alpha1.Person{}, &v1alpha1.Stream{}).
		Build()
	reg := testRegistry(t)
	agent := newFakeAgent(t, reg)
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	opener := &fakeOpener{srv: srv, token: testAgentToken}
	te := &testEnv{
		agent:  agent,
		opener: opener,
		clock:  clocktesting.NewFakeClock(testNow),
		out:    &bytes.Buffer{},
		errOut: &bytes.Buffer{},
	}
	te.Env = &cli.Env{
		Kube:      kube,
		Namespace: testNamespace,
		Agent:     opener,
		Registry:  reg,
		Clock:     te.clock,
		Out:       te.out,
		Err:       te.errOut,
	}
	t.Cleanup(func() {
		for _, s := range []string{te.out.String(), te.errOut.String()} {
			if strings.Contains(s, testAgentToken) {
				t.Errorf("the agent token was printed:\n%s", s)
			}
		}
		if opens, closes := opener.counts(); opens != closes {
			t.Errorf("agent opened %d times, closed %d times", opens, closes)
		}
	})
	return te
}

func objectMeta(name string, created time.Time) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace, CreationTimestamp: metav1.NewTime(created)}
}

func condition(typ string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: message,
		LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour).Truncate(time.Second)),
	}
}
