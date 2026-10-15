package drivers

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
)

// AgentProblem says whether the agent's view can be trusted now, for every
// driver's Unknown row (design §7, §8.3):
//   - v1alpha1.ReasonAgentUnreachable when the controller's latest poll
//     failed, or the last good one is stale (now - PolledAt > staleAfter);
//   - v1alpha1.ReasonPBXDown when the agent reports Asterisk is not up;
//   - "" otherwise.
func (o Observations) AgentProblem(now time.Time, staleAfter time.Duration) string {
	switch {
	case !o.AgentReachable || now.Sub(o.PolledAt) > staleAfter:
		return v1alpha1.ReasonAgentUnreachable
	case !o.Agent.AsteriskUp:
		return v1alpha1.ReasonPBXDown
	default:
		return ""
	}
}

// ConfigLive reports whether the agent serves the bundle the controller
// compiled last, so every device's current config (Device.ConfigVersion)
// can be fetched. Until it does, a resync would only make a phone re-read
// its old config and spend an attempt.
func (o Observations) ConfigLive() bool {
	return o.CompiledVersion != "" && o.Agent.AppliedVersion == o.CompiledVersion
}

// ApplyFailure is why the agent refused or rolled back the bundle the
// controller compiled last (ConfigCurrent=False, reason
// v1alpha1.ReasonApplyFailed), or "" when it did not: an error about an
// older bundle does not count.
func (o Observations) ApplyFailure() string {
	if o.CompiledVersion == "" || o.Agent.LastApplyVersion != o.CompiledVersion {
		return ""
	}
	return o.Agent.LastApplyError
}

// Condition builds a condition for Decision.Conditions. The controller adds
// ObservedGeneration and LastTransitionTime.
func Condition(conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: message}
}

// RegistrationGrace is how long after its expiry a registration still
// counts as a live contact. A device refreshes shortly before expiry (the
// 79x5 5 s before, its SEP file's timerRegisterDelta), and the controller
// sees the refresh only once the agent's AMI poll and its own poll (5 s
// each) have passed, or later when a reconcile reads the snapshot it
// cached: without the grace, a healthy phone would flicker to Unreachable
// in that gap. A device that did not refresh is gone once Asterisk drops
// its expired contact, or the grace ends.
const RegistrationGrace = 15 * time.Second

// RegistrationLive reports whether a registration has not expired, give or
// take RegistrationGrace.
func RegistrationLive(expiresAt, now time.Time) bool {
	return expiresAt.Add(RegistrationGrace).After(now)
}
