package compile_test

import (
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

func TestSettingsValidate(t *testing.T) {
	if err := houseSettings().Validate(); err != nil {
		t.Fatalf("the house settings are invalid: %v", err)
	}
	edit := func(f func(s *compile.Settings)) compile.Settings {
		s := houseSettings()
		f(&s)
		return s
	}
	for _, s := range []compile.Settings{
		edit(func(s *compile.Settings) { s.StreamBridge = "stream-bridge.telephony.svc:9092" }),
		edit(func(s *compile.Settings) { s.StreamBridge = "192.168.0.240:9092" }),
		edit(func(s *compile.Settings) { s.PBXAddress = "127.0.0.1" }),
		edit(func(s *compile.Settings) { s.RTPPorts = compile.PortRange{First: 10000, Last: 10000} }),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for _, tc := range []struct {
		name string
		s    compile.Settings
		want string
	}{
		{"a host name as the PBX address", edit(func(s *compile.Settings) { s.PBXAddress = "pbx.lan" }), `pbxAddress "pbx.lan" is not an IPv4 address`},
		{"IPv6", edit(func(s *compile.Settings) { s.PBXAddress = "fd00::1" }), `pbxAddress "fd00::1" is not an IPv4 address`},
		{"0.0.0.0", edit(func(s *compile.Settings) { s.PBXAddress = "0.0.0.0" }), "pbxAddress 0.0.0.0 is not a host address"},
		{"broadcast", edit(func(s *compile.Settings) { s.PBXAddress = "255.255.255.255" }), "is not a host address"},
		{"multicast NTP", edit(func(s *compile.Settings) { s.NTPServer = "224.0.1.1" }), "ntpServer 224.0.1.1 is not a host address"},
		{"a host name as the NTP server", edit(func(s *compile.Settings) { s.NTPServer = "pool.ntp.org" }), `ntpServer "pool.ntp.org" is not an IPv4 address`},
		{"no SIP port", edit(func(s *compile.Settings) { s.SIPPort = 0 }), "sipPort 0 is not a port"},
		{"a SIP port too high", edit(func(s *compile.Settings) { s.SIPPort = 65536 }), "sipPort 65536 is not a port"},
		{"no device HTTP port", edit(func(s *compile.Settings) { s.DeviceHTTPPort = -1 }), "deviceHTTPPort -1 is not a port"},
		{"an RTP port out of range", edit(func(s *compile.Settings) { s.RTPPorts.Last = 70000 }), "rtpPorts.last 70000 is not a port"},
		{"an empty RTP range", edit(func(s *compile.Settings) { s.RTPPorts = compile.PortRange{First: 10099, Last: 10000} }), "rtpPorts 10099-10000 is empty"},
		{"SIP inside RTP", edit(func(s *compile.Settings) { s.SIPPort = 10050 }), "sipPort 10050 is inside rtpPorts"},
		{"device HTTP inside RTP", edit(func(s *compile.Settings) { s.DeviceHTTPPort = 10000 }), "deviceHTTPPort 10000 is inside rtpPorts"},
		{"device HTTP on the SIP port", edit(func(s *compile.Settings) { s.DeviceHTTPPort = 5060 }), "deviceHTTPPort and sipPort are both 5060"},
		{"no time zone", edit(func(s *compile.Settings) { s.Timezone = "" }), "timezone is empty"},
		{"another backend", edit(func(s *compile.Settings) { s.Signalling = "chan_sip" }), `signalling "chan_sip" is not implemented`},
		{"a bad number plan", edit(func(s *compile.Settings) { s.NumberPlan.People = "1XX" }), `settings: number plan: devices "1XX" and people "1XX" overlap`},
		{"no allowed networks", edit(func(s *compile.Settings) { s.AllowedNetworks = nil }), "allowedNetworks is empty"},
		{"an address, not a network", edit(func(s *compile.Settings) { s.AllowedNetworks = []string{"192.168.20.0"} }), `"192.168.20.0" is not an IPv4 network in CIDR form`},
		{"an IPv6 network", edit(func(s *compile.Settings) { s.AllowedNetworks = []string{"fd00::/8"} }), `"fd00::/8" is not an IPv4 network`},
		{"host bits set", edit(func(s *compile.Settings) { s.AllowedNetworks = []string{"192.168.20.5/24"} }), "did you mean 192.168.20.0/24?"},
		{"a network twice", edit(func(s *compile.Settings) { s.AllowedNetworks = []string{"10.0.0.0/8", "10.0.0.0/8"} }), "10.0.0.0/8 is listed twice"},
		{"no AMI secret", edit(func(s *compile.Settings) { s.AMISecret = compile.SecretRef{} }), "amiSecret"},
		{"an AMI secret name a placeholder cannot carry", edit(func(s *compile.Settings) { s.AMISecret.Name = "Patchbay_AMI" }), "amiSecret"},
		{"a stream bridge without a port", edit(func(s *compile.Settings) { s.StreamBridge = "stream-bridge" }), `streamBridge "stream-bridge" is not host:port`},
		{"a stream bridge without a host", edit(func(s *compile.Settings) { s.StreamBridge = ":9092" }), `streamBridge ":9092" is not host:port`},
		{"a stream bridge port out of range", edit(func(s *compile.Settings) { s.StreamBridge = "bridge:0" }), `streamBridge "bridge:0" is not host:port`},
	} {
		err := tc.s.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}

// TestCheckDrivers: settings every registered driver can render with pass;
// a time zone the 79x5 cannot name is refused at controller start
// (otherwise the first Phone makes every compile fail, and the install
// freezes with only a CompileFailed Event to say why).
func TestCheckDrivers(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	s := houseSettings()
	if err := s.CheckDrivers(reg); err != nil {
		t.Errorf("the house's settings: %v", err)
	}
	s.Timezone = "America/Vancouver"
	if err := s.CheckDrivers(reg); err == nil || !strings.Contains(err.Error(), "cisco79x5") || !strings.Contains(err.Error(), "America/Vancouver") {
		t.Errorf("America/Vancouver: %v, want cisco79x5's refusal", err)
	}
	if err := s.CheckDrivers(nil); err == nil {
		t.Error("no registry: accepted")
	}
}
