//go:build !linux

package fileserver

import "errors"

// The file server runs only on Linux, in the PBX pod; elsewhere the drop
// fails, so the server never serves with privilege it could not give up.
func osSyscalls() dropSyscalls {
	unsupported := errors.New("dropping privileges is supported on Linux only")
	return dropSyscalls{
		setgroups:  func([]int) error { return unsupported },
		setresgid:  func(int, int, int) error { return unsupported },
		setresuid:  func(int, int, int) error { return unsupported },
		readStatus: func() ([]byte, error) { return nil, unsupported },
	}
}
