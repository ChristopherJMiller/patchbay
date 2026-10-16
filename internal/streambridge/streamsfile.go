package streambridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// maxStreamsFile bounds what the bridge reads of streams.json (a ConfigMap
// holds at most 1 MiB).
const maxStreamsFile = 4 << 20

// streamsFile is the bridge's view of streams.json (docs/plans/release-b.md
// 6.1 BR.5): read at start and every StreamsPollInterval; when its bytes
// change, decoded; a valid document replaces the loaded one, an invalid one
// is reported (BridgeStatus.StreamsError) and the loaded one stays. The
// file is opened by name each time, so the kubelet's update of a mounted
// ConfigMap (a new ..data directory, swapped in by renaming a symlink) is
// read on the next poll.
type streamsFile struct {
	path string
	m    *manager
	log  logr.Logger

	last    []byte // what the last successful read returned
	lastErr string // the failure last reported, so each is counted once
}

// poll reads the file once.
func (w *streamsFile) poll() {
	data, err := readFile(w.path, maxStreamsFile)
	if err != nil {
		w.last = nil
		w.failed(err.Error())
		return
	}
	if w.last != nil && bytes.Equal(data, w.last) {
		return
	}
	w.last = data
	d, err := streams.Decode(data)
	if err != nil {
		w.failed(err.Error())
		return
	}
	w.lastErr = ""
	w.m.setDocument(d)
	w.log.Info("streams loaded", "version", d.Version, "streams", len(d.Streams), "pools", len(d.Pools))
}

// failed reports that the newest file could not be loaded: the loaded
// document (if any) stays in use.
func (w *streamsFile) failed(msg string) {
	msg = printable(msg, api.MaxMessageBytes)
	w.m.setStreamsError(msg)
	if msg == w.lastErr {
		return
	}
	w.lastErr = msg
	w.m.metrics.loadFailures.Inc()
	w.log.Info("streams.json was not loaded; the loaded streams stay in use", "file", w.path, "error", msg)
}

// run polls every StreamsPollInterval until ctx ends, calling each after
// each poll.
func (w *streamsFile) run(ctx context.Context, clk clock.WithTicker, each func()) {
	for {
		select {
		case <-clk.After(StreamsPollInterval):
		case <-ctx.Done():
			return
		}
		w.poll()
		each()
	}
}

// readFile reads at most max bytes of path.
func readFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's own --streams-file.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist (the %s ConfigMap is not mounted yet)", path, streams.FileName)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return data, nil
}
