// Package fileserver is `patchbay fileserver` (W2.3): TFTP :69 and HTTP
// :6970, the files phones fetch by name, unauthenticated, from the voice
// VLAN. It runs as its own container in the PBX pod, so the code that
// parses untrusted packets holds no token, no AMI password and no
// resolved secret beyond the phone files it serves anyway
// (docs/plans/release-a.md §5.2.1, "Port 69 privilege").
//
// Run binds both ports as root with only NET_BIND_SERVICE, SETUID and
// SETGID, then drops to layout.FileserverUID:FileserverGID with no groups
// and no capabilities (DropPrivileges), and checks that the kernel agrees,
// before it opens any file or socket of its own. Afterwards the live
// volume's modes keep it to what is world-readable: the tftp/ tree and the
// firmware store (layout.TreeModes).
//
// A request names a file, not a path. Both transports resolve it the same
// way: the firmware store first (a verified file of a manifest load; a
// Shared name only from the default load), then the tftp/ tree of the
// version live when the request arrived, through an os.Root, regular files
// only. Every request is reported to the agent once (sensor.FetchEvent)
// over the unix socket layout.FetchSocket (Reporter), never waiting on it.
package fileserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/netlimit"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// Config is the file server's.
type Config struct {
	// LiveDir is the live config directory (compile.LiveDir), mounted
	// read-only.
	LiveDir string
	// Firmware is the firmware store (firmware.NewDisk(layout.FirmwareDir,
	// ...)), read-only.
	Firmware firmware.Store
	// TFTPListen and HTTPListen are "<pbx address>:69" and ":6970": a
	// literal IP address and port, never a host name (resolving one would
	// read files before the drop).
	TFTPListen string
	HTTPListen string
	// ReportSocket is the agent's fetch log socket (layout.FetchSocket).
	ReportSocket string
	// Recorder receives the fetch events. Nil: a Reporter on ReportSocket,
	// started after the drop.
	Recorder sensor.FetchRecorder
	// UID and GID are what the server drops to (layout.FileserverUID,
	// layout.FileserverGID): never root.
	UID, GID int
	// Drop drops privileges. Nil: DropPrivileges.
	Drop func(uid, gid int) error
	// Now is the clock. Nil: time.Now.
	Now func() time.Time
	// Log is the logger.
	Log logr.Logger
}

func (cfg Config) check() (tftpAddr, httpAddr netip.AddrPort, err error) {
	var errs []error
	if cfg.LiveDir == "" {
		errs = append(errs, errors.New("no live directory"))
	}
	if cfg.Firmware == nil {
		errs = append(errs, errors.New("no firmware store"))
	}
	if cfg.Recorder == nil && cfg.ReportSocket == "" {
		errs = append(errs, errors.New("no recorder and no report socket"))
	}
	if cfg.UID <= 0 || cfg.GID <= 0 {
		errs = append(errs, fmt.Errorf("uid %d, gid %d: the file server never serves as root", cfg.UID, cfg.GID))
	}
	tftpAddr, err = netip.ParseAddrPort(cfg.TFTPListen)
	if err != nil {
		errs = append(errs, fmt.Errorf("TFTP listen address: %w", err))
	}
	httpAddr, err = netip.ParseAddrPort(cfg.HTTPListen)
	if err != nil {
		errs = append(errs, fmt.Errorf("HTTP listen address: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return tftpAddr, httpAddr, fmt.Errorf("fileserver: %w", err)
	}
	return tftpAddr, httpAddr, nil
}

// network is "udp4"/"tcp4" or "udp6"/"tcp6": one address family, so the
// TFTP reply source logic (IP_PKTINFO) sees the family it was bound with.
func network(proto string, a netip.AddrPort) string {
	if a.Addr().Unmap().Is4() {
		return proto + "4"
	}
	return proto + "6"
}

// Run binds TFTPListen (UDP) and HTTPListen (TCP), drops privileges, then
// serves both until ctx ends, and returns nil. Before Drop returns it
// opens no file and no other socket of its own: no live config, no
// firmware, no fetch log (its own Reporter connects only afterwards). (Go's
// TCP listen itself reads the kernel tunable /proc/sys/net/core/somaxconn
// once.) A Drop error closes both listeners and is returned: the process
// must exit non-zero rather than serve with privilege.
func Run(ctx context.Context, cfg Config) error {
	tftpAddr, httpAddr, err := cfg.check()
	if err != nil {
		return err
	}
	lc := net.ListenConfig{}
	pc, err := lc.ListenPacket(ctx, network("udp", tftpAddr), tftpAddr.String())
	if err != nil {
		return fmt.Errorf("fileserver: TFTP: %w", err)
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return fmt.Errorf("fileserver: TFTP: %T is not a UDP socket", pc)
	}
	ln, err := lc.Listen(ctx, network("tcp", httpAddr), httpAddr.String())
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("fileserver: HTTP: %w", err)
	}

	drop := cfg.Drop
	if drop == nil {
		drop = DropPrivileges
	}
	if err := drop(cfg.UID, cfg.GID); err != nil {
		_ = udp.Close()
		_ = ln.Close()
		return fmt.Errorf("fileserver: dropping privileges: %w", err)
	}
	cfg.Log.Info("privileges dropped", "uid", cfg.UID, "gid", cfg.GID)
	return serve(ctx, cfg, udp, ln, defaultTFTP)
}

// serve runs both servers on bound sockets until ctx ends or one fails.
func serve(ctx context.Context, cfg Config, udp *net.UDPConn, ln net.Listener, o tftpOptions) error {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	rec := cfg.Recorder
	if rec == nil {
		r := NewReporter(cfg.ReportSocket, cfg.Log.WithName("fetchlog"))
		rctx, stop := context.WithCancel(context.WithoutCancel(ctx))
		defer stop() // after the servers are down, so their last events queue
		wg.Go(func() { _ = r.Run(rctx) })
		rec = r
	}
	transfers := o.transfers
	if transfers <= 0 {
		transfers = maxTransfers
	}
	s := &server{
		liveDir: cfg.LiveDir, firmware: cfg.Firmware, rec: rec, now: cfg.Now, log: cfg.Log,
		slots: make(chan struct{}, transfers),
	}

	ts := s.tftpServer(o)
	hs := s.httpServer()
	errc := make(chan error, 2)
	go func() {
		if err := ts.Serve(udp); err != nil {
			errc <- fmt.Errorf("fileserver: TFTP: %w", err)
		}
	}()
	go func() {
		if err := hs.Serve(netlimit.New(ln, maxHTTPConns, maxHTTPConnsPerSource, cfg.Log)); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("fileserver: HTTP: %w", err)
		}
	}()
	cfg.Log.Info("serving", "tftp", udp.LocalAddr().String(), "http", ln.Addr().String())

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutdownCtx)
	ts.Shutdown() // closes udp and waits for the transfers in flight
	_ = udp.Close()
	return err
}
