package cisco79x5

import (
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Internals the external tests (package cisco79x5_test) check.

var (
	Fold             = fold
	RunsLoad         = runsLoad
	DialTemplateWith = dialTemplate
)

const (
	SettingsAccessRestricted = settingsAccessRestricted
	DialTemplateLiteralStar  = dialTemplateLiteralStar
	ReasonNoRuleMatched      = reasonNoRuleMatched
)

// NewWithLoads is a driver with another manifest, e.g. the Cisco load with
// every digest pending, so a test can decide for a load whose digests are
// not known yet.
func NewWithLoads(loads []drivers.FirmwareLoad) *Driver {
	return &Driver{loads: cloneLoads(loads)}
}

// WithTweak returns a copy of d whose profiles are changed by f.
func (d *Driver) WithTweak(f func(*drivers.Profile)) *Driver {
	return &Driver{loads: cloneLoads(d.loads), tweak: f}
}

// RuleStates lists the states of the rule table, in order.
func RuleStates() []drivers.State {
	out := make([]drivers.State, len(rules))
	for i, r := range rules {
		out[i] = r.state
	}
	return out
}

// MatchingRows lists every row (numbered from 1) whose predicate holds for
// the input, in table order; Decide uses the first.
func (d *Driver) MatchingRows(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) []int {
	in := d.newInput(dev, obs, s, now)
	var out []int
	for i, r := range rules {
		if r.match(in) {
			out = append(out, i+1)
		}
	}
	return out
}
