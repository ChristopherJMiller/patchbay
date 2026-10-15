package drivers_test

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// TestMemoryDeepCopy: the copy equals the memory and shares no pointer
// with it, for every pointer field Memory has (a field added later is
// checked too), so a Decision built from it never aliases the device's.
func TestMemoryDeepCopy(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	m := drivers.Memory{
		FirstSeen: at.DeepCopy(), Firmware: "SIP45", Address: "192.168.20.5",
		LastConfigFetch: &v1alpha1.ConfigFetch{Version: "v1", At: at}, LastRegistered: at.DeepCopy(),
		UpgradeStartedAt: at.DeepCopy(), LastAction: &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: "v1", At: at, Attempts: 1},
		HandledRequest: "r1",
	}
	c := m.DeepCopy()
	if !reflect.DeepEqual(c, m) {
		t.Fatalf("DeepCopy = %+v, want %+v", c, m)
	}
	mv, cv := reflect.ValueOf(m), reflect.ValueOf(c)
	for i := range mv.NumField() {
		f := mv.Type().Field(i)
		if mv.Field(i).IsZero() {
			t.Errorf("the test sets no %s: set every field", f.Name)
			continue
		}
		if k := f.Type.Kind(); (k == reflect.Pointer || k == reflect.Slice || k == reflect.Map) &&
			mv.Field(i).Pointer() == cv.Field(i).Pointer() {
			t.Errorf("DeepCopy shares %s", f.Name)
		}
	}
	c.LastAction.Attempts = 2
	if m.LastAction.Attempts != 1 {
		t.Error("changing the copy changed the memory")
	}
	if z := (drivers.Memory{}).DeepCopy(); !reflect.DeepEqual(z, drivers.Memory{}) {
		t.Errorf("the empty memory's copy = %+v", z)
	}
}

func TestSeconds(t *testing.T) {
	t1 := time.Date(2026, 10, 15, 12, 0, 1, 500, time.FixedZone("x", 3600))
	if got, want := drivers.FloorSecond(t1), time.Date(2026, 10, 15, 11, 0, 1, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("FloorSecond = %v, want %v in UTC", got, want)
	}
	if got, want := drivers.CeilSecond(t1), time.Date(2026, 10, 15, 11, 0, 2, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("CeilSecond = %v, want %v in UTC", got, want)
	}
	whole := time.Date(2026, 10, 15, 12, 0, 1, 0, time.UTC)
	if !drivers.CeilSecond(whole).Equal(whole) || !drivers.FloorSecond(whole).Equal(whole) {
		t.Error("a whole second moved")
	}
}

func TestLatestFetch(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	fs := []agentapi.Fetch{
		{File: "a", At: t0.Add(2 * time.Second)}, {File: "b", At: t0.Add(2 * time.Second)}, {File: "c", At: t0},
	}
	if f, ok := drivers.LatestFetch(fs, nil); !ok || f.File != "b" {
		t.Errorf("LatestFetch = %+v, %v; want b (the last of the latest)", f, ok)
	}
	if f, ok := drivers.LatestFetch(fs, func(f agentapi.Fetch) bool { return f.File == "c" }); !ok || f.File != "c" {
		t.Errorf("LatestFetch(c) = %+v, %v", f, ok)
	}
	if _, ok := drivers.LatestFetch(nil, nil); ok {
		t.Error("LatestFetch of nothing found one")
	}
}

func TestEarliestFetch(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	fs := []agentapi.Fetch{{File: "a", At: t0.Add(time.Second)}, {File: "b", At: t0}, {File: "c", At: t0}}
	if f, ok := drivers.EarliestFetch(fs); !ok || f.File != "b" {
		t.Errorf("EarliestFetch = %+v, %v; want b (the first of the earliest)", f, ok)
	}
	if _, ok := drivers.EarliestFetch(nil); ok {
		t.Error("EarliestFetch of nothing found one")
	}
}
