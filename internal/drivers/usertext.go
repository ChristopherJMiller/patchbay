package drivers

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// UserText neutralizes free text a person wrote (a label, a person's name)
// before it reaches any file: every run of "@", together with the white
// space between them, becomes one "@", so no "@@" is left and none can
// appear later. Text that starts like a placeholder (@@secret:...@@) would
// be resolved by the agent into a file served to devices or, malformed,
// fail Compile for every resource. A driver may drop white space (the
// 79x5's screen label drops every Unicode space), which would join "@ @"
// into "@@"; so white space here is unicode.IsSpace, that same test.
//
// Compile applies it to every label and name it renders; a driver that
// reports what a device shows (the 79x5's LabelShortened) applies it too,
// so the report matches the device.
func UserText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		i += size
		if r == '@' {
			i = afterAtRun(s, i)
		}
	}
	return b.String()
}

// afterAtRun is where the run of "@"s and white space that starts at i,
// just after an "@", ends: after its last "@" (white space after it stays).
func afterAtRun(s string, i int) int {
	end := i
	for j := i; j < len(s); {
		r, size := utf8.DecodeRuneInString(s[j:])
		j += size
		if r == '@' {
			end = j
		} else if !unicode.IsSpace(r) {
			break
		}
	}
	return end
}
