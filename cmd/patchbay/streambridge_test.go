package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// TestStreamBridgeFlags: the command's flags are the contract's names with
// its defaults (the chart passes them; test/chart checks the chart against
// the same constants), and it takes the zap flags.
func TestStreamBridgeFlags(t *testing.T) {
	cmd := newStreamBridgeCmd()
	for name, def := range map[string]string{
		streambridge.FlagListen:        streambridge.DefaultListen,
		streambridge.FlagControlListen: streambridge.DefaultControlListen,
		streambridge.FlagStreamsFile:   streambridge.DefaultStreamsFile,
		streambridge.FlagFFmpeg:        streambridge.DefaultFFmpeg,
		"zap-log-level":                "",
	} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("no --%s", name)
			continue
		}
		if def != "" && f.DefValue != def {
			t.Errorf("--%s defaults to %q, want %q", name, f.DefValue, def)
		}
	}
}

func TestStreamBridgeRefusesABadAddress(t *testing.T) {
	_, err := run(t, context.Background(), "stream-bridge", "--"+streambridge.FlagListen, "nonsense")
	if err == nil || !strings.Contains(err.Error(), "--"+streambridge.FlagListen) {
		t.Fatalf("error = %v, want --%s named", err, streambridge.FlagListen)
	}
}

// TestStreamBridgeRuns runs the command as the chart does, on loopback
// ports, with a stand-in for ffmpeg (a script that only answers -version)
// and a generated streams.json: it becomes ready, its observations report
// that file and that decoder, and it stops cleanly when its context ends.
// It never reads a kubeconfig (run's does not exist).
func TestStreamBridgeRuns(t *testing.T) {
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\necho 'ffmpeg version not-a-real-ffmpeg'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	doc, err := streams.Document{Streams: map[string]streams.Stream{
		streams.UUID("radio"): {Name: "radio", Number: "750", Source: "http://127.0.0.1:1/radio"},
	}}.Sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := doc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, streams.FileName)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	control := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// As run does, but the command runs in its own goroutine.
	tmp := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(tmp, "no-such-kubeconfig"))
	t.Setenv("HOME", tmp)
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"stream-bridge",
		"--" + streambridge.FlagListen, freeAddr(t),
		"--" + streambridge.FlagControlListen, control,
		"--" + streambridge.FlagStreamsFile, file,
		"--" + streambridge.FlagFFmpeg, ffmpeg,
		"--zap-log-level", "error", "--kubeconfig", missingKubeconfig(t)})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	get := func(path string) (int, []byte, error) {
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
		defer rcancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+control+path, nil)
		if err != nil {
			return 0, nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body, err := get(bridgeapi.PathReadyz)
		if err == nil && code == http.StatusOK {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("stream-bridge exited before it was ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready within 15 s: %d %s %v", code, body, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, body, err := get(bridgeapi.PathObservations)
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET %s: %d %s %v", bridgeapi.PathObservations, code, body, err)
	}
	var obs bridgeapi.Observations
	if err := json.Unmarshal(body, &obs); err != nil {
		t.Fatal(err)
	}
	if obs.Bridge.FFmpeg != "ffmpeg version not-a-real-ffmpeg" || obs.Bridge.StreamsVersion != doc.Version ||
		len(obs.Streams) != 1 || obs.Streams[0].Name != "radio" {
		t.Errorf("observations %+v, streams %+v", obs.Bridge, obs.Streams)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stream-bridge stopped with %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream-bridge did not stop within 10 s of its context ending")
	}
}

// TestControllerStreamBridgeURL: the controller takes the bridge's control
// API as --stream-bridge-url, the flag the chart passes, empty by default
// (controller.Setup then derives the bridge Service's address).
func TestControllerStreamBridgeURL(t *testing.T) {
	f := newControllerCmd(&globalFlags{}).Flags().Lookup("stream-bridge-url")
	if f == nil || f.DefValue != "" {
		t.Fatalf("--stream-bridge-url: %+v", f)
	}
}
