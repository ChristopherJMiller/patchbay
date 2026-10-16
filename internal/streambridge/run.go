package streambridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"
	"k8s.io/utils/clock"

	"github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// Run is the bridge: it serves AudioSocket on o.Listen and the control
// API on o.ControlListen until ctx ends, then stops accepting, sends every
// caller a hangup frame, stops every decoder and returns nil. It fails at
// once on a bad Options or a listener that cannot bind. Zero fields take
// the Default* values; a nil Source is NewFFmpegSource(o.FFmpeg, o.CAFile,
// o.Log); nil Registerer and Gatherer are a registry of the bridge's own
// (with the Go and process collectors); a Registerer that is also a
// Gatherer serves as both.
func Run(ctx context.Context, o Options) error { return run(ctx, o, nil) }

// versioner is a Source that can say which decoder it runs
// (FFmpegSource): readiness waits for it.
type versioner interface {
	Version(ctx context.Context) (string, error)
}

// run is Run; bound, if not nil, is told the listeners' addresses once
// both are bound (tests listen on port 0).
func run(ctx context.Context, o Options, bound func(audio, control net.Addr)) error {
	o, err := o.complete()
	if err != nil {
		return err
	}
	log := o.Log
	m := newManager(o.Source, o.Clock, log, o.Clock.Now())
	if err := m.register(o.Registerer); err != nil {
		return err
	}
	var lc net.ListenConfig
	audioLn, err := lc.Listen(ctx, "tcp", o.Listen)
	if err != nil {
		return fmt.Errorf("stream-bridge: listen on %s (AudioSocket): %w", o.Listen, err)
	}
	controlLn, err := lc.Listen(ctx, "tcp", o.ControlListen)
	if err != nil {
		_ = audioLn.Close()
		return fmt.Errorf("stream-bridge: listen on %s (control API): %w", o.ControlListen, err)
	}
	if bound != nil {
		bound(audioLn.Addr(), controlLn.Addr())
	}
	log.Info("stream bridge starting", "version", version.String(), "audioSocket", audioLn.Addr().String(),
		"control", controlLn.Addr().String(), "streamsFile", o.StreamsFile)

	file := &streamsFile{path: o.StreamsFile, m: m, log: log}
	file.poll()
	ver, needFFmpeg := o.Source.(versioner)
	checkFFmpeg := func() {
		if !needFFmpeg {
			return
		}
		if _, _, have, _ := m.status(); have != "" {
			return
		}
		v, err := ver.Version(ctx)
		if err != nil {
			m.setFFmpeg("", printable(err.Error(), api.MaxMessageBytes))
			log.Info("the decoder is not usable yet", "error", err.Error())
			return
		}
		m.setFFmpeg(v, "")
		log.Info("decoder found", "ffmpeg", v)
	}
	checkFFmpeg()

	srv := &server{m: m, clock: o.Clock, log: log}
	ctl := newControl(m, o.Gatherer, needFFmpeg)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.serve(gctx, audioLn) })
	g.Go(func() error { return serveControl(gctx, controlLn, ctl.handler(), log) })
	g.Go(func() error {
		file.run(gctx, o.Clock, checkFFmpeg)
		return nil
	})
	err = g.Wait()
	m.stopAll()
	if err != nil {
		return err
	}
	log.Info("stream bridge stopped")
	return nil
}

// complete fills o's zero fields with their defaults and checks it.
func (o Options) complete() (Options, error) {
	o.Listen = cmp.Or(o.Listen, DefaultListen)
	o.ControlListen = cmp.Or(o.ControlListen, DefaultControlListen)
	o.StreamsFile = cmp.Or(o.StreamsFile, DefaultStreamsFile)
	o.FFmpeg = cmp.Or(o.FFmpeg, DefaultFFmpeg)
	for flag, addr := range map[string]string{FlagListen: o.Listen, FlagControlListen: o.ControlListen} {
		if err := checkAddress(addr); err != nil {
			return o, fmt.Errorf("stream-bridge: --%s %q: %w", flag, addr, err)
		}
	}
	if _, port, _ := net.SplitHostPort(o.Listen); o.Listen == o.ControlListen && port != "0" {
		return o, fmt.Errorf("stream-bridge: --%s and --%s are both %q", FlagListen, FlagControlListen, o.Listen)
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Source == nil {
		o.Source = NewFFmpegSource(o.FFmpeg, o.CAFile, o.Log)
	}
	switch {
	case o.Registerer == nil && o.Gatherer == nil:
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		o.Registerer, o.Gatherer = reg, reg
	case o.Gatherer == nil:
		g, ok := o.Registerer.(prometheus.Gatherer)
		if !ok {
			return o, errors.New("stream-bridge: Options.Registerer is set but Options.Gatherer is not")
		}
		o.Gatherer = g
	case o.Registerer == nil:
		return o, errors.New("stream-bridge: Options.Gatherer is set but Options.Registerer is not")
	}
	return o, nil
}

// checkAddress accepts host:port with a port of 0-65535 (an empty host
// listens on every address).
func checkAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("port %q is not a number from 0 to 65535", port)
	}
	return nil
}
