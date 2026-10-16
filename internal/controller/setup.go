package controller

import (
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentclient"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/bridgeclient"
	"github.com/ChristopherJMiller/patchbay/internal/controller/bundle"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/controller/discovery"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/controller/numbers"
	"github.com/ChristopherJMiller/patchbay/internal/controller/poller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/secrets"
	"github.com/ChristopherJMiller/patchbay/internal/controller/streamstate"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
)

// The controller's components were built in parallel against the contract
// interfaces (internal/controller/install and agentview; for streams,
// internal/streambridge/api), each satisfying the others' seams by method
// set only (docs/plans/release-a.md §6.2, docs/plans/release-b.md §6.2).
// These assertions are where the build checks that they fit.
var (
	_ agentview.Actor                = (*agentclient.Client)(nil)
	_ poller.Source                  = (*agentclient.Client)(nil)
	_ agentclient.TokenSource        = (*secrets.Manager)(nil)
	_ agentview.Observer             = (*poller.Poller)(nil)
	_ agentview.Observer             = (*agentview.Holder)(nil)
	_ manager.LeaderElectionRunnable = (*poller.Poller)(nil)
	_ agentview.SnapshotFunc         = (*discovery.Reporter)(nil).Report
	_ streamstate.Source             = (*bridgeclient.Client)(nil)
	_ manager.LeaderElectionRunnable = (*streamstate.Poller)(nil)
)

// Setup wires the controller into mgr (built by NewManager with the same
// opts): the install's Secrets and its compiled bundle (pbx-secrets, then
// pbx-config, with <fullname>-streams written first), the poller of the
// agent's observations, the Phone, Softphone and Person reconcilers, the
// Stream reconciler with its poller of the stream bridge's observations,
// discovery and the metrics.
//
// The pieces meet in three places, so no component hands state to another
// in memory except through them:
//   - the cluster: the bundle reconciler writes pbx-config and pbx-secrets,
//     which the device reconcilers, the poller and discovery read back
//     (install.ReadCompiled);
//   - an agentview.Holder: the poller publishes each snapshot of the
//     agent's document in it, and the device reconcilers and the metrics
//     read it (the poller is never a reconciler's dependency, so the order
//     here has no cycle);
//   - agentview.Changes: the poller enqueues the devices whose
//     observations changed.
//
// The Stream reconciler (internal/controller/streamstate) keeps its own
// poller of the stream bridge, and reads what the bundle reconciler
// compiled from <fullname>-streams (install.ReadStreams). It polls only
// when the settings name a stream bridge.
//
// Everything that decides or writes runs only on the leader: the
// reconcilers and the poller. s is the install's settings (settings.Load,
// then install.CheckSettings; Setup checks it against reg too,
// compile.Settings.CheckDrivers); reg the driver registry
// (builtin.Registry()).
func Setup(mgr ctrl.Manager, opts Options, s compile.Settings, reg *drivers.Registry) error {
	names := install.NewNames(opts.Fullname)
	if err := names.Validate(); err != nil {
		return fmt.Errorf("controller: --fullname: %w", err)
	}
	if err := install.CheckSettings(s, names); err != nil {
		return fmt.Errorf("controller: settings: %w", err)
	}
	if reg == nil {
		return errors.New("controller: no driver registry")
	}
	if err := s.CheckDrivers(reg); err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	if opts.Namespace == "" {
		return errors.New("controller: namespace is required")
	}
	if opts.AgentURL == "" {
		opts.AgentURL = install.AgentURL(names, opts.Namespace, agentapi.DefaultPort)
	}
	if opts.StreamBridgeURL == "" {
		opts.StreamBridgeURL = install.StreamBridgeURL(names, opts.Namespace, bridgeapi.DefaultControlPort)
	}
	if s.StreamBridge != "" {
		// Checked here, before anything is wired; built below with its logger.
		if _, err := bridgeclient.New(bridgeclient.Config{URL: opts.StreamBridgeURL}); err != nil {
			return fmt.Errorf("controller: --stream-bridge-url: %w", err)
		}
	}
	registerer := opts.Registerer
	if registerer == nil {
		registerer = ctrlmetrics.Registry
	}

	log := mgr.GetLogger().WithName("patchbay")
	c := mgr.GetClient()
	ns := opts.Namespace
	holder := &agentview.Holder{}
	changes := agentview.NewChanges(agentview.DefaultChangesBuffer)
	recorder := mgr.GetEventRecorder(install.EventRecorderName)

	m, err := metrics.New(metrics.Config{Registerer: registerer, Reader: c, Namespace: ns, Observer: holder})
	if err != nil {
		return fmt.Errorf("controller: metrics: %w", err)
	}
	sec, err := secrets.New(secrets.Config{
		Client: c, Namespace: ns, Names: names, Registry: reg, Log: log.WithName("secrets"),
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	agent, err := agentclient.New(agentclient.Config{URL: opts.AgentURL, Tokens: sec, Log: log.WithName("agentclient")})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	reporter := discovery.New(discovery.Config{
		Reader: c, Namespace: ns, Names: names, Recorder: recorder, Log: log.WithName("discovery"),
	})
	p, err := poller.New(poller.Config{
		Source: agent, Reader: c, Namespace: ns, Names: names, Holder: holder, Changes: changes,
		OnSnapshot: []agentview.SnapshotFunc{reporter.Report}, Metrics: m, Log: log.WithName("poller"),
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	if err := mgr.Add(p); err != nil {
		return fmt.Errorf("controller: adding the poller: %w", err)
	}

	b, err := bundle.New(bundle.Config{
		Client: c, Namespace: ns, Names: names, Settings: s, Registry: reg, Secrets: sec,
		Recorder: recorder, Metrics: m, Log: log.WithName("bundle"),
	})
	if err != nil {
		return fmt.Errorf("controller: %w", err)
	}
	if err := b.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: the bundle reconciler: %w", err)
	}
	if err := devices.Setup(mgr, devices.Config{
		Client: c, Namespace: ns, Names: names, Settings: s, Registry: reg,
		Observer: holder, Actor: agent, Changes: changes, Recorder: recorder, Log: log.WithName("devices"),
	}); err != nil {
		return fmt.Errorf("controller: the device reconcilers: %w", err)
	}
	if err := numbers.Setup(mgr, numbers.Config{
		Client: c, Namespace: ns, Names: names, Settings: s, Recorder: recorder, Log: log.WithName("numbers"),
	}); err != nil {
		return fmt.Errorf("controller: the Person reconciler: %w", err)
	}
	// The stream bridge is polled only when the settings name one
	// (streamBridge.enabled in the chart); without it every routed Stream
	// is Unavailable and nothing is polled.
	var bridge streamstate.Source
	if s.StreamBridge != "" {
		bc, err := bridgeclient.New(bridgeclient.Config{URL: opts.StreamBridgeURL, Log: log.WithName("bridgeclient")})
		if err != nil {
			return fmt.Errorf("controller: --stream-bridge-url: %w", err)
		}
		bridge = bc
	}
	if err := streamstate.Setup(mgr, streamstate.Config{
		Client: c, Namespace: ns, Names: names, Settings: s, Bridge: bridge,
		Recorder: recorder, Metrics: m, Log: log.WithName("streams"),
	}); err != nil {
		return fmt.Errorf("controller: the Stream reconciler: %w", err)
	}
	return nil
}
