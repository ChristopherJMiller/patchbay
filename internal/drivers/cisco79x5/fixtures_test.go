package cisco79x5_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// update rewrites the golden files: go test ./internal/drivers/cisco79x5 -run Golden -update
var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// t0 is the instant every test time is relative to.
var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// ago is t0 minus d.
func ago(d time.Duration) time.Time { return t0.Add(-d) }

// mt is a *metav1.Time at t.
func mt(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

const (
	deskMAC     = "D0C78915E8C8"
	deskSEP     = "SEPD0C78915E8C8.cnf.xml"
	deskIP      = "192.168.20.101"
	pbx         = "192.168.0.240"
	cisco       = "Cisco's cmterm-7945_7965-sip.9-4-2SR4-3.zip"
	deskContact = "sip:100@192.168.20.101:49152;transport=tcp"
	deskUA      = "Cisco-CP7945G/9.4.2"
	deskSecret  = "@@secret:patchbay-phone-desk/sip-password@@"
)

// housePatterns is compile.DefaultNumberPlan().DialPatterns(): EchoTest,
// Emergency, Devices, People, Streams, then Reserved.
var housePatterns = []string{"600", "911", "1XX", "3XX", "7XX", "2XX", "0", "9.", "*8"}

// ciscoFiles are the default load's eight files, in manifest order.
var ciscoFiles = []string{
	"SIP45.9-4-2SR4-3S.loads",
	"apps45.9-4-2SR4-3.sbn",
	"cnu45.9-4-2SR4-3.sbn",
	"cvm45sip.9-4-2SR4-3.sbn",
	"dsp45.9-4-2SR4-3.sbn",
	"jar45sip.9-4-2SR4-3.sbn",
	"term45.default.loads",
	"term65.default.loads",
}

// version is a fake config version: the SHA-256 of seed.
func version(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// The desk phone's current config version (V) and an older one.
var (
	vNow = version("desk v2")
	vOld = version("desk v1")
)

func houseCtx() drivers.RenderContext {
	return drivers.RenderContext{
		PBXAddress:        pbx,
		SIPPort:           5060,
		DeviceHTTPURL:     "http://192.168.0.240:8088",
		DirectoryURL:      "http://192.168.0.240:8088/directory.xml",
		AuthenticationURL: "http://192.168.0.240:8088/authenticate",
		ServicesURL:       "http://192.168.0.240:8088/services.xml",
		NTPServer:         "192.168.20.1",
		Timezone:          "America/Los_Angeles",
		Signalling:        drivers.SignallingPJSIP,
		DialPatterns:      housePatterns,
		SIPPassword:       deskSecret,
	}
}

// phone is a Phone resource.
func phone(name string, model v1alpha1.PhoneModel, mac, number, label string) *v1alpha1.Phone {
	p := &v1alpha1.Phone{
		Spec: v1alpha1.PhoneSpec{
			Model:  model,
			MAC:    v1alpha1.MAC(mac),
			Number: v1alpha1.Number(number),
			Label:  label,
		},
	}
	p.Name = name
	return p
}

// desk is the house's Phone desk as a Device, with config version vNow and
// no memory.
func desk() drivers.Device {
	d := drivers.PhoneDevice(phone("desk", v1alpha1.PhoneModelCisco7945, deskMAC, "100", "Desk"))
	d.ConfigVersion = vNow
	return d
}

// pendingLoads is Cisco's manifest with every digest drivers.SHA256Pending,
// as a load is before anyone has taken its files' SHA-256s.
func pendingLoads() []drivers.FirmwareLoad {
	loads := cisco79x5.New().Firmware()
	for i := range loads {
		for j := range loads[i].Files {
			loads[i].Files[j].SHA256 = drivers.SHA256Pending
		}
	}
	return loads
}

// staged is a firmware store holding the named files of the default load
// (all eight when none are named).
func staged(names ...string) agentapi.FirmwareStore {
	if len(names) == 0 {
		names = ciscoFiles
	}
	var s agentapi.FirmwareStore
	for _, n := range names {
		s.Files = append(s.Files, agentapi.FirmwareFileStatus{Load: cisco79x5.DefaultLoad, Name: n, Present: true})
	}
	return s
}

// golden compares got with testdata/<rel>, or rewrites it with -update.
func golden(t *testing.T, rel string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", rel)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update and review the diff):\n--- got\n%s", path, got)
	}
}
