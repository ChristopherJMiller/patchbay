package compile

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// patternSyntax is one number-plan pattern: digits, X (any digit), "*" and
// "#" (literal), with an optional trailing "." (one or more digits).
var patternSyntax = regexp.MustCompile(`^[0-9*#X]+\.?$`)

// plainNumber is a built-in number: digits only.
var plainNumber = regexp.MustCompile(`^[0-9]+$`)

// Validate checks that the plan can be compiled and dialled:
//   - every pattern is digits, X, "*" and "#", with an optional trailing
//     "." (NumberPlan's notation); EchoTest and Emergency are plain digits;
//   - the ranges (Devices, People, Streams and each Reserved pattern) are
//     pairwise disjoint: no number matches two of them;
//   - EchoTest and Emergency lie in no Devices, People or Streams range and
//     differ from each other. They may lie in a Reserved pattern (the
//     default plan's 911 is inside 9.): a built-in is an exact extension,
//     which Asterisk prefers to any pattern, and the 79x5's dial template
//     lists it first;
//   - no fixed-length pattern shadows a number someone must be able to
//     dial: a phone's dial template sends a number as soon as it matches a
//     fixed-length pattern, so if every number of one pattern were a prefix
//     of a number in Devices, People, Streams, EchoTest or Emergency, that
//     longer number could never be dialled from a 79x5 (People 1X beside
//     Devices 1XX sends "10" before the third digit). Numbers only a
//     Reserved pattern holds may be shadowed: they play "not available"
//     anyway.
func (p NumberPlan) Validate() error {
	var errs []error
	type named struct{ what, pattern string }
	ranges := []named{{"devices", p.Devices}, {"people", p.People}, {"streams", p.Streams}}
	for i, r := range p.Reserved {
		ranges = append(ranges, named{fmt.Sprintf("reserved[%d]", i), r})
	}
	builtins := []named{{"echoTest", p.EchoTest}, {"emergency", p.Emergency}}

	valid := true
	for _, r := range ranges {
		if !patternSyntax.MatchString(r.pattern) {
			errs = append(errs, fmt.Errorf("number plan: %s %q is not a pattern (digits, X, * and #, with an optional trailing .)", r.what, r.pattern))
			valid = false
		}
	}
	for _, b := range builtins {
		if !plainNumber.MatchString(b.pattern) {
			errs = append(errs, fmt.Errorf("number plan: %s %q is not a number (digits only)", b.what, b.pattern))
			valid = false
		}
	}
	if !valid {
		return errors.Join(errs...)
	}

	for i, a := range ranges {
		for _, b := range ranges[i+1:] {
			if patternsIntersect(a.pattern, b.pattern) {
				errs = append(errs, fmt.Errorf("number plan: %s %q and %s %q overlap", a.what, a.pattern, b.what, b.pattern))
			}
		}
	}
	for _, b := range builtins {
		for _, r := range ranges[:3] {
			if MatchPattern(r.pattern, b.pattern) {
				errs = append(errs, fmt.Errorf("number plan: %s %s is inside %s %q", b.what, b.pattern, r.what, r.pattern))
			}
		}
	}
	if p.EchoTest == p.Emergency {
		errs = append(errs, fmt.Errorf("number plan: echoTest and emergency are both %s", p.EchoTest))
	}

	// Shadowing: a fixed-length pattern against every pattern a person must
	// be able to dial.
	dialled := append(slices.Clone(ranges[:3]), builtins...)
	for _, a := range append(slices.Clone(ranges), builtins...) {
		if strings.HasSuffix(a.pattern, ".") {
			continue
		}
		for _, b := range dialled {
			if a == b {
				continue
			}
			if patternShadows(a.pattern, b.pattern) {
				errs = append(errs, fmt.Errorf("number plan: %s %q shadows %s %q: a phone dials as soon as %q matches, so the longer numbers cannot be dialled",
					a.what, a.pattern, b.what, b.pattern, a.pattern))
			}
		}
	}
	return errors.Join(errs...)
}

// DialPatterns lists every complete-number pattern of the plan for a
// driver's dial template (drivers.RenderContext.DialPatterns): EchoTest,
// Emergency, Devices, People, Streams, then each Reserved pattern.
func (p NumberPlan) DialPatterns() []string {
	return append([]string{p.EchoTest, p.Emergency, p.Devices, p.People, p.Streams}, p.Reserved...)
}

// MatchPattern reports whether number matches pattern, in NumberPlan's
// notation: a digit, "*" or "#" matches itself, X matches any digit, and a
// trailing "." matches one or more digits.
func MatchPattern(pattern, number string) bool {
	body, tail := strings.CutSuffix(pattern, ".")
	if len(number) < len(body) || (!tail && len(number) != len(body)) || (tail && len(number) == len(body)) {
		return false
	}
	for i := range len(body) {
		if !classMatches(body[i], number[i]) {
			return false
		}
	}
	for i := len(body); i < len(number); i++ {
		if !isDigit(number[i]) {
			return false
		}
	}
	return true
}

// patternsIntersect reports whether some number matches both patterns.
func patternsIntersect(a, b string) bool {
	ab, at := strings.CutSuffix(a, ".")
	bb, bt := strings.CutSuffix(b, ".")
	if len(ab) > len(bb) {
		ab, at, bb, bt = bb, bt, ab, at
	}
	// Now len(ab) <= len(bb).
	for i := range len(ab) {
		if !classesIntersect(ab[i], bb[i]) {
			return false
		}
	}
	switch {
	case !at && !bt:
		return len(ab) == len(bb)
	case !at:
		// a is fixed and no longer than b's body; b needs at least one more.
		return false
	default:
		// a's tail must cover the rest of b's body (and then, if b has a
		// tail, one digit more; if not, b's body must be strictly longer).
		for i := len(ab); i < len(bb); i++ {
			if !digitClass(bb[i]) {
				return false
			}
		}
		return bt || len(bb) > len(ab)
	}
}

// patternShadows reports whether some number matching the fixed-length
// pattern a is a proper prefix of some number matching b.
func patternShadows(a, b string) bool {
	bb, bt := strings.CutSuffix(b, ".")
	if !bt && len(bb) <= len(a) {
		return false
	}
	for i := range len(a) {
		if i < len(bb) {
			if !classesIntersect(a[i], bb[i]) {
				return false
			}
		} else if !digitClass(a[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digitClass reports whether a pattern character can stand for a digit.
func digitClass(c byte) bool { return isDigit(c) || c == 'X' }

// classMatches reports whether the pattern character c matches the number
// character n.
func classMatches(c, n byte) bool {
	if c == 'X' {
		return isDigit(n)
	}
	return c == n
}

// classesIntersect reports whether two pattern characters match a common
// character.
func classesIntersect(a, b byte) bool {
	switch {
	case a == 'X':
		return digitClass(b)
	case b == 'X':
		return digitClass(a)
	default:
		return a == b
	}
}
