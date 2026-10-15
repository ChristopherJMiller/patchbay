package compile

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ResourceKind is the kind of a resource that claims a number.
type ResourceKind string

// Resource kinds, in tie-break order: when two resources were created in
// the same second, the earlier kind in this list keeps the number.
const (
	ResourcePhone     ResourceKind = "Phone"
	ResourceSoftphone ResourceKind = "Softphone"
	ResourcePerson    ResourceKind = "Person"
	ResourceStream    ResourceKind = "Stream"
)

// kindOrder is ResolveClaims' tie-break order.
var kindOrder = []ResourceKind{ResourcePhone, ResourceSoftphone, ResourcePerson, ResourceStream}

// ResourceRef names one resource in the PBX namespace.
type ResourceRef struct {
	Kind ResourceKind
	Name string
}

// String is "<Kind> <name>", as claim messages name a resource.
func (r ResourceRef) String() string { return string(r.Kind) + " " + r.Name }

// ClaimProblem is why a resource holds no number (it is not routed).
type ClaimProblem struct {
	// NumberConflict: an older resource holds the number.
	NumberConflict bool
	// MACConflict: an older Phone has the same MAC.
	MACConflict bool
	// NumberOutOfPlan: the number is not in the kind's range of the plan.
	NumberOutOfPlan bool
	// Message is for people: who holds it, or which range it should be in.
	Message string
}

// Claims is the number table: who holds each number, and every resource
// that holds none and why. ResolveClaims builds it from the resources alone,
// so the controller (conditions) and Compile (routing) always agree.
type Claims struct {
	// Holders maps each held number to the resource that holds it.
	Holders map[string]ResourceRef
	// Problems lists every resource that holds no number, and why.
	Problems map[ResourceRef]ClaimProblem
}

// claimant is one resource's claim: its number, its MAC (Phones only) and
// its age.
type claimant struct {
	ref     ResourceRef
	number  string
	mac     string
	created time.Time
}

// older reports whether a wins a contest against b: the older
// creationTimestamp, then the kind order, then the name.
func older(a, b claimant) int {
	if c := a.created.Compare(b.created); c != 0 {
		return c
	}
	if c := cmp.Compare(slices.Index(kindOrder, a.ref.Kind), slices.Index(kindOrder, b.ref.Kind)); c != 0 {
		return c
	}
	return cmp.Compare(a.ref.Name, b.ref.Name)
}

// ResolveClaims builds the number table from every resource (design §4,
// "Numbers"). It is a pure function of the resources and the plan:
//
//  1. MACs (Phones only, compared upper case): the oldest Phone keeps a MAC;
//     every other Phone with it gets MACConflict and holds no number.
//  2. The plan: a Phone's or Softphone's number must be in Devices, a
//     Person's in People, a Stream's in Streams; anything else gets
//     NumberOutOfPlan and holds no number.
//  3. Numbers among the rest: the oldest resource keeps a number, across
//     kinds; every other one gets NumberConflict.
//
// "Oldest" is the older creationTimestamp; ties go to the kind order (Phone,
// Softphone, Person, Stream), then the name.
func ResolveClaims(in Inputs, plan NumberPlan) Claims {
	c := Claims{Holders: map[string]ResourceRef{}, Problems: map[ResourceRef]ClaimProblem{}}

	var all []claimant
	for i := range in.Phones {
		p := &in.Phones[i]
		all = append(all, claimant{
			ref:     ResourceRef{ResourcePhone, p.Name},
			number:  string(p.Spec.Number),
			mac:     p.Spec.MAC.Normalized(),
			created: p.CreationTimestamp.Time,
		})
	}
	for i := range in.Softphones {
		s := &in.Softphones[i]
		all = append(all, claimant{ref: ResourceRef{ResourceSoftphone, s.Name}, number: string(s.Spec.Number), created: s.CreationTimestamp.Time})
	}
	for i := range in.People {
		p := &in.People[i]
		all = append(all, claimant{ref: ResourceRef{ResourcePerson, p.Name}, number: string(p.Spec.Number), created: p.CreationTimestamp.Time})
	}
	for i := range in.Streams {
		s := &in.Streams[i]
		all = append(all, claimant{ref: ResourceRef{ResourceStream, s.Name}, number: string(s.Spec.Number), created: s.CreationTimestamp.Time})
	}
	slices.SortStableFunc(all, older)

	// 1. MACs. all is oldest first, so the first Phone with a MAC keeps it.
	macHolders := map[string]ResourceRef{}
	var rest []claimant
	for _, cl := range all {
		if cl.ref.Kind == ResourcePhone && cl.mac != "" {
			if holder, taken := macHolders[cl.mac]; taken {
				c.Problems[cl.ref] = ClaimProblem{
					MACConflict: true,
					Message:     fmt.Sprintf("MAC %s is %s's, which is older", cl.mac, holder),
				}
				continue
			}
			macHolders[cl.mac] = cl.ref
		}
		rest = append(rest, cl)
	}

	// 2 and 3. The plan, then the numbers, oldest first.
	for _, cl := range rest {
		if pattern := kindRange(plan, cl.ref.Kind); !MatchPattern(pattern, cl.number) {
			c.Problems[cl.ref] = ClaimProblem{
				NumberOutOfPlan: true,
				Message:         fmt.Sprintf("number %s is not in the %s range %s", cl.number, cl.ref.Kind, pattern),
			}
			continue
		}
		if holder, taken := c.Holders[cl.number]; taken {
			c.Problems[cl.ref] = ClaimProblem{
				NumberConflict: true,
				Message:        fmt.Sprintf("number %s is held by %s, which is older", cl.number, holder),
			}
			continue
		}
		c.Holders[cl.number] = cl.ref
	}
	return c
}

// kindRange is the plan's range for a kind.
func kindRange(plan NumberPlan, kind ResourceKind) string {
	switch kind {
	case ResourcePhone, ResourceSoftphone:
		return plan.Devices
	case ResourcePerson:
		return plan.People
	case ResourceStream:
		return plan.Streams
	}
	return ""
}

// Holds reports whether the resource holds its number: it is routed.
func (c Claims) Holds(ref ResourceRef) bool {
	_, problem := c.Problems[ref]
	return !problem
}

// Apply sets a device's claim fields (NumberConflict, MACConflict,
// NumberOutOfPlan, ClaimMessage) from the table, clearing them when the
// device holds its number.
func (c Claims) Apply(dev *drivers.Device) {
	p := c.Problems[deviceRef(dev.Kind, dev.Name)]
	dev.NumberConflict = p.NumberConflict
	dev.MACConflict = p.MACConflict
	dev.NumberOutOfPlan = p.NumberOutOfPlan
	dev.ClaimMessage = p.Message
}

// deviceRef is the ResourceRef of a device.
func deviceRef(kind drivers.DeviceKind, name string) ResourceRef {
	switch kind {
	case drivers.KindPhone:
		return ResourceRef{ResourcePhone, name}
	case drivers.KindSoftphone:
		return ResourceRef{ResourceSoftphone, name}
	}
	return ResourceRef{ResourceKind(kind), name}
}
