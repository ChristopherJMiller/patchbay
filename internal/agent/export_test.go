package agent

import "time"

// SetTimeouts shortens the control API's request timeout and the firmware
// route's for a test, restoring them when it ends.
func SetTimeouts(t interface{ Cleanup(func()) }, control, firmware time.Duration) {
	oldControl, oldFirmware := controlTimeout, firmwareUploadTimeout
	controlTimeout, firmwareUploadTimeout = control, firmware
	t.Cleanup(func() { controlTimeout, firmwareUploadTimeout = oldControl, oldFirmware })
}
