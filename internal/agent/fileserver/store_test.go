package fileserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// phoneDriver is a driver with a firmware manifest: model "phone-1"
// defaults to load DEF; OLD is another load that also carries the Shared
// term.default.loads; PEND has a pending digest.
type phoneDriver struct{ loads []drivers.FirmwareLoad }

func (phoneDriver) Name() string     { return "phone" }
func (phoneDriver) Models() []string { return []string{"phone-1"} }
func (phoneDriver) Profile(model string) (drivers.Profile, bool) {
	if model != "phone-1" {
		return drivers.Profile{}, false
	}
	return drivers.Profile{
		Model: model,
		Provisioning: drivers.Provisioning{
			DefaultFirmware: "DEF",
			Password:        drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
		},
		Endpoint: drivers.EndpointOptions{Transport: drivers.SIPTransportTCP, Codecs: []string{"ulaw"}, RegistrationExpiry: time.Hour},
	}, true
}
func (phoneDriver) Render(drivers.Device, drivers.RenderContext) (drivers.Files, error) {
	return nil, nil
}
func (phoneDriver) Decide(dev drivers.Device, _ drivers.Observations, _ drivers.DecideSettings, _ time.Time) drivers.Decision {
	return drivers.Decision{State: dev.LastState, StatusPatch: dev.Memory}
}
func (d phoneDriver) Firmware() []drivers.FirmwareLoad { return d.loads }

func fwBytes(load, name string) []byte { return generated(load+"/"+name, 1300) }

func fwFile(load, name string, shared bool) drivers.FirmwareFile {
	sum := sha256.Sum256(fwBytes(load, name))
	return drivers.FirmwareFile{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: 1300, Shared: shared}
}

// TestServesTheFirmwareStore: through the real store, both transports
// serve a verified file by name, a Shared name from the default load only,
// and never a pending or unverified file.
func TestServesTheFirmwareStore(t *testing.T) {
	reg, err := drivers.NewRegistry(phoneDriver{loads: []drivers.FirmwareLoad{
		{Name: "DEF", Models: []string{"phone-1"}, Files: []drivers.FirmwareFile{
			fwFile("DEF", "def.loads", false), fwFile("DEF", "apps.sbn", false), fwFile("DEF", "term.default.loads", true),
		}},
		{Name: "OLD", Models: []string{"phone-1"}, Files: []drivers.FirmwareFile{
			fwFile("OLD", "old.loads", false), fwFile("OLD", "term.default.loads", true),
		}},
		{Name: "PEND", Models: []string{"phone-1"}, Files: []drivers.FirmwareFile{
			{Name: "pend.sbn", SHA256: drivers.SHA256Pending, Size: 10},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := firmware.NewManifest(reg)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "firmware")
	put := func(load, name string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, load), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, load, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("DEF", "def.loads", fwBytes("DEF", "def.loads"))
	put("DEF", "apps.sbn", fwBytes("OLD", "apps.sbn")) // unverified: another digest
	put("OLD", "old.loads", fwBytes("OLD", "old.loads"))
	put("OLD", "term.default.loads", fwBytes("OLD", "term.default.loads"))
	put("PEND", "pend.sbn", []byte("anything at all"))

	e := startEnv(t, firmware.NewDisk(dir, m, logr.Discard()), fastTFTP())
	e.live.version(vA, map[string][]byte{sepName: generated("sep", 10)})
	e.live.swap(vA)

	check := func(name string, want []byte) {
		t.Helper()
		got, err := tftpGet(t, e.tftp, name, 0)
		ev := e.rec.next()
		resp, body := httpGet(t, e.http, "/"+name)
		hev := e.rec.next()
		if want == nil {
			if err == nil || resp.StatusCode != http.StatusNotFound || ev.Result != api.FetchNotFound || hev.Result != api.FetchNotFound {
				t.Errorf("%s: TFTP %v %s, HTTP %d %s; want not served", name, err, ev.Result, resp.StatusCode, hev.Result)
			}
			return
		}
		if err != nil || !bytes.Equal(got, want) || !bytes.Equal(body, want) || ev.Result != api.FetchServed || hev.Result != api.FetchServed {
			t.Errorf("%s: TFTP %v %s, HTTP %d %s; want served", name, err, ev.Result, resp.StatusCode, hev.Result)
		}
		if ev.BundleVersion != "" || ev.BundlePath != "" {
			t.Errorf("%s: firmware reported with a bundle version: %+v", name, ev)
		}
	}
	check("def.loads", fwBytes("DEF", "def.loads"))
	check("old.loads", fwBytes("OLD", "old.loads")) // a non-default load's own file
	check("apps.sbn", nil)                          // unverified
	check("pend.sbn", nil)                          // pending
	check("term.default.loads", nil)                // Shared: only OLD's copy is stored
	put("DEF", "term.default.loads", fwBytes("DEF", "term.default.loads"))
	check("term.default.loads", fwBytes("DEF", "term.default.loads"))
	check("DEF/def.loads", nil) // a path into the store is no name
}
