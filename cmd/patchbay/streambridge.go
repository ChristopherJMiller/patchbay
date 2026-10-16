package main

import (
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
)

func newStreamBridgeCmd() *cobra.Command {
	o := streambridge.Options{}
	cmd := &cobra.Command{
		Use:   "stream-bridge",
		Short: "Run the stream bridge: live audio sources to callers over AudioSocket",
		Long: `Run the stream bridge (design §9; docs/streams.md). Asterisk dials it over
AudioSocket (--listen) for a Stream's number with the Stream's UUID; the
bridge plays that Stream's source to the caller as 16 kHz signed linear audio,
paced at 20 ms, through one ffmpeg (--ffmpeg) per source shared by everyone
listening, and stops it 5 s after the last caller leaves. It enforces the
tuner pools' limits (at most max sources of a pool at once) and refuses a
call that would break one.

What it plays is streams.json (--streams-file, the <fullname>-streams
ConfigMap the controller compiles, mounted), read at start and checked every
2 s. Its control API (--control-listen) serves the controller its
observations (/v1/observations, read only, no token), /healthz, /readyz and
/metrics. It reads no kubeconfig and holds no credentials.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&o.Listen, streambridge.FlagListen, streambridge.DefaultListen,
		"the AudioSocket address Asterisk dials")
	f.StringVar(&o.ControlListen, streambridge.FlagControlListen, streambridge.DefaultControlListen,
		"the control API's address: observations, /healthz, /readyz, /metrics")
	f.StringVar(&o.StreamsFile, streambridge.FlagStreamsFile, streambridge.DefaultStreamsFile,
		"streams.json (the <fullname>-streams ConfigMap, mounted as a whole volume so its updates are seen)")
	f.StringVar(&o.FFmpeg, streambridge.FlagFFmpeg, streambridge.DefaultFFmpeg,
		"the ffmpeg binary that decodes the sources")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		o.Log = ctrl.Log.WithName("stream-bridge")
		return streambridge.Run(cmd.Context(), o)
	}
	return cmd
}
