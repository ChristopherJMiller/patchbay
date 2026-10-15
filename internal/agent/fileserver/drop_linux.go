//go:build linux

package fileserver

import (
	"os"
	"syscall"
)

func osSyscalls() dropSyscalls {
	return dropSyscalls{
		setgroups:  syscall.Setgroups,
		setresgid:  syscall.Setresgid,
		setresuid:  syscall.Setresuid,
		readStatus: func() ([]byte, error) { return os.ReadFile("/proc/self/status") },
	}
}
