//go:build e2e

package cisco79x5_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// TestManifestHasE2ELoad is TestManifestHasNoE2ELoad's twin: the e2e build
// has the generated test load, genuinely verified.
func TestManifestHasE2ELoad(t *testing.T) {
	loads := cisco79x5.New().Firmware()
	if err := drivers.ValidateFirmware(loads); err != nil {
		t.Fatal(err)
	}
	if _, err := drivers.NewRegistry(cisco79x5.New()); err != nil {
		t.Fatal(err)
	}
	l, ok := drivers.LoadByName(loads, "E2E45.0-0-1S")
	if !ok {
		t.Fatalf("no e2e load in %v", loads)
	}
	if !slices.Equal(l.Models, []string{"cisco-7945", "cisco-7965"}) || l.Source != "patchbay e2e test load (generated)" {
		t.Errorf("load = %+v", l)
	}
	var names []string
	for _, f := range l.Files {
		names = append(names, f.Name)
		b := cisco79x5.E2ELoadFile(f.Name)
		sum := sha256.Sum256(b)
		if f.SHA256 != hex.EncodeToString(sum[:]) || f.Size != int64(len(b)) || f.Shared {
			t.Errorf("%s: digest %s size %d shared %v; want the digest and size of E2ELoadFile, not shared", f.Name, f.SHA256, f.Size, f.Shared)
		}
	}
	if !slices.Equal(names, []string{"E2E45.0-0-1S.loads", "appsE2E.0-0-1.sbn"}) {
		t.Errorf("files = %v", names)
	}
	if len(l.PendingFiles()) != 0 {
		t.Error("the e2e load has pending digests")
	}
}
