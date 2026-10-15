package agentclient

import "time"

// SetTimeouts shortens the per-call timeouts for one test.
func SetTimeouts(observations, action time.Duration) (restore func()) {
	o, a := observationsTimeout, actionTimeout
	observationsTimeout, actionTimeout = observations, action
	return func() { observationsTimeout, actionTimeout = o, a }
}
