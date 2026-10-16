// Package streams is the release B contract between the compiler, the
// stream bridge and the controller (docs/plans/release-b.md): how a Stream
// is named on the wire (UUID), what audio crosses AudioSocket (Codec,
// SampleRate, FrameBytes), and streams.json, the document the controller
// compiles and the bridge plays from (Document).
//
// The flow:
//
//	Stream resources + settings
//	    -> compile.Compile: the dialplan (each Stream's number dials
//	       AudioSocket/<bridge>/<UUID(name)>/c(Codec)) and Bundle.Streams
//	    -> the controller writes Bundle.Streams, Encoded, into the
//	       <fullname>-streams ConfigMap (install.StreamsConfigMap)
//	    -> the bridge reads it, mounted, Decodes it, and maps the UUID
//	       Asterisk sends in its first AudioSocket frame to a source.
//
// The bridge never reads resources, and the controller never probes a
// source (a probe would hold a tuner).
//
// The package is pure (no I/O, no clock) and is a contract: compile, the
// bridge and the controller build on it in parallel. Change it
// deliberately.
package streams
