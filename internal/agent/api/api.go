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
	// PathHealthz answers 200 while the agent process is alive. No token.
	PathHealthz = "/healthz"
	// PathReadyz answers 200 once the agent has applied a config version.
	// It backs the native sidecar's startup probe, so Asterisk starts only
	// after valid config exists. No token.
	PathReadyz = "/readyz"
)

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
	// GeneratedAt is when the agent assembled this document. The controller
	// uses it, with its own clock, to judge staleness.
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

	// LastApplyError is why the most recent apply was refused or rolled
	// back. Empty when the latest version applied cleanly.
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
	// File is the requested path, as the device asked for it.
	File string `json:"file"`

	// Transport is how it was requested.
	Transport Transport `json:"transport"`

	// Result is what happened.
	Result FetchResult `json:"result"`

	// At is when the transfer finished (or failed).
	At time.Time `json:"at"`

	// Source is the device's IP address.
	Source string `json:"source"`

	// Version is the config version of the file that was served, when it is
	// one of the device's own rendered files. Empty otherwise.
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
type WebScrape struct {
	// At is when the scrape finished.
	At time.Time `json:"at"`

	// Address is the IP address that was scraped.
	Address string `json:"address"`

	// Error is why the scrape failed. The fields below are from the last
	// successful scrape, if any.
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
// reads.
type DeviceInformation struct {
	// AppLoadID is the running application load, e.g. SIP45.9-4-2SR4-3S, or
	// jar45sccp.9-4-2SR4-3 on SCCP firmware.
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

// FirmwareStore is the firmware store sensor: which files of each driver's
// manifest are on the firmware volume with the pinned SHA-256.
type FirmwareStore struct {
	// Files lists every manifest file the agent knows about.
	Files []FirmwareFileStatus `json:"files"`
}

// FirmwareFileStatus is one manifest file's presence on the firmware volume.
type FirmwareFileStatus struct {
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

// ActionResponse is the body the agent answers an action with.
type ActionResponse struct {
	// Sent is true when the NOTIFY went out. It says nothing about whether
	// the phone acted on it: that shows up later in the fetch log.
	Sent bool `json:"sent"`

	// Error is why it was not sent (no contact, AMI down, ...).
	Error string `json:"error,omitempty"`
}

// Error is the body of any non-2xx answer from the control API.
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
