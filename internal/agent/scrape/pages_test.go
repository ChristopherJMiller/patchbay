package scrape_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
)

// The fixtures are generated here from the documented field lists
// (docs/research/2026-10-14-cisco-7945-capabilities.md §6, release-a.md
// W2.7), through the exported page shapes or by hand for the variants a
// marshaller cannot produce. capture's files are written by hand too.

// capture is one page as a real CP-7945G (hardware revision 13, SIP
// 9.4(2)SR4-3) served it on the bench on 2026-10-15: its element names,
// order and CRLF line ends, its asterisk-wrapped versionID, its
// /NetworkConfigurationX breaking off where the phone's own renderer
// throws a NullPointerException, and its empty /DeviceLogX. Every value
// that identified the phone or the house (MAC, number, serial, addresses,
// URLs, time) is invented: the addresses are 192.0.2.0/24 (RFC 5737).
func capture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cp7945g-sip-9.4.2sr4-3", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What the scraper keeps of capture's pages.
var (
	captureInfo = api.DeviceInformation{
		VersionID:        "*SIP45.9-4-2SR4-3S*",
		AppLoadID:        "jar45sip.9-4-2SR4-3.sbn",
		BootLoadID:       "tnp45.9-3-1-CR17.bin",
		HardwareRevision: "13.0",
		SerialNumber:     "FCH00000000",
		ModelNumber:      "CP-7945G",
	}
	captureNC = api.NetworkConfiguration{CallManager1: "192.0.2.10   Active"}
)

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

// TestParseRefusesWhatIsNotThePage: every parser fails on these. Only the
// /NetworkConfigurationX parser keeps anything, and only from a page that
// broke off after its root element started (ErrIncomplete); an empty
// /DeviceLogX is an empty log (TestParseDeviceLog).
func TestParseRefusesWhatIsNotThePage(t *testing.T) {
	parsers := []struct {
		root, other string
		parse       func([]byte) (kept bool, err error)
		emptyIsPage bool
	}{
		{"DeviceInformation", "NetworkConfiguration", func(b []byte) (bool, error) {
			got, err := scrape.ParseDeviceInformation(b)
			return got != nil, err
		}, false},
		{"NetworkConfiguration", "DeviceLog", func(b []byte) (bool, error) {
			got, err := scrape.ParseNetworkConfiguration(b)
			return got != nil, err
		}, false},
		{"DeviceLog", "DeviceInformation", func(b []byte) (bool, error) {
			got, err := scrape.ParseDeviceLog(b)
			return got != nil, err
		}, true},
	}
	// The bodies in which the page's root element starts, so the
	// /NetworkConfigurationX parser keeps what came before the break.
	rootStarts := map[string]bool{
		"unclosed": true, "mismatched": true, "two roots": true, "invalid UTF-8": true,
		"an undefined entity": true, "text after the root": true,
	}
	for name, tmpl := range notPages {
		for _, p := range parsers {
			if p.emptyIsPage && strings.TrimSpace(tmpl) == "" {
				continue
			}
			body := strings.NewReplacer("ROOT", p.root, "OTHER", p.other).Replace(tmpl)
			kept, err := p.parse([]byte(body))
			if err == nil {
				t.Errorf("%s: the %s parser accepted %q", name, p.root, body)
				continue
			}
			partial := p.root == "NetworkConfiguration" && rootStarts[name]
			if kept != partial || errors.Is(err, scrape.ErrIncomplete) != partial {
				t.Errorf("%s: the %s parser kept something: %v, error %v; want kept and ErrIncomplete only from a page that broke off",
					name, p.root, kept, err)
			}
		}
	}
}

// TestParseCapture: the real CP-7945G's pages. /DeviceInformationX is
// whole; /NetworkConfigurationX keeps every element completed before the
// break; the empty /DeviceLogX is an empty log.
func TestParseCapture(t *testing.T) {
	info, err := scrape.ParseDeviceInformation(capture(t, "DeviceInformationX.xml"))
	if err != nil || *info != captureInfo {
		t.Errorf("DeviceInformationX: %+v, %v\nwant %+v", info, err, captureInfo)
	}
	nc, err := scrape.ParseNetworkConfiguration(capture(t, "NetworkConfigurationX.xml"))
	if !errors.Is(err, scrape.ErrIncomplete) || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("NetworkConfigurationX: error %v, want ErrIncomplete at an unexpected EOF", err)
	}
	if nc == nil || *nc != captureNC {
		t.Errorf("NetworkConfigurationX: %+v, want %+v", nc, captureNC)
	}
	log, err := scrape.ParseDeviceLog(capture(t, "DeviceLogX.xml"))
	if err != nil || log != nil {
		t.Errorf("DeviceLogX: %q, %v; want an empty log", log, err)
	}
}

// TestParseNetworkConfigurationIncomplete: a page that breaks off keeps
// the fields of the elements completed before the break, never the text
// of one the break cut, and says so (ErrIncomplete).
func TestParseNetworkConfigurationIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want api.NetworkConfiguration
	}{
		{"cut after a whole element", `<NetworkConfiguration><AdminVLANId>5</AdminVLANId><CallManager1>192.168.0.240  Active</CallManager1>`,
			api.NetworkConfiguration{AdminVLAN: "5", CallManager1: "192.168.0.240  Active"}},
		{"cut inside a field: that field is not read", `<NetworkConfiguration><AdminVLANId>5</AdminVLANId><CallManager1>192.168.0.240`,
			api.NetworkConfiguration{AdminVLAN: "5"}},
		{"cut inside a field's later twin: the whole first one stands", `<NetworkConfiguration><CallManager1/><CallManager1>192.168.0.240  Active</CallManager1><CallManager1>junk`,
			api.NetworkConfiguration{CallManager1: "192.168.0.240  Active"}},
		{"cut as the phone does", "<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n<NetworkConfiguration><AdminVLANId>9</AdminVLANId>" +
			"<EnergywisePowerLevel>java.lang.NullPointerException",
			api.NetworkConfiguration{AdminVLAN: "9"}},
		{"cut before any field", `<NetworkConfiguration>`, api.NetworkConfiguration{}},
		{"broken by a bad tag", `<NetworkConfiguration><AdminVLANId>5</AdminVLANId><CallManager1>x</CallManager2></NetworkConfiguration>`,
			api.NetworkConfiguration{AdminVLAN: "5"}},
		{"followed by junk", `<NetworkConfiguration><AdminVLANId>5</AdminVLANId></NetworkConfiguration><html/>`,
			api.NetworkConfiguration{AdminVLAN: "5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scrape.ParseNetworkConfiguration([]byte(tc.body))
			if !errors.Is(err, scrape.ErrIncomplete) {
				t.Errorf("error %v, want ErrIncomplete", err)
			}
			if got == nil || *got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
	// Strict where the load lives: /DeviceInformationX that breaks off is
	// no page at all.
	if got, err := scrape.ParseDeviceInformation([]byte(`<DeviceInformation><versionID>SIP45.9-4-2SR4-3S</versionID><appLoadID>jar`)); got != nil || err == nil {
		t.Errorf("a DeviceInformationX that breaks off: %+v, %v", got, err)
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
		// The real CP-7945G answers 200 with nothing at all (capture).
		{"an empty body is an empty log", ``, nil},
		{"so is white space", " \r\n\t", nil},
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

// TestParseBoundsWhatThePhoneSays: the page is the phone's (or whoever
// answers at its address on the voice VLAN): every field is kept as valid
// UTF-8 with no control character (a terminal escape or carriage return
// would reach `patchbay status`) and at most MaxField bytes, every log
// line at most MaxLogLine, so a 256 KiB page cannot put 40 kB into a
// Phone's status.
func TestParseBoundsWhatThePhoneSays(t *testing.T) {
	huge := "SIP45." + strings.Repeat("A", 40000) + "\x1b[2J\rspoofed\u009b"
	info, err := scrape.ParseDeviceInformation(page(t, scrape.DeviceInformationX{VersionID: huge, ModelNumber: "CP-7945G\r\u009b[1A"}))
	if err != nil {
		t.Fatal(err)
	}
	clean := func(what, s string, limit int) {
		t.Helper()
		if len(s) > limit || strings.ContainsAny(s, "\x1b\r\n\u009b") {
			t.Errorf("%s: %d bytes (limit %d) %q…", what, len(s), limit, s[:min(len(s), 20)])
		}
	}
	clean("versionID", info.VersionID, scrape.MaxField)
	if !strings.HasPrefix(info.VersionID, "SIP45.AAAA") || info.ModelNumber != "CP-7945G  [1A" {
		t.Errorf("versionID %q…, model %q", info.VersionID[:10], info.ModelNumber)
	}
	net, err := scrape.ParseNetworkConfiguration(page(t, scrape.NetworkConfigurationX{CallManager1: huge}))
	if err != nil {
		t.Fatal(err)
	}
	clean("callManager1", net.CallManager1, scrape.MaxField)
	log, err := scrape.ParseDeviceLog(page(t, scrape.DeviceLogX{Status: []string{huge, "10:02:18a 7: Trust List update failed"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[1] != "10:02:18a 7: Trust List update failed" {
		t.Fatalf("log %d lines, last %q", len(log), log[len(log)-1])
	}
	clean("log line", log[0], scrape.MaxLogLine)
}
