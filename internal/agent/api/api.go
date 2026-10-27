// Package api is the wire contract between the controller and the agent: the
// JSON documents of the agent's control API on :9440 (design §5).
//
// The agent senses and acts; it never decides and never writes resource
// status. The controller polls GET /v1/observations every few seconds and
// turns what changed into reconciles, and asks the agent to act through POST
// /v1/actions/{resync,restart}.
//
// Every /v1 request carries the generated agent token as a bearer token. The
// documents carry no secrets. The agent's memory is disposable: anything that
// must survive an agent restart is copied into resource status by the
// controller.
//
// The package holds types only, so both sides (and tests and the e2e fake
// phone) can import it without pulling in either implementation.
package api

import "time"

// Paths of the control API.
const (
	// PathObservations is GET: the agent's current Observations document.
	PathObservations = "/v1/observations"
	// PathActionResync is POST with an ActionRequest: make a phone re-read
	// its config (Cisco service-control, action=restart).
	PathActionResync = "/v1/actions/resync"
	// PathActionRestart is POST with an ActionRequest: reboot a phone
	// (Cisco service-control, action=reset).
	PathActionRestart = "/v1/actions/restart"
	// PathActionPush is POST with a PushRequest: store a message page for a
	// phone and push it over the phone's /CGI/Execute (or clear the screen,
	// or send raw Execute URIs). Answered 200 with a PushResponse once the
	// request decoded and named a device.
	PathActionPush = "/v1/actions/push"
	// PathFirmware is the firmware upload endpoint's prefix: PUT
	// FirmwarePath(load, name) with the file's bytes as the body (and a
	// Content-Length of at most MaxFirmwareFileSize) stores one file of a
	// manifest load on the firmware volume, after the agent has checked its
	// SHA-256 against the manifest compiled into the agent. `patchbay
	// firmware load` reaches it through pods/portforward. The answer is a
	// FirmwareUploadResponse with:
	//   - 201 Created: verified and stored;
	//   - 200 OK: already stored with that digest (uploads are idempotent);
	//   - 404 Not Found: no such load, or no such file in it;
	//   - 412 Precondition Failed: the manifest's digest is SHA256Pending,
	//     so nothing can verify the file and nothing is stored;
	//   - 413 Content Too Large: over MaxFirmwareFileSize;
	//   - 422 Unprocessable Content: the digest differs; nothing is stored.
	PathFirmware = "/v1/firmware"
	// PathHealthz answers 200 while the agent process is alive. No token.
	PathHealthz = "/healthz"
	// PathReadyz answers 200 once the agent has applied a config version and
	// installed every sound prompt from its image (503 with the reason
	// before). It backs the native sidecar's startup probe, so Asterisk
	// starts only after valid config exists and 911 never plays a missing
	// announcement. No token.
	PathReadyz = "/readyz"
	// PathMetrics is GET: the agent's Prometheus metrics (the chart's
	// PodMonitor scrapes it). No token: it carries counts and version
	// digests, never a secret.
	PathMetrics = "/metrics"
)

// FirmwarePath is the upload path of one file of one load, e.g.
// /v1/firmware/SIP45.9-4-2SR4-3S/apps45.9-4-2SR4-3.sbn. Load and file names
// are manifest names, which never contain a slash.
func FirmwarePath(load, name string) string {
	return PathFirmware + "/" + load + "/" + name
}

// FirmwareUploadTimeout is how long one firmware file's upload may take,
// end to end: the CLI's budget for each file through its port-forward,
// and the agent's deadline for an authenticated PUT on PathFirmware (every
// other request gets 30 s). A 32 MiB file needs about 110 KB/s.
const FirmwareUploadTimeout = 5 * time.Minute

// MaxFirmwareFileSize is the largest firmware file the agent accepts. The
// largest 79x5 file is about 4.6 MB.
const MaxFirmwareFileSize = 32 << 20

// ProvisioningPathPrefix is where device HTTP (:8088) serves a softphone's
// provisioning document: ProvisioningPathPrefix + <one-time token>. The
// controller builds status.provisioningURL from it; the agent serves the
// document only to the current token, once.
const ProvisioningPathPrefix = "/p/"

// DefaultPort is the agent's control API port on the PBX node.
const DefaultPort = 9440

// Cisco service-control bodies. The agent sends a SIP "NOTIFY Event:
// service-control" through AMI PJSIPNotify with body action=<value> and the
// contact's RegisterCallId (design §5). The names are Cisco's and read
// backwards: "restart" re-reads config, "reset" reboots.
const (
	// ServiceControlRestart is the body action for a resync.
	ServiceControlRestart = "restart"
	// ServiceControlReset is the body action for a restart (reboot).
	ServiceControlReset = "reset"
)

// Observations is the document GET /v1/observations returns: everything the
// agent's four sensors have seen (design §8.2), plus its own health.
//
// A device appears here once any sensor has seen it. A device the agent has
// never seen is simply absent, which is itself an observation (Unseen).
type Observations struct {
	// GeneratedAt is when the agent assembled this document, by its clock.
	// Informational: the controller judges staleness by its own clock, from
	// when its poll succeeded (drivers.Observations.PolledAt), never from
	// this, so a skewed node clock cannot make observations look fresh.
	GeneratedAt time.Time `json:"generatedAt"`

	// Agent is the agent's and Asterisk's health.
	Agent AgentStatus `json:"agent"`

	// Devices holds per-device observations for every device in the applied
	// bundle that any sensor has seen. Order is not significant.
	Devices []DeviceObservations `json:"devices"`

	// UnknownDevices are MACs that requested SEP<MAC>.cnf.xml without a
	// Phone resource (design §5, Discovery).
	UnknownDevices []UnknownDevice `json:"unknownDevices"`

	// Firmware is the firmware store sensor: what is staged on the firmware
	// volume.
	Firmware FirmwareStore `json:"firmware"`
}

// AgentStatus is the agent's own health and the result of its last apply.
type AgentStatus struct {
	// Version is the agent binary's version.
	Version string `json:"version,omitempty"`

	// StartedAt is when this agent process started. A change means the
	// agent restarted and its ring buffers are empty.
	StartedAt time.Time `json:"startedAt"`

	// AppliedVersion is the bundle version that is live (served over
	// TFTP/HTTP and loaded by Asterisk). Empty before the first apply.
	AppliedVersion string `json:"appliedVersion,omitempty"`

	// LastApplyVersion is the bundle version the most recent apply
	// attempted, whether it succeeded or not. With LastApplyError it tells
	// the controller which version was refused.
	LastApplyVersion string `json:"lastApplyVersion,omitempty"`

	// LastApplyError is why the most recent apply (of LastApplyVersion) was
	// refused or rolled back. Empty when it applied cleanly.
	LastApplyError string `json:"lastApplyError,omitempty"`

	// LastApplyAt is when the agent last attempted an apply.
	LastApplyAt *time.Time `json:"lastApplyAt,omitempty"`

	// AsteriskUp is true while the agent's AMI session to Asterisk is
	// established.
	AsteriskUp bool `json:"asteriskUp"`

	// AsteriskVersion is what Asterisk reports, e.g. "22.6.0".
	AsteriskVersion string `json:"asteriskVersion,omitempty"`
}

// DeviceObservations is everything the sensors have seen of one device.
type DeviceObservations struct {
	// Device is the device's ID in the applied bundle: the name of its PJSIP
	// endpoint, which the compiler assigns. Actions address devices by it.
	Device string `json:"device"`

	// MAC is the hardware address, upper case, for phones. Empty for
	// softphones.
	MAC string `json:"mac,omitempty"`

	// Fetches is a ring buffer of this device's file fetches, oldest first
	// (sensor 1, the fetch log).
	Fetches []Fetch `json:"fetches,omitempty"`

	// Registration is the device's SIP registration from AMI (sensor 2).
	// Nil when it has no contact.
	Registration *Registration `json:"registration,omitempty"`

	// Web is the latest scrape of the phone's own web pages (sensor 3).
	// Nil for softphones and for phones not scraped yet.
	Web *WebScrape `json:"web,omitempty"`

	// Calls is how many channels are up on the device's endpoint now (AMI).
	// A phone in a call is never resynced: a resync can drop the call.
	Calls int `json:"calls,omitempty"`

	// StreamCalls is how many of Calls are calls into a Stream (radio or TV
	// from the stream bridge), not to a person. Calls - StreamCalls are the
	// person calls, which hold messages back; a Stream call does not. The
	// agent classifies by channel shape and learns no stream names.
	StreamCalls int `json:"streamCalls,omitempty"`

	// Page is the version of the message page the phone last acknowledged (a
	// CiscoIPPhoneResponse with Status 0) for the device, "" for none. A page
	// merely stored, or refused, does not count. The agent
	// keeps pages in memory, so a restart empties it and the controller
	// sees that here.
	Page string `json:"page,omitempty"`
}

// Transport is how a file was fetched.
type Transport string

// Transports the agent serves files on (design §5).
const (
	// TransportTFTP is TFTP on udp 69.
	TransportTFTP Transport = "tftp"
	// TransportHTTP6970 is HTTP on tcp 6970, which 9.x Cisco firmware may
	// try before TFTP. Same files as TFTP.
	TransportHTTP6970 Transport = "http6970"
	// TransportHTTPDevice is the device HTTP server on tcp 8088 (directory,
	// softphone provisioning, XML apps).
	TransportHTTPDevice Transport = "http8088"
)

// FetchResult is the outcome of one fetch.
type FetchResult string

// Fetch outcomes.
const (
	// FetchServed means the whole file was sent.
	FetchServed FetchResult = "served"
	// FetchNotFound means the file does not exist in the live directory
	// (for example SEP<MAC>.cnf.xml.sgn, or firmware not staged).
	FetchNotFound FetchResult = "notFound"
	// FetchError means the transfer started and failed.
	FetchError FetchResult = "error"
)

// Fetch is one entry of the fetch log.
type Fetch struct {
	// File is the requested path, as the device asked for it, except for a
	// softphone's provisioning document: that request carries the one-time
	// token, so File is the document's bundle path
	// (provisioning/<device ID>.xml) and the token is never recorded.
	File string `json:"file"`

	// Transport is how it was requested.
	Transport Transport `json:"transport"`

	// Result is what happened.
	Result FetchResult `json:"result"`

	// At is when the transfer finished (or failed).
	At time.Time `json:"at"`

	// Source is the device's IP address.
	Source string `json:"source"`

	// Version is the device's config version (its compiled index entry's
	// version, in the bundle version the file was served from) when the
	// fetch served one of the device's own rendered files (its index
	// Files). Empty otherwise. Every served provisioning document carries
	// it: without it the fetch spends the token but never becomes the
	// softphone's lastConfigFetch.
	Version string `json:"version,omitempty"`

	// Error describes a FetchError.
	Error string `json:"error,omitempty"`
}

// QualifyStatus is the result of Asterisk's OPTIONS qualify, if the device
// answers OPTIONS at all (a bench question for the 79x5, design §8.6).
type QualifyStatus string

// Qualify outcomes.
const (
	// QualifyUnknown means qualify is off or has no result yet.
	QualifyUnknown QualifyStatus = "unknown"
	// QualifyReachable means the device answered the last OPTIONS.
	QualifyReachable QualifyStatus = "reachable"
	// QualifyUnreachable means it did not.
	QualifyUnreachable QualifyStatus = "unreachable"
)

// Registration is a device's SIP registration as AMI reports it.
type Registration struct {
	// Contact is the registered contact URI.
	Contact string `json:"contact"`

	// Address is the IP address the REGISTER came from.
	Address string `json:"address"`

	// UserAgent is the REGISTER's User-Agent, e.g. "Cisco-CP7945G/9.4.2",
	// which identifies SIP firmware.
	UserAgent string `json:"userAgent,omitempty"`

	// CallID is the REGISTER's Call-ID, which a Cisco service-control NOTIFY
	// must quote (RegisterCallId).
	CallID string `json:"callID,omitempty"`

	// ExpiresAt is when the registration lapses unless refreshed.
	ExpiresAt time.Time `json:"expiresAt"`

	// Qualify is the OPTIONS qualify result.
	Qualify QualifyStatus `json:"qualify,omitempty"`
}

// WebScrape is one scrape of a phone's own web pages, fetched by the agent
// from the IP in the fetch log, so it works before the phone registers.
//
// /DeviceInformationX decides a scrape: it carries the load. The scrape
// succeeds when that page was read and parsed; a failed scrape adopts
// nothing: it carries the previous successful scrape's fields (none if
// there was none) with its own At and Error. /NetworkConfigurationX and
// /DeviceLogX are best effort and never fail a scrape (a real CP-7945G on
// SIP 9.4(2)SR4-3 breaks off the first mid-page and serves the second
// empty): in a successful scrape, NetworkConfiguration is nil when its
// page could not be read and holds only the fields completed before the
// break when the page broke off, and DeviceLog is nil when /DeviceLogX is
// empty or could not be read. The cisco79x5 driver reads a scrape only when
// Error is empty and At is not before the phone's last config fetch or
// firmware download (a "fresh scrape").
//
// The pages are whatever answers at the phone's address, so every field
// and log line is valid UTF-8 with each control character a space, a field
// at most 128 bytes and a log line at most 256 (scrape.MaxField,
// scrape.MaxLogLine).
type WebScrape struct {
	// At is when the scrape finished: when its last page was read or given
	// up on, or when it failed.
	At time.Time `json:"at"`

	// Address is the IP address that was scraped.
	Address string `json:"address"`

	// Error is why the scrape failed; empty when it succeeded. When set,
	// the fields below are from the last successful scrape, if any.
	Error string `json:"error,omitempty"`

	// DeviceInformation is /DeviceInformationX.
	DeviceInformation *DeviceInformation `json:"deviceInformation,omitempty"`

	// NetworkConfiguration is /NetworkConfigurationX.
	NetworkConfiguration *NetworkConfiguration `json:"networkConfiguration,omitempty"`

	// DeviceLog is the status messages from /DeviceLogX, oldest first. Trust
	// and config errors surface here.
	DeviceLog []string `json:"deviceLog,omitempty"`
}

// DeviceInformation is the subset of /DeviceInformationX the state machine
// reads. The element names are the XML twin's (appLoadID, versionID, ...).
// The agent reports both load fields as the phone gives them; the driver
// normalises them and accepts either (docs/plans/release-a.md §8: a real
// CP-7945G on SIP 9.4(2)SR4-3 reports versionID *SIP45.9-4-2SR4-3S* and
// appLoadID jar45sip.9-4-2SR4-3.sbn).
type DeviceInformation struct {
	// VersionID is the running load's ID, the name the SEP file's
	// loadInformation and the firmware manifest use, e.g. SIP45.9-4-2SR4-3S,
	// or SCCP45.9-4-2SR4-3S on SCCP firmware (the "Version" line of the
	// phone's Device Information page). A real 7945 wraps it in asterisks,
	// *SIP45.9-4-2SR4-3S*, and it is reported so.
	VersionID string `json:"versionID,omitempty"`
	// AppLoadID is the running application image, a file of the load, e.g.
	// jar45sip.9-4-2SR4-3.sbn, or jar45sccp.9-4-2SR4-3.sbn on SCCP firmware.
	// It is not the load ID: never compare it with a load name.
	AppLoadID string `json:"appLoadID,omitempty"`
	// BootLoadID is the boot load.
	BootLoadID string `json:"bootLoadID,omitempty"`
	// HardwareRevision is the hardware revision.
	HardwareRevision string `json:"hardwareRevision,omitempty"`
	// SerialNumber is the serial number.
	SerialNumber string `json:"serialNumber,omitempty"`
	// ModelNumber is the model, e.g. CP-7945G.
	ModelNumber string `json:"modelNumber,omitempty"`
}

// NetworkConfiguration is the subset of /NetworkConfigurationX the state
// machine reads.
type NetworkConfiguration struct {
	// AdminVLAN is the phone's Admin. VLAN ID. It must be empty on an
	// untagged voice access port (design §2).
	AdminVLAN string `json:"adminVLAN,omitempty"`
	// CallManager1 is the "Unified CM 1" line as the phone shows it, e.g.
	// "192.168.0.240  Active".
	CallManager1 string `json:"callManager1,omitempty"`
}

// UnknownDevice is a MAC that asked for config patchbay does not have.
type UnknownDevice struct {
	// MAC is the hardware address from the requested file name, upper case.
	MAC string `json:"mac"`
	// File is what it asked for, e.g. SEPD0C78915EBB5.cnf.xml.
	File string `json:"file"`
	// Source is its IP address.
	Source string `json:"source"`
	// FirstSeen is the first such request since the agent started.
	FirstSeen time.Time `json:"firstSeen"`
	// LastSeen is the latest such request.
	LastSeen time.Time `json:"lastSeen"`
}

// FirmwareStore is the firmware store sensor: which files of each load in
// each driver's manifest are on the firmware volume with the pinned SHA-256.
// The store keeps files per load, because loads can share a file name with
// different contents (see drivers.FirmwareLoad).
type FirmwareStore struct {
	// Files lists every manifest file the agent knows about, one entry per
	// (load, name).
	Files []FirmwareFileStatus `json:"files"`
}

// FirmwareFileStatus is one manifest file's presence on the firmware volume.
type FirmwareFileStatus struct {
	// Load is the load the file belongs to, e.g. SIP45.9-4-2SR4-3S.
	Load string `json:"load"`
	// Name is the file name as phones request it.
	Name string `json:"name"`
	// Present is true when the file exists and its SHA-256 matches the
	// manifest. A file with the wrong digest is reported as not present.
	Present bool `json:"present"`
}

// ActionRequest is the body of POST /v1/actions/resync and
// /v1/actions/restart.
type ActionRequest struct {
	// Device is the target's ID (DeviceObservations.Device).
	Device string `json:"device"`

	// Version is the config version the controller wants the device to
	// reach. The agent logs it; it does not check it.
	Version string `json:"version,omitempty"`
}

// ActionResponse is the body the agent answers an action with: always with
// 200 once the request decoded and named a device (an unknown device, no
// contact, no Call-ID or AMI down is Sent false with the reason); a body
// that does not decode, has unknown fields or names no device gets 400 and
// an Error.
type ActionResponse struct {
	// Sent is true when the NOTIFY went out. It says nothing about whether
	// the phone acted on it: that shows up later in the fetch log.
	Sent bool `json:"sent"`

	// Error is why it was not sent (no contact, AMI down, ...).
	Error string `json:"error,omitempty"`
}

// Push limits.
const (
	// MaxPushBody is the largest push request body the agent reads.
	MaxPushBody = 16 << 10
	// MaxPushPage is the largest PushRequest.Page.
	MaxPushPage = 8 << 10
	// MaxPushExecute is how many items one Execute holds (the phone's limit).
	MaxPushExecute = 3
	// MaxPhoneAnswer bounds PushResponse.Phone.
	MaxPhoneAnswer = 200
)

// PushRequest is the body of POST /v1/actions/push. With Page, the agent
// stores the page (served at the device HTTP message path), then pushes an
// Execute that points the phone at it, after a Play: item when Beep is set.
// With Clear it drops the stored page and pushes the clearing URI. With
// Execute it pushes exactly those URIs and nothing else (the bench's
// tool); Execute cannot be combined with Page, Beep or Clear.
type PushRequest struct {
	// Device is the target's ID (DeviceObservations.Device).
	Device string `json:"device"`

	// PageVersion names the page's content: it goes in the URL's query so
	// a phone does not show a cached older page, and in the observations.
	PageVersion string `json:"pageVersion,omitempty"`

	// Page is the CiscoIPPhone XML document, at most MaxPushPage bytes.
	Page string `json:"page,omitempty"`

	// Beep adds the chime ahead of the page.
	Beep bool `json:"beep,omitempty"`

	// Clear returns the phone to its idle display and drops the page.
	Clear bool `json:"clear,omitempty"`

	// Execute is raw URIs to push: at most MaxPushExecute, each starting
	// Init:, Key:, SoftKey: or Play:.
	Execute []string `json:"execute,omitempty"`
}

// PushResponse is the body the agent answers a push with: always 200 once
// the request decoded and named a device (a refusal or a phone's error is
// Sent false with the reason); undecodable bodies get 400 and an Error.
type PushResponse struct {
	// Sent is true when the phone accepted the Execute (status 0).
	Sent bool `json:"sent"`

	// Error is why it was not sent or not accepted.
	Error string `json:"error,omitempty"`

	// Phone is the phone's answer, at most MaxPhoneAnswer bytes: "status N"
	// for a CiscoIPPhoneResponse, "error N" for a CiscoIPPhoneError.
	Phone string `json:"phone,omitempty"`
}

// FirmwareUploadResponse is the body the agent answers a firmware upload
// with (see PathFirmware for the status codes).
type FirmwareUploadResponse struct {
	// Load and Name are the manifest load and file the upload was for.
	Load string `json:"load"`
	Name string `json:"name"`

	// SHA256 is the lower-case hex digest of the bytes received.
	SHA256 string `json:"sha256,omitempty"`

	// Size is how many bytes were received.
	Size int64 `json:"size"`

	// Stored is true when the file is on the firmware volume with the
	// manifest's digest after this request.
	Stored bool `json:"stored"`

	// Error says why it was not stored.
	Error string `json:"error,omitempty"`
}

// Error is the body of any non-2xx answer from the control API, except a
// firmware upload that got past authentication: that is always answered
// with a FirmwareUploadResponse.
type Error struct {
	// Error is a human-readable message.
	Error string `json:"error"`
}

// Empty returns an Observations document with nothing observed yet: every
// list present and empty (JSON [] rather than null), so a consumer never has
// to tell "none" from "missing".
func Empty(now time.Time, agent AgentStatus) Observations {
	return Observations{
		GeneratedAt:    now,
		Agent:          agent,
		Devices:        []DeviceObservations{},
		UnknownDevices: []UnknownDevice{},
		Firmware:       FirmwareStore{Files: []FirmwareFileStatus{}},
	}
}
