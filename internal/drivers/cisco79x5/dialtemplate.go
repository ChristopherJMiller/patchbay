package cisco79x5

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"slices"
	"strconv"
	"strings"
)

// DialTemplateFile is the dial template's name: the SEP file's dialTemplate,
// and the bundle path tftp/dialplan.xml. Every phone shares it.
const DialTemplateFile = "dialplan.xml"

// dialTemplateLiteralStar says whether the dial template may contain a
// pattern with a literal "*" (or "#"), such as page-all's "*8".
// usecallmanager.nz documents `\*` as a literal star, but if this firmware
// read `\*8` as the wildcard "*" then "8", any number would dial the moment
// an 8 is pressed ("18" instead of "180"). Omitting "*8" costs only the
// catch-all's wait. Bench (plan §8): true adds match="\*8" once the phone is
// seen to wait for the third digit of "180".
const dialTemplateLiteralStar = false

const (
	// timeoutFixed is a complete fixed-length number's timeout: dial at once.
	timeoutFixed = 0
	// timeoutOpen is the wait after a variable-length number ("9." in
	// Asterisk notation) for another digit.
	timeoutOpen = 3
	// timeoutCatchAll is the wait after anything no pattern names.
	timeoutCatchAll = 5
	// catchAllMatch matches anything, so a number outside the plan still
	// dials after timeoutCatchAll instead of the phone's 15 s default.
	catchAllMatch = "*"
)

// dialTemplateEntry is one <TEMPLATE/>.
type dialTemplateEntry struct {
	match   string
	timeout int
}

// DialTemplate returns the dial template (dialplan.xml) for the number
// plan's complete-number patterns, so every complete number dials at once
// instead of after the phone's 15 s timeout (design §4, §11: "dialling 600
// connects at once"). The format is https://usecallmanager.nz/dial-template.html:
// a <dialTemplate> root, a <versionStamp>, then one <TEMPLATE match="…"
// timeout="…"/> per pattern. In match, a digit or # matches itself, "."
// matches one digit, # or *, "*" matches zero or more of them, and `\*` is a
// literal star.
//
// Each pattern (Asterisk notation, see drivers.RenderContext.DialPatterns)
// becomes:
//   - a fixed-length pattern: X becomes ".", timeout 0 (dial at once);
//   - a trailing "." (one or more digits): "<prefix>.*", timeout 3;
//   - a pattern with "*" or "#": omitted (dialTemplateLiteralStar);
//   - an empty pattern, or one outside the notation: omitted, since an empty
//     match is a hotline (the phone dials the moment it goes off-hook).
//
// Last comes the catch-all <TEMPLATE match="*" timeout="5"/>.
//
// Whether the phone takes the first matching template or the most specific
// one is not documented, so the templates are written most specific first
// (see moreSpecific; a stable sort, so equally specific patterns keep the
// plan's order). The two readings then pick the same template for every
// number. A pattern that converts to a match already written is written
// once. versionStamp is the first 32 hex digits of the SHA-256 of the
// template lines, so the file depends only on the patterns and is
// byte-identical for every phone.
func DialTemplate(patterns []string) []byte {
	return dialTemplate(patterns, dialTemplateLiteralStar)
}

func dialTemplate(patterns []string, literalStar bool) []byte {
	var entries []dialTemplateEntry
	for _, p := range patterns {
		e, ok := convertPattern(p, literalStar)
		if !ok || slices.ContainsFunc(entries, func(o dialTemplateEntry) bool { return o.match == e.match }) {
			continue
		}
		entries = append(entries, e)
	}
	slices.SortStableFunc(entries, func(a, b dialTemplateEntry) int {
		switch {
		case moreSpecific(a.match, b.match):
			return -1
		case moreSpecific(b.match, a.match):
			return 1
		default:
			return 0
		}
	})
	entries = append(entries, dialTemplateEntry{match: catchAllMatch, timeout: timeoutCatchAll})

	var lines bytes.Buffer
	for _, e := range entries {
		lines.WriteString(`  <TEMPLATE match="`)
		escapeAttr(&lines, e.match)
		lines.WriteString(`" timeout="`)
		lines.WriteString(strconv.Itoa(e.timeout))
		lines.WriteString("\"/>\n")
	}
	sum := sha256.Sum256(lines.Bytes())

	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString("<dialTemplate>\n")
	out.WriteString("  <versionStamp>" + hex.EncodeToString(sum[:])[:32] + "</versionStamp>\n")
	out.Write(lines.Bytes())
	out.WriteString("</dialTemplate>\n")
	return out.Bytes()
}

// convertPattern converts one Asterisk-notation pattern to a template, or
// reports false when the pattern is left out.
func convertPattern(p string, literalStar bool) (dialTemplateEntry, bool) {
	body, open := strings.CutSuffix(p, ".")
	if body == "" {
		return dialTemplateEntry{}, false
	}
	var match strings.Builder
	for _, r := range body {
		switch {
		case r >= '0' && r <= '9':
			match.WriteRune(r)
		case r == 'X':
			match.WriteRune('.')
		case r == '*' && literalStar:
			match.WriteString(`\*`)
		case r == '#' && literalStar:
			match.WriteRune('#')
		default:
			return dialTemplateEntry{}, false
		}
	}
	if open {
		return dialTemplateEntry{match: match.String() + ".*", timeout: timeoutOpen}, true
	}
	return dialTemplateEntry{match: match.String(), timeout: timeoutFixed}, true
}

// moreSpecific reports whether template match a matches fewer numbers than
// b, in the sense the phone could use to pick the "most specific" template:
// a fixed-length match (no "*" wildcard) before a variable-length one; then
// more literal characters; then, among variable-length matches, more "."
// (a longer minimum), and among fixed-length ones fewer "." (two fixed
// matches can only both match a number of their common length).
func moreSpecific(a, b string) bool {
	fa, la, da := templateShape(a)
	fb, lb, db := templateShape(b)
	switch {
	case fa != fb:
		return fa
	case la != lb:
		return la > lb
	case fa:
		return da < db
	default:
		return da > db
	}
}

// templateShape describes a match: whether it has no "*" wildcard, how many
// literal characters it has (a digit, "#" or `\*`), and how many ".".
func templateShape(match string) (fixed bool, literals, dots int) {
	fixed = true
	for i := 0; i < len(match); i++ {
		switch match[i] {
		case '\\':
			literals++
			i++ // `\*` is one literal star
		case '*':
			fixed = false
		case '.':
			dots++
		default:
			literals++
		}
	}
	return fixed, literals, dots
}

// escapeAttr writes s escaped for a double-quoted XML attribute value.
func escapeAttr(b *bytes.Buffer, s string) {
	// xml.EscapeText escapes <, >, &, ' and " (and white space controls),
	// which is all an attribute needs. Writing to a bytes.Buffer never fails.
	_ = xml.EscapeText(b, []byte(s))
}
