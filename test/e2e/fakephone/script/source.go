package script

// The fake stream source, `fakephone source` (release B,
// docs/plans/release-b.md §6.6): an HTTP server that runs in the e2e kind
// cluster as the stream bridge's sources, so no test reaches a real tuner
// or the internet. The packaging stage builds it; the e2e harness deploys
// it and reads its SourceStatus. Contract file: types and constants only.
//
// It serves two listeners from one process:
//   - a tuner listener (DefaultTunerPort), which behaves as an HDHomeRun
//     with --tuners tuners: each open stream holds one tuner until the
//     client disconnects; with none free (open streams plus Occupied) a
//     request gets 503 Service Unavailable with the header
//     "X-HDHomeRun-Error: 805 All Tuners In Use", as the real device
//     answers; the first audio comes --lock-delay after the request, as a
//     tuner locks;
//   - a radio listener (DefaultRadioPort): no tuners, no limit, no delay.
//
// On either, GET of any other path streams a tone, endlessly and paced at
// real time: a WAV header (PCM, SourceSampleRate, SourceChannels, 16-bit,
// with the largest data size), then the samples, the tone's frequency
// taken from the SourceParamHz query parameter (DefaultSourceHz without
// it). A real source is not 16 kHz mono either, so the bridge's resampling
// and downmix are exercised.

// Paths, parameters and defaults of the fake source.
const (
	// SourcePathStatus is GET on either listener: that listener's
	// SourceStatus as JSON.
	SourcePathStatus = "/fake/status"
	// SourcePathOccupy is POST on the tuner listener with
	// ?SourceParamTuners=<n>: hold n tuners as a DVR recording would (0
	// frees them). The answer is the new SourceStatus.
	SourcePathOccupy = "/fake/occupy"
	// SourceParamTuners is SourcePathOccupy's parameter.
	SourceParamTuners = "tuners"
	// SourceParamHz is a stream path's query parameter: the tone, in Hz.
	SourceParamHz = "hz"

	// DefaultSourceHz is the tone without SourceParamHz.
	DefaultSourceHz = 1000
	// SourceSampleRate and SourceChannels are what the WAV carries.
	SourceSampleRate = 48000
	SourceChannels   = 2

	// DefaultTunerPort and DefaultRadioPort are the listeners' ports (the
	// tuner's is the HDHomeRun's stream port).
	DefaultTunerPort = 5004
	DefaultRadioPort = 8000

	// HDHomeRunErrorHeader and HDHomeRunAllTunersInUse are what a busy
	// tuner listener answers, beside its 503.
	HDHomeRunErrorHeader    = "X-HDHomeRun-Error"
	HDHomeRunAllTunersInUse = "805 All Tuners In Use"
)

// SourceStatus is one listener's state.
type SourceStatus struct {
	// Tuners is the tuner listener's --tuners; 0 on the radio listener
	// (no limit).
	Tuners int `json:"tuners"`
	// Occupied is how many tuners SourcePathOccupy holds.
	Occupied int `json:"occupied"`
	// Open is how many streams are being served now (each holds a tuner on
	// the tuner listener), by path (no query); never null.
	Open map[string]int `json:"open"`
	// Opened counts every stream served since the process started, by
	// path; never null.
	Opened map[string]uint64 `json:"opened"`
	// Refused counts the requests answered 503 because no tuner was free.
	Refused uint64 `json:"refused"`
}

// OpenTotal is how many streams are open on the listener, every path
// together: on the tuner listener, the tuners in use but Occupied.
func (s SourceStatus) OpenTotal() int {
	n := 0
	for _, v := range s.Open {
		n += v
	}
	return n
}
