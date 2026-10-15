package scrape_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
)

// The fixtures are generated here from the documented field lists
// (docs/research/2026-10-14-cisco-7945-capabilities.md §6, release-a.md
// W2.7), through the exported page shapes or by hand for the variants a
// marshaller cannot produce. None is copied from a phone.

func page(t *testing.T, v any) []byte {
	t.Helper()
	b, err := scrape.MarshalPage(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var fullInfo = api.DeviceInformation{
	VersionID:        "SIP45.9-4-2SR4-3S",
	AppLoadID:        "jar45sip.9-4-2SR4-3.sbn",
	BootLoadID:       "tnp45.8-3-1-21a.bin",
	HardwareRevision: "1.0",
	SerialNumber:     "FCH0000TEST",
	ModelNumber:      "CP-7945G",
}

func infoX(d api.DeviceInformation) scrape.DeviceInformationX {
	return scrape.DeviceInformationX{
		VersionID: d.VersionID, AppLoadID: d.AppLoadID, BootLoadID: d.BootLoadID,
		HardwareRevision: d.HardwareRevision, SerialNumber: d.SerialNumber, ModelNumber: d.ModelNumber,
	}
}

func TestParseDeviceInformation(t *testing.T) {
	latin1 := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" +
		"<DeviceInformation><modelNumber>CP-7945G</modelNumber><serialNumber>FCH\xe9</serialNumber></DeviceInformation>"
	for _, tc := range []struct {
		name string
		body string
		want api.DeviceInformation
	}{
		{"generated", string(page(t, infoX(fullInfo))), fullInfo},
		{"element names in other cases", `<?xml version="1.0"?>
<DEVICEINFORMATION>
  <VersionId>SIP45.9-4-2SR4-3S</VersionId>
  <APPLOADID>jar45sip.9-4-2SR4-3.sbn</APPLOADID>
  <BootLoadId>tnp45.8-3-1-21a.bin</BootLoadId>
  <HARDWAREREVISION>1.0</HARDWAREREVISION>
  <SerialNumber>FCH0000TEST</SerialNumber>
  <ModelNUMBER>CP-7945G</ModelNUMBER>
</DEVICEINFORMATION>`, fullInfo},
		{"the X-suffixed root", `<DeviceInformationX><versionID>SIP45.9-4-2SR4-3S</versionID></DeviceInformationX>`,
			api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S"}},
		{"ISO-8859-1 with a Latin-1 byte", latin1,
			api.DeviceInformation{ModelNumber: "CP-7945G", SerialNumber: "FCH\u00e9"}},
		{"latin1 label", strings.Replace(latin1, "ISO-8859-1", "latin1", 1),
			api.DeviceInformation{ModelNumber: "CP-7945G", SerialNumber: "FCH\u00e9"}},
		{"US-ASCII label", `<?xml version="1.0" encoding="US-ASCII"?><DeviceInformation><modelNumber>CP-7945G</modelNumber></DeviceInformation>`,
			api.DeviceInformation{ModelNumber: "CP-7945G"}},
		{"UTF-8 label", "<?xml version=\"1.0\" encoding=\"UTF-8\"?><DeviceInformation><serialNumber>FCH\u00e9</serialNumber></DeviceInformation>",
			api.DeviceInformation{SerialNumber: "FCH\u00e9"}},
		{"missing fields", `<DeviceInformation><appLoadID>jar45sip.9-4-2SR4-3.sbn</appLoadID></DeviceInformation>`,
			api.DeviceInformation{AppLoadID: "jar45sip.9-4-2SR4-3.sbn"}},
		{"no fields at all", `<DeviceInformation/>`, api.DeviceInformation{}},
		{"white space is trimmed", "<DeviceInformation><versionID>\n\t SIP45.9-4-2SR4-3S \n</versionID></DeviceInformation>",
			api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S"}},
		{"unknown elements, comments and attributes are ignored", `<!-- generated -->
<DeviceInformation build="x"><MACAddress>D0C78915E8C8</MACAddress><udi>phone</udi><versionID a="b">SIP45.9-4-2SR4-3S</versionID></DeviceInformation>`,
			api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S"}},
		{"the first non-empty element wins", `<DeviceInformation><versionID/><versionID>SIP45.9-4-2SR4-3S</versionID><versionID>other</versionID></DeviceInformation>`,
			api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S"}},
		{"nested elements are not fields", `<DeviceInformation><group><versionID>nested</versionID></group></DeviceInformation>`,
			api.DeviceInformation{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scrape.ParseDeviceInformation([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseDeviceInformation: %v", err)
			}
			if *got != tc.want {
				t.Errorf("got %+v\nwant %+v", *got, tc.want)
			}
		})
	}
}

// notPages are bodies no parser may accept: a phone that answers HTML, a
// broken document, and documents that are not one page. ROOT stands for
// the page's own root element and OTHER for another page's.
var notPages = map[string]string{
	"empty":                ``,
	"white space":          "  \n",
	"plain text":           `SIP45.9-4-2SR4-3S`,
	"HTML":                 `<!DOCTYPE html><html><head><title>Device Information</title></head><body><table><tr><td>Version</td><td>SIP45.9-4-2SR4-3S</td></tr></table>&nbsp;<br></body></html>`,
	"well-formed XHTML":    `<html><body><versionID>SIP45.9-4-2SR4-3S</versionID><AdminVLANId>5</AdminVLANId><status>x</status></body></html>`,
	"unclosed":             `<ROOT><status>SIP45.9-4-2SR4-3S</status>`,
	"mismatched":           `<ROOT><status>x</appLoadID></ROOT>`,
	"two roots":            `<ROOT/><ROOT/>`,
	"unsupported charset":  `<?xml version="1.0" encoding="EBCDIC-US"?><ROOT/>`,
	"invalid UTF-8":        "<ROOT><status>\xff\xfe</status></ROOT>",
	"an undefined entity":  `<ROOT><status>&nbsp;</status></ROOT>`,
	"text after the root":  `<ROOT/>trailing`,
	"another page's root":  `<OTHER/>`,
	"a root with a suffix": `<ROOTXY/>`,
}

func TestParseRefusesWhatIsNotThePage(t *testing.T) {
	parsers := []struct {
		root, other string
		parse       func([]byte) (any, error)
	}{
		{"DeviceInformation", "NetworkConfiguration", func(b []byte) (any, error) { return scrape.ParseDeviceInformation(b) }},
		{"NetworkConfiguration", "DeviceLog", func(b []byte) (any, error) { return scrape.ParseNetworkConfiguration(b) }},
		{"DeviceLog", "DeviceInformation", func(b []byte) (any, error) { return scrape.ParseDeviceLog(b) }},
	}
	for name, tmpl := range notPages {
		for _, p := range parsers {
			body := strings.NewReplacer("ROOT", p.root, "OTHER", p.other).Replace(tmpl)
			if got, err := p.parse([]byte(body)); err == nil {
				t.Errorf("%s: the %s parser accepted %q: %+v", name, p.root, body, got)
			}
		}
	}
}

func TestParseNetworkConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want api.NetworkConfiguration
	}{
		{"generated", string(page(t, scrape.NetworkConfigurationX{AdminVLANID: "5", CallManager1: "192.168.0.240  Active"})),
			api.NetworkConfiguration{AdminVLAN: "5", CallManager1: "192.168.0.240  Active"}},
		{"generated, empty Admin VLAN", string(page(t, scrape.NetworkConfigurationX{CallManager1: "192.168.0.240  Active"})),
			api.NetworkConfiguration{CallManager1: "192.168.0.240  Active"}},
		{"AdminVLANID", `<NetworkConfiguration><AdminVLANID>7</AdminVLANID></NetworkConfiguration>`,
			api.NetworkConfiguration{AdminVLAN: "7"}},
		{"lower case", `<networkconfiguration><adminvlanid>7</adminvlanid><callmanager1>x  Active</callmanager1></networkconfiguration>`,
			api.NetworkConfiguration{AdminVLAN: "7", CallManager1: "x  Active"}},
		{"UnifiedCM1", `<NetworkConfiguration><UnifiedCM1>192.168.0.240  Active</UnifiedCM1></NetworkConfiguration>`,
			api.NetworkConfiguration{CallManager1: "192.168.0.240  Active"}},
		{"CUCM1", `<NetworkConfiguration><CUCM1>192.168.0.240  Standby</CUCM1></NetworkConfiguration>`,
			api.NetworkConfiguration{CallManager1: "192.168.0.240  Standby"}},
		{"Unified CM 2 is not 1", `<NetworkConfiguration><CallManager2>other</CallManager2><UnifiedCM2>x</UnifiedCM2></NetworkConfiguration>`,
			api.NetworkConfiguration{}},
		{"the X-suffixed root", `<NetworkConfigurationX><AdminVLANId>9</AdminVLANId></NetworkConfigurationX>`,
			api.NetworkConfiguration{AdminVLAN: "9"}},
		{"the operational VLAN is not the admin VLAN", `<NetworkConfiguration><OperationalVLANId>20</OperationalVLANId></NetworkConfiguration>`,
			api.NetworkConfiguration{}},
		{"missing fields", `<NetworkConfiguration/>`, api.NetworkConfiguration{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scrape.ParseNetworkConfiguration([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseNetworkConfiguration: %v", err)
			}
			if *got != tc.want {
				t.Errorf("got %+v\nwant %+v", *got, tc.want)
			}
		})
	}
}

func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %02d", i+1)
	}
	return out
}

func TestParseDeviceLog(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"generated", string(page(t, scrape.DeviceLogX{Status: []string{"TFTP Timeout : SEPD0C78915E8C8.cnf.xml.sgn", "Trust List Update Failed"}})),
			[]string{"TFTP Timeout : SEPD0C78915E8C8.cnf.xml.sgn", "Trust List Update Failed"}},
		{"the last 20 lines, oldest first", string(page(t, scrape.DeviceLogX{Status: lines(25)})), lines(25)[5:]},
		{"exactly 20", string(page(t, scrape.DeviceLogX{Status: lines(20)})), lines(20)},
		{"empty and blank lines dropped, the rest trimmed", "<DeviceLog><status/><status>  a  </status><status>\n\t</status><status>b</status></DeviceLog>",
			[]string{"a", "b"}},
		{"status in any case, at any depth", `<DeviceLog><STATUS>a</STATUS><group><Status>b</Status></group></DeviceLog>`,
			[]string{"a", "b"}},
		{"other leaves are ignored when there are status elements", `<DeviceLog><time>1</time><status>a</status></DeviceLog>`,
			[]string{"a"}},
		{"every leaf without status elements", `<DeviceLog><line>a</line><entry><msg>b</msg><at> </at></entry><line>c</line></DeviceLog>`,
			[]string{"a", "b", "c"}},
		{"a status element's whole text", `<DeviceLog><status>a <b>bold</b> c</status></DeviceLog>`,
			[]string{"a bold c"}},
		{"lines inside one element", "<DeviceLog><status>a\nb\r\n\n c </status></DeviceLog>",
			[]string{"a", "b", "c"}},
		{"Latin-1", "<?xml version=\"1.0\" encoding=\"iso-8859-1\"?><DeviceLog><status>caf\xe9</status></DeviceLog>",
			[]string{"caf\u00e9"}},
		{"the X-suffixed root", `<devicelogx><status>a</status></devicelogx>`, []string{"a"}},
		{"empty log", `<DeviceLog></DeviceLog>`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scrape.ParseDeviceLog([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseDeviceLog: %v", err)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestPageShapesRoundTrip: what the exported shapes marshal to is exactly
// what the parsers read, so the e2e fake phone cannot drift from them.
func TestPageShapesRoundTrip(t *testing.T) {
	info, err := scrape.ParseDeviceInformation(page(t, infoX(fullInfo)))
	if err != nil || *info != fullInfo {
		t.Errorf("DeviceInformationX: %+v, %v", info, err)
	}
	nc, err := scrape.ParseNetworkConfiguration(page(t, scrape.NetworkConfigurationX{AdminVLANID: "1", CallManager1: "c"}))
	if err != nil || *nc != (api.NetworkConfiguration{AdminVLAN: "1", CallManager1: "c"}) {
		t.Errorf("NetworkConfigurationX: %+v, %v", nc, err)
	}
	log, err := scrape.ParseDeviceLog(page(t, scrape.DeviceLogX{Status: []string{"a & <b>"}}))
	if err != nil || !reflect.DeepEqual(log, []string{"a & <b>"}) {
		t.Errorf("DeviceLogX: %q, %v", log, err)
	}
	if b := page(t, scrape.DeviceLogX{}); !strings.HasPrefix(string(b), "<?xml") {
		t.Errorf("MarshalPage has no XML declaration: %s", b)
	}
}
