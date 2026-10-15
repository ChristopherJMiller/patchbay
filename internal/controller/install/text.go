package install

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds on the text the controller writes into status and Events. Much
// of it carries what a device sent (its web page's load ID and log, its
// SIP contact and user agent) or what the agent reported (an apply
// error), so the controller, the last writer before the API server,
// bounds it: a status the API server refuses as too long would freeze the
// resource's status for good, and an Event it refuses is lost.
const (
	// MaxEventNote is the longest note events.k8s.io/v1 accepts.
	MaxEventNote = 1024
	// MaxStatusMessage bounds every condition message and human action
	// the device reconcilers write (the CRDs allow 32768 bytes for a
	// condition message and set no limit on humanAction).
	MaxStatusMessage = 2048
	// MaxStatusValue bounds a short value observed from a device:
	// status.observed.firmware, status.observed.address.
	MaxStatusValue = 128
)

// ellipsis ends text Text has cut.
const ellipsis = "…"

// Text is s made safe to write into status or an Event: valid UTF-8
// (each invalid byte U+FFFD), every control character (C0, DEL and C1,
// newlines included) a space, so no terminal escape or carriage return
// reaches `patchbay status` or kubectl, and at most limit bytes, cut on a
// rune boundary and ending in "…" when cut ("" when limit leaves no room
// for the ellipsis).
func Text(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	cut := limit - len(ellipsis)
	if cut <= 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// EventNote is an Event's note as it may be recorded: Text bounded by
// MaxEventNote. Every reconciler passes its notes through it.
func EventNote(note string) string { return Text(note, MaxEventNote) }
