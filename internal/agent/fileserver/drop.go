package fileserver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// dropSyscalls are the system calls a privilege drop makes: a seam, so the
// drop's order and its failure paths are tested without privilege.
type dropSyscalls struct {
	setgroups  func(gids []int) error
	setresgid  func(rgid, egid, sgid int) error
	setresuid  func(ruid, euid, suid int) error
	readStatus func() ([]byte, error) // /proc/self/status
}

// DropPrivileges gives up root for good: no supplementary groups, then
// real, effective and saved gid, then real, effective and saved uid (which
// clears every capability), and then reads /proc/self/status and checks
// with CheckDropped that the kernel agrees. Go applies the set*id calls to
// every thread of the process.
//
// The caller has bound its privileged ports and opened nothing else; it
// must exit, not serve, when this returns an error.
func DropPrivileges(uid, gid int) error {
	return drop(osSyscalls(), uid, gid)
}

func drop(sys dropSyscalls, uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return fmt.Errorf("fileserver: refusing to drop to uid %d, gid %d: not a non-root identity", uid, gid)
	}
	if err := sys.setgroups([]int{}); err != nil {
		return fmt.Errorf("fileserver: setgroups: %w", err)
	}
	if err := sys.setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("fileserver: setresgid %d: %w", gid, err)
	}
	if err := sys.setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("fileserver: setresuid %d: %w", uid, err)
	}
	status, err := sys.readStatus()
	if err != nil {
		return fmt.Errorf("fileserver: reading the process status: %w", err)
	}
	return CheckDropped(status, uid, gid)
}

// CheckDropped checks a /proc/<pid>/status text for a process that has
// given up root: all four uids (real, effective, saved, file system) are
// uid and all four gids gid, there are no supplementary groups, the
// inheritable, permitted, effective and ambient capability sets are empty,
// and no_new_privs is set, so nothing it executes could gain any back.
// The bounding set is not checked: it limits what could be regained, and
// changing it needs CAP_SETPCAP, which the process never had.
//
// Every field must be present exactly once.
func CheckDropped(procStatus []byte, uid, gid int) error {
	fields := map[string]string{}
	var errs []error
	sc := bufio.NewScanner(bytes.NewReader(procStatus))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if _, dup := fields[key]; dup {
			errs = append(errs, fmt.Errorf("%s appears twice", key))
		}
		fields[key] = strings.TrimSpace(value)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("fileserver: reading the process status: %w", err)
	}
	field := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			errs = append(errs, fmt.Errorf("%s is missing", key))
		}
		return v, ok
	}
	ids := func(key string, want int) {
		v, ok := field(key)
		if !ok {
			return
		}
		got := strings.Fields(v)
		w := strconv.Itoa(want)
		if len(got) != 4 || got[0] != w || got[1] != w || got[2] != w || got[3] != w {
			errs = append(errs, fmt.Errorf("%s is %q, want %s four times", key, v, w))
		}
	}
	ids("Uid", uid)
	ids("Gid", gid)
	if v, ok := field("Groups"); ok && v != "" {
		errs = append(errs, fmt.Errorf("the Groups field is %q, want none", v))
	}
	for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		v, ok := field(key)
		if !ok {
			continue
		}
		if caps, err := strconv.ParseUint(v, 16, 64); err != nil || caps != 0 {
			errs = append(errs, fmt.Errorf("%s is %q, want 0", key, v))
		}
	}
	if v, ok := field("NoNewPrivs"); ok && v != "1" {
		errs = append(errs, fmt.Errorf("the NoNewPrivs field is %q, want 1", v))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("fileserver: the privilege drop did not take: %w", err)
	}
	return nil
}
