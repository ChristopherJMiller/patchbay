package controller_test

import (
	"context"
	"fmt"
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
		{"stream source not a URL", stream("nourl", "king5"), "source must be an absolute URL"},
		{"stream source relative", stream("relative", "/auto/v5.1"), "source must be an absolute URL"},
		// Release B (docs/plans/release-b.md §2(d)): the bridge's decoder
		// reads only http and https streams, never a playlist, and a
		// source is shown everywhere, so it holds no credentials.
		{"stream source with a space", stream("space", "http://192.168.0.58:5004/auto/v5.1 x"), "spec.source"},
		{"stream source rtsp", stream("rtsp", "rtsp://192.168.0.58/auto/v5.1"), "source must be an http or https URL"},
		{"stream source a file", stream("file", "file:///etc/passwd"), "source must be an absolute URL"},
		{"stream source with a password", stream("password", "http://admin:hunter2@192.168.0.58:5004/auto/v5.1"),
			"source must not carry credentials"},
		{"stream source with a user", stream("user", "https://chris@example.com/live"), "source must not carry credentials"},
		{"stream source a .pls playlist", stream("pls", "https://api.somafm.com/groovesalad256.pls"), "source is a playlist"},
		{"stream source a .M3U playlist", stream("m3u", "http://example.com/Radio.M3U"), "source is a playlist"},
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

// TestSchemaStreamSources is the other half of the source rules: the
// stream URLs a house uses are accepted (an HDHomeRun channel, SomaFM's
// Icecast mounts, an HLS playlist, an upper-case scheme, an IPv6 host, a
// query), and so are the status fields the controller writes.
func TestSchemaStreamSources(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	for i, src := range []string{
		"http://192.168.0.58:5004/auto/v5.1",
		"https://ice2.somafm.com/groovesalad-128-mp3",
		"https://example.com/live/playlist.m3u8",
		"HTTPS://Example.com/Live",
		"http://[2001:db8::1]:8000/radio",
		"http://fake-tuner.patchbay-e2e.svc:5004/auto/v7.1?hz=1300",
	} {
		s := &v1alpha1.Stream{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("s%d", i), Namespace: ns},
			Spec:       v1alpha1.StreamSpec{Number: v1alpha1.Number(fmt.Sprintf("7%02d", i)), Name: "Stream", Source: src},
		}
		if err := testClient.Create(ctx, s); err != nil {
			t.Errorf("create a Stream with source %q: %v", src, err)
			continue
		}
		if i > 0 {
			continue
		}
		at := metav1.NewTime(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
		s.Status = v1alpha1.StreamStatus{
			State: "Busy", ObservedGeneration: 1, Listeners: 2, TunerPool: "hdhomerun", LastPlayed: &at,
			LastFailure: &v1alpha1.StreamFailure{At: at, Reason: v1alpha1.StreamFailureTunerBusy, Message: "pool hdhomerun (1 at a time) is playing kiro7"},
		}
		if err := testClient.Status().Update(ctx, s); err != nil {
			t.Fatalf("status update: %v", err)
		}
		bad := s.DeepCopy()
		bad.Status.LastFailure.Reason = "Gremlins"
		if err := testClient.Status().Update(ctx, bad); !apierrors.IsInvalid(err) {
			t.Errorf("a failure reason outside the enum: %v, want Invalid", err)
		}
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

// TestSchemaDisplay: the API server takes a Phone's display exactly when
// v1alpha1.PhoneDisplay.Validate does (the house default in the settings is
// checked by Validate alone), and its rules refuse what the 79x5 could not
// be given: anything but whole minutes within the bounds, a start that is
// not HH:MM, no day or a day twice.
func TestSchemaDisplay(t *testing.T) {
	needEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	yes, no := true, false
	d := func(x time.Duration) *metav1.Duration { return &metav1.Duration{Duration: x} }
	win := func(start string, x time.Duration, days ...v1alpha1.Weekday) *v1alpha1.DisplayOnWindow {
		return &v1alpha1.DisplayOnWindow{Start: start, Duration: metav1.Duration{Duration: x}, Days: days}
	}
	for i, tc := range []struct {
		name    string
		display v1alpha1.PhoneDisplay
		wantMsg string // "" = accepted
	}{
		{"empty", v1alpha1.PhoneDisplay{}, ""},
		{"idle 1m and wake", v1alpha1.PhoneDisplay{IdleTimeout: d(time.Minute), WakeOnCall: &yes}, ""},
		{"idle 23h59m", v1alpha1.PhoneDisplay{IdleTimeout: d(23*time.Hour + 59*time.Minute), WakeOnCall: &no}, ""},
		{"Mon-Fri 07:30 for 10h30m", v1alpha1.PhoneDisplay{OnWindow: win("07:30", 10*time.Hour+30*time.Minute,
			v1alpha1.Monday, v1alpha1.Tuesday, v1alpha1.Wednesday, v1alpha1.Thursday, v1alpha1.Friday)}, ""},
		{"00:00 for 24h every day", v1alpha1.PhoneDisplay{OnWindow: win("00:00", 24*time.Hour, v1alpha1.Weekdays()...)}, ""},
		{"23:59 for 1m on Sunday", v1alpha1.PhoneDisplay{OnWindow: win("23:59", time.Minute, v1alpha1.Sunday)}, ""},

		{"idle 0", v1alpha1.PhoneDisplay{IdleTimeout: d(0)}, "idleTimeout must be whole minutes from 1m to 23h59m"},
		{"idle 59s", v1alpha1.PhoneDisplay{IdleTimeout: d(59 * time.Second)}, "idleTimeout must be whole minutes"},
		{"idle 90s", v1alpha1.PhoneDisplay{IdleTimeout: d(90 * time.Second)}, "idleTimeout must be whole minutes"},
		{"idle 24h", v1alpha1.PhoneDisplay{IdleTimeout: d(24 * time.Hour)}, "idleTimeout must be whole minutes"},
		{"idle 500ms", v1alpha1.PhoneDisplay{IdleTimeout: d(500 * time.Millisecond)}, "spec.display.idleTimeout"},
		{"start 7:30", v1alpha1.PhoneDisplay{OnWindow: win("7:30", time.Hour, v1alpha1.Monday)}, "spec.display.onWindow.start"},
		{"start 24:00", v1alpha1.PhoneDisplay{OnWindow: win("24:00", time.Hour, v1alpha1.Monday)}, "spec.display.onWindow.start"},
		{"start 07:60", v1alpha1.PhoneDisplay{OnWindow: win("07:60", time.Hour, v1alpha1.Monday)}, "spec.display.onWindow.start"},
		{"duration 0", v1alpha1.PhoneDisplay{OnWindow: win("07:30", 0, v1alpha1.Monday)}, "duration must be whole minutes from 1m to 24h"},
		{"duration 24h1m", v1alpha1.PhoneDisplay{OnWindow: win("07:30", 24*time.Hour+time.Minute, v1alpha1.Monday)}, "duration must be whole minutes"},
		{"duration 61s", v1alpha1.PhoneDisplay{OnWindow: win("07:30", 61*time.Second, v1alpha1.Monday)}, "duration must be whole minutes"},
		{"no days", v1alpha1.PhoneDisplay{OnWindow: win("07:30", time.Hour)}, "spec.display.onWindow.days"},
		{"a day twice", v1alpha1.PhoneDisplay{OnWindow: win("07:30", time.Hour, v1alpha1.Monday, v1alpha1.Monday)}, "spec.display.onWindow.days"},
		{"not a day", v1alpha1.PhoneDisplay{OnWindow: win("07:30", time.Hour, "monday")}, "spec.display.onWindow.days"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &v1alpha1.Phone{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("display-%d", i), Namespace: ns},
				Spec: v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "D0C78915E8C8", Number: "100",
					Display: tc.display.DeepCopy()},
			}
			err := testClient.Create(ctx, p)
			if valid := tc.display.Validate() == nil; valid != (tc.wantMsg == "") {
				t.Errorf("Validate() = %v, but the API server should %s it", tc.display.Validate(),
					map[bool]string{true: "take", false: "refuse"}[tc.wantMsg == ""])
			}
			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				var got v1alpha1.Phone
				if err := testClient.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
					t.Fatal(err)
				}
				if !equality.Semantic.DeepEqual(got.Spec.Display, p.Spec.Display) {
					t.Errorf("display read back as %+v, want %+v", got.Spec.Display, p.Spec.Display)
				}
				return
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("create = %v; want Invalid mentioning %q", err, tc.wantMsg)
			}
		})
	}

	// Durations as a person writes them: what Go's time.ParseDuration takes
	// but the schema does not (fractions, other units, a sign), and the
	// shortest forms it does.
	for i, tc := range []struct {
		idle, duration string
		ok             bool
	}{
		{"1m", "10h30m", true},
		{"90m", "1440m", true},
		{"120s", "86400s", true},
		{"1h1m", "24h0m0s", true},
		{"1.5m", "1h", false},
		{"1m", "0.5h", false},
		{"-1m", "1h", false},
		{"1d", "1h", false},
		{"1m", "1h ", false},
		{"", "1h", false},
		{"1m", "100000h", false},
	} {
		t.Run("written "+tc.idle+" "+tc.duration, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{
				"spec": map[string]any{"model": "cisco-7945", "mac": "D0C78915E8C8", "number": "100",
					"display": map[string]any{
						"idleTimeout": tc.idle,
						"onWindow":    map[string]any{"start": "07:30", "duration": tc.duration, "days": []any{"Monday"}},
					}},
			}}
			u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Phone"))
			u.SetNamespace(ns)
			u.SetName(fmt.Sprintf("written-%d", i))
			err := testClient.Create(ctx, u)
			switch {
			case tc.ok && err != nil:
				t.Errorf("create: %v", err)
			case !tc.ok && !apierrors.IsInvalid(err):
				t.Errorf("create = %v; want Invalid", err)
			}
		})
	}
}
