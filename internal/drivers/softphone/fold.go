package softphone

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// fold turns what the sensors see now into the softphone's next memory
// (sensor → memory), before any row of the rule table changes it further.
// Every row except Unknown starts from it. It is cisco79x5's fold without
// firmware (docs/plans/release-a.md T1, T2):
//
//   - FirstSeen: if nil and the device has a fetch or a live contact, the
//     earliest fetch's time, else now;
//   - Address: the live contact's address, else the latest fetch's source
//     unless the softphone registered after that fetch, else unchanged: the
//     address of whichever it did last (a softphone's fetches are rare, so
//     once its contact lapses the fetch is usually older than the
//     registration whose address is already remembered);
//   - LastConfigFetch: the latest served fetch of ProvisioningFile(ID) with
//     a version, if newer than the remembered one. While LastConfigFetch is
//     nil and LastAction is a rotateToken (a reissue cleared it, S3), only a
//     fetch after that rotation counts: the fetch from before the reissue is
//     still in the agent's ring buffer and must not undo it;
//   - LastRegistered: with a live contact, now when nil, when at least a
//     minute old (coarse, so status is not rewritten every poll), or when
//     the remembered config fetch is after it (so a fetch while registered
//     never looks newer than the registration: S6 and S8 split on that
//     order); else unchanged;
//   - Firmware and UpgradeStartedAt are not a softphone's; LastAction and
//     HandledRequest are the rows'.
//
// Times. Status keeps metav1.Time, which is stored in whole seconds, while
// the agent reports times to the nanosecond. Every time the fold or a row
// writes is therefore whole seconds already, so a status round trip changes
// nothing: observed events (FirstSeen, LastConfigFetch.At) are rounded down,
// as storing them would; marks that later fetches are compared against
// (LastRegistered here, a rotation's LastAction.At in the rows) are rounded
// up, so nothing that happened before the mark can look newer than it once
// the mark is stored. A rotation stored rounded down could make the fetch
// that spent the token count again a poll later and rotate a second time,
// or make the pre-reissue fetch count after a reissue.
func fold(dev drivers.Device, obs drivers.Observations, now time.Time, p drivers.Profile) drivers.Memory {
	m := dev.Memory.DeepCopy()
	d := obs.Device
	if d == nil {
		return m
	}
	reg := liveRegistration(d, now, p)

	if m.FirstSeen == nil && (len(d.Fetches) > 0 || reg != nil) {
		first := floorSecond(now)
		if f, ok := drivers.EarliestFetch(d.Fetches); ok {
			first = floorSecond(f.At)
		}
		m.FirstSeen = &first
	}

	if reg != nil && reg.Address != "" {
		m.Address = reg.Address
	} else if f, ok := drivers.LatestFetch(d.Fetches, func(f agentapi.Fetch) bool { return f.Source != "" }); ok &&
		(m.LastRegistered == nil || f.At.After(m.LastRegistered.Time)) {
		m.Address = f.Source
	}

	if f, ok := drivers.LatestFetch(d.Fetches, func(f agentapi.Fetch) bool { return isConfigFetch(dev.ID, f, m) }); ok {
		at := floorSecond(f.At)
		last := m.LastConfigFetch
		if last == nil || at.After(last.At.Time) || (at.Equal(&last.At) && f.Version != last.Version) {
			m.LastConfigFetch = &v1alpha1.ConfigFetch{Version: f.Version, At: at}
		}
	}

	if reg != nil {
		lr := m.LastRegistered
		if lr == nil || now.Sub(lr.Time) >= lastRegisteredStep || fetchedAfter(m, lr.Time) {
			at := ceilSecond(now)
			m.LastRegistered = &at
		}
	}
	return m
}

// isConfigFetch reports whether f is a fetch that can become
// LastConfigFetch: a served fetch of the softphone's provisioning document
// with a version, and after the reissue if a reissue cleared the memory.
func isConfigFetch(id string, f agentapi.Fetch, m drivers.Memory) bool {
	if f.File != ProvisioningFile(id) || f.Result != agentapi.FetchServed || f.Version == "" {
		return false
	}
	if m.LastConfigFetch == nil {
		if la := m.LastAction; la != nil && la.Kind == v1alpha1.ActionRotateToken {
			return f.At.After(la.At.Time)
		}
	}
	return true
}

// liveRegistration returns the device's registration when it is a live
// contact: not expired (drivers.RegistrationLive), and not failing qualify
// when the profile qualifies at all. Otherwise nil.
func liveRegistration(d *agentapi.DeviceObservations, now time.Time, p drivers.Profile) *agentapi.Registration {
	if d == nil || d.Registration == nil {
		return nil
	}
	r := d.Registration
	if !drivers.RegistrationLive(r.ExpiresAt, now) {
		return nil
	}
	if p.Endpoint.QualifyFrequency > 0 && r.Qualify == agentapi.QualifyUnreachable {
		return nil
	}
	return r
}

// fetchedAfter reports whether the remembered config fetch is after t.
func fetchedAfter(m drivers.Memory, t time.Time) bool {
	return m.LastConfigFetch != nil && m.LastConfigFetch.At.After(t)
}

// floorSecond and ceilSecond are drivers.FloorSecond and CeilSecond as the
// metav1.Time status stores.
func floorSecond(t time.Time) metav1.Time { return metav1.NewTime(drivers.FloorSecond(t)) }

func ceilSecond(t time.Time) metav1.Time { return metav1.NewTime(drivers.CeilSecond(t)) }
