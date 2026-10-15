package apply

import "time"

// SetIntervals shortens how often Run looks at AMI and verify retries.
func (a *Applier) SetIntervals(poll, verify time.Duration) {
	a.pollEvery, a.verifyEvery = poll, verify
}
