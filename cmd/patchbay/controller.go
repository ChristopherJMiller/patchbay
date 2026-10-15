package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

func newControllerCmd(g *globalFlags) *cobra.Command {
	opts := controller.Options{}
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the controller: the only decider and the only status writer",
		Long: `Run the controller manager. It watches only its own namespace (the PBX's),
compiles the config bundle, runs each device's state machine and writes status.
Exactly one replica decides at a time when --leader-elect is set.`,
		Args: cobra.NoArgs,
	}
	cmd.Flags().StringVar(&opts.Namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"the PBX namespace to watch (default $POD_NAMESPACE)")
	cmd.Flags().StringVar(&opts.MetricsAddr, "metrics-bind-address", ":8080",
		`Prometheus metrics address; "0" disables`)
	cmd.Flags().StringVar(&opts.ProbeAddr, "health-probe-bind-address", ":8081",
		`/healthz and /readyz address; "0" disables`)
	cmd.Flags().BoolVar(&opts.LeaderElection, "leader-elect", false,
		"hold a Lease before deciding, so only one replica acts")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		log := ctrl.Log.WithName("controller")
		// Check the namespace before touching any kubeconfig.
		if opts.Namespace == "" {
			return errors.New("controller: --namespace (or POD_NAMESPACE) is required")
		}
		cfg, err := g.restConfig()
		if err != nil {
			return err
		}
		mgr, err := controller.NewManager(cfg, opts)
		if err != nil {
			return err
		}
		log.Info("starting", "version", version.String(), "namespace", opts.Namespace,
			"leaderElection", opts.LeaderElection)
		return mgr.Start(cmd.Context())
	}
	return cmd
}
