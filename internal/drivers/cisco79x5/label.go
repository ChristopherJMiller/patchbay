package cisco79x5

import (
	"strings"
	"unicode"
)

const (
	// screenLabelMax is the longest phoneLabel the 79x5 shows at the top of
	// its screen (usecallmanager.nz, SEPMAC.cnf.xml: "Maximum length of 12
	// characters ... the 7900 series does not support spaces").
	screenLabelMax = 12
	// lineLabelMax is the longest featureLabel / displayName (30).
	lineLabelMax = 30
)

// ScreenLabel is the phone's short screen label (phoneLabel) derived from its
// label: every space removed (any Unicode white space, since the 7900 series
// shows none), then the first 12 runes. It never splits a rune; it can cut a
// combining mark from its base, which the bench would show as a dangling
// accent. A label of only spaces gives an empty screen label.
func ScreenLabel(label string) string {
	noSpace := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, label)
	return firstRunes(noSpace, screenLabelMax)
}

// LineLabel is the label on the phone's line button and its caller ID name
// (featureLabel and displayName): the label as written, cut to its first 30
// runes.
func LineLabel(label string) string {
	return firstRunes(label, lineLabelMax)
}

// firstRunes returns s cut to at most n runes.
func firstRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
