package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami/amitest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// houseBoot is the Boot of a PBX pod for intaketest.House's settings.
var houseBoot = compile.BootFor("192.168.0.240", 5060)

var (
	desk   = drivers.DeviceID(drivers.KindPhone, "desk")
	front  = drivers.DeviceID(drivers.KindPhone, "front")
	laptop = drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
)

// asterisk simulates Asterisk behind the fake AMI: a reload of res_pjsip.so
// or pbx_config.so reads the version the current link names, as the real
// one reads its config through the link. Its endpoints are the sections
// of type endpoint in that version's pjsip.conf, as the real one's are,
// not the index's devices: verify must expect exactly what the compiled
// files define.
type asterisk struct {
	t    *testing.T
	live string
	ami  *amitest.Fake

	mu         sync.Mutex
	drop       map[string]string // version -> an endpoint its pjsip.conf "fails" to define
	extra      map[string]string // version -> an endpoint its pjsip.conf defines beyond the index
	failReload map[string]error  // "<version> <module>" -> the reload's error
	reloads    []string          // "<module>@<version prefix>"
}

func newAsterisk(t *testing.T, live string) *asterisk {
	a := &asterisk{t: t, live: live, ami: amitest.New(), drop: map[string]string{}, extra: map[string]string{}, failReload: map[string]error{}}
	a.ami.OnReload(a.reload)
	return a
}

// current reads the live version's index, as Asterisk reads its files.
func (a *asterisk) current() (string, compile.Index) {
	v, err := layout.Current(a.live)
	if err != nil {
		return "", compile.Index{}
	}
	raw, err := os.ReadFile(filepath.Join(a.live, "v"+v, layout.IndexFile))
	if err != nil {
		a.t.Errorf("Asterisk cannot read the live version: %v", err)
		return "", compile.Index{}
	}
	var idx compile.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		a.t.Error(err)
	}
	return v, idx
}

func (a *asterisk) reload(module string) error {
	v, _ := a.current()
	if v == "" {
		return nil // no config yet
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloads = append(a.reloads, module+"@"+v[:4])
	if err := a.failReload[v+" "+module]; err != nil {
		return err
	}
	switch module {
	case "res_pjsip.so":
		var eps []string
		for _, id := range a.endpoints(v) {
			if id != a.drop[v] {
				eps = append(eps, id)
			}
		}
		if x := a.extra[v]; x != "" {
			eps = append(eps, x)
		}
		a.ami.SetEndpoints(eps...)
	case "pbx_config.so":
		a.ami.SetContexts(compile.DevicesContext, compile.UnavailableContext, "patchbay-people")
	}
	return nil
}

// endpoints are the names of the sections of type endpoint in version v's
// pjsip.conf.
func (a *asterisk) endpoints(v string) []string {
	raw, err := os.ReadFile(filepath.Join(a.live, "v"+v, "asterisk", "pjsip.conf"))
	if err != nil {
		a.t.Errorf("Asterisk cannot read pjsip.conf: %v", err)
		return nil
	}
	var out []string
	section := ""
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "["); ok {
			section = strings.TrimSuffix(name, "]")
		} else if line == "type = endpoint" {
			out = append(out, section)
		}
	}
	return out
}

// boot starts Asterisk on the live version and brings AMI up.
func (a *asterisk) boot() {
	_ = a.reload("res_pjsip.so")
	_ = a.reload("pbx_config.so")
	a.mu.Lock()
	a.reloads = nil
	a.mu.Unlock()
	a.ami.SetUp(true, "22.11.0")
}

func (a *asterisk) resetReloads() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloads = nil
}

func (a *asterisk) reloaded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reloads)
}

type env struct {
	t   *testing.T
	dir string
	ast *asterisk
	m   *metrics.Metrics
	a   *apply.Applier
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), m: metrics.New(), now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	e.ast = newAsterisk(t, e.dir)
	e.a = e.applier(apply.Config{})
	return e
}

// applier makes an Applier on the env's live directory (a restart, when
// called again).
func (e *env) applier(cfg apply.Config) *apply.Applier {
	e.t.Helper()
	cfg.LiveDir, cfg.AMI, cfg.Metrics = e.dir, e.ast.ami, e.m
	if cfg.Boot.Digest == "" {
		cfg.Boot = houseBoot
	}
	if cfg.FSGroup == 0 {
		cfg.FSGroup = os.Getgid()
	}
	if cfg.VerifyTimeout == 0 {
		cfg.VerifyTimeout = 300 * time.Millisecond
	}
	cfg.Now = func() time.Time { return e.now }
	cfg.Log = logr.Discard()
	a, err := apply.New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	a.SetIntervals(5*time.Millisecond, 5*time.Millisecond)
	return a
}

func (e *env) apply(r intake.Resolved) error {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.a.Apply(ctx, r)
}

func (e *env) mustApply(r intake.Resolved) {
	e.t.Helper()
	if err := e.apply(r); err != nil {
		e.t.Fatalf("Apply %s: %v", r.Bundle.Version[:4], err)
	}
}

func (e *env) current() string {
	e.t.Helper()
	v, err := layout.Current(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// versionsOnDisk lists the v<version> directories.
func (e *env) versionsOnDisk() []string {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		if v, ok := layout.VersionFromDir(en.Name()); ok {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func (e *env) failures() float64 {
	families, _ := e.m.Registry().Gather()
	for _, f := range families {
		if f.GetName() == "patchbay_agent_apply_failures_total" {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return -1
}

func (e *env) appliedVersionMetric() []string {
	families, _ := e.m.Registry().Gather()
	var out []string
	for _, f := range families {
		if f.GetName() == "patchbay_agent_applied_version_info" {
			for _, s := range f.GetMetric() {
				out = append(out, s.GetLabel()[0].GetValue())
			}
		}
	}
	return out
}

// runApplier runs a.Run until the test ends.
func runApplier(t *testing.T, a *apply.Applier) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// house and its variants, resolved.
func house(t *testing.T) intake.Resolved { return intaketest.House().Resolved(t) }

func houseWithFront(t *testing.T) intake.Resolved {
	in := intaketest.House()
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	return in.Resolved(t)
}

func TestWriteModesAndGroup(t *testing.T) {
	// A restrictive umask must not change a single mode.
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	group := os.Getgid()
	// A supplementary group proves the group is set, not inherited.
	if groups, _ := os.Getgroups(); len(groups) > 0 {
		for _, g := range groups {
			if g != os.Getgid() {
				group = g
				break
			}
		}
	}
	e := newEnv(t)
	e.a = e.applier(apply.Config{FSGroup: group})
	r := house(t)
	e.mustApply(r)

	vdir := filepath.Join(e.dir, "v"+r.Bundle.Version)
	check := func(path string, want fs.FileMode) {
		t.Helper()
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode().Perm(), want)
		}
		if gid := int(fi.Sys().(*syscall.Stat_t).Gid); gid != group {
			t.Errorf("%s: group %d, want %d", path, gid, group)
		}
	}
	check(vdir, layout.VersionDirMode)
	check(filepath.Join(vdir, layout.IndexFile), layout.IndexFileMode)
	for _, tree := range compile.Trees {
		dirMode, _, _ := layout.TreeModes(tree)
		check(filepath.Join(vdir, tree), dirMode)
	}
	for p, content := range r.Files {
		tree, _, _ := strings.Cut(p, "/")
		_, fileMode, _ := layout.TreeModes(tree)
		path := filepath.Join(vdir, filepath.FromSlash(p))
		check(path, fileMode)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(content) {
			t.Errorf("%s: content differs (%v)", p, err)
		}
	}
	// Nothing but the bundle's files, the trees and the index.
	n := 0
	_ = filepath.WalkDir(vdir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if n != len(r.Files)+1 {
		t.Errorf("%d files in the version directory, want %d", n, len(r.Files)+1)
	}
	raw, _ := os.ReadFile(filepath.Join(vdir, layout.IndexFile))
	want, _ := json.Marshal(r.Bundle.Index())
	if string(raw) != string(want) {
		t.Errorf("bundle.json = %s, want %s", raw, want)
	}
	link, err := os.Readlink(filepath.Join(e.dir, compile.CurrentLink))
	if err != nil || link != "v"+r.Bundle.Version {
		t.Errorf("current -> %q (%v), want a relative link to the version", link, err)
	}
	// A nested path gets its tree's directory mode too.
	r2 := house(t)
	r2.Files["tftp/ringers/ring1.raw"] = []byte("x")
	r2.Bundle.Files["tftp/ringers/ring1.raw"] = []byte("x")
	r2.Bundle.Version = intaketest.Version('a')
	e.ast.ami.SetUp(false, "")
	e.mustApply(r2)
	check(filepath.Join(e.dir, "v"+r2.Bundle.Version, "tftp", "ringers"), 0o755)
	check(filepath.Join(e.dir, "v"+r2.Bundle.Version, "tftp", "ringers", "ring1.raw"), 0o644)
}

// TestSwapIsAtomic: a reader resolving current over and over, during 50
// applies, always finds one complete version.
func TestSwapIsAtomic(t *testing.T) {
	e := newEnv(t)
	e.a = e.applier(apply.Config{Keep: 100})
	var versions []intake.Resolved
	for i := range 50 {
		in := intaketest.House()
		in.Phones[0].Spec.Label = "Desk " + string(rune('A'+i%26)) + string(rune('a'+i/26))
		versions = append(versions, in.Resolved(t))
	}
	stop := make(chan struct{})
	var reads int
	var bad []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, err := layout.Current(e.dir)
			if errors.Is(err, layout.ErrNoCurrent) && reads == 0 {
				continue // before the first apply
			}
			if err != nil {
				bad = append(bad, err.Error())
				continue
			}
			reads++
			raw, err := os.ReadFile(filepath.Join(e.dir, "v"+v, layout.IndexFile))
			var idx compile.Index
			if err != nil || json.Unmarshal(raw, &idx) != nil || idx.Version != v {
				bad = append(bad, "index of "+v[:4])
				continue
			}
			for _, d := range idx.Devices {
				for _, f := range d.Files {
					if _, err := os.Stat(filepath.Join(e.dir, "v"+v, f)); err != nil {
						bad = append(bad, f+" of "+v[:4])
					}
				}
			}
			for _, f := range []string{"asterisk/asterisk.conf", "asterisk/pjsip.conf", "http/directory.xml"} {
				if _, err := os.Stat(filepath.Join(e.dir, "v"+v, f)); err != nil {
					bad = append(bad, f+" of "+v[:4])
				}
			}
		}
	}()
	for _, r := range versions {
		e.mustApply(r) // AMI down: write and swap only
	}
	close(stop)
	<-done
	if reads < 50 || len(bad) > 0 {
		t.Errorf("%d reads; incomplete versions seen: %v", reads, bad)
	}
	if e.current() != versions[49].Bundle.Version {
		t.Error("the last apply is not current")
	}
}

func TestReloadPlan(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	in := intaketest.House()
	e.mustApply(in.Resolved(t))
	want := []string{
		"Reload res_pjsip.so", "Reload pbx_config.so", "Reload res_rtp_asterisk.so", "Reload res_pjsip_notify.so",
		"Reload indications", "Reload logger", "Reload manager", "SetAccount patchbay-agent", "Endpoints", "DialplanContexts",
	}
	if got := e.ast.ami.Calls(); !slices.Equal(got, want) {
		t.Errorf("first version:\n got %q\nwant %q", got, want)
	}

	steps := []struct {
		name   string
		change func(*intaketest.Install)
		want   []string
	}{
		{"a label", func(in *intaketest.Install) { in.Phones[0].Spec.Label = "Front desk" },
			[]string{"Reload res_pjsip.so", "Reload pbx_config.so", "Endpoints", "DialplanContexts"}},
		{"the RTP ports", func(in *intaketest.Install) { in.Settings.RTPPorts.Last = 10199 },
			[]string{"Reload res_pjsip.so", "Reload pbx_config.so", "Reload res_rtp_asterisk.so", "Endpoints", "DialplanContexts"}},
		{"the AMI password", func(in *intaketest.Install) { in.Secrets[intaketest.AMIRef] = []byte("not-a-secret-ami-0002") },
			[]string{"Reload res_pjsip.so", "Reload pbx_config.so", "Reload manager", "SetAccount patchbay-agent", "Endpoints", "DialplanContexts"}},
	}
	for _, s := range steps {
		s.change(in)
		e.ast.ami.ResetCalls()
		e.mustApply(in.Resolved(t))
		if got := e.ast.ami.Calls(); !slices.Equal(got, s.want) {
			t.Errorf("%s:\n got %q\nwant %q", s.name, got, s.want)
		}
	}
	if _, pass := e.ast.ami.Account(); pass != "not-a-secret-ami-0002" {
		t.Errorf("AMI account password = %q, want the new one", pass)
	}

	// Each other owner, for a change to its file alone (on top of the last).
	r := in.Resolved(t)
	for i, o := range []struct{ file, module string }{
		{"pjsip_notify.conf", "res_pjsip_notify.so"}, {"indications.conf", "indications"}, {"logger.conf", "logger"},
		{"asterisk.conf", ""}, {"modules.conf", ""},
	} {
		r.Files = maps.Clone(r.Files)
		r.Files["asterisk/"+o.file] = append(slices.Clone(r.Files["asterisk/"+o.file]), []byte("; changed\n")...)
		r.Bundle.Version = intaketest.Version("12345"[i])
		e.ast.ami.ResetCalls()
		e.mustApply(r)
		want := []string{"Reload res_pjsip.so", "Reload pbx_config.so"}
		if o.module != "" {
			want = append(want, "Reload "+o.module)
		}
		want = append(want, "Endpoints", "DialplanContexts")
		if got := e.ast.ami.Calls(); !slices.Equal(got, want) {
			t.Errorf("%s changed:\n got %q\nwant %q", o.file, got, want)
		}
	}
}

func TestVerifyMismatchRollsBack(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	grants1v, grants1 := e.a.Grants()
	e.ast.resetReloads()

	v2 := houseWithFront(t)
	e.ast.drop[v2.Bundle.Version] = front
	e.ast.ami.ResetCalls()
	err := e.apply(v2)
	if err == nil || err.Error() != "verify: endpoints missing "+front {
		t.Fatalf("Apply = %v, want the mismatch", err)
	}
	if e.current() != v1.Bundle.Version {
		t.Error("current was not swapped back")
	}
	// The reloads ran on v2, then again on v1.
	want := []string{"res_pjsip.so@" + v2.Bundle.Version[:4], "pbx_config.so@" + v2.Bundle.Version[:4],
		"res_pjsip.so@" + v1.Bundle.Version[:4], "pbx_config.so@" + v1.Bundle.Version[:4]}
	if got := e.ast.reloaded(); !slices.Equal(got, want) {
		t.Errorf("reloads %q, want %q", got, want)
	}
	if eps, _ := e.ast.ami.Endpoints(context.Background()); !slices.Equal(eps, []string{desk, laptop}) {
		t.Errorf("Asterisk holds %v after the rollback", eps)
	}
	st := e.a.Status()
	if st.AppliedVersion != v1.Bundle.Version || st.LastApplyVersion != v2.Bundle.Version ||
		st.LastApplyError != err.Error() || st.LastApplyAt == nil || !st.Verified {
		t.Errorf("Status = %+v", st)
	}
	if e.failures() != 1 {
		t.Errorf("apply failures = %v, want 1", e.failures())
	}
	if gv, g := e.a.Grants(); gv != grants1v || !slices.Equal(g, grants1) {
		t.Error("the grants changed with a rolled-back version")
	}
	if v, _, _ := e.a.Applied(); v != v1.Bundle.Version {
		t.Errorf("Applied = %s", v)
	}
	if got := e.appliedVersionMetric(); !slices.Equal(got, []string{v1.Bundle.Version}) {
		t.Errorf("applied version metric = %v", got)
	}

	// An unexpected endpoint and a missing context are reported too.
	v3 := house(t)
	v3.Bundle.Version = intaketest.Version('3')
	e.ast.extra[v3.Bundle.Version] = front
	e.ast.ami.OnContexts(func() ([]string, error) { return []string{compile.DevicesContext}, nil })
	err = e.apply(v3)
	if err == nil || err.Error() != "verify: endpoints unexpected "+front+"; dialplan missing "+compile.UnavailableContext {
		t.Errorf("Apply = %v", err)
	}
}

func TestReloadErrorRollsBack(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	e.ast.resetReloads()
	v2 := houseWithFront(t)
	e.ast.failReload[v2.Bundle.Version+" pbx_config.so"] = errors.New("ami: Reload: An unknown error occurred")
	err := e.apply(v2)
	if err == nil || err.Error() != "reload pbx_config.so: ami: Reload: An unknown error occurred" {
		t.Fatalf("Apply = %v", err)
	}
	if e.current() != v1.Bundle.Version {
		t.Error("current was not swapped back")
	}
	st := e.a.Status()
	if st.AppliedVersion != v1.Bundle.Version || st.LastApplyVersion != v2.Bundle.Version || st.LastApplyError != err.Error() {
		t.Errorf("Status = %+v", st)
	}
	want := []string{"res_pjsip.so@" + v2.Bundle.Version[:4], "pbx_config.so@" + v2.Bundle.Version[:4],
		"res_pjsip.so@" + v1.Bundle.Version[:4], "pbx_config.so@" + v1.Bundle.Version[:4]}
	if got := e.ast.reloaded(); !slices.Equal(got, want) {
		t.Errorf("reloads %q, want %q", got, want)
	}
	// The same version again, once fixed, applies.
	delete(e.ast.failReload, v2.Bundle.Version+" pbx_config.so")
	e.mustApply(v2)
	if st := e.a.Status(); st.AppliedVersion != v2.Bundle.Version || st.LastApplyError != "" {
		t.Errorf("Status after the retry = %+v", st)
	}
}

// TestFirstVersionWithoutAMI: before Asterisk runs, the first version is
// written and swapped, Ready passes, and it is verified once AMI connects.
func TestFirstVersionWithoutAMI(t *testing.T) {
	e := newEnv(t)
	if e.a.Ready() == nil {
		t.Error("ready before any version")
	}
	runApplier(t, e.a)
	v1 := house(t)
	e.mustApply(v1)
	if err := e.a.Ready(); err != nil {
		t.Errorf("Ready = %v", err)
	}
	st := e.a.Status()
	if st.AppliedVersion != v1.Bundle.Version || st.LastApplyVersion != v1.Bundle.Version || st.Verified || st.LastApplyError != "" {
		t.Errorf("Status before AMI = %+v", st)
	}
	if user, pass := e.ast.ami.Account(); user != compile.AMIUsername || pass != v1.AMIPassword {
		t.Error("the account was not handed to AMI before it connects")
	}
	if got := e.ast.ami.Calls(); !slices.Equal(got, []string{"SetAccount patchbay-agent"}) {
		t.Errorf("calls before AMI: %q", got)
	}
	e.ast.boot()
	waitFor(t, "verification", func() bool { return e.a.Status().Verified })
	if got := e.ast.reloaded(); len(got) != 7 {
		t.Errorf("reloads on connect %q, want the full plan", got)
	}

	// A first version that fails verification stays live, with the error.
	e2 := newEnv(t)
	runApplier(t, e2.a)
	b := house(t)
	e2.ast.drop[b.Bundle.Version] = laptop
	e2.mustApply(b)
	e2.ast.boot()
	waitFor(t, "the failure", func() bool { return e2.a.Status().LastApplyError != "" })
	st = e2.a.Status()
	if st.AppliedVersion != b.Bundle.Version || st.Verified || st.LastApplyError != "verify: endpoints missing "+laptop {
		t.Errorf("Status = %+v", st)
	}
	if e2.current() != b.Bundle.Version || e2.a.Ready() != nil {
		t.Error("the first version did not stay live")
	}
	// Tried once per AMI session: a new session tries again.
	n, reloads := e2.failures(), len(e2.ast.reloaded())
	time.Sleep(500 * time.Millisecond)
	if e2.failures() != n || len(e2.ast.reloaded()) != reloads {
		t.Error("verification retried on the same session")
	}
	delete(e2.ast.drop, b.Bundle.Version)
	e2.ast.ami.SetUp(true, "22.11.0") // a new session
	waitFor(t, "verification on the new session", func() bool { return e2.a.Status().Verified })
	if st := e2.a.Status(); st.LastApplyError != "" {
		t.Errorf("Status = %+v", st)
	}
}

// TestUnverifiedRollsBackOnConnect: a version swapped while AMI was down
// that fails once it connects rolls back to the last verified version.
func TestUnverifiedRollsBackOnConnect(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	runApplier(t, e.a)
	v1 := house(t)
	e.mustApply(v1)
	e.ast.ami.SetUp(false, "")

	v2 := houseWithFront(t)
	e.ast.drop[v2.Bundle.Version] = front
	e.mustApply(v2)
	if e.current() != v2.Bundle.Version || e.a.Status().Verified {
		t.Fatal("v2 is not live unverified")
	}
	if _, g := e.a.Grants(); len(g) != 1 || g[0].BundleVersion != v2.Bundle.Version {
		t.Errorf("grants of the unverified version: %v", g)
	}
	e.ast.ami.SetUp(true, "22.11.0")
	waitFor(t, "the rollback", func() bool { return e.a.Status().AppliedVersion == v1.Bundle.Version })
	st := e.a.Status()
	if st.LastApplyVersion != v2.Bundle.Version || st.LastApplyError != "verify: endpoints missing "+front || !st.Verified {
		t.Errorf("Status = %+v", st)
	}
	if e.current() != v1.Bundle.Version {
		t.Error("current was not swapped back")
	}
	if _, g := e.a.Grants(); len(g) != 1 || g[0].BundleVersion != v1.Bundle.Version {
		t.Errorf("grants after the rollback: %v", g)
	}
}

// TestAdoptOnRestart: a new Applier adopts the live version without
// rewriting or reloading it, and verifies it once AMI is up.
func TestAdoptOnRestart(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	vdir := filepath.Join(e.dir, "v"+v1.Bundle.Version)
	before, _ := os.Stat(filepath.Join(vdir, "asterisk", "pjsip.conf"))
	_ = os.MkdirAll(filepath.Join(e.dir, ".staging-left-by-a-crash"), 0o700)

	e.ast.ami.ResetCalls()
	e.ast.ami.SetUp(false, "")
	b := e.applier(apply.Config{})
	if _, err := os.Stat(filepath.Join(e.dir, ".staging-left-by-a-crash")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a staging directory left by a crash was not removed")
	}
	st := b.Status()
	if st.AppliedVersion != v1.Bundle.Version || st.Verified || b.Ready() != nil {
		t.Errorf("Status after the restart = %+v", st)
	}
	if v, g := b.Grants(); v != "" || g != nil {
		t.Errorf("Grants before intake hands the version over = %q %v, want none", v, g)
	}
	if v, idx, ok := b.Applied(); !ok || v != v1.Bundle.Version || len(idx.Devices) != 2 {
		t.Errorf("Applied = %s %v %v", v, idx, ok)
	}
	runApplier(t, b)
	// Intake hands it over again: no write, no reload; the account goes to AMI.
	e.a = b
	e.mustApply(v1)
	if gv, g := b.Grants(); gv != v1.Bundle.Version || len(g) != 1 {
		t.Errorf("Grants after the hand-over = %s %v", gv, g)
	}
	e.ast.ami.SetUp(true, "22.11.0")
	waitFor(t, "verification", func() bool { return b.Status().Verified })
	for _, c := range e.ast.ami.Calls() {
		if strings.HasPrefix(c, "Reload") {
			t.Errorf("the adopted version was reloaded: %q", e.ast.ami.Calls())
		}
	}
	after, _ := os.Stat(filepath.Join(vdir, "asterisk", "pjsip.conf"))
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Error("the adopted version was rewritten")
	}
}

func TestKeepsThreeVersions(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	var all []string
	for i := range 5 {
		in := intaketest.House()
		in.Phones[0].Spec.Label = "Desk " + string(rune('A'+i))
		r := in.Resolved(t)
		all = append(all, r.Bundle.Version)
		e.mustApply(r)
		time.Sleep(2 * time.Millisecond)
	}
	want := slices.Sorted(slices.Values(all[2:]))
	if got := e.versionsOnDisk(); !slices.Equal(got, want) {
		t.Errorf("versions on disk %v, want the newest three %v", got, want)
	}
	if _, ok := e.a.Index(all[0]); ok {
		t.Error("the index of a removed version is still known")
	}
	// A failed version is pruned like any other, never the live one or the
	// one before it.
	broken := houseWithFront(t)
	e.ast.drop[broken.Bundle.Version] = front
	_ = e.apply(broken)
	for i := range 3 {
		in := intaketest.House()
		in.Phones[0].Spec.Label = "Desk " + string(rune('a'+i))
		e.mustApply(in.Resolved(t))
		time.Sleep(2 * time.Millisecond)
	}
	got := e.versionsOnDisk()
	if len(got) != 3 || slices.Contains(got, broken.Bundle.Version) {
		t.Errorf("versions on disk %v", got)
	}
	if !slices.Contains(got, e.current()) {
		t.Error("the live version was removed")
	}
	entries, _ := os.ReadDir(e.dir)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			t.Errorf("left behind: %s", en.Name())
		}
	}
}

func TestGrants(t *testing.T) {
	e := newEnv(t)
	if v, g := e.a.Grants(); v != "" || g != nil {
		t.Errorf("before any apply: %q %v", v, g)
	}
	in := intaketest.House()
	in.AddSoftphone("den", "151", "Den")
	r := in.Resolved(t)
	e.mustApply(r)
	v, grants := e.a.Grants()
	if v != r.Bundle.Version || len(grants) != 2 {
		t.Fatalf("Grants = %s %v", v, grants)
	}
	den := drivers.DeviceID(drivers.KindSoftphone, "den")
	for i, id := range []string{laptop, den} {
		g, d := grants[i], r.Bundle.Devices[id]
		if g.Device != id || g.Token != r.Tokens[id] || g.File != d.ConfigFile || g.BundleVersion != v || g.Version != d.Version {
			t.Errorf("grant %d = %+v (token ok %t)", i, g, g.Token == r.Tokens[id])
		}
	}
	if grants[0].Device > grants[1].Device {
		t.Error("grants are not sorted by device")
	}
	grants[0].Token = "changed"
	if _, again := e.a.Grants(); again[0].Token == "changed" {
		t.Error("Grants returned the Applier's own slice")
	}
	// No softphone: an empty list, not "unknown".
	none := intaketest.House()
	none.Softphones = nil
	none.People = nil
	e.mustApply(none.Resolved(t))
	if v, g := e.a.Grants(); v == "" || g == nil || len(g) != 0 {
		t.Errorf("Grants with no softphone = %q %v, want the version and an empty list", v, g)
	}
}

func TestIndexOfAnOlderVersion(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1, v2 := house(t), houseWithFront(t)
	e.mustApply(v1)
	e.mustApply(v2)
	b := e.applier(apply.Config{}) // a restart
	idx, ok := b.Index(v1.Bundle.Version)
	if !ok || idx.Version != v1.Bundle.Version || len(idx.Devices) != 2 || idx.Devices[desk].Version != v1.Bundle.Devices[desk].Version {
		t.Errorf("Index(v1) = %+v %v", idx, ok)
	}
	if idx, ok := b.Index(v2.Bundle.Version); !ok || len(idx.Devices) != 3 {
		t.Errorf("Index(v2) = %+v %v", idx, ok)
	}
	for _, v := range []string{intaketest.Version('f'), "../../etc", ""} {
		if _, ok := b.Index(v); ok {
			t.Errorf("Index(%q) found", v)
		}
	}
	// A corrupt index is not an index.
	if err := os.WriteFile(filepath.Join(e.dir, "v"+v1.Bundle.Version, layout.IndexFile), []byte(`{"version":"x"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.applier(apply.Config{}).Index(v1.Bundle.Version); ok {
		t.Error("an index naming another version was read")
	}
}

func TestRefusesInvalidVersion(t *testing.T) {
	e := newEnv(t)
	r := house(t)
	r.Bundle.Version = "../escape"
	err := e.apply(r)
	if err == nil || !strings.Contains(err.Error(), "not a bundle version") {
		t.Fatalf("Apply = %v", err)
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
		t.Errorf("an invalid version wrote %d entries", len(entries))
	}
	st := e.a.Status()
	if st.LastApplyVersion != "" || st.LastApplyError == "" || st.AppliedVersion != "" || e.failures() != 1 {
		t.Errorf("Status = %+v, failures %v", st, e.failures())
	}
	// A path that is not a bundle path is refused before anything is
	// written.
	r = house(t)
	r.Files["../outside"] = []byte("x")
	if err := e.apply(r); err == nil {
		t.Error("a path outside the trees was written")
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
		t.Errorf("a refused bundle wrote %d entries", len(entries))
	}

	e.a.Refuse(intaketest.Version('b'), errors.New("unresolved placeholder patchbay-ami/password"))
	st = e.a.Status()
	if st.LastApplyVersion != intaketest.Version('b') || st.LastApplyError != "unresolved placeholder patchbay-ami/password" || e.failures() != 3 {
		t.Errorf("after Refuse: %+v, failures %v", st, e.failures())
	}
}

// TestSameVersionAgain: applying the live version again rewrites and
// reloads nothing, and clears an older failure.
func TestSameVersionAgain(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	e.a.Refuse(intaketest.Version('c'), errors.New("x"))
	e.ast.ami.ResetCalls()
	e.mustApply(v1)
	if got := e.ast.ami.Calls(); len(got) != 0 {
		t.Errorf("calls %q, want none", got)
	}
	if st := e.a.Status(); st.LastApplyVersion != v1.Bundle.Version || st.LastApplyError != "" || !st.Verified {
		t.Errorf("Status = %+v", st)
	}
}

// TestRollbackRestoresTheAccount: a new AMI password that fails is undone
// on Asterisk (manager reloads again) and in the client.
func TestRollbackRestoresTheAccount(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	in := intaketest.House()
	v1 := in.Resolved(t)
	e.mustApply(v1)
	in.Secrets[intaketest.AMIRef] = []byte("not-a-secret-ami-0002")
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	v2 := in.Resolved(t)
	e.ast.drop[v2.Bundle.Version] = front
	e.ast.ami.ResetCalls()
	if err := e.apply(v2); err == nil {
		t.Fatal("v2 applied")
	}
	calls := e.ast.ami.Calls()
	var setAccounts, managers int
	for _, c := range calls {
		switch c {
		case "SetAccount patchbay-agent":
			setAccounts++
		case "Reload manager":
			managers++
		}
	}
	if setAccounts != 2 || managers != 2 {
		t.Errorf("calls %q: want manager reloaded and the account set, twice", calls)
	}
	if _, pass := e.ast.ami.Account(); pass != v1.AMIPassword {
		t.Error("the old account was not restored")
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	if _, err := apply.New(apply.Config{AMI: amitest.New(), Boot: houseBoot}); err == nil {
		t.Error("no live directory: accepted")
	}
	if _, err := apply.New(apply.Config{LiveDir: t.TempDir(), Boot: houseBoot}); err == nil {
		t.Error("no AMI: accepted")
	}
	if _, err := apply.New(apply.Config{LiveDir: t.TempDir(), AMI: amitest.New()}); err == nil {
		t.Error("no Boot: accepted")
	}
	// A broken current link is not adopted.
	dir := t.TempDir()
	if err := os.Symlink("v"+intaketest.Version('d'), filepath.Join(dir, compile.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	a, err := apply.New(apply.Config{LiveDir: dir, AMI: amitest.New(), Boot: houseBoot})
	if err != nil || a.Ready() == nil {
		t.Errorf("a dangling current link: %v, ready %v", err, a.Ready())
	}
	// A missing live directory is created.
	missing := filepath.Join(t.TempDir(), "live")
	if _, err := apply.New(apply.Config{LiveDir: missing, AMI: amitest.New(), Boot: houseBoot}); err != nil {
		t.Error(err)
	}
	if fi, err := os.Stat(missing); err != nil || !fi.IsDir() {
		t.Error("the live directory was not created")
	}
}

// TestRefusesAnotherBoot: a version compiled for another SIP bind than
// this PBX pod's (a controller that has not recompiled since pbx.ports.sip
// or pbx.address changed), or with another release's asterisk.conf or
// modules.conf, is refused before anything is written. A reload cannot
// move the transports or load a module, so such a version would go live
// and verify while Asterisk kept what it booted with; as the first version
// it would be what Asterisk boots on. The pod's own version applies.
func TestRefusesAnotherBoot(t *testing.T) {
	e := newEnv(t)
	other := intaketest.House()
	other.Settings.SIPPort = 5070
	stale := other.Resolved(t)

	err := e.apply(stale)
	if err == nil || !strings.Contains(err.Error(), "192.168.0.240:5070") || !strings.Contains(err.Error(), "192.168.0.240:5060") {
		t.Fatalf("Apply of a version for port 5070 in a 5060 pod = %v, want a refusal naming both", err)
	}
	if e.a.Ready() == nil {
		t.Error("ready on a refused first version: Asterisk would boot on nothing, or on it")
	}
	if st := e.a.Status(); st.AppliedVersion != "" || st.LastApplyVersion != stale.Bundle.Version || st.LastApplyError != err.Error() {
		t.Errorf("Status = %+v, want nothing applied and the refusal", st)
	}
	if v := e.versionsOnDisk(); len(v) != 0 {
		t.Errorf("a refused version was written: %v", v)
	}

	good := intaketest.House().Resolved(t)
	e.mustApply(good)
	e.ast.boot()
	e.ast.resetReloads()
	if err := e.apply(stale); err == nil {
		t.Fatal("the stale version applied over a live one")
	}
	if cur := e.current(); cur != good.Bundle.Version {
		t.Errorf("live version %.4s, want %.4s", cur, good.Bundle.Version)
	}
	if r := e.ast.reloaded(); len(r) != 0 {
		t.Errorf("Asterisk was reloaded for a refused version: %v", r)
	}

	// Another release's module list is refused the same way.
	files := maps.Clone(good.Files)
	files["asterisk/modules.conf"] = append(slices.Clone(files["asterisk/modules.conf"]), "load => res_new.so\n"...)
	next := good
	next.Files = files
	next.Bundle.Version = intaketest.Version('e')
	if err := e.apply(next); err == nil || !strings.Contains(err.Error(), "modules.conf") {
		t.Errorf("Apply with another modules.conf = %v, want a refusal", err)
	}
}

// TestVerifyIgnoresUnrenderedDevices: a device in the index that is not
// rendered (a 7975, Invalid until its load is in the manifest; or a number
// or MAC conflict) has no PJSIP endpoint, and verify does not expect one:
// otherwise every bundle holding such a device would fail verification and
// roll back, and no change would ever apply.
func TestVerifyIgnoresUnrenderedDevices(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	in := intaketest.House()
	in.Phones = append(in.Phones, v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", CreationTimestamp: metav1.Now()},
		Spec:       v1alpha1.PhoneSpec{Model: "cisco-7975", MAC: "D0C78915A001", Number: "110", Label: "Lobby"},
	})
	in.Secrets[compile.PasswordRef(drivers.DeviceID(drivers.KindPhone, "lobby"))] = []byte("not-a-secret-lobby-0001")
	r := in.Resolved(t)
	lobby, ok := r.Bundle.Devices[drivers.DeviceID(drivers.KindPhone, "lobby")]
	if !ok || lobby.Version != "" {
		t.Fatalf("lobby in the index: %+v, %v; want it there, unrendered", lobby, ok)
	}
	e.mustApply(r)
	if st := e.a.Status(); !st.Verified || st.AppliedVersion != r.Bundle.Version {
		t.Errorf("Status = %+v, want the version verified", st)
	}
	// And the next version applies too.
	in.Phones[0].Spec.Label = "Desk Two"
	e.mustApply(in.Resolved(t))
}
