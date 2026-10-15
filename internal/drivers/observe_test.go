package drivers_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func TestAgentProblem(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	const stale = 30 * time.Second
	healthy := drivers.Observations{AgentReachable: true, PolledAt: now, Agent: agentapi.AgentStatus{AsteriskUp: true}}
	for _, tc := range []struct {
		name   string
		mutate func(*drivers.Observations)
		want   string
	}{
		{"healthy", func(*drivers.Observations) {}, ""},
		{"polled exactly StaleAfter ago (not stale yet)", func(o *drivers.Observations) { o.PolledAt = now.Add(-stale) }, ""},
		{"polled just over StaleAfter ago", func(o *drivers.Observations) { o.PolledAt = now.Add(-stale - time.Nanosecond) },
			v1alpha1.ReasonAgentUnreachable},
		{"never polled", func(o *drivers.Observations) { o.PolledAt = time.Time{} }, v1alpha1.ReasonAgentUnreachable},
		{"latest poll failed", func(o *drivers.Observations) { o.AgentReachable = false }, v1alpha1.ReasonAgentUnreachable},
		{"unreachable outranks Asterisk down", func(o *drivers.Observations) {
			o.AgentReachable, o.Agent.AsteriskUp = false, false
		}, v1alpha1.ReasonAgentUnreachable},
		{"Asterisk down", func(o *drivers.Observations) { o.Agent.AsteriskUp = false }, v1alpha1.ReasonPBXDown},
	} {
		o := healthy
		tc.mutate(&o)
		if got := o.AgentProblem(now, stale); got != tc.want {
			t.Errorf("%s: AgentProblem = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestConfigLiveAndApplyFailure(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		compiled, applied, tried string
		applyErr                 string
		wantLive                 bool
		wantFailure              string
	}{
		{name: "nothing compiled yet", wantLive: false},
		{name: "live", compiled: "v2", applied: "v2", tried: "v2", wantLive: true},
		{name: "pending", compiled: "v2", applied: "v1", tried: "v1", wantLive: false},
		{name: "refused", compiled: "v2", applied: "v1", tried: "v2", applyErr: "pjsip: endpoint missing",
			wantFailure: "pjsip: endpoint missing"},
		{name: "an older version's error does not count", compiled: "v3", applied: "v1", tried: "v2",
			applyErr: "old trouble"},
	} {
		o := drivers.Observations{
			CompiledVersion: tc.compiled,
			Agent:           agentapi.AgentStatus{AppliedVersion: tc.applied, LastApplyVersion: tc.tried, LastApplyError: tc.applyErr},
		}
		if got := o.ConfigLive(); got != tc.wantLive {
			t.Errorf("%s: ConfigLive = %t, want %t", tc.name, got, tc.wantLive)
		}
		if got := o.ApplyFailure(); got != tc.wantFailure {
			t.Errorf("%s: ApplyFailure = %q, want %q", tc.name, got, tc.wantFailure)
		}
	}
}

func TestCondition(t *testing.T) {
	got := drivers.Condition(v1alpha1.ConditionReady, metav1.ConditionFalse, "Unseen", "connect it")
	want := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Unseen", Message: "connect it"}
	if got != want {
		t.Fatalf("Condition = %+v, want %+v", got, want)
	}
}
