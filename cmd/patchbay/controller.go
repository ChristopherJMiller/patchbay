package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// defaultSettingsFile is where the chart mounts <fullname>-settings.
const defaultSettingsFile = "/etc/patchbay/settings/" + install.SettingsKey

func newControllerCmd(g *globalFlags) *cobra.Command {
	opts := controller.Options{}
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller: the only decider and the only status writer",
		Long: `Run the controller manager. It watches only its own namespace (the PBX's).
With the install's settings (--settings-file, the chart's <fullname>-settings)
it mints the install's Secrets and compiles every Phone, Softphone, Person
and Stream into the bundle the agent applies (<fullname>-pbx-secrets, then
<fullname>-pbx-config); polls the agent's observations (--agent-url, with the
token it minted); runs each device's state machine, writes status and sends
the agent's actions; and reports unknown phones as Events. Exactly one
replica decides at a time when --leader-elect is set.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&opts.Namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"the PBX namespace to watch (default $POD_NAMESPACE)")
	f.StringVar(&opts.Fullname, "fullname", "",
		"the chart's fullname, which names every object of the install (<fullname>-pbx-config, ...); required")
	f.StringVar(&opts.SettingsFile, "settings-file", defaultSettingsFile,
		"the install's settings.json (the chart's <fullname>-settings ConfigMap, mounted)")
	f.StringVar(&opts.AgentURL, "agent-url", "",
		"the agent's control API (default http://<fullname>-agent.<namespace>.svc:9440)")
	f.StringVar(&opts.MetricsAddr, "metrics-bind-address", ":8080",
		`Prometheus metrics address; "0" disables`)
	f.StringVar(&opts.ProbeAddr, "health-probe-bind-address", ":8081",
		`/healthz and /readyz address; "0" disables`)
	f.BoolVar(&opts.LeaderElection, "leader-elect", false,
		"hold a Lease before deciding, so only one replica acts")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		log := ctrl.Log.WithName("controller")
		// Check the flags and the settings before touching any kubeconfig.
		var errs []error
		if opts.Namespace == "" {
			errs = append(errs, errors.New("--namespace (or POD_NAMESPACE) is required"))
		}
		if opts.Fullname == "" {
			errs = append(errs, errors.New("--fullname is required"))
		} else if err := install.NewNames(opts.Fullname).Validate(); err != nil {
			errs = append(errs, fmt.Errorf("--fullname: %w", err))
		}
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("controller: %w", err)
		}
		s, err := settings.Load(opts.SettingsFile)
		if err != nil {
			return fmt.Errorf("controller: %w", err)
		}
		if err := install.CheckSettings(s, install.NewNames(opts.Fullname)); err != nil {
			return fmt.Errorf("controller: %s: %w", opts.SettingsFile, err)
		}
		reg, err := builtin.Registry()
		if err != nil {
			return fmt.Errorf("controller: %w", err)
		}
		if err := s.CheckDrivers(reg); err != nil {
			return fmt.Errorf("controller: %s: %w", opts.SettingsFile, err)
		}
		cfg, err := g.restConfig()
		if err != nil {
			return err
		}
		mgr, err := controller.NewManager(cfg, opts)
		if err != nil {
			return err
		}
		if err := controller.Setup(mgr, opts, s, reg); err != nil {
			return err
		}
		log.Info("starting", "version", version.String(), "namespace", opts.Namespace,
			"fullname", opts.Fullname, "settings", opts.SettingsFile, "pbxAddress", s.PBXAddress,
			"leaderElection", opts.LeaderElection)
		return mgr.Start(cmd.Context())
	}
	return cmd
}
