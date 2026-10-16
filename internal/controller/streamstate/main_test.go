package streamstate_test

import (
	"os"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
)

// env is the envtest API server of TestStreamStatusEnvtest (nil under
// -short); every other test uses the fake client.
var env *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }
