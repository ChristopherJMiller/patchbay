package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The commands below are the CLI surface of design §6 that later releases
// fill in, in place so the shape is settled and scripts get a clear
// non-zero exit until each lands.

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
