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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
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
			condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNoStreamBridge,
				`streams are disabled (streamBridge.enabled is false): dialling 700 plays "not available"`),
			condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
		}},
	}
	played := metav1.NewTime(testNow.Add(-2 * time.Minute))
	king5 := &v1alpha1.Stream{
		ObjectMeta: objectMeta("king5", created),
		Spec:       v1alpha1.StreamSpec{Number: "705", Name: "KING 5", Source: "http://192.168.0.58:5004/auto/v5.1"},
		Status: v1alpha1.StreamStatus{
			State: "Playing", Listeners: 2, TunerPool: "hdhomerun", LastPlayed: &played,
			// History: it played since, so status does not print it.
			LastFailure: &v1alpha1.StreamFailure{At: metav1.NewTime(testNow.Add(-time.Hour)), Reason: v1alpha1.StreamFailureSourceFailed, Message: "an old failure"},
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonPlaying, "2 listeners; tuner pool hdhomerun"),
				condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
			},
		},
	}
	kiro7 := &v1alpha1.Stream{
		ObjectMeta: objectMeta("kiro7", created),
		Spec:       v1alpha1.StreamSpec{Number: "707", Name: "KIRO 7", Source: "http://192.168.0.58:5004/auto/v7.1"},
		Status: v1alpha1.StreamStatus{
			State: "Busy", TunerPool: "hdhomerun",
			LastFailure: &v1alpha1.StreamFailure{At: metav1.NewTime(testNow.Add(-time.Minute)), Reason: v1alpha1.StreamFailureTunerBusy,
				Message: "pool hdhomerun (1 at a time) is playing king5 (705)"},
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonTunerBusy, "pool hdhomerun (1 at a time) is playing king5 (705)"),
				condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
			},
		},
	}
	groove := &v1alpha1.Stream{
		ObjectMeta: objectMeta("groovesalad", created),
		Spec:       v1alpha1.StreamSpec{Number: "750", Name: "Groove Salad", Source: "https://ice2.somafm.com/groovesalad-128-mp3"},
		Status: v1alpha1.StreamStatus{
			State: "Failing",
			LastFailure: &v1alpha1.StreamFailure{At: metav1.NewTime(testNow.Add(-30 * time.Second)), Reason: v1alpha1.StreamFailureSourceEnded,
				Message: "ffmpeg exited with status 0: the source ended\x1b[2J"},
			Conditions: []metav1.Condition{
				condition(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonSourceFailed, "2026-10-15T09:29:30Z: ffmpeg exited with status 0: the source ended"),
				condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
			},
		},
	}
	other := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E000FF", Number: "199"},
	}
	drone := &v1alpha1.Stream{
		ObjectMeta: objectMeta("dronezone", created),
		Spec:       v1alpha1.StreamSpec{Number: "751", Name: "Drone Zone", Source: "https://ice2.somafm.com/dronezone-128-mp3"},
		Status: v1alpha1.StreamStatus{State: "Playing", Listeners: 1, LastPlayed: &played, Conditions: []metav1.Condition{
			condition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonPlaying, "1 listener"),
			condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, v1alpha1.ReasonNoConflict, ""),
		}},
	}
	return []client.Object{drone, radio, king5, kiro7, groove, tablet, kitchen, chris, desk, laptop, garage, other}
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
	renewed := 3 * time.Second
	objs := append(statusObjects(), controllerObjects(&renewed, statusObservations().Agent.AppliedVersion, time.Hour, 0, "")...)
	te := newTestEnv(t, objs...)
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
	want := "Agent: v0.3.0, Asterisk up (22.6.0), config 0123456789ab applied\n" +
		"Controller: no controller Lease in namespace telephony: none has run here\n\nUnknown phones: none\n"
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

// controllerObjects are an install's controller-side objects: its Lease,
// renewed renewed ago (nil: no holder), pbx-config at version, and the
// bundle Events at the given ages.
func controllerObjects(renewed *time.Duration, version string, written, failed time.Duration, failNote string) []client.Object {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "patchbay" + install.LeaseSuffix, Namespace: testNamespace}}
	if renewed != nil {
		holder := "patchbay-controller-5d9c7_0a1b"
		at := metav1.NewMicroTime(testNow.Add(-*renewed))
		lease.Spec.HolderIdentity, lease.Spec.RenewTime = &holder, &at
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "patchbay-pbx-config", Namespace: testNamespace,
		Annotations: map[string]string{install.AnnotationBundleVersion: version}}}
	event := func(name, reason, note string, ago time.Duration) *eventsv1.Event {
		return &eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, EventTime: metav1.NewMicroTime(testNow.Add(-ago)),
			Reason: reason, Note: note, Regarding: corev1.ObjectReference{Kind: "ConfigMap", Namespace: testNamespace, Name: cm.Name},
		}
	}
	objs := []client.Object{lease, cm, event("written", install.EventReasonConfigWritten, "version written", written)}
	if failNote != "" {
		objs = append(objs, event("failed", install.EventReasonCompileFailed, failNote, failed))
	}
	return objs
}

// TestStatusController: status says whether a controller holds the
// install's Lease and renews it, which version it last compiled beside the
// one the agent applied, and a compile failure newer than the last
// version written; and marks a resource changed since its status was
// written. Otherwise a stopped controller, or a frozen compile, looks
// exactly like a healthy install.
func TestStatusController(t *testing.T) {
	applied := statusObservations().Agent.AppliedVersion
	seconds := func(n int) *time.Duration { d := time.Duration(n) * time.Second; return &d }
	for _, tc := range []struct {
		name string
		objs []client.Object
		want []string
	}{
		{"healthy", controllerObjects(seconds(4), applied, time.Hour, 0, ""), []string{
			"Controller patchbay: patchbay-controller-5d9c7_0a1b, lease renewed 4s ago\n",
			"Config: version 0123456789ab compiled and applied\n",
		}},
		{"stale lease", controllerObjects(seconds(90), applied, time.Hour, 0, ""), []string{
			"Controller patchbay: patchbay-controller-5d9c7_0a1b last renewed its lease 1m30s ago: it may be down, and what follows is as it last wrote it\n",
		}},
		{"no holder", controllerObjects(nil, applied, time.Hour, 0, ""), []string{
			"Controller patchbay: nobody holds the lease: nothing decides or compiles, and what follows is as last written\n",
		}},
		{"compile failing", controllerObjects(seconds(4), strings.Repeat("e", 64), time.Hour, time.Minute,
			"compile: Phone desk: cisco79x5: no time zone"), []string{
			"Config: version eeeeeeeeeeee compiled, the agent has 0123456789ab applied\n",
			"Config: compiling has failed since 2026-10-15T09:29:00Z, the version above stays: compile: Phone desk: cisco79x5: no time zone\n",
		}},
		{"an old compile failure", controllerObjects(seconds(4), applied, time.Minute, time.Hour, "compile: fixed since"), []string{
			"Config: version 0123456789ab compiled and applied\n",
		}},
		{"no lease", nil, []string{
			"Controller: no controller Lease in namespace telephony: none has run here\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, tc.objs...)
			te.agent.observations = statusObservations()
			if err := cli.Status(context.Background(), te.Env); err != nil {
				t.Fatal(err)
			}
			out := te.out.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if tc.name == "an old compile failure" && strings.Contains(out, "fixed since") {
				t.Errorf("an older compile failure than the last version written is printed:\n%s", out)
			}
		})
	}
}

// TestStatusMarksUndecidedChanges: a resource whose spec changed since
// the controller wrote its status says so beside its state.
func TestStatusMarksUndecidedChanges(t *testing.T) {
	desk := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "desk", Namespace: testNamespace, Generation: 3},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100", Label: "Desk"},
		Status:     v1alpha1.PhoneStatus{State: "Registered", ObservedGeneration: 2},
	}
	te := newTestEnv(t, desk)
	te.agent.observations = statusObservations()
	if err := cli.Status(context.Background(), te.Env); err != nil {
		t.Fatal(err)
	}
	if want := "Phone desk (100): Registered (its spec changed since; not decided again yet)\n"; !strings.Contains(te.out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, te.out)
	}
}
