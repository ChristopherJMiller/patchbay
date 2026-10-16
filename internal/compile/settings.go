package compile

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Validate checks the install settings before anything is compiled from
// them: the PBX address and NTP server are IPv4 addresses (the 79x5 takes
// no names for either, and the transports bind the PBX address); every
// port is 1-65535, the RTP range is in order and holds neither the SIP nor
// the device HTTP port; a time zone is named; the signalling backend is
// pjsip; the number plan is valid; the allowed networks are IPv4 network
// addresses in CIDR form, at least one and none twice; the AMI secret names
// a Secret key a placeholder can carry; the stream bridge is empty or
// host:port, the host an IP address or a DNS name (so it is safe inside
// Dial()); the stream settings keep StreamSettings' rules; the house
// default display passes v1alpha1.PhoneDisplay.Validate (the checks the
// CRD makes of a Phone's).
func (s Settings) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("settings: "+format, args...)) }

	if err := checkIPv4("pbxAddress", s.PBXAddress); err != nil {
		errs = append(errs, err)
	}
	if err := checkIPv4("ntpServer", s.NTPServer); err != nil {
		errs = append(errs, err)
	}
	for _, p := range []struct {
		what string
		port int
	}{{"sipPort", s.SIPPort}, {"rtpPorts.first", s.RTPPorts.First}, {"rtpPorts.last", s.RTPPorts.Last}, {"deviceHTTPPort", s.DeviceHTTPPort}} {
		if p.port < 1 || p.port > 65535 {
			bad("%s %d is not a port (1-65535)", p.what, p.port)
		}
	}
	if s.RTPPorts.First > s.RTPPorts.Last {
		bad("rtpPorts %d-%d is empty (first is after last)", s.RTPPorts.First, s.RTPPorts.Last)
	}
	inRTP := func(port int) bool { return port >= s.RTPPorts.First && port <= s.RTPPorts.Last }
	if inRTP(s.SIPPort) {
		bad("sipPort %d is inside rtpPorts %d-%d", s.SIPPort, s.RTPPorts.First, s.RTPPorts.Last)
	}
	if inRTP(s.DeviceHTTPPort) {
		bad("deviceHTTPPort %d is inside rtpPorts %d-%d", s.DeviceHTTPPort, s.RTPPorts.First, s.RTPPorts.Last)
	}
	if s.DeviceHTTPPort == s.SIPPort {
		bad("deviceHTTPPort and sipPort are both %d", s.SIPPort)
	}
	if s.Timezone == "" {
		bad("timezone is empty (an IANA zone name, e.g. America/Los_Angeles)")
	}
	if s.Signalling != drivers.SignallingPJSIP {
		bad("signalling %q is not implemented (only %q)", s.Signalling, drivers.SignallingPJSIP)
	}
	if err := s.NumberPlan.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("settings: %w", err))
	}
	if len(s.AllowedNetworks) == 0 {
		bad("allowedNetworks is empty: no device could register")
	}
	seen := map[netip.Prefix]bool{}
	for _, n := range s.AllowedNetworks {
		p, err := netip.ParsePrefix(n)
		switch {
		case err != nil || !p.Addr().Is4():
			bad("allowedNetworks: %q is not an IPv4 network in CIDR form (e.g. 192.168.20.0/24)", n)
		case p.Masked() != p:
			bad("allowedNetworks: %q is not a network address (did you mean %s?)", n, p.Masked())
		case seen[p]:
			bad("allowedNetworks: %s is listed twice", n)
		default:
			seen[p] = true
		}
	}
	if err := checkSecretRef(s.AMISecret); err != nil {
		bad("amiSecret: %v", err)
	}
	if s.StreamBridge != "" && !bridgeAddress(s.StreamBridge) {
		bad("streamBridge %q is not host:port", s.StreamBridge)
	}
	errs = append(errs, s.Streams.validate()...)
	if err := s.PhoneDefaults.Display.Validate(); err != nil {
		bad("phoneDefaults.display: %w", err)
	}
	return errors.Join(errs...)
}

// checkIPv4 accepts a plain IPv4 address that a host can bind or reach: not
// 0.0.0.0, multicast or the broadcast address.
func checkIPv4(what, addr string) error {
	a, err := netip.ParseAddr(addr)
	switch {
	case err != nil || !a.Is4():
		return fmt.Errorf("settings: %s %q is not an IPv4 address", what, addr)
	case a.IsUnspecified() || a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return fmt.Errorf("settings: %s %s is not a host address", what, addr)
	}
	return nil
}

// checkSecretRef accepts a reference whose placeholder the agent's pattern
// matches back to the same Secret name and key.
func checkSecretRef(r SecretRef) error {
	m := PlaceholderPattern.FindStringSubmatch(r.Placeholder())
	if r.Name == "" || r.Key == "" || m == nil || m[0] != r.Placeholder() || m[1] != r.Name || m[2] != r.Key {
		return fmt.Errorf("%+v does not name a Secret key (a lower-case Secret name and a data key)", r)
	}
	return nil
}

// bridgeAddress accepts the stream bridge's AudioSocket address as the
// dialplan writes it into Dial(AudioSocket/<address>/…): host:port, the
// host an IP address (IPv6 in brackets, no zone) or a DNS subdomain, the
// port 1-65535 in digits, and none of "/", ",", "(" or ")", which would end
// the dial string's part or Dial's argument.
func bridgeAddress(addr string) bool {
	if strings.ContainsAny(addr, "/,()") {
		return false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !hostName(host) || port == "" || strings.Trim(port, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// hostName accepts an IP address without a zone (IPv6 without brackets, as
// url.URL.Hostname gives it) or a DNS subdomain (lower case).
func hostName(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	return h != "" && len(validation.IsDNS1123Subdomain(h)) == 0
}

// validate checks the stream settings: CallLimit is zero or whole seconds
// from MinCallLimit to MaxCallLimit; each tuner pool's Name is a DNS label,
// listed once; its Hosts are not empty, each an IP address or a DNS name,
// compared lower-case ("HDHomeRun.lan" is "hdhomerun.lan"), none in two
// pools; its Max is 1 to streams.MaxPoolMax. Every message names the
// field.
func (s StreamSettings) validate() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("settings: "+format, args...)) }
	if d := s.CallLimit.Duration; d != 0 && (d < MinCallLimit || d > MaxCallLimit || d%time.Second != 0) {
		bad("streams.callLimit %s is not whole seconds from %s to %s", d, fmtLimit(MinCallLimit), fmtLimit(MaxCallLimit))
	}
	names := map[string]bool{}
	hosts := map[string]string{} // lower-case host -> the pool that lists it
	for i, p := range s.TunerPools {
		field := fmt.Sprintf("streams.tunerPools[%d]", i)
		switch {
		case len(validation.IsDNS1123Label(p.Name)) > 0:
			bad("%s: name %q is not a DNS label", field, p.Name)
		case names[p.Name]:
			bad("%s: name %q is listed twice", field, p.Name)
		}
		names[p.Name] = true
		if len(p.Hosts) == 0 {
			bad("%s: hosts is empty", field)
		}
		for _, h := range p.Hosts {
			key := strings.ToLower(h)
			if !hostName(key) {
				bad("%s: host %q is not an IP address or a DNS name (no scheme, port or brackets)", field, h)
				continue
			}
			if other, dup := hosts[key]; dup && other != p.Name {
				bad("%s: host %q is in pools %q and %q", field, h, other, p.Name)
				continue
			}
			hosts[key] = p.Name
		}
		if p.Max < 1 || p.Max > streams.MaxPoolMax {
			bad("%s: max %d is not 1-%d", field, p.Max, streams.MaxPoolMax)
		}
	}
	return errs
}

// fmtLimit writes a bound of the call limit as people write it (10s, 24h).
func fmtLimit(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}
