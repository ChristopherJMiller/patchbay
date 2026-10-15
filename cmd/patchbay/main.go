// Command patchbay is the one patchbay binary (design §6): the controller,
// the PBX agent and the stream bridge as server modes, and the client CLI
// (status, firmware, softphone, phone) that needs only a kubeconfig. It can
// be installed as kubectl-patchbay.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "patchbay:", err)
		os.Exit(1)
	}
}
