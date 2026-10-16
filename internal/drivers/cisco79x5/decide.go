package cisco79x5

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Timeouts of the rule table (design §8.3). Each fires at the limit: "after
// 5 min" matches once now - start >= 5m, "within 3 min" while now - t < 3m.
const (
	// trustResetAfter: NeedsTrustReset once the first .sgn request has made
	// no progress for this long.
	trustResetAfter = 3 * time.Minute
	// firmwareWindow: Upgrading while a firmware file was fetched within it.
	firmwareWindow = 3 * time.Minute
	// upgradeStuckAfter: an upgrade this old is stuck.
	upgradeStuckAfter = 20 * time.Minute
	// registeringFor: Registering asks a human after this long.
	registeringFor = 5 * time.Minute
	// resyncEvery: at most one resync of a version per this long.
	resyncEvery = 5 * time.Minute
	// resyncAttempts: after this many resyncs of a version, ask a human.
	resyncAttempts = 2
	// unreachableGrace: Unreachable asks a human after this long.
	unreachableGrace = 10 * time.Minute

	// slowRequeue re-checks a phone that is Unseen or Registered.
	slowRequeue = 10 * time.Minute
	// applyPendingRequeue re-checks a phone waiting for the agent to apply
	// its new config.
	applyPendingRequeue = 15 * time.Second
	// inCallRequeue re-checks a phone waiting for its call to end.
	inCallRequeue = 30 * time.Second

	// logLines is how many of the phone's latest log lines P12 quotes.
	logLines = 3
)

// Patterns over /DeviceLogX lines for NeedsTrustReset (row 5). Bench-
// refinable (plan §8). "Load Authentication Failed" is a bad firmware image,
// not a trust problem, so a line that matches loadAuthPattern never counts.
var (
	trustErrorPattern = regexp.MustCompile(`(?i)(trust ?list|\bITL\b|\bCTL\b|(config|cnf).*(verif|signat))`)
	loadAuthPattern   = regexp.MustCompile(`(?i)load auth`)
)

// reasonNoRuleMatched is the defensive branch's reason; no input reaches it
// (TestDecideTotal).
const reasonNoRuleMatched = "NoRuleMatched"

// rule is one row of the state machine: the first row whose match holds
// decides.
type rule struct {
	state   drivers.State
	match   func(*input) bool
	outcome func(*input) drivers.Decision
}

// rules is the cisco79x5 state machine, design §8.3 in order (first match
// wins); docs/plans/release-a.md §4 T1 has every predicate and outcome.
var rules = []rule{
	{drivers.StateInvalid, (*input).isInvalid, (*input).invalid},
	{drivers.StateUnknown, (*input).isUnknown, (*input).unknown},
	{drivers.StateUnseen, (*input).isUnseen, (*input).unseen},
	{drivers.StateNeedsFirmware, (*input).isNeedsFirmware, (*input).needsFirmware},
	{drivers.StateNeedsTrustReset, (*input).isNeedsTrustReset, (*input).needsTrustReset},
	{drivers.StateUpgrading, (*input).isUpgrading, (*input).upgrading},
	{drivers.StateRegistering, (*input).isRegistering, (*input).registering},
	{drivers.StateConfigStale, (*input).isConfigStale, (*input).configStale},
	{drivers.StateRegistered, (*input).isRegistered, (*input).registered},
	{drivers.StateUnreachable, (*input).isUnreachable, (*input).unreachable},
}

// Decide is one step of the phone's state machine. See drivers.Driver.Decide.
func (d *Driver) Decide(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) drivers.Decision {
	in := d.newInput(dev, obs, s, now)
	if i := in.firstRule(); i >= 0 {
		return rules[i].outcome(in)
	}
	return in.noRuleMatched()
}

// input is everything the rules read: the arguments, the folded memory and
// the definitions of plan §4 T1, each computed once.
type input struct {
	dev drivers.Device
	obs drivers.Observations
	s   drivers.DecideSettings
	now time.Time

	p      drivers.Profile
	served bool                 // the driver serves dev.Model
	target string               // the target load's name
	load   drivers.FirmwareLoad // the target load, when loadOK
	loadOK bool                 // the target load is in the manifest for the model

	defLoad   drivers.FirmwareLoad // the model's default load, when defOK
	defOK     bool
	defStaged bool // the default load is FirmwareAvailable

	d    *agentapi.DeviceObservations // nil when no sensor has seen it
	m    drivers.Memory               // dev.Memory folded with the observations
	live bool                         // a live contact

	available   bool                // the target load is staged (Missing is empty)
	current     bool                // the last config fetch is dev.ConfigVersion
	fwWrong     bool                // it reports a load that is not the target
	fwRequested bool                // it asked for a target-load file that was not there
	lastFw      time.Time           // the latest served firmware fetch (whole seconds), or zero
	sgnFirst    time.Time           // the first .sgn request since its last progress, or zero
	web         *agentapi.WebScrape // the scrape, when freshScrape says it still holds
	trustLog    bool                // the fresh scrape's log shows a trust error
}

func (d *Driver) newInput(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) *input {
	in := &input{dev: dev, obs: obs, s: s, now: now, d: obs.Device}
	in.p, in.served = d.profile(dev.Model)
	in.target = targetName(dev, in.p)
	in.load, in.loadOK = d.load(dev.Model, in.target)
	in.defLoad, in.defOK = d.load(dev.Model, in.p.Provisioning.DefaultFirmware)
	in.defStaged = in.defOK && len(in.defLoad.Missing(obs.Firmware)) == 0

	in.m = fold(dev, obs, now, in.p, in.load)
	in.live = liveContact(in.d, now, in.p)
	in.available = in.loadOK && len(in.load.Missing(obs.Firmware)) == 0
	in.current = in.m.LastConfigFetch != nil && in.m.LastConfigFetch.Version == dev.ConfigVersion
	in.fwWrong = in.m.Firmware != "" && !runsLoad(in.m.Firmware, in.load)

	if in.d == nil {
		return in
	}
	in.lastFw = lastFirmwareFetch(in.d.Fetches)
	in.web = freshScrape(in.d.Web, in.m, in.lastFw)
	var since time.Time // the last config fetch, or zero: "at or after"
	if in.m.LastConfigFetch != nil {
		since = in.m.LastConfigFetch.At.Time
	}
	// progress is the phone's last sign of progress: its last config fetch
	// or firmware download. A .sgn request before it is old news.
	progress := since
	if in.lastFw.After(progress) {
		progress = in.lastFw
	}
	loadFiles := map[string]bool{}
	for _, f := range in.load.Files {
		loadFiles[f.Name] = true
	}
	sgn := ConfigFileName(dev.MAC) + ".sgn"
	for _, f := range in.d.Fetches {
		at := drivers.FloorSecond(f.At)
		if f.Result == agentapi.FetchNotFound && loadFiles[f.File] && !at.Before(since) {
			in.fwRequested = true
		}
		if f.File == sgn && (progress.IsZero() || at.After(progress)) &&
			(in.sgnFirst.IsZero() || f.At.Before(in.sgnFirst)) {
			in.sgnFirst = f.At
		}
	}
	if in.web != nil {
		for _, l := range in.web.DeviceLog {
			if trustErrorPattern.MatchString(l) && !loadAuthPattern.MatchString(l) {
				in.trustLog = true
			}
		}
	}
	return in
}

// firstRule returns the index of the first rule that matches, or -1.
func (in *input) firstRule() int {
	for i, r := range rules {
		if r.match(in) {
			return i
		}
	}
	return -1
}

// ---- Predicates, in table order ----

func (in *input) isInvalid() bool {
	reason, _ := in.invalidReason()
	return reason != ""
}

func (in *input) isUnknown() bool {
	return in.obs.AgentProblem(in.now, in.s.StaleAfter) != ""
}

func (in *input) isUnseen() bool {
	return in.m.FirstSeen == nil
}

func (in *input) isNeedsFirmware() bool {
	return (in.fwWrong || in.fwRequested) && !in.available
}

// isNeedsTrustReset: no contact, and it has asked only for its signed config
// for 3 minutes since its last progress (a config fetch or a firmware
// download: the download a factory reset starts is progress), or a fresh
// scrape's log shows a trust error. A scrape from before that progress, or
// the older success a failed scrape carries, is not read: while the phone
// flashes its web page is down.
func (in *input) isNeedsTrustReset() bool {
	if in.live {
		return false
	}
	return (!in.sgnFirst.IsZero() && in.now.Sub(in.sgnFirst) >= trustResetAfter) || in.trustLog
}

func (in *input) isUpgrading() bool {
	if !in.lastFw.IsZero() && in.now.Sub(in.lastFw) < firmwareWindow {
		return true
	}
	return in.m.UpgradeStartedAt != nil && !in.live && !fetchedAfter(in.m, in.upgradeRef())
}

func (in *input) isRegistering() bool {
	return !in.live && (in.m.LastRegistered == nil || fetchedAfter(in.m, in.m.LastRegistered.Time))
}

func (in *input) isConfigStale() bool {
	return in.live && (!in.current || in.fwWrong)
}

func (in *input) isRegistered() bool {
	return in.live
}

func (in *input) isUnreachable() bool {
	return !in.live && in.m.LastRegistered != nil && !fetchedAfter(in.m, in.m.LastRegistered.Time)
}

// upgradeRef is the later of the last firmware fetch and UpgradeStartedAt, so
// an agent restart that empties the fetch log mid-upgrade does not end the
// upgrade on a config fetch from before it.
func (in *input) upgradeRef() time.Time {
	ref := in.lastFw
	if u := in.m.UpgradeStartedAt; u != nil && u.After(ref) {
		ref = u.Time
	}
	return ref
}

// ---- Outcomes ----

// invalidReason is why the spec breaks a driver rule, in the order Ready
// reports it, with Ready's message; "" when it breaks none.
func (in *input) invalidReason() (reason, message string) {
	claim := func(fallback string) string {
		if in.dev.ClaimMessage != "" {
			return in.dev.ClaimMessage
		}
		return fallback
	}
	switch {
	case !in.served:
		return ReasonUnknownModel, fmt.Sprintf("cisco79x5 does not serve model %q", in.dev.Model)
	case in.dev.MACConflict:
		return ReasonMACConflict, claim("an older Phone has MAC " + in.dev.MAC)
	case in.dev.NumberConflict:
		return ReasonNumberConflict, claim("an older resource holds number " + in.dev.Number)
	case in.dev.NumberOutOfPlan:
		return v1alpha1.ReasonNumberNotInPlan, claim("number " + in.dev.Number + " is not in the number plan's device range")
	case !in.loadOK:
		return ReasonUnknownFirmware, textP1(in.target, in.dev.Model)
	}
	return "", ""
}

// Row 1: the spec breaks a driver rule.
func (in *input) invalid() drivers.Decision {
	reason, msg := in.invalidReason()
	m := in.m
	in.dropRequest(&m)
	unknown := func(t string) metav1.Condition {
		return drivers.Condition(t, metav1.ConditionUnknown, ReasonInvalid, "")
	}
	conflict := func(t string, set bool, reason string) metav1.Condition {
		if set {
			return drivers.Condition(t, metav1.ConditionTrue, reason, in.dev.ClaimMessage)
		}
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNoConflict, "")
	}
	return drivers.Decision{
		State: drivers.StateInvalid,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg),
			unknown(v1alpha1.ConditionFirmwareAvailable),
			unknown(v1alpha1.ConditionFirmwareCurrent),
			unknown(v1alpha1.ConditionProvisioned),
			unknown(v1alpha1.ConditionRegistered),
			unknown(v1alpha1.ConditionConfigCurrent),
			conflict(v1alpha1.ConditionNumberConflict, in.dev.NumberConflict, ReasonNumberConflict),
			conflict(v1alpha1.ConditionMACConflict, in.dev.MACConflict, ReasonMACConflict),
			in.unsupported(),
			in.labelShortened(),
		},
		StatusPatch: m,
		HumanAction: msg,
	}
}

// Row 2: the agent cannot be trusted. Only Ready and Registered are set;
// memory and any request are untouched; the last known state is kept.
func (in *input) unknown() drivers.Decision {
	reason := in.obs.AgentProblem(in.now, in.s.StaleAfter)
	status, msg := metav1.ConditionUnknown, "cannot reach the PBX agent; registration unknown"
	if reason == v1alpha1.ReasonPBXDown {
		status, msg = metav1.ConditionFalse, "Asterisk is down"
	}
	state := in.dev.LastState
	if state == "" {
		state = drivers.StateUnknown
	}
	return drivers.Decision{
		State: state,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, status, reason, msg),
			drivers.Condition(v1alpha1.ConditionRegistered, status, reason, msg),
		},
		StatusPatch: in.dev.Memory.DeepCopy(),
	}
}

// Row 3: never seen.
func (in *input) unseen() drivers.Decision {
	m := in.endUpgrade()
	in.dropRequest(&m)
	human := textP2(in.dev.MAC, in.s.PBXAddress)
	return in.decision(drivers.StateUnseen, ReasonUnseen, human, m, human, slowRequeue)
}

// Row 4: it runs, or asked for, a load that is not staged.
func (in *input) needsFirmware() drivers.Decision {
	m := in.endUpgrade()
	in.dropRequest(&m)
	human := textP3(in.load.Source, in.load.Missing(in.obs.Firmware))
	if pending := in.load.PendingFiles(); len(pending) > 0 {
		human = textP4(in.load.Source, pending)
	}
	return in.decision(drivers.StateNeedsFirmware, ReasonNeedsFirmware, human, m, human, 0)
}

// Row 5: it holds a trust list from its old CUCM.
func (in *input) needsTrustReset() drivers.Decision {
	m := in.endUpgrade()
	in.dropRequest(&m)
	human := textP5
	if !in.defStaged {
		human = textP6(in.defaultSource())
	}
	return in.decision(drivers.StateNeedsTrustReset, ReasonNeedsTrustReset, human, m, human, 0)
}

// Row 6: firmware is downloading, or the upgrade it started has not ended
// with a config fetch. No actions ever: never resync or restart mid-upgrade.
func (in *input) upgrading() drivers.Decision {
	m := in.m
	in.dropRequest(&m)
	if m.UpgradeStartedAt == nil {
		// Only the firmware-fetch clause matches without UpgradeStartedAt,
		// so there are fetches; the earliest of the last 20 min is the start.
		start := drivers.FloorSecond(in.now)
		var fetches []agentapi.Fetch
		if in.d != nil {
			fetches = in.d.Fetches
		}
		for _, f := range fetches {
			if f.Result == agentapi.FetchServed && isFirmwareFile(f.File) &&
				in.now.Sub(f.At) <= upgradeStuckAfter && drivers.FloorSecond(f.At).Before(start) {
				start = drivers.FloorSecond(f.At)
			}
		}
		m.UpgradeStartedAt = timePtr(start)
	}
	human := textP7
	if in.now.Sub(m.UpgradeStartedAt.Time) >= upgradeStuckAfter {
		human = textP8
	}
	requeue := in.soonest(in.sgnDeadline(), deadline(in.lastFw, firmwareWindow), m.UpgradeStartedAt.Add(upgradeStuckAfter))
	return in.decision(drivers.StateUpgrading, ReasonUpgrading, human, m, human, requeue)
}

// Row 7: it has the config it needs (or none yet) but has not registered.
func (in *input) registering() drivers.Decision {
	m := in.endUpgrade()
	in.dropRequest(&m)
	start := m.FirstSeen.Time
	if m.LastConfigFetch != nil {
		start = m.LastConfigFetch.At.Time
	}
	if in.now.Sub(start) < registeringFor {
		requeue := in.soonest(in.sgnDeadline(), start.Add(registeringFor))
		return in.decision(drivers.StateRegistering, ReasonRegistering, "waiting for it to register", m, "", requeue)
	}
	var human string
	switch {
	case m.LastConfigFetch == nil:
		human = textP11(in.dev.MAC, in.s.PBXAddress)
	case in.fwWrong:
		human = textP10(m.Firmware, in.target)
	case in.adminVLAN() != "":
		human = textP9(in.adminVLAN(), in.defStaged, in.defaultSource())
	default:
		human = textP12(int(in.now.Sub(start)/time.Minute), in.d)
	}
	return in.decision(drivers.StateRegistering, ReasonRegistering, human, m, human, in.soonest(in.sgnDeadline()))
}

// Row 8: registered, but on an old config version or the wrong load. A
// resync makes it re-read its config (and upgrade); never during a call or
// before its new config is live, at most once per version per 5 minutes,
// and a human after two.
func (in *input) configStale() drivers.Decision {
	m := in.endUpgrade()
	var (
		actions []drivers.Action
		human   string
		ready   string
		requeue time.Duration
		waiting bool
	)
	switch {
	case !in.obs.ConfigLive(): // (i)
		if failure := in.obs.ApplyFailure(); failure != "" {
			ready = "the PBX agent refused the new config: " + failure
		} else {
			ready, requeue = "waiting for the PBX agent to apply the new config", applyPendingRequeue
		}
	case in.d.Calls > 0: // (ii)
		ready, requeue, waiting = "waiting for the call to end before a resync", inCallRequeue, true
	case in.requestPending(): // (iii)
		actions = in.requestedResync(&m)
		ready, requeue = "resync sent on request", resyncEvery
	case !in.p.Provisioning.ResyncByNotify: // (iv)
		human = textP13
	case in.resyncedV() && in.now.Sub(m.LastAction.At.Time) < resyncEvery: // (v) wait
		ready = "resync sent; waiting for it to fetch its config"
		requeue = in.soonest(m.LastAction.At.Add(resyncEvery))
	case in.resyncedV() && m.LastAction.Attempts >= resyncAttempts: // (v) human
		human = textP13
	case in.resyncedV() && m.LastAction.Attempts == 1: // (v) second attempt
		actions = in.resync(&m, 2)
		ready, requeue = "resync sent (attempt 2)", resyncEvery
	default: // (vi), and (v) after only requested resyncs
		actions = in.resync(&m, 1)
		ready, requeue = "resync sent (attempt 1)", resyncEvery
	}
	if human != "" {
		ready = human
	}
	return drivers.Decision{
		State:        drivers.StateConfigStale,
		Conditions:   in.standard(drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonConfigStale, ready), m, waiting),
		Actions:      actions,
		StatusPatch:  m,
		HumanAction:  human,
		RequeueAfter: requeue,
	}
}

// Row 9: registered, current config, target load.
func (in *input) registered() drivers.Decision {
	m := in.endUpgrade()
	var actions []drivers.Action
	if in.requestPending() && in.d.Calls == 0 && in.obs.ConfigLive() {
		actions = in.requestedResync(&m)
	}
	dec := in.decision(drivers.StateRegistered, ReasonRegistered, "", m, "", slowRequeue)
	dec.Conditions[0].Status = metav1.ConditionTrue
	dec.Actions = actions
	return dec
}

// Row 10: it was registered, and the contact is gone.
func (in *input) unreachable() drivers.Decision {
	m := in.endUpgrade()
	in.dropRequest(&m)
	since := m.LastRegistered.Time
	if in.now.Sub(since) < unreachableGrace {
		requeue := in.soonest(in.sgnDeadline(), since.Add(unreachableGrace))
		return in.decision(drivers.StateUnreachable, ReasonUnreachable,
			"its registration lapsed; waiting out the 10 min grace", m, "", requeue)
	}
	return in.decision(drivers.StateUnreachable, ReasonUnreachable, textP14, m, textP14, in.soonest(in.sgnDeadline()))
}

// noRuleMatched is the defensive branch: rows 7 to 10 partition every seen
// device, so it is never reached (TestDecideTotal).
func (in *input) noRuleMatched() drivers.Decision {
	return drivers.Decision{
		State: drivers.StateUnknown,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionUnknown, reasonNoRuleMatched, "no rule matched"),
		},
		StatusPatch: in.dev.Memory.DeepCopy(),
	}
}

// ---- Helpers ----

// decision builds a Decision for rows 3 to 10 with the standard conditions
// and Ready False.
func (in *input) decision(state drivers.State, reason, readyMsg string, m drivers.Memory, human string, requeue time.Duration) drivers.Decision {
	return drivers.Decision{
		State:        state,
		Conditions:   in.standard(drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, reason, readyMsg), m, false),
		StatusPatch:  m,
		HumanAction:  human,
		RequeueAfter: requeue,
	}
}

// endUpgrade returns the folded memory with UpgradeStartedAt cleared: every
// row but Invalid, Unknown and Upgrading ends an upgrade.
func (in *input) endUpgrade() drivers.Memory {
	m := in.m
	m.UpgradeStartedAt = nil
	return m
}

// requestPending reports a request annotation not yet acted on.
func (in *input) requestPending() bool {
	return in.dev.Request != "" && in.dev.Request != in.dev.Memory.HandledRequest
}

// dropRequest marks a pending request handled without acting on it: rows
// other than ConfigStale and Registered drop requests (the controller emits
// RequestIgnored).
func (in *input) dropRequest(m *drivers.Memory) {
	if in.requestPending() {
		m.HandledRequest = in.dev.Request
	}
}

// resyncedV reports whether the last action was a resync to this device's
// current config version.
func (in *input) resyncedV() bool {
	a := in.m.LastAction
	return a != nil && a.Kind == v1alpha1.ActionResync && a.Version == in.dev.ConfigVersion
}

// resync records a resync to the current config version in m and returns it.
func (in *input) resync(m *drivers.Memory, attempts int32) []drivers.Action {
	v := in.dev.ConfigVersion
	m.LastAction = &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: v, At: metav1.NewTime(drivers.CeilSecond(in.now)), Attempts: attempts}
	return []drivers.Action{{Kind: v1alpha1.ActionResync, Version: v}}
}

// requestedResync answers the pending request with a resync to the current
// version and marks it handled. A requested resync is not counted: it keeps
// the attempts of an earlier resync of this version (0 if none).
func (in *input) requestedResync(m *drivers.Memory) []drivers.Action {
	var attempts int32
	if in.resyncedV() {
		attempts = in.m.LastAction.Attempts
	}
	m.HandledRequest = in.dev.Request
	return in.resync(m, attempts)
}

// sgnDeadline is when a pending .sgn request becomes NeedsTrustReset, or zero.
func (in *input) sgnDeadline() time.Time {
	if in.live {
		return time.Time{}
	}
	return deadline(in.sgnFirst, trustResetAfter)
}

// deadline is t+d, or zero when t is zero.
func deadline(t time.Time, d time.Duration) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Add(d)
}

// soonest is the time from now to the nearest deadline still ahead, or 0.
func (in *input) soonest(deadlines ...time.Time) time.Duration {
	var out time.Duration
	for _, t := range deadlines {
		if left := t.Sub(in.now); !t.IsZero() && left > 0 && (out == 0 || left < out) {
			out = left
		}
	}
	return out
}

// adminVLAN is the phone's Admin. VLAN ID from a fresh scrape of its web
// page (a factory reset clears it), or "".
func (in *input) adminVLAN() string {
	if in.web == nil || in.web.NetworkConfiguration == nil {
		return ""
	}
	return in.web.NetworkConfiguration.AdminVLAN
}

// defaultSource is where the model's default load comes from, for the texts
// that send a human to stage it.
func (in *input) defaultSource() string {
	if in.defOK {
		return in.defLoad.Source
	}
	return in.load.Source
}

// ---- Conditions ----

// standard is the condition list of rows 3 to 10, in the driver's fixed
// order. m is the Decision's memory (for the resync count); waiting says
// the phone waits for a call to end.
func (in *input) standard(ready metav1.Condition, m drivers.Memory, waiting bool) []metav1.Condition {
	return []metav1.Condition{
		ready,
		in.firmwareAvailable(),
		in.firmwareCurrent(),
		in.provisioned(),
		in.registeredCondition(),
		in.configCurrent(m, waiting),
		drivers.Condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, ReasonNoConflict, ""),
		drivers.Condition(v1alpha1.ConditionMACConflict, metav1.ConditionFalse, ReasonNoConflict, ""),
		in.unsupported(),
		in.labelShortened(),
	}
}

func (in *input) firmwareAvailable() metav1.Condition {
	const t = v1alpha1.ConditionFirmwareAvailable
	switch {
	case in.available:
		return drivers.Condition(t, metav1.ConditionTrue, ReasonStaged, in.target)
	case len(in.load.PendingFiles()) > 0:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonManifestPending,
			"the manifest has no SHA-256 yet for "+strings.Join(in.load.PendingFiles(), ", "))
	default:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNotStaged,
			"missing: "+strings.Join(in.load.Missing(in.obs.Firmware), ", "))
	}
}

func (in *input) firmwareCurrent() metav1.Condition {
	const t = v1alpha1.ConditionFirmwareCurrent
	switch {
	case in.m.Firmware == "":
		return drivers.Condition(t, metav1.ConditionUnknown, ReasonNotReported, "it has not reported its load yet")
	case in.fwWrong:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonOtherLoad,
			fmt.Sprintf("running %s, target %s", in.m.Firmware, in.target))
	default:
		return drivers.Condition(t, metav1.ConditionTrue, ReasonRunning, in.target)
	}
}

func (in *input) provisioned() metav1.Condition {
	const t = v1alpha1.ConditionProvisioned
	f := in.m.LastConfigFetch
	switch {
	case f == nil:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNotFetched, "it has not fetched "+ConfigFileName(in.dev.MAC))
	case in.current:
		return drivers.Condition(t, metav1.ConditionTrue, ReasonFetched, "fetched version "+shortVersion(f.Version))
	default:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonOldVersion,
			fmt.Sprintf("fetched version %s, current %s", shortVersion(f.Version), shortVersion(in.dev.ConfigVersion)))
	}
}

func (in *input) registeredCondition() metav1.Condition {
	const t = v1alpha1.ConditionRegistered
	switch {
	case in.live:
		r := in.d.Registration
		msg := r.Contact
		if r.UserAgent != "" {
			msg += ", " + r.UserAgent
		}
		return drivers.Condition(t, metav1.ConditionTrue, ReasonContact, msg)
	case in.d != nil && in.d.Registration != nil && drivers.RegistrationLive(in.d.Registration.ExpiresAt, in.now) && qualifyFails(in.d.Registration, in.p):
		return drivers.Condition(t, metav1.ConditionFalse, ReasonQualifyFailed, in.d.Registration.Contact+" does not answer OPTIONS")
	default:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNoContact, "no SIP registration")
	}
}

func (in *input) configCurrent(m drivers.Memory, waiting bool) metav1.Condition {
	const t = v1alpha1.ConditionConfigCurrent
	f := in.m.LastConfigFetch
	switch {
	case in.current && in.obs.ConfigLive():
		return drivers.Condition(t, metav1.ConditionTrue, ReasonCurrent, "version "+shortVersion(in.dev.ConfigVersion))
	case in.obs.ApplyFailure() != "":
		return drivers.Condition(t, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, in.obs.ApplyFailure())
	case !in.obs.ConfigLive():
		return drivers.Condition(t, metav1.ConditionFalse, v1alpha1.ReasonApplyPending, "the PBX agent has not applied the latest config yet")
	case f == nil:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNotFetched, "it has not fetched "+ConfigFileName(in.dev.MAC))
	}
	msg := fmt.Sprintf("running version %s, current %s", shortVersion(f.Version), shortVersion(in.dev.ConfigVersion))
	if a := m.LastAction; a != nil && a.Kind == v1alpha1.ActionResync && a.Version == in.dev.ConfigVersion && a.Attempts > 0 {
		msg += fmt.Sprintf("; resyncs sent: %d", a.Attempts)
	}
	if waiting {
		msg += "; waiting for the call to end"
	}
	return drivers.Condition(t, metav1.ConditionFalse, ReasonStale, msg)
}

func (in *input) unsupported() metav1.Condition {
	return drivers.Condition(v1alpha1.ConditionUnsupported, metav1.ConditionFalse, ReasonAllSupported, "")
}

func (in *input) labelShortened() metav1.Condition {
	const t = v1alpha1.ConditionLabelShortened
	// What the phone shows: the label as Compile renders it (drivers.UserText:
	// runs of "@" collapsed), then the 79x5's own limits.
	l := in.dev.Label
	shown := drivers.UserText(l)
	screen, line := ScreenLabel(shown), LineLabel(shown)
	if screen == l && line == l {
		return drivers.Condition(t, metav1.ConditionFalse, ReasonAsWritten, "")
	}
	return drivers.Condition(t, metav1.ConditionTrue, ReasonShortened,
		fmt.Sprintf(`shows "%s" on screen and "%s" on its line`, screen, line))
}

// shortVersion is a config version cut for messages.
func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// ---- Human-action texts (plan §4 T1, verbatim) ----

const (
	factoryReset = "hold # while powering on, then 123456789*0#"

	textP5  = "factory reset: " + factoryReset
	textP7  = "upgrading: do not unplug"
	textP8  = "upgrade stuck: check the phone's screen"
	textP13 = "press Settings then **#**, or power-cycle it"
	textP14 = "not responding; power-cycle it"
)

func textP1(target, model string) string {
	return fmt.Sprintf("no firmware load %s for %s in the cisco79x5 manifest", target, model)
}

func textP2(mac, pbx string) string {
	return fmt.Sprintf("connect it to a VLAN 20 switch port; it will request %s from %s", ConfigFileName(mac), pbx)
}

func textP3(source string, missing []string) string {
	return fmt.Sprintf("run `patchbay firmware load <zip>` with %s; missing: %s", source, strings.Join(missing, ", "))
}

func textP4(source string, pending []string) string {
	return fmt.Sprintf("the cisco79x5 firmware manifest has no SHA-256 yet for %s: put each file's sha256sum from %s "+
		"into internal/drivers/cisco79x5/firmware.go, release patchbay with it, then run `patchbay firmware load <zip>`",
		strings.Join(pending, ", "), source)
}

func textP6(source string) string {
	return fmt.Sprintf("stage firmware first: run `patchbay firmware load <zip>` with %s; "+
		"a factory reset with nothing to download leaves the phone stuck", source)
}

func textP9(vlan string, defaultStaged bool, source string) string {
	if defaultStaged {
		return fmt.Sprintf("its Admin. VLAN ID is %s but its switch port is untagged; "+
			"clear it with a factory reset (its settings menu is restricted): %s", vlan, factoryReset)
	}
	return fmt.Sprintf("its Admin. VLAN ID is %s but its switch port is untagged; "+
		"stage firmware first with `patchbay firmware load <zip>` (%s), then factory-reset it: %s", vlan, source, factoryReset)
}

func textP10(running, target string) string {
	return fmt.Sprintf("it runs %s, not %s: power-cycle it so it fetches its config and upgrades", running, target)
}

func textP11(mac, pbx string) string {
	return fmt.Sprintf("it has not fetched %s from %s: check its TFTP server (DHCP option 66)", ConfigFileName(mac), pbx)
}

// textP12 quotes the phone's latest log lines, or says why it cannot.
func textP12(minutes int, d *agentapi.DeviceObservations) string {
	head := fmt.Sprintf("fetched its config %d min ago but has not registered", minutes)
	var w *agentapi.WebScrape
	if d != nil {
		w = d.Web
	}
	switch {
	case w == nil:
		return head + "; its web page has not been read yet"
	case w.Error != "":
		return head + "; its web page could not be read: " + w.Error
	case len(w.DeviceLog) == 0:
		return head + "; its log is empty"
	}
	lines := w.DeviceLog[max(0, len(w.DeviceLog)-logLines):]
	return head + "; its log says: " + strings.Join(lines, " | ")
}
