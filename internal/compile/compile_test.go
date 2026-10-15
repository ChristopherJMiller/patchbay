package compile_test

import (
	"errors"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestPlaceholderRoundTrip(t *testing.T) {
	for _, ref := range []compile.SecretRef{
		{Name: "phone-desk", Key: "sip-password"},
		{Name: "patchbay.agent", Key: "token"},
		{Name: "softphone-chris-laptop", Key: "provisioning_token"},
	} {
		text := "<authPassword>" + ref.Placeholder() + "</authPassword>"
		m := compile.PlaceholderPattern.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("%q: placeholder not matched in %q", ref, text)
		}
		if got := (compile.SecretRef{Name: m[1], Key: m[2]}); got != ref {
			t.Errorf("parsed %+v, want %+v", got, ref)
		}
	}
}

func TestPlaceholderPatternRejects(t *testing.T) {
	for _, text := range []string{
		"@@secret:Upper/key@@", // Secret names are lower case
		"@@secret:name@@",      // no key
		"@@secret:/key@@",      // no name
		"@@secret:name/key with space@@",
		"@secret:name/key@",
	} {
		if compile.PlaceholderPattern.MatchString(text) {
			t.Errorf("%q matched; want no match", text)
		}
	}
}

func TestDefaultNumberPlan(t *testing.T) {
	p := compile.DefaultNumberPlan()
	if p.Devices != "1XX" || p.People != "3XX" || p.Streams != "7XX" ||
		p.EchoTest != "600" || p.Emergency != "911" || len(p.Reserved) != 4 {
		t.Fatalf("default number plan drifted from design §4: %+v", p)
	}
}

// TestCompileIsAStub pins the foundation-stage contract: Compile exists with
// its final signature and says plainly that it is not written yet.
func TestCompileIsAStub(t *testing.T) {
	_, err := compile.Compile(compile.Inputs{}, compile.Settings{NumberPlan: compile.DefaultNumberPlan()})
	if !errors.Is(err, compile.ErrNotImplemented) {
		t.Fatalf("Compile error = %v, want ErrNotImplemented", err)
	}
}
