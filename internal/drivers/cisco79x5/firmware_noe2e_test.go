//go:build !e2e

package cisco79x5_test

import (
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// TestManifestHasNoE2ELoad: a binary built without the e2e tag (the
// production image, `make build`) accepts no file that is not Cisco's.
func TestManifestHasNoE2ELoad(t *testing.T) {
	loads := cisco79x5.New().Firmware()
	if len(loads) != 1 || loads[0].Name != cisco79x5.DefaultLoad {
		t.Fatalf("manifest = %v, want only %s", loads, cisco79x5.DefaultLoad)
	}
	for _, l := range loads {
		if strings.Contains(l.Name, "E2E") {
			t.Errorf("the default build's manifest has the e2e load %s", l.Name)
		}
	}
}
