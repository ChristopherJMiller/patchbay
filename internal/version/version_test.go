package version_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/version"
)

func TestString(t *testing.T) {
	s := version.String()
	for _, want := range []string{version.Version, version.Commit, runtime.Version()} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
