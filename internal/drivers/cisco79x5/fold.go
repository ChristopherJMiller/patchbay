package cisco79x5

import (
	"strings"
	"time"
	"unicode"

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
//   - Firmware: what the phone last reported, normalised (loadID: a real
//     7945 wraps its versionID in asterisks), kept only while it can still
//     be true. A download changes the load: when the phone comes back from
//     one (its first config fetch after the last firmware fetch, or after
//     UpgradeStartedAt when the agent's log no longer holds the download),
//     the load it reported before is forgotten. Then the load of a fresh
//     scrape (freshScrape) is taken (reportedLoad: whichever of versionID
//     and appLoadID names target, the device's target load, else
//     versionID, else appLoadID). Last, a live SIP registration rules out
//     an SCCP load (design §8.2: an SCCP phone never sends a SIP REGISTER).
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
// times are compared with memory at whole seconds too (drivers.FloorSecond).
func fold(dev drivers.Device, obs drivers.Observations, now time.Time, p drivers.Profile, target drivers.FirmwareLoad) drivers.Memory {
	m := dev.Memory.DeepCopy()
	m.Firmware = loadID(m.Firmware)
	d := obs.Device
	if d == nil {
		return m
	}
	live := liveContact(d, now, p)

	if m.FirstSeen == nil {
		if f, ok := drivers.EarliestFetch(d.Fetches); ok {
			m.FirstSeen = timePtr(drivers.FloorSecond(f.At))
		} else if live {
			m.FirstSeen = timePtr(drivers.FloorSecond(now))
		}
	}

	if live && d.Registration.Address != "" {
		m.Address = d.Registration.Address
	} else if f, ok := drivers.LatestFetch(d.Fetches, nil); ok && f.Source != "" {
		m.Address = f.Source
	}

	cfg := ConfigFileName(dev.MAC)
	if f, ok := drivers.LatestFetch(d.Fetches, func(f agentapi.Fetch) bool {
		return f.File == cfg && f.Result == agentapi.FetchServed && f.Version != ""
	}); ok {
		at := drivers.FloorSecond(f.At)
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
		if r := reportedLoad(w.DeviceInformation, target); r != "" {
			m.Firmware = r
		}
	}
	if live && sccpLoad(m.Firmware) {
		m.Firmware = ""
	}

	if live && (m.LastRegistered == nil ||
		now.Sub(m.LastRegistered.Time) >= registeredResolution ||
		fetchedAfter(m, m.LastRegistered.Time)) {
		m.LastRegistered = timePtr(drivers.CeilSecond(now))
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
	if drivers.FloorSecond(w.At).Before(since) {
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
			if at := drivers.FloorSecond(f.At); at.After(out) {
				out = at
			}
		}
	}
	return out
}

// sccpLoad reports whether a reported load is SCCP firmware: a load
// (SCCP45.9-4-2SR4-3S, *SCCP45.9-4-2SR4-3S* as a 7945 reports it) or an
// image (jar45sccp.9-4-2SR4-3.sbn).
func sccpLoad(reported string) bool {
	return strings.Contains(strings.ToLower(loadID(reported)), "sccp")
}

// loadID is a load or image name as the phone reports it, normalised: a
// real CP-7945G on SIP 9.4(2)SR4-3 wraps the running load in asterisks in
// /DeviceInformationX's versionID (*SIP45.9-4-2SR4-3S*, observed
// 2026-10-15), so every asterisk and white space around it is trimmed. A
// load name never starts or ends with either.
func loadID(reported string) string {
	return strings.TrimFunc(reported, func(r rune) bool { return r == '*' || unicode.IsSpace(r) })
}

// reportedLoad is the load a scrape's /DeviceInformationX reports,
// normalised (loadID), for the device whose target load is target: its
// versionID when that names target (runsLoad), else its appLoadID when
// that does, so either matching counts; else its versionID, else its
// appLoadID; "" when it reports neither.
func reportedLoad(di *agentapi.DeviceInformation, target drivers.FirmwareLoad) string {
	v, a := loadID(di.VersionID), loadID(di.AppLoadID)
	switch {
	case runsLoad(v, target):
		return v
	case runsLoad(a, target):
		return a
	case v != "":
		return v
	}
	return a
}

// liveContact reports whether the device holds a registration that has not
// expired (drivers.RegistrationLive: give or take the grace for a refresh
// not seen yet) and, when qualify is on, answers it.
func liveContact(d *agentapi.DeviceObservations, now time.Time, p drivers.Profile) bool {
	if d == nil || d.Registration == nil || !drivers.RegistrationLive(d.Registration.ExpiresAt, now) {
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

// runsLoad reports whether a reported load (Memory.Firmware), normalised
// (loadID), is the given load: its name (versionID, SIP45.9-4-2SR4-3S, or
// *SIP45.9-4-2SR4-3S* as a real 7945 reports it), or one of its .sbn
// images with or without the suffix (appLoadID, jar45sip.9-4-2SR4-3.sbn).
// An SCCP image or load (jar45sccp..., SCCP45...) never matches a SIP
// load, and neither does a .loads file name.
func runsLoad(reported string, load drivers.FirmwareLoad) bool {
	reported = loadID(reported)
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

func timePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}
