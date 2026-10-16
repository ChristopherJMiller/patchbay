package main

import (
	"flag"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// globalFlags are the flags every subcommand shares.
type globalFlags struct {
	kubeconfig string
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:   "patchbay",
		Short: "A Kubernetes-native PBX: Asterisk 22 plus a Go operator",
		Long: `patchbay runs a home phone system on Kubernetes.

Server modes (run by the Helm chart):
  controller      decides: compiles config, drives each device's state machine, writes status
  agent           the PBX pod's sidecar: applies config, watches phones, talks to Asterisk
  fileserver      the PBX pod's TFTP and HTTP :6970 server, unprivileged after binding
  stream-bridge   plays live audio sources (TV, internet radio) to callers of a Stream

Client commands need only a kubeconfig.`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("patchbay {{.Version}}\n")
	root.PersistentFlags().StringVar(&g.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (client commands: default $KUBECONFIG, then ~/.kube/config, never the in-cluster config; server modes: $KUBECONFIG, the in-cluster config, then ~/.kube/config)")

	root.AddCommand(
		newControllerCmd(g),
		newAgentCmd(g),
		newFileserverCmd(),
		newStreamBridgeCmd(),
		newStatusCmd(g),
		newModelsCmd(),
		newFirmwareCmd(g),
		newSoftphoneCmd(g),
		newPhoneCmd(g),
		newVersionCmd(),
	)
	return root
}

// restConfig loads the server modes' Kubernetes client config: the
// --kubeconfig file when given, otherwise controller-runtime's usual chain
// ($KUBECONFIG, the in-cluster config, ~/.kube/config). The client commands
// never use it: cli.NewEnv never falls back to the in-cluster config.
func (g *globalFlags) restConfig() (*rest.Config, error) {
	if g.kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", g.kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("loading kubeconfig %s: %w", g.kubeconfig, err)
		}
		return cfg, nil
	}
	return ctrl.GetConfig()
}

// bindZapFlags adds the controller-runtime logging flags (--zap-devel,
// --zap-log-level, ...) to cmd and returns the options they fill.
func bindZapFlags(cmd *cobra.Command) *zap.Options {
	opts := &zap.Options{}
	fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	opts.BindFlags(fs)
	cmd.Flags().AddGoFlagSet(fs)
	return opts
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "patchbay", version.String())
			return err
		},
	}
}
