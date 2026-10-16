package streambridge

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// Source starts decoders: the seam between the session manager and ffmpeg,
// which the bridge's tests fake (a decoder that yields a known tone, never
// starts, fails, or stalls).
type Source interface {
	// Start starts a decoder for one source URL (a streams.Stream.Source).
	// It returns once the decoder runs (the ffmpeg process started), not
	// once audio flows. The decoder runs until ctx ends or it fails.
	Start(ctx context.Context, source string) (Decoder, error)
}

// Decoder is one running source.
type Decoder interface {
	// Read reads the decoded audio: streams.SampleRate, mono, signed
	// 16-bit little-endian, as it arrives. Once the decoder has ended,
	// Read returns an error (io.EOF, or another).
	io.Reader
	// Wait waits for the decoder to end and says why: nil if it ended
	// because its ctx did; otherwise an error whose text is a person's
	// reason, printable and at most api.MaxMessageBytes ("ffmpeg exited
	// with status 8: Server returned 503 Service Unavailable"), which
	// becomes the stream's api.Failure.Message.
	Wait() error
}

// FFmpeg's options for the bridge's decoder (docs/plans/release-b.md
// §2(m)). Checked 2026-10-16 with ffmpeg 9.0.1 (the dev shell) on a
// generated MPEG-TS (MPEG-2 video and AC-3 audio, like an HDHomeRun's) and
// an MP3, each served over HTTP at real time: the probe limits bring the
// first PCM frame from 2.8 s (ffmpeg's defaults, for the TS) to 0.15 s; an
// unlisted protocol or format is refused with a clear error.
const (
	// ffmpegProtocols: only network sources; never file:, pipe:, data:,
	// concat: or subfile:, even through a redirect or an HLS playlist.
	ffmpegProtocols = "http,https,tcp,tls,crypto"
	// ffmpegFormats: the demuxers of a tuner's MPEG-TS, Icecast and HLS
	// radio (MP3, AAC, Ogg, FLAC, fragmented MP4) and the e2e fake's WAV.
	ffmpegFormats = "mpegts,mp3,aac,ogg,flac,wav,hls,mov"
	// ffmpegRWTimeout: a read or write that blocks this long (µs) fails
	// the source.
	ffmpegRWTimeout = "10000000"
	// ffmpegProbeSize and ffmpegAnalyzeDuration (bytes, µs) bound how much
	// of the source ffmpeg reads before decoding (bench §8: an HDHomeRun
	// channel's real audio track).
	ffmpegProbeSize       = "1000000"
	ffmpegAnalyzeDuration = "1000000"
)

// FFmpegArgs is the decoder's command line, without the binary: read
// source and write streams.SampleRate mono signed 16-bit little-endian PCM
// to standard output, audio only (the source's first, or best, audio
// track). An https source has its server's certificate verified against
// caFile ("" is the system's trust store); ffmpeg refuses -tls_verify on
// anything but https, so only https gets it. source must pass
// streams.CheckSource.
func FFmpegArgs(source, caFile string) []string {
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", ffmpegProtocols,
		"-format_whitelist", ffmpegFormats,
		"-rw_timeout", ffmpegRWTimeout,
		"-probesize", ffmpegProbeSize,
		"-analyzeduration", ffmpegAnalyzeDuration,
	}
	if strings.HasPrefix(strings.ToLower(source), "https:") {
		args = append(args, "-tls_verify", "1")
		if caFile != "" {
			args = append(args, "-ca_file", caFile)
		}
	}
	return append(args,
		"-i", source,
		"-vn", "-sn", "-dn",
		"-ac", "1",
		"-ar", strconv.Itoa(streams.SampleRate),
		"-acodec", "pcm_s16le",
		"-f", "s16le",
		"pipe:1",
	)
}
