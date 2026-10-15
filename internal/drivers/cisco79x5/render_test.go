package cisco79x5_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// renderCase is one device to render.
type renderCase struct {
	name string
	dev  drivers.Device
	ctx  drivers.RenderContext
}

// goldenCases are the Render golden cases (plan §4 T1).
func goldenCases() []renderCase {
	withLabel := func(label string) drivers.Device {
		d := drivers.PhoneDevice(phone("desk", v1alpha1.PhoneModelCisco7945, deskMAC, "100", label))
		d.ConfigVersion = vNow
		return d
	}
	lab := drivers.PhoneDevice(phone("lab", v1alpha1.PhoneModelCisco7965, "D0C78915EBB5", "101", "Lab"))
	lab.ConfigVersion = version("lab")
	labCtx := houseCtx()
	labCtx.SIPPassword = "@@secret:patchbay-phone-lab/sip-password@@"
	ny := houseCtx()
	ny.Timezone = "America/New_York"
	explicit := desk()
	explicit.Firmware = cisco79x5.DefaultLoad
	return []renderCase{
		{"desk", desk(), houseCtx()},
		{"lab-7965", lab, labCtx},
		{"hostile-label", withLabel(`R&D; "Lab" <1> Küche`), houseCtx()},
		{"long-label", withLabel("Upstairs Office Phone"), houseCtx()},
		{"new-york", desk(), ny},
		{"explicit-firmware", explicit, houseCtx()},
	}
}

// TestRenderGolden pins every rendered file. The goldens were reviewed line
// by line against https://usecallmanager.nz/sepmac-cnf-xml.html and the
// research notes.
//
// The SEP file follows the page's sample element by element. Sample
// elements left out, and why:
//   - allowAutoConfig, dadEnable, ipPreferenceModeControl,
//     ipMediaAddressFamilyPreference: IPv6 and dual-stack knobs; the voice
//     VLAN is IPv4 only (ipAddressMode 0).
//   - securedSipPort, TVS, TLSResumptionTimer, the secure*URL elements,
//     capfAuthMode, capfList, certHash: TLS, trust verification and
//     certificate enrolment; release A has no TLS and issues no trust list
//     (research §7: a managed ITL is irreversible).
//   - vpnGroup: no VPN (softphones away from home are out of scope).
//   - messagesNumber, messagesURL and the Voicemail phoneService: no
//     voicemail in release A.
//   - softKeyFile, servicesURL, informationURL, idleURL, idleTimeout,
//     authenticationURL: soft keys and XML services are release C; until
//     then the phone keeps its built-in soft keys.
//   - blfAudibleAlertSettingOfIdleStation/BusyStation: BLF is unsupported
//     on stock PJSIP (design §8.5); no BLF key is rendered.
//   - recordingTone, recordingToneLocalVolume, recordingToneRemoteVolume,
//     recordingToneDuration: nothing records calls.
//   - dscpVideo, videoRtcp, separateMute, hideVideoByDefault, ciscoCamera:
//     video (videoCapability is written as 0).
//   - addOnModules: no key expansion module (the 7945 takes none).
//   - featurePolicyFile (8961/9951/9971), inactiveLoadInformation and
//     defaultWallpaperFile (8800/8900/9900), holdResumeKey,
//     simplifiedNewCall, actionableAlert, softkeyControl, lineMode,
//     callParkMonitor, markCallAsSpam, lowerYourVoiceAlert,
//     problemReportUploadURL (7800/8800), recentsSoftKey (7800),
//     webAdmin, adminPassword, cdpEnable, accessContacts, accessFavorites,
//     accessVoicemail, accessApps, applicationURL, appButtonTimer,
//     appButtonPriority, homeScreen, sendKeyAction, specialNumbers,
//     outOfRangeAlert, scanningMode, powerOffWhenCharging (8821),
//     bluetooth, bluetoothProfile, btpbap, bthfu, wifi, sdio, usb1, usb2,
//     usbClasses, kemOneColumn: the page names other series, or hardware
//     the 79x5 does not have.
//   - tls12Only: the 79x5 speaks TLS 1.0 only (research §7).
//   - daysDisplayNotActive, displayOnTime, displayOnDuration,
//     displayIdleTimeout, displayOnWhenIncomingCall, daysBacklightNotActive,
//     backlightOnTime, backlightOnDuration, backlightIdleTimeout,
//     backlightOnWhenIncomingCall: office-hours power saving; a house phone
//     keeps its own default.
//   - minimumRingVolume, adminConfigurableRinger, disableTLSCiphers,
//     loadServer, userId, ownerId: empty or placeholder in the sample, with
//     nothing to set (patchbay serves firmware itself).
//   - sshUserId, sshPassword: SSH is off, and a password here would travel
//     in clear over TFTP.
//
// Values that differ from the sample: dateTemplate M/D/YA (US order, 12-hour
// clock; the sample's D/M/Y); timerRegisterExpires 600 (the profile's
// RegistrationExpiry; sample 3600); settingsAccess 2 (restricted; bench,
// plan §8); sshAccess 1 and pcPort 1 (disabled, design §4);
// peerFirmwareSharing 0 (every firmware fetch must reach the fetch log);
// recordingOption disable (no dead Record soft key); phonePersonalization
// 0 (nothing authenticates /CGI/Execute until release C);
// advertiseG722Codec 1 (enabled; the sample's 2 is "device default").
// Placeholders get the house's values (time zone, NTP, the PBX, the line,
// English_United_States / United_States; the locale versions are empty, as
// no locale is downloaded). The network locale is written as the sample
// writes it, a <networkLocale> text element plus <networkLocaleInfo><name>
// (the plan's table calls it networkLocale/name).
func TestRenderGolden(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			files, err := cisco79x5.New().Render(c.dev, c.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 2 {
				t.Errorf("Render returned %d files, want 2", len(files))
			}
			for path, b := range files {
				golden(t, "render/"+c.name+"/"+path, b)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	d := cisco79x5.New()
	model := func(m v1alpha1.PhoneModel) drivers.Device {
		dev := drivers.PhoneDevice(phone("desk", m, deskMAC, "100", "Desk"))
		dev.ConfigVersion = vNow
		return dev
	}
	for _, c := range []struct {
		name    string
		dev     func(*drivers.Device)
		ctx     func(*drivers.RenderContext)
		invalid bool // wraps drivers.ErrInvalidSpec
	}{
		{name: "unknown model", dev: func(dv *drivers.Device) { *dv = model("cisco-7940") }, invalid: true},
		{name: "unknown load", dev: func(dv *drivers.Device) { dv.Firmware = "SIP45.9-4-2SR3-1S" }, invalid: true},
		{name: "load for another model", dev: func(dv *drivers.Device) {
			*dv = model(v1alpha1.PhoneModelCisco7975)
			dv.Firmware = cisco79x5.DefaultLoad
		}, invalid: true},
		{name: "7975 default not in manifest", dev: func(dv *drivers.Device) { *dv = model(v1alpha1.PhoneModelCisco7975) }, invalid: true},
		{name: "unknown time zone", ctx: func(c *drivers.RenderContext) { c.Timezone = "Europe/Berlin" }},
		{name: "lower-case MAC", dev: func(dv *drivers.Device) { dv.MAC = "d0c78915e8c8" }},
		{name: "short MAC", dev: func(dv *drivers.Device) { dv.MAC = "D0C78915E8C" }},
		{name: "empty password placeholder", ctx: func(c *drivers.RenderContext) { c.SIPPassword = "" }},
		{name: "256-character directory URL", ctx: func(c *drivers.RenderContext) {
			c.DirectoryURL = "http://192.168.0.240:8088/" + strings.Repeat("d", 256-26)
		}},
		{name: "51-character PBX address", ctx: func(c *drivers.RenderContext) { c.PBXAddress = strings.Repeat("p", 51) }},
		{name: "empty PBX address", ctx: func(c *drivers.RenderContext) { c.PBXAddress = "" }},
		{name: "51-character NTP server", ctx: func(c *drivers.RenderContext) { c.NTPServer = strings.Repeat("1", 51) }},
		{name: "NTP server by name", ctx: func(c *drivers.RenderContext) { c.NTPServer = "ntp.example.org" }},
		{name: "SIP port 0", ctx: func(c *drivers.RenderContext) { c.SIPPort = 0 }},
		{name: "unknown backend", ctx: func(c *drivers.RenderContext) { c.Signalling = "chan_sip" }},
		{name: "bad dial pattern", ctx: func(c *drivers.RenderContext) { c.DialPatterns = []string{"1XX", "[12]"} }},
		{name: "31-character device ID", dev: func(dv *drivers.Device) { dv.ID = strings.Repeat("i", 31) }},
		{name: "empty number", dev: func(dv *drivers.Device) { dv.Number = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dev, ctx := desk(), houseCtx()
			if c.dev != nil {
				c.dev(&dev)
			}
			if c.ctx != nil {
				c.ctx(&ctx)
			}
			files, err := d.Render(dev, ctx)
			if err == nil {
				t.Fatalf("Render succeeded: %v", files)
			}
			if got := errors.Is(err, drivers.ErrInvalidSpec); got != c.invalid {
				t.Errorf("errors.Is(%v, ErrInvalidSpec) = %v, want %v", err, got, c.invalid)
			}
		})
	}

	// At the limits, it renders.
	ctx := houseCtx()
	ctx.DirectoryURL = "http://192.168.0.240:8088/" + strings.Repeat("d", 255-26)
	ctx.PBXAddress = strings.Repeat("p", 50)
	if _, err := d.Render(desk(), ctx); err != nil {
		t.Errorf("a 255-character URL and a 50-character address: %v", err)
	}
}

// sepFields are the SEP elements with documented limits or required values.
type sepFields struct {
	XMLName         xml.Name
	TimeZone        string `xml:"devicePool>dateTimeSetting>timeZone"`
	NTPName         string `xml:"devicePool>dateTimeSetting>ntps>ntp>name"`
	NTPMode         string `xml:"devicePool>dateTimeSetting>ntps>ntp>ntpMode"`
	ProcessNodeName string `xml:"devicePool>callManagerGroup>members>member>callManager>processNodeName"`
	SIPPort         string `xml:"devicePool>callManagerGroup>members>member>callManager>ports>sipPort"`
	Register        string `xml:"sipProfile>sipProxies>registerWithProxy"`
	RemoteCc        string `xml:"sipProfile>sipCallFeatures>remoteCcEnable"`
	RemotePartyID   string `xml:"sipProfile>sipStack>remotePartyID"`
	Expires         string `xml:"sipProfile>sipStack>timerRegisterExpires"`
	PhoneLabel      string `xml:"sipProfile>phoneLabel"`
	DialTemplate    string `xml:"sipProfile>dialTemplate"`
	Line            struct {
		Button       string `xml:"button,attr"`
		LineIndex    string `xml:"lineIndex,attr"`
		FeatureID    string `xml:"featureID"`
		FeatureLabel string `xml:"featureLabel"`
		Proxy        string `xml:"proxy"`
		Port         string `xml:"port"`
		Name         string `xml:"name"`
		DisplayName  string `xml:"displayName"`
		AuthName     string `xml:"authName"`
		AuthPassword string `xml:"authPassword"`
		Contact      string `xml:"contact"`
	} `xml:"sipProfile>sipLines>line"`
	LoadInformation string `xml:"loadInformation"`
	PCPort          string `xml:"vendorConfig>pcPort"`
	WebAccess       string `xml:"vendorConfig>webAccess"`
	SSHAccess       string `xml:"vendorConfig>sshAccess"`
	SettingsAccess  string `xml:"vendorConfig>settingsAccess"`
	VersionStamp    string `xml:"versionStamp"`
	Transport       string `xml:"transportLayerProtocol"`
	G722            string `xml:"advertiseG722Codec"`
	DirectoryURL    string `xml:"directoryURL"`
}

var versionStampElement = regexp.MustCompile(`<versionStamp>[^<]*</versionStamp>`)

func TestRenderProperties(t *testing.T) {
	d := cisco79x5.New()
	// A Phone whose name is the longest Kubernetes allows: its ID is
	// shortened and its label (the name) is cut.
	longName := strings.Repeat("a", 240) + ".kitchen-wall"
	long := drivers.PhoneDevice(phone(longName, v1alpha1.PhoneModelCisco7945, deskMAC, "199", ""))
	long.ConfigVersion = vNow
	cases := append(goldenCases(), renderCase{"253-character name", long, houseCtx()})

	var dialplans [][]byte
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files, err := d.Render(c.dev, c.ctx)
			if err != nil {
				t.Fatal(err)
			}
			for path, b := range files {
				if !strings.HasPrefix(path, "tftp/") {
					t.Errorf("%s is outside tftp/", path)
				}
				if err := xml.Unmarshal(b, new(struct{})); err != nil {
					t.Errorf("%s does not parse: %v", path, err)
				}
				if !utf8.Valid(b) || !bytes.HasPrefix(b, []byte(`<?xml version="1.0" encoding="UTF-8"?>`)) {
					t.Errorf("%s is not UTF-8 with an XML declaration saying so", path)
				}
			}
			sepPath := "tftp/" + cisco79x5.ConfigFileName(c.dev.MAC)
			sep, ok := files[sepPath]
			if !ok {
				t.Fatalf("no %s in %v", sepPath, files)
			}
			dialplans = append(dialplans, files["tftp/dialplan.xml"])

			var f sepFields
			if err := xml.Unmarshal(sep, &f); err != nil {
				t.Fatal(err)
			}
			l := f.Line
			for name, v := range map[string]string{
				"authName": l.AuthName, "name": l.Name, "displayName": l.DisplayName, "featureLabel": l.FeatureLabel,
			} {
				if n := utf8.RuneCountInString(v); n > 30 || n == 0 {
					t.Errorf("%s has %d characters (%q), want 1 to 30", name, n, v)
				}
			}
			if n := utf8.RuneCountInString(f.PhoneLabel); n > 12 || strings.Contains(f.PhoneLabel, " ") {
				t.Errorf("phoneLabel %q: %d characters or a space", f.PhoneLabel, n)
			}
			if len(f.ProcessNodeName) > 50 || len(f.NTPName) > 50 || len(f.DirectoryURL) > 255 {
				t.Errorf("a host or URL is over its limit: %+v", f)
			}
			// The values plan §4 T1's table requires.
			want := map[string][2]string{
				"root":                   {f.XMLName.Local, "device"},
				"timeZone":               {f.TimeZone, mustTZ(t, c.ctx.Timezone)},
				"ntp name":               {f.NTPName, c.ctx.NTPServer},
				"ntpMode":                {f.NTPMode, "unicast"},
				"processNodeName":        {f.ProcessNodeName, c.ctx.PBXAddress},
				"sipPort":                {f.SIPPort, "5060"},
				"registerWithProxy":      {f.Register, "true"},
				"remoteCcEnable":         {f.RemoteCc, "true"},
				"remotePartyID":          {f.RemotePartyID, "true"},
				"timerRegisterExpires":   {f.Expires, "600"},
				"phoneLabel":             {f.PhoneLabel, cisco79x5.ScreenLabel(c.dev.Label)},
				"dialTemplate":           {f.DialTemplate, "dialplan.xml"},
				"line button":            {l.Button, "1"},
				"line lineIndex":         {l.LineIndex, "1"},
				"featureID":              {l.FeatureID, "9"},
				"featureLabel":           {l.FeatureLabel, cisco79x5.LineLabel(c.dev.Label)},
				"displayName":            {l.DisplayName, cisco79x5.LineLabel(c.dev.Label)},
				"proxy":                  {l.Proxy, "USECALLMANAGER"},
				"port":                   {l.Port, "5060"},
				"name":                   {l.Name, c.dev.Number},
				"contact":                {l.Contact, c.dev.Number},
				"authName":               {l.AuthName, c.dev.ID},
				"authPassword":           {l.AuthPassword, c.ctx.SIPPassword},
				"loadInformation":        {f.LoadInformation, cisco79x5.DefaultLoad},
				"pcPort":                 {f.PCPort, "1"},
				"webAccess":              {f.WebAccess, "0"},
				"sshAccess":              {f.SSHAccess, "1"},
				"settingsAccess":         {f.SettingsAccess, "2"},
				"versionStamp":           {f.VersionStamp, c.dev.ConfigVersion[:32]},
				"transportLayerProtocol": {f.Transport, "1"},
				"advertiseG722Codec":     {f.G722, "1"},
				"directoryURL":           {f.DirectoryURL, c.ctx.DirectoryURL},
			}
			for name, gw := range want {
				if gw[0] != gw[1] {
					t.Errorf("%s = %q, want %q", name, gw[0], gw[1])
				}
			}
			// The placeholder is written as is: the agent finds it by text.
			if !bytes.Contains(sep, []byte("<authPassword>"+c.ctx.SIPPassword+"</authPassword>")) {
				t.Errorf("the password placeholder is escaped or missing")
			}

			// Compile's two passes: without a config version, then with it.
			// The outputs differ only inside <versionStamp>.
			first := c.dev
			first.ConfigVersion = ""
			files1, err := d.Render(first, c.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(files1[sepPath], []byte("<versionStamp></versionStamp>")) {
				t.Error("first pass has a non-empty versionStamp")
			}
			strip := func(b []byte) []byte { return versionStampElement.ReplaceAll(b, nil) }
			if !bytes.Equal(strip(files1[sepPath]), strip(sep)) {
				t.Error("the two passes differ outside <versionStamp>")
			}
			if !bytes.Equal(files1["tftp/dialplan.xml"], files["tftp/dialplan.xml"]) {
				t.Error("the two passes' dial templates differ")
			}
		})
	}
	// The dial template is shared: byte-identical for every device rendered
	// with the same dial patterns, whatever else differs.
	for i, dp := range dialplans {
		if !bytes.Equal(dp, dialplans[0]) {
			t.Errorf("case %d's dialplan.xml differs", i)
		}
	}
	if settingsAccess := cisco79x5.SettingsAccessRestricted; settingsAccess != 2 {
		t.Errorf("settingsAccessRestricted = %d before the bench, want CUCM's Restricted (2)", settingsAccess)
	}
}

func mustTZ(t *testing.T, iana string) string {
	t.Helper()
	tz, ok := cisco79x5.WindowsTimeZone(iana)
	if !ok {
		t.Fatalf("no Windows name for %s", iana)
	}
	return tz
}

// TestRenderDecideAgree: Render refuses a device with ErrInvalidSpec
// exactly when Decide says it is Invalid.
func TestRenderDecideAgree(t *testing.T) {
	d := cisco79x5.New()
	devices := map[string]drivers.Device{}
	for _, c := range goldenCases() {
		devices[c.name] = c.dev
	}
	for name, mutate := range map[string]func(*drivers.Device){
		"unknown model": func(dv *drivers.Device) { dv.Model = "cisco-7940" },
		"unknown load":  func(dv *drivers.Device) { dv.Firmware = "SIP45.9-4-2SR3-1S" },
		"7975":          func(dv *drivers.Device) { dv.Model = string(v1alpha1.PhoneModelCisco7975) },
		"7975 with the 7945 load": func(dv *drivers.Device) {
			dv.Model = string(v1alpha1.PhoneModelCisco7975)
			dv.Firmware = cisco79x5.DefaultLoad
		},
		"7965 with the default load": func(dv *drivers.Device) {
			dv.Model = string(v1alpha1.PhoneModelCisco7965)
			dv.Firmware = cisco79x5.DefaultLoad
		},
		"lower-case MAC": func(dv *drivers.Device) { dv.MAC = strings.ToLower(dv.MAC) },
	} {
		dev := desk()
		mutate(&dev)
		devices[name] = dev
	}
	for name, dev := range devices {
		_, err := d.Render(dev, houseCtx())
		dec := d.Decide(dev, healthy(), settings, t0)
		if errors.Is(err, drivers.ErrInvalidSpec) != (dec.State == drivers.StateInvalid) {
			t.Errorf("%s: Render error %v, Decide state %s", name, err, dec.State)
		}
	}
}
