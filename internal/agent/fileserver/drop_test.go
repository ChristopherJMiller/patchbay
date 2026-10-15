package fileserver

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// goodStatus is /proc/self/status of a file server that dropped to
// 65533:65533 in a container with NET_BIND_SERVICE, SETUID and SETGID in
// its bounding set and allowPrivilegeEscalation false.
const goodStatus = `Name:	patchbay
Umask:	0022
State:	R (running)
Tgid:	7
Ngid:	0
Pid:	7
PPid:	0
TracerPid:	0
Uid:	65533	65533	65533	65533
Gid:	65533	65533	65533	65533
FDSize:	64
Groups:
NStgid:	7
NSpid:	7
VmPeak:	 1262116 kB
Threads:	6
SigQ:	0/62498
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	00000000000004c0
CapAmb:	0000000000000000
NoNewPrivs:	1
Seccomp:	2
Seccomp_filters:	1
Speculation_Store_Bypass:	thread vulnerable
Cpus_allowed_list:	0-7
voluntary_ctxt_switches:	3
`

func replaceLine(status, key, line string) string {
	var out []string
	for l := range strings.SplitSeq(status, "\n") {
		if strings.HasPrefix(l, key+":") {
			if line == "" {
				continue
			}
			l = line
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestCheckDropped: every field the drop must have changed is checked, and
// each wrong once fails the check.
func TestCheckDropped(t *testing.T) {
	if err := CheckDropped([]byte(goodStatus), 65533, 65533); err != nil {
		t.Fatalf("good status: %v", err)
	}
	if err := CheckDropped([]byte(goodStatus), 65532, 65533); err == nil {
		t.Error("another uid than asked: want an error")
	}
	if err := CheckDropped([]byte(goodStatus), 65533, 65532); err == nil {
		t.Error("another gid than asked: want an error")
	}
	for _, tc := range []struct{ key, line string }{
		{"Uid", "Uid:\t0\t65533\t65533\t65533"},        // real uid still root
		{"Uid", "Uid:\t65533\t0\t65533\t65533"},        // effective
		{"Uid", "Uid:\t65533\t65533\t0\t65533"},        // saved: setuid(0) would succeed
		{"Uid", "Uid:\t65533\t65533\t65533\t0"},        // file system uid
		{"Uid", "Uid:\t65533\t65533\t65533"},           // short
		{"Uid", "Uid:\t65533\t65533\t65533\t65533\t1"}, // long
		{"Uid", ""}, // missing
		{"Gid", "Gid:\t0\t65533\t65533\t65533"},
		{"Gid", "Gid:\t65533\t0\t65533\t65533"},
		{"Gid", "Gid:\t65533\t65533\t0\t65533"},
		{"Gid", "Gid:\t65533\t65533\t65533\t0"},
		{"Gid", ""},
		{"Groups", "Groups:\t1000 "}, // the pod's fsGroup
		{"Groups", "Groups:\t0"},
		{"Groups", ""},
		{"CapInh", "CapInh:\t0000000000000400"},
		{"CapInh", ""},
		{"CapPrm", "CapPrm:\t0000000000000400"},
		{"CapPrm", "CapPrm:\tzz"},
		{"CapPrm", ""},
		{"CapEff", "CapEff:\t00000000000004c0"},
		{"CapEff", ""},
		{"CapAmb", "CapAmb:\t0000000000000400"},
		{"CapAmb", ""},
		{"NoNewPrivs", "NoNewPrivs:\t0"},
		{"NoNewPrivs", ""},
	} {
		status := replaceLine(goodStatus, tc.key, tc.line)
		if status == goodStatus {
			t.Fatalf("%s: the sample did not change", tc.key)
		}
		if err := CheckDropped([]byte(status), 65533, 65533); err == nil {
			t.Errorf("%q: CheckDropped = nil, want an error", tc.line)
		} else if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%q: the error %q does not name %s", tc.line, err, tc.key)
		}
	}
	// A field twice: which one would the kernel mean? Refuse.
	if err := CheckDropped([]byte(goodStatus+"CapEff:\t0000000000000400\n"), 65533, 65533); err == nil {
		t.Error("a field twice: want an error")
	}
	// CapBnd is not ours to change (it needs CAP_SETPCAP): not checked.
	if err := CheckDropped([]byte(replaceLine(goodStatus, "CapBnd", "CapBnd:\t000001ffffffffff")), 65533, 65533); err != nil {
		t.Errorf("a full bounding set: %v", err)
	}
	if err := CheckDropped(nil, 65533, 65533); err == nil {
		t.Error("an empty status: want an error")
	}
}

// fakeSys records the drop's system calls in order, and can fail any.
type fakeSys struct {
	calls  []string
	fail   string // the call that fails
	status string // what /proc/self/status reads
}

func (f *fakeSys) syscalls() dropSyscalls {
	call := func(name string, args ...any) error {
		f.calls = append(f.calls, name+fmt.Sprint(args...))
		if name == f.fail {
			return errors.New(name + " failed")
		}
		return nil
	}
	return dropSyscalls{
		setgroups: func(gids []int) error { return call("setgroups", gids) },
		setresgid: func(r, e, s int) error { return call("setresgid", []int{r, e, s}) },
		setresuid: func(r, e, s int) error { return call("setresuid", []int{r, e, s}) },
		readStatus: func() ([]byte, error) {
			if err := call("readStatus"); err != nil {
				return nil, err
			}
			return []byte(f.status), nil
		},
	}
}

// TestDropOrder: groups go first, then the gid (which needs CAP_SETGID),
// then the uid (which gives up the rest), and only then the check.
func TestDropOrder(t *testing.T) {
	f := &fakeSys{status: goodStatus}
	if err := drop(f.syscalls(), 65533, 65533); err != nil {
		t.Fatal(err)
	}
	want := []string{"setgroups[]", "setresgid[65533 65533 65533]", "setresuid[65533 65533 65533]", "readStatus"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

// TestDropFails: any step failing, or a status that shows the drop did not
// take, is an error, and nothing after a failed step runs.
func TestDropFails(t *testing.T) {
	steps := []string{"setgroups", "setresgid", "setresuid", "readStatus"}
	for i, step := range steps {
		f := &fakeSys{status: goodStatus, fail: step}
		err := drop(f.syscalls(), 65533, 65533)
		if err == nil || !strings.Contains(err.Error(), step+" failed") {
			t.Errorf("%s failing: drop = %v, want its error", step, err)
		}
		if len(f.calls) != i+1 {
			t.Errorf("%s failing: calls = %q, want nothing after it", step, f.calls)
		}
	}
	f := &fakeSys{status: replaceLine(goodStatus, "CapEff", "CapEff:\t0000000000000400")}
	if err := drop(f.syscalls(), 65533, 65533); err == nil || !strings.Contains(err.Error(), "CapEff") {
		t.Errorf("capabilities left: drop = %v, want the check's error", err)
	}
	for _, ids := range [][2]int{{0, 65533}, {65533, 0}, {-1, 65533}, {65533, -1}} {
		f := &fakeSys{status: goodStatus}
		if err := drop(f.syscalls(), ids[0], ids[1]); err == nil || len(f.calls) != 0 {
			t.Errorf("drop to %v = %v after %q, want a refusal before any call", ids, err, f.calls)
		}
	}
}

// TestCheckDroppedReadsTheKernelsFields: this process's own status has
// every field CheckDropped needs, spelled as it expects (whatever else is
// wrong with it: it is not a dropped file server).
func TestCheckDroppedReadsTheKernelsFields(t *testing.T) {
	status, err := osSyscalls().readStatus()
	if err != nil {
		t.Skipf("no /proc/self/status: %v", err)
	}
	err = CheckDropped(status, 65533, 65533)
	if err == nil {
		t.Fatal("the test process passes as a dropped file server")
	}
	if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "twice") {
		t.Errorf("the kernel's status does not parse: %v", err)
	}
}

// TestDropPrivilegesWithoutPrivilege: the real drop, run without root,
// fails at its first step and says so.
func TestDropPrivilegesWithoutPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the real drop would drop the test process")
	}
	if err := DropPrivileges(65533, 65533); err == nil || !strings.Contains(err.Error(), "setgroups") {
		t.Errorf("DropPrivileges without privilege = %v, want the setgroups error", err)
	}
}
