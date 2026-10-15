//go:build e2e

package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// The e2e build's CLI lists the e2e test load, with genuine digests.
func TestModelsE2ELoad(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cli.Models(&buf, reg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	i := strings.Index(out, "  firmware load E2E45.0-0-1S\n")
	if i < 0 {
		t.Fatalf("no e2e load in:\n%s", out)
	}
	if strings.Contains(strings.SplitN(out[i:], "\n\n", 2)[0], "pending") {
		t.Errorf("the e2e load shows a pending digest:\n%s", out[i:])
	}
}
