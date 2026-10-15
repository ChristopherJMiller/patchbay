package apply_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/devicehttp"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/provision"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// TestSpentTokenStaysSpentThroughARollback wires the real apply to the real
// device HTTP as the agent does: a token spent on v1 stays spent when v2
// (its token rotated) is swapped in while AMI is down, device HTTP prunes,
// and v2 then fails verification and is rolled back to v1.
func TestSpentTokenStaysSpentThroughARollback(t *testing.T) {
	e := newEnv(t)
	runApplier(t, e.a)
	dh, err := devicehttp.New(devicehttp.Config{
		LiveDir: e.dir, StateDir: filepath.Join(t.TempDir(), "state"),
		Grants: e.a, Settled: provision.SourceFunc(e.a.SettledGrants), Log: logr.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(dh)
	defer ts.Close()
	get := func(token string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/p/"+token, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	e.ast.boot()
	in := intaketest.House()
	v1 := in.Resolved(t)
	e.mustApply(v1)
	token1 := v1.Tokens[laptop]
	if code := get(token1); code != http.StatusOK {
		t.Fatalf("first fetch = %d", code)
	}
	if code := get(token1); code != http.StatusNotFound {
		t.Fatalf("second fetch = %d", code)
	}

	// The controller rotates the token; v2 also adds a phone Asterisk
	// will not take. It lands while AMI is down.
	e.ast.ami.SetUp(false, "")
	in.Secrets[compile.ProvisioningTokenRef(laptop)] = []byte("not-a-secret-token-chris-laptop-0002")
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	v2 := in.Resolved(t)
	e.ast.drop[v2.Bundle.Version] = front
	e.mustApply(v2)
	get("not-a-token") // a provisioning request prunes

	e.ast.ami.SetUp(true, "22.11.0")
	waitFor(t, "the rollback", func() bool { return e.a.Status().AppliedVersion == v1.Bundle.Version })
	if code := get(token1); code != http.StatusNotFound {
		t.Errorf("v1's spent token after the rollback = %d, want 404 (served twice)", code)
	}
}
