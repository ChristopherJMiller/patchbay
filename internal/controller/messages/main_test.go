package messages_test

import (
	"os"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
)

// kube is the envtest API server of TestMessagesEnvtest (nil under -short);
// every other test uses the fake client.
var kube *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &kube)) }
