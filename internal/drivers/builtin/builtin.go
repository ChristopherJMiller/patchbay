// Package builtin wires the drivers patchbay ships into one registry
// (docs/plans/release-a.md decision (f)). The controller, the agent (for the
// firmware manifests) and the CLI each call Registry once at start;
// compile.Compile takes the registry as an argument, so its own tests can use
// fake drivers instead.
package builtin

import (
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

// Registry returns the registry of every built-in driver: cisco79x5, then
// softphone. It fails only if a driver breaks the registry's rules (a
// profile or firmware manifest that does not validate), which is a bug.
func Registry() (*drivers.Registry, error) {
	return drivers.NewRegistry(cisco79x5.New(), softphone.New())
}
