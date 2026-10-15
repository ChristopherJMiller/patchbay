package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// `fakephone run` lives a script.Script: the e2e harness's long-lived
// phones (docs/plans/release-a.md §6.2.8). The script package documents
// what each step and the automatic behaviours do; this file is the step
// loop and the State, fetch.go the config and file fetches, register.go
// the registration and the NOTIFYs, calls.go the calls, web.go the
// phone's HTTP server.
//
// Every step and every record is one JSON line on stdout ({"msg":"step",
// "phase":"start"|"done"|"failed",...}, {"msg":"fetch","record":{...}},
// ...), so a test can wait on the pod's log as well as on its State. No
// line, error or State ever holds a password (only its SHA-256), the
// provisioning URL or its token.

// Defaults for what a script leaves out.
const (
	userAgentCisco    = "Cisco-CP7945G/9.4.2 (patchbay fakephone)"
	userAgentLinphone = "Linphone/5.2 (patchbay fakephone)"
	// defaultSIPPort is where a phone listens and, before it has read a
	// config, where it assumes the PBX listens.
	defaultSIPPort = 5060
	// retryRegisterAfter is how soon a refresh that failed is tried again.
	retryRegisterAfter = 10 * time.Second
)

func newRunCmd(rf *rootFlags) *cobra.Command {
	var (
		path    string
		localIP string
		sipPort int
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Live a scripted phone's life (the e2e suite's long-lived phones)",
		Long: `Run the script.Script in --script FILE, else in $FAKEPHONE_SCRIPT: a
Cisco 79x5 (TFTP config, REGISTER over the config's transport, web pages
on the script's webListen) or Linphone (one-time provisioning URL, REGISTER
over UDP). Steps run in order; NOTIFYs, refreshes and incoming calls are
handled throughout. GET /fakephone/state returns the State; POST
/fakephone/signal/<name> releases a waitSignal step. A failed step exits 1
naming the step; when the steps are done the phone runs until it is
stopped (exit 0), or exits 0 at once with exitWhenDone. --timeout does not
apply.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := readScript(path)
			if err != nil {
				return err
			}
			sc, err := script.Parse(raw)
			if err != nil {
				return err
			}
			return runScript(cmd.Context(), sc, runOptions{
				LocalIP: localIP, SIPPort: sipPort, Out: cmd.OutOrStdout(), Log: rf.sipLogger(),
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&path, "script", "", "the script's JSON file; default $"+script.EnvScript)
	f.StringVar(&localIP, "local-ip", env("LOCAL_IP", ""),
		"address for Via, Contact and SDP; default: the route towards the script's server ($FAKEPHONE_LOCAL_IP)")
	f.IntVar(&sipPort, "sip-port", envInt("SIP_PORT", defaultSIPPort), "local SIP port to listen on ($FAKEPHONE_SIP_PORT)")
	return cmd
}

// readScript reads the script from path, else from $FAKEPHONE_SCRIPT.
func readScript(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own flag.
	}
	if v := os.Getenv(script.EnvScript); v != "" {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("no script: pass --script FILE or set $%s", script.EnvScript)
}

// runOptions are what run needs besides the script.
type runOptions struct {
	LocalIP string // "" detects the route towards the script's server
	SIPPort int    // 0 is defaultSIPPort
	// TFTPPort is where cisco mode TFTPs; 0 is script.TFTPPort (tests
	// serve elsewhere).
	TFTPPort int
	// TFTPTimeout is how long one TFTP packet may take; 0 is tftpTimeout.
	TFTPTimeout time.Duration
	// RetryAfter is how soon a failed refresh is retried; 0 is
	// retryRegisterAfter.
	RetryAfter time.Duration
	// HTTP fetches the provisioning URL; nil is a client with a timeout.
	HTTP *http.Client
	// Out takes the JSON lines; nil is stdout. Log takes the SIP stack's
	// own (text) logging; nil discards it.
	Out io.Writer
	Log *slog.Logger
	// Started, when set, is called once the phone listens (tests learn its
	// web address from it).
	Started func(*runner)
}

// runner is one running script.
type runner struct {
	sc   script.Script
	opts runOptions
	log  *slog.Logger // the JSON lines
	ctx  context.Context

	phone   *phone
	webAddr netip.AddrPort

	// cfgMu serialises config fetches (a step's and a NOTIFY's).
	cfgMu sync.Mutex
	// regMu serialises the REGISTERs that share the registration's
	// Call-ID, so their CSeqs go out in order.
	regMu sync.Mutex

	mu       sync.Mutex
	changed  chan struct{} // closed and replaced on every change of what follows
	state    script.State  // Step, Label, Done, Config, Web, Fetches, Registrations, Signals
	webSet   bool
	config   *deviceConfig
	pending  map[string]int // signals received and not yet waited for
	notifies []*notifyEntry
	calls    []*callEntry
	reg      registration
}

func newRunner(sc script.Script, opts runOptions) *runner {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.SIPPort == 0 {
		opts.SIPPort = defaultSIPPort
	}
	if opts.TFTPPort == 0 {
		opts.TFTPPort = script.TFTPPort
	}
	if opts.TFTPTimeout == 0 {
		opts.TFTPTimeout = tftpTimeout
	}
	if opts.RetryAfter == 0 {
		opts.RetryAfter = retryRegisterAfter
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	r := &runner{
		sc: sc, opts: opts,
		log:     slog.New(slog.NewJSONHandler(opts.Out, nil)),
		changed: make(chan struct{}),
		state:   script.NewState(sc.Mode),
		pending: map[string]int{},
	}
	if sc.Web != nil {
		r.state.Web, r.webSet = cloneWeb(*sc.Web), true
	}
	return r
}

// runScript runs sc until a step fails (the error names it), the steps
// are done (with ExitWhenDone), or ctx ends (nil: the phone was stopped).
func runScript(ctx context.Context, sc script.Script, opts runOptions) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	r := newRunner(sc, opts)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.ctx = ctx

	if err := r.start(ctx); err != nil {
		return err
	}
	defer r.phone.close()

	for i, st := range sc.Steps {
		r.setStep(i, st.Label)
		r.log.Info("step", "step", i, "label", st.Label, "action", st.Action(), "phase", "start")
		if err := r.do(ctx, st); err != nil {
			if ctx.Err() != nil {
				r.log.Info("stopped", "step", i, "label", st.Label)
				return nil
			}
			r.log.Error("step", "step", i, "label", st.Label, "action", st.Action(), "phase", "failed", "error", err.Error())
			return fmt.Errorf("step %d (%s) %s: %w", i, st.Label, st.Action(), err)
		}
		r.log.Info("step", "step", i, "label", st.Label, "action", st.Action(), "phase", "done")
	}
	r.setStep(len(sc.Steps), "")
	r.log.Info("done", "steps", len(sc.Steps))
	if sc.ExitWhenDone {
		return nil
	}
	<-ctx.Done()
	r.log.Info("stopped")
	return nil
}

// start listens: SIP on LocalIP:SIPPort (UDP and TCP), and the web server.
func (r *runner) start(ctx context.Context) error {
	server, err := netip.ParseAddr(r.sc.Server)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	pbx := netip.AddrPortFrom(server, defaultSIPPort)
	local, err := localAddrFor(r.opts.LocalIP, pbx)
	if err != nil {
		return err
	}
	transport := "udp"
	if r.sc.Mode == script.ModeCisco {
		transport = "tcp"
	}
	r.phone, err = startPhone(ctx, phoneConfig{
		Account:   account{Server: pbx, Transport: transport},
		LocalIP:   local,
		SIPPort:   r.opts.SIPPort,
		UserAgent: r.userAgent(),
		OnNotify:  r.onNotify,
		OnInvite:  r.onInvite,
	}, r.opts.Log)
	if err != nil {
		return err
	}
	if err := r.serveWeb(ctx); err != nil {
		r.phone.close()
		return err
	}
	r.log.Info("start", "mode", r.sc.Mode, "web", r.webAddr.String(),
		"sip", netip.AddrPortFrom(local, uint16(r.opts.SIPPort)).String(), "steps", len(r.sc.Steps))
	if r.opts.Started != nil {
		r.opts.Started(r)
	}
	return nil
}

func (r *runner) userAgent() string {
	switch {
	case r.sc.UserAgent != "":
		return r.sc.UserAgent
	case r.sc.Mode == script.ModeCisco:
		return userAgentCisco
	default:
		return userAgentLinphone
	}
}

func (r *runner) toneHz() float64 {
	if r.sc.ToneHz != 0 {
		return r.sc.ToneHz
	}
	return script.DefaultToneHz
}

// do runs one step.
func (r *runner) do(ctx context.Context, st script.Step) error {
	switch {
	case st.FetchConfig != nil:
		return r.retry(ctx, st.FetchConfig.Expect, st.FetchConfig.Retry, func() (script.Result, error) {
			return r.fetchConfig(ctx)
		})
	case st.Fetch != nil:
		return r.fetchFile(ctx, st.Fetch)
	case st.SetWeb != nil:
		r.setWeb(*st.SetWeb)
		return nil
	case st.Register != nil:
		return r.registerStep(ctx, st.Register)
	case st.Unregister != nil:
		return r.unregister(ctx)
	case st.StopRefreshing != nil:
		r.stopRefreshing()
		return nil
	case st.Call != nil:
		return r.placeCall(ctx, st.Call)
	case st.Hangup != nil:
		return r.hangupAll(ctx)
	case st.Transfer != nil:
		return r.transfer(ctx, st.Transfer.To)
	case st.WaitNotify != nil:
		return r.waitNotify(ctx, st.WaitNotify)
	case st.WaitCall != nil:
		return r.waitCall(ctx, st.WaitCall)
	case st.WaitSignal != nil:
		return r.waitSignal(ctx, st.WaitSignal)
	case st.Sleep != nil:
		return sleep(ctx, st.Sleep.For.D())
	}
	return errors.New("no action") // Validate refuses this
}

// changedLocked wakes every waiter; r.mu is held.
func (r *runner) changedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// waitUntil waits until cond (called with r.mu held) holds, for at most
// within (0: no limit); what names it in the error.
func (r *runner) waitUntil(ctx context.Context, within time.Duration, what string, cond func() bool) error {
	var timeout <-chan time.Time
	if within > 0 {
		t := time.NewTimer(within)
		defer t.Stop()
		timeout = t.C
	}
	for {
		r.mu.Lock()
		ok, ch := cond(), r.changed
		r.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-timeout:
			return fmt.Errorf("no %s within %s", what, within)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runner) setStep(i int, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Step, r.state.Label, r.state.Done = i, label, i >= len(r.sc.Steps)
	r.changedLocked()
}

func (r *runner) setWeb(w script.Web) {
	r.mu.Lock()
	r.state.Web, r.webSet = cloneWeb(w), true
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("web", "web", w)
}

func cloneWeb(w script.Web) script.Web {
	w.Log = slices.Clone(w.Log)
	return w
}

// currentConfig is the config last parsed, or nil.
func (r *runner) currentConfig() *deviceConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.config
}

// snapshot is the State now.
func (r *runner) snapshot() script.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state
	s.Web = cloneWeb(s.Web)
	if s.Config != nil {
		c := *s.Config
		s.Config = &c
	}
	s.Fetches = slices.Clone(s.Fetches)
	s.Registrations = slices.Clone(s.Registrations)
	s.Signals = slices.Clone(s.Signals)
	s.Registered = time.Now().Before(r.reg.until) // refreshed while refreshes succeed
	s.Notifies = make([]script.NotifyRecord, 0, len(r.notifies))
	for _, n := range r.notifies {
		rec := n.rec
		rec.SubscriptionState = slices.Clone(rec.SubscriptionState)
		s.Notifies = append(s.Notifies, rec)
	}
	s.Calls = make([]script.CallRecord, 0, len(r.calls))
	for _, c := range r.calls {
		s.Calls = append(s.Calls, c.recordLocked())
	}
	return s
}

// signal records a signal and releases a waitSignal step for it.
func (r *runner) signal(name string) {
	r.mu.Lock()
	r.state.Signals = append(r.state.Signals, name)
	r.pending[name]++
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("signal", "name", name)
}

func (r *runner) waitSignal(ctx context.Context, w *script.WaitSignal) error {
	return r.waitUntil(ctx, w.Within.D(), "signal "+w.Name, func() bool {
		if r.pending[w.Name] == 0 {
			return false
		}
		r.pending[w.Name]-- // each signal releases one wait
		return true
	})
}

// cloneFrames copies a ToneFrames map (nil stays nil).
func cloneFrames(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	return maps.Clone(m)
}
