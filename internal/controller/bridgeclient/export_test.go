package bridgeclient

import "time"

// SetTimeout shortens the request timeout for one test.
func SetTimeout(d time.Duration) (restore func()) {
	old := requestTimeout
	requestTimeout = d
	return func() { requestTimeout = old }
}
