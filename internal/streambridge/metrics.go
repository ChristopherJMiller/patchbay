package streambridge

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The bridge's metrics (docs/plans/release-b.md 6.1 BR.7), registered on
// Options.Registerer and served from Options.Gatherer on api.PathMetrics:
//
//	patchbay_stream_bridge_calls{stream}                    open calls per loaded stream (by name)
//	patchbay_stream_bridge_sessions{pool}                   running sessions (decoders) per pool; "none" without one
//	patchbay_stream_bridge_session_starts_total{result}     sessions that played (first audio) or failed before it
//	patchbay_stream_bridge_refused_calls_total{reason}      unknown_stream, tuner_busy, bridge_full
//	patchbay_stream_bridge_frames_dropped_total             frames a full caller queue dropped
//	patchbay_stream_bridge_underrun_frames_total            silent frames sent while a session was Playing
//	patchbay_stream_bridge_streams_version_info{version}    1 for the loaded streams.json (one series)
//	patchbay_stream_bridge_streams_load_failures_total      streams.json files that could not be loaded
//
// The three gauges are read from the manager at each scrape, so a stream
// or pool that is gone has no series. Labels carry stream and pool names,
// versions and fixed words: never a source URL, a caller or audio.
const (
	resultPlayed = "played"
	resultFailed = "failed"

	refusedUnknown   = "unknown_stream"
	refusedTunerBusy = "tuner_busy"
	refusedFull      = "bridge_full"

	// noPool is the sessions gauge's label for sources in no pool.
	noPool = "none"
)

// metrics are the bridge's counters.
type metrics struct {
	starts       *prometheus.CounterVec
	refused      *prometheus.CounterVec
	dropped      prometheus.Counter
	underruns    prometheus.Counter
	loadFailures prometheus.Counter
}

func newMetrics() *metrics {
	mt := &metrics{
		starts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patchbay_stream_bridge_session_starts_total",
			Help: "Sessions (decoders) that played (their first audio arrived) or failed before any audio.",
		}, []string{"result"}),
		refused: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patchbay_stream_bridge_refused_calls_total",
			Help: "Calls refused: an unknown stream, a busy tuner pool, or a bridge at its limit of calls or sessions.",
		}, []string{"reason"}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "patchbay_stream_bridge_frames_dropped_total",
			Help: "Audio frames dropped from a full caller queue (a slow caller, or a source that bursts).",
		}),
		underruns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "patchbay_stream_bridge_underrun_frames_total",
			Help: "Silent frames sent to callers of a playing session because no audio was queued.",
		}),
		loadFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "patchbay_stream_bridge_streams_load_failures_total",
			Help: "streams.json files that could not be read or decoded (the previous one stays in use).",
		}),
	}
	for _, r := range []string{resultPlayed, resultFailed} {
		mt.starts.WithLabelValues(r)
	}
	for _, r := range []string{refusedUnknown, refusedTunerBusy, refusedFull} {
		mt.refused.WithLabelValues(r)
	}
	return mt
}

var (
	callsDesc = prometheus.NewDesc("patchbay_stream_bridge_calls",
		"Open calls per stream of the loaded streams.json.", []string{"stream"}, nil)
	sessionsDesc = prometheus.NewDesc("patchbay_stream_bridge_sessions",
		"Running sessions (one decoder each) per tuner pool; pool=\"none\" for sources in no pool.", []string{"pool"}, nil)
	versionDesc = prometheus.NewDesc("patchbay_stream_bridge_streams_version_info",
		"1 for the version of the streams.json the bridge plays from.", []string{"version"}, nil)
)

// gauges reads the three gauges from the manager at each scrape.
type gauges struct{ m *manager }

func (g gauges) Describe(ch chan<- *prometheus.Desc) {
	ch <- callsDesc
	ch <- sessionsDesc
	ch <- versionDesc
}

func (g gauges) Collect(ch chan<- prometheus.Metric) {
	calls, sessions, version := g.m.gaugeValues()
	for name, n := range calls {
		ch <- prometheus.MustNewConstMetric(callsDesc, prometheus.GaugeValue, float64(n), name)
	}
	for pool, n := range sessions {
		ch <- prometheus.MustNewConstMetric(sessionsDesc, prometheus.GaugeValue, float64(n), pool)
	}
	if version != "" {
		ch <- prometheus.MustNewConstMetric(versionDesc, prometheus.GaugeValue, 1, version)
	}
}
