package apply_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// TestVerifyReportsTheMismatchAtTheDeadline: when the verify timeout ends
// while a check is in flight (the real client then answers with the
// context's error), the failure still names the mismatch the checks before
// it found, not the deadline.
func TestVerifyReportsTheMismatchAtTheDeadline(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)

	e.a = e.applier(apply.Config{VerifyTimeout: 100 * time.Millisecond})
	v2 := houseWithFront(t)
	var calls atomic.Int32
	e.ast.ami.OnEndpoints(func() ([]string, error) {
		if calls.Add(1) == 1 {
			return []string{desk, laptop}, nil // front is missing
		}
		// A check in flight at the deadline.
		time.Sleep(150 * time.Millisecond)
		return nil, errors.New("ami: PJSIPShowEndpoints: no reply: " + context.DeadlineExceeded.Error())
	})
	err := e.apply(v2)
	if err == nil || err.Error() != "verify: endpoints missing "+front {
		t.Errorf("Apply = %v, want the mismatch", err)
	}
	if st := e.a.Status(); st.LastApplyError != "verify: endpoints missing "+front {
		t.Errorf("Status = %+v", st)
	}

	// With no check completed, the deadline is the reason.
	e.ast.ami.OnEndpoints(func() ([]string, error) {
		time.Sleep(150 * time.Millisecond)
		return nil, context.DeadlineExceeded
	})
	v3 := house(t)
	v3.Bundle.Version = intaketest.Version('3')
	if err := e.apply(v3); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("Apply with no completed check = %v", err)
	}
}

// TestRestartRollsBackToTheAdoptedVersion: after an agent restart, the
// adopted live version is what a failing version rolls back to, even when
// that version arrives (AMI down: the new client has no account yet)
// before the adopted one was verified again.
func TestRestartRollsBackToTheAdoptedVersion(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	v2 := houseWithFront(t)
	e.ast.drop[v2.Bundle.Version] = front
	if err := e.apply(v2); err == nil {
		t.Fatal("v2 applied")
	}
	if e.current() != v1.Bundle.Version {
		t.Fatal("v2 was not rolled back")
	}

	// The agent restarts; pbx-config still holds v2, so intake hands v2
	// over first, before AMI is up.
	e.ast.ami.SetUp(false, "")
	e.a = e.applier(apply.Config{})
	runApplier(t, e.a)
	if err := e.apply(v2); err != nil {
		t.Fatalf("Apply with AMI down = %v", err)
	}
	if e.current() != v2.Bundle.Version {
		t.Fatal("v2 is not live unverified")
	}
	e.ast.resetReloads()
	e.ast.ami.SetUp(true, "22.11.0")
	waitFor(t, "the rollback to the adopted version", func() bool {
		return e.a.Status().AppliedVersion == v1.Bundle.Version
	})
	if e.current() != v1.Bundle.Version {
		t.Error("current was not swapped back to the adopted version")
	}
	st := e.a.Status()
	if st.LastApplyVersion != v2.Bundle.Version || st.LastApplyError != "verify: endpoints missing "+front {
		t.Errorf("Status = %+v", st)
	}
	// Asterisk is back on v1, and v1 is verified on this session.
	waitFor(t, "the adopted version verified", func() bool { return e.a.Status().Verified })
	if got := e.ast.reloaded(); len(got) < 2 || got[len(got)-1] != "pbx_config.so@"+v1.Bundle.Version[:4] {
		t.Errorf("reloads %q: Asterisk was not reloaded with v1", got)
	}
	// Verifying the restored version is not an apply: v2's failure stays
	// the last apply.
	if st := e.a.Status(); st.LastApplyVersion != v2.Bundle.Version || st.LastApplyError == "" {
		t.Errorf("Status after v1 was verified again = %+v", st)
	}
}

// TestSettledGrants: SettledGrants are the live version's grants only once
// no failure can roll it back: verified after it was applied, and not
// reached by a rollback (a rollback can make an older version's grants
// live again, so device HTTP must not have forgotten their spends).
func TestSettledGrants(t *testing.T) {
	e := newEnv(t)
	runApplier(t, e.a)
	settled := func() string {
		v, g := e.a.SettledGrants()
		if (v == "") != (g == nil) {
			t.Errorf("SettledGrants = %q, %v", v, g)
		}
		return v
	}
	if settled() != "" {
		t.Error("settled before any version")
	}
	v1 := house(t)
	e.mustApply(v1) // AMI down: unverified
	if settled() != "" {
		t.Error("an unverified version is settled")
	}
	e.ast.boot()
	waitFor(t, "v1 verified", func() bool { return settled() == v1.Bundle.Version })
	if _, g := e.a.SettledGrants(); len(g) != 1 || g[0].Device != laptop || g[0].Token == "" {
		t.Errorf("settled grants %v", g)
	}

	// v2 fails: back on v1, which is no longer settled.
	in := intaketest.House()
	in.Secrets[compile.ProvisioningTokenRef(laptop)] = []byte("not-a-secret-token-chris-laptop-0002")
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	v2 := in.Resolved(t)
	e.ast.drop[v2.Bundle.Version] = front
	if err := e.apply(v2); err == nil {
		t.Fatal("v2 applied")
	}
	if v, _, _ := e.a.Applied(); v != v1.Bundle.Version {
		t.Fatalf("Applied = %s", v)
	}
	if settled() != "" {
		t.Error("a version reached by a rollback is settled")
	}
	// The next version that verifies settles.
	v3 := houseWithFront(t)
	e.mustApply(v3)
	if settled() != v3.Bundle.Version {
		t.Error("a verified version is not settled")
	}

	// An adopted version is settled once it is verified and intake handed
	// its tokens over.
	e.ast.ami.SetUp(false, "")
	b := e.applier(apply.Config{})
	runApplier(t, b)
	e.a = b
	if settled() != "" {
		t.Error("an adopted version is settled before intake hands it over")
	}
	e.mustApply(v3)
	e.ast.ami.SetUp(true, "22.11.0")
	waitFor(t, "the adopted version verified", func() bool { return b.Status().Verified })
	if settled() != v3.Bundle.Version {
		t.Error("the adopted version, verified and handed over, is not settled")
	}
}

// TestSameVersionKeepsItsAccount: a version's secret values are fixed by
// its version, so applying the live version again never changes the
// account or the grants it has.
func TestSameVersionKeepsItsAccount(t *testing.T) {
	e := newEnv(t)
	e.ast.boot()
	v1 := house(t)
	e.mustApply(v1)
	_, before := e.a.Grants()
	again := house(t)
	again.AMIPassword = "not-a-secret-ami-0002"
	for id := range again.Tokens {
		again.Tokens[id] = "not-a-secret-token-0002"
	}
	e.mustApply(again)
	if _, pass := e.ast.ami.Account(); pass != v1.AMIPassword {
		t.Error("the live version's account changed")
	}
	if _, after := e.a.Grants(); !slices.Equal(after, before) {
		t.Errorf("grants %v, want %v", after, before)
	}
}
