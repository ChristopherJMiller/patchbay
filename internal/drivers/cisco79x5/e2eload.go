package cisco79x5

import (
	"bytes"
	"strings"
)

// e2eSBNSize is the size of every generated .sbn file of the e2e test load.
const e2eSBNSize = 4096

// E2ELoadFile returns the bytes of one file of the e2e test load, generated
// deterministically from its name: the line "patchbay e2e firmware <name>"
// once for a .loads file, and repeated (and cut) to 4096 bytes for a .sbn.
//
// The e2e load exists in the manifest only of binaries built with the e2e
// build tag (e2eload_e2e.go), whose digests are taken from these bytes; this
// function is untagged so the e2e harness can build the zip it loads. Nothing
// here is Cisco's, and no file is ever written to the repo.
func E2ELoadFile(name string) []byte {
	line := "patchbay e2e firmware " + name + "\n"
	if !strings.HasSuffix(name, ".sbn") {
		return []byte(line)
	}
	b := bytes.Repeat([]byte(line), e2eSBNSize/len(line)+1)
	return b[:e2eSBNSize]
}
