package compile_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

func ref(kind compile.ResourceKind, name string) compile.ResourceRef {
	return compile.ResourceRef{Kind: kind, Name: name}
}

func TestResolveClaims(t *testing.T) {
	plan := compile.DefaultNumberPlan()
	for _, tc := range []struct {
		name     string
		in       compile.Inputs
		holders  map[string]compile.ResourceRef
		problems map[compile.ResourceRef]compile.ClaimProblem
	}{
		{
			name: "the older resource keeps a number",
			in: compile.Inputs{Phones: []v1alpha1.Phone{
				phone("kitchen", 5, modelPhone, "D0C78915EBB5", "100", ""),
				phone("desk", 0, modelPhone, "D0C78915E8C8", "100", ""),
			}},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "desk")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePhone, "kitchen"): {
				NumberConflict: true, Message: "number 100 is held by Phone desk, which is older",
			}},
		},
		{
			name: "a tie goes to the name",
			in: compile.Inputs{Phones: []v1alpha1.Phone{
				phone("b", 0, modelPhone, "D0C78915EBB5", "100", ""),
				phone("a", 0, modelPhone, "D0C78915E8C8", "100", ""),
			}},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "a")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePhone, "b"): {
				NumberConflict: true, Message: "number 100 is held by Phone a, which is older",
			}},
		},
		{
			name: "a tie across kinds goes to the kind order",
			in: compile.Inputs{
				Phones:     []v1alpha1.Phone{phone("z", 0, modelPhone, "D0C78915E8C8", "100", "")},
				Softphones: []v1alpha1.Softphone{softphone("a", 0, "100", "")},
			},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "z")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourceSoftphone, "a"): {
				NumberConflict: true, Message: "number 100 is held by Phone z, which is older",
			}},
		},
		{
			name: "an older softphone beats a newer phone",
			in: compile.Inputs{
				Phones:     []v1alpha1.Phone{phone("desk", 5, modelPhone, "D0C78915E8C8", "150", "")},
				Softphones: []v1alpha1.Softphone{softphone("laptop", 0, "150", "")},
			},
			holders: map[string]compile.ResourceRef{"150": ref(compile.ResourceSoftphone, "laptop")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePhone, "desk"): {
				NumberConflict: true, Message: "number 150 is held by Softphone laptop, which is older",
			}},
		},
		{
			name: "MACs compare case-insensitively; the MAC loser holds no number",
			in: compile.Inputs{Phones: []v1alpha1.Phone{
				phone("desk", 0, modelPhone, "D0C78915E8C8", "100", ""),
				phone("office", 5, modelPhone, "d0c78915e8c8", "101", ""),
			}},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "desk")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePhone, "office"): {
				MACConflict: true, Message: "MAC D0C78915E8C8 is Phone desk's, which is older",
			}},
		},
		{
			name: "a MAC loser does not block a newer claimant of its number",
			in: compile.Inputs{
				Phones: []v1alpha1.Phone{
					phone("desk", 0, modelPhone, "D0C78915E8C8", "100", ""),
					phone("office", 1, modelPhone, "D0C78915E8C8", "101", ""),
				},
				Softphones: []v1alpha1.Softphone{softphone("laptop", 2, "101", "")},
			},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "desk"), "101": ref(compile.ResourceSoftphone, "laptop")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePhone, "office"): {
				MACConflict: true, Message: "MAC D0C78915E8C8 is Phone desk's, which is older",
			}},
		},
		{
			name: "out of plan holds nothing, and does not block",
			in: compile.Inputs{
				Phones:     []v1alpha1.Phone{phone("lab", 0, modelPhone, "D0C78915E8C8", "600", "")},
				Softphones: []v1alpha1.Softphone{softphone("tablet", 0, "1500", ""), softphone("pad", 1, "301", "")},
				People:     []v1alpha1.Person{person("chris", 2, "301", "Chris"), person("front", 0, "100", "Front")},
				Streams:    []v1alpha1.Stream{stream("king5", 0, "705"), stream("news", 0, "305")},
			},
			holders: map[string]compile.ResourceRef{"301": ref(compile.ResourcePerson, "chris"), "705": ref(compile.ResourceStream, "king5")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{
				ref(compile.ResourcePhone, "lab"):        {NumberOutOfPlan: true, Message: "number 600 is not in the Phone range 1XX"},
				ref(compile.ResourceSoftphone, "tablet"): {NumberOutOfPlan: true, Message: "number 1500 is not in the Softphone range 1XX"},
				ref(compile.ResourceSoftphone, "pad"):    {NumberOutOfPlan: true, Message: "number 301 is not in the Softphone range 1XX"},
				ref(compile.ResourcePerson, "front"):     {NumberOutOfPlan: true, Message: "number 100 is not in the Person range 3XX"},
				ref(compile.ResourceStream, "news"):      {NumberOutOfPlan: true, Message: "number 305 is not in the Stream range 7XX"},
			},
		},
		{
			name: "a person and a phone on one number: the plan decides first",
			in: compile.Inputs{
				Phones: []v1alpha1.Phone{phone("desk", 5, modelPhone, "D0C78915E8C8", "100", "")},
				People: []v1alpha1.Person{person("chris", 0, "100", "Chris")},
			},
			holders: map[string]compile.ResourceRef{"100": ref(compile.ResourcePhone, "desk")},
			problems: map[compile.ResourceRef]compile.ClaimProblem{ref(compile.ResourcePerson, "chris"): {
				NumberOutOfPlan: true, Message: "number 100 is not in the Person range 3XX",
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := compile.ResolveClaims(tc.in, plan)
			if !maps.Equal(got.Holders, tc.holders) {
				t.Errorf("holders %v, want %v", got.Holders, tc.holders)
			}
			if !reflect.DeepEqual(got.Problems, tc.problems) {
				t.Errorf("problems %v, want %v", got.Problems, tc.problems)
			}
		})
	}
}

func TestClaimsApply(t *testing.T) {
	in := compile.Inputs{Phones: []v1alpha1.Phone{
		phone("desk", 0, modelPhone, "D0C78915E8C8", "100", ""),
		phone("office", 5, modelPhone, "D0C78915E8C8", "101", ""),
		phone("kitchen", 5, modelPhone, "D0C78915EBB5", "100", ""),
		phone("lab", 5, modelPhone, "D0C78915A001", "600", ""),
	}}
	claims := compile.ResolveClaims(in, compile.DefaultNumberPlan())
	for i, want := range []drivers.Device{
		{},
		{MACConflict: true, ClaimMessage: "MAC D0C78915E8C8 is Phone desk's, which is older"},
		{NumberConflict: true, ClaimMessage: "number 100 is held by Phone desk, which is older"},
		{NumberOutOfPlan: true, ClaimMessage: "number 600 is not in the Phone range 1XX"},
	} {
		dev := drivers.PhoneDevice(&in.Phones[i])
		// Stale claim fields are cleared, not merged.
		dev.NumberConflict, dev.MACConflict, dev.NumberOutOfPlan, dev.ClaimMessage = true, true, true, "stale"
		claims.Apply(&dev)
		got := drivers.Device{NumberConflict: dev.NumberConflict, MACConflict: dev.MACConflict,
			NumberOutOfPlan: dev.NumberOutOfPlan, ClaimMessage: dev.ClaimMessage}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: claim fields %+v, want %+v", dev.Name, got, want)
		}
		if holds := claims.Holds(ref(compile.ResourcePhone, dev.Name)); holds != (i == 0) {
			t.Errorf("%s: Holds = %t", dev.Name, holds)
		}
	}
	sp := softphone("laptop", 0, "150", "")
	dev := drivers.SoftphoneDevice(&sp)
	claims.Apply(&dev)
	if dev.NumberConflict || dev.MACConflict || dev.NumberOutOfPlan || dev.ClaimMessage != "" {
		t.Errorf("a softphone absent from the claims got %+v", dev)
	}
	if got := ref(compile.ResourcePerson, "chris").String(); got != "Person chris" {
		t.Errorf("ResourceRef.String() = %q", got)
	}
}
