package controller

// The tests of this package build a manager per test and may run the
// package more than once in one process (go test -count=N), so each Setup
// registers controller names already used: turn off controller-runtime's
// process-wide check for them, never for the binary.
func init() { skipNameValidation = true }
