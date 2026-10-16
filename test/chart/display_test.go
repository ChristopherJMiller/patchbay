package chart_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
)

// parsed is the settings the controller reads from a rendered chart.
func parsed(t *testing.T, m manifest) (raw map[string]any, display *v1alpha1.PhoneDisplay) {
	t.Helper()
	js := settingsJSON(t, m)
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		t.Fatal(err)
	}
	got, err := settings.Parse([]byte(js))
	if err != nil {
		t.Fatalf("the controller refuses the chart's settings: %v", err)
	}
	if err := install.CheckSettings(got, install.NewNames(release)); err != nil {
		t.Fatal(err)
	}
	return raw, got.PhoneDefaults.Display
}

// TestChartPhoneDefaults: phoneDefaults.display reaches settings.json as
// the controller reads it (compile.Settings.PhoneDefaults.Display), and
// only when set: the default render has no phoneDefaults at all, an empty
// display is kept (it is a display: no day has an on-window), and a null
// one is unset.
func TestChartPhoneDefaults(t *testing.T) {
	raw, display := parsed(t, render(t))
	if _, ok := raw["phoneDefaults"]; ok || display != nil {
		t.Errorf("the default render has phoneDefaults %v (display %+v), want none", raw["phoneDefaults"], display)
	}

	yes, no := true, false
	weekdays := []v1alpha1.Weekday{v1alpha1.Monday, v1alpha1.Tuesday, v1alpha1.Wednesday, v1alpha1.Thursday, v1alpha1.Friday}
	for file, want := range map[string]*v1alpha1.PhoneDisplay{
		"display-values.yaml": {IdleTimeout: &metav1.Duration{Duration: time.Minute}, WakeOnCall: &yes},
		"all-features-values.yaml": {IdleTimeout: &metav1.Duration{Duration: 90 * time.Minute}, WakeOnCall: &no,
			OnWindow: &v1alpha1.DisplayOnWindow{Start: "07:30", Duration: metav1.Duration{Duration: 10*time.Hour + 30*time.Minute}, Days: weekdays}},
	} {
		_, display := parsed(t, renderFile(t, filepath.Join(chartDir, "ci", file)))
		if !reflect.DeepEqual(display, want) {
			t.Errorf("%s: phoneDefaults.display = %s, want %s", file, show(display), show(want))
		}
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("pbx: {nodeName: top, address: 192.0.2.10}\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	raw, display = parsed(t, renderFile(t, write("empty.yaml", "phoneDefaults: {display: {}}\n")))
	if pd, _ := raw["phoneDefaults"].(map[string]any); pd == nil || pd["display"] == nil || display == nil ||
		!reflect.DeepEqual(*display, v1alpha1.PhoneDisplay{}) {
		t.Errorf("display {}: phoneDefaults %v, display %s; want an empty display kept", raw["phoneDefaults"], show(display))
	}
	raw, display = parsed(t, renderFile(t, write("null.yaml", "phoneDefaults: {display: null}\n")))
	if _, ok := raw["phoneDefaults"]; ok || display != nil {
		t.Errorf("display null: phoneDefaults %v, want none", raw["phoneDefaults"])
	}
	// A null field is unset; an unquoted start is still a string; a day
	// list from --set reaches the settings.
	_, display = parsed(t, renderFile(t, write("fields.yaml",
		"phoneDefaults: {display: {idleTimeout: null, onWindow: {start: 07:30, duration: 24h, days: [Sunday]}}}\n")))
	want := &v1alpha1.PhoneDisplay{OnWindow: &v1alpha1.DisplayOnWindow{Start: "07:30", Duration: metav1.Duration{Duration: 24 * time.Hour},
		Days: []v1alpha1.Weekday{v1alpha1.Sunday}}}
	if !reflect.DeepEqual(display, want) {
		t.Errorf("null idleTimeout, unquoted start: %s, want %s", show(display), show(want))
	}
	_, display = parsed(t, render(t, "phoneDefaults.display.idleTimeout=5m", "phoneDefaults.display.onWindow.start=08:00",
		"phoneDefaults.display.onWindow.duration=8h", "phoneDefaults.display.onWindow.days={Monday,Wednesday}"))
	want = &v1alpha1.PhoneDisplay{IdleTimeout: &metav1.Duration{Duration: 5 * time.Minute}, OnWindow: &v1alpha1.DisplayOnWindow{
		Start: "08:00", Duration: metav1.Duration{Duration: 8 * time.Hour}, Days: []v1alpha1.Weekday{v1alpha1.Monday, v1alpha1.Wednesday}}}
	if !reflect.DeepEqual(display, want) {
		t.Errorf("--set: %s, want %s", show(display), show(want))
	}
}

// TestChartPhoneDefaultsRefused: the chart refuses a display of the wrong
// shape (`helm template` fails, naming the field); values of the right
// shape but out of bounds render, and the controller refuses them at
// start, naming the field.
func TestChartPhoneDefaultsRefused(t *testing.T) {
	minimal := filepath.Join(chartDir, "ci", "minimal-values.yaml")
	for set, want := range map[string]string{
		"phoneDefaults.screen=off":                     "phoneDefaults.screen is not a house default",
		"phoneDefaults.display=5":                      "phoneDefaults.display must be a map",
		"phoneDefaults.display.idle=1m":                "phoneDefaults.display.idle is not a field",
		"phoneDefaults.display.wakeOnCall=yes":         `phoneDefaults.display.wakeOnCall must be true or false, not "yes"`,
		"phoneDefaults.display.onWindow=weekdays":      "phoneDefaults.display.onWindow must be a map",
		"phoneDefaults.display.onWindow.colour=red":    "phoneDefaults.display.onWindow.colour is not a field",
		"phoneDefaults.display.onWindow.days={Monday}": "phoneDefaults.display.onWindow.start is required",
		"phoneDefaults.display.onWindow.start=07:30":   "phoneDefaults.display.onWindow.duration is required",
		"phoneDefaults.display.onWindow.days=Monday":   "phoneDefaults.display.onWindow.start is required",
	} {
		_, stderr, err := helmTemplate(t, minimal, set)
		if err == nil || !strings.Contains(stderr, want) {
			t.Errorf("--set %s: err %v, stderr %q; want a failure saying %q", set, err, stderr, want)
		}
	}
	_, stderr, err := helmTemplate(t, minimal, "phoneDefaults.display.onWindow.start=07:30",
		"phoneDefaults.display.onWindow.duration=1h", "phoneDefaults.display.onWindow.days=Monday")
	if err == nil || !strings.Contains(stderr, "phoneDefaults.display.onWindow.days must be a list") {
		t.Errorf("days as a string: err %v, stderr %q; want a failure saying it must be a list", err, stderr)
	}

	for set, want := range map[string]string{
		"phoneDefaults.display.idleTimeout=30s": "phoneDefaults.display: idleTimeout: 30s is not whole minutes",
		"phoneDefaults.display.idleTimeout=60":  "missing unit",
	} {
		_, err := settings.Parse([]byte(settingsJSON(t, render(t, set))))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: the controller says %v, want %q", set, err, want)
		}
	}
}

// TestChartPhoneDefaultsRollOnlyTheController: a house default display
// changes settings.json, so checksum/settings rolls the controller; the
// PBX pod's template, checksum/asterisk-restart included, is unchanged, so
// Asterisk keeps running and no call drops.
func TestChartPhoneDefaultsRollOnlyTheController(t *testing.T) {
	plain := render(t)
	display := renderFile(t, filepath.Join(chartDir, "ci", "display-values.yaml"))
	controller := func(m manifest) string {
		return get[appsv1.Deployment](t, m, "Deployment", release+"-controller").Spec.Template.Annotations["checksum/settings"]
	}
	if controller(plain) == controller(display) {
		t.Error("a house default display left checksum/settings unchanged")
	}
	pbxPod := func(m manifest) appsv1.Deployment { return *get[appsv1.Deployment](t, m, "Deployment", release+"-pbx") }
	if a, b := pbxPod(plain), pbxPod(display); !reflect.DeepEqual(a.Spec.Template, b.Spec.Template) {
		t.Errorf("a house default display changed the PBX pod template (checksum/asterisk-restart %q -> %q)",
			a.Spec.Template.Annotations["checksum/asterisk-restart"], b.Spec.Template.Annotations["checksum/asterisk-restart"])
	}
}

func show(d *v1alpha1.PhoneDisplay) string {
	if d == nil {
		return "nil"
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err.Error()
	}
	return string(b)
}
