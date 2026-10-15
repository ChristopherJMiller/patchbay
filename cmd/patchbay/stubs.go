package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The commands below are the CLI surface of design §6, in place so the
// shape is settled and scripts get a clear non-zero exit until each lands.

func newStreamBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stream-bridge",
		Short: "Run the stream bridge: live audio sources to callers over AudioSocket (release B)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("stream-bridge: %w (release B)", errNotImplemented)
		},
	}
}

func newStatusCmd(_ *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show every device: state, conditions, any human action needed, and unknown phones",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("status: %w", errNotImplemented)
		},
	}
}

func newModelsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "Print the capability profile of every supported model",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("models: %w", errNotImplemented)
		},
	}
}

func newFirmwareCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firmware",
		Short: "Stage phone firmware on the PBX's firmware volume",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "load <zip>",
		Short: "Check a firmware zip against the driver manifest (SHA-256) and upload the matching files",
		Long: `Check each file in a firmware zip against the driver manifest (SHA-256)
locally, then upload only matching files to the agent through the Kubernetes
API (pods/portforward). The agent re-verifies before writing. Firmware never
goes into git.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("firmware load: %w", errNotImplemented)
		},
	})
	return cmd
}

func newSoftphoneCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "softphone",
		Short: "Work with softphones",
	}
	var reissue bool
	qr := &cobra.Command{
		Use:   "qr <name>",
		Short: "Print a softphone's one-time provisioning QR code in the terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("softphone qr: %w", errNotImplemented)
		},
	}
	qr.Flags().BoolVar(&reissue, "reissue", false, "request a fresh token (the old QR code stops working)")
	cmd.AddCommand(qr)
	return cmd
}

func newPhoneCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "phone",
		Short: "Work with hardware phones",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "resync <name>",
		Short: "Ask the controller to make a phone re-read its config",
		Long: `Set the resync request annotation on a Phone. The controller acts on it;
only the controller decides.`,
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("phone resync: %w", errNotImplemented)
		},
	})
	return cmd
}
