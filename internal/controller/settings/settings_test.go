package settings_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// chartDefaults is testdata/chart-defaults.json: the settings.json the chart
// must render with its default values, the ci minimal values
// (pbx.address 192.0.2.10) and the release name "patchbay". test/chart
// checks the chart's rendering against the same file.
const chartDefaults = "testdata/chart-defaults.json"

func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(chartDefaults)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseChartDefaults(t *testing.T) {
	got, err := settings.Parse(readFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	want := compile.Settings{
		PBXAddress:      "192.0.2.10",
		SIPPort:         5060,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  8088,
		NTPServer:       "192.168.20.1",
		Timezone:        "UTC",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{"192.168.20.0/24", "192.168.0.0/24"},
		AMISecret:       compile.SecretRef{Name: "patchbay-ami", Key: "password"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(chart defaults) =\n%+v\nwant\n%+v", got, want)
	}
	if err := install.CheckSettings(got, install.NewNames("patchbay")); err != nil {
		t.Errorf("the chart's defaults fail CheckSettings for fullname patchbay: %v", err)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	raw := strings.Replace(string(readFixture(t)), `"sipPort": 5060,`, `"sipPort": 5060, "sipPortt": 5061,`, 1)
	_, err := settings.Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "sipPortt") {
		t.Fatalf("Parse with an unknown field = %v, want an error naming it", err)
	}
	// A nested unknown field too.
	raw = strings.Replace(string(readFixture(t)), `"first": 10000,`, `"first": 10000, "step": 2,`, 1)
	if _, err := settings.Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "step") {
		t.Fatalf("Parse with an unknown nested field = %v, want an error naming it", err)
	}
}

func TestParseRejectsTrailingData(t *testing.T) {
	for name, tail := range map[string]string{
		"second document": `{}`,
		"garbage":         `x`,
		"second value":    `1`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := append(readFixture(t), []byte("\n"+tail)...)
			if _, err := settings.Parse(raw); err == nil || !strings.Contains(err.Error(), "trailing") {
				t.Fatalf("Parse with trailing %q = %v, want a trailing-data error", tail, err)
			}
		})
	}
	// Trailing white space is fine.
	if _, err := settings.Parse(append(readFixture(t), []byte("\n\n  \t\n")...)); err != nil {
		t.Fatalf("trailing white space: %v", err)
	}
}

func TestParseValidates(t *testing.T) {
	raw := strings.Replace(string(readFixture(t)), `"sipPort": 5060`, `"sipPort": 0`, 1)
	_, err := settings.Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "sipPort") {
		t.Fatalf("Parse with sipPort 0 = %v, want an error naming sipPort", err)
	}
	for name, raw := range map[string]string{
		"empty":     ``,
		"null":      `null`,
		"empty doc": `{}`,
		"array":     `[]`,
		"wrong type": strings.Replace(string(readFixture(t)),
			`"sipPort": 5060`, `"sipPort": "5060"`, 1),
	} {
		if _, err := settings.Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%s) passed", name)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, install.SettingsKey)
	if err := os.WriteFile(path, readFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := settings.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := settings.Parse(readFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}

	missing := filepath.Join(dir, "nope", install.SettingsKey)
	if _, err := settings.Load(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("Load(missing) = %v, want an error naming %s", err, missing)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"sipPort": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Load(invalid) = %v, want an error naming %s", err, bad)
	}
}
