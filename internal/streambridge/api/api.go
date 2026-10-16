// Package api is the wire contract between the controller and the stream
// bridge: the JSON the bridge's control API serves on DefaultControlPort
// (docs/plans/release-b.md §3).
//
// The bridge senses; it never decides and never writes resource status.
// The controller polls GET PathObservations every few seconds and decides
// each Stream's state from the document (Idle, Playing, Busy, Failing; the
// controller never probes a source, because a probe would hold a tuner).
// The bridge's memory is disposable: whatever must survive a bridge restart
// (a stream's last failure, when it last played) is copied into
// Stream.status by the controller.
//
// The control API is read only and needs no token: the document carries
// stream names, numbers, sources (which hold no credentials,
// streams.CheckSource), listener counts and bounded error text, and no
// secret. The AudioSocket listener (DefaultAudioSocketPort) is the only
// other thing the bridge serves.
//
// The package holds types only, so both sides (and the e2e suite) import it
// without either implementation.
package api

import "time"

// Ports and paths.
const (
	// DefaultAudioSocketPort is where the bridge takes AudioSocket
	// connections from Asterisk (design §9: 9092).
	DefaultAudioSocketPort = 9092
	// DefaultControlPort is the control API's port.
	DefaultControlPort = 9093

	// PathObservations is GET: the Observations document.
	PathObservations = "/v1/observations"
	// PathHealthz answers 200 while the process is alive.
	PathHealthz = "/healthz"
	// PathReadyz answers 200 once the bridge has loaded a streams.json and
	// found its decoder (ffmpeg -version ran); 503 with the reason before.
	PathReadyz = "/readyz"
	// PathMetrics is GET: Prometheus metrics.
	PathMetrics = "/metrics"
)

// MaxMessageBytes bounds every message the bridge reports (a failure's
// text, StreamsError): longer text is cut and ends in "…", and control
// characters are written as spaces.
const MaxMessageBytes = 512

// Observations is GET PathObservations' document.
type Observations struct {
	// GeneratedAt is when the bridge built the document, by its clock.
	// Informational: the controller judges staleness by its own clock.
	GeneratedAt time.Time `json:"generatedAt"`

	// Bridge is the bridge's own health.
	Bridge BridgeStatus `json:"bridge"`

	// Streams has one entry per stream of the loaded streams.json, sorted
	// by name; empty (never null) before one is loaded.
	Streams []StreamObservations `json:"streams"`

	// Pools has one entry per pool of the loaded streams.json, sorted by
	// name; never null.
	Pools []PoolObservations `json:"pools"`
}

// BridgeStatus is the bridge process's health.
type BridgeStatus struct {
	// Version is the bridge binary's version.
	Version string `json:"version,omitempty"`
	// StartedAt is when this process started; a change means it restarted
	// and its memory (sessions, failures) is gone.
	StartedAt time.Time `json:"startedAt"`
	// FFmpeg is the first line of the decoder's `-version` output, or ""
	// before it was run.
	FFmpeg string `json:"ffmpeg,omitempty"`

	// StreamsVersion is the version (streams.Document.Version) of the
	// streams.json the bridge plays from; "" before it loaded one.
	StreamsVersion string `json:"streamsVersion,omitempty"`
	// StreamsLoadedAt is when it loaded that version.
	StreamsLoadedAt *time.Time `json:"streamsLoadedAt,omitempty"`
	// StreamsError is why the newest file could not be loaded (the
	// previous version stays in use), or "".
	StreamsError string `json:"streamsError,omitempty"`

	// Calls is how many AudioSocket connections are open now.
	Calls int `json:"calls"`
	// UnknownCalls counts calls refused since StartedAt because their UUID
	// is in no loaded streams.json (Asterisk's dialplan is ahead of the
	// bridge's file, or a hand-written dialplan).
	UnknownCalls uint64 `json:"unknownCalls"`
	// FullCalls counts calls refused since StartedAt because the bridge was
	// at its limit of calls or sessions (streambridge.MaxCalls,
	// MaxSessions).
	FullCalls uint64 `json:"fullCalls"`
}

// StreamObservations is one stream of the loaded streams.json.
type StreamObservations struct {
	// UUID, Name, Number, Source and Pool are the stream's entry in the
	// loaded streams.json.
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Number string `json:"number"`
	Source string `json:"source"`
	Pool   string `json:"pool,omitempty"`

	// Listeners is how many calls on this stream are open now.
	Listeners int `json:"listeners"`

	// Session is its source's session while one runs (shared with every
	// stream that has the same Source); nil otherwise.
	Session *SessionObservations `json:"session,omitempty"`

	// LastPlayed is when a call on this stream last began to hear its
	// source (a session's first audio, or joining a session that already
	// had it). With LastFailure it tells a stream that works from one that
	// is failing.
	LastPlayed *time.Time `json:"lastPlayed,omitempty"`

	// LastFailure is the newest failure that concerned this stream since
	// StartedAt: its source would not start or stopped while a call on it
	// was open, or a call on it was refused because its pool was full.
	LastFailure *Failure `json:"lastFailure,omitempty"`

	// Calls counts the calls on this stream since StartedAt, refused or
	// not.
	Calls uint64 `json:"calls"`
}

// SessionState is where a session (one source playing, one decoder) is.
type SessionState string

// Session states.
const (
	// SessionStarting: the decoder runs and no audio has come yet (a tuner
	// locking, an Icecast connection); callers hear silence.
	SessionStarting SessionState = "Starting"
	// SessionPlaying: audio is flowing to the session's callers.
	SessionPlaying SessionState = "Playing"
	// SessionDraining: the last caller left; the decoder is stopped
	// streambridge.Grace later unless a caller joins first. It still
	// holds its source (a tuner).
	SessionDraining SessionState = "Draining"
)

// SessionObservations is a running session.
type SessionObservations struct {
	State SessionState `json:"state"`
	// StartedAt is when its decoder started.
	StartedAt time.Time `json:"startedAt"`
	// FirstAudioAt is when its first audio arrived; nil while Starting.
	FirstAudioAt *time.Time `json:"firstAudioAt,omitempty"`
	// Listeners is how many calls hear it now, across every stream with
	// its source.
	Listeners int `json:"listeners"`
}

// FailureReason says what went wrong. The values are
// v1alpha1.StreamFailureReason's (TestFailureReasons).
type FailureReason string

// Failure reasons.
const (
	// FailureSourceFailed: the source would not start. The decoder exited
	// before any audio (the HDHomeRun answered 503 because every tuner was
	// busy, a DNS name did not resolve, a TLS certificate was refused), or
	// no audio came within streambridge.StartTimeout. Its callers were
	// sent away ("not available").
	FailureSourceFailed FailureReason = "SourceFailed"
	// FailureSourceEnded: the source stopped while calls heard it (the
	// decoder exited, or no audio came for streambridge.StallTimeout). Its
	// callers were sent away.
	FailureSourceEnded FailureReason = "SourceEnded"
	// FailureTunerBusy: a call was refused because its stream's pool
	// already ran Pool.Max sessions of other sources, each with callers
	// (the stream's own source was not among them).
	FailureTunerBusy FailureReason = "TunerBusy"
)

// Failure is one failure.
type Failure struct {
	// At is when it happened.
	At time.Time `json:"at"`
	// Reason is what happened.
	Reason FailureReason `json:"reason"`
	// Message says it for a person: for a source, the decoder's exit and
	// its last error line ("ffmpeg exited with status 8 before any audio:
	// Server returned 503 Service Unavailable"); for TunerBusy, what held
	// the pool ("pool hdhomerun (1 at a time) is playing kiro7"). At most
	// MaxMessageBytes, printable.
	Message string `json:"message"`
}

// PoolObservations is one tuner pool.
type PoolObservations struct {
	// Name and Max are the pool's entry in the loaded streams.json.
	Name string `json:"name"`
	Max  int    `json:"max"`
	// Sessions are the pool's running sessions (each holds one of its
	// tuners), sorted by source; never null.
	Sessions []PoolSession `json:"sessions"`
}

// PoolSession is one running session of a pool.
type PoolSession struct {
	// Source is the session's source URL.
	Source string `json:"source"`
	// Streams are the names of the loaded streams with that source,
	// sorted.
	Streams []string `json:"streams"`
	// State is the session's state.
	State SessionState `json:"state"`
	// Listeners is how many calls hear it now.
	Listeners int `json:"listeners"`
}
