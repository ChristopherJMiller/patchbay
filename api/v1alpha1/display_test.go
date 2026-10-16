package v1alpha1_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func window(start string, d time.Duration, days ...v1alpha1.Weekday) *v1alpha1.DisplayOnWindow {
	return &v1alpha1.DisplayOnWindow{Start: start, Duration: metav1.Duration{Duration: d}, Days: days}
}

// TestDisplayValidate: Validate refuses what the schema's rules refuse
// (the envtest TestSchemaDisplay checks the API server against the same
// cases), and names the field.
func TestDisplayValidate(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name    string
		d       *v1alpha1.PhoneDisplay
		wantErr string // "" = valid
	}{
		{"nil", nil, ""},
		{"empty", &v1alpha1.PhoneDisplay{}, ""},
		{"idle 1m", &v1alpha1.PhoneDisplay{IdleTimeout: dur(time.Minute)}, ""},
		{"idle 23h59m", &v1alpha1.PhoneDisplay{IdleTimeout: dur(v1alpha1.DisplayIdleTimeoutMax)}, ""},
		{"wake only", &v1alpha1.PhoneDisplay{WakeOnCall: &yes}, ""},
		{"window 00:00 for 24h every day", &v1alpha1.PhoneDisplay{OnWindow: window("00:00", 24*time.Hour, v1alpha1.Weekdays()...)}, ""},
		{"window 23:59 for 1m on Sunday", &v1alpha1.PhoneDisplay{OnWindow: window("23:59", time.Minute, v1alpha1.Sunday)}, ""},

		{"idle 0", &v1alpha1.PhoneDisplay{IdleTimeout: dur(0)}, "idleTimeout: 0s is not whole minutes from 1m to 23h59m"},
		{"idle 59s", &v1alpha1.PhoneDisplay{IdleTimeout: dur(59 * time.Second)}, "idleTimeout"},
		{"idle 90s", &v1alpha1.PhoneDisplay{IdleTimeout: dur(90 * time.Second)}, "idleTimeout"},
		{"idle 24h", &v1alpha1.PhoneDisplay{IdleTimeout: dur(24 * time.Hour)}, "idleTimeout"},
		{"idle negative", &v1alpha1.PhoneDisplay{IdleTimeout: dur(-time.Minute)}, "idleTimeout"},
		{"start 7:30", &v1alpha1.PhoneDisplay{OnWindow: window("7:30", time.Hour, v1alpha1.Monday)}, "onWindow.start"},
		{"start 24:00", &v1alpha1.PhoneDisplay{OnWindow: window("24:00", time.Hour, v1alpha1.Monday)}, "onWindow.start"},
		{"start 07:60", &v1alpha1.PhoneDisplay{OnWindow: window("07:60", time.Hour, v1alpha1.Monday)}, "onWindow.start"},
		{"start empty", &v1alpha1.PhoneDisplay{OnWindow: window("", time.Hour, v1alpha1.Monday)}, "onWindow.start"},
		{"duration 0", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", 0, v1alpha1.Monday)}, "onWindow.duration: 0s is not whole minutes from 1m to 24h"},
		{"duration 24h1m", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", 24*time.Hour+time.Minute, v1alpha1.Monday)}, "onWindow.duration"},
		{"duration 61s", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", 61*time.Second, v1alpha1.Monday)}, "onWindow.duration"},
		{"no days", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", time.Hour)}, "onWindow.days: no day"},
		{"a day twice", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", time.Hour, v1alpha1.Monday, v1alpha1.Monday)}, "onWindow.days: Monday is listed twice"},
		{"not a day", &v1alpha1.PhoneDisplay{OnWindow: window("07:30", time.Hour, "monday")}, `onWindow.days: "monday" is not a day`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.d.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestWeekdays(t *testing.T) {
	days := v1alpha1.Weekdays()
	if len(days) != 7 || days[0] != v1alpha1.Monday || days[6] != v1alpha1.Sunday {
		t.Errorf("Weekdays() = %v", days)
	}
	for _, d := range days {
		if !d.Valid() {
			t.Errorf("%s is not Valid", d)
		}
	}
	for _, d := range []v1alpha1.Weekday{"", "monday", "Mon", "Funday"} {
		if d.Valid() {
			t.Errorf("%q is Valid", d)
		}
	}
	days[0] = "changed"
	if v1alpha1.Weekdays()[0] != v1alpha1.Monday {
		t.Error("Weekdays returns shared data")
	}
}
