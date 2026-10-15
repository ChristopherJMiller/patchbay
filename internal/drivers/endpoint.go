package drivers

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// SIPTransport is the SIP transport a model registers and is called over.
type SIPTransport string

// SIP transports. The compiler renders one PJSIP transport of each kind on
// the PBX address (transport-udp, transport-tcp); an endpoint names one.
const (
	SIPTransportUDP SIPTransport = "udp"
	SIPTransportTCP SIPTransport = "tcp"
)

// KnownCodecs are the codec names EndpointOptions.Codecs may use: Asterisk's
// names for the codec modules the compiled modules.conf always loads.
var KnownCodecs = []string{"g722", "ulaw", "alaw"}

// EndpointOptions are the PJSIP endpoint and AOR options a model needs on
// the Asterisk side (design §4: for the 79x5, TCP, force_rport=no,
// rewrite_contact=no, caller ID by Remote-Party-ID, G.722 then G.711µ).
// The driver states them as data in its Profile; the compiler renders them
// into pjsip.conf, so a driver never writes Asterisk config text.
//
// What every endpoint gets regardless of model is the compiler's, not an
// option here: context, auth (userpass, username = device ID), aors (the
// device's number), identify_by = auth_username, disallow = all, dtmf_mode
// = rfc4733, media_address and bind_rtp_to_media_address (the PBX address),
// the ACL, and an AOR with max_contacts = 1 and remove_existing = yes.
type EndpointOptions struct {
	// Transport is the endpoint's transport (pjsip `transport`), and the
	// transport the driver tells the device to use.
	Transport SIPTransport

	// Codecs are the codecs Asterisk allows on the endpoint, in preference
	// order (pjsip `allow`), by the names in KnownCodecs.
	Codecs []string

	// ForceRport is pjsip `force_rport`. The 79x5 needs it off.
	ForceRport bool

	// RewriteContact is pjsip `rewrite_contact`. The 79x5 needs it off.
	RewriteContact bool

	// RTPSymmetric is pjsip `rtp_symmetric`: send RTP back to where it came
	// from rather than to the SDP address.
	RTPSymmetric bool

	// DirectMedia is pjsip `direct_media`: let two endpoints exchange RTP
	// directly. Off in release A: the PBX stays in the media path.
	DirectMedia bool

	// SendRPID is pjsip `send_rpid`: caller ID by Remote-Party-ID, which the
	// 79x5 reads (its remotePartyID).
	SendRPID bool

	// SendPAI is pjsip `send_pai`: caller ID by P-Asserted-Identity.
	SendPAI bool

	// TrustIDOutbound is pjsip `trust_id_outbound`: send the identity
	// headers (RPID/PAI) to this endpoint even when the caller ID is marked
	// private. Every endpoint is a house device.
	TrustIDOutbound bool

	// QualifyFrequency is the AOR's `qualify_frequency`: how often Asterisk
	// sends OPTIONS to the contact. Zero turns qualify off. Whether the 79x5
	// answers OPTIONS is a bench question (design §8.6); until it is known
	// to, qualify stays off, because a contact that fails qualify is
	// skipped when dialling. Whole seconds.
	QualifyFrequency time.Duration

	// RegistrationExpiry is the AOR's default_expiration and
	// maximum_expiration: the longest registration Asterisk grants. A
	// device that asks for longer is granted this, so it must honour the
	// Expires Asterisk answers with. At least a minute, whole seconds.
	RegistrationExpiry time.Duration
}

// Validate checks the options can be rendered: a known transport, at least
// one codec, each known and listed once, and durations in whole seconds
// (RegistrationExpiry at least 60 s, Asterisk's minimum_expiration).
func (e EndpointOptions) Validate() error {
	var errs []error
	switch e.Transport {
	case SIPTransportUDP, SIPTransportTCP:
	default:
		errs = append(errs, fmt.Errorf("endpoint: transport %q is not udp or tcp", e.Transport))
	}
	if len(e.Codecs) == 0 {
		errs = append(errs, errors.New("endpoint: no codecs"))
	}
	for i, c := range e.Codecs {
		if !slices.Contains(KnownCodecs, c) {
			errs = append(errs, fmt.Errorf("endpoint: codec %q is not one of %v", c, KnownCodecs))
		}
		if slices.Contains(e.Codecs[:i], c) {
			errs = append(errs, fmt.Errorf("endpoint: codec %q is listed twice", c))
		}
	}
	if e.QualifyFrequency < 0 || e.QualifyFrequency%time.Second != 0 {
		errs = append(errs, fmt.Errorf("endpoint: qualify frequency %s is not a whole number of seconds >= 0", e.QualifyFrequency))
	}
	if e.RegistrationExpiry < time.Minute || e.RegistrationExpiry%time.Second != 0 {
		errs = append(errs, fmt.Errorf("endpoint: registration expiry %s is not a whole number of seconds >= 60", e.RegistrationExpiry))
	}
	return errors.Join(errs...)
}
