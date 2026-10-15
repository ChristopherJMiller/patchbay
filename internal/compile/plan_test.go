package compile_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestMatchPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		yes, no []string
	}{
		{"1XX", []string{"100", "150", "199"}, []string{"10", "1000", "200", "1X0", "", "1*0"}},
		{"9.", []string{"91", "911", "9123456"}, []string{"9", "8", "9*", "19"}},
		{"*8", []string{"*8"}, []string{"8", "*", "*80", "#8"}},
		{"0", []string{"0"}, []string{"00", "1", ""}},
		{"600", []string{"600"}, []string{"601", "60", "6000"}},
		{"X.", []string{"12", "999"}, []string{"1", "*1"}},
		{"#X", []string{"#1"}, []string{"*1", "#", "#12"}},
	} {
		for _, n := range tc.yes {
			if !compile.MatchPattern(tc.pattern, n) {
				t.Errorf("MatchPattern(%q, %q) = false, want true", tc.pattern, n)
			}
		}
		for _, n := range tc.no {
			if compile.MatchPattern(tc.pattern, n) {
				t.Errorf("MatchPattern(%q, %q) = true, want false", tc.pattern, n)
			}
		}
	}
}

func TestDialPatterns(t *testing.T) {
	got := compile.DefaultNumberPlan().DialPatterns()
	if want := []string{"600", "911", "1XX", "3XX", "7XX", "2XX", "0", "9.", "*8"}; !slices.Equal(got, want) {
		t.Errorf("DialPatterns() = %v, want %v", got, want)
	}
}

func TestNumberPlanValidate(t *testing.T) {
	if err := compile.DefaultNumberPlan().Validate(); err != nil {
		t.Fatalf("the default plan (911 inside 9.) is invalid: %v", err)
	}
	ok := func(edit func(p *compile.NumberPlan)) compile.NumberPlan {
		p := compile.DefaultNumberPlan()
		edit(&p)
		return p
	}
	for _, tc := range []struct {
		name string
		plan compile.NumberPlan
	}{
		{"a two-digit range on its own", ok(func(p *compile.NumberPlan) { p.Streams = "8X" })},
		{"no reserved patterns", ok(func(p *compile.NumberPlan) { p.Reserved = nil })},
		{"a built-in inside a reserved pattern", ok(func(p *compile.NumberPlan) { p.EchoTest = "222" })},
		{"a reserved number shadowed by a built-in", ok(func(p *compile.NumberPlan) { p.Reserved = append(p.Reserved, "60XX") })},
		{"a variable range beside a fixed one", ok(func(p *compile.NumberPlan) { p.Streams = "7."; p.Reserved = []string{"0", "*8"} })},
	} {
		if err := tc.plan.Validate(); err != nil {
			t.Errorf("%s: %+v is invalid: %v", tc.name, tc.plan, err)
		}
	}

	for _, tc := range []struct {
		name string
		plan compile.NumberPlan
		want string
	}{
		{"overlapping ranges", ok(func(p *compile.NumberPlan) { p.People = "10X" }), `devices "1XX" and people "10X" overlap`},
		{"a reserved pattern over a range", ok(func(p *compile.NumberPlan) { p.Reserved = []string{"1."} }), `devices "1XX" and reserved[0] "1." overlap`},
		{"a reserved pattern listed twice", ok(func(p *compile.NumberPlan) { p.Reserved = []string{"0", "0"} }), `reserved[0] "0" and reserved[1] "0" overlap`},
		{"a built-in inside the device range", ok(func(p *compile.NumberPlan) { p.EchoTest = "100" }), `echoTest 100 is inside devices "1XX"`},
		{"equal built-ins", ok(func(p *compile.NumberPlan) { p.Emergency = "600" }), "echoTest and emergency are both 600"},
		{"a range a phone dials before the next digit", ok(func(p *compile.NumberPlan) { p.People = "1X" }), `people "1X" shadows devices "1XX"`},
		{"a built-in that shadows a range", ok(func(p *compile.NumberPlan) { p.EchoTest = "60"; p.Streams = "6XX" }), `echoTest "60" shadows streams "6XX"`},
		{"a reserved number that shadows a range", ok(func(p *compile.NumberPlan) { p.Reserved = []string{"3"} }), `reserved[0] "3" shadows people "3XX"`},
		{"a built-in that shadows a built-in", ok(func(p *compile.NumberPlan) { p.Emergency = "6000" }), `echoTest "600" shadows emergency "6000"`},
		{"a dot that is not last", ok(func(p *compile.NumberPlan) { p.Streams = "X.X" }), `streams "X.X" is not a pattern`},
		{"a letter", ok(func(p *compile.NumberPlan) { p.Devices = "12a" }), `devices "12a" is not a pattern`},
		{"an empty range", ok(func(p *compile.NumberPlan) { p.People = "" }), `people "" is not a pattern`},
		{"a lone dot", ok(func(p *compile.NumberPlan) { p.Reserved = []string{"."} }), `reserved[0] "." is not a pattern`},
		{"a built-in pattern", ok(func(p *compile.NumberPlan) { p.Emergency = "9X1" }), `emergency "9X1" is not a number`},
		{"a built-in with a star", ok(func(p *compile.NumberPlan) { p.EchoTest = "*6" }), `echoTest "*6" is not a number`},
	} {
		err := tc.plan.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}
