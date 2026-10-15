package intaketest_test

import (
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
)

func TestHouse(t *testing.T) {
	in := intaketest.House()
	in.AddSoftphone("den", "151", "Den")
	r := in.Resolved(t)
	if len(r.Bundle.Devices) != 3 || len(r.Tokens) != 2 {
		t.Errorf("devices %d, tokens %d", len(r.Bundle.Devices), len(r.Tokens))
	}
	data := in.SecretData(t, r.Bundle)
	if len(data) != len(in.Values()) {
		t.Errorf("%d projected keys for %d secrets", len(data), len(in.Values()))
	}
	cm := intaketest.ConfigMap(t, "ns", "cm", r.Bundle)
	if cm.Data["bundle.json"] == "" || cm.Namespace != "ns" {
		t.Errorf("ConfigMap = %+v", cm.ObjectMeta)
	}
	if s := intaketest.Secret("ns", "s", "7", data); s.ResourceVersion != "7" || len(s.Data) != len(data) {
		t.Errorf("Secret = %+v", s.ObjectMeta)
	}
	if v := intaketest.Version('a'); !layout.ValidVersion(v) || strings.Trim(v, "a") != "" {
		t.Errorf("Version('a') = %q", v)
	}
}
