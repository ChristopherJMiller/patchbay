package cisco79x5_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// template is one parsed <TEMPLATE/>.
type template struct {
	match   string
	timeout int
}

// parseDialTemplate parses a dial template the way the phone's XML parser
// would see it, failing on anything outside the documented format
// (https://usecallmanager.nz/dial-template.html): a <dialTemplate> root, a
// <versionStamp>, and upper-case <TEMPLATE> elements with lower-case match
// and timeout attributes.
func parseDialTemplate(t *testing.T, b []byte) (stamp string, ts []template) {
	t.Helper()
	var doc struct {
		XMLName      xml.Name
		VersionStamp string `xml:"versionStamp"`
		Templates    []struct {
			Match   *string    `xml:"match,attr"`
			Timeout *string    `xml:"timeout,attr"`
			Other   []xml.Attr `xml:",any,attr"`
		} `xml:"TEMPLATE"`
		Other []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("dial template does not parse: %v\n%s", err, b)
	}
	if doc.XMLName.Local != "dialTemplate" {
		t.Errorf("root is <%s>, want <dialTemplate>", doc.XMLName.Local)
	}
	for _, o := range doc.Other {
		t.Errorf("unexpected element <%s>", o.XMLName.Local)
	}
	for _, e := range doc.Templates {
		if e.Match == nil || e.Timeout == nil || len(e.Other) > 0 {
			t.Fatalf("TEMPLATE needs exactly match and timeout: %+v", e)
		}
		n, err := strconv.Atoi(*e.Timeout)
		if err != nil || n < 0 {
			t.Fatalf("timeout %q is not a number of seconds", *e.Timeout)
		}
		ts = append(ts, template{*e.Match, n})
	}
	return doc.VersionStamp, ts
}

func TestDialTemplateGolden(t *testing.T) {
	got := cisco79x5.DialTemplate(housePatterns)
	golden(t, "dialplan.xml", got)

	stamp, ts := parseDialTemplate(t, got)
	// The stamp is the SHA-256 of the template lines, so the file depends
	// only on the patterns.
	var lines []byte
	for _, l := range bytes.SplitAfter(got, []byte("\n")) {
		if bytes.Contains(l, []byte("<TEMPLATE ")) {
			lines = append(lines, l...)
		}
	}
	sum := sha256.Sum256(lines)
	if want := hex.EncodeToString(sum[:])[:32]; stamp != want {
		t.Errorf("versionStamp = %q, want %q", stamp, want)
	}
	want := []template{
		{"600", 0}, {"911", 0}, {"0", 0}, {"1..", 0}, {"3..", 0}, {"7..", 0}, {"2..", 0},
		{"9.*", 3},
		{"*", 5},
	}
	if !slices.Equal(ts, want) {
		t.Errorf("templates = %v, want %v", ts, want)
	}
	if !bytes.Equal(got, cisco79x5.DialTemplate(slices.Clone(housePatterns))) {
		t.Error("DialTemplate is not deterministic")
	}
}

func TestDialTemplateConversion(t *testing.T) {
	for _, c := range []struct {
		name     string
		patterns []string
		want     []template // before the catch-all
	}{
		{"fixed", []string{"1XX"}, []template{{"1..", 0}}},
		{"one or more", []string{"9."}, []template{{"9.*", 3}}},
		{"X then one or more", []string{"5X."}, []template{{"5..*", 3}}},
		{"star and hash omitted", []string{"*8", "#1", "1*", "*8."}, nil},
		{"empty would be a hotline", []string{"", "."}, nil},
		{"outside the notation", []string{"1N", "[12]X", "1.X", "9..", "x1"}, nil},
		{"written once", []string{"1XX", "1XX", "600", "600"}, []template{{"600", 0}, {"1..", 0}}},
		{"most specific first", []string{"*", "9.", "XXX", "911", "9X."}, []template{
			{"911", 0}, {"...", 0}, {"9..*", 3}, {"9.*", 3},
		}},
		{"none", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ts := parseDialTemplate(t, cisco79x5.DialTemplate(c.patterns))
			want := append(slices.Clone(c.want), template{"*", 5})
			if !slices.Equal(ts, want) {
				t.Errorf("DialTemplate(%q) = %v, want %v", c.patterns, ts, want)
			}
		})
	}

	// The bench switch (plan §8): off, so "*8" is omitted; on, it becomes
	// the documented literal star.
	if cisco79x5.DialTemplateLiteralStar {
		t.Error("dialTemplateLiteralStar is on before the bench has shown `\\*` is literal")
	}
	_, ts := parseDialTemplate(t, cisco79x5.DialTemplateWith([]string{"*8", "#1"}, true))
	if want := []template{{`\*8`, 0}, {"#1", 0}, {"*", 5}}; !slices.Equal(ts, want) {
		t.Errorf("with literal stars: %v, want %v", ts, want)
	}
}

// compiled caches templateRegexp: the matching test asks about the same few
// templates hundreds of thousands of times.
var compiled sync.Map // match -> *regexp.Regexp

// templateRegexp compiles a match per the documented syntax: a digit or #
// matches itself, "." one digit, # or *, "*" zero or more of them, and `\*`
// a literal star.
func templateRegexp(match string) *regexp.Regexp {
	if re, ok := compiled.Load(match); ok {
		return re.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(match); i++ {
		switch c := match[i]; {
		case c == '\\' && i+1 < len(match):
			b.WriteString(regexp.QuoteMeta(match[i+1 : i+2]))
			i++
		case c == '.':
			b.WriteString(`[0-9#*]`)
		case c == '*':
			b.WriteString(`[0-9#*]*`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re := regexp.MustCompile(b.String())
	compiled.Store(match, re)
	return re
}

// token is one element of a match: a literal character, "." (any) or "*".
type token struct {
	any, star bool
	lit       byte
}

func tokens(match string) []token {
	var out []token
	for i := 0; i < len(match); i++ {
		switch c := match[i]; c {
		case '\\':
			out = append(out, token{lit: match[i+1]})
			i++
		case '.':
			out = append(out, token{any: true})
		case '*':
			out = append(out, token{star: true})
		default:
			out = append(out, token{lit: c})
		}
	}
	return out
}

// subset reports whether every string match a matches is matched by b, for
// matches whose only "*" is the last element (what DialTemplate writes).
// It is the "more specific" relation of the second reading.
func subset(t *testing.T, a, b string) bool {
	ta, tb := tokens(a), tokens(b)
	for i, x := range ta {
		if x.star && i != len(ta)-1 {
			t.Fatalf("%q has a * that is not last", a)
		}
	}
	for i, x := range tb {
		if x.star && i != len(tb)-1 {
			t.Fatalf("%q has a * that is not last", b)
		}
	}
	aStar := len(ta) > 0 && ta[len(ta)-1].star
	bStar := len(tb) > 0 && tb[len(tb)-1].star
	if aStar {
		ta = ta[:len(ta)-1]
	}
	if bStar {
		tb = tb[:len(tb)-1]
	}
	// a's strings are its prefix then (if aStar) anything.
	if !bStar && (aStar || len(ta) != len(tb)) {
		return false
	}
	if len(ta) < len(tb) {
		return false
	}
	for i := range tb {
		switch {
		case tb[i].any:
		case ta[i].any:
			return false
		case ta[i].lit != tb[i].lit:
			return false
		}
	}
	return true
}

// chosen is the template the phone acts on after the digits so far, under
// one reading of the undocumented choice; ok is false when no template
// matches all the digits.
type reading func(t *testing.T, ts []template, digits string) (template, bool)

// firstMatch: the first full match in file order.
func firstMatch(_ *testing.T, ts []template, digits string) (template, bool) {
	for _, x := range ts {
		if templateRegexp(x.match).MatchString(digits) {
			return x, true
		}
	}
	return template{}, false
}

// mostSpecific: the full match that matches nothing the others do not (a
// subset of every other full match). Fails when no such template exists,
// because the phone's choice would then depend on an undocumented rule.
func mostSpecific(t *testing.T, ts []template, digits string) (template, bool) {
	var full []template
	for _, x := range ts {
		if templateRegexp(x.match).MatchString(digits) {
			full = append(full, x)
		}
	}
	for _, m := range full {
		if !slices.ContainsFunc(full, func(o template) bool { return !subset(t, m.match, o.match) }) {
			return m, true
		}
	}
	if len(full) > 0 {
		t.Fatalf("after %q no full match is the most specific of %v", digits, full)
	}
	return template{}, false
}

// dial simulates dialling digits one by one: it returns how many digits
// were dialled when the phone dialled at once (a full match with timeout
// 0; 0 if never), and the template chosen after the last digit.
func dial(t *testing.T, r reading, ts []template, number string) (dialledAt int, last template) {
	for i := 1; i <= len(number); i++ {
		x, ok := r(t, ts, number[:i])
		if !ok {
			t.Fatalf("no template matches %q: the catch-all is missing", number[:i])
		}
		if x.timeout == 0 && dialledAt == 0 {
			dialledAt = i
		}
		last = x
	}
	return dialledAt, last
}

// TestDialTemplateMatching is the §11 criterion "dialling 600 connects at
// once", and the 911 review focus (plan §10.2), checked under both
// readings of how the phone picks among matching templates.
func TestDialTemplateMatching(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"house", cisco79x5.DialTemplate(housePatterns)},
		{"literal stars", cisco79x5.DialTemplateWith(housePatterns, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := parseDialTemplate(t, tc.b)

			// Both readings choose the same template after every prefix of
			// every string of up to 4 keys.
			alphabet := "0123456789*#"
			var all []string
			frontier := []string{""}
			for range 4 {
				var next []string
				for _, p := range frontier {
					for _, k := range alphabet {
						next = append(next, p+string(k))
					}
				}
				all = append(all, next...)
				frontier = next
			}
			all = append(all, "9206555", "912", "1000")
			for _, n := range all {
				a, aok := firstMatch(t, ts, n)
				b, bok := mostSpecific(t, ts, n)
				if a != b || aok != bok {
					t.Fatalf("after %q: first match %v, most specific %v", n, a, b)
				}
			}

			for number, match := range map[string]string{
				"100": "1..", "150": "1..", "301": "3..", "600": "600", "911": "911", "705": "7..", "0": "0",
			} {
				for _, r := range []reading{firstMatch, mostSpecific} {
					at, last := dial(t, r, ts, number)
					if at != len(number) || last.match != match {
						t.Errorf("%s dials after %d digits on %v; want at once after all %d on %q",
							number, at, last, len(number), match)
					}
				}
			}
			for _, number := range []string{"912", "9206555"} {
				for _, r := range []reading{firstMatch, mostSpecific} {
					if at, last := dial(t, r, ts, number); at != 0 || last.timeout != 3 {
						t.Errorf("%s: dials after %d digits, then waits on %v; want a 3 s wait", number, at, last)
					}
				}
			}
			// 911 is never cut short by 9.: after 9 and 91 the phone waits.
			for _, prefix := range []string{"9", "91"} {
				if x, _ := firstMatch(t, ts, prefix); x.timeout == 0 {
					t.Errorf("%q dials at once on %v", prefix, x)
				}
			}
		})
	}

	_, ts := parseDialTemplate(t, cisco79x5.DialTemplate(housePatterns))
	for _, x := range ts {
		if strings.Contains(x.match, "8") || strings.Contains(x.match, `\`) {
			t.Errorf("template %q: *8 must be omitted while dialTemplateLiteralStar is off", x.match)
		}
	}
}
