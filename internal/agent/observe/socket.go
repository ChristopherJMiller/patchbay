package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// maxSocketConns bounds the file server's connections: it holds one, and
// a few more cover its reconnects. Any beyond are closed at once.
const maxSocketConns = 8

// ServeFetchSocket listens on the unix socket at path for the file
// server's fetch events (the sensor wire format) and hands each to rec,
// until ctx is done; it returns nil then, and removes the socket.
//
// A stale socket file is removed first (anything else at path is an
// error), and the socket is made layout.FetchSocketMode, so the file
// server (another uid, no groups) can connect. Events the file server
// cannot report are dropped (fileServerMayReport): an http8088 event, one
// that claims a Device or a Version, which the file server never knows, a
// bundle path outside tftp/, a provisioning document, or a name that may
// carry a provisioning token (a compromised file server must not claim a
// softphone's provisioning fetch, nor hand a token on). A line
// that is not one valid event closes its connection; the events before it
// stand.
func ServeFetchSocket(ctx context.Context, path string, rec sensor.FetchRecorder, log logr.Logger) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return fmt.Errorf("observe: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("observe: removing the stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("observe: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("observe: listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, layout.FetchSocketMode); err != nil {
		_ = ln.Close()
		return fmt.Errorf("observe: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	log.Info("listening for the file server's fetch events", "socket", path)

	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, maxSocketConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				_ = ln.Close()
				return nil
			}
			log.Info("accepting a fetch event connection", "error", err.Error())
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			log.Info("too many fetch event connections; closing one")
			_ = conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			serveEvents(ctx, conn, rec, log)
		})
	}
}

func serveEvents(ctx context.Context, conn net.Conn, rec sensor.FetchRecorder, log logr.Logger) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()
	r := sensor.NewFetchEventReader(conn)
	for {
		e, err := r.Next()
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Info("closing a fetch event connection: a bad line", "error", err.Error())
			}
			return
		case !fileServerMayReport(e):
			// Never the File itself: it may hold a token.
			log.Info("dropping a fetch event the file server cannot report", "transport", string(e.Transport))
			continue
		}
		rec.RecordFetch(e)
	}
}

// fileServerMayReport reports whether the file server could have sent e.
// It knows no device and no version (device HTTP is in-process), it reads
// only the firmware store and the tftp/ tree, and it reports a name that
// may carry a provisioning token as sensor.ProvisioningUnknownFile only.
// Anything else is a compromised or broken file server: it must not claim
// a softphone's provisioning fetch (which would rotate its token and
// count as its config fetch) or hand a token to the observations.
func fileServerMayReport(e sensor.FetchEvent) bool {
	tree, err := compile.TreeOf(e.File)
	switch {
	case e.Transport == api.TransportHTTPDevice || e.Device != "" || e.Version != "":
		return false
	case e.BundlePath != "" && !strings.HasPrefix(e.BundlePath, compile.TreeTFTP+"/"):
		return false
	case err == nil && tree == compile.TreeProvisioning:
		return false
	case sensor.ProvisioningName(e.File): // p/ itself is not one
		return false
	}
	return true
}
