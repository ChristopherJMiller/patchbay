package script_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// TestSourceStatusJSON pins the fake source's status document, which the
// e2e harness reads to count tuners in use.
func TestSourceStatusJSON(t *testing.T) {
	s := script.SourceStatus{
		Tuners: 2, Occupied: 1,
		Open:   map[string]int{"/auto/v5.1": 1, "/auto/v7.1": 1},
		Opened: map[string]uint64{"/auto/v5.1": 3}, Refused: 1,
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{`"tuners":2`, `"occupied":1`, `"open":{`, `"opened":{`, `"refused":1`} {
		if !strings.Contains(string(raw), f) {
			t.Errorf("no %s in %s", f, raw)
		}
	}
	if s.OpenTotal() != 2 || (script.SourceStatus{}).OpenTotal() != 0 {
		t.Errorf("OpenTotal = %d", s.OpenTotal())
	}
	if script.DefaultTunerPort != 5004 || script.SourcePathStatus != "/fake/status" {
		t.Error("the fake tuner's port or status path moved")
	}
}
