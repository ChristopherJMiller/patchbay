package compile

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Validate checks the install settings before anything is compiled from
// them: the PBX address and NTP server are IPv4 addresses (the 79x5 takes
// no names for either, and the transports bind the PBX address); every
// port is 1-65535, the RTP range is in order and holds neither the SIP nor
// the device HTTP port; a time zone is named; the signalling backend is
// pjsip; the number plan is valid; the allowed networks are IPv4 network
// addresses in CIDR form, at least one and none twice; the AMI secret names
// a Secret key a placeholder can carry; the stream bridge is empty or
// host:port; the house default display passes
// v1alpha1.PhoneDisplay.Validate (the checks the CRD makes of a Phone's).
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
	if s.StreamBridge != "" {
		host, port, err := net.SplitHostPort(s.StreamBridge)
		n, perr := strconv.Atoi(port)
		if err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
			bad("streamBridge %q is not host:port", s.StreamBridge)
		}
	}
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
