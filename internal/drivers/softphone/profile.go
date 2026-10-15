package softphone

import (
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Condition reasons (exported: they are status contract). Reasons the API
// names for every driver (AgentUnreachable, PBXDown, ApplyFailed,
// ApplyPending, Offline, NumberNotInPlan) are v1alpha1's.
const (
	ReasonUnknownModel = "UnknownModel"
	ReasonIssued       = "Issued"
	ReasonProvisioned  = "Provisioned"
	ReasonRegistered   = "Registered"
	ReasonFetched      = "Fetched"
	ReasonNotFetched   = "NotFetched"
	ReasonOldVersion   = "OldVersion"
	ReasonContact      = "Contact"
	ReasonNoContact    = "NoContact"
	ReasonCurrent      = "Current"
	ReasonStale        = "Stale"
	ReasonNoConflict   = "NoConflict"
	ReasonInvalid      = "Invalid"

	// ReasonNumberConflict is NumberConflict=True's reason, and Ready's when
	// the conflict is why the softphone is Invalid. The condition message
	// (Device.ClaimMessage) names the older holder.
	ReasonNumberConflict = "NumberConflict"
)

// registrationExpiry is the longest registration Asterisk grants a
// softphone, and what the provisioning document asks Linphone to request
// (proxy_0/reg_expires), so the two agree by construction.
const registrationExpiry = 600 * time.Second

// profile is the softphone model's capability profile (docs/plans/release-a.md
// §2(a) and T2).
//
// Endpoint, decided deliberately (plan decision (a)):
//   - UDP: Linphone's default transport for a third-party SIP account, and
//     softphones are on the LAN only (no TLS in release A);
//   - force_rport, rewrite_contact and rtp_symmetric on. A softphone is a
//     laptop or a handset on Wi-Fi: its address changes as it roams and
//     renews DHCP, and the address Linphone writes into Contact and SDP can
//     be the wrong interface's (a VPN, a container bridge, a hotspot's NAT).
//     With these on, Asterisk answers, calls and sends RTP to where the
//     packets actually came from, which over UDP is always where the
//     softphone listens. That opens nothing: there is no NAT rule, and the
//     endpoint ACL still admits only the LAN and the voice VLAN. The 79x5,
//     on TCP at a fixed address, needs the opposite;
//   - caller ID by P-Asserted-Identity rather than Remote-Party-ID, which
//     liblinphone does not read at all; caller names reach Linphone in the
//     From header, which Asterisk fills from the caller ID.
var profile = drivers.Profile{
	Model: drivers.ModelSoftphone,
	Hardware: drivers.Hardware{
		Codecs: []string{"g722", "ulaw", "alaw"},
	},
	Provisioning: drivers.Provisioning{
		ConfigFile:     "provisioning/{ID}.xml",
		ScreenLabelMax: 0,
		LineLabelMax:   64,
		Password:       drivers.PasswordPolicy{Length: 24, Alphabet: drivers.Alphanumeric},
		// A softphone is re-provisioned by scanning a QR code, never by a
		// NOTIFY: ConfigStale's resync does not exist for it.
		ResyncByNotify: false,
	},
	Endpoint: drivers.EndpointOptions{
		Transport:          drivers.SIPTransportUDP,
		Codecs:             []string{"g722", "ulaw"},
		ForceRport:         true,
		RewriteContact:     true,
		RTPSymmetric:       true,
		DirectMedia:        false,
		SendRPID:           false,
		SendPAI:            true,
		TrustIDOutbound:    true,
		QualifyFrequency:   0,
		RegistrationExpiry: registrationExpiry,
	},
	// Features on stock PJSIP. Wideband holds because the provisioning
	// document enables G.722, which Linphone ships disabled. Caller ID
	// updates (FeatureCallerIDUpdate) are absent, so Unsupported: liblinphone
	// never reads Remote-Party-ID, and reads P-Asserted-Identity only at call
	// setup and ringing, and only with sip/call_logs_use_asserted_id_instead_of_from
	// set (coreapi/callbacks.c call_received, src/conference/session/call-session.cpp
	// updateToFromAssertedIdentity, linphone-sdk b5cd87b6), so a mid-call
	// update never reaches its screen. The caller's name at setup comes in
	// From and needs no feature.
	Features: map[drivers.Signalling]map[drivers.Feature]drivers.Support{
		drivers.SignallingPJSIP: {
			drivers.FeatureHold:        drivers.Supported,
			drivers.FeatureTransfer:    drivers.Supported,
			drivers.FeatureWideband:    drivers.Supported,
			drivers.FeatureRFC4733DTMF: drivers.Supported,
		},
	},
}
