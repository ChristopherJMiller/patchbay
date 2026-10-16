package drivers_test

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// TestDisplayWithDefaults: the Phone's own fields win, field by field;
// onWindow is one field; the result shares nothing with either input.
func TestDisplayWithDefaults(t *testing.T) {
	yes, no := true, false
	minute := &metav1.Duration{Duration: time.Minute}
	hour := &metav1.Duration{Duration: time.Hour}
	weekdays := &v1alpha1.DisplayOnWindow{Start: "07:30", Duration: metav1.Duration{Duration: 10*time.Hour + 30*time.Minute},
		Days: []v1alpha1.Weekday{v1alpha1.Monday, v1alpha1.Tuesday, v1alpha1.Wednesday, v1alpha1.Thursday, v1alpha1.Friday}}
	sunday := &v1alpha1.DisplayOnWindow{Start: "09:00", Duration: metav1.Duration{Duration: 2 * time.Hour},
		Days: []v1alpha1.Weekday{v1alpha1.Sunday}}
	house := &v1alpha1.PhoneDisplay{IdleTimeout: minute, WakeOnCall: &yes}

	for _, tc := range []struct {
		name       string
		own, house *v1alpha1.PhoneDisplay
		want       *v1alpha1.PhoneDisplay
	}{
		{"neither", nil, nil, nil},
		{"house only", nil, house, house},
		{"own only", &v1alpha1.PhoneDisplay{OnWindow: weekdays}, nil, &v1alpha1.PhoneDisplay{OnWindow: weekdays}},
		{"empty own takes the whole house", &v1alpha1.PhoneDisplay{}, house, house},
		{"empty own, no house: still set", &v1alpha1.PhoneDisplay{}, nil, &v1alpha1.PhoneDisplay{}},
		{"own window, house idle and wake", &v1alpha1.PhoneDisplay{OnWindow: weekdays}, house,
			&v1alpha1.PhoneDisplay{IdleTimeout: minute, WakeOnCall: &yes, OnWindow: weekdays}},
		{"own idle wins", &v1alpha1.PhoneDisplay{IdleTimeout: hour}, house,
			&v1alpha1.PhoneDisplay{IdleTimeout: hour, WakeOnCall: &yes}},
		{"own wakeOnCall false wins over true", &v1alpha1.PhoneDisplay{WakeOnCall: &no}, house,
			&v1alpha1.PhoneDisplay{IdleTimeout: minute, WakeOnCall: &no}},
		{"own window replaces the house's whole", &v1alpha1.PhoneDisplay{OnWindow: sunday},
			&v1alpha1.PhoneDisplay{OnWindow: weekdays}, &v1alpha1.PhoneDisplay{OnWindow: sunday}},
		{"house window when own has none", &v1alpha1.PhoneDisplay{IdleTimeout: hour},
			&v1alpha1.PhoneDisplay{OnWindow: weekdays}, &v1alpha1.PhoneDisplay{IdleTimeout: hour, OnWindow: weekdays}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownBefore, houseBefore := tc.own.DeepCopy(), tc.house.DeepCopy()
			got := drivers.DisplayWithDefaults(tc.own, tc.house)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DisplayWithDefaults =\n %s\nwant\n %s", show(got), show(tc.want))
			}
			if got == nil {
				return
			}
			// A copy: changing it changes neither input.
			got.IdleTimeout = &metav1.Duration{Duration: 5 * time.Minute}
			got.WakeOnCall = &no
			if got.OnWindow != nil {
				got.OnWindow.Days[0] = "changed"
				got.OnWindow.Start = "changed"
			}
			if !reflect.DeepEqual(tc.own, ownBefore) || !reflect.DeepEqual(tc.house, houseBefore) {
				t.Error("the result aliases an input")
			}
		})
	}
	if *house.WakeOnCall != true || house.IdleTimeout.Duration != time.Minute || weekdays.Days[0] != v1alpha1.Monday {
		t.Error("an input was changed")
	}
}

func show(d *v1alpha1.PhoneDisplay) string {
	if d == nil {
		return "nil"
	}
	s := "{"
	if d.IdleTimeout != nil {
		s += " idle=" + d.IdleTimeout.Duration.String()
	}
	if d.WakeOnCall != nil {
		s += map[bool]string{true: " wake=true", false: " wake=false"}[*d.WakeOnCall]
	}
	if w := d.OnWindow; w != nil {
		s += " window=" + w.Start + "+" + w.Duration.Duration.String()
		for _, day := range w.Days {
			s += "," + string(day)
		}
	}
	return s + " }"
}
