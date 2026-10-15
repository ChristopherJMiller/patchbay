package cisco79x5

import (
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// registeredResolution is how coarsely LastRegistered advances while the
// phone stays registered, so status is not rewritten on every poll.
const registeredResolution = time.Minute

// fold turns what the sensors see now into the state machine's memory (plan
// §4 T1, "Fold"). Every rule but Unknown starts from it. It never touches
// UpgradeStartedAt, LastAction or HandledRequest: the rules do.
//
//   - FirstSeen: once, at the earliest fetch, or now if the first sight is a
//     live contact.
//   - Address: the live contact's, else the latest fetch's source.
//   - LastConfigFetch: the latest served fetch of SEP<MAC>.cnf.xml with a
//     version, when newer than the remembered one.
//   - Firmware: what the phone last reported, kept only while it can still
//     be true. A download changes the load: when the phone comes back from
//     one (its first config fetch after the last firmware fetch, or after
//     UpgradeStartedAt when the agent's log no longer holds the download),
//     the load it reported before is forgotten. Then the load of a fresh
//     scrape (freshScrape) is taken: its versionID, else its appLoadID
//     (runsLoad accepts both). Last, a live SIP registration rules out an
//     SCCP load (design §8.2: an SCCP phone never sends a SIP REGISTER).
//   - LastRegistered: while there is a live contact, now, when it was never
//     set, is a minute old or more, or is older than the last config fetch
//     (so a fetch while registered never looks newer than the registration:
//     Registering and Unreachable split on that order).
//
// Times. Status keeps metav1.Time in whole seconds, while the agent reports
// times to the nanosecond, so every time the fold or a row writes is whole
// seconds already and a status round trip changes nothing (TestDecideTotal,
// TestLifecycle). Observed events (FirstSeen, LastConfigFetch.At, and
// UpgradeStartedAt in the Upgrading row) round down, as storing them would;
// marks that later fetches are compared with (LastRegistered here, a
// resync's LastAction.At in the rows) round up, so nothing that happened
// before a mark can look newer than it once the mark is stored. The agent's
// times are compared with memory at whole seconds too (floorSecond).
func fold(dev drivers.Device, obs drivers.Observations, now time.Time, p drivers.Profile) drivers.Memory {
	m := copyMemory(dev.Memory)
	d := obs.Device
	if d == nil {
		return m
	}
	live := liveContact(d, now, p)

	if m.FirstSeen == nil {
		if f, ok := earliestFetch(d.Fetches); ok {
			m.FirstSeen = timePtr(floorSecond(f.At))
		} else if live {
			m.FirstSeen = timePtr(floorSecond(now))
		}
	}

	if live && d.Registration.Address != "" {
		m.Address = d.Registration.Address
	} else if f, ok := latestFetch(d.Fetches, nil); ok && f.Source != "" {
		m.Address = f.Source
	}

	cfg := ConfigFileName(dev.MAC)
	if f, ok := latestFetch(d.Fetches, func(f agentapi.Fetch) bool {
		return f.File == cfg && f.Result == agentapi.FetchServed && f.Version != ""
	}); ok {
		at := floorSecond(f.At)
		last := m.LastConfigFetch
		if last == nil || at.After(last.At.Time) || (at.Equal(last.At.Time) && f.Version != last.Version) {
			m.LastConfigFetch = &v1alpha1.ConfigFetch{Version: f.Version, At: metav1.NewTime(at)}
		}
	}

	lastFw := lastFirmwareFetch(d.Fetches)
	loadChanged := lastFw
	if u := m.UpgradeStartedAt; u != nil && u.After(loadChanged) {
		loadChanged = u.Time
	}
	if prev := dev.Memory.LastConfigFetch; !loadChanged.IsZero() && fetchedAfter(m, loadChanged) &&
		(prev == nil || !prev.At.After(loadChanged)) {
		m.Firmware = ""
	}
	if w := freshScrape(d.Web, m, lastFw); w != nil && w.DeviceInformation != nil {
		if v := w.DeviceInformation.VersionID; v != "" {
			m.Firmware = v
		} else if a := w.DeviceInformation.AppLoadID; a != "" {
			m.Firmware = a
		}
	}
	if live && sccpLoad(m.Firmware) {
		m.Firmware = ""
	}

	if live && (m.LastRegistered == nil ||
		now.Sub(m.LastRegistered.Time) >= registeredResolution ||
		fetchedAfter(m, m.LastRegistered.Time)) {
		m.LastRegistered = timePtr(ceilSecond(now))
	}
	return m
}

// freshScrape returns the scrape when what it read can still be true: it
// succeeded (a failed scrape carries an older success's fields, of unknown
// age) and it is no older than the phone's last config fetch or firmware
// download, either of which means the phone restarted or changed its load
// since. Otherwise nil. lastFw is lastFirmwareFetch of the fetch log.
func freshScrape(w *agentapi.WebScrape, m drivers.Memory, lastFw time.Time) *agentapi.WebScrape {
	if w == nil || w.Error != "" {
		return nil
	}
	since := lastFw
	if f := m.LastConfigFetch; f != nil && f.At.After(since) {
		since = f.At.Time
	}
	if floorSecond(w.At).Before(since) {
		return nil
	}
	return w
}

// lastFirmwareFetch is the time of the latest served firmware fetch, in
// whole seconds, or zero.
func lastFirmwareFetch(fs []agentapi.Fetch) time.Time {
	var out time.Time
	for _, f := range fs {
		if f.Result == agentapi.FetchServed && isFirmwareFile(f.File) {
			if at := floorSecond(f.At); at.After(out) {
				out = at
			}
		}
	}
	return out
}

// sccpLoad reports whether a reported load is SCCP firmware: a load
// (SCCP45.9-4-2SR4-3S) or an image (jar45sccp.9-4-2SR4-3.sbn).
func sccpLoad(reported string) bool {
	return strings.Contains(strings.ToLower(reported), "sccp")
}

// liveContact reports whether the device holds a registration that has not
// expired and, when qualify is on, answers it.
func liveContact(d *agentapi.DeviceObservations, now time.Time, p drivers.Profile) bool {
	if d == nil || d.Registration == nil || !d.Registration.ExpiresAt.After(now) {
		return false
	}
	return !qualifyFails(d.Registration, p)
}

// qualifyFails reports whether qualify is on and the contact failed it.
func qualifyFails(r *agentapi.Registration, p drivers.Profile) bool {
	return p.Endpoint.QualifyFrequency > 0 && r.Qualify == agentapi.QualifyUnreachable
}

// fetchedAfter reports whether the remembered config fetch is after t.
func fetchedAfter(m drivers.Memory, t time.Time) bool {
	return m.LastConfigFetch != nil && m.LastConfigFetch.At.After(t)
}

// isFirmwareFile reports whether a file name is a firmware file (a load's
// .loads or .sbn).
func isFirmwareFile(name string) bool {
	return strings.HasSuffix(name, ".sbn") || strings.HasSuffix(name, ".loads")
}

// runsLoad reports whether a reported load (Memory.Firmware) is the given
// load: its name (versionID, SIP45.9-4-2SR4-3S), or one of its .sbn images
// with or without the suffix (appLoadID, jar45sip.9-4-2SR4-3.sbn). An SCCP
// image or load (jar45sccp..., SCCP45...) never matches a SIP load, and
// neither does a .loads file name.
func runsLoad(reported string, load drivers.FirmwareLoad) bool {
	if reported == "" {
		return false
	}
	if reported == load.Name {
		return true
	}
	image := strings.TrimSuffix(reported, ".sbn")
	for _, f := range load.Files {
		if base, ok := strings.CutSuffix(f.Name, ".sbn"); ok && base == image {
			return true
		}
	}
	return false
}

func earliestFetch(fs []agentapi.Fetch) (agentapi.Fetch, bool) {
	var out agentapi.Fetch
	for i, f := range fs {
		if i == 0 || f.At.Before(out.At) {
			out = f
		}
	}
	return out, len(fs) > 0
}

// latestFetch returns the latest fetch that keep accepts (any, if keep is
// nil); of several at the same time, the last in the log (oldest first).
func latestFetch(fs []agentapi.Fetch, keep func(agentapi.Fetch) bool) (agentapi.Fetch, bool) {
	var out agentapi.Fetch
	found := false
	for _, f := range fs {
		if (keep == nil || keep(f)) && (!found || !f.At.Before(out.At)) {
			out, found = f, true
		}
	}
	return out, found
}

// floorSecond is t rounded down to a whole second, in UTC: an observed
// event as status will store it.
func floorSecond(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// ceilSecond is t rounded up to a whole second, in UTC: a mark that nothing
// before t may look newer than once stored.
func ceilSecond(t time.Time) time.Time {
	f := floorSecond(t)
	if f.Before(t) {
		f = f.Add(time.Second)
	}
	return f
}

func timePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

// copyMemory deep-copies memory, so a Decision never aliases the device's.
func copyMemory(m drivers.Memory) drivers.Memory {
	return drivers.Memory{
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
