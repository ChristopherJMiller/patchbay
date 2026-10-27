package messages

// Internals the external tests (package messages_test) check.

// PhoneViewOf is the shell's PhoneView of one Phone (phoneView).
var PhoneViewOf = phoneView

// RuleNames lists the rule table's row names, in order.
func RuleNames() []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.name
	}
	return out
}

// MatchingRows lists every row (numbered from 1) whose predicate holds for
// the Message named name in the input, in table order; Decide uses the
// first. Nil when no such Message is in the input.
func MatchingRows(in Input, name string) []int {
	d := newDecider(in)
	for _, m := range d.msgs {
		if m.Name != name {
			continue
		}
		var out []int
		for i, r := range rules {
			if r.match(d, m) {
				out = append(out, i+1)
			}
		}
		return out
	}
	return nil
}
