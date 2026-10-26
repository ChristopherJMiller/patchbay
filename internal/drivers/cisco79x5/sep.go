package cisco79x5

import "encoding/xml"

// settingsAccessRestricted is vendorConfig/settingsAccess: CUCM's
// "Restricted" (2), which leaves only the user preferences in the phone's
// Settings menu, so nobody can change its network or call settings from the
// keypad (design §4). usecallmanager.nz documents only 0 (disabled) and 1
// (enabled). Bench (plan §8): if the phone treats 2 as disabled or enabled,
// decide with Chris between 0 and 1, and fix P9 (decide.go) to match.
const settingsAccessRestricted = 2

// The SEP<MAC>.cnf.xml document, element by element as in the sample of
// https://usecallmanager.nz/sepmac-cnf-xml.html (checked 2026-10-15), with
// the elements a 79x5 on stock PJSIP has no use for left out. The golden
// test's comment (render_test.go) names every left-out sample element and
// why, and every value that differs from the sample's.
//
// encoding/xml writes the fields in declaration order and escapes all text.

type sepDevice struct {
	XMLName                 xml.Name             `xml:"device"`
	FullConfig              bool                 `xml:"fullConfig"`
	DeviceProtocol          string               `xml:"deviceProtocol"`
	IPAddressMode           int                  `xml:"ipAddressMode"`
	RedirectEnable          bool                 `xml:"redirectEnable"`
	EchoMultiEnable         bool                 `xml:"echoMultiEnable"`
	DevicePool              sepDevicePool        `xml:"devicePool"`
	SIPProfile              sepSIPProfile        `xml:"sipProfile"`
	MissedCallLoggingOption string               `xml:"MissedCallLoggingOption"`
	CommonProfile           sepCommonProfile     `xml:"commonProfile"`
	LoadInformation         string               `xml:"loadInformation"`
	VendorConfig            sepVendorConfig      `xml:"vendorConfig"`
	VersionStamp            string               `xml:"versionStamp"`
	UserLocale              sepUserLocale        `xml:"userLocale"`
	NetworkLocale           string               `xml:"networkLocale"`
	NetworkLocaleInfo       sepNetworkLocaleInfo `xml:"networkLocaleInfo"`
	DeviceSecurityMode      int                  `xml:"deviceSecurityMode"`
	ServicesURL             string               `xml:"servicesURL"`
	DirectoryURL            string               `xml:"directoryURL"`
	ProxyServerURL          string               `xml:"proxyServerURL"`
	PhoneServices           sepPhoneServices     `xml:"phoneServices"`
	TransportLayerProtocol  int                  `xml:"transportLayerProtocol"`
	PhonePersonalization    int                  `xml:"phonePersonalization"`
	AutoCallPickupEnable    bool                 `xml:"autoCallPickupEnable"`
	DNDCallAlert            int                  `xml:"dndCallAlert"`
	DNDReminderTimer        int                  `xml:"dndReminderTimer"`
	AdvertiseG722Codec      int                  `xml:"advertiseG722Codec"`
	Rollover                int                  `xml:"rollover"`
	JoinAcrossLines         int                  `xml:"joinAcrossLines"`
	EncrConfig              bool                 `xml:"encrConfig"`
}

type sepDevicePool struct {
	DateTimeSetting  sepDateTimeSetting  `xml:"dateTimeSetting"`
	CallManagerGroup sepCallManagerGroup `xml:"callManagerGroup"`
}

type sepDateTimeSetting struct {
	DateTemplate string   `xml:"dateTemplate"`
	TimeZone     string   `xml:"timeZone"`
	NTPs         []sepNTP `xml:"ntps>ntp"`
}

type sepNTP struct {
	Name    string `xml:"name"`
	NTPMode string `xml:"ntpMode"`
}

type sepCallManagerGroup struct {
	Members []sepMember `xml:"members>member"`
}

type sepMember struct {
	Priority    int            `xml:"priority,attr"`
	CallManager sepCallManager `xml:"callManager"`
}

type sepCallManager struct {
	SIPPort         int    `xml:"ports>sipPort"`
	ProcessNodeName string `xml:"processNodeName"`
}

type sepSIPProfile struct {
	RegisterWithProxy                    bool               `xml:"sipProxies>registerWithProxy"`
	SIPCallFeatures                      sepSIPCallFeatures `xml:"sipCallFeatures"`
	SIPStack                             sepSIPStack        `xml:"sipStack"`
	AutoAnswerTimer                      int                `xml:"autoAnswerTimer"`
	AutoAnswerAltBehavior                bool               `xml:"autoAnswerAltBehavior"`
	AutoAnswerOverride                   bool               `xml:"autoAnswerOverride"`
	TransferOnhookEnabled                bool               `xml:"transferOnhookEnabled"`
	EnableVad                            bool               `xml:"enableVad"`
	PreferredCodec                       string             `xml:"preferredCodec"`
	DTMFAvtPayload                       int                `xml:"dtmfAvtPayload"`
	DTMFDbLevel                          int                `xml:"dtmfDbLevel"`
	DTMFOutofBand                        string             `xml:"dtmfOutofBand"`
	AlwaysUsePrimeLine                   bool               `xml:"alwaysUsePrimeLine"`
	AlwaysUsePrimeLineVoiceMail          bool               `xml:"alwaysUsePrimeLineVoiceMail"`
	KPML                                 int                `xml:"kpml"`
	PhoneLabel                           string             `xml:"phoneLabel"`
	StutterMsgWaiting                    int                `xml:"stutterMsgWaiting"`
	CallStats                            bool               `xml:"callStats"`
	OffhookToFirstDigitTimer             int                `xml:"offhookToFirstDigitTimer"`
	SilentPeriodBetweenCallWaitingBursts int                `xml:"silentPeriodBetweenCallWaitingBursts"`
	StartMediaPort                       int                `xml:"startMediaPort"`
	StopMediaPort                        int                `xml:"stopMediaPort"`
	NATEnabled                           bool               `xml:"natEnabled"`
	NATReceivedProcessing                bool               `xml:"natReceivedProcessing"`
	NATAddress                           string             `xml:"natAddress"`
	Lines                                []sepLine          `xml:"sipLines>line"`
	ExternalNumberMask                   string             `xml:"externalNumberMask"`
	VoipControlPort                      int                `xml:"voipControlPort"`
	DSCPForAudio                         int                `xml:"dscpForAudio"`
	RingSettingBusyStationPolicy         int                `xml:"ringSettingBusyStationPolicy"`
	DialTemplate                         string             `xml:"dialTemplate"`
}

type sepSIPCallFeatures struct {
	CallForwardURI              string `xml:"callForwardURI"`
	CallPickupURI               string `xml:"callPickupURI"`
	CallPickupListURI           string `xml:"callPickupListURI"`
	CallPickupGroupURI          string `xml:"callPickupGroupURI"`
	MeetMeServiceURI            string `xml:"meetMeServiceURI"`
	AbbreviatedDialURI          string `xml:"abbreviatedDialURI"`
	CallHoldRingback            int    `xml:"callHoldRingback"`
	SemiAttendedTransfer        bool   `xml:"semiAttendedTransfer"`
	AnonymousCallBlock          int    `xml:"anonymousCallBlock"`
	CallerIDBlocking            int    `xml:"callerIdBlocking"`
	RemoteCcEnable              bool   `xml:"remoteCcEnable"`
	RFC2543Hold                 bool   `xml:"rfc2543Hold"`
	CnfJoinEnabled              bool   `xml:"cnfJoinEnabled"`
	DNDControl                  int    `xml:"dndControl"`
	LocalCfwdEnable             bool   `xml:"localCfwdEnable"`
	RetainForwardInformation    bool   `xml:"retainForwardInformation"`
	URIDialingDisplayPreference int    `xml:"uriDialingDisplayPreference"`
}

type sepSIPStack struct {
	SIPInviteRetx         int    `xml:"sipInviteRetx"`
	SIPRetx               int    `xml:"sipRetx"`
	TimerInviteExpires    int    `xml:"timerInviteExpires"`
	TimerRegisterExpires  int    `xml:"timerRegisterExpires"`
	TimerRegisterDelta    int    `xml:"timerRegisterDelta"`
	TimerKeepAliveExpires int    `xml:"timerKeepAliveExpires"`
	TimerSubscribeExpires int    `xml:"timerSubscribeExpires"`
	TimerSubscribeDelta   int    `xml:"timerSubscribeDelta"`
	TimerT1               int    `xml:"timerT1"`
	TimerT2               int    `xml:"timerT2"`
	MaxRedirects          int    `xml:"maxRedirects"`
	RemotePartyID         bool   `xml:"remotePartyID"`
	UserInfo              string `xml:"userInfo"`
}

type sepLine struct {
	Button                   int                       `xml:"button,attr"`
	LineIndex                int                       `xml:"lineIndex,attr"`
	FeatureID                int                       `xml:"featureID"`
	FeatureLabel             string                    `xml:"featureLabel"`
	Proxy                    string                    `xml:"proxy"`
	Port                     int                       `xml:"port"`
	Name                     string                    `xml:"name"`
	DisplayName              string                    `xml:"displayName"`
	AutoAnswer               sepAutoAnswer             `xml:"autoAnswer"`
	CallWaiting              int                       `xml:"callWaiting"`
	AuthName                 string                    `xml:"authName"`
	AuthPassword             string                    `xml:"authPassword"`
	Contact                  string                    `xml:"contact"`
	SharedLine               bool                      `xml:"sharedLine"`
	MessageWaitingLampPolicy int                       `xml:"messageWaitingLampPolicy"`
	MessageWaitingAMWI       int                       `xml:"messageWaitingAMWI"`
	RingSettingIdle          int                       `xml:"ringSettingIdle"`
	RingSettingActive        int                       `xml:"ringSettingActive"`
	ForwardCallInfoDisplay   sepForwardCallInfoDisplay `xml:"forwardCallInfoDisplay"`
	MaxNumCalls              int                       `xml:"maxNumCalls"`
	BusyTrigger              int                       `xml:"busyTrigger"`
	RecordingOption          string                    `xml:"recordingOption"`
}

type sepAutoAnswer struct {
	AutoAnswerEnabled int    `xml:"autoAnswerEnabled"`
	AutoAnswerMode    string `xml:"autoAnswerMode"`
}

type sepForwardCallInfoDisplay struct {
	CallerName       bool `xml:"callerName"`
	CallerNumber     bool `xml:"callerNumber"`
	RedirectedNumber bool `xml:"redirectedNumber"`
	DialedNumber     bool `xml:"dialedNumber"`
}

type sepCommonProfile struct {
	PhonePassword         string `xml:"phonePassword"`
	BackgroundImageAccess bool   `xml:"backgroundImageAccess"`
	CallLogBlfEnabled     int    `xml:"callLogBlfEnabled"`
}

type sepVendorConfig struct {
	DisableSpeaker                 bool `xml:"disableSpeaker"`
	DisableSpeakerAndHeadset       bool `xml:"disableSpeakerAndHeadset"`
	EnableMuteFeature              bool `xml:"enableMuteFeature"`
	EnableGroupListen              bool `xml:"enableGroupListen"`
	G722CodecSupport               int  `xml:"g722CodecSupport"`
	HandsetWidebandEnable          int  `xml:"handsetWidebandEnable"`
	HeadsetWidebandEnable          int  `xml:"headsetWidebandEnable"`
	HeadsetWidebandUIControl       int  `xml:"headsetWidebandUIControl"`
	HandsetWidebandUIControl       int  `xml:"handsetWidebandUIControl"`
	SettingsAccess                 int  `xml:"settingsAccess"`
	VideoCapability                int  `xml:"videoCapability"`
	WebAccess                      int  `xml:"webAccess"`
	WebProtocol                    int  `xml:"webProtocol"`
	SSHAccess                      int  `xml:"sshAccess"`
	DisplayRefreshRate             int  `xml:"displayRefreshRate"`
	sepDisplay                          // the screen's power saving (display.go)
	MoreKeyReversionTimer          int  `xml:"moreKeyReversionTimer"`
	AutoSelectLineEnable           int  `xml:"autoSelectLineEnable"`
	AutoCallSelect                 int  `xml:"autoCallSelect"`
	IncomingCallToastTimer         int  `xml:"incomingCallToastTimer"`
	JoinAndDirectTransferPolicy    int  `xml:"joinAndDirectTransferPolicy"`
	PeerFirmwareSharing            int  `xml:"peerFirmwareSharing"`
	DetectCMConnectionFailure      int  `xml:"detectCMConnectionFailure"`
	RTCP                           int  `xml:"rtcp"`
	GARP                           int  `xml:"garp"`
	EhookEnable                    int  `xml:"ehookEnable"`
	ShowCallHistoryForSelectedLine int  `xml:"showCallHistoryForSelectedLine"`
	DialToneFromReleaseKey         int  `xml:"dialToneFromReleaseKey"`
	DFBit                          int  `xml:"dfBit"`
	PCPort                         int  `xml:"pcPort"`
	SpanToPCPort                   int  `xml:"spanToPCPort"`
	VoiceVlanAccess                int  `xml:"voiceVlanAccess"`
	EnableCdpSwPort                int  `xml:"enableCdpSwPort"`
	EnableCdpPcPort                int  `xml:"enableCdpPcPort"`
	EnableLldpSwPort               int  `xml:"enableLldpSwPort"`
	EnableLldpPcPort               int  `xml:"enableLldpPcPort"`
	PowerNegotiation               int  `xml:"powerNegotiation"`
	EapAuthentication              int  `xml:"eapAuthentication"`
}

type sepUserLocale struct {
	Name       string `xml:"name"`
	UID        int    `xml:"uid"`
	LangCode   string `xml:"langCode"`
	Version    string `xml:"version"`
	WinCharSet string `xml:"winCharSet"`
}

type sepNetworkLocaleInfo struct {
	Name    string `xml:"name"`
	Version string `xml:"version"`
}

type sepPhoneServices struct {
	UseHTTPS     bool              `xml:"useHTTPS,attr"`
	Provisioning int               `xml:"provisioning"`
	Services     []sepPhoneService `xml:"phoneService"`
}

type sepPhoneService struct {
	Type     int    `xml:"type,attr"`
	Category int    `xml:"category,attr"`
	Name     string `xml:"name"`
	URL      string `xml:"url"`
	Vendor   string `xml:"vendor"`
	Version  string `xml:"version"`
}
