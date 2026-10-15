//go:build e2e

package cisco79x5

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// e2eLoadName is the e2e test load: genuinely verified (its digests are
// computed from E2ELoadFile), just not Cisco's. Phones in e2e select it with
// spec.firmware. It exists only in binaries built with the e2e tag, so no
// production binary accepts a file that is not Cisco's (plan §2(b)).
const e2eLoadName = "E2E45.0-0-1S"

func init() {
	extraLoads = append(extraLoads, e2eLoad())
}

func e2eLoad() drivers.FirmwareLoad {
	l := drivers.FirmwareLoad{
		Name:   e2eLoadName,
		Source: "patchbay e2e test load (generated)",
		Models: []string{string(v1alpha1.PhoneModelCisco7945), string(v1alpha1.PhoneModelCisco7965)},
	}
	for _, name := range []string{e2eLoadName + ".loads", "appsE2E.0-0-1.sbn"} {
		b := E2ELoadFile(name)
		sum := sha256.Sum256(b)
		l.Files = append(l.Files, drivers.FirmwareFile{
			Name:   name,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(b)),
		})
	}
	return l
}
