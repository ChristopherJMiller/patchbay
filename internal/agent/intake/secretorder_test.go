package intake_test

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// TestSecretOfAnotherVersion: the controller writes pbx-secrets before
// pbx-config, and the two informers deliver independently, so the intake
// sees one version's ConfigMap with another's Secret. It applies a version
// only with the values it was compiled with: a Secret ahead of the applied
// version is ignored, a ConfigMap ahead of its Secret waits for it, and
// only a Secret that never comes is refused, after the grace.
func TestSecretOfAnotherVersion(t *testing.T) {
	in := intaketest.House()
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	b1 := in.Compile(t)
	s1 := in.SecretData(t, b1)
	// The AMI password rotates (W3.2 regenerates a deleted AMI Secret).
	in.Secrets[intaketest.AMIRef] = []byte("not-a-secret-ami-0002")
	b2 := in.Compile(t)
	s2 := in.SecretData(t, b2)
	// The front phone is removed: its keys leave the Secret.
	in.Phones = in.Phones[:1]
	b3 := in.Compile(t)
	s3 := in.SecretData(t, b3)
	// The desk's password rotates; its Secret comes late.
	in.Secrets[compile.PasswordRef(desk)] = []byte("not-a-secret-desk-0002")
	b4 := in.Compile(t)
	s4 := in.SecretData(t, b4)
	v1, v2, v3, v4 := b1.Version[:4], b2.Version[:4], b3.Version[:4], b4.Version[:4]

	h := newHarness(t, logr.Discard(), intaketest.ConfigMap(t, ns, cmName, b1))
	h.putSecret(intaketest.Secret(ns, secretName, "1", s1))
	h.expectCalls("apply " + v1)

	// Secret first, AMI password rotated: v1 is not re-applied with v2's
	// password (Asterisk still runs v1's manager.conf), and nothing waits to
	// refuse it.
	timers := h.timerCount()
	h.putSecret(intaketest.Secret(ns, secretName, "2", s2))
	h.expectCalls("apply " + v1)
	if h.timerCount() != timers {
		t.Error("a Secret ahead of the applied version set a timer")
	}
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b2))
	h.expectCalls("apply "+v1, "apply "+v2)

	// Secret first, a device removed: the live v2 is not refused for the
	// keys the new Secret no longer has.
	timers = h.timerCount()
	h.putSecret(intaketest.Secret(ns, secretName, "3", s3))
	h.expectCalls("apply "+v1, "apply "+v2)
	if len(h.applier.refused) != 0 || h.timerCount() != timers {
		t.Fatalf("refused %v, or a timer was set", h.applier.refused)
	}
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b3))
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3)

	// ConfigMap first: v2 again (the old password), while the Secret is
	// still v3's: it waits, without a refusal, and applies once its Secret
	// arrives.
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b2))
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3)
	if d := h.lastTimer(); d != intake.DefaultSecretGrace {
		t.Errorf("the wait for the Secret is %s, want %s", d, intake.DefaultSecretGrace)
	}
	h.putSecret(intaketest.Secret(ns, secretName, "4", s2))
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3, "apply "+v2)

	// A version whose Secret never comes is refused once the grace has
	// passed, and applied when the Secret comes after all.
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b4))
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3, "apply "+v2)
	h.advance(intake.DefaultSecretGrace)
	h.fireLast()
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3, "apply "+v2, "refuse "+v4)
	if err := h.applier.refused[0]; err == nil || !strings.Contains(err.Error(), "pbx-secrets") {
		t.Errorf("refusal = %v", err)
	}
	if d := h.lastTimer(); d != intake.DefaultRetryAfter {
		t.Errorf("after the refusal the timer is %s, want %s", d, intake.DefaultRetryAfter)
	}
	h.putSecret(intaketest.Secret(ns, secretName, "5", s4))
	h.expectCalls("apply "+v1, "apply "+v2, "apply "+v3, "apply "+v2, "refuse "+v4, "apply "+v4)
}
