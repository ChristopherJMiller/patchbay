// Package ami is the agent's Asterisk Manager Interface client (W2.5): one
// session to Asterisk on 127.0.0.1 (compile.AMIPort), as the bundle's AMI
// account (compile.AMIUsername, the AMI password from the projected
// Secret).
//
// This file is the contract that apply, the actions and the observation
// store build on, so each can be tested against a fake. The client itself
// (Dial, reconnecting with backoff, the action encodings) lives beside it.
//
// The rules every implementation keeps:
//   - one session; while none is up, every call fails at once with
//     ErrNotConnected (no queueing), and State says why;
//   - AMI is a line protocol: no value the client sends (action fields,
//     variables, header names, values, body lines) may hold CR or LF, or
//     the call fails with ErrUnsafeValue and nothing is sent;
//   - Asterisk splits a "Variable:" field at commas and strips quotes and
//     backslashes from it (manager.c, AST_STANDARD_APP_ARGS), so Notify
//     also refuses with ErrUnsafeValue a header name holding "=" or ":",
//     and any header name, header value or body line holding ",", `"` or
//     `\`;
//   - a call honours its context's deadline, and a reply that does not
//     come is an error, never a hang;
//   - the account's password is never logged, never part of an error, and
//     never sent: the login is AMI's MD5 challenge-response, so whatever
//     answers on the port (a host-network process that bound it while
//     Asterisk was down) learns only the digest of its own challenge.
package ami

import (
	"context"
	"errors"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// Errors every Client returns.
var (
	// ErrNotConnected is returned while no session is up.
	ErrNotConnected = errors.New("ami: not connected to Asterisk")
	// ErrUnsafeValue is returned for a value AMI would not carry as given
	// (see the package rules).
	ErrUnsafeValue = errors.New("ami: a value AMI cannot carry as given")
)

// Client is what the agent needs from AMI. The actions it uses, and the
// class each needs, are compile.AMIClasses' list: Reload,
// PJSIPShowEndpoints, PJSIPShowContacts, PJSIPNotify, CoreShowChannels,
// ShowDialPlan (and CoreSettings for the version).
type Client interface {
	// SetAccount sets the account to log in as. Until it is called the
	// client does not connect. When the account or password changes (a new
	// bundle's AMI account), the client logs in again with it; apply first
	// reloads "manager" over the old session, so Asterisk knows the new
	// password before the client uses it.
	SetAccount(username, password string)

	// State is the session's health, for AgentStatus (AsteriskUp,
	// AsteriskVersion) and the startup order. It never blocks.
	State() State

	// Reload reloads one module with the Reload action, e.g. "res_pjsip.so",
	// "pbx_config.so" (the dialplan), "manager". (Not ModuleLoad with
	// LoadType reload: on Asterisk 22.11 that answers "Module not found."
	// for the built-ins manager, logger and indications.) It fails if
	// Asterisk answers with an error.
	Reload(ctx context.Context, module string) error

	// Endpoints lists every PJSIP endpoint's name (PJSIPShowEndpoints'
	// EndpointList ObjectName), sorted.
	Endpoints(ctx context.Context) ([]string, error)

	// DialplanContexts lists every dialplan context's name (ShowDialPlan's
	// ListDialplan Context), sorted and unique.
	DialplanContexts(ctx context.Context) ([]string, error)

	// Contacts lists every registered contact (PJSIPShowContacts'
	// ContactList).
	Contacts(ctx context.Context) ([]Contact, error)

	// Channels lists every channel up now (CoreShowChannels'
	// CoreShowChannel).
	Channels(ctx context.Context) ([]Channel, error)

	// Notify sends one out-of-dialog SIP NOTIFY to an endpoint's contacts
	// (PJSIPNotify with Endpoint and one Variable per header and per body
	// line, in order; a body line is sent as Variable "Content=<line>").
	// It fails if Asterisk refuses the action.
	Notify(ctx context.Context, n Notify) error
}

// State is the session's health.
type State struct {
	// Up is true while an authenticated session is established.
	Up bool
	// Version is Asterisk's version once known, e.g. "22.11.0".
	Version string
	// Since is when Up last changed.
	Since time.Time
	// Error is why the session is down (the last dial or login error), or
	// empty while it is up. Never holds the password.
	Error string
}

// Contact is one registered contact, as PJSIPShowContacts reports it.
type Contact struct {
	// Endpoint is the endpoint that registered it (the device ID), when
	// Asterisk reports one.
	Endpoint string
	// AOR is the contact's AOR (the device's number).
	AOR string
	// URI is the contact URI.
	URI string
	// Address is the IP address the REGISTER came from (ViaAddr, else the
	// URI's host), canonical.
	Address string
	// UserAgent is the REGISTER's User-Agent.
	UserAgent string
	// CallID is the REGISTER's Call-ID (CallId), which a Cisco
	// service-control NOTIFY quotes. Empty when Asterisk does not report it:
	// then no NOTIFY can be sent to this contact.
	CallID string
	// ExpiresAt is when the registration lapses (ExpirationTime).
	ExpiresAt time.Time
	// Qualify is the qualify status (Status: Reachable, Unreachable; any
	// other value, e.g. NonQualified or Unknown, is QualifyUnknown).
	Qualify api.QualifyStatus
}

// Channel is one channel up now.
type Channel struct {
	// Name is the channel's name, e.g. PJSIP/phone-desk-00000001.
	Name string
	// Endpoint is the PJSIP endpoint it belongs to (the name without
	// "PJSIP/" and its "-<sequence>" suffix), or "" for any other channel
	// technology.
	Endpoint string
	// Application is the dialplan application the channel is running now
	// (CoreShowChannel's Application), e.g. Dial or AppDial, or "" while it
	// rings.
	Application string
	// ApplicationData is that application's argument, e.g.
	// AudioSocket/patchbay-streams.telephony.svc:9092/<uuid>/c(slin16),,gS(10)
	// on the phone's leg of a call into a Stream.
	ApplicationData string
	// BridgeID is the bridge the channel is in (BridgeId), "" for none. Two
	// channels with the same BridgeID are talking to each other.
	BridgeID string
}

// Notify is one SIP NOTIFY.
type Notify struct {
	// Endpoint is the PJSIP endpoint (the device ID).
	Endpoint string
	// Headers are sent in order, e.g. Event: service-control.
	Headers []Header
	// Body lines are sent in order, each as one line of the body.
	Body []string
}

// Header is one SIP header.
type Header struct {
	Name  string
	Value string
}
