package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PhoneDisplay is when a phone's screen is on: its power saving
// (Phone.spec.display, whose documentation explains the schedule and the
// house default). The install's house default (settings.json
// phoneDefaults.display) has the same shape.
type PhoneDisplay struct {
	// IdleTimeout is how long the screen stays on, outside the on-window,
	// after the last key press: whole minutes from 1m to 23h59m, written as
	// a duration ("1m", "90m", "1h30m"). The 79x5's displayIdleTimeout.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=24
	// +kubebuilder:validation:Pattern=`^([0-9]{1,5}h([0-9]{1,5}m)?([0-9]{1,5}s)?|[0-9]{1,5}m([0-9]{1,5}s)?|[0-9]{1,5}s)$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('^([0-9]{1,5}h([0-9]{1,5}m)?([0-9]{1,5}s)?|[0-9]{1,5}m([0-9]{1,5}s)?|[0-9]{1,5}s)$') || (duration(self) >= duration('1m') && duration(self) <= duration('23h59m') && duration(self).getSeconds() % 60 == 0)",message="idleTimeout must be whole minutes from 1m to 23h59m"
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// WakeOnCall turns the screen on when a call rings while it is off.
	// The 79x5's displayOnWhenIncomingCall.
	// +optional
	WakeOnCall *bool `json:"wakeOnCall,omitempty"`

	// OnWindow is the daily window the screen stays on. Unset means no day
	// has one.
	// +optional
	OnWindow *DisplayOnWindow `json:"onWindow,omitempty"`
}

// DisplayOnWindow is the daily window a phone's screen stays on: from Start,
// for Duration, on each of Days.
type DisplayOnWindow struct {
	// Start is when the screen turns on, as HH:MM on a 24-hour clock in the
	// install's time zone ("07:30"). The 79x5's displayOnTime.
	// +required
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`

	// Duration is how long it stays on: whole minutes from 1m to 24h,
	// written as a duration ("10h30m"). The 79x5's displayOnDuration.
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=24
	// +kubebuilder:validation:Pattern=`^([0-9]{1,5}h([0-9]{1,5}m)?([0-9]{1,5}s)?|[0-9]{1,5}m([0-9]{1,5}s)?|[0-9]{1,5}s)$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('^([0-9]{1,5}h([0-9]{1,5}m)?([0-9]{1,5}s)?|[0-9]{1,5}m([0-9]{1,5}s)?|[0-9]{1,5}s)$') || (duration(self) >= duration('1m') && duration(self) <= duration('24h') && duration(self).getSeconds() % 60 == 0)",message="duration must be whole minutes from 1m to 24h"
	Duration metav1.Duration `json:"duration"`

	// Days are the days with an on-window, each at most once. On every
	// other day the screen is off but for idleTimeout after a key press.
	// The 79x5 takes the days without one (daysDisplayNotActive).
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	Days []Weekday `json:"days"`
}

// Weekday is a day of the week, in English with a capital letter.
//
// +kubebuilder:validation:Enum=Monday;Tuesday;Wednesday;Thursday;Friday;Saturday;Sunday
type Weekday string

// The days of the week, Monday first.
const (
	Monday    Weekday = "Monday"
	Tuesday   Weekday = "Tuesday"
	Wednesday Weekday = "Wednesday"
	Thursday  Weekday = "Thursday"
	Friday    Weekday = "Friday"
	Saturday  Weekday = "Saturday"
	Sunday    Weekday = "Sunday"
)

// Weekdays lists the days of the week, Monday first.
func Weekdays() []Weekday {
	return []Weekday{Monday, Tuesday, Wednesday, Thursday, Friday, Saturday, Sunday}
}

// Bounds of the display durations, as the schema's rules state them.
const (
	// DisplayIdleTimeoutMax is the longest idleTimeout (23h59m): the 79x5
	// writes it as HH:MM.
	DisplayIdleTimeoutMax = 23*time.Hour + 59*time.Minute
	// DisplayOnDurationMax is the longest onWindow.duration: a whole day.
	DisplayOnDurationMax = 24 * time.Hour
)

var displayStartPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Validate checks what the schema's rules check, for a display that did
// not come through the API server (the house default in the install's
// settings): idleTimeout and onWindow.duration are whole minutes within
// their bounds, onWindow.start is HH:MM, and onWindow.days names at least
// one day, each a Weekday and none twice. A nil display is valid. Errors
// name the field from the display down ("onWindow.start").
func (d *PhoneDisplay) Validate() error {
	if d == nil {
		return nil
	}
	var errs []error
	if d.IdleTimeout != nil {
		if err := checkMinutes(d.IdleTimeout.Duration, DisplayIdleTimeoutMax); err != nil {
			errs = append(errs, fmt.Errorf("idleTimeout: %w", err))
		}
	}
	if w := d.OnWindow; w != nil {
		if !displayStartPattern.MatchString(w.Start) {
			errs = append(errs, fmt.Errorf("onWindow.start: %q is not HH:MM on a 24-hour clock (00:00 to 23:59)", w.Start))
		}
		if err := checkMinutes(w.Duration.Duration, DisplayOnDurationMax); err != nil {
			errs = append(errs, fmt.Errorf("onWindow.duration: %w", err))
		}
		if len(w.Days) == 0 {
			errs = append(errs, errors.New("onWindow.days: no day; name at least one (or leave onWindow out)"))
		}
		seen := map[Weekday]bool{}
		for _, day := range w.Days {
			switch {
			case !day.Valid():
				errs = append(errs, fmt.Errorf("onWindow.days: %q is not a day (Monday to Sunday)", day))
			case seen[day]:
				errs = append(errs, fmt.Errorf("onWindow.days: %s is listed twice", day))
			}
			seen[day] = true
		}
	}
	return errors.Join(errs...)
}

// Valid reports whether the day is one of Weekdays.
func (w Weekday) Valid() bool {
	for _, d := range Weekdays() {
		if w == d {
			return true
		}
	}
	return false
}

// checkMinutes accepts whole minutes from 1m to most.
func checkMinutes(d, most time.Duration) error {
	if d < time.Minute || d > most || d%time.Minute != 0 {
		return fmt.Errorf("%v is not whole minutes from 1m to %s", d, shortDuration(most))
	}
	return nil
}

// shortDuration writes a whole-minute duration without the zero seconds
// (23h59m, not 23h59m0s).
func shortDuration(d time.Duration) string {
	h, m := d/time.Hour, (d%time.Hour)/time.Minute
	switch {
	case m == 0:
		return fmt.Sprintf("%dh", h)
	case h == 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
