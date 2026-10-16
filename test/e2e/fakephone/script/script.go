// Package script is the contract between the e2e harness and `fakephone
// run` (docs/plans/release-a.md §6.2, W3.10): the Script a simulated phone
// lives through, step by step, and the State it reports while it does.
//
// The harness builds a Script in Go, puts its JSON in the pod's
// environment (EnvScript), and drives and observes the running phone over
// the phone's own HTTP server (WebListen): POST SignalPath(name) releases a
// WaitSignal step, GET PathState returns the State. It reaches that server
// through the API server's pods/proxy, so it needs nothing in the image.
//
// What a running phone does by itself, between and during steps:
//   - cisco mode: on a service-control NOTIFY it answers 200 and, for
//     action=restart or action=reset, fetches its config again (as
//     FetchConfig); if the credentials, server or expiry it then holds
//     differ from its registration's while it is refreshing, it REGISTERs
//     again with the new ones at once;
//   - both modes: while refreshing, it re-REGISTERs at half the granted
//     expiry; it answers OPTIONS with 200; it answers an incoming INVITE as
//     Answer says (no Answer: 486 Busy Here), sending ToneHz and counting
//     the received packets that carry each Answer.DetectHz; it ends a call
//     on BYE;
//   - cisco mode: it serves the three pages the agent scrapes
//     (scrape.PathDeviceInformation and the others, built with
//     internal/agent/scrape's page types and MarshalPage) from the current
//     Web, or 503 while Web.Down or before any Web was set; with
//     Web.NetworkConfigurationBreaksOff and Web.EmptyDeviceLog, as a real
//     CP-7945G on SIP 9.4(2)SR4-3 serves them.
//
// A step that fails ends the run: the phone logs why and exits 1 (the pod
// fails and its log says which step). When the steps are done it keeps
// running (registered, answering) until it is stopped, unless
// ExitWhenDone. It never logs, and State never holds, a password or a
// provisioning token: passwords appear only as SHA-256 digests, and the
// provisioning URL only as "provisioning".
package script

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// EnvScript is the environment variable `fakephone run` reads the Script's
// JSON from (unless --script names a file).
const EnvScript = "FAKEPHONE_SCRIPT"

// The phone's own HTTP paths, besides the scraped pages.
const (
	// PathState is GET: the State as JSON.
	PathState = "/fakephone/state"
	// PathSignalPrefix + name is POST: releases WaitSignal{Name: name}. A
	// signal sent before its step runs is remembered.
	PathSignalPrefix = "/fakephone/signal/"
)

// SignalPath is the path that sends signal name.
func SignalPath(name string) string { return PathSignalPrefix + name }

// Defaults for Script's zero values.
const (
	// DefaultWebListen is port 80, where the agent scrapes phones: the pod
	// needs net.ipv4.ip_unprivileged_port_start=0 (a safe sysctl) or
	// NET_BIND_SERVICE to bind it as non-root.
	DefaultWebListen = ":80"
	// DefaultToneHz is the tone a phone sends in a call.
	DefaultToneHz = 1000
	// DefaultAnswerAfter is how long an incoming call rings before 200 OK.
	DefaultAnswerAfter = time.Second
	// TFTPPort is where cisco mode fetches its files (Server:69).
	TFTPPort = 69
)

// Mode is what the phone is.
type Mode string

// Modes.
const (
	// ModeCisco is a 79x5: it fetches SEP<MAC>.cnf.xml over TFTP, registers
	// with the credentials, server, port, transport (TCP) and
	// timerRegisterExpires in it, and serves fake web pages.
	ModeCisco Mode = "cisco"
	// ModeLinphone is Linphone remote provisioning: it GETs the
	// provisioning URL once per FetchConfig, reads the lpconfig XML
	// (proxy_0, auth_info_0, misc/config-uri) and registers over UDP.
	ModeLinphone Mode = "linphone"
)

// Script is one simulated phone's life.
type Script struct {
	Mode Mode `json:"mode"`

	// Server is the PBX's IP address: cisco mode fetches from Server:69;
	// SIP goes wherever the fetched config says.
	Server string `json:"server"`

	// MAC is the phone's MAC (cisco mode, required): 12 upper-case hex
	// digits, naming SEP<MAC>.cnf.xml.
	MAC string `json:"mac,omitempty"`

	// ProvisioningURL is what the QR code holds (linphone mode, required).
	// It carries a one-time token: never log it.
	ProvisioningURL string `json:"provisioningURL,omitempty"`

	// WebListen is the phone's HTTP server address; "" is
	// DefaultWebListen.
	WebListen string `json:"webListen,omitempty"`

	// Web is the scraped pages at start (cisco mode). Nil: they answer 503
	// until a SetWeb step.
	Web *Web `json:"web,omitempty"`

	// Answer is how incoming calls are answered; nil rejects them (486).
	Answer *Answer `json:"answer,omitempty"`

	// ToneHz is the tone this phone sends in every call; 0 is
	// DefaultToneHz.
	ToneHz float64 `json:"toneHz,omitempty"`

	// UserAgent is the SIP User-Agent; "" is "Cisco-CP7945G/9.4.2
	// (patchbay fakephone)" in cisco mode and "Linphone/5.2 (patchbay
	// fakephone)" in linphone mode.
	UserAgent string `json:"userAgent,omitempty"`

	// Steps run in order.
	Steps []Step `json:"steps"`

	// ExitWhenDone exits 0 after the last step instead of running on.
	ExitWhenDone bool `json:"exitWhenDone,omitempty"`
}

// Step is one thing the phone does. Exactly one action field is set.
type Step struct {
	// Label names the step in the log and in State.
	Label string `json:"label,omitempty"`

	FetchConfig    *FetchConfig    `json:"fetchConfig,omitempty"`
	Fetch          *Fetch          `json:"fetch,omitempty"`
	SetWeb         *Web            `json:"setWeb,omitempty"`
	Register       *Register       `json:"register,omitempty"`
	Unregister     *Unregister     `json:"unregister,omitempty"`
	StopRefreshing *StopRefreshing `json:"stopRefreshing,omitempty"`
	Call           *Call           `json:"call,omitempty"`
	Hangup         *Hangup         `json:"hangup,omitempty"`
	Transfer       *Transfer       `json:"transfer,omitempty"`
	WaitNotify     *WaitNotify     `json:"waitNotify,omitempty"`
	WaitCall       *WaitCall       `json:"waitCall,omitempty"`
	WaitSignal     *WaitSignal     `json:"waitSignal,omitempty"`
	Sleep          *Sleep          `json:"sleep,omitempty"`
}

// Action names the step's action ("fetchConfig", "fetch", ...), or "" when
// none or several are set.
func (s Step) Action() string {
	var names []string
	for name, set := range map[string]bool{
		"fetchConfig": s.FetchConfig != nil, "fetch": s.Fetch != nil, "setWeb": s.SetWeb != nil,
		"register": s.Register != nil, "unregister": s.Unregister != nil, "stopRefreshing": s.StopRefreshing != nil,
		"call": s.Call != nil, "hangup": s.Hangup != nil, "transfer": s.Transfer != nil,
		"waitNotify": s.WaitNotify != nil, "waitCall": s.WaitCall != nil, "waitSignal": s.WaitSignal != nil,
		"sleep": s.Sleep != nil,
	} {
		if set {
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

// Result is the outcome a fetch expects or had.
type Result string

// Results.
const (
	// Served: the whole file came (TFTP), or 200 (HTTP).
	Served Result = "served"
	// NotFound: TFTP "file not found", or HTTP 404.
	NotFound Result = "notFound"
	// Any accepts whatever happens (a fetch whose only purpose is to be
	// seen in the fetch log).
	Any Result = "any"
	// Failed is only ever recorded: the transfer failed some other way.
	Failed Result = "error"
)

// Retry repeats a fetch: every Every until it gives the expected result,
// for at most Within. A zero Retry makes one attempt.
type Retry struct {
	Every  Duration `json:"every,omitempty"`
	Within Duration `json:"within,omitempty"`
}

// FetchConfig fetches the phone's config document and, when it is served,
// parses it into State.Config: cisco mode TFTPs SEP<MAC>.cnf.xml, linphone
// mode GETs ProvisioningURL (no retries on a 404 unless Retry says so).
type FetchConfig struct {
	// Expect is Served (the default), NotFound or Any.
	Expect Result `json:"expect,omitempty"`
	Retry
}

// Fetch TFTPs one file (cisco mode), the way a phone asks for its trust
// list, a signed config or its firmware.
type Fetch struct {
	// File is the file name; "{mac}" stands for the MAC and "{load}" for
	// the loadInformation of the config last parsed (Expand), e.g.
	// "SEP{mac}.cnf.xml.sgn", "{load}.loads".
	File string `json:"file"`
	// Expect is Served (the default), NotFound or Any.
	Expect Result `json:"expect,omitempty"`
	Retry
}

// Expand replaces "{mac}" and "{load}" in a Fetch.File.
func Expand(file, mac, load string) string {
	return strings.NewReplacer("{mac}", mac, "{load}", load).Replace(file)
}

// Web is the phone's scraped pages (cisco mode). The fields are
// /DeviceInformationX's, /NetworkConfigurationX's and /DeviceLogX's.
type Web struct {
	// Down makes every page answer 503: the phone is rebooting, or its web
	// server is wedged (design §2).
	Down bool `json:"down,omitempty"`

	VersionID        string   `json:"versionID,omitempty"`
	AppLoadID        string   `json:"appLoadID,omitempty"`
	BootLoadID       string   `json:"bootLoadID,omitempty"`
	HardwareRevision string   `json:"hardwareRevision,omitempty"`
	SerialNumber     string   `json:"serialNumber,omitempty"`
	ModelNumber      string   `json:"modelNumber,omitempty"`
	AdminVLAN        string   `json:"adminVLAN,omitempty"`
	CallManager1     string   `json:"callManager1,omitempty"`
	Log              []string `json:"log,omitempty"`

	// NetworkConfigurationBreaksOff ends /NetworkConfigurationX as a real
	// CP-7945G on SIP 9.4(2)SR4-3 does (observed 2026-10-15): after its
	// fields, at <EnergywisePowerLevel>java.lang.NullPointerException, the
	// renderer's crash, with no closing tag.
	NetworkConfigurationBreaksOff bool `json:"networkConfigurationBreaksOff,omitempty"`
	// EmptyDeviceLog makes /DeviceLogX a 200 with an empty body, as that
	// phone's is. Log must then be empty.
	EmptyDeviceLog bool `json:"emptyDeviceLog,omitempty"`
}

func (w Web) validate() error {
	if w.EmptyDeviceLog && len(w.Log) > 0 {
		return errors.New("web: an empty device log has no log lines")
	}
	return nil
}

// Register REGISTERs with the credentials of the config last parsed and,
// unless NoRefresh, keeps the registration fresh.
type Register struct {
	// Expires is the expiry to ask for; 0 is the config's
	// (timerRegisterExpires, reg_expires).
	Expires int `json:"expires,omitempty"`
	// Expect is the final SIP status the step requires; 0 is 200. A step
	// expecting anything else never refreshes (a wrong-password check).
	Expect int `json:"expect,omitempty"`
	// NoRefresh registers once.
	NoRefresh bool `json:"noRefresh,omitempty"`
	// Password, when set, replaces the config's password for this
	// REGISTER only (a check that an old password is refused). It is test
	// data, never a real secret.
	Password string `json:"password,omitempty"`
}

// Unregister sends REGISTER with Expires: 0 and stops refreshing.
type Unregister struct{}

// StopRefreshing stops refreshing without unregistering: the contact
// lapses at its expiry (a phone gone silent).
type StopRefreshing struct{}

// Call places a call to Number and sends ToneHz once it is answered.
type Call struct {
	Number string `json:"number"`
	// Hold is how long the call stays up before the phone hangs up; with
	// Open the step ends once the call is answered and it stays up until a
	// Hangup step or the far end's BYE.
	Hold Duration `json:"hold,omitempty"`
	Open bool     `json:"open,omitempty"`
	// ExpectToneHz, when set, fails a held call unless at least
	// MinToneFrames received 20 ms packets carried that tone.
	ExpectToneHz  float64 `json:"expectToneHz,omitempty"`
	MinToneFrames int     `json:"minToneFrames,omitempty"`
}

// Hangup ends every call that is up (BYE).
type Hangup struct{}

// Transfer blind-transfers the call that is up (the latest answered one)
// with an in-dialog REFER to sip:<To>@<the PBX>, and succeeds on 202
// Accepted. The phone then leaves the call (BYE after the REFER's NOTIFY
// reports a final status, or at once).
type Transfer struct {
	To string `json:"to"`
}

// WaitNotify waits for a NOTIFY with Event (default "service-control")
// that arrives after the step starts, for at most Within (0: no limit). The
// refetch it triggers happens on its own; the step ends after it.
type WaitNotify struct {
	Event  string   `json:"event,omitempty"`
	Within Duration `json:"within,omitempty"`
}

// WaitCall waits for an incoming call that starts after the step does to
// be answered (and, with Ended, to end), for at most Within (0: no limit).
type WaitCall struct {
	Ended  bool     `json:"ended,omitempty"`
	Within Duration `json:"within,omitempty"`
}

// WaitSignal waits until POST SignalPath(Name) arrives (or arrived
// earlier), for at most Within (0: no limit).
type WaitSignal struct {
	Name   string   `json:"name"`
	Within Duration `json:"within,omitempty"`
}

// Sleep waits For.
type Sleep struct {
	For Duration `json:"for"`
}

// Answer is how the phone answers incoming calls.
type Answer struct {
	// After is how long a call rings (180) before 200 OK; 0 is
	// DefaultAnswerAfter.
	After Duration `json:"after,omitempty"`
	// Status, when set, answers with this final status instead (e.g. 486).
	Status int `json:"status,omitempty"`
	// Hold hangs up an answered call after this long; 0 waits for the far
	// end's BYE.
	Hold Duration `json:"hold,omitempty"`
	// DetectHz are the tones whose received packets each call record
	// counts (CallRecord.ToneFrames).
	DetectHz []float64 `json:"detectHz,omitempty"`
}

var (
	macPattern    = regexp.MustCompile(`^[0-9A-F]{12}$`)
	signalPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dialPattern   = regexp.MustCompile(`^[0-9*#]{1,32}$`)
	digitsPattern = regexp.MustCompile(`^[0-9]{1,16}$`)
)

// Validate checks a script before it runs.
func (s Script) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("script: "+format, args...)) }
	if _, err := netip.ParseAddr(s.Server); err != nil {
		bad("server %q is not an IP address", s.Server)
	}
	switch s.Mode {
	case ModeCisco:
		if !macPattern.MatchString(s.MAC) {
			bad("mac %q is not 12 upper-case hex digits", s.MAC)
		}
		if s.ProvisioningURL != "" {
			bad("a cisco phone has no provisioning URL")
		}
		if s.Web != nil {
			if err := s.Web.validate(); err != nil {
				errs = append(errs, fmt.Errorf("script: %w", err))
			}
		}
	case ModeLinphone:
		u, err := url.Parse(s.ProvisioningURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("the provisioning URL is not an http URL") // never quote it: it holds a token
		}
		if s.MAC != "" || s.Web != nil {
			bad("a linphone phone has no MAC and no web pages")
		}
	default:
		bad("mode %q is not %q or %q", s.Mode, ModeCisco, ModeLinphone)
	}
	if s.ToneHz != 0 && (s.ToneHz < 100 || s.ToneHz > 3500) {
		bad("toneHz %g is outside 100-3500", s.ToneHz)
	}
	if a := s.Answer; a != nil {
		if a.After < 0 || a.Hold < 0 {
			bad("answer: negative duration")
		}
		if a.Status != 0 && (a.Status < 300 || a.Status > 699) {
			bad("answer: status %d is not a final failure status", a.Status)
		}
		for _, hz := range a.DetectHz {
			if hz < 100 || hz > 3500 {
				bad("answer: detectHz %g is outside 100-3500", hz)
			}
		}
	}
	for i, st := range s.Steps {
		if err := st.validate(s.Mode); err != nil {
			errs = append(errs, fmt.Errorf("script: step %d (%s): %w", i, st.Label, err))
		}
	}
	return errors.Join(errs...)
}

func (st Step) validate(mode Mode) error {
	action := st.Action()
	if action == "" {
		return errors.New("exactly one action must be set")
	}
	if mode == ModeLinphone && (st.Fetch != nil || st.SetWeb != nil) {
		return fmt.Errorf("%s is for cisco phones", action)
	}
	retry := func(r Retry) error {
		if r.Every < 0 || r.Within < 0 || (r.Within > 0 && r.Every <= 0) {
			return errors.New("a retry needs every > 0 and within >= 0")
		}
		return nil
	}
	expect := func(r Result) error {
		switch r {
		case "", Served, NotFound, Any:
			return nil
		case Failed: // only ever recorded
		}
		return fmt.Errorf("expect %q is not %q, %q or %q", r, Served, NotFound, Any)
	}
	switch {
	case st.FetchConfig != nil:
		return errors.Join(expect(st.FetchConfig.Expect), retry(st.FetchConfig.Retry))
	case st.Fetch != nil:
		f := st.Fetch
		if f.File == "" || len(f.File) > 255 || strings.ContainsAny(f.File, `/\`) {
			return fmt.Errorf("file %q is not a plain file name", f.File)
		}
		return errors.Join(expect(f.Expect), retry(f.Retry))
	case st.Register != nil:
		r := st.Register
		if r.Expires < 0 || (r.Expect != 0 && (r.Expect < 100 || r.Expect > 699)) {
			return fmt.Errorf("expires %d / expect %d", r.Expires, r.Expect)
		}
	case st.Call != nil:
		c := st.Call
		if !dialPattern.MatchString(c.Number) || c.Hold < 0 || c.MinToneFrames < 0 {
			return fmt.Errorf("call %q: a number of digits, * and #, and non-negative hold and frames", c.Number)
		}
		if c.Open && c.Hold != 0 {
			return errors.New("an open call has no hold")
		}
		if c.ExpectToneHz != 0 && c.Open {
			return errors.New("an open call checks no tone")
		}
	case st.Transfer != nil:
		if !digitsPattern.MatchString(st.Transfer.To) {
			return fmt.Errorf("transfer to %q: a number", st.Transfer.To)
		}
	case st.WaitNotify != nil:
		if st.WaitNotify.Within < 0 {
			return errors.New("negative within")
		}
	case st.WaitCall != nil:
		if st.WaitCall.Within < 0 {
			return errors.New("negative within")
		}
	case st.WaitSignal != nil:
		if !signalPattern.MatchString(st.WaitSignal.Name) || st.WaitSignal.Within < 0 {
			return fmt.Errorf("signal %q: a DNS label and a non-negative within", st.WaitSignal.Name)
		}
	case st.Sleep != nil:
		if st.Sleep.For <= 0 {
			return errors.New("sleep for a positive duration")
		}
	case st.SetWeb != nil:
		return st.SetWeb.validate()
	case st.Unregister != nil, st.StopRefreshing != nil, st.Hangup != nil:
	}
	return nil
}

// Parse decodes a script's JSON strictly (an unknown field is an error)
// and validates it.
func Parse(data []byte) (Script, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Script
	if err := dec.Decode(&s); err != nil {
		return Script{}, fmt.Errorf("script: %w", err)
	}
	if dec.More() {
		return Script{}, errors.New("script: trailing data after the script")
	}
	if err := s.Validate(); err != nil {
		return Script{}, err
	}
	return s, nil
}

// Encode validates the script and returns its JSON, for EnvScript.
func (s Script) Encode() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s)
	return string(b), err
}

// Duration is a time.Duration written in JSON as a Go duration string
// ("90s", "3m").
type Duration time.Duration

// D is the duration as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON writes "1m30s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// State is what GET PathState returns: where the script is and everything
// the phone has seen. Lists are oldest first; the phone starts from
// NewState, so an empty list is [] and never null.
type State struct {
	Mode Mode `json:"mode"`
	// Step is the index of the step running, len(steps) once all are done.
	Step  int    `json:"step"`
	Label string `json:"label,omitempty"`
	Done  bool   `json:"done"`

	// Registered is whether the phone holds a registration it is
	// refreshing (or that has not lapsed yet).
	Registered bool `json:"registered"`

	// Config is the config last parsed; nil before the first.
	Config *Config `json:"config,omitempty"`

	// Web is the pages being served (cisco mode).
	Web Web `json:"web"`

	Fetches       []FetchRecord        `json:"fetches"`
	Registrations []RegistrationRecord `json:"registrations"`
	Notifies      []NotifyRecord       `json:"notifies"`
	Calls         []CallRecord         `json:"calls"`
	// Signals are the signals received, in order.
	Signals []string `json:"signals"`
}

// Config is what the phone read from its config document.
type Config struct {
	// Source is "SEP<MAC>.cnf.xml", or "provisioning" (never the URL).
	Source string `json:"source"`
	// SHA256 is the hex digest of the document as fetched.
	SHA256 string `json:"sha256"`
	// Version is the SEP file's versionStamp ("" for linphone).
	Version string `json:"version,omitempty"`
	// PhoneLabel is sipProfile/phoneLabel; LineLabel the line's
	// displayName (cisco mode).
	PhoneLabel string `json:"phoneLabel,omitempty"`
	LineLabel  string `json:"lineLabel,omitempty"`
	// LoadInformation is the load the SEP file names (cisco mode).
	LoadInformation string `json:"loadInformation,omitempty"`
	// User is the SIP user (the number); AuthUser the digest username (the
	// device ID).
	User     string `json:"user"`
	AuthUser string `json:"authUser"`
	// PasswordSHA256 is the hex SHA-256 of the password, never the
	// password: the harness compares it with the Secret's value.
	PasswordSHA256 string `json:"passwordSHA256"`
	// Proxy is where SIP goes, host:port; Transport "tcp" or "udp".
	Proxy     string `json:"proxy"`
	Transport string `json:"transport"`
	// Expires is the expiry the document asks for (timerRegisterExpires,
	// reg_expires).
	Expires int `json:"expires"`
	// ConfigURICleared is linphone mode's misc/config-uri written empty
	// with overwrite (Review Focus 4).
	ConfigURICleared bool `json:"configURICleared,omitempty"`
}

// FetchRecord is one fetch the phone made.
type FetchRecord struct {
	// File is the file name, or "provisioning".
	File   string    `json:"file"`
	Result Result    `json:"result"`
	At     time.Time `json:"at"`
	Bytes  int64     `json:"bytes,omitempty"`
	SHA256 string    `json:"sha256,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// RegistrationRecord is one REGISTER transaction.
type RegistrationRecord struct {
	At time.Time `json:"at"`
	// Expires is what was asked for (0: an unregister).
	Expires int `json:"expires"`
	// Status is the final SIP status; Granted the expiry the 200 granted.
	Status  int `json:"status"`
	Granted int `json:"granted,omitempty"`
	// AuthUser and PasswordSHA256 are the credentials it used.
	AuthUser       string `json:"authUser"`
	PasswordSHA256 string `json:"passwordSHA256"`
}

// NotifyRecord is one NOTIFY the PBX sent the phone.
type NotifyRecord struct {
	At                time.Time `json:"at"`
	Event             string    `json:"event"`
	SubscriptionState []string  `json:"subscriptionState,omitempty"`
	// Body is the NOTIFY's body as text.
	Body string `json:"body"`
	// Refetched is whether the phone fetched its config because of it.
	Refetched bool `json:"refetched"`
}

// CallRecord is one call, placed or received.
type CallRecord struct {
	// Direction is "in" or "out".
	Direction string `json:"direction"`
	// Number is the number dialled (out) or the From user (in).
	Number string `json:"number"`
	CallID string `json:"callID"`
	// FromDisplay, RemotePartyID and PAssertedIdentity are the caller's
	// identity as the INVITE carried it (in): where the caller ID name
	// shows.
	FromDisplay       string `json:"fromDisplay,omitempty"`
	RemotePartyID     string `json:"remotePartyID,omitempty"`
	PAssertedIdentity string `json:"pAssertedIdentity,omitempty"`
	// Status is the final status of the INVITE (out: the PBX's answer; in:
	// the phone's).
	Status   int       `json:"status"`
	Answered bool      `json:"answered"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	// Received is how many RTP packets arrived; ToneFrames how many of them
	// carried each tone in Answer.DetectHz (in) or ExpectToneHz (out),
	// keyed by the frequency written with %g ("1000").
	Received   int            `json:"received"`
	ToneFrames map[string]int `json:"toneFrames,omitempty"`
	// TransferredTo is the number a Transfer step sent this call to, and
	// TransferStatus the REFER's final status.
	TransferredTo  string `json:"transferredTo,omitempty"`
	TransferStatus int    `json:"transferStatus,omitempty"`
}

// HzKey is the ToneFrames key of a frequency.
func HzKey(hz float64) string { return fmt.Sprintf("%g", hz) }

// NewState is the State of a phone that has done nothing yet, every list
// empty (not nil).
func NewState(m Mode) State {
	return State{
		Mode:          m,
		Fetches:       []FetchRecord{},
		Registrations: []RegistrationRecord{},
		Notifies:      []NotifyRecord{},
		Calls:         []CallRecord{},
		Signals:       []string{},
	}
}
