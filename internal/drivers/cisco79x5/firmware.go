package cisco79x5

import (
	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ciscoLoads is the manifest of Cisco's loads (design §12, research
// 2026-10-14-cisco-7945-firmware.md). Sizes are from the research and only
// advisory; a file is verified by its SHA-256 alone.
//
// No digest is known yet: nobody has taken one from Cisco's zip, and Cisco
// publishes none in public. Every file is drivers.SHA256Pending until then,
// so the load is never FirmwareAvailable and the agent refuses its files
// (412). To fill a digest in:
//
//  1. download cmterm-7945_7965-sip.9-4-2SR4-3.zip from Cisco (a Cisco.com
//     account; docs/research/2026-10-14-cisco-7945-firmware.md);
//  2. unzip it and run `sha256sum` on each of the eight files below;
//  3. replace that file's drivers.SHA256Pending with the 64 lower-case hex
//     digits, one line per file (a size that differs from Cisco's zip is
//     corrected at the same time);
//  4. run the tests, commit, release patchbay, then `patchbay firmware load
//     <zip>`.
//
// Never commit a firmware file, encrypted or not (CLAUDE.md, hard rule 4).
var ciscoLoads = []drivers.FirmwareLoad{
	{
		Name:   DefaultLoad,
		Source: "Cisco's cmterm-7945_7965-sip.9-4-2SR4-3.zip",
		Models: []string{string(v1alpha1.PhoneModelCisco7945), string(v1alpha1.PhoneModelCisco7965)},
		Files: []drivers.FirmwareFile{
			{Name: "SIP45.9-4-2SR4-3S.loads", SHA256: drivers.SHA256Pending, Size: 673},
			{Name: "apps45.9-4-2SR4-3.sbn", SHA256: drivers.SHA256Pending, Size: 4637904},
			{Name: "cnu45.9-4-2SR4-3.sbn", SHA256: drivers.SHA256Pending, Size: 582170},
			{Name: "cvm45sip.9-4-2SR4-3.sbn", SHA256: drivers.SHA256Pending, Size: 2693976},
			{Name: "dsp45.9-4-2SR4-3.sbn", SHA256: drivers.SHA256Pending, Size: 364907},
			{Name: "jar45sip.9-4-2SR4-3.sbn", SHA256: drivers.SHA256Pending, Size: 2260034},
			// What a factory-reset phone asks for to learn which load to
			// boot. Every SIP and SCCP zip carries its own copy, so they are
			// Shared: TFTP serves the default load's.
			{Name: "term45.default.loads", SHA256: drivers.SHA256Pending, Size: 673, Shared: true},
			{Name: "term65.default.loads", SHA256: drivers.SHA256Pending, Size: 673, Shared: true},
		},
	},
}

// extraLoads are appended to the manifest by New. Only the e2e build adds
// one (e2eload_e2e.go); the production binary's manifest is ciscoLoads alone.
var extraLoads []drivers.FirmwareLoad

// cloneLoads deep-copies a manifest, so no caller can change the driver's.
func cloneLoads(loads []drivers.FirmwareLoad) []drivers.FirmwareLoad {
	out := make([]drivers.FirmwareLoad, len(loads))
	for i, l := range loads {
		l.Models = append([]string(nil), l.Models...)
		l.Files = append([]drivers.FirmwareFile(nil), l.Files...)
		out[i] = l
	}
	return out
}
