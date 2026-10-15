package softphone

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Decide is one step of the softphone state machine (design §8.4; rows S1
// to S8 of docs/plans/release-a.md T2): the first row of rules whose
// predicate holds decides. Pure: no I/O, no clock but now.
func (*Driver) Decide(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) drivers.Decision {
	f := newFacts(dev, obs, s, now)
	for _, r := range rules {
		if r.match(f) {
			return r.outcome(f)
		}
	}
	return noRuleMatched(f)
}

// Human-action texts, verbatim (docs/plans/release-a.md, T1's list, S1-S3).
const (
	// textScan (S1) asks for the QR code to be scanned.
	textScan = "scan `patchbay softphone qr %s` in Linphone"
	// textRescan (S2) asks for it again after the settings changed.
	textRescan = "its settings changed: scan `patchbay softphone qr %s` in Linphone again"
	// textNotRegistering (S3) is shown from registerWithin after a fetch
	// with no registration (an addition to design §8.4).
	textNotRegistering = "Linphone fetched its settings %d min ago but has not registered: is it on the LAN?"
)

// Timers of the rule table.
const (
	// registerWithin is how long after fetching its settings Linphone may
	// take to register before a person is asked (S6).
	registerWithin = 5 * time.Minute
	// registeredRecheck is the slow re-check of a Registered softphone.
	registeredRecheck = 10 * time.Minute
	// lastRegisteredStep is how coarsely LastRegistered advances while a
	// contact stays live, so status is not rewritten at every poll.
	lastRegisteredStep = time.Minute
)

// rule is one row of the table: the state it reports, when it matches, and
// what it decides. Rows are tried in order and the first match wins.
type rule struct {
	row     string
	state   drivers.State
	match   func(*facts) bool
	outcome func(*facts) drivers.Decision
}

// rules is the softphone's rule table (design §8.4, docs/plans/release-a.md
// T2), first match wins:
//
//	S1 Invalid      the spec breaks a driver rule: model, MAC or number
//	                conflict, number out of plan
//	S2 Unknown      the agent is unreachable or stale, or Asterisk is down
//	S3 Issued       a reissue request: rotateToken, forget the config fetch
//	S4 Provisioned  a served fetch after the last rotation: rotateToken, so
//	                each QR code works once
//	S5 Issued       the current settings are not fetched (never, or only an
//	                older version)
//	S6 Provisioned  current settings, no registration since; a person is
//	                asked after registerWithin
//	S7 Registered   current settings and a live contact
//	S8 Offline      current settings, registered since, no contact now:
//	                Ready, because apps sleep
//
// S5 to S8 partition every softphone that reaches them (not current: S5;
// current: S6, S7 or S8), so the table is total; noRuleMatched is a
// defensive branch TestDecideTotal proves unreachable.
var rules = []rule{
	{row: "S1", state: drivers.StateInvalid, match: (*facts).isInvalid, outcome: (*facts).invalid},
	{row: "S2", state: drivers.StateUnknown, match: (*facts).hasAgentProblem, outcome: (*facts).unknown},
	{row: "S3", state: drivers.StateIssued, match: (*facts).requestPending, outcome: (*facts).reissue},
	{row: "S4", state: drivers.StateProvisioned, match: (*facts).tokenSpent, outcome: (*facts).spent},
	{row: "S5", state: drivers.StateIssued, match: (*facts).notCurrent, outcome: (*facts).issued},
	{row: "S6", state: drivers.StateProvisioned, match: (*facts).waitingToRegister, outcome: (*facts).provisioned},
	{row: "S7", state: drivers.StateRegistered, match: (*facts).registeredNow, outcome: (*facts).registered},
	{row: "S8", state: drivers.StateOffline, match: (*facts).asleep, outcome: (*facts).offline},
}

// facts is what the rows read: the inputs, the folded memory and the live
// registration.
type facts struct {
	dev drivers.Device
	obs drivers.Observations
	s   drivers.DecideSettings
	now time.Time

	// m is dev.Memory after the fold.
	m drivers.Memory
	// reg is the live registration, nil when there is no live contact.
	reg *agentapi.Registration
}

func newFacts(dev drivers.Device, obs drivers.Observations, s drivers.DecideSettings, now time.Time) *facts {
	return &facts{
		dev: dev, obs: obs, s: s, now: now,
		m:   fold(dev, obs, now, profile),
		reg: liveRegistration(obs.Device, now, profile),
	}
}

// ---- predicates ----

// invalidReason is why the spec breaks a driver rule, in the order the
// reasons are checked (the first is Ready's reason), with the message for
// people; "" when it breaks none.
func (f *facts) invalidReason() (reason, message string) {
	dev := f.dev
	claim := func(fallback string) string {
		if dev.ClaimMessage != "" {
			return dev.ClaimMessage
		}
		return fallback
	}
	switch {
	case dev.Model != drivers.ModelSoftphone:
		return ReasonUnknownModel, fmt.Sprintf("model %q is not served by the softphone driver", dev.Model)
	case dev.MACConflict: // never set for a softphone; kept for the Driver contract
		return v1alpha1.ConditionMACConflict, claim("its MAC is held by an older resource")
	case dev.NumberConflict:
		return ReasonNumberConflict, claim(fmt.Sprintf("number %s is held by an older resource", dev.Number))
	case dev.NumberOutOfPlan:
		return v1alpha1.ReasonNumberNotInPlan, claim(fmt.Sprintf("number %s is not in the number plan's device range", dev.Number))
	default:
		return "", ""
	}
}

func (f *facts) isInvalid() bool {
	reason, _ := f.invalidReason()
	return reason != ""
}

func (f *facts) hasAgentProblem() bool { return f.obs.AgentProblem(f.now, f.s.StaleAfter) != "" }

// requestPending reports a reissue request not yet acted on.
func (f *facts) requestPending() bool {
	return f.dev.Request != "" && f.dev.Request != f.m.HandledRequest
}

// tokenSpent reports a served fetch of the provisioning document after the
// last rotateToken (any, if there was none): its one-time URL was used, so
// the token must be replaced.
func (f *facts) tokenSpent() bool {
	d := f.obs.Device
	if d == nil {
		return false
	}
	la := f.m.LastAction
	rotated := la != nil && la.Kind == v1alpha1.ActionRotateToken
	for _, x := range d.Fetches {
		if x.File == ProvisioningFile(f.dev.ID) && x.Result == agentapi.FetchServed &&
			(!rotated || x.At.After(la.At.Time)) {
			return true
		}
	}
	return false
}

// current reports whether the softphone's last fetch is of its compiled
// version.
func current(dev drivers.Device, m drivers.Memory) bool {
	return dev.ConfigVersion != "" && m.LastConfigFetch != nil && m.LastConfigFetch.Version == dev.ConfigVersion
}

// registeredSinceFetch reports a registration seen after the last config
// fetch.
func registeredSinceFetch(m drivers.Memory) bool {
	return m.LastRegistered != nil && m.LastConfigFetch != nil && m.LastRegistered.After(m.LastConfigFetch.At.Time)
}

func (f *facts) notCurrent() bool { return !current(f.dev, f.m) }

func (f *facts) waitingToRegister() bool {
	return current(f.dev, f.m) && f.reg == nil && !registeredSinceFetch(f.m)
}

func (f *facts) registeredNow() bool { return current(f.dev, f.m) && f.reg != nil }

func (f *facts) asleep() bool {
	return current(f.dev, f.m) && f.reg == nil && registeredSinceFetch(f.m)
}

// ---- outcomes ----

// S1: the spec breaks a driver rule. No actions; a pending request is
// answered (marked handled) with none.
func (f *facts) invalid() drivers.Decision {
	reason, msg := f.invalidReason()
	m := f.m
	if f.requestPending() {
		m.HandledRequest = f.dev.Request
	}
	const notEvaluated = "not evaluated while the softphone is invalid"
	return drivers.Decision{
		State: drivers.StateInvalid,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg),
			drivers.Condition(v1alpha1.ConditionProvisioned, metav1.ConditionUnknown, ReasonInvalid, notEvaluated),
			drivers.Condition(v1alpha1.ConditionRegistered, metav1.ConditionUnknown, ReasonInvalid, notEvaluated),
			drivers.Condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionUnknown, ReasonInvalid, notEvaluated),
			f.numberConflict(),
		},
		StatusPatch: m,
		HumanAction: msg,
	}
}

// S2: the agent cannot be trusted now. Last known state, Ready and
// Registered only, memory and request untouched.
func (f *facts) unknown() drivers.Decision {
	problem := f.obs.AgentProblem(f.now, f.s.StaleAfter)
	status, msg := metav1.ConditionUnknown, "the PBX agent cannot be reached, or its observations are stale"
	if problem == v1alpha1.ReasonPBXDown {
		status, msg = metav1.ConditionFalse, "Asterisk is not running"
	}
	state := f.dev.LastState
	if state == "" {
		state = drivers.StateUnknown
	}
	return drivers.Decision{
		State: state,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, status, problem, msg),
			drivers.Condition(v1alpha1.ConditionRegistered, status, problem, msg),
		},
		StatusPatch: f.dev.Memory.DeepCopy(),
	}
}

// S3: a reissue request. Mint a new token and send the softphone back to
// Issued by forgetting its config fetch.
func (f *facts) reissue() drivers.Decision {
	m := f.m
	m.HandledRequest = f.dev.Request
	m.LastAction = f.rotation()
	m.LastConfigFetch = nil
	return drivers.Decision{
		State: drivers.StateIssued,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonIssued,
				"a new provisioning token was requested; waiting for Linphone to fetch its settings"),
			f.registeredByContact()),
		Actions:     []drivers.Action{{Kind: v1alpha1.ActionRotateToken}},
		StatusPatch: m,
		HumanAction: fmt.Sprintf(textScan, f.dev.Name),
	}
}

// S4: the one-time URL was used. Rotate the token so the QR code cannot be
// used again.
func (f *facts) spent() drivers.Decision {
	m := f.m
	m.LastAction = f.rotation()
	var requeue time.Duration
	if current(f.dev, m) && f.reg == nil && !registeredSinceFetch(m) {
		requeue = f.untilRegisterWithin(m)
	}
	return drivers.Decision{
		State: drivers.StateProvisioned,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonProvisioned,
				"Linphone fetched its settings, which spends its one-time URL; minting a new one"),
			f.registeredByContact()),
		Actions:      []drivers.Action{{Kind: v1alpha1.ActionRotateToken}},
		StatusPatch:  m,
		RequeueAfter: requeue,
	}
}

// S5: the softphone has not fetched its current settings: never, or only
// an older version (its password or the PBX changed).
func (f *facts) issued() drivers.Decision {
	m := f.m
	msg, human := "waiting for Linphone to fetch its settings from its one-time URL", fmt.Sprintf(textScan, f.dev.Name)
	if m.LastConfigFetch != nil {
		msg, human = "its settings changed since Linphone fetched them", fmt.Sprintf(textRescan, f.dev.Name)
	}
	if f.obs.ApplyFailure() != "" {
		// The PBX refused the bundle with the current settings, so a scan
		// would fetch old ones (or nothing): ConfigCurrent says why, and
		// nobody is asked to scan until it is served.
		human = ""
	}
	return drivers.Decision{
		State: drivers.StateIssued,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonIssued, msg),
			f.registeredByContact()),
		StatusPatch: m,
		HumanAction: human,
	}
}

// S6: current settings fetched, no registration since. From registerWithin
// after the fetch a person is asked (an addition to design §8.4).
func (f *facts) provisioned() drivers.Decision {
	m := f.m
	d := drivers.Decision{
		State: drivers.StateProvisioned,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, ReasonProvisioned,
				"Linphone fetched its settings; waiting for it to register"),
			f.registeredByContact()),
		StatusPatch: m,
	}
	if wait := f.untilRegisterWithin(m); wait > 0 {
		d.RequeueAfter = wait
	} else {
		d.HumanAction = fmt.Sprintf(textNotRegistering, int(f.now.Sub(m.LastConfigFetch.At.Time)/time.Minute))
	}
	return d
}

// S7: current settings and a live contact.
func (f *facts) registered() drivers.Decision {
	m := f.m
	from := f.reg.Address
	if from == "" {
		from = f.reg.Contact
	}
	return drivers.Decision{
		State: drivers.StateRegistered,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionTrue, ReasonRegistered, "registered from "+from),
			f.registeredByContact()),
		StatusPatch:  m,
		RequeueAfter: registeredRecheck,
	}
}

// S8: current settings, registered since, no contact now. Mobile apps sleep:
// Ready, not an error.
func (f *facts) offline() drivers.Decision {
	m := f.m
	msg := "last registered " + m.LastRegistered.UTC().Format(time.RFC3339) + "; an app asleep or away from home is normal"
	return drivers.Decision{
		State: drivers.StateOffline,
		Conditions: f.conditions(m,
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonOffline, msg),
			drivers.Condition(v1alpha1.ConditionRegistered, metav1.ConditionFalse, v1alpha1.ReasonOffline, msg)),
		StatusPatch: m,
	}
}

// noRuleMatched is the defensive end of the table: never reached
// (TestDecideTotal), and harmless if it ever is.
func noRuleMatched(f *facts) drivers.Decision {
	const msg = "no rule matched"
	return drivers.Decision{
		State: drivers.StateUnknown,
		Conditions: []metav1.Condition{
			drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionUnknown, string(drivers.StateUnknown), msg),
			drivers.Condition(v1alpha1.ConditionRegistered, metav1.ConditionUnknown, string(drivers.StateUnknown), msg),
		},
		StatusPatch: f.dev.Memory.DeepCopy(),
	}
}

// ---- helpers ----

// rotation is the LastAction of a rotateToken sent now. Its time is rounded
// up to the second (see fold), so no fetch from before it can look newer
// once status has stored it.
func (f *facts) rotation() *v1alpha1.LastAction {
	return &v1alpha1.LastAction{Kind: v1alpha1.ActionRotateToken, At: ceilSecond(f.now), Attempts: 1}
}

// untilRegisterWithin is how long until registerWithin has passed since
// the last config fetch; 0 once it has.
func (f *facts) untilRegisterWithin(m drivers.Memory) time.Duration {
	if m.LastConfigFetch == nil {
		return 0
	}
	if wait := m.LastConfigFetch.At.Add(registerWithin).Sub(f.now); wait > 0 {
		return wait
	}
	return 0
}

// conditions lists every condition the softphone owns, in their fixed
// order: Ready, Provisioned, Registered, ConfigCurrent, NumberConflict.
func (f *facts) conditions(m drivers.Memory, ready, registered metav1.Condition) []metav1.Condition {
	return []metav1.Condition{ready, f.provisionedCondition(m), registered, f.configCurrent(m), f.numberConflict()}
}

func (f *facts) registeredByContact() metav1.Condition {
	if f.reg == nil {
		return drivers.Condition(v1alpha1.ConditionRegistered, metav1.ConditionFalse, ReasonNoContact, "no live registration")
	}
	msg := f.reg.Contact
	if f.reg.UserAgent != "" {
		msg += ", " + f.reg.UserAgent
	}
	return drivers.Condition(v1alpha1.ConditionRegistered, metav1.ConditionTrue, ReasonContact, msg)
}

func (f *facts) provisionedCondition(m drivers.Memory) metav1.Condition {
	switch {
	case current(f.dev, m):
		return drivers.Condition(v1alpha1.ConditionProvisioned, metav1.ConditionTrue, ReasonFetched,
			fmt.Sprintf("fetched version %s at %s", short(m.LastConfigFetch.Version), m.LastConfigFetch.At.UTC().Format(time.RFC3339)))
	case m.LastConfigFetch == nil:
		return drivers.Condition(v1alpha1.ConditionProvisioned, metav1.ConditionFalse, ReasonNotFetched,
			"Linphone has not fetched its settings")
	default:
		return drivers.Condition(v1alpha1.ConditionProvisioned, metav1.ConditionFalse, ReasonOldVersion,
			fmt.Sprintf("fetched version %s; the current version is %s", short(m.LastConfigFetch.Version), short(f.dev.ConfigVersion)))
	}
}

func (f *facts) configCurrent(m drivers.Memory) metav1.Condition {
	const t = v1alpha1.ConditionConfigCurrent
	switch {
	case current(f.dev, m) && f.obs.ConfigLive():
		return drivers.Condition(t, metav1.ConditionTrue, ReasonCurrent, "version "+short(f.dev.ConfigVersion))
	case f.obs.ApplyFailure() != "":
		return drivers.Condition(t, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, f.obs.ApplyFailure())
	case !f.obs.ConfigLive():
		return drivers.Condition(t, metav1.ConditionFalse, v1alpha1.ReasonApplyPending,
			"the PBX does not serve the latest config yet")
	case m.LastConfigFetch == nil:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonNotFetched, "Linphone has not fetched its settings")
	default:
		return drivers.Condition(t, metav1.ConditionFalse, ReasonStale,
			fmt.Sprintf("Linphone has version %s; the current version is %s", short(m.LastConfigFetch.Version), short(f.dev.ConfigVersion)))
	}
}

func (f *facts) numberConflict() metav1.Condition {
	if f.dev.NumberConflict {
		msg := f.dev.ClaimMessage
		if msg == "" {
			msg = fmt.Sprintf("number %s is held by an older resource", f.dev.Number)
		}
		return drivers.Condition(v1alpha1.ConditionNumberConflict, metav1.ConditionTrue, ReasonNumberConflict, msg)
	}
	return drivers.Condition(v1alpha1.ConditionNumberConflict, metav1.ConditionFalse, ReasonNoConflict, "")
}

// short is a config version as messages show it.
func short(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}
