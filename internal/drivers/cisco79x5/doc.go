// Package cisco79x5 is the driver for Cisco CP-7945G, CP-7965G and CP-7975G
// phones on Cisco's SIP firmware (9.4(2)), registered to stock Asterisk PJSIP
// in "CUCM mode" (design §4, §8.3; docs/research/2026-10-14-cisco-7945-*.md).
//
// It renders two files into the bundle's tftp/ tree: SEP<MAC>.cnf.xml, the
// phone's whole configuration, and dialplan.xml, a dial template generated
// from the number plan and shared by every phone (render.go, sep.go,
// dialtemplate.go). Its Decide is the ordered rule table of design §8.3
// (decide.go), fed by the fold of the agent's four sensors into the memory
// kept in status (fold.go). Its firmware manifest (firmware.go) names the SIP
// load's files with SHA-256s that stay pending until they are taken from
// Cisco's own zip.
//
// Everything here is pure: no I/O, no clock, no randomness.
//
// Facts the bench has not confirmed yet are single values with a safe
// default, each a one-line change once answered (docs/plans/release-a.md §8):
// Provisioning.ResyncByNotify, Password.Length and Endpoint.QualifyFrequency
// (profile.go), the transfer and rtpRx feature levels (profile.go),
// settingsAccessRestricted (sep.go), dialTemplateLiteralStar
// (dialtemplate.go) and trustErrorPattern (decide.go).
package cisco79x5
