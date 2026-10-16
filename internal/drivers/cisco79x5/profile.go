package cisco79x5

import (
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Name is the driver's name.
const Name = "cisco79x5"

// DefaultLoad is the load a CP-7945G or CP-7965G runs when its Phone names
// none: SIP 9.4(2)SR4-3, from cmterm-7945_7965-sip.9-4-2SR4-3.zip (design §12).
const DefaultLoad = "SIP45.9-4-2SR4-3S"

// defaultLoad7975 is the CP-7975G's SIP 9.4(2)SR4-3 load. It is not in the
// manifest (its zip, sizes and file names were never researched), so a 7975
// is Invalid with reason UnknownFirmware until it is added: honest, not
// guessed.
const defaultLoad7975 = "SIP75.9-4-2SR4-3S"

// Condition reasons. They are status contract: tools read them.
const (
	ReasonUnknownModel    = "UnknownModel"
	ReasonUnknownFirmware = "UnknownFirmware"
	ReasonUnseen          = "Unseen"
	ReasonNeedsFirmware   = "NeedsFirmware"
	ReasonNeedsTrustReset = "NeedsTrustReset"
	ReasonUpgrading       = "Upgrading"
	ReasonRegistering     = "Registering"
	ReasonConfigStale     = "ConfigStale"
	ReasonRegistered      = "Registered"
	ReasonUnreachable     = "Unreachable"
	ReasonStaged          = "Staged"
	ReasonNotStaged       = "NotStaged"
	ReasonManifestPending = "ManifestPending"
	ReasonRunning         = "Running"
	ReasonOtherLoad       = "OtherLoad"
	ReasonNotReported     = "NotReported"
	ReasonFetched         = "Fetched"
	ReasonNotFetched      = "NotFetched"
	ReasonOldVersion      = "OldVersion"
	ReasonContact         = "Contact"
	ReasonNoContact       = "NoContact"
	ReasonQualifyFailed   = "QualifyFailed"
	ReasonCurrent         = "Current"
	ReasonStale           = "Stale"
	ReasonNoConflict      = "NoConflict"
	ReasonAllSupported    = "AllSupported"
	ReasonShortened       = "Shortened"
	ReasonAsWritten       = "AsWritten"
	ReasonInvalid         = "Invalid"
	// ReasonUnsupportedFeature is Unsupported True, and Ready False, when
	// the spec asks for a feature the profile lacks on the backend.
	ReasonUnsupportedFeature = "UnsupportedFeature"

	// ReasonNumberConflict and ReasonMACConflict are the reasons of a
	// NumberConflict or MACConflict condition that is True, and of Ready
	// when one of them makes the phone Invalid.
	ReasonNumberConflict = "NumberConflict"
	ReasonMACConflict    = "MACConflict"
)

// models are the models the driver serves, in the CRD enum's order.
var models = []string{
	string(v1alpha1.PhoneModelCisco7945),
	string(v1alpha1.PhoneModelCisco7965),
	string(v1alpha1.PhoneModelCisco7975),
}

// profile returns a fresh copy of a model's profile (callers may change it),
// and false for a model the driver does not serve.
//
// Only the CP-7945G is in the house. The 7965 and 7975 hardware figures are
// the 7945's with their line key counts (6 and 8) from Cisco's data sheets;
// the rest is unverified.
func profile(model string) (drivers.Profile, bool) {
	var lineKeys int
	load := DefaultLoad
	switch v1alpha1.PhoneModel(model) {
	case v1alpha1.PhoneModelCisco7945:
		lineKeys = 2
	case v1alpha1.PhoneModelCisco7965:
		lineKeys = 6
	case v1alpha1.PhoneModelCisco7975:
		lineKeys = 8
		load = defaultLoad7975
	default:
		return drivers.Profile{}, false
	}
	return drivers.Profile{
		Model: model,
		Hardware: drivers.Hardware{
			LineKeys:           lineKeys,
			Softkeys:           4,
			Display:            drivers.Size{Width: 320, Height: 240},
			ServiceArea:        drivers.Size{Width: 298, Height: 156},
			Wallpaper:          drivers.Size{Width: 320, Height: 212},
			WallpaperThumbnail: drivers.Size{Width: 80, Height: 53},
			RingtoneFormat:     "raw µ-law, 8 kHz, 8-bit mono, 240–16080 samples",
			Codecs:             []string{"g722", "ulaw", "alaw", "g729a", "ilbc", "l16"},
		},
		Provisioning: drivers.Provisioning{
			ConfigFile:      "SEP{MAC}.cnf.xml",
			DefaultFirmware: load,
			ScreenLabelMax:  screenLabelMax,
			LineLabelMax:    lineLabelMax,
			// "The 79x5 reportedly fails with 32-character passwords and is
			// known to work at 12 or fewer" (design §4). Bench: the highest
			// length that registers.
			Password: drivers.PasswordPolicy{Length: 12, Alphabet: drivers.Alphanumeric},
			// Bench (§8.6): does a service-control NOTIFY make it re-read its
			// config? Until it is known not to, try, and ask a human after
			// two attempts.
			ResyncByNotify: true,
		},
		Endpoint: endpoint(),
		Features: map[drivers.Signalling]map[drivers.Feature]drivers.Support{
			drivers.SignallingPJSIP: pjsipFeatures(),
		},
	}, true
}

// endpoint is what a 79x5 needs from PJSIP (design §4, plan §2(a)): TCP,
// force_rport and rewrite_contact off, caller ID by Remote-Party-ID, G.722
// then G.711µ.
func endpoint() drivers.EndpointOptions {
	return drivers.EndpointOptions{
		Transport:       drivers.SIPTransportTCP,
		Codecs:          []string{"g722", "ulaw"},
		ForceRport:      false,
		RewriteContact:  false,
		RTPSymmetric:    false,
		DirectMedia:     false,
		SendRPID:        true,
		SendPAI:         false,
		TrustIDOutbound: true,
		// Bench (§8.6): does it answer OPTIONS? Off until it is known to: a
		// contact that fails qualify is skipped when dialling.
		QualifyFrequency: 0,
		// Also rendered into the SEP file (sipStack/timerRegisterExpires), so
		// the phone asks for exactly what Asterisk grants.
		RegistrationExpiry: 600 * time.Second,
	}
}

// pjsipFeatures is the feature table on stock Asterisk 22 PJSIP (design
// §8.5, research §4). Every feature is listed, so nothing is Unsupported by
// omission.
func pjsipFeatures() map[drivers.Feature]drivers.Support {
	return map[drivers.Feature]drivers.Support{
		// These need Cisco-proprietary SIP (out-of-dialog x-cisco-remotecc
		// REFERs, PUBLISH, Cisco PIDF) that stock PJSIP lacks; CUCM mode
		// registers line 1 only.
		drivers.FeatureBLF:             drivers.Unsupported,
		drivers.FeaturePark:            drivers.Unsupported,
		drivers.FeatureConference:      drivers.Unsupported,
		drivers.FeatureDNDSync:         drivers.Unsupported,
		drivers.FeatureCallForwardSync: drivers.Unsupported,
		drivers.FeatureIntercom:        drivers.Unsupported,
		drivers.FeatureSecondLine:      drivers.Unsupported,

		drivers.FeatureHold:            drivers.Supported,
		drivers.FeatureWideband:        drivers.Supported,
		drivers.FeatureRFC4733DTMF:     drivers.Supported,
		drivers.FeatureCallerIDUpdate:  drivers.Supported,
		drivers.FeatureDistinctiveRing: drivers.Supported,
		// Bench 2026-10-15 (§8.6): transfer works on stock PJSIP, a CP-7945G
		// on SIP 9.4(2)SR4-3, with two calls up and the held one recalled.
		drivers.FeatureTransfer: drivers.Supported,
		// Pickup and MeetMe are x-cisco-serviceuri-* INVITEs the dialplan
		// must answer; nothing has dialled one yet.
		drivers.FeaturePickup: drivers.Untested,
		drivers.FeatureMeetMe: drivers.Untested,
		// Release C features: documented as working on the 79x5, not yet
		// tried against this firmware.
		drivers.FeatureSpeedDial:        drivers.Untested,
		drivers.FeatureServiceURL:       drivers.Untested,
		drivers.FeatureWallpaper:        drivers.Untested,
		drivers.FeatureCustomRingtones:  drivers.Untested,
		drivers.FeatureXMLServices:      drivers.Untested,
		drivers.FeaturePushNotification: drivers.Untested,
		// Bench (§8.6): RTPRx push on SIP 9.4(2); a 2008 report says early
		// 79x5 firmware ignored it.
		drivers.FeatureRTPRx: drivers.Untested,
		// Phone.spec.display (display.go). Bench 2026-10-15 (plan §8): with
		// every day inactive and a 1 min idle timeout the 7945's screen goes
		// dark a minute after the last key press. Wake-on-ring not yet seen.
		drivers.FeatureDisplayPowerSave: drivers.Supported,
	}
}
