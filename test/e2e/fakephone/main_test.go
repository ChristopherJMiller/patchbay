package main

import (
	"bytes"
	"os"
	"sync"
	"testing"
)

// sipLog collects what sipgo logs in this test process, every SIP message
// included (verbose), so TestRunNeverLogsSecrets can read it.
var sipLog lockedBuffer

func TestMain(m *testing.M) {
	// As a subprocess (TestStepFailureExits): be the fakephone binary.
	if os.Getenv("FAKEPHONE_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	// sipgo's logger is process-wide: set it before any test starts a SIP
	// stack, so no command sets it under a running one.
	setSIPLogging(&sipLog, true)
	os.Exit(m.Run())
}

// lockedBuffer is a bytes.Buffer safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
