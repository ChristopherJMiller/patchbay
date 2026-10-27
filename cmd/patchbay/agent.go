package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/agent"
	"github.com/ChristopherJMiller/patchbay/internal/agent/actions"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/agent/observe"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/agent/push"
	"github.com/ChristopherJMiller/patchbay/internal/agent/scrape"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sounds"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// agentTokenEnv is the environment variable the agent token can come from
// when --token-file is not given.
const agentTokenEnv = "PATCHBAY_AGENT_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential.

// agentOptions are `patchbay agent`'s flags.
type agentOptions struct {
	listen    string // control API
	tokenFile string

	namespace       string
	bundleConfigMap string // <fullname>-pbx-config
	secrets         string // <fullname>-pbx-secrets

	liveDir     string
	firmwareDir string
	stateDir    string
	soundsDir   string
	soundsSrc   string
	soundsVoice string
	fetchSocket string
	fsGroup     int

	deviceHTTPListen string // <pbx address>:8088
	sipBind          string // <pbx address>:<SIP port>, what Asterisk's transports bind
	amiAddress       string
}

func newAgentCmd(g *globalFlags) *cobra.Command {
	o := agentOptions{}
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the PBX agent: the sidecar that senses and acts, never decides",
		Long: `Run the agent, the native sidecar in the PBX pod (design §5). It

  - watches the compiled bundle (--bundle-configmap) and its projected
    Secret (--secrets), resolves the placeholders, writes each version into
    the live directory, reloads Asterisk over AMI, verifies, and rolls back
    a version Asterisk does not take;
  - serves device HTTP (--device-http-listen): the directory and each
    softphone's one-time provisioning document;
  - keeps the observations the controller polls: the file server's fetches
    (over --fetch-socket), device HTTP's, registrations and calls from AMI,
    and the phones' own web pages;
  - sends the resync and restart NOTIFYs, and stores verified firmware
    uploads (--firmware-dir);
  - serves the control API (--listen, bearer token) and /metrics.

It reports ready once the first config version is live and the sound prompts
are installed (--sounds-voice, from --sounds-src into --sounds-dir), so
Asterisk starts only on valid config. It refuses to start with a voice the
image does not carry. It applies only versions compiled for --sip-bind, the
address and port this pod's Asterisk binds: a reload cannot move them.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", net.JoinHostPort("", strconv.Itoa(api.DefaultPort)), "control API address")
	f.StringVar(&o.tokenFile, "token-file", "", "file holding the control API bearer token (default $"+agentTokenEnv+")")
	f.StringVar(&o.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "the PBX namespace (default $POD_NAMESPACE)")
	f.StringVar(&o.bundleConfigMap, "bundle-configmap", "", "the compiled bundle's ConfigMap (<fullname>-pbx-config); required")
	f.StringVar(&o.secrets, "secrets", "", "the bundle's projected Secret (<fullname>-pbx-secrets); required")
	f.StringVar(&o.liveDir, "live-dir", compile.LiveDir, "the live config directory, shared with Asterisk and the file server")
	f.StringVar(&o.firmwareDir, "firmware-dir", layout.FirmwareDir, "the firmware store, on the firmware volume")
	f.StringVar(&o.stateDir, "state-dir", layout.StateDir,
		"the agent's persistent state (spent provisioning tokens), on the firmware volume")
	f.StringVar(&o.soundsDir, "sounds-dir", compile.SoundsDir, "where the sound prompts are installed for Asterisk")
	f.StringVar(&o.soundsSrc, "sounds-src", sounds.DefaultSource, "where the image carries the sound prompts, a directory per voice")
	f.StringVar(&o.soundsVoice, "sounds-voice", sounds.DefaultVoice,
		"the sound prompts' voice: one of "+strings.Join(sounds.Voices, ", "))
	f.StringVar(&o.fetchSocket, "fetch-socket", layout.FetchSocket, "the unix socket the file server reports its fetches to")
	f.IntVar(&o.fsGroup, "fs-group", layout.DefaultFSGroup,
		"the group every live config file gets (the pod's fsGroup, Asterisk's group)")
	f.StringVar(&o.deviceHTTPListen, "device-http-listen", "", "device HTTP address, <pbx address>:8088; required")
	f.StringVar(&o.sipBind, "sip-bind", "",
		"<pbx address>:<SIP port>, where this pod's Asterisk binds SIP: only versions compiled for it are applied; required")
	f.StringVar(&o.amiAddress, "ami-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(compile.AMIPort)),
		"Asterisk's AMI address")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		log := ctrl.Log.WithName("agent")
		// Check the flags before touching any kubeconfig.
		if err := o.check(); err != nil {
			return err
		}
		tokens, err := agentTokens(o.tokenFile, log)
		if err != nil {
			return err
		}
		cfg, err := g.restConfig()
		if err != nil {
			return err
		}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		log.Info("starting", "version", version.String(), "namespace", o.namespace,
			"configMap", o.bundleConfigMap, "secret", o.secrets)
		return runAgent(cmd.Context(), o, tokens, cs, log)
	}
	return cmd
}

// boot is the boot config of this pod's Asterisk, from --sip-bind.
func (o agentOptions) boot() (compile.Boot, error) {
	host, port, err := net.SplitHostPort(o.sipBind)
	if err != nil {
		return compile.Boot{}, fmt.Errorf("--sip-bind %q: %w", o.sipBind, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || net.ParseIP(host) == nil {
		return compile.Boot{}, fmt.Errorf("--sip-bind %q: want <IP address>:<port>", o.sipBind)
	}
	return compile.BootFor(host, p), nil
}

func (o agentOptions) check() error {
	var errs []error
	for _, f := range []struct{ flag, value string }{
		{"--namespace (or POD_NAMESPACE)", o.namespace},
		{"--bundle-configmap", o.bundleConfigMap},
		{"--secrets", o.secrets},
		{"--device-http-listen", o.deviceHTTPListen},
		{"--sip-bind", o.sipBind},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.flag))
		}
	}
	if o.sipBind != "" {
		if _, err := o.boot(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := sounds.CheckVoice(o.soundsVoice); err != nil {
		errs = append(errs, fmt.Errorf("--sounds-voice: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}

// runAgent wires the agent's components and runs them until ctx ends (a
// clean shutdown returns nil) or one of them fails (then every other one
// stops and the error is returned, so the container restarts).
func runAgent(ctx context.Context, o agentOptions, tokens func() []string, cs kubernetes.Interface, log logr.Logger) error {
	// The prompts first: the agent is not ready without them, and Asterisk
	// starts only once it is. A failure is reported by /readyz, so the pod
	// says why it never becomes ready instead of restarting in a loop.
	if err := sounds.Install(o.soundsSrc, o.soundsVoice, o.soundsDir); err != nil {
		log.Error(err, "the sound prompts are not installed; not ready", "voice", o.soundsVoice)
	} else {
		log.Info("sound prompts installed", "voice", o.soundsVoice)
	}

	reg, err := builtin.Registry()
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	manifest, err := firmware.NewManifest(reg)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	disk := firmware.NewDisk(o.firmwareDir, manifest, log.WithName("firmware"))
	m := metrics.New()
	boot, err := o.boot()
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	conn := ami.New(ami.Config{Addr: o.amiAddress, Log: log.WithName("ami")})
	applier, err := apply.New(apply.Config{
		LiveDir: o.liveDir, FSGroup: o.fsGroup, AMI: conn, Boot: boot, Metrics: m, Log: log.WithName("apply"),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	var pages push.Pages // shared: the pusher stores, device HTTP serves, observations report
	store := observe.New(observe.Config{
		AMI: conn, Applied: applier.Applied, Index: applier.Index, Status: applier.Status,
		Firmware: disk, Pages: &pages, Metrics: m, Log: log.WithName("observe"),
	})
	in, err := intake.New(intake.Config{
		Client: cs, Namespace: o.namespace, ConfigMap: o.bundleConfigMap, Secret: o.secrets,
		Applier: applier, Log: log.WithName("intake"),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	// After the store: New re-records each persisted spend's served fetch.
	dh, err := devicehttp.New(devicehttp.Config{
		LiveDir: o.liveDir, Grants: applier, Settled: provision.SourceFunc(applier.SettledGrants),
		Recorder: store, StateDir: o.stateDir, Credentials: applier.PushCredentials, Pages: &pages,
		Log: log.WithName("devicehttp"),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	pusher, err := push.New(push.Config{
		Credentials: applier.PushCredentials, Target: store.Target, DeviceHTTPURL: "http://" + o.deviceHTTPListen,
		Pages: &pages, Log: log.WithName("push"),
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	scraper := scrape.New(scrape.Config{Targets: store, Recorder: store, Log: log.WithName("scrape")})
	h, err := agent.NewHandler(agent.Options{
		Tokens: tokens,
		Ready: func() error {
			return errors.Join(applier.Ready(), sounds.Check(o.soundsDir))
		},
		Observations: store.Document,
		Push:         pusher,
		Actions:      actions.New(actions.Config{AMI: conn, Applied: applier.Applied, Log: log.WithName("actions")}),
		Firmware:     firmware.NewUploadHandler(disk, log.WithName("firmware")),
		Metrics:      m.Handler(),
		Log:          log.WithName("api"),
	})
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	run := func(name string, f func(context.Context) error) {
		g.Go(func() error {
			if err := f(ctx); err != nil {
				return fmt.Errorf("agent: %s: %w", name, err)
			}
			return nil
		})
	}
	run("ami", conn.Run)
	run("apply", applier.Run)
	run("intake", in.Run)
	run("observe", store.Run)
	run("scrape", scraper.Run)
	run("fetch socket", func(ctx context.Context) error {
		return observe.ServeFetchSocket(ctx, o.fetchSocket, store, log.WithName("fetchlog"))
	})
	run("device HTTP", func(ctx context.Context) error { return dh.Run(ctx, o.deviceHTTPListen) })
	run("control API", func(ctx context.Context) error { return agent.Serve(ctx, o.listen, h, log.WithName("api")) })
	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// agentTokens returns the control API's token source: the token file,
// re-read whenever its Secret changes, or else the environment's fixed token.
func agentTokens(file string, log logr.Logger) (func() []string, error) {
	if file != "" {
		f, err := agent.NewTokenFile(file, agent.DefaultTokenGrace, nil, log.WithName("token"))
		if err != nil {
			return nil, fmt.Errorf("--token-file: %w", err)
		}
		return f.Tokens, nil
	}
	if tok := strings.TrimSpace(os.Getenv(agentTokenEnv)); tok != "" {
		return agent.StaticToken(tok), nil
	}
	return nil, errors.New("agent: no control API token: pass --token-file or set " + agentTokenEnv)
}
