package streambridge

import (
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Options configures the bridge (Run). cmd/patchbay's stream-bridge
// command fills it from the flags named below; tests fill it directly.
type Options struct {
	// Listen is the AudioSocket address (FlagListen; DefaultListen).
	Listen string
	// ControlListen is the control API's address: observations, metrics,
	// probes (FlagControlListen; DefaultControlListen).
	ControlListen string
	// StreamsFile is streams.json, the <fullname>-streams ConfigMap mounted
	// (FlagStreamsFile; DefaultStreamsFile). It is read at start and
	// checked again every StreamsPollInterval: a new valid version replaces
	// the old one (calls already playing keep their session); an invalid
	// one is reported (BridgeStatus.StreamsError) and the old one stays.
	StreamsFile string
	// FFmpeg is the decoder binary (FlagFFmpeg; DefaultFFmpeg). Used only
	// when Source is nil.
	FFmpeg string
	// CAFile is the trust store for https sources (FFmpegArgs); "" is the
	// system's. Tests set it; there is no flag.
	CAFile string
	// Source starts decoders. Nil: ffmpeg at FFmpeg with FFmpegArgs.
	Source Source
	// Clock is the bridge's clock (pacing, grace, timeouts); nil is the
	// real one.
	Clock clock.WithTicker
	// Registerer takes the bridge's metrics and Gatherer serves them on
	// /metrics; both nil: a new registry of the bridge's own.
	Registerer prometheus.Registerer
	Gatherer   prometheus.Gatherer
	// Log is the bridge's logger. It never logs a caller's audio.
	Log logr.Logger
}

// The stream-bridge command's flags (the chart passes them; test/chart
// checks it against these names).
const (
	FlagListen        = "listen"
	FlagControlListen = "control-listen"
	FlagStreamsFile   = "streams-file"
	FlagFFmpeg        = "ffmpeg"
)

// Defaults: the ports of design §9 and the chart's mounts.
var (
	// DefaultListen is ":9092".
	DefaultListen = ":" + strconv.Itoa(api.DefaultAudioSocketPort)
	// DefaultControlListen is ":9093".
	DefaultControlListen = ":" + strconv.Itoa(api.DefaultControlPort)
)

const (
	// DefaultStreamsDir is where the chart mounts the <fullname>-streams
	// ConfigMap in the bridge.
	DefaultStreamsDir = "/etc/patchbay/streams"
	// DefaultStreamsFile is streams.json in it.
	DefaultStreamsFile = DefaultStreamsDir + "/" + streams.FileName
	// DefaultFFmpeg is where the bridge image installs ffmpeg (Alpine's
	// package).
	DefaultFFmpeg = "/usr/bin/ffmpeg"
)

// Timing and bounds (docs/plans/release-b.md §2(k), §2(l); §8 says which a
// bench may move).
const (
	// Grace is how long a session's decoder keeps running after its last
	// caller leaves (design §9: 5 s), so a caller who redials at once
	// hears audio at once; then the decoder stops and the tuner is free.
	Grace = 5 * time.Second
	// PreemptSettle is how long the bridge waits after stopping a pool's
	// Draining session, to make room for another source of the pool,
	// before it starts the new one: time for the tuner device to free the
	// tuner (bench §8).
	PreemptSettle = time.Second
	// StartTimeout: a session with no audio this long after its decoder
	// started fails (FailureSourceFailed).
	StartTimeout = 15 * time.Second
	// StallTimeout: a playing session with no audio for this long ends
	// (FailureSourceEnded).
	StallTimeout = 10 * time.Second
	// IDTimeout is how long a new connection has to send its UUID frame.
	IDTimeout = 5 * time.Second
	// QueueFrames is each caller's queue, in frames (1 s): a full queue
	// drops its oldest frame.
	QueueFrames = 50
	// MaxCalls is the most AudioSocket connections at once; more are
	// refused (BridgeStatus.FullCalls).
	MaxCalls = 64
	// MaxSessions is the most sessions (decoders) at once; a call that
	// would start one more is refused.
	MaxSessions = 8
	// StreamsPollInterval is how often StreamsFile is checked.
	StreamsPollInterval = 2 * time.Second
	// StopTimeout is how long a stopped decoder has to exit before it is
	// killed (ffmpeg: SIGTERM, then SIGKILL).
	StopTimeout = 2 * time.Second
)
