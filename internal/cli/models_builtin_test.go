//go:build !e2e

package cli_test

import (
	"bytes"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// TestModels pins what `patchbay models` prints for the drivers patchbay
// ships. Not under the e2e tag: that build's manifest has the e2e load.
func TestModels(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cli.Models(&buf, reg); err != nil {
		t.Fatal(err)
	}
	golden(t, "models.golden", buf.Bytes())
}
