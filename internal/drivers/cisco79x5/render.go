package cisco79x5

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Limits from https://usecallmanager.nz/sepmac-cnf-xml.html.
const (
	// maxURL is the longest URL element (directoryURL, ...).
	maxURL = 255
	// maxHost is the longest processNodeName or NTP name.
	maxHost = 50
	// maxLineField is the longest line name, displayName, authName,
	// authPassword and featureLabel.
	maxLineField = 30
	// versionStampLength is how much of the config version goes into the
	// SEP file's versionStamp.
	versionStampLength = 32
)

// macPattern is a MAC as the SEP file name needs it: 12 upper-case hex
// digits (TFTP file names are case-sensitive).
var macPattern = regexp.MustCompile(`^[0-9A-F]{12}$`)

// ConfigFileName is the phone's config file name, SEP<MAC>.cnf.xml. mac must
// already be upper case, as drivers.Device.MAC is.
func ConfigFileName(mac string) string {
	return "SEP" + mac + ".cnf.xml"
}

// Render renders the phone's SEP<MAC>.cnf.xml and the shared dialplan.xml
// into tftp/. See drivers.Driver.Render for the contract and
// docs/plans/release-a.md §4 T1 for every value.
func (d *Driver) Render(dev drivers.Device, ctx drivers.RenderContext) (drivers.Files, error) {
	p, load, err := d.target(dev)
	if err != nil {
		return nil, err
	}
	tz, ok := WindowsTimeZone(ctx.Timezone)
	if !ok {
		return nil, fmt.Errorf("cisco79x5: no Windows time zone name for %q", ctx.Timezone)
	}
	if err := checkRender(dev, ctx); err != nil {
		return nil, err
	}

	stamp := dev.ConfigVersion
	if len(stamp) > versionStampLength {
		stamp = stamp[:versionStampLength]
	}
	doc := sep(dev, ctx, p, load.Name, tz, stamp)
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cisco79x5: marshal SEP file: %w", err)
	}
	var file bytes.Buffer
	file.WriteString(xml.Header)
	file.Write(body)
	file.WriteString("\n")

	return drivers.Files{
		"tftp/" + ConfigFileName(dev.MAC): file.Bytes(),
		"tftp/" + DialTemplateFile:        DialTemplate(ctx.DialPatterns),
	}, nil
}

// target returns the device's profile and target load, or an error wrapping
// drivers.ErrInvalidSpec when the model is not served or the target load is
// not in the manifest for it (the Invalid rule, with the same reasons).
func (d *Driver) target(dev drivers.Device) (drivers.Profile, drivers.FirmwareLoad, error) {
	p, ok := d.profile(dev.Model)
	if !ok {
		return p, drivers.FirmwareLoad{}, fmt.Errorf("%w: cisco79x5 does not serve model %q", drivers.ErrInvalidSpec, dev.Model)
	}
	load, ok := d.load(dev.Model, targetName(dev, p))
	if !ok {
		return p, load, fmt.Errorf("%w: %s", drivers.ErrInvalidSpec, textP1(targetName(dev, p), dev.Model))
	}
	return p, load, nil
}

// targetName is the device's target load: spec.firmware, or the model's
// default.
func targetName(dev drivers.Device, p drivers.Profile) string {
	if dev.Firmware != "" {
		return dev.Firmware
	}
	return p.Provisioning.DefaultFirmware
}

// checkRender checks the values Render writes against the phone's limits.
// Each failure is a plain error: the caller could have kept within it.
func checkRender(dev drivers.Device, ctx drivers.RenderContext) error {
	switch {
	case !macPattern.MatchString(dev.MAC):
		return fmt.Errorf("cisco79x5: MAC %q is not 12 upper-case hex digits", dev.MAC)
	case dev.ID == "" || len(dev.ID) > maxLineField:
		return fmt.Errorf("cisco79x5: device ID %q must be 1 to %d characters (authName)", dev.ID, maxLineField)
	case dev.Number == "" || len(dev.Number) > maxLineField:
		return fmt.Errorf("cisco79x5: number %q must be 1 to %d characters (line name)", dev.Number, maxLineField)
	case ctx.SIPPassword == "":
		// The placeholder itself is longer than authPassword's 30
		// characters; what the agent puts in its place is the 12-character
		// password (Provisioning.Password).
		return fmt.Errorf("cisco79x5: %s has no SIP password placeholder", dev.ID)
	case ctx.PBXAddress == "" || len(ctx.PBXAddress) > maxHost:
		return fmt.Errorf("cisco79x5: PBX address %q must be 1 to %d characters", ctx.PBXAddress, maxHost)
	case ctx.SIPPort < 1 || ctx.SIPPort > 65535:
		return fmt.Errorf("cisco79x5: SIP port %d is not a port", ctx.SIPPort)
	case len(ctx.NTPServer) > maxHost:
		return fmt.Errorf("cisco79x5: NTP server %q is over %d characters", ctx.NTPServer, maxHost)
	case len(ctx.DirectoryURL) > maxURL:
		return fmt.Errorf("cisco79x5: directory URL is %d characters, over %d", len(ctx.DirectoryURL), maxURL)
	case ctx.Signalling != drivers.SignallingPJSIP:
		return fmt.Errorf("cisco79x5: signalling backend %q is not implemented", ctx.Signalling)
	}
	if _, err := netip.ParseAddr(ctx.NTPServer); err != nil {
		return fmt.Errorf("cisco79x5: NTP server %q is not an IP address (the 79x5 cannot resolve a name there)", ctx.NTPServer)
	}
	for _, pat := range ctx.DialPatterns {
		if err := checkDialPattern(pat); err != nil {
			return err
		}
	}
	return nil
}

// sep builds the SEP document. Values that differ from usecallmanager's
// sample are commented here; the golden test's comment lists them all.
func sep(dev drivers.Device, ctx drivers.RenderContext, p drivers.Profile, load, tz, stamp string) sepDevice {
	line := LineLabel(dev.Label)
	return sepDevice{
		FullConfig:      true,
		DeviceProtocol:  "SIP",
		IPAddressMode:   0, // IPv4 only: the voice VLAN has no IPv6
		RedirectEnable:  false,
		EchoMultiEnable: false,
		DevicePool: sepDevicePool{
			DateTimeSetting: sepDateTimeSetting{
				// The sample's D/M/Y swapped to the American month first,
				// with a 12-hour clock: the user locale is English (US).
				DateTemplate: "M/D/YA",
				TimeZone:     tz,
				NTPs:         []sepNTP{{Name: ctx.NTPServer, NTPMode: "unicast"}},
			},
			CallManagerGroup: sepCallManagerGroup{
				Members: []sepMember{{
					Priority:    0,
					CallManager: sepCallManager{SIPPort: ctx.SIPPort, ProcessNodeName: ctx.PBXAddress},
				}},
			},
		},
		SIPProfile: sepSIPProfile{
			RegisterWithProxy: true,
			SIPCallFeatures: sepSIPCallFeatures{
				CallForwardURI:              "x-cisco-serviceuri-cfwdall",
				CallPickupURI:               "x-cisco-serviceuri-pickup",
				CallPickupListURI:           "x-cisco-serviceuri-opickup",
				CallPickupGroupURI:          "x-cisco-serviceuri-gpickup",
				MeetMeServiceURI:            "x-cisco-serviceuri-meetme",
				AbbreviatedDialURI:          "x-cisco-serviceuri-abbrdial",
				CallHoldRingback:            1,
				SemiAttendedTransfer:        true,
				AnonymousCallBlock:          0,
				CallerIDBlocking:            0,
				RemoteCcEnable:              true,
				RFC2543Hold:                 false,
				CnfJoinEnabled:              true,
				DNDControl:                  0,
				LocalCfwdEnable:             true,
				RetainForwardInformation:    false,
				URIDialingDisplayPreference: 1,
			},
			SIPStack: sepSIPStack{
				SIPInviteRetx:      6,
				SIPRetx:            10,
				TimerInviteExpires: 180,
				// The sample's 3600 replaced with the expiry Asterisk grants
				// (the AOR's maximum_expiration), so the two agree.
				TimerRegisterExpires:  int(p.Endpoint.RegistrationExpiry / time.Second),
				TimerRegisterDelta:    5,
				TimerKeepAliveExpires: 120,
				TimerSubscribeExpires: 120,
				TimerSubscribeDelta:   5,
				TimerT1:               500,
				TimerT2:               4000,
				MaxRedirects:          70,
				RemotePartyID:         true,
				UserInfo:              "Phone",
			},
			AutoAnswerTimer:                      1,
			AutoAnswerAltBehavior:                false,
			AutoAnswerOverride:                   true,
			TransferOnhookEnabled:                false,
			EnableVad:                            false,
			PreferredCodec:                       "none",
			DTMFAvtPayload:                       101,
			DTMFDbLevel:                          3,
			DTMFOutofBand:                        "avt",
			AlwaysUsePrimeLine:                   false,
			AlwaysUsePrimeLineVoiceMail:          false,
			KPML:                                 0,
			PhoneLabel:                           ScreenLabel(dev.Label),
			StutterMsgWaiting:                    0,
			CallStats:                            true,
			OffhookToFirstDigitTimer:             15000,
			SilentPeriodBetweenCallWaitingBursts: 10,
			StartMediaPort:                       16384,
			StopMediaPort:                        32766,
			NATEnabled:                           false,
			NATReceivedProcessing:                false,
			NATAddress:                           "",
			Lines: []sepLine{{
				Button:       1,
				LineIndex:    1,
				FeatureID:    9, // a phone line
				FeatureLabel: line,
				Proxy:        "USECALLMANAGER",
				Port:         ctx.SIPPort,
				Name:         dev.Number,
				DisplayName:  line,
				AutoAnswer: sepAutoAnswer{
					AutoAnswerEnabled: 0,
					AutoAnswerMode:    "Auto Answer with Headset",
				},
				CallWaiting:              1,
				AuthName:                 dev.ID,
				AuthPassword:             ctx.SIPPassword,
				Contact:                  dev.Number,
				SharedLine:               false,
				MessageWaitingLampPolicy: 3,
				MessageWaitingAMWI:       0,
				RingSettingIdle:          4,
				RingSettingActive:        5,
				ForwardCallInfoDisplay: sepForwardCallInfoDisplay{
					CallerName: true, CallerNumber: true, RedirectedNumber: true, DialedNumber: true,
				},
				MaxNumCalls: 5,
				BusyTrigger: 4,
				// The sample's "enable" turned off: nothing records calls, and
				// a Record softkey would be a dead key.
				RecordingOption: "disable",
			}},
			ExternalNumberMask:           "",
			VoipControlPort:              5060, // the phone's own SIP port
			DSCPForAudio:                 184,
			RingSettingBusyStationPolicy: 0,
			DialTemplate:                 DialTemplateFile,
		},
		MissedCallLoggingOption: "1",
		CommonProfile: sepCommonProfile{
			PhonePassword:         "",
			BackgroundImageAccess: true,
			CallLogBlfEnabled:     2,
		},
		LoadInformation: load,
		VendorConfig: sepVendorConfig{
			DisableSpeaker:           false,
			DisableSpeakerAndHeadset: false,
			EnableMuteFeature:        false,
			EnableGroupListen:        true,
			G722CodecSupport:         2, // enabled
			HandsetWidebandEnable:    2,
			HeadsetWidebandEnable:    2,
			HeadsetWidebandUIControl: 1,
			HandsetWidebandUIControl: 1,
			SettingsAccess:           settingsAccessRestricted,
			VideoCapability:          0,
			WebAccess:                0, // 0 = enabled: the web pages are a sensor
			WebProtocol:              0,
			SSHAccess:                1, // 1 = disabled
			DisplayRefreshRate:       0,
			MoreKeyReversionTimer:    5,
			AutoSelectLineEnable:     1,
			AutoCallSelect:           1,
			IncomingCallToastTimer:   5,
			// The sample's 1 turned off: a phone that took its firmware from
			// another phone would upgrade unseen by the fetch log, and so by
			// the Upgrading rule.
			PeerFirmwareSharing:            0,
			JoinAndDirectTransferPolicy:    0,
			DetectCMConnectionFailure:      0,
			RTCP:                           1,
			GARP:                           0,
			EhookEnable:                    0,
			ShowCallHistoryForSelectedLine: 0,
			DialToneFromReleaseKey:         0,
			DFBit:                          0,
			// 1 = disabled: a computer behind the phone would land in the
			// voice VLAN on its untagged access port (design §4).
			PCPort:            1,
			SpanToPCPort:      1,
			VoiceVlanAccess:   0,
			EnableCdpSwPort:   1,
			EnableCdpPcPort:   0,
			EnableLldpSwPort:  1,
			EnableLldpPcPort:  0,
			PowerNegotiation:  0,
			EapAuthentication: 1,
		},
		VersionStamp: stamp,
		UserLocale: sepUserLocale{
			Name:       "English_United_States",
			UID:        1,
			WinCharSet: "utf-8",
		},
		NetworkLocale:      "United_States",
		NetworkLocaleInfo:  sepNetworkLocaleInfo{Name: "United_States"},
		DeviceSecurityMode: 1, // non-secure: no TLS in release A
		DirectoryURL:       ctx.DirectoryURL,
		ProxyServerURL:     "",
		PhoneServices: sepPhoneServices{
			UseHTTPS:     false,
			Provisioning: 2,
			// The sample's Voicemail entry is left out: no voicemail in A.
			Services: []sepPhoneService{
				{Type: 1, Name: "Missed Calls", URL: "Application:Cisco/MissedCalls"},
				{Type: 1, Name: "Received Calls", URL: "Application:Cisco/ReceivedCalls"},
				{Type: 1, Name: "Placed Calls", URL: "Application:Cisco/PlacedCalls"},
			},
		},
		TransportLayerProtocol: transportLayerProtocol(p.Endpoint.Transport),
		// The sample's 1 turned off: personalization takes pushed XML on
		// /CGI/Execute, which nothing authenticates until release C sets an
		// authenticationURL.
		PhonePersonalization: 0,
		AutoCallPickupEnable: true,
		DNDCallAlert:         1,
		DNDReminderTimer:     5,
		// The sample's 2 (device default) made explicit: G.722 on.
		AdvertiseG722Codec: 1,
		Rollover:           0,
		JoinAcrossLines:    0,
		EncrConfig:         false,
	}
}

// checkDialPattern reports a pattern outside the notation of
// drivers.RenderContext.DialPatterns (digits, X, "*", "#", and one trailing
// "."), which is a bug in whoever built the context. DialTemplate would
// leave such a pattern out; Render refuses it instead.
func checkDialPattern(p string) error {
	body, _ := strings.CutSuffix(p, ".")
	if body == "" {
		return fmt.Errorf("cisco79x5: dial pattern %q is empty", p)
	}
	for _, r := range body {
		if (r < '0' || r > '9') && r != 'X' && r != '*' && r != '#' {
			return fmt.Errorf("cisco79x5: dial pattern %q: %q is not a digit, X, * or #", p, r)
		}
	}
	return nil
}

// transportLayerProtocol is the SEP file's code for a SIP transport: 1 TCP,
// 2 UDP. The profile never names UDP for a 79x5 ("UDP causes SIP retransmit
// errors"), but the code follows the profile.
func transportLayerProtocol(t drivers.SIPTransport) int {
	if t == drivers.SIPTransportUDP {
		return 2
	}
	return 1
}
