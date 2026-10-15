package provision_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
)

// TestGrantNeverPrintsItsToken: however a grant reaches a log line or a
// document, its token stays out.
func TestGrantNeverPrintsItsToken(t *testing.T) {
	const token = "not-a-secret-token-0123456789AB"
	g := provision.Grant{
		Device: "softphone-den", Token: token, File: "provisioning/softphone-den.xml",
		BundleVersion: strings.Repeat("a", 64), Version: strings.Repeat("b", 64),
	}
	var logged strings.Builder
	log := funcr.New(func(prefix, args string) { logged.WriteString(prefix + args + "\n") }, funcr.Options{})
	log.Info("granted", "grant", g, "grants", []provision.Grant{g})

	js, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		fmt.Sprint(g), fmt.Sprintf("%v %+v %#v %s", g, g, g, g),
		fmt.Sprintf("%v %+v", []provision.Grant{g}, &g),
		string(js), logged.String(),
	} {
		if strings.Contains(out, token) {
			t.Errorf("the token leaked: %s", out)
		}
		if !strings.Contains(out, "softphone-den") {
			t.Errorf("the device is missing: %s", out)
		}
	}
}
