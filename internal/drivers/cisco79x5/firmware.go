package cisco79x5

import (
	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ciscoLoads is the manifest of Cisco's loads (design §12, research
// 2026-10-14-cisco-7945-firmware.md). Sizes are from the research and only
// advisory; a file is verified by its SHA-256 alone.
//
// The digests (2026-10-15) were taken from a copy of the 9.4(2)SR4-3 files,
// not yet from Cisco's own download: Cisco's software site no longer lists
// the 7900 series, and Cisco publishes no digests in public. Every file of
// that copy has the size Cisco's release notes give, and each image carries
// Cisco's signing header; the phone itself refuses an image whose Cisco
// signature fails ("Load Authentication Failed"), so a wrong digest here can
// stall an upgrade but cannot run foreign code on the phone. They are to be
// confirmed against Cisco's copy (the CUCM device pack, which carries this
// load); a file that differs there is replaced here. To change a digest:
//
//  1. take the files from Cisco (cmterm-7945_7965-sip.9-4-2SR4-3.zip, or the
//     load's files from a CUCM device pack; a Cisco.com account,
//     docs/research/2026-10-14-cisco-7945-firmware.md);
//  2. run `sha256sum` on each of the eight files below;
//  3. replace that file's digest with the 64 lower-case hex digits (a size
//     that differs is corrected at the same time; drivers.SHA256Pending marks
//     a file whose digest is not known);
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
			{Name: "SIP45.9-4-2SR4-3S.loads", SHA256: "c269fb8de6551e64ab7b2ad7c7335690599f28100957fdc5bfbcf7c75046493d", Size: 673},
			{Name: "apps45.9-4-2SR4-3.sbn", SHA256: "24087cd5bd54411dd9b9b0dc9de668334cee9829fd2ca140b6050267512d49e2", Size: 4637904},
			{Name: "cnu45.9-4-2SR4-3.sbn", SHA256: "f2321bdc9388781cd85f36ff6418edb33716b9ea8a48ef4d8f3847f226d78a9b", Size: 582170},
			{Name: "cvm45sip.9-4-2SR4-3.sbn", SHA256: "7c4dafb1532f952267b242b531f9405b0957339a18b2e0195e5bc2ea5de42c5f", Size: 2693976},
			{Name: "dsp45.9-4-2SR4-3.sbn", SHA256: "12bdff18f06e68a6ec6ed5661aa15fbe1ca58e8b75f46ff44f19ee7dc8a99d1c", Size: 364907},
			{Name: "jar45sip.9-4-2SR4-3.sbn", SHA256: "ae6391837f8e071ccc9fb9ea91edff128d303e489dd2e0f85fb495fea7e5c22f", Size: 2260034},
			// What a factory-reset phone asks for to learn which load to
			// boot. Every SIP and SCCP zip carries its own copy, so they are
			// Shared: TFTP serves the default load's.
			{Name: "term45.default.loads", SHA256: "4816646f9c7cd63b0c1f76706241db6087da13665c0069266a82630c48bd4852", Size: 673, Shared: true},
			{Name: "term65.default.loads", SHA256: "62ce7e642b2ff461b1566aa11722fcbaf7fd16d7030f7a463092a3c309465ebf", Size: 673, Shared: true},
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
