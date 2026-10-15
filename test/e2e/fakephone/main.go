// Command fakephone is the e2e suite's simulated phone (design §6). It talks
// to the PBX the way a Cisco 79x5 does at the protocol level: REGISTER with
// digest auth over TCP, G.711 µ-law calls, TFTP config fetches. It can also
// GET a URL, to reach the agent from the pod network as the controller does.
//
// It runs as an ordinary Pod in the kind cluster, so every exchange crosses
// the pod network to the PBX on the node's host network, as a phone on the
// voice VLAN crosses the router to the PBX node. Each subcommand exits 0 when
// the exchange succeeded and 1 otherwise; the e2e harness reads its exit code
// and logs.
//
// Flags fall back to FAKEPHONE_* environment variables, so a Pod spec can set
// the server and credentials once.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakephone:", err)
		os.Exit(1)
	}
}

// env returns the FAKEPHONE_<name> variable, or def.
func env(name, def string) string {
	if v, ok := os.LookupEnv("FAKEPHONE_" + name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(env(name, "")); err == nil {
		return v
	}
	return def
}

type rootFlags struct {
	verbose bool
	timeout time.Duration
}

func newRootCmd() *cobra.Command {
	rf := &rootFlags{}
	root := &cobra.Command{
		Use:           "fakephone",
		Short:         "A simulated Cisco 79x5 for patchbay's e2e tests",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&rf.verbose, "verbose", "v", env("VERBOSE", "") != "",
		"debug logging, including every SIP message ($FAKEPHONE_VERBOSE)")
	root.PersistentFlags().DurationVar(&rf.timeout, "timeout", 30*time.Second,
		"give up after this long")
	root.AddCommand(newRegisterCmd(rf), newCallCmd(rf), newTFTPCmd(rf), newHTTPCmd(rf))
	return root
}

func (rf *rootFlags) logger() *slog.Logger {
	level := slog.LevelInfo
	if rf.verbose {
		level = slog.LevelDebug
		sip.SIPDebug = true
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// sipgo logs through its own default logger; keep it quiet unless -v.
	sipLevel := slog.LevelWarn
	if rf.verbose {
		sipLevel = slog.LevelDebug
	}
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: sipLevel})))
	return log
}

// bindSIPFlags adds the flags every SIP subcommand shares.
func bindSIPFlags(cmd *cobra.Command, o *sipOptions) {
	f := cmd.Flags()
	f.StringVar(&o.Server, "server", env("SERVER", ""), "the PBX as ip:port ($FAKEPHONE_SERVER)")
	f.StringVar(&o.Transport, "transport", env("TRANSPORT", "tcp"),
		"tcp (what the 79x5 uses) or udp ($FAKEPHONE_TRANSPORT)")
	f.StringVar(&o.User, "user", env("USER", ""), "endpoint and auth username ($FAKEPHONE_USER)")
	f.StringVar(&o.Password, "password", env("PASSWORD", ""), "auth password ($FAKEPHONE_PASSWORD)")
	f.StringVar(&o.LocalIP, "local-ip", env("LOCAL_IP", ""),
		"address for Via, Contact and SDP; default: the route towards --server ($FAKEPHONE_LOCAL_IP)")
	f.IntVar(&o.SIPPort, "sip-port", envInt("SIP_PORT", 5060), "local SIP port to listen on ($FAKEPHONE_SIP_PORT)")
	f.StringVar(&o.UserAgent, "user-agent", env("USER_AGENT", "patchbay-fakephone"),
		"User-Agent header ($FAKEPHONE_USER_AGENT)")
}

func newRegisterCmd(rf *rootFlags) *cobra.Command {
	var o sipOptions
	var expires int
	cmd := &cobra.Command{
		Use:   "register",
		Short: "REGISTER with digest auth; exit 0 on 200 OK",
		Long: `Register the phone's contact with the PBX, answering the digest challenge.
The contact stays registered for --expires seconds after fakephone exits;
--expires 0 unregisters.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := rf.logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), rf.timeout)
			defer cancel()
			p, err := newPhone(ctx, o, log)
			if err != nil {
				return err
			}
			defer p.close()

			res, challenged, err := p.register(ctx, expires)
			if err != nil {
				return err
			}
			if res.StatusCode != sip.StatusOK {
				return fmt.Errorf("REGISTER: %d %s (challenged=%t)", res.StatusCode, res.Reason, challenged)
			}
			contact := ""
			if h := res.Contact(); h != nil {
				contact = h.Value()
			}
			log.Info("registered", "server", o.Server, "transport", p.transport,
				"user", o.User, "challenged", challenged, "contact", contact)
			fmt.Fprintf(cmd.OutOrStdout(), "registered %s via %s challenged=%t contact=%s\n",
				o.User, p.transport, challenged, contact)
			return nil
		},
	}
	bindSIPFlags(cmd, &o)
	cmd.Flags().IntVar(&expires, "expires", 300, "registration lifetime in seconds; 0 unregisters")
	return cmd
}

func newCallCmd(rf *rootFlags) *cobra.Command {
	var (
		o              sipOptions
		mo             mediaOptions
		expectTone     bool
		minToneFrames  int
		rtpPort        int
		hangupDeadline = 5 * time.Second
	)
	cmd := &cobra.Command{
		Use:   "call <number>",
		Short: "Call a number, send a tone, check what comes back, hang up",
		Long: `Place a call (INVITE with digest auth, G.711 µ-law offer), wait for 200 OK,
then send a --tone-hz tone for --duration while measuring the tone in the
audio that comes back (Goertzel). With --expect-tone (the default, for the
echo test) the call passes only if at least --min-tone-frames 20 ms packets
carried the tone. Then BYE.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := rf.logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), rf.timeout)
			defer cancel()
			p, err := newPhone(ctx, o, log)
			if err != nil {
				return err
			}
			defer p.close()

			var lc net.ListenConfig
			pc, err := lc.ListenPacket(ctx, "udp", net.JoinHostPort(p.localIP.String(), strconv.Itoa(rtpPort)))
			if err != nil {
				return fmt.Errorf("RTP socket: %w", err)
			}
			rtpConn := pc.(*net.UDPConn)
			defer func() { _ = rtpConn.Close() }()
			localRTP := rtpConn.LocalAddr().(*net.UDPAddr).Port

			c, err := p.invite(ctx, args[0], localRTP)
			if err != nil {
				return err
			}
			log.Info("call answered", "number", args[0], "remoteRTP", c.remote.Addr.String(),
				"payloadTypes", c.remote.PayloadTypes, "localRTP", localRTP)

			res, mediaErr := runMedia(ctx, rtpConn, c.remote.Addr, mo)

			byeCtx, byeCancel := context.WithTimeout(context.WithoutCancel(ctx), hangupDeadline)
			defer byeCancel()
			byeErr := c.hangup(byeCtx)

			log.Info("media", "result", res.String())
			fmt.Fprintf(cmd.OutOrStdout(), "call %s: %s\n", args[0], res)
			if mediaErr != nil {
				return mediaErr
			}
			if byeErr != nil {
				return fmt.Errorf("BYE: %w", byeErr)
			}
			if res.Received == 0 {
				return errors.New("no RTP came back")
			}
			if expectTone && res.ToneFrames < minToneFrames {
				return fmt.Errorf("the %.0f Hz tone came back in %d packets, want at least %d",
					mo.ToneHz, res.ToneFrames, minToneFrames)
			}
			return nil
		},
	}
	bindSIPFlags(cmd, &o)
	f := cmd.Flags()
	f.DurationVar(&mo.Duration, "duration", 3*time.Second, "how long to send the tone")
	f.DurationVar(&mo.Tail, "tail", 500*time.Millisecond, "how long to keep listening after the tone")
	f.Float64Var(&mo.ToneHz, "tone-hz", 1000, "tone frequency")
	f.BoolVar(&expectTone, "expect-tone", true, "fail unless the tone comes back (the echo test)")
	f.IntVar(&minToneFrames, "min-tone-frames", 50, "with --expect-tone: packets that must carry the tone")
	f.IntVar(&rtpPort, "rtp-port", 0, "local RTP port; 0 picks one")
	return cmd
}

func newTFTPCmd(rf *rootFlags) *cobra.Command {
	tftpCmd := &cobra.Command{
		Use:   "tftp",
		Short: "TFTP client, as the phone fetches its config",
	}
	var server, out string
	get := &cobra.Command{
		Use:   "get <file>",
		Short: "Fetch a file over TFTP (octet mode); exit 0 on success",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := rf.logger()
			if server == "" {
				return errors.New("--server is required")
			}
			w := cmd.OutOrStdout()
			if out != "" && out != "-" {
				f, err := os.Create(out) //nolint:gosec // G304: the path is the test's own flag.
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				w = f
			}
			n, err := tftpGet(server, args[0], min(rf.timeout, 5*time.Second), w)
			if err != nil {
				return err
			}
			log.Info("fetched", "file", args[0], "bytes", n, "server", server)
			return nil
		},
	}
	get.Flags().StringVar(&server, "server", env("TFTP_SERVER", ""), "TFTP server as host:port ($FAKEPHONE_TFTP_SERVER)")
	get.Flags().StringVarP(&out, "output", "o", "-", "where to write the file; - is stdout")
	tftpCmd.AddCommand(get)
	return tftpCmd
}

func newHTTPCmd(rf *rootFlags) *cobra.Command {
	httpCmd := &cobra.Command{
		Use:   "http",
		Short: "HTTP client, as a pod-network client sees the agent",
	}
	var (
		headers      []string
		expectStatus int
	)
	get := &cobra.Command{
		Use:   "get <url>",
		Short: "GET a URL; exit 0 when the status is --expect-status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := rf.logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), rf.timeout)
			defer cancel()
			code, body, err := httpGet(ctx, args[0], headers)
			if err != nil {
				return err
			}
			log.Info("fetched", "url", args[0], "status", code, "bytes", len(body))
			fmt.Fprintf(cmd.OutOrStdout(), "%d\n%s", code, body)
			if code != expectStatus {
				return fmt.Errorf("GET %s: status %d, want %d", args[0], code, expectStatus)
			}
			return nil
		},
	}
	get.Flags().StringArrayVarP(&headers, "header", "H", nil, `request header "Name: value" (repeatable)`)
	get.Flags().IntVar(&expectStatus, "expect-status", 200, "the status that counts as success")
	httpCmd.AddCommand(get)
	return httpCmd
}
