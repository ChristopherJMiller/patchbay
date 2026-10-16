// Package streambridge is `patchbay stream-bridge` (design §9,
// docs/plans/release-b.md): the AudioSocket server that plays a Stream's
// live source to every caller of its number, and holds the source (a
// tuner) only while someone listens.
//
// Asterisk's dialplan sends a call on a stream number to the bridge with
// Dial(AudioSocket/<bridge>:9092/<stream UUID>/c(slin16)). The bridge:
//   - reads the UUID frame and looks the stream up in streams.json (the
//     <fullname>-streams ConfigMap, mounted; it never reads resources);
//     an unknown UUID gets a hangup frame: "not available";
//   - joins the call to its source's session, starting one if none runs:
//     one decoder (ffmpeg, FFmpegArgs) per source, shared by every caller
//     of every stream with that source;
//   - refuses the call (hangup) if the stream's tuner pool already runs
//     its Max sessions of other sources with callers; a pool's idle
//     (Draining) session is stopped to make room instead;
//   - fans each streams.FrameBytes frame out to a bounded queue per caller
//     (QueueFrames; a full queue drops its oldest frame, so a slow caller
//     never stalls the others), and paces each caller at one frame per
//     streams.FrameDuration on a monotonic clock, sending silence when its
//     queue is empty (a tuner locking);
//   - reads and discards the caller's audio, ignores DTMF;
//   - stops the decoder Grace after its last caller leaves, which frees
//     the tuner;
//   - if a source fails to start or stops, sends every caller of its
//     session a hangup frame (the dialplan then plays "not available") and
//     records the failure;
//   - serves its observations (streambridge/api) for the controller, and
//     metrics, on the control port.
//
// Asterisk ends each call at the install's time limit
// (settings streams.callLimit); the bridge only sees the connection close.
//
// Contract files (docs/plans/release-b.md §4): doc.go, options.go and
// source.go here, audiosocket/frame.go and api/api.go. The bridge stage
// owns every other file of this package and its subpackages.
package streambridge
