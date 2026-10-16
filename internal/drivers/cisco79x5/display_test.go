package cisco79x5_test

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

func TestHHMM(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{time.Minute, "00:01"},
		{59 * time.Minute, "00:59"},
		{time.Hour, "01:00"},
		{10*time.Hour + 30*time.Minute, "10:30"},
		{23*time.Hour + 59*time.Minute, "23:59"},
		{24 * time.Hour, "24:00"},
	} {
		if got := cisco79x5.HHMM(tc.d); got != tc.want {
			t.Errorf("HHMM(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestCiscoDay: daysDisplayNotActive numbers the days from 1, Sunday, to
// 7, Saturday.
func TestCiscoDay(t *testing.T) {
	want := map[v1alpha1.Weekday]int{
		v1alpha1.Sunday: 1, v1alpha1.Monday: 2, v1alpha1.Tuesday: 3, v1alpha1.Wednesday: 4,
		v1alpha1.Thursday: 5, v1alpha1.Friday: 6, v1alpha1.Saturday: 7,
	}
	for _, d := range v1alpha1.Weekdays() {
		if got := cisco79x5.CiscoDay(d); got != want[d] {
			t.Errorf("CiscoDay(%s) = %d, want %d", d, got, want[d])
		}
	}
}

// TestDaysNotActive: the days without an on-window, as Cisco numbers,
// ascending, whatever order the days are listed in.
func TestDaysNotActive(t *testing.T) {
	for _, tc := range []struct {
		days []v1alpha1.Weekday
		want string
	}{
		{v1alpha1.Weekdays(), ""},
		{[]v1alpha1.Weekday{v1alpha1.Monday, v1alpha1.Tuesday, v1alpha1.Wednesday, v1alpha1.Thursday, v1alpha1.Friday}, "1,7"},
		{[]v1alpha1.Weekday{v1alpha1.Friday, v1alpha1.Monday, v1alpha1.Thursday, v1alpha1.Tuesday, v1alpha1.Wednesday}, "1,7"},
		{[]v1alpha1.Weekday{v1alpha1.Sunday}, "2,3,4,5,6,7"},
		{[]v1alpha1.Weekday{v1alpha1.Saturday}, "1,2,3,4,5,6"},
		{[]v1alpha1.Weekday{v1alpha1.Saturday, v1alpha1.Sunday}, "2,3,4,5,6"},
		{nil, "1,2,3,4,5,6,7"},
	} {
		if got := cisco79x5.DaysNotActive(tc.days); got != tc.want {
			t.Errorf("DaysNotActive(%v) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

// displayFields are the display elements of a SEP file; nil when absent.
type displayFields struct {
	DaysNotActive *string `xml:"vendorConfig>daysDisplayNotActive"`
	OnTime        *string `xml:"vendorConfig>displayOnTime"`
	OnDuration    *string `xml:"vendorConfig>displayOnDuration"`
	IdleTimeout   *string `xml:"vendorConfig>displayIdleTimeout"`
	OnWhenCall    *string `xml:"vendorConfig>displayOnWhenIncomingCall"`
}

func (f displayFields) String() string {
	s := func(p *string) string {
		if p == nil {
			return "-"
		}
		return `"` + *p + `"`
	}
	return "days " + s(f.DaysNotActive) + " on " + s(f.OnTime) + " for " + s(f.OnDuration) +
		" idle " + s(f.IdleTimeout) + " call " + s(f.OnWhenCall)
}

func str(s string) *string { return &s }

func minutes(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func onWindow(start string, d time.Duration, days ...v1alpha1.Weekday) *v1alpha1.DisplayOnWindow {
	return &v1alpha1.DisplayOnWindow{Start: start, Duration: metav1.Duration{Duration: d}, Days: days}
}

var monToFri = []v1alpha1.Weekday{v1alpha1.Monday, v1alpha1.Tuesday, v1alpha1.Wednesday, v1alpha1.Thursday, v1alpha1.Friday}

// TestRenderDisplay: what each display renders, the Phone's own fields
// over the house default's; nothing at all without either, so the SEP
// file is the one rendered before the feature existed.
func TestRenderDisplay(t *testing.T) {
	yes, no := true, false
	house := &v1alpha1.PhoneDisplay{IdleTimeout: minutes(time.Minute), WakeOnCall: &yes}
	for _, tc := range []struct {
		name       string
		own, house *v1alpha1.PhoneDisplay
		want       displayFields
	}{
		{"neither", nil, nil, displayFields{}},
		{"empty own: no day has an on-window", &v1alpha1.PhoneDisplay{}, nil,
			displayFields{DaysNotActive: str("1,2,3,4,5,6,7")}},
		{"house only", nil, house,
			displayFields{DaysNotActive: str("1,2,3,4,5,6,7"), IdleTimeout: str("00:01"), OnWhenCall: str("1")}},
		{"own window over the house's idle and wake", &v1alpha1.PhoneDisplay{OnWindow: onWindow("07:30", 10*time.Hour+30*time.Minute, monToFri...)}, house,
			displayFields{DaysNotActive: str("1,7"), OnTime: str("07:30"), OnDuration: str("10:30"), IdleTimeout: str("00:01"), OnWhenCall: str("1")}},
		{"own wake off and idle 23h59m win", &v1alpha1.PhoneDisplay{IdleTimeout: minutes(23*time.Hour + 59*time.Minute), WakeOnCall: &no}, house,
			displayFields{DaysNotActive: str("1,2,3,4,5,6,7"), IdleTimeout: str("23:59"), OnWhenCall: str("0")}},
		{"every day, all day", &v1alpha1.PhoneDisplay{OnWindow: onWindow("00:00", 24*time.Hour, v1alpha1.Weekdays()...)}, nil,
			displayFields{DaysNotActive: str(""), OnTime: str("00:00"), OnDuration: str("24:00")}},
		{"Sunday 23:59 for 1m", &v1alpha1.PhoneDisplay{OnWindow: onWindow("23:59", time.Minute, v1alpha1.Sunday)}, nil,
			displayFields{DaysNotActive: str("2,3,4,5,6,7"), OnTime: str("23:59"), OnDuration: str("00:01")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := desk()
			dev.Display = tc.own
			ctx := houseCtx()
			ctx.DisplayDefaults = tc.house
			files, err := cisco79x5.New().Render(dev, ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got displayFields
			if err := xml.Unmarshal(files["tftp/"+deskSEP], &got); err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want.String() {
				t.Errorf("rendered %s\n    want %s", got, tc.want)
			}
		})
	}

	// Without a display the SEP file is byte for byte the desk golden.
	files, err := cisco79x5.New().Render(desk(), houseCtx())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "render/desk/tftp/"+deskSEP, files["tftp/"+deskSEP])
}

// TestRenderDisplayRefused: a model whose profile lacks the feature is
// Invalid with its own display (Render's ErrInvalidSpec, Decide's
// Unsupported), and ignores the house default; a display the schema would
// refuse (the house default is checked by the settings, a Phone's by the
// API server) is a plain error.
func TestRenderDisplayRefused(t *testing.T) {
	yes := true
	without := cisco79x5.New().WithTweak(func(p *drivers.Profile) {
		p.Features[drivers.SignallingPJSIP][drivers.FeatureDisplayPowerSave] = drivers.Unsupported
	})
	dev := desk()
	dev.Display = &v1alpha1.PhoneDisplay{WakeOnCall: &yes}
	if _, err := without.Render(dev, houseCtx()); !errors.Is(err, drivers.ErrInvalidSpec) ||
		!strings.Contains(err.Error(), "displayPowerSave") {
		t.Errorf("Render of an unsupported display = %v, want ErrInvalidSpec naming displayPowerSave", err)
	}
	ctx := houseCtx()
	ctx.DisplayDefaults = &v1alpha1.PhoneDisplay{WakeOnCall: &yes}
	files, err := without.Render(desk(), ctx)
	if err != nil {
		t.Fatalf("Render with only a house default on a model without the feature: %v", err)
	}
	golden(t, "render/desk/tftp/"+deskSEP, files["tftp/"+deskSEP])

	for name, bad := range map[string]*v1alpha1.PhoneDisplay{
		"idle 90s":       {IdleTimeout: minutes(90 * time.Second)},
		"idle 24h":       {IdleTimeout: minutes(24 * time.Hour)},
		"start 24:00":    {OnWindow: onWindow("24:00", time.Hour, v1alpha1.Monday)},
		"duration 0":     {OnWindow: onWindow("07:30", 0, v1alpha1.Monday)},
		"no days":        {OnWindow: onWindow("07:30", time.Hour)},
		"a day twice":    {OnWindow: onWindow("07:30", time.Hour, v1alpha1.Monday, v1alpha1.Monday)},
		"not a day":      {OnWindow: onWindow("07:30", time.Hour, "Funday")},
		"duration 24h1m": {OnWindow: onWindow("07:30", 24*time.Hour+time.Minute, v1alpha1.Monday)},
	} {
		for _, where := range []string{"own", "house"} {
			dev, ctx := desk(), houseCtx()
			if where == "own" {
				dev.Display = bad
			} else {
				ctx.DisplayDefaults = bad
			}
			_, err := cisco79x5.New().Render(dev, ctx)
			if err == nil || errors.Is(err, drivers.ErrInvalidSpec) {
				t.Errorf("%s (%s): Render = %v, want a plain error", name, where, err)
			}
		}
	}
}
