package streambridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// encode is d as streams.json.
func encode(t *testing.T, d streams.Document) []byte {
	t.Helper()
	b, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// kubeletDir lays out data as the kubelet mounts a ConfigMap key:
// dir/streams.json -> ..data/streams.json, ..data -> ..<version>/.
// Each call swaps in a new version as the kubelet does, by renaming a
// symlink over ..data.
func kubeletDir(t *testing.T, dir, version string, data []byte) string {
	t.Helper()
	ts := filepath.Join(dir, "..2026_10_16_"+version)
	if err := os.MkdirAll(ts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ts, streams.FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(ts), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, streams.FileName)
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		if err := os.Symlink(filepath.Join("..data", streams.FileName), link); err != nil {
			t.Fatal(err)
		}
	}
	return link
}

func TestStreamsLoad(t *testing.T) {
	d := doc(t, []streams.Pool{hdhomerun}, king5, groovesalad)
	path := kubeletDir(t, t.TempDir(), "a", encode(t, d))
	m, reg := testManager(t, newFakeSource(), nil, streams.Document{})
	if loaded, _, _, _ := m.status(); loaded {
		t.Fatal("loaded before any file")
	}
	(&streamsFile{path: path, m: m, log: logr.Discard()}).poll()
	o := m.observations()
	if o.Bridge.StreamsVersion != d.Version || o.Bridge.StreamsLoadedAt == nil || !o.Bridge.StreamsLoadedAt.Equal(epoch) ||
		o.Bridge.StreamsError != "" || len(o.Streams) != 2 || len(o.Pools) != 1 {
		t.Errorf("after the first poll: %+v", o.Bridge)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_version_info", "version="+d.Version); v != 1 {
		t.Errorf("version info %v", v)
	}
}

func TestStreamsReload(t *testing.T) {
	clk := clocktesting.NewFakeClock(epoch)
	dir := t.TempDir()
	first := doc(t, nil, groovesalad)
	path := kubeletDir(t, dir, "a", encode(t, first))
	m, reg := testManager(t, newFakeSource(), clk, streams.Document{})
	w := &streamsFile{path: path, m: m, log: logr.Discard()}
	w.poll()
	ctx, cancel := context.WithCancel(context.Background())
	polls := make(chan struct{}, 10)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.run(ctx, clk, func() { polls <- struct{}{} })
	}()
	defer func() { cancel(); <-finished }()

	second := doc(t, []streams.Pool{hdhomerun}, groovesalad, king5)
	kubeletDir(t, dir, "b", encode(t, second))
	eventually(t, "the poll timer", clk.HasWaiters)
	clk.Step(StreamsPollInterval)
	select {
	case <-polls:
	case <-time.After(5 * time.Second):
		t.Fatal("no poll")
	}
	o := m.observations().Bridge
	if o.StreamsVersion != second.Version || !o.StreamsLoadedAt.Equal(epoch.Add(StreamsPollInterval)) {
		t.Errorf("after one poll: %+v", o)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_version_info", "version="+first.Version); v != -1 {
		t.Error("the previous version's series remains")
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_version_info", "version="+second.Version); v != 1 {
		t.Errorf("version info %v", v)
	}
}

func TestStreamsInvalidKeepsOld(t *testing.T) {
	dir := t.TempDir()
	good := doc(t, nil, groovesalad)
	path := kubeletDir(t, dir, "a", encode(t, good))
	m, reg := testManager(t, newFakeSource(), nil, streams.Document{})
	w := &streamsFile{path: path, m: m, log: logr.Discard()}
	w.poll()

	bad := strings.Replace(string(encode(t, good)), `"750"`, `"7x0"`, 1)
	kubeletDir(t, dir, "b", []byte(bad))
	w.poll()
	w.poll() // the same bad file is reported once
	o := m.observations()
	if o.Bridge.StreamsVersion != good.Version || len(o.Streams) != 1 || !strings.Contains(o.Bridge.StreamsError, "number") {
		t.Errorf("after an invalid file: %+v", o.Bridge)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_load_failures_total", ""); v != 1 {
		t.Errorf("load failures %v, want 1", v)
	}

	kubeletDir(t, dir, "c", []byte("{\"version\": \"x\"} trailing"))
	w.poll()
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_load_failures_total", ""); v != 2 {
		t.Errorf("load failures %v, want 2", v)
	}
	third := doc(t, []streams.Pool{hdhomerun}, groovesalad, king5Spare)
	kubeletDir(t, dir, "d", encode(t, third))
	w.poll()
	if o := m.observations().Bridge; o.StreamsError != "" || o.StreamsVersion != third.Version {
		t.Errorf("after a valid file again: %+v", o)
	}
}

func TestStreamsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, streams.FileName)
	m, reg := testManager(t, newFakeSource(), nil, streams.Document{})
	w := &streamsFile{path: path, m: m, log: logr.Discard()}
	w.poll()
	w.poll()
	loaded, why, _, _ := m.status()
	if loaded || !strings.Contains(why, "does not exist") {
		t.Errorf("loaded %v, error %q", loaded, why)
	}
	if v := gauge(t, reg, "patchbay_stream_bridge_streams_load_failures_total", ""); v != 1 {
		t.Errorf("load failures %v, want 1 for a file missing twice", v)
	}
	d := doc(t, nil, groovesalad)
	kubeletDir(t, dir, "a", encode(t, d))
	w.poll()
	if loaded, why, _, _ := m.status(); !loaded || why != "" {
		t.Errorf("after the file appeared: loaded %v, %q", loaded, why)
	}
}

func TestStreamsTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), streams.FileName)
	if err := os.WriteFile(path, make([]byte, maxStreamsFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := testManager(t, newFakeSource(), nil, streams.Document{})
	(&streamsFile{path: path, m: m, log: logr.Discard()}).poll()
	if _, why, _, _ := m.status(); !strings.Contains(why, "larger than") {
		t.Errorf("%q", why)
	}
}
