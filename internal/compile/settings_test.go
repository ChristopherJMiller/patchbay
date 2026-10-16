package compile_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
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
		edit(func(s *compile.Settings) {
			s.PhoneDefaults.Display = &v1alpha1.PhoneDisplay{IdleTimeout: &metav1.Duration{Duration: time.Minute}}
		}),
		edit(func(s *compile.Settings) { s.PhoneDefaults.Display = &v1alpha1.PhoneDisplay{} }),
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
		{"a house display idle of 30s", edit(func(s *compile.Settings) {
			s.PhoneDefaults.Display = &v1alpha1.PhoneDisplay{IdleTimeout: &metav1.Duration{Duration: 30 * time.Second}}
		}), "settings: phoneDefaults.display: idleTimeout: 30s is not whole minutes from 1m to 23h59m"},
		{"a house display window on no day", edit(func(s *compile.Settings) {
			s.PhoneDefaults.Display = &v1alpha1.PhoneDisplay{OnWindow: &v1alpha1.DisplayOnWindow{
				Start: "07:30", Duration: metav1.Duration{Duration: time.Hour}}}
		}), "settings: phoneDefaults.display: onWindow.days: no day"},
		{"a house display window from 7:30", edit(func(s *compile.Settings) {
			s.PhoneDefaults.Display = &v1alpha1.PhoneDisplay{OnWindow: &v1alpha1.DisplayOnWindow{
				Start: "7:30", Duration: metav1.Duration{Duration: time.Hour}, Days: []v1alpha1.Weekday{v1alpha1.Monday}}}
		}), `settings: phoneDefaults.display: onWindow.start: "7:30" is not HH:MM`},
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

// TestSettingsValidateStreams: the stream bridge's address is safe inside
// Dial(), the call limit is whole seconds from 10s to 24h, and the tuner
// pools are named once, hold hosts that are in one pool each, and play 1
// to streams.MaxPoolMax sources (docs/plans/release-b.md RT.1).
func TestSettingsValidateStreams(t *testing.T) {
	edit := func(f func(s *compile.Settings)) compile.Settings {
		s := houseSettings()
		s.StreamBridge = "patchbay-streams.telephony.svc:9092"
		f(&s)
		return s
	}
	limit := func(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }
	pools := func(p ...compile.TunerPool) func(s *compile.Settings) {
		return func(s *compile.Settings) { s.Streams.TunerPools = p }
	}
	for name, s := range map[string]compile.Settings{
		"no stream settings":     edit(func(*compile.Settings) {}),
		"no stream bridge":       edit(func(s *compile.Settings) { s.StreamBridge = "" }),
		"an IPv4 bridge":         edit(func(s *compile.Settings) { s.StreamBridge = "10.96.0.12:9092" }),
		"an IPv6 bridge":         edit(func(s *compile.Settings) { s.StreamBridge = "[fd00::12]:9092" }),
		"a one-label bridge":     edit(func(s *compile.Settings) { s.StreamBridge = "bridge:1" }),
		"the shortest limit":     edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(compile.MinCallLimit) }),
		"the longest limit":      edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(compile.MaxCallLimit) }),
		"a 90 minute limit":      edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(90 * time.Minute) }),
		"the house's tuner pool": edit(pools(compile.TunerPool{Name: "hdhomerun", Hosts: []string{"192.168.0.58"}, Max: 1})),
		"two pools": edit(pools(
			compile.TunerPool{Name: "hdhomerun", Hosts: []string{"192.168.0.58", "hdhomerun.lan"}, Max: 1},
			compile.TunerPool{Name: "attic", Hosts: []string{"fd00::58", "Attic.LAN"}, Max: streams.MaxPoolMax})),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name string
		s    compile.Settings
		want string
	}{
		{"a bridge with a path", edit(func(s *compile.Settings) { s.StreamBridge = "bridge/x:9092" }), `streamBridge "bridge/x:9092" is not host:port`},
		{"a bridge with a comma", edit(func(s *compile.Settings) { s.StreamBridge = "a,b:9092" }), `streamBridge "a,b:9092" is not host:port`},
		{"a bridge with parentheses", edit(func(s *compile.Settings) { s.StreamBridge = "a(b):9092" }), `streamBridge "a(b):9092" is not host:port`},
		{"a bridge with an upper-case name", edit(func(s *compile.Settings) { s.StreamBridge = "Bridge:9092" }), `streamBridge "Bridge:9092" is not host:port`},
		{"a bridge with an underscore", edit(func(s *compile.Settings) { s.StreamBridge = "stream_bridge:9092" }), `streamBridge "stream_bridge:9092" is not host:port`},
		{"a bridge with a signed port", edit(func(s *compile.Settings) { s.StreamBridge = "bridge:+9092" }), `streamBridge "bridge:+9092" is not host:port`},
		{"a bridge with an IPv6 zone", edit(func(s *compile.Settings) { s.StreamBridge = "[fe80::1%eth0]:9092" }), `is not host:port`},
		{"a bridge with a space", edit(func(s *compile.Settings) { s.StreamBridge = "a b:9092" }), `streamBridge "a b:9092" is not host:port`},
		{"a negative limit", edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(-time.Minute) }), "streams.callLimit -1m0s is not whole seconds from 10s to 24h"},
		{"a limit under 10s", edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(9 * time.Second) }), "streams.callLimit 9s is not whole seconds from 10s to 24h"},
		{"a limit over 24h", edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(24*time.Hour + time.Second) }), "streams.callLimit 24h0m1s is not whole seconds from 10s to 24h"},
		{"a limit of part of a second", edit(func(s *compile.Settings) { s.Streams.CallLimit = limit(90*time.Second + time.Millisecond) }), "streams.callLimit 1m30.001s is not whole seconds from 10s to 24h"},
		{"a pool without a name", edit(pools(compile.TunerPool{Hosts: []string{"h"}, Max: 1})), `streams.tunerPools[0]: name "" is not a DNS label`},
		{"a pool named with a dot", edit(pools(compile.TunerPool{Name: "hd.homerun", Hosts: []string{"h"}, Max: 1})), `streams.tunerPools[0]: name "hd.homerun" is not a DNS label`},
		{"a pool named in upper case", edit(pools(compile.TunerPool{Name: "HDHomeRun", Hosts: []string{"h"}, Max: 1})), `streams.tunerPools[0]: name "HDHomeRun" is not a DNS label`},
		{"a pool listed twice", edit(pools(
			compile.TunerPool{Name: "tv", Hosts: []string{"a"}, Max: 1},
			compile.TunerPool{Name: "tv", Hosts: []string{"b"}, Max: 1})), `streams.tunerPools[1]: name "tv" is listed twice`},
		{"a pool without hosts", edit(pools(compile.TunerPool{Name: "tv", Max: 1})), "streams.tunerPools[0]: hosts is empty"},
		{"a host with a port", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{"192.168.0.58:5004"}, Max: 1})), `streams.tunerPools[0]: host "192.168.0.58:5004" is not an IP address or a DNS name`},
		{"a host with a scheme", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{"http://192.168.0.58"}, Max: 1})), `streams.tunerPools[0]: host "http://192.168.0.58" is not an IP address or a DNS name`},
		{"an IPv6 host in brackets", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{"[fd00::58]"}, Max: 1})), `streams.tunerPools[0]: host "[fd00::58]" is not an IP address or a DNS name`},
		{"an empty host", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{""}, Max: 1})), `streams.tunerPools[0]: host "" is not an IP address or a DNS name`},
		{"a host in two pools", edit(pools(
			compile.TunerPool{Name: "attic", Hosts: []string{"HDHomeRun.lan"}, Max: 1},
			compile.TunerPool{Name: "tv", Hosts: []string{"hdhomerun.LAN"}, Max: 1})), `host "hdhomerun.LAN" is in pools "attic" and "tv"`},
		{"a pool of none", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{"h"}})), "streams.tunerPools[0]: max 0 is not 1-16"},
		{"a pool of too many", edit(pools(compile.TunerPool{Name: "tv", Hosts: []string{"h"}, Max: streams.MaxPoolMax + 1})), "streams.tunerPools[0]: max 17 is not 1-16"},
	} {
		err := tc.s.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}
