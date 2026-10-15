package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// TestPhoneRoundTrip installs nothing itself: the CRDs come from the chart
// via the suite. It creates a Phone, reads it back, and writes status through
// the status subresource the way the controller will.
func TestPhoneRoundTrip(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)

	phone := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "desk", Namespace: ns},
		Spec: v1alpha1.PhoneSpec{
			Model:    v1alpha1.PhoneModelCisco7945,
			MAC:      "d0c78915e8c8", // lower case is accepted
			Number:   "100",
			Label:    "Desk",
			Firmware: "SIP45.9-4-2SR4-3S",
		},
	}
	if err := testClient.Create(ctx, phone); err != nil {
		t.Fatalf("create Phone: %v", err)
	}

	var got v1alpha1.Phone
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(phone), &got); err != nil {
		t.Fatalf("get Phone: %v", err)
	}
	if got.Spec != phone.Spec {
		t.Fatalf("spec round trip: got %+v, want %+v", got.Spec, phone.Spec)
	}
	if got.Spec.MAC.Normalized() != "D0C78915E8C8" {
		t.Fatalf("MAC normalisation: got %q", got.Spec.MAC.Normalized())
	}

	at := metav1.NewTime(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	earlier := metav1.NewTime(at.Add(-time.Hour))
	got.Status = v1alpha1.PhoneStatus{
		State:              "ConfigStale",
		ObservedGeneration: got.Generation,
		Observed: &v1alpha1.PhoneObserved{
			FirstSeen:        &earlier,
			Firmware:         "SIP45.9-4-2SR4-3S",
			Address:          "192.168.20.100",
			LastConfigFetch:  &v1alpha1.ConfigFetch{Version: "7c1e", At: at},
			LastRegistered:   &at,
			UpgradeStartedAt: &earlier,
		},
		LastAction:     &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: "9f00", At: at, Attempts: 2},
		HumanAction:    "press Settings then **#**, or power-cycle it",
		HandledRequest: "2026-10-14T11:58:00Z",
	}
	meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionRegistered, Status: metav1.ConditionTrue, Reason: "Registered",
		ObservedGeneration: got.Generation,
	})
	want := got.Status.DeepCopy()
	if err := testClient.Status().Update(ctx, &got); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var after v1alpha1.Phone
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(phone), &after); err != nil {
		t.Fatalf("get Phone after status: %v", err)
	}
	// Conditions carry a nanosecond LastTransitionTime that the API server
	// truncates to seconds, so they are checked by type, the rest exactly.
	if after.Status.State != want.State ||
		!equality.Semantic.DeepEqual(after.Status.Observed, want.Observed) ||
		!equality.Semantic.DeepEqual(after.Status.LastAction, want.LastAction) ||
		after.Status.HumanAction != want.HumanAction ||
		after.Status.HandledRequest != want.HandledRequest ||
		!meta.IsStatusConditionTrue(after.Status.Conditions, v1alpha1.ConditionRegistered) {
		t.Fatalf("status did not round trip:\n got %+v\nwant %+v", after.Status, *want)
	}
}

// TestSamplesAreValid creates every object in config/samples, so the samples
// can never drift from the schema.
func TestSamplesAreValid(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)

	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "patchbay_v1alpha1_*.yaml"))
	if err != nil || len(paths) != 4 {
		t.Fatalf("expected 4 samples, got %v (err %v)", paths, err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &u.Object); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		u.SetNamespace(ns)
		if err := testClient.Create(ctx, u); err != nil {
			t.Errorf("%s: create: %v", filepath.Base(p), err)
		}
	}
}

// TestSchemaRejects checks the validation the CRDs carry: digit-only numbers,
// 12-hex-digit MACs, the model enum, the Person ring-target CEL rule and the
// Stream source URL rule.
func TestSchemaRejects(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	om := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: ns} }
	person := func(name string, rings ...v1alpha1.RingTarget) *v1alpha1.Person {
		return &v1alpha1.Person{ObjectMeta: om(name), Spec: v1alpha1.PersonSpec{Number: "301", Name: "Chris", Rings: rings}}
	}
	phone := func(name string, mut func(*v1alpha1.PhoneSpec)) *v1alpha1.Phone {
		p := &v1alpha1.Phone{ObjectMeta: om(name), Spec: v1alpha1.PhoneSpec{
			Model: v1alpha1.PhoneModelCisco7965, MAC: "D0C78915EBB5", Number: "101",
		}}
		mut(&p.Spec)
		return p
	}
	stream := func(name, source string) *v1alpha1.Stream {
		return &v1alpha1.Stream{ObjectMeta: om(name), Spec: v1alpha1.StreamSpec{Number: "705", Name: "KING 5", Source: source}}
	}

	for _, tc := range []struct {
		name    string
		obj     client.Object
		wantMsg string
	}{
		{"person ring sets both", person("both", v1alpha1.RingTarget{Phone: "desk", Softphone: "chris-laptop"}),
			"exactly one of phone or softphone must be set"},
		{"person ring sets neither", person("neither", v1alpha1.RingTarget{}),
			"exactly one of phone or softphone must be set"},
		{"person with no rings", person("none"), "spec.rings"},
		{"number with a letter", phone("letter", func(s *v1alpha1.PhoneSpec) { s.Number = "10a" }), "spec.number"},
		{"MAC with separators", phone("colons", func(s *v1alpha1.PhoneSpec) { s.MAC = "D0:C7:89:15:E8:C8" }), "spec.mac"},
		{"MAC too short", phone("short", func(s *v1alpha1.PhoneSpec) { s.MAC = "D0C78915E8C" }), "spec.mac"},
		{"unknown model", phone("model", func(s *v1alpha1.PhoneSpec) { s.Model = "cisco-7940" }), "spec.model"},
		{"firmware with a space", phone("fwspace", func(s *v1alpha1.PhoneSpec) { s.Firmware = "SIP45 9" }), "spec.firmware"},
		// Labels and names end up in device XML and Asterisk config: a line
		// break could inject a config line, so control characters never get in.
		{"label with a line break", phone("newline", func(s *v1alpha1.PhoneSpec) { s.Label = "Desk\n[evil]" }), "spec.label"},
		{"label with a tab", phone("tab", func(s *v1alpha1.PhoneSpec) { s.Label = "Desk\tA" }), "spec.label"},
		{"label with a C1 control", phone("c1", func(s *v1alpha1.PhoneSpec) { s.Label = "Desk\u0085" }), "spec.label"},
		{"softphone label with a NUL", &v1alpha1.Softphone{ObjectMeta: om("nul"),
			Spec: v1alpha1.SoftphoneSpec{Number: "150", Label: "a\x00b"}}, "spec.label"},
		{"person name with a carriage return", &v1alpha1.Person{ObjectMeta: om("cr"), Spec: v1alpha1.PersonSpec{
			Number: "301", Name: "Chris\r", Rings: []v1alpha1.RingTarget{{Phone: "desk"}}}}, "spec.name"},
		{"stream name with a line break", &v1alpha1.Stream{ObjectMeta: om("lf"), Spec: v1alpha1.StreamSpec{
			Number: "705", Name: "KING\n5", Source: "http://192.168.0.58:5004/auto/v5.1"}}, "spec.name"},
		{"stream source not a URL", stream("nourl", "king 5"), "source must be an absolute URL"},
		{"stream source relative", stream("relative", "/auto/v5.1"), "source must be an absolute URL"},
		// design §4 writes `number: 100`; the schema wants a string (CLAUDE.md,
		// "Deviations from the design").
		{"number as a bare YAML integer", bareNumberPhone(ns), "spec.number in body must be of type string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := testClient.Create(ctx, tc.obj)
			if err == nil {
				t.Fatalf("create succeeded; want rejection mentioning %q", tc.wantMsg)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("want an Invalid error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestSchemaAcceptsPunctuation is the other half of the control-character
// rule: quotes, ampersands, semicolons, angle brackets and non-ASCII text are
// valid labels, and escaping them is the compiler's job, not the schema's.
func TestSchemaAcceptsPunctuation(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	label := `R&D; "Lab" <1> Küche`
	p := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: ns},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "D0C78915E8C8", Number: "100", Label: label},
	}
	if err := testClient.Create(ctx, p); err != nil {
		t.Fatalf("create Phone with label %q: %v", label, err)
	}
	var got v1alpha1.Phone
	if err := testClient.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil || got.Spec.Label != label {
		t.Fatalf("label round trip: %q, %v", got.Spec.Label, err)
	}
}

// bareNumberPhone is a Phone written the way design §4's example writes it,
// with `number: 100` unquoted (an integer once parsed).
func bareNumberPhone(ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"model": "cisco-7945", "mac": "D0C78915E8C8", "number": int64(100)},
	}}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Phone"))
	u.SetNamespace(ns)
	u.SetName("bare-number")
	return u
}
