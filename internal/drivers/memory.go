package drivers

import (
	"time"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// DeepCopy is a copy of m that shares no pointer with it, so a Decision
// built from a device's memory never aliases it. Every driver's fold starts
// from it; TestMemoryDeepCopy checks every field, so a field added to
// Memory cannot be shallow-copied by one driver and not another.
func (m Memory) DeepCopy() Memory {
	return Memory{
		FirstSeen:        m.FirstSeen.DeepCopy(),
		Firmware:         m.Firmware,
		Address:          m.Address,
		LastConfigFetch:  m.LastConfigFetch.DeepCopy(),
		LastRegistered:   m.LastRegistered.DeepCopy(),
		UpgradeStartedAt: m.UpgradeStartedAt.DeepCopy(),
		LastAction:       m.LastAction.DeepCopy(),
		HandledRequest:   m.HandledRequest,
	}
}

// FloorSecond is t rounded down to a whole second, in UTC: an observed
// event as status will store it (metav1.Time keeps whole seconds).
func FloorSecond(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// CeilSecond is t rounded up to a whole second, in UTC: a mark that
// nothing before t may look newer than once stored.
func CeilSecond(t time.Time) time.Time {
	f := FloorSecond(t)
	if f.Before(t) {
		f = f.Add(time.Second)
	}
	return f
}

// LatestFetch returns the latest fetch that keep accepts (any, if keep is
// nil); of several at the same time, the last in the log (oldest first).
func LatestFetch(fs []agentapi.Fetch, keep func(agentapi.Fetch) bool) (agentapi.Fetch, bool) {
	var out agentapi.Fetch
	found := false
	for _, f := range fs {
		if (keep == nil || keep(f)) && (!found || !f.At.Before(out.At)) {
			out, found = f, true
		}
	}
	return out, found
}

// EarliestFetch returns the earliest fetch; of several at the same time,
// the first in the log.
func EarliestFetch(fs []agentapi.Fetch) (agentapi.Fetch, bool) {
	var out agentapi.Fetch
	for i, f := range fs {
		if i == 0 || f.At.Before(out.At) {
			out = f
		}
	}
	return out, len(fs) > 0
}
