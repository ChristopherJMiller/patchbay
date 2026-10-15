// Package apply is the agent's apply, verify and rollback (W2.2): a
// commit-confirm of each resolved bundle (design §5, Apply).
//
// Apply writes the version completely into its own directory (modes and
// group per package layout), swaps the current link by renaming a symlink
// over it, reloads exactly the modules whose configuration changed, and
// verifies that Asterisk holds the bundle's endpoints and contexts. A
// reload error or a mismatch swaps the previous version back, reloads it,
// and reports the failure; the previous version stays applied.
//
// Reloads (docs/plans/release-a.md §5, W2.2): res_pjsip.so, then
// pbx_config.so (the dialplan), then the owner of each other asterisk/
// file whose resolved bytes changed (rtp.conf: res_rtp_asterisk.so,
// pjsip_notify.conf: res_pjsip_notify.so, indications.conf: indications,
// logger.conf: logger), and manager last when manager.conf changed,
// followed by AMI.SetAccount with the new account. The PJSIP transports,
// asterisk.conf and modules.conf are not reloadable; only settings change
// them, and the chart restarts the PBX pod for those.
//
// With AMI down (Asterisk starting, crashed or restarting) a version is
// written and swapped unverified: Ready passes, Asterisk reads it when it
// starts, and Run reloads and verifies it once AMI connects, rolling back
// to the last verified version on a failure. The very first version has
// nothing to roll back to: it stays live with the error set. An agent
// restart adopts the live version without rewriting or reloading it, and
// verifies it once AMI is up; its grants and AMI account are known once
// intake hands it over again. An adopted version was live before the
// restart, so it is what a newer version that fails rolls back to,
// verified again or not.
//
// A version's secret values are fixed by its version (intake applies a
// version only with the values it was compiled with), so applying the live
// version again changes nothing it already has.
//
// SettledGrants are the grants device HTTP prunes spent tokens against:
// the live version's, once no failure can roll it back (see there).
package apply

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Config configures an Applier.
type Config struct {
	// LiveDir is the live directory (compile.LiveDir).
	LiveDir string
	// FSGroup is the group every written file and directory gets (the
	// pod's fsGroup, layout.DefaultFSGroup); negative leaves groups alone.
	FSGroup int
	// AMI is the session to Asterisk.
	AMI ami.Client
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// Keep is how many versions stay on disk; 0 means 3. The live and the
	// previous version always stay.
	Keep int
	// VerifyTimeout bounds verification; 0 means 30 s.
	VerifyTimeout time.Duration
	// Now is the clock for LastApplyAt; nil means time.Now.
	Now func() time.Time
	// Log receives what apply does.
	Log logr.Logger
}

// Status is the result of the applies so far (api.AgentStatus's fields).
type Status struct {
	AppliedVersion   string
	LastApplyVersion string
	LastApplyError   string
	LastApplyAt      *time.Time
	// Verified is false while the live version waits for AMI (or failed
	// verification with nothing to roll back to).
	Verified bool
}

// Defaults.
const (
	DefaultKeep          = 3
	DefaultVerifyTimeout = 30 * time.Second
)

// Reloadable owners of asterisk/ files besides res_pjsip.so and
// pbx_config.so, in the order they are reloaded.
var owners = []struct{ file, module string }{
	{"rtp.conf", "res_rtp_asterisk.so"},
	{"pjsip_notify.conf", "res_pjsip_notify.so"},
	{"indications.conf", "indications"},
	{"logger.conf", "logger"},
}

// managerModule is reloaded last, when manager.conf changed.
const managerModule = "manager"

// live is a version that is, or was, live.
type live struct {
	version string
	index   compile.Index
	// grants and account are nil while unknown (an adopted version
	// before intake hands it over again).
	grants  []provision.Grant
	account *account
	// verified is true once Asterisk was seen to hold it.
	verified bool
	// adopted is true for the version live when the agent started: a
	// rollback target whether or not it is verified again.
	adopted bool
	// restored is true once a rollback made it live again: a version an
	// older one's grants may have to come back from, so not settled.
	restored bool
	// While unverified: the reloads to run before verifying (nil for an
	// adopted version), the version to roll back to (the last verified or
	// adopted one; nil: none), and the AMI session last tried
	// (State.Since).
	plan  []string
	prev  *live
	tried time.Time
}

type account struct{ username, password string }

// Applier applies bundles. It implements intake.Applier and
// provision.Source.
type Applier struct {
	cfg Config

	// op serializes Apply, Refuse and Run's verification.
	op sync.Mutex

	// pollEvery and verifyEvery are how often Run looks at AMI and verify
	// retries (tests shorten them).
	pollEvery   time.Duration
	verifyEvery time.Duration

	mu      sync.Mutex
	status  Status
	cur     *live
	indexes map[string]compile.Index
	acct    *account // the account last handed to AMI.SetAccount
}

// New prepares the live directory (creating it if missing, removing what a
// crash left) and adopts the version the current link names, if any,
// without rewriting or reloading it.
func New(cfg Config) (*Applier, error) {
	switch {
	case cfg.LiveDir == "":
		return nil, errors.New("apply: no live directory")
	case cfg.AMI == nil:
		return nil, errors.New("apply: no AMI client")
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultKeep
	}
	if cfg.VerifyTimeout <= 0 {
		cfg.VerifyTimeout = DefaultVerifyTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &Applier{
		cfg: cfg, pollEvery: time.Second, verifyEvery: 250 * time.Millisecond,
		indexes: map[string]compile.Index{},
	}
	if err := mkdirLive(cfg.LiveDir); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	if err := cleanup(cfg.LiveDir); err != nil {
		return nil, fmt.Errorf("apply: cleaning the live directory: %w", err)
	}
	v, err := layout.Current(cfg.LiveDir)
	switch {
	case errors.Is(err, layout.ErrNoCurrent):
		return a, nil
	case err != nil:
		cfg.Log.Info("the current link is unusable; the next apply replaces it", "error", err.Error())
		return a, nil
	}
	idx, err := readIndex(cfg.LiveDir, v)
	if err != nil {
		cfg.Log.Info("the live version cannot be read; the next apply replaces it", "version", v, "error", err.Error())
		return a, nil
	}
	a.cur = &live{version: v, index: idx, adopted: true}
	a.indexes[v] = idx
	a.status.AppliedVersion = v
	cfg.Metrics.SetAppliedVersion(v)
	cfg.Log.Info("adopted the live version; it is verified once AMI is up", "version", v)
	return a, nil
}

// Apply applies a resolved version (see the package documentation). It
// returns nil once the version is live, verified or (AMI down) waiting to
// be; an error when it was refused or rolled back.
func (a *Applier) Apply(ctx context.Context, r intake.Resolved) error {
	a.op.Lock()
	defer a.op.Unlock()
	v := r.Bundle.Version
	if !layout.ValidVersion(v) {
		err := fmt.Errorf("version %.80q is not a bundle version", v)
		a.failed("", err)
		return err
	}
	next := &live{
		version: v, index: r.Bundle.Index(), grants: grantsOf(v, r),
		account: &account{r.Bundle.AMI.Username, r.AMIPassword},
	}
	cur := a.current()

	if cur != nil && cur.version == v {
		// Live already: nothing to write or reload. An adopted version
		// learns its grants and account here; a version that has them
		// keeps them (its version fixes them).
		a.mu.Lock()
		if cur.grants == nil {
			cur.grants = next.grants
		}
		if cur.account == nil {
			cur.account = next.account
		}
		acct := cur.account
		a.mu.Unlock()
		a.setAccount(acct)
		if !cur.verified && a.cfg.AMI.State().Up {
			return a.verifyLive(ctx)
		}
		a.succeeded(v)
		return nil
	}

	if err := a.write(r); err != nil {
		a.failed(v, err)
		return err
	}
	a.cacheIndex(v, next.index)
	// The last verified version is what a failure rolls back to, or the
	// adopted one (it was live before the agent restarted; otherwise a
	// version that failed before the restart would stay live after it).
	next.prev = cur
	if cur != nil && !cur.verified && !cur.adopted {
		next.prev = cur.prev
	}
	// Reload what differs from what Asterisk may be running: the last
	// verified version, or an unverified one it may have started with.
	next.plan = a.reloadPlan(r.Files, next.prev, cur)
	if err := a.swap(v); err != nil {
		a.failed(v, err)
		return err
	}

	if !a.cfg.AMI.State().Up {
		// Unverified: Asterisk reads it when it starts; Run verifies it.
		a.setLive(next)
		a.setAccount(next.account)
		a.succeeded(v)
		a.cfg.Log.Info("version live, unverified: AMI is down", "version", v)
		return nil
	}

	// The reloads go over the session as it is: a new AMI password is
	// known to Asterisk only after "manager" reloads.
	err := a.reload(ctx, next.plan)
	if err == nil {
		a.setAccount(next.account)
		err = a.verify(ctx, next.index)
	}
	if err != nil {
		a.rollBack(ctx, next, err)
		return err
	}
	next.verified, next.plan, next.prev = true, nil, nil
	a.setLive(next)
	a.succeeded(v)
	a.cfg.Log.Info("version applied and verified", "version", v)
	a.prune(v, prevVersion(cur))
	return nil
}

// Refuse records a version that never reached Apply.
func (a *Applier) Refuse(version string, reason error) {
	a.op.Lock()
	defer a.op.Unlock()
	a.failed(version, reason)
}

// Ready is nil once a version is live (verified or not).
func (a *Applier) Ready() error {
	if a.current() == nil {
		return errors.New("no config version is live yet")
	}
	return nil
}

// Status is the result of the applies so far.
func (a *Applier) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.status
	if s.LastApplyAt != nil {
		at := *s.LastApplyAt
		s.LastApplyAt = &at
	}
	s.Verified = a.cur != nil && a.cur.verified
	return s
}

// Grants are the live version's provisioning grants, a copy: ("", nil)
// before the first apply, and while the live version's tokens are not
// known yet (adopted at start, before intake hands it over again), so
// device HTTP never prunes a spend against a list it does not have.
func (a *Applier) Grants() (string, []provision.Grant) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil || a.cur.grants == nil {
		return "", nil
	}
	return a.cur.version, slices.Clone(a.cur.grants)
}

// SettledGrants are the live version's grants once it is settled, a copy;
// ("", nil) otherwise. Device HTTP prunes spent tokens against them only
// (devicehttp.Config.Settled), so a spend is never forgotten while a
// version that grants its token can still become live again.
//
// A version is settled once this agent verified it, its tokens are known,
// and it did not become live through a rollback. A rollback only ever
// returns to the last verified (or adopted) version, so no version older
// than a settled one can come back by rollback. Not settled: an unverified
// version (a failure rolls back to the one before it, whose tokens may be
// spent), an adopted one before intake hands it over, and a version a
// rollback restored (the failed version may be applied again, after an
// agent restart, with the tokens it granted), until a newer version
// settles.
func (a *Applier) SettledGrants() (string, []provision.Grant) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.cur
	if c == nil || !c.verified || c.restored || c.grants == nil {
		return "", nil
	}
	return c.version, slices.Clone(c.grants)
}

// Applied is the live version and its index. The index is shared: do not
// modify it.
func (a *Applier) Applied() (string, compile.Index, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return "", compile.Index{}, false
	}
	return a.cur.version, a.cur.index, true
}

// Index is a version's index: from memory, else from v<version>/bundle.json
// while that version is on disk. The index is shared: do not modify it.
func (a *Applier) Index(version string) (compile.Index, bool) {
	if !layout.ValidVersion(version) {
		return compile.Index{}, false
	}
	a.mu.Lock()
	idx, ok := a.indexes[version]
	a.mu.Unlock()
	if ok {
		return idx, true
	}
	idx, err := readIndex(a.cfg.LiveDir, version)
	if err != nil {
		return compile.Index{}, false
	}
	a.cacheIndex(version, idx)
	return idx, true
}

// Run verifies an unverified live version once per AMI session, until ctx
// is done; it returns nil then.
func (a *Applier) Run(ctx context.Context) error {
	t := time.NewTicker(a.pollEvery)
	defer t.Stop()
	for {
		a.mu.Lock()
		cur := a.cur
		pending := cur != nil && !cur.verified
		var tried time.Time
		if pending {
			tried = cur.tried
		}
		a.mu.Unlock()
		if st := a.cfg.AMI.State(); pending && st.Up && !st.Since.Equal(tried) {
			a.op.Lock()
			if a.current() == cur && !cur.verified {
				_ = a.verifyLive(ctx)
			}
			a.op.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// verifyLive reloads and verifies the unverified live version on the
// session that is up now, rolling back to its last verified version on a
// failure. The caller holds op.
func (a *Applier) verifyLive(ctx context.Context) error {
	cur := a.current()
	a.mu.Lock()
	cur.tried = a.cfg.AMI.State().Since
	a.mu.Unlock()
	err := a.reload(ctx, cur.plan)
	if err == nil {
		err = a.verify(ctx, cur.index)
	}
	if err != nil {
		if cur.prev != nil {
			a.rollBack(ctx, cur, err)
			return err
		}
		// Nothing to roll back to: it stays live, unverified, with the
		// error; the next AMI session tries again.
		a.failed(cur.version, err)
		return err
	}
	a.mu.Lock()
	prev, restored := cur.prev, cur.restored
	cur.verified, cur.plan, cur.prev = true, nil, nil
	a.mu.Unlock()
	if !restored {
		// A version a rollback restored was not applied: the failed
		// version stays the last apply, with its error.
		a.succeeded(cur.version)
	}
	a.cfg.Log.Info("live version verified", "version", cur.version)
	a.prune(cur.version, prevVersion(prev))
	return nil
}

// rollBack swaps failed's last verified version back in after err, reloads
// it and restores its account; with nothing to roll back to, failed stays
// live. Either way the failure is recorded.
func (a *Applier) rollBack(ctx context.Context, failed *live, err error) {
	prev := failed.prev
	if prev == nil {
		a.setLive(failed)
		a.failed(failed.version, err)
		a.cfg.Log.Info("version failed with nothing to roll back to; it stays live", "version", failed.version, "error", err.Error())
		return
	}
	if serr := a.swap(prev.version); serr != nil {
		a.cfg.Log.Error(serr, "rolling back: the swap failed", "version", prev.version)
	}
	if rerr := a.reloadPatiently(ctx, failed.plan); rerr != nil {
		a.cfg.Log.Info("rolling back: a reload failed", "version", prev.version, "error", rerr.Error())
	}
	if prev.account != nil {
		a.setAccount(prev.account)
	}
	a.mu.Lock()
	prev.restored = true
	a.mu.Unlock()
	a.setLive(prev)
	a.failed(failed.version, err)
	a.cfg.Log.Info("version rolled back", "failed", failed.version, "live", prev.version, "error", err.Error())
}

// reloadPlan lists the modules to reload to go from any of the versions
// from to files: all of them when there is none, or one cannot be read.
func (a *Applier) reloadPlan(files map[string][]byte, from ...*live) []string {
	next := map[string][]byte{}
	for p, b := range files {
		if name, ok := strings.CutPrefix(p, compile.TreeAsterisk+"/"); ok {
			next[name] = b
		}
	}
	var olds []map[string][]byte
	all := true
	for _, l := range from {
		if l == nil {
			continue
		}
		old, err := asteriskFiles(a.cfg.LiveDir, l.version)
		if err != nil {
			a.cfg.Log.Info("cannot read a previous version; reloading everything", "version", l.version, "error", err.Error())
			olds, all = nil, true
			break
		}
		olds, all = append(olds, old), false
	}
	changed := func(name string) bool {
		if all {
			return true
		}
		for _, old := range olds {
			o, inOld := old[name]
			n, inNext := next[name]
			if inOld != inNext || string(o) != string(n) {
				return true
			}
		}
		return false
	}
	plan := []string{"res_pjsip.so", "pbx_config.so"}
	for _, o := range owners {
		if changed(o.file) {
			plan = append(plan, o.module)
		}
	}
	if changed("manager.conf") {
		plan = append(plan, managerModule)
	}
	return plan
}

// reload runs the plan in order, stopping at the first error.
func (a *Applier) reload(ctx context.Context, plan []string) error {
	for _, m := range plan {
		rctx, cancel := context.WithTimeout(ctx, a.cfg.VerifyTimeout)
		err := a.cfg.AMI.Reload(rctx, m)
		cancel()
		if err != nil {
			return fmt.Errorf("reload %s: %w", m, err)
		}
	}
	return nil
}

// reloadPatiently is reload for a rollback: the session may be logging in
// again (a new account was just handed to AMI), so a reload that finds no
// session is tried again until VerifyTimeout.
func (a *Applier) reloadPatiently(ctx context.Context, plan []string) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.VerifyTimeout)
	defer cancel()
	for _, m := range plan {
		for {
			err := a.cfg.AMI.Reload(ctx, m)
			if err == nil {
				break
			}
			if !errors.Is(err, ami.ErrNotConnected) {
				return fmt.Errorf("reload %s: %w", m, err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("reload %s: %w", m, err)
			case <-time.After(a.verifyEvery):
			}
		}
	}
	return nil
}

// verify retries until Asterisk's endpoints are exactly the index's
// rendered devices and its dialplan has both patchbay contexts, or
// VerifyTimeout passes. It then reports the last check that completed (the
// mismatch), not a check the deadline cut short; the deadline itself only
// when no check completed.
func (a *Applier) verify(ctx context.Context, idx compile.Index) error {
	var want []string
	for id, d := range idx.Devices {
		if d.Version != "" {
			want = append(want, id)
		}
	}
	slices.Sort(want)
	ctx, cancel := context.WithTimeout(ctx, a.cfg.VerifyTimeout)
	defer cancel()
	var last error
	for {
		err := a.check(ctx, want)
		if err == nil {
			return nil
		}
		if ctx.Err() == nil || last == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("verify: %w", last)
		case <-time.After(a.verifyEvery):
		}
	}
}

func (a *Applier) check(ctx context.Context, want []string) error {
	got, err := a.cfg.AMI.Endpoints(ctx)
	if err != nil {
		return err
	}
	contexts, err := a.cfg.AMI.DialplanContexts(ctx)
	if err != nil {
		return err
	}
	var parts, endpoints []string
	if missing := minus(want, got); len(missing) > 0 {
		endpoints = append(endpoints, "missing "+strings.Join(missing, ", "))
	}
	if unexpected := minus(got, want); len(unexpected) > 0 {
		endpoints = append(endpoints, "unexpected "+strings.Join(unexpected, ", "))
	}
	if len(endpoints) > 0 {
		parts = append(parts, "endpoints "+strings.Join(endpoints, "; "))
	}
	if missing := minus([]string{compile.DevicesContext, compile.UnavailableContext}, contexts); len(missing) > 0 {
		parts = append(parts, "dialplan missing "+strings.Join(missing, ", "))
	}
	if len(parts) > 0 {
		return errors.New(strings.Join(parts, "; "))
	}
	return nil
}

// minus is a without b's elements, in a's order.
func minus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// grantsOf is one grant per rendered softphone, sorted by device ID; never
// nil.
func grantsOf(version string, r intake.Resolved) []provision.Grant {
	grants := []provision.Grant{}
	for _, id := range slices.Sorted(maps.Keys(r.Tokens)) {
		d, ok := r.Bundle.Devices[id]
		if !ok {
			continue
		}
		grants = append(grants, provision.Grant{
			Device: id, Token: r.Tokens[id], File: d.ConfigFile, BundleVersion: version, Version: d.Version,
		})
	}
	return grants
}

func (a *Applier) current() *live {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

func (a *Applier) setLive(l *live) {
	a.mu.Lock()
	a.cur = l
	a.status.AppliedVersion = l.version
	a.mu.Unlock()
	a.cfg.Metrics.SetAppliedVersion(l.version)
}

func (a *Applier) cacheIndex(v string, idx compile.Index) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.indexes[v] = idx
}

// setAccount hands AMI the account when it differs from the last one.
func (a *Applier) setAccount(acct *account) {
	a.mu.Lock()
	same := a.acct != nil && *a.acct == *acct
	if !same {
		a.acct = acct
	}
	a.mu.Unlock()
	if !same {
		a.cfg.AMI.SetAccount(acct.username, acct.password)
	}
}

// succeeded records an apply of version that left it live.
func (a *Applier) succeeded(version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	a.status.LastApplyVersion, a.status.LastApplyError, a.status.LastApplyAt = version, "", &now
}

// failed records an apply of version that failed.
func (a *Applier) failed(version string, err error) {
	a.mu.Lock()
	now := a.cfg.Now()
	a.status.LastApplyVersion, a.status.LastApplyError, a.status.LastApplyAt = version, err.Error(), &now
	a.mu.Unlock()
	a.cfg.Metrics.ApplyFailed()
}

func prevVersion(l *live) string {
	if l == nil {
		return ""
	}
	return l.version
}
