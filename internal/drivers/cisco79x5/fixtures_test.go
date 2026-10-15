package cisco79x5_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites the golden files: go test ./internal/drivers/cisco79x5 -run Golden -update
var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// housePatterns is compile.DefaultNumberPlan().DialPatterns(): EchoTest,
// Emergency, Devices, People, Streams, then Reserved.
var housePatterns = []string{"600", "911", "1XX", "3XX", "7XX", "2XX", "0", "9.", "*8"}

// golden compares got with testdata/<rel>, or rewrites it with -update.
func golden(t *testing.T, rel string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", rel)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update and review the diff):\n--- got\n%s", path, got)
	}
}
