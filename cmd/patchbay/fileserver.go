package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ChristopherJMiller/patchbay/internal/agent/fileserver"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// fileserverOptions are `patchbay fileserver`'s flags.
type fileserverOptions struct {
	liveDir     string
	firmwareDir string
	tftpListen  string
	httpListen  string
	fetchSocket string
	uid, gid    int
}

func newFileserverCmd() *cobra.Command {
	o := fileserverOptions{}
	cmd := &cobra.Command{
		Use:   "fileserver",
		Short: "Run the file server: TFTP :69 and HTTP :6970 for the phones, as an unprivileged user",
		Long: `Run the file server, the PBX pod's third container. It serves the files phones
fetch by name, unauthenticated, from the voice VLAN: the firmware store's
verified files (--firmware-dir) first, then the live config's tftp/ tree
(--live-dir), over TFTP (--tftp-listen) and Cisco's HTTP port
(--http-listen), and reports every request to the agent over --fetch-socket.

It starts as root with only NET_BIND_SERVICE, SETUID and SETGID, binds both
ports, then drops to --uid:--gid with no groups and no capabilities and checks
/proc/self/status, all before it opens a file; it exits non-zero if the drop
did not take. It reads no kubeconfig and holds no token, no AMI password and
no resolved secret beyond the phone files it serves anyway.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&o.liveDir, "live-dir", compile.LiveDir, "the live config directory (read-only)")
	f.StringVar(&o.firmwareDir, "firmware-dir", layout.FirmwareDir, "the firmware store (read-only)")
	f.StringVar(&o.tftpListen, "tftp-listen", "", "TFTP address, <pbx address>:69 (a literal IP); required")
	f.StringVar(&o.httpListen, "http-listen", "", "Cisco HTTP address, <pbx address>:6970 (a literal IP); required")
	f.StringVar(&o.fetchSocket, "fetch-socket", layout.FetchSocket, "the agent's fetch log socket")
	f.IntVar(&o.uid, "uid", layout.FileserverUID, "the uid to drop to after binding (never 0)")
	f.IntVar(&o.gid, "gid", layout.FileserverGID, "the gid to drop to after binding (never 0)")
	zapOpts := bindZapFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(zapOpts)))
		log := ctrl.Log.WithName("fileserver")
		if o.tftpListen == "" || o.httpListen == "" {
			return errors.New("fileserver: --tftp-listen and --http-listen are required")
		}
		// The manifest is code and NewDisk does no I/O: nothing here
		// opens a file before fileserver.Run has dropped privileges.
		reg, err := builtin.Registry()
		if err != nil {
			return fmt.Errorf("fileserver: %w", err)
		}
		manifest, err := firmware.NewManifest(reg)
		if err != nil {
			return fmt.Errorf("fileserver: %w", err)
		}
		log.Info("starting", "version", version.String(), "tftp", o.tftpListen, "http", o.httpListen)
		return fileserver.Run(cmd.Context(), fileserver.Config{
			LiveDir:      o.liveDir,
			Firmware:     firmware.NewDisk(o.firmwareDir, manifest, log.WithName("firmware")),
			TFTPListen:   o.tftpListen,
			HTTPListen:   o.httpListen,
			ReportSocket: o.fetchSocket,
			UID:          o.uid,
			GID:          o.gid,
			Log:          log,
		})
	}
	return cmd
}
