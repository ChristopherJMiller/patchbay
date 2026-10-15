// Command fakephone is the e2e suite's simulated phone (design §6). It talks
// to the PBX the way a Cisco 79x5 does at the protocol level: REGISTER with
// digest auth over TCP, G.711 µ-law calls, TFTP config fetches, and the
// service-control NOTIFY that asks it to re-read its config. It also carries
// the suite's other in-cluster tools: HTTP requests to the agent from the
// pod network (as the controller makes them), an AMI client, and two
// probes of the node (process credentials and file access).
//
// It runs as an ordinary Pod in the kind cluster, so every exchange crosses
// the pod network to the PBX on the node's host network, as a phone on the
// voice VLAN crosses the router to the PBX node. Each subcommand exits 0 when
// the exchange succeeded and 1 otherwise; the e2e harness reads its exit code
// and logs.
//
// Flags fall back to FAKEPHONE_* environment variables, so a Pod spec can set
// the server and credentials once (from Secrets).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	root.AddCommand(newRegisterCmd(rf), newCallCmd(rf), newTFTPCmd(rf), newHTTPCmd(rf),
		newAMICmd(rf), newProcCmd(), newReadCmd())
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
	f.StringVar(&o.User, "user", env("USER", ""),
		"the SIP user: the phone's number, its AOR ($FAKEPHONE_USER)")
	f.StringVar(&o.AuthUser, "auth-user", env("AUTH_USER", ""),
		"the digest username: the device ID (phone-desk); default --user ($FAKEPHONE_AUTH_USER)")
	f.StringVar(&o.Password, "password", env("PASSWORD", ""), "auth password ($FAKEPHONE_PASSWORD)")
	f.StringVar(&o.LocalIP, "local-ip", env("LOCAL_IP", ""),
		"address for Via, Contact and SDP; default: the route towards --server ($FAKEPHONE_LOCAL_IP)")
	f.StringVar(&o.ContactHost, "contact-host", "",
		"put this host in the Contact instead of --local-ip (to test the PBX's contact ACL)")
	f.IntVar(&o.SIPPort, "sip-port", envInt("SIP_PORT", 5060), "local SIP port to listen on ($FAKEPHONE_SIP_PORT)")
	f.StringVar(&o.UserAgent, "user-agent", env("USER_AGENT", "patchbay-fakephone"),
		"User-Agent header ($FAKEPHONE_USER_AGENT)")
}

func newRegisterCmd(rf *rootFlags) *cobra.Command {
	var (
		o          sipOptions
		expires    int
		waitNotify time.Duration
		event      string
		refetch    string
		tftpServer string
	)
	cmd := &cobra.Command{
		Use:   "register",
		Short: "REGISTER with digest auth; exit 0 on 200 OK",
		Long: `Register the phone's contact with the PBX, answering the digest challenge.
The contact stays registered for --expires seconds after fakephone exits;
--expires 0 unregisters.

With --wait-notify, stay registered and listening until the PBX sends a
NOTIFY with Event: --event (Cisco's service-control: re-read the config),
print it, then (with --refetch) fetch that file over TFTP from
--tftp-server, as the phone does; exit 0 only if all of that happened.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := rf.logger()
			timeout := rf.timeout
			if waitNotify > 0 {
				timeout += waitNotify
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
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
				"user", o.User, "authUser", p.authUser(), "challenged", challenged, "contact", contact)
			fmt.Fprintf(cmd.OutOrStdout(), "registered %s via %s challenged=%t contact=%s\n",
				o.User, p.transport, challenged, contact)
			if waitNotify <= 0 {
				return nil
			}

			req, err := p.waitNotify(ctx, event, waitNotify)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "notify event=%s subscription-state=%q body=%q\n",
				event, headerValues(req, "Subscription-State"), string(req.Body()))
			if refetch == "" {
				return nil
			}
			sum := sha256.New()
			n, err := tftpGet(tftpServer, refetch, 5*time.Second, sum)
			if err != nil {
				return fmt.Errorf("refetching after the NOTIFY: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "refetched %s bytes=%d sha256=%s\n", refetch, n, hex.EncodeToString(sum.Sum(nil)))
			return nil
		},
	}
	bindSIPFlags(cmd, &o)
	f := cmd.Flags()
	f.IntVar(&expires, "expires", 300, "registration lifetime in seconds; 0 unregisters")
	f.DurationVar(&waitNotify, "wait-notify", 0, "after registering, wait this long for a NOTIFY (0: do not wait)")
	f.StringVar(&event, "event", "service-control", "with --wait-notify: the NOTIFY Event to wait for")
	f.StringVar(&refetch, "refetch", "", "with --wait-notify: the file to fetch over TFTP once the NOTIFY came")
	f.StringVar(&tftpServer, "tftp-server", env("TFTP_SERVER", ""), "with --refetch: TFTP server host:port ($FAKEPHONE_TFTP_SERVER)")
	return cmd
}

// headerValues lists every value of a header, in order.
func headerValues(req *sip.Request, name string) []string {
	var out []string
	for _, h := range req.GetHeaders(name) {
		out = append(out, h.Value())
	}
	return out
}

func newCallCmd(rf *rootFlags) *cobra.Command {
	var (
		o              sipOptions
		mo             mediaOptions
		expectTone     bool
		minToneFrames  int
		expectAudio    time.Duration
		minVoice       time.Duration
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
carried the tone. With --expect-audio (an announcement) it passes only if at
least that much of what came back was not silence, and at least --min-voice
of it was not the SIT tone either (the prompt itself). Then BYE, unless the
PBX hung up first.`,
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
			return checkMedia(res, mo, expectTone, minToneFrames, expectAudio, minVoice)
		},
	}
	bindSIPFlags(cmd, &o)
	f := cmd.Flags()
	f.DurationVar(&mo.Duration, "duration", 3*time.Second, "how long to send the tone")
	f.DurationVar(&mo.Tail, "tail", 500*time.Millisecond, "how long to keep listening after the tone")
	f.Float64Var(&mo.ToneHz, "tone-hz", 1000, "tone frequency")
	f.BoolVar(&expectTone, "expect-tone", true, "fail unless the tone comes back (the echo test)")
	f.IntVar(&minToneFrames, "min-tone-frames", 50, "with --expect-tone: packets that must carry the tone")
	f.DurationVar(&expectAudio, "expect-audio", 0, "fail unless at least this much non-silent audio comes back")
	f.DurationVar(&minVoice, "min-voice", 0, "with --expect-audio: of it, at least this much that is not the SIT tone")
	f.IntVar(&rtpPort, "rtp-port", 0, "local RTP port; 0 picks one")
	return cmd
}

// checkMedia is the call's verdict on what came back.
func checkMedia(res mediaResult, mo mediaOptions, expectTone bool, minToneFrames int, expectAudio, minVoice time.Duration) error {
	frame := frameDuration * time.Millisecond
	switch {
	case res.Received == 0:
		return errors.New("no RTP came back")
	case expectTone && res.ToneFrames < minToneFrames:
		return fmt.Errorf("the %.0f Hz tone came back in %d packets, want at least %d", mo.ToneHz, res.ToneFrames, minToneFrames)
	case time.Duration(res.Loud)*frame < expectAudio:
		return fmt.Errorf("%s of non-silent audio came back, want at least %s", time.Duration(res.Loud)*frame, expectAudio)
	case time.Duration(res.Voice)*frame < minVoice:
		return fmt.Errorf("%s of it was not the SIT tone, want at least %s", time.Duration(res.Voice)*frame, minVoice)
	}
	return nil
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
		Long: `Fetch a file over TFTP in octet mode and write it to --output (stdout by
default). The log line says how many bytes came and their SHA-256.`,
		Args: cobra.ExactArgs(1),
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
			sum := sha256.New()
			n, err := tftpGet(server, args[0], min(rf.timeout, 5*time.Second), io.MultiWriter(w, sum))
			if err != nil {
				return err
			}
			log.Info("fetched", "file", args[0], "bytes", n, "sha256", hex.EncodeToString(sum.Sum(nil)), "server", server)
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
		data         string
		dataBase64   string
		printBody    bool
	)
	do := func(cmd *cobra.Command, method, url string) error {
		log := rf.logger()
		ctx, cancel := context.WithTimeout(cmd.Context(), rf.timeout)
		defer cancel()
		var body []byte
		switch {
		case data != "" && dataBase64 != "":
			return errors.New("--data and --data-base64 are exclusive")
		case dataBase64 != "":
			b, err := base64.StdEncoding.DecodeString(dataBase64)
			if err != nil {
				return fmt.Errorf("--data-base64: %w", err)
			}
			body = b
		case data != "":
			body = []byte(data)
		}
		code, resp, err := httpDo(ctx, method, url, headers, body)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(resp)
		log.Info("fetched", "method", method, "url", redactURL(url), "status", code,
			"bytes", len(resp), "sha256", hex.EncodeToString(sum[:]))
		fmt.Fprintf(cmd.OutOrStdout(), "%d\n", code)
		if printBody {
			_, _ = cmd.OutOrStdout().Write(resp)
		}
		if code != expectStatus {
			return fmt.Errorf("%s %s: status %d, want %d", method, redactURL(url), code, expectStatus)
		}
		return nil
	}
	get := &cobra.Command{
		Use:   "get <url>",
		Short: "GET a URL; exit 0 when the status is --expect-status",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return do(cmd, "GET", args[0]) },
	}
	send := &cobra.Command{
		Use:   "send <method> <url>",
		Short: "Send a request with --data or --data-base64 as its body; exit 0 when the status is --expect-status",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return do(cmd, strings.ToUpper(args[0]), args[1]) },
	}
	for _, c := range []*cobra.Command{get, send} {
		c.Flags().StringArrayVarP(&headers, "header", "H", nil, `request header "Name: value" (repeatable)`)
		c.Flags().IntVar(&expectStatus, "expect-status", 200, "the status that counts as success")
		c.Flags().BoolVar(&printBody, "print-body", true, "print the response body after the status line")
		httpCmd.AddCommand(c)
	}
	send.Flags().StringVar(&data, "data", "", "the request body")
	send.Flags().StringVar(&dataBase64, "data-base64", "", "the request body, base64")
	return httpCmd
}
