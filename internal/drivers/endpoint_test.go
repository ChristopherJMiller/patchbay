package drivers_test

import (
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestEndpointOptionsValidate(t *testing.T) {
	good := validProfile("cisco-7945").Endpoint
	if err := good.Validate(); err != nil {
		t.Fatalf("the 79x5's options: %v", err)
	}
	udp := good
	udp.Transport, udp.ForceRport, udp.RewriteContact, udp.RTPSymmetric = drivers.SIPTransportUDP, true, true, true
	udp.QualifyFrequency, udp.RegistrationExpiry = time.Minute, 10*time.Minute
	if err := udp.Validate(); err != nil {
		t.Fatalf("softphone-like options: %v", err)
	}
	for name, mutate := range map[string]func(*drivers.EndpointOptions){
		"no transport":                 func(e *drivers.EndpointOptions) { e.Transport = "" },
		"tls (not rendered)":           func(e *drivers.EndpointOptions) { e.Transport = "tls" },
		"no codecs":                    func(e *drivers.EndpointOptions) { e.Codecs = nil },
		"an unknown codec":             func(e *drivers.EndpointOptions) { e.Codecs = []string{"g729"} },
		"a codec twice":                func(e *drivers.EndpointOptions) { e.Codecs = []string{"ulaw", "g722", "ulaw"} },
		"a negative qualify frequency": func(e *drivers.EndpointOptions) { e.QualifyFrequency = -time.Second },
		"a fractional qualify":         func(e *drivers.EndpointOptions) { e.QualifyFrequency = 1500 * time.Millisecond },
		"no registration expiry":       func(e *drivers.EndpointOptions) { e.RegistrationExpiry = 0 },
		"an expiry under a minute":     func(e *drivers.EndpointOptions) { e.RegistrationExpiry = 59 * time.Second },
		"a fractional expiry":          func(e *drivers.EndpointOptions) { e.RegistrationExpiry = 90*time.Second + time.Millisecond },
	} {
		e := good
		e.Codecs = append([]string(nil), good.Codecs...)
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}
