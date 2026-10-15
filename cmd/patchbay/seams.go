package main

import (
	"net/http"

	"github.com/ChristopherJMiller/patchbay/internal/agent"
	"github.com/ChristopherJMiller/patchbay/internal/agent/actions"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/fileserver"
	"github.com/ChristopherJMiller/patchbay/internal/agent/firmware"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/observe"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// The agent's components were built in parallel against the contract
// interfaces, each implementing the others' seams by method set only
// (docs/plans/release-a.md, W2 task level). These assertions are where the
// build checks that they fit.
var (
	_ ami.Client            = (*ami.Conn)(nil)
	_ intake.Applier        = (*apply.Applier)(nil)
	_ provision.Source      = (*apply.Applier)(nil)
	_ sensor.FetchRecorder  = (*observe.Store)(nil)
	_ sensor.ScrapeRecorder = (*observe.Store)(nil)
	_ sensor.ScrapeTargets  = (*observe.Store)(nil)
	_ firmware.Store        = (*firmware.Disk)(nil)
	_ agent.Actions         = (*actions.Actions)(nil)
	_ sensor.FetchRecorder  = (*fileserver.Reporter)(nil)
	_ http.Handler          = (*devicehttp.Server)(nil)
)
