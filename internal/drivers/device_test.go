package drivers_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestDeviceID(t *testing.T) {
	for _, tc := range []struct {
		kind drivers.DeviceKind
		name string
		want string
	}{
		{drivers.KindPhone, "desk", "phone-desk"},
		{drivers.KindSoftphone, "chris-laptop", "softphone-chris-laptop"},
		{drivers.KindPhone, "upstairs.office", "phone-upstairs.office"},
		// 30 characters exactly (the 79x5's authName limit): kept whole.
		{drivers.KindPhone, strings.Repeat("a", 24), "phone-" + strings.Repeat("a", 24)},
		{drivers.KindSoftphone, strings.Repeat("a", 20), "softphone-" + strings.Repeat("a", 20)},
		// One more: the name is cut, and 8 hex digits of its SHA-256 added.
		{drivers.KindPhone, strings.Repeat("a", 25), "phone-" + strings.Repeat("a", 15) + "-2f521e2a"},
		{drivers.KindSoftphone, strings.Repeat("a", 21), "softphone-" + strings.Repeat("a", 11) + "-7df8e299"},
	} {
		if got := drivers.DeviceID(tc.kind, tc.name); got != tc.want {
			t.Errorf("DeviceID(%s, %q) = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}

	// Longer IDs are cut to 30 with a hash of the whole name, so two long
	// names that share a prefix still differ, and a cut never leaves a
	// trailing dot or dash before the hash. A Phone keeps 15 characters of
	// the name, a Softphone 11: the dash and dot below fall at the last kept
	// character of one or the other.
	id := regexp.MustCompile(`^(phone|softphone)-[a-z0-9]([-a-z0-9.]*[a-z0-9])?-[0-9a-f]{8}$`)
	seen := map[string]string{}
	for _, name := range []string{
		strings.Repeat("a", 31),
		strings.Repeat("a", 253),
		strings.Repeat("a", 252) + "b",
		strings.Repeat("a", 14) + "-" + strings.Repeat("b", 20),
		strings.Repeat("a", 14) + "." + strings.Repeat("b", 20),
		strings.Repeat("a", 10) + "-" + strings.Repeat("b", 20),
		strings.Repeat("a", 10) + "." + strings.Repeat("b", 20),
	} {
		for _, kind := range []drivers.DeviceKind{drivers.KindPhone, drivers.KindSoftphone} {
			got := drivers.DeviceID(kind, name)
			if len(got) > drivers.DeviceIDMaxLength || !id.MatchString(got) {
				t.Errorf("DeviceID(%s, %d chars) = %q (%d chars)", kind, len(name), got, len(got))
			}
			if prev, dup := seen[got]; dup {
				t.Errorf("DeviceID collision: %q for %q and %q", got, prev, name)
			}
			seen[got] = name
			if again := drivers.DeviceID(kind, name); again != got {
				t.Errorf("DeviceID is not deterministic: %q then %q", got, again)
			}
		}
	}
}

func at(minutes int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, 10, 14, 12, minutes, 0, 0, time.UTC))
	return &t
}

func TestPhoneDevice(t *testing.T) {
	p := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "desk",
			Annotations: map[string]string{v1alpha1.AnnotationResyncRequested: "2026-10-14T12:00:00Z"},
		},
		Spec: v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "d0c78915e8c8", Number: "100", Label: "Desk",
			Display: &v1alpha1.PhoneDisplay{IdleTimeout: &metav1.Duration{Duration: time.Minute}, OnWindow: &v1alpha1.DisplayOnWindow{
				Start: "07:30", Duration: metav1.Duration{Duration: time.Hour}, Days: []v1alpha1.Weekday{v1alpha1.Monday}}}},
		Status: v1alpha1.PhoneStatus{
			State: "Registered",
			Observed: &v1alpha1.PhoneObserved{
				FirstSeen: at(1), Firmware: "SIP45.9-4-2SR4-3S", Address: "192.168.20.100",
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: "7c1e", At: *at(2)},
				LastRegistered:  at(3), UpgradeStartedAt: at(0),
			},
			LastAction:     &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: "7c1e", At: *at(2), Attempts: 1},
			HandledRequest: "2026-10-13T09:00:00Z",
		},
	}
	got := drivers.PhoneDevice(p)
	want := drivers.Device{
		Kind: drivers.KindPhone, Name: "desk", ID: "phone-desk", Model: "cisco-7945",
		MAC: "D0C78915E8C8", Number: "100", Label: "Desk",
		Display: p.Spec.Display.DeepCopy(),
		Request: "2026-10-14T12:00:00Z", LastState: drivers.StateRegistered,
		Memory: drivers.Memory{
			FirstSeen: at(1), Firmware: "SIP45.9-4-2SR4-3S", Address: "192.168.20.100",
			LastConfigFetch: &v1alpha1.ConfigFetch{Version: "7c1e", At: *at(2)},
			LastRegistered:  at(3), UpgradeStartedAt: at(0),
			LastAction:     &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: "7c1e", At: *at(2), Attempts: 1},
			HandledRequest: "2026-10-13T09:00:00Z",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PhoneDevice:\n got %+v\nwant %+v", got, want)
	}

	// Memory is a copy: a driver changing it does not touch the resource.
	got.Memory.LastConfigFetch.Version = "changed"
	got.Memory.LastAction.Attempts = 9
	*got.Memory.FirstSeen = *at(59)
	got.Display.OnWindow.Days[0] = v1alpha1.Sunday
	got.Display.IdleTimeout.Duration = time.Hour
	if p.Status.Observed.LastConfigFetch.Version != "7c1e" || p.Status.LastAction.Attempts != 1 ||
		!p.Status.Observed.FirstSeen.Equal(at(1)) ||
		p.Spec.Display.OnWindow.Days[0] != v1alpha1.Monday || p.Spec.Display.IdleTimeout.Duration != time.Minute {
		t.Fatal("PhoneDevice's Memory or Display aliases the resource")
	}

	// Writing the memory back gives the status it came from.
	var st v1alpha1.PhoneStatus
	drivers.PhoneDevice(p).Memory.ToPhoneStatus(&st)
	if !reflect.DeepEqual(st.Observed, p.Status.Observed) || !reflect.DeepEqual(st.LastAction, p.Status.LastAction) ||
		st.HandledRequest != p.Status.HandledRequest {
		t.Fatalf("ToPhoneStatus did not round trip:\n got %+v\nwant %+v", st, p.Status)
	}
	// Empty memory clears observed rather than writing an empty object.
	st.Observed = &v1alpha1.PhoneObserved{Address: "stale"}
	drivers.Memory{}.ToPhoneStatus(&st)
	if st.Observed != nil || st.LastAction != nil || st.HandledRequest != "" {
		t.Fatalf("empty memory wrote %+v", st)
	}

	// Defaults: no label means the resource name; an over-long request is
	// no request.
	p.Spec.Label = ""
	p.Annotations[v1alpha1.AnnotationResyncRequested] = strings.Repeat("x", drivers.RequestMaxLength+1)
	p.Spec.Display = nil
	p.Status = v1alpha1.PhoneStatus{}
	d := drivers.PhoneDevice(p)
	if d.Label != "desk" || d.Display != nil || d.Request != "" || d.LastState != "" || !reflect.DeepEqual(d.Memory, drivers.Memory{}) {
		t.Fatalf("defaults: %+v", d)
	}
	p.Annotations[v1alpha1.AnnotationResyncRequested] = strings.Repeat("x", drivers.RequestMaxLength)
	if d := drivers.PhoneDevice(p); len(d.Request) != drivers.RequestMaxLength {
		t.Fatal("a request of exactly RequestMaxLength characters was dropped")
	}
}

func TestSoftphoneDevice(t *testing.T) {
	s := &v1alpha1.Softphone{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chris-laptop",
			Annotations: map[string]string{
				v1alpha1.AnnotationReissueRequested: "r1",
				v1alpha1.AnnotationResyncRequested:  "not a softphone's annotation",
			},
		},
		Spec: v1alpha1.SoftphoneSpec{Number: "150", Label: "Chris (laptop)"},
		Status: v1alpha1.SoftphoneStatus{
			State:      "Offline",
			Observed:   &v1alpha1.SoftphoneObserved{FirstSeen: at(1), Address: "192.168.0.31", LastRegistered: at(5)},
			LastAction: &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: *at(4), Attempts: 1},
		},
	}
	got := drivers.SoftphoneDevice(s)
	want := drivers.Device{
		Kind: drivers.KindSoftphone, Name: "chris-laptop", ID: "softphone-chris-laptop",
		Model: drivers.ModelSoftphone, Number: "150", Label: "Chris (laptop)",
		Request: "r1", LastState: drivers.StateOffline,
		Memory: drivers.Memory{
			FirstSeen: at(1), Address: "192.168.0.31", LastRegistered: at(5),
			LastAction: &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: *at(4), Attempts: 1},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SoftphoneDevice:\n got %+v\nwant %+v", got, want)
	}

	// A softphone keeps no firmware memory.
	got.Memory.Firmware, got.Memory.UpgradeStartedAt = "x", at(9)
	var st v1alpha1.SoftphoneStatus
	got.Memory.ToSoftphoneStatus(&st)
	if !reflect.DeepEqual(st.Observed, s.Status.Observed) || !reflect.DeepEqual(st.LastAction, s.Status.LastAction) {
		t.Fatalf("ToSoftphoneStatus:\n got %+v\nwant %+v", st, s.Status)
	}
}
