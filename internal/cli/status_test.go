package cli_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
)

// statusURL is laptop's provisioning URL: status must never print it.
const statusURL = "http://192.168.0.240:8088/p/not-a-secret-provisioning-token2"

// statusObjects is a small install in every state the output formats.
func statusObjects() []client.Object {
	created := testNow.Add(-24 * time.Hour)
	healthy := []metav1.Condition{
		condition(v1alpha1.ConditionReady, metav1.ConditionTrue, "Registered", "registered from 192.168.20.10"),
		condition(v1alpha1.ConditionFirmwareAvailable, metav1.ConditionTrue, "Staged", ""),
		condition(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionTrue, "Running", ""),
		condition(v1alpha1.ConditionProvisioned, metav1.ConditionTrue, "Fetched", ""),
		condition(v1alpha1.ConditionRegistered, metav1.ConditionTrue, "Contact", ""),
		condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, "Current", ""),
		condition(v1alpha1.ConditionUnsupported, metav1.ConditionFalse, "AllSupported", ""),
		condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, "NoConflict", ""),
		condition(v1alpha1.ConditionMACConflict, metav1.ConditionFalse, "NoConflict", ""),
		condition(v1alpha1.ConditionLabelShortened, metav1.ConditionFalse, "AsWritten", ""),
	}
	desk := &v1alpha1.Phone{
		ObjectMeta: objectMeta("desk", created),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100", Label: "Desk"},
		Status:     v1alpha1.PhoneStatus{State: "Registered", Conditions: healthy},
	}
	kitchen := &v1alpha1.Phone{
		ObjectMeta: objectMeta("kitchen", created),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00002", Number: "101", Label: "Kitchen Phone"},
		Status: v1alpha1.PhoneStatus{
			State: "NeedsFirmware",
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionFalse, "NeedsFirmware", "stage the load"),
				condition(v1alpha1.ConditionFirmwareAvailable, metav1.ConditionFalse, "NotStaged", "missing: a.sbn"),
				condition(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionFalse, "OtherLoad", "runs SCCP45.9-4-2SR4-3S"),
				condition(v1alpha1.ConditionProvisioned, metav1.ConditionTrue, "Fetched", ""),
				condition(v1alpha1.ConditionRegistered, metav1.ConditionUnknown, "AgentUnreachable", ""),
				condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, "Current", ""),
				condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, "NoConflict", ""),
				condition(v1alpha1.ConditionLabelShortened, metav1.ConditionTrue, "Shortened", "the screen shows KitchenPhon\nthe line shows Kitchen Phone"),
				condition("SomethingNew", metav1.ConditionTrue, "Novel", "a condition this CLI does not know"),
			},
			HumanAction: "Run patchbay firmware load <zip> with Cisco's zip.",
		},
	}
	garage := &v1alpha1.Phone{
		ObjectMeta: objectMeta("garage", created),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7965, MAC: "0200E2E00003", Number: "102"},
	}
	laptop := &v1alpha1.Softphone{
		ObjectMeta: objectMeta("laptop", created),
		Spec:       v1alpha1.SoftphoneSpec{Number: "150", Label: "Laptop"},
		Status: v1alpha1.SoftphoneStatus{
			State:           "Issued",
			ProvisioningURL: statusURL,
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionFalse, "Issued", "scan the QR code in Linphone"),
				condition(v1alpha1.ConditionProvisioned, metav1.ConditionFalse, "NotFetched", ""),
				condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyPending, ""),
			},
			HumanAction: "Scan the QR code from patchbay softphone qr laptop in Linphone.",
		},
	}
	tablet := &v1alpha1.Softphone{
		ObjectMeta: objectMeta("tablet", created.Add(time.Hour)),
		Spec:       v1alpha1.SoftphoneSpec{Number: "150"},
		Status: v1alpha1.SoftphoneStatus{
			State: "Invalid",
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionFalse, "NumberConflict", "number 150 is held by Softphone laptop, which is older"),
				condition(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, "NumberConflict", "number 150 is held by Softphone laptop, which is older"),
			},
		},
	}
	chris := &v1alpha1.Person{
		ObjectMeta: objectMeta("chris", created),
		Spec:       v1alpha1.PersonSpec{Number: "301", Name: "Chris", Rings: []v1alpha1.RingTarget{{Phone: "desk"}, {Softphone: "laptop"}}},
		Status: v1alpha1.PersonStatus{State: "Ready", Conditions: []metav1.Condition{
			condition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonRouted, "rings desk, laptop"),
			condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
		}},
	}
	radio := &v1alpha1.Stream{
		ObjectMeta: objectMeta("radio", created),
		Spec:       v1alpha1.StreamSpec{Number: "700", Name: "Radio", Source: "http://radio.invalid/stream"},
		Status: v1alpha1.StreamStatus{State: "Unavailable", Conditions: []metav1.Condition{
			condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNoStreamBridge, "release A has no stream bridge"),
			condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
		}},
	}
	other := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E000FF", Number: "199"},
	}
	return []client.Object{radio, tablet, kitchen, chris, desk, laptop, garage, other}
}

func statusObservations() agentapi.Observations {
	applyAt := testNow.Add(-time.Minute)
	doc := agentapi.Empty(testNow, agentapi.AgentStatus{
		Version:          "v0.3.0",
		StartedAt:        testNow.Add(-time.Hour),
		AppliedVersion:   "0123456789abcdef0123456789abcdef",
		LastApplyVersion: "fedcba9876543210fedcba9876543210",
		LastApplyError:   "verify: endpoint phone-kitchen is missing",
		LastApplyAt:      &applyAt,
		AsteriskUp:       true,
		AsteriskVersion:  "22.6.0",
	})
	doc.UnknownDevices = []agentapi.UnknownDevice{
		{
			MAC: "D0C78915EBB5", File: "SEPD0C78915EBB5.cnf.xml", Source: "192.168.20.31",
			FirstSeen: testNow.Add(-10 * time.Minute), LastSeen: testNow.Add(-5 * time.Second),
		},
		{
			MAC: "0200E2E10001", File: "SEP0200E2E10001.cnf.xml", Source: "192.168.20.30",
			FirstSeen: testNow.Add(-3 * time.Minute), LastSeen: testNow.Add(-3 * time.Minute),
		},
	}
	return doc
}

func TestStatus(t *testing.T) {
	te := newTestEnv(t, statusObjects()...)
	te.agent.observations = statusObservations()
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	golden(t, "status.golden", te.out.Bytes())
	if strings.Contains(te.out.String(), statusURL) || strings.Contains(te.out.String(), "not-a-secret-provisioning") {
		t.Error("status printed a provisioning URL")
	}
	if opens, _ := te.opener.counts(); opens != 1 {
		t.Errorf("opened the agent %d times, want once", opens)
	}
	if te.errOut.Len() != 0 {
		t.Errorf("stderr: %s", te.errOut)
	}
}

func TestStatusAgentDown(t *testing.T) {
	te := newTestEnv(t, statusObjects()...)
	doc := agentapi.Empty(testNow, agentapi.AgentStatus{Version: "v0.3.0", StartedAt: testNow})
	te.agent.observations = doc
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	out := te.out.String()
	for _, want := range []string{
		"Agent: v0.3.0, Asterisk down, no config applied yet\n",
		"Unknown phones: none\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusAgentUnreachable(t *testing.T) {
	te := newTestEnv(t, statusObjects()...)
	te.opener.err = errors.New("found 0 running PBX pods in namespace telephony")
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatalf("Status with no agent: %v, want exit 0", err)
	}
	out := te.out.String()
	for _, want := range []string{
		"Agent: could not ask the agent: found 0 running PBX pods in namespace telephony\n",
		"Phone desk (100): Registered\n",
		"Unknown phones: could not ask the agent: found 0 running PBX pods in namespace telephony\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Agent:") > strings.Index(out, "Phone desk") || strings.Index(out, "Phone desk") > strings.Index(out, "Unknown phones") {
		t.Errorf("sections out of order:\n%s", out)
	}
}

// The agent refuses the token: the answer's error, never the token, and
// still exit 0.
func TestStatusAgentRefuses(t *testing.T) {
	te := newTestEnv(t, statusObjects()...)
	te.opener.token = "not-a-secret-wrong-token"
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if want := "Agent: could not ask the agent: GET /v1/observations: 401 unauthorized\n"; !strings.Contains(te.out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, te.out)
	}
	if strings.Contains(te.out.String(), "not-a-secret-wrong-token") {
		t.Error("status printed the token")
	}
}

func TestStatusEmpty(t *testing.T) {
	te := newTestEnv(t)
	te.agent.observations = agentapi.Empty(testNow, agentapi.AgentStatus{
		Version: "v0.3.0", AppliedVersion: "0123456789abcdef", AsteriskUp: true, AsteriskVersion: "22.6.0",
	})
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	want := "Agent: v0.3.0, Asterisk up (22.6.0), config 0123456789ab applied\n\nUnknown phones: none\n"
	if te.out.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", te.out, want)
	}
}

// Status fails only when the resources cannot be listed, and then before it
// asks the agent.
func TestStatusListFails(t *testing.T) {
	te := newTestEnv(t)
	scheme, err := cli.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	te.Kube = interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden: cannot list softphones")
		},
	})
	err = cli.Status(context.Background(), te.Env)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("Status = %v, want the list error", err)
	}
	if opens, _ := te.opener.counts(); opens != 0 {
		t.Errorf("asked the agent %d times after failing to list", opens)
	}
}

func TestStatusAgentLine(t *testing.T) {
	for _, tc := range []struct {
		agent agentapi.AgentStatus
		want  string
	}{
		{agentapi.AgentStatus{Version: "v1", AsteriskUp: true, AsteriskVersion: "22.6.0", AppliedVersion: "abc"},
			"Agent: v1, Asterisk up (22.6.0), config abc applied"},
		{agentapi.AgentStatus{AsteriskUp: true, AppliedVersion: "0123456789abcdef"},
			"Agent: unknown version, Asterisk up, config 0123456789ab applied"},
		{agentapi.AgentStatus{Version: "v1", LastApplyVersion: "fedcba9876543210", LastApplyError: "verify failed"},
			"Agent: v1, Asterisk down, no config applied yet, last apply of fedcba987654 failed: verify failed"},
	} {
		te := newTestEnv(t)
		te.agent.observations = agentapi.Empty(testNow, tc.agent)
		if err := cli.Status(context.Background(), te.Env); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := strings.Cut(te.out.String(), "\n"); got != tc.want {
			t.Errorf("agent line %q, want %q", got, tc.want)
		}
	}
}

// What the agent answers when it is not the document: the status line says
// so, and status still succeeds.
func TestStatusAgentAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"plain-text error", http.StatusInternalServerError, "oops", "GET /v1/observations: 500 Internal Server Error"},
		{"api error", http.StatusServiceUnavailable, `{"error":"not ready"}`, "GET /v1/observations: 503 not ready"},
		{"not JSON", http.StatusOK, "<html>", "GET /v1/observations: invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			te.opener.srv = srv
			if err := cli.Status(context.Background(), te.Env); err != nil {
				t.Fatal(err)
			}
			if want := "Agent: could not ask the agent: " + tc.want; !strings.HasPrefix(te.out.String(), want) {
				t.Errorf("output %q, want it to start with %q", te.out, want)
			}
		})
	}
	t.Run("no opener", func(t *testing.T) {
		te := newTestEnv(t)
		te.Agent = nil
		if err := cli.Status(context.Background(), te.Env); err != nil {
			t.Fatal(err)
		}
		if want := "Agent: could not ask the agent: no way to reach the agent\n"; !strings.HasPrefix(te.out.String(), want) {
			t.Errorf("output %q, want it to start with %q", te.out, want)
		}
	})
}

// TestStatusPrintsNoEscapes: status text can carry what a device sent (its
// web page, its SIP user agent) or the agent reported; a terminal escape,
// carriage return or C1 control in it could rewrite the lines above, so
// status prints each control character but a newline as a space.
func TestStatusPrintsNoEscapes(t *testing.T) {
	const evil = "\x1b[2J\rspoofed\u009b[1A\x07"
	hostile := &v1alpha1.Phone{
		ObjectMeta: objectMeta("hostile", testNow),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00009", Number: "109"},
		Status: v1alpha1.PhoneStatus{State: "NeedsFirmware" + evil, HumanAction: "stage it" + evil,
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionFalse, "OtherLoad", "running SIP45"+evil+"\nsecond line"),
			}},
	}
	te := newTestEnv(t, hostile)
	doc := statusObservations()
	doc.Agent.LastApplyError = "verify" + evil
	doc.UnknownDevices[0].Source += evil
	doc.UnknownDevices[0].File += evil
	te.agent.observations = doc
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	out := te.out.String()
	if strings.ContainsAny(out, "\x1b\r\a\u009b") {
		t.Errorf("status printed a control character:\n%q", out)
	}
	for _, want := range []string{"running SIP45 [2J spoofed [1A \n    second line", "human action: stage it [2J"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
