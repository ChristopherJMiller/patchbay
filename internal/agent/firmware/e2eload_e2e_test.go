//go:build e2e

package firmware_test

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// TestE2ELoad: the e2e build's test load, uploaded from
// cisco79x5.E2ELoadFile, is stored and served, as e2e will do through the
// agent; a body that is not Cisco's file is refused under Cisco's own load
// by its pinned SHA-256 (422), and nothing is stored.
func TestE2ELoad(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	m, err := firmware.NewManifest(reg)
	if err != nil {
		t.Fatal(err)
	}
	d := firmware.NewDisk(filepath.Join(t.TempDir(), "firmware"), m, logr.Discard())
	cisco, err := reg.ForModel("cisco-7945")
	if err != nil {
		t.Fatal(err)
	}
	load, ok := drivers.LoadByName(cisco.Firmware(), "E2E45.0-0-1S")
	if !ok {
		t.Fatal("the e2e build has no e2e load")
	}
	for _, f := range load.Files {
		b := cisco79x5.E2ELoadFile(f.Name)
		if code, resp := d.Put(context.Background(), load.Name, f.Name, bytes.NewReader(b)); code != http.StatusCreated || !resp.Stored {
			t.Fatalf("Put %s = %d %+v, want 201", f.Name, code, resp)
		}
		_, got, err := openBytes(t, d, f.Name)
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("Open %s: %v", f.Name, err)
		}
		if !present(d.Status(), load.Name, f.Name) {
			t.Errorf("Status: %s not present", f.Name)
		}
	}
	if code, resp := d.Put(context.Background(), cisco79x5.DefaultLoad, "apps45.9-4-2SR4-3.sbn", bytes.NewReader([]byte("x"))); code != http.StatusUnprocessableEntity || resp.Stored {
		t.Errorf("a wrong body for Cisco's file = %d %+v, want 422, not stored", code, resp)
	}
	if present(d.Status(), cisco79x5.DefaultLoad, "apps45.9-4-2SR4-3.sbn") {
		t.Error("Status: the refused file is present")
	}
}
