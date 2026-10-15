package cli_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/cli"
)

// The test registry has every profile field and each kind of load: a
// Shared file, a pending digest, a software model with no hardware.
func TestModelsTestRegistry(t *testing.T) {
	var buf bytes.Buffer
	if err := cli.Models(&buf, testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	golden(t, "models-test-registry.golden", buf.Bytes())
}

func TestModelsErrors(t *testing.T) {
	if err := cli.Models(&bytes.Buffer{}, nil); err == nil {
		t.Error("Models with no registry: no error")
	}
	if err := cli.Models(failingWriter{}, testRegistry(t)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Models to a failing writer = %v, want its error", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
