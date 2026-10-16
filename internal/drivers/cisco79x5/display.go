package cisco79x5

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// The screen's power saving (Phone.spec.display) as the SEP file's
// vendorConfig elements, after usecallmanager.nz's SEPMAC.cnf.xml page
// (community documented, checked 2026-10-15; plan §8 has the bench row):
//
//   - daysDisplayNotActive: the days with no on-window, comma-separated,
//     1 = Sunday to 7 = Saturday (Cisco's default 1,7);
//   - displayOnTime, displayOnDuration: the daily on-window, HH:MM (07:30
//     and 10:30);
//   - displayIdleTimeout: outside the window, how long the screen stays on
//     after a key press, HH:MM (01:00);
//   - displayOnWhenIncomingCall: 1 turns the screen on for a ringing call
//     (0).
//
// Nothing is rendered without a display (neither the Phone's nor the house
// default), so the phone keeps every Cisco default and the file is what it
// was before the feature.

// ciscoDays numbers the days as daysDisplayNotActive does.
var ciscoDays = map[v1alpha1.Weekday]int{
	v1alpha1.Sunday:    1,
	v1alpha1.Monday:    2,
	v1alpha1.Tuesday:   3,
	v1alpha1.Wednesday: 4,
	v1alpha1.Thursday:  5,
	v1alpha1.Friday:    6,
	v1alpha1.Saturday:  7,
}

// ciscoDay is the day's number in daysDisplayNotActive (1 Sunday to 7
// Saturday), 0 for a name that is not a day.
func ciscoDay(d v1alpha1.Weekday) int { return ciscoDays[d] }

// daysNotActive is daysDisplayNotActive for an on-window on days: the
// numbers of every other day, ascending, comma-separated. Empty when every
// day has the window.
func daysNotActive(days []v1alpha1.Weekday) string {
	on := map[int]bool{}
	for _, d := range days {
		on[ciscoDay(d)] = true
	}
	var off []string
	for n := 1; n <= 7; n++ {
		if !on[n] {
			off = append(off, strconv.Itoa(n))
		}
	}
	return strings.Join(off, ",")
}

// allDays is daysDisplayNotActive with no on-window on any day.
var allDays = daysNotActive(nil)

// hhmm writes a whole-minute duration as the SEP file's HH:MM: 1m is
// 00:01, 23h59m 23:59, and a whole day 24:00.
func hhmm(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", d/time.Hour, (d%time.Hour)/time.Minute)
}

// displayFor is the display a device renders: its own, over the house
// default (drivers.DisplayWithDefaults), on a model whose profile has
// FeatureDisplayPowerSave on the backend. The house default is ignored on
// a model without it; a display of the device's own is then an error
// wrapping drivers.ErrInvalidSpec (Decide's Unsupported). A display the
// schema refuses (v1alpha1.PhoneDisplay.Validate) is a plain error: the
// API server and the settings check both refuse it first.
func displayFor(dev drivers.Device, ctx drivers.RenderContext, p drivers.Profile) (*v1alpha1.PhoneDisplay, error) {
	if !hasDisplay(p, ctx.Signalling) {
		if dev.Display != nil {
			return nil, fmt.Errorf("%w: %s", drivers.ErrInvalidSpec, textNoDisplay(dev.Model, ctx.Signalling))
		}
		return nil, nil
	}
	d := drivers.DisplayWithDefaults(dev.Display, ctx.DisplayDefaults)
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("cisco79x5: display: %w", err)
	}
	return d, nil
}

// hasDisplay is whether the profile can render a display on the backend.
func hasDisplay(p drivers.Profile, s drivers.Signalling) bool {
	return p.Supports(s, drivers.FeatureDisplayPowerSave) != drivers.Unsupported
}

// sepDisplay is the display's part of the vendorConfig, embedded after
// displayRefreshRate as in usecallmanager's sample. Every element is
// omitted when unset: a pointer for the two whose zero value is a value
// (an empty day list, 0).
type sepDisplay struct {
	DaysDisplayNotActive      *string `xml:"daysDisplayNotActive,omitempty"`
	DisplayOnTime             string  `xml:"displayOnTime,omitempty"`
	DisplayOnDuration         string  `xml:"displayOnDuration,omitempty"`
	DisplayIdleTimeout        string  `xml:"displayIdleTimeout,omitempty"`
	DisplayOnWhenIncomingCall *int    `xml:"displayOnWhenIncomingCall,omitempty"`
}

// setDisplay writes the display's elements: none for a nil display;
// daysDisplayNotActive for any other, every day when it has no onWindow
// (the screen is then off but for displayIdleTimeout after a key press);
// displayOnTime and displayOnDuration with an onWindow; and
// displayIdleTimeout and displayOnWhenIncomingCall when set.
func (v *sepDisplay) setDisplay(d *v1alpha1.PhoneDisplay) {
	if d == nil {
		return
	}
	days := allDays
	if w := d.OnWindow; w != nil {
		// Every day listed leaves the element empty. Whether the phone
		// reads an empty list as no day (rather than its default 1,7) is
		// for the bench.
		days = daysNotActive(w.Days)
		v.DisplayOnTime = w.Start
		v.DisplayOnDuration = hhmm(w.Duration.Duration)
	}
	v.DaysDisplayNotActive = &days
	if d.IdleTimeout != nil {
		v.DisplayIdleTimeout = hhmm(d.IdleTimeout.Duration)
	}
	if d.WakeOnCall != nil {
		wake := 0
		if *d.WakeOnCall {
			wake = 1
		}
		v.DisplayOnWhenIncomingCall = &wake
	}
}

// textNoDisplay is Ready's and Unsupported's message, and the human action,
// for a display on a model without the feature.
func textNoDisplay(model string, s drivers.Signalling) string {
	return fmt.Sprintf("spec.display: %s has no %s on %s; remove it", model, drivers.FeatureDisplayPowerSave, s)
}
