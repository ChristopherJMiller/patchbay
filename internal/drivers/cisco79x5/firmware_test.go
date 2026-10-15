package cisco79x5_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

func TestManifest(t *testing.T) {
	loads := cisco79x5.New().Firmware()
	if err := drivers.ValidateFirmware(loads); err != nil {
		t.Fatal(err)
	}
	l, ok := drivers.LoadByName(loads, cisco79x5.DefaultLoad)
	if !ok {
		t.Fatalf("no load %s", cisco79x5.DefaultLoad)
	}
	if l.Source != "Cisco's cmterm-7945_7965-sip.9-4-2SR4-3.zip" {
		t.Errorf("source = %q", l.Source)
	}
	if !slices.Equal(l.Models, []string{"cisco-7945", "cisco-7965"}) {
		t.Errorf("models = %v", l.Models)
	}
	// Sizes from docs/research/2026-10-14-cisco-7945-firmware.md.
	type file struct {
		name   string
		size   int64
		shared bool
	}
	want := []file{
		{"SIP45.9-4-2SR4-3S.loads", 673, false},
		{"apps45.9-4-2SR4-3.sbn", 4637904, false},
		{"cnu45.9-4-2SR4-3.sbn", 582170, false},
		{"cvm45sip.9-4-2SR4-3.sbn", 2693976, false},
		{"dsp45.9-4-2SR4-3.sbn", 364907, false},
		{"jar45sip.9-4-2SR4-3.sbn", 2260034, false},
		{"term45.default.loads", 673, true},
		{"term65.default.loads", 673, true},
	}
	var got []file
	for _, f := range l.Files {
		got = append(got, file{f.Name, f.Size, f.Shared})
		if !f.Pending() {
			t.Errorf("%s has digest %q; no genuine digest is known yet, so it must be pending", f.Name, f.SHA256)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if !slices.Equal(l.PendingFiles(), ciscoFiles) {
		t.Errorf("pending = %v", l.PendingFiles())
	}

	// Firmware returns a copy.
	loads[0].Files[0].SHA256 = strings.Repeat("0", 64)
	loads[0].Models[0] = "changed"
	if again := cisco79x5.New().Firmware(); !again[0].Files[0].Pending() || again[0].Models[0] != "cisco-7945" {
		t.Error("Firmware returns shared data")
	}
	d := cisco79x5.New()
	fw := d.Firmware()
	fw[0].Files[0].SHA256 = strings.Repeat("0", 64)
	if !d.Firmware()[0].Files[0].Pending() {
		t.Error("Firmware returns the driver's own manifest")
	}
}

func TestE2ELoadFile(t *testing.T) {
	loads := cisco79x5.E2ELoadFile("E2E45.0-0-1S.loads")
	if string(loads) != "patchbay e2e firmware E2E45.0-0-1S.loads\n" {
		t.Errorf(".loads = %q", loads)
	}
	sbn := cisco79x5.E2ELoadFile("appsE2E.0-0-1.sbn")
	line := "patchbay e2e firmware appsE2E.0-0-1.sbn\n"
	if len(sbn) != 4096 || !bytes.HasPrefix(sbn, []byte(line+line)) || !bytes.HasPrefix([]byte(strings.Repeat(line, 200)), sbn) {
		t.Errorf(".sbn is %d bytes, not 4096 bytes of the repeated line", len(sbn))
	}
	if !bytes.Equal(sbn, cisco79x5.E2ELoadFile("appsE2E.0-0-1.sbn")) {
		t.Error("E2ELoadFile is not deterministic")
	}
}
