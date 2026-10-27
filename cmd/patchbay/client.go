package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// The client commands are thin cobra glue over internal/cli, which holds
// their behaviour and its tests (docs/plans/release-a.md §6.2.7). Each one
// that talks to the cluster takes -n/--namespace and --instance, and loads
// only the kubeconfig the root's --kubeconfig names, else $KUBECONFIG,
// else ~/.kube/config: never the in-cluster config (cli.NewEnv).

// clientFlags binds -n/--namespace and --instance on cmd.
func clientFlags(cmd *cobra.Command) *cli.ClientFlags {
	f := &cli.ClientFlags{}
	cmd.Flags().StringVarP(&f.Namespace, "namespace", "n", "",
		"the PBX's namespace (default: the kubeconfig context's, else default)")
	cmd.Flags().StringVar(&f.Instance, "instance", "",
		"the Helm release, when the namespace holds more than one PBX")
	return f
}

// env builds the command's cli.Env.
func env(cmd *cobra.Command, g *globalFlags, f *cli.ClientFlags) (*cli.Env, error) {
	return cli.NewEnv(g.kubeconfig, *f, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func newStatusCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show every device: state, conditions, any human action needed, and unknown phones",
		Long: `List every Phone, Softphone, Person and Stream with its state, each condition
that is not healthy and any human action, then ask the agent (through a
port-forward to the PBX pod) for its own state and the phones that asked
for config without a Phone. It exits 0 whenever the resources could be
listed: an agent it cannot reach is a line in the report.`,
		Args: cobra.NoArgs,
	}
	f := clientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		e, err := env(cmd, g, f)
		if err != nil {
			return err
		}
		return cli.Status(cmd.Context(), e)
	}
	return cmd
}

func newModelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "Print the capability profile of every supported model",
		Long: `Print each driver's models: hardware, provisioning, the PJSIP endpoint
options, features, and every firmware load with its files and SHA-256s
("pending" for a file whose digest no release has filled in yet). It needs
no cluster.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := builtin.Registry()
			if err != nil {
				return err
			}
			return cli.Models(cmd.OutOrStdout(), reg)
		},
	}
}

func newFirmwareCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firmware",
		Short: "Stage phone firmware on the PBX's firmware volume",
	}
	load := &cobra.Command{
		Use:   "load <zip>",
		Short: "Check a firmware zip against the driver manifest (SHA-256) and upload the matching files",
		Long: `Check each file in a firmware zip against every driver's manifest locally:
by base name (case-sensitive, wherever it sits in the zip) and SHA-256.
A file whose digest the manifest does not have yet, whose digest differs,
or whose name differs only in case is refused here and never uploaded.
The matching files are then uploaded to the agent through the Kubernetes
API (pods/portforward), which verifies each again before storing it.
Firmware never goes into git. It exits non-zero if any file failed or
none matched.`,
		Args: cobra.ExactArgs(1),
	}
	f := clientFlags(load)
	load.RunE = func(cmd *cobra.Command, args []string) error {
		e, err := env(cmd, g, f)
		if err != nil {
			return err
		}
		return cli.FirmwareLoad(cmd.Context(), e, args[0])
	}
	cmd.AddCommand(load)
	return cmd
}

func newSoftphoneCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "softphone",
		Short: "Work with softphones",
	}
	var reissue bool
	qr := &cobra.Command{
		Use:   "qr <name>",
		Short: "Print a softphone's one-time provisioning QR code in the terminal",
		Long: `Print the Softphone's provisioning URL and its QR code; scan it in Linphone
(Assistant, Fetch remote configuration). The URL works once. With
--reissue, ask the controller for a new one first (the old code stops
working) and wait up to 30 s until the agent serves it.`,
		Args: cobra.ExactArgs(1),
	}
	f := clientFlags(qr)
	qr.Flags().BoolVar(&reissue, "reissue", false, "request a fresh token (the old QR code stops working)")
	qr.RunE = func(cmd *cobra.Command, args []string) error {
		e, err := env(cmd, g, f)
		if err != nil {
			return err
		}
		return cli.SoftphoneQR(cmd.Context(), e, args[0], reissue)
	}
	cmd.AddCommand(qr)
	return cmd
}

func newPhoneCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "phone",
		Short: "Work with hardware phones",
	}
	resync := &cobra.Command{
		Use:   "resync <name>",
		Short: "Ask the controller to make a phone re-read its config",
		Long: `Set the resync request annotation on a Phone. The controller acts on it once,
and only for a Registered or ConfigStale phone: only the controller
decides.`,
		Args: cobra.ExactArgs(1),
	}
	f := clientFlags(resync)
	resync.RunE = func(cmd *cobra.Command, args []string) error {
		e, err := env(cmd, g, f)
		if err != nil {
			return err
		}
		return cli.PhoneResync(cmd.Context(), e, args[0])
	}
	cmd.AddCommand(resync)

	var push cli.TestPush
	testPush := &cobra.Command{
		Use:   "test-push <name>",
		Short: "Push a test message page to a phone, through the agent",
		Long: `A diagnostic for "do pushes work?": ask the agent directly (through a
port-forward to the PBX pod, as firmware load does) to push to a Cisco
phone, outside the controller. By default it pushes a page of one message,
with its time in the install's timezone. --clear returns the phone to idle.
--execute pushes raw Init:, Key:, SoftKey: or Play: URIs (at most 3) and
nothing else; it cannot be combined with --text, --beep or --clear.`,
		Args: cobra.ExactArgs(1),
	}
	tf := clientFlags(testPush)
	testPush.Flags().StringVar(&push.Text, "text", cli.DefaultPushText, "the message to show")
	testPush.Flags().BoolVar(&push.Beep, "beep", false, "chime ahead of the page")
	testPush.Flags().BoolVar(&push.Clear, "clear", false, "return the phone to idle and drop the page")
	testPush.Flags().StringArrayVar(&push.Execute, "execute", nil, "a raw URI to push (repeatable)")
	testPush.MarkFlagsMutuallyExclusive("clear", "text")
	testPush.RunE = func(cmd *cobra.Command, args []string) error {
		if len(push.Execute) > 0 && (cmd.Flags().Changed("text") || push.Beep || push.Clear) {
			return errors.New("--execute cannot be combined with --text, --beep or --clear")
		}
		e, err := env(cmd, g, tf)
		if err != nil {
			return err
		}
		return cli.PhoneTestPush(cmd.Context(), e, args[0], push)
	}
	cmd.AddCommand(testPush)
	return cmd
}
