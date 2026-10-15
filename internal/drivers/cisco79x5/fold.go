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
//   - Firmware: the scrape's versionID, else its appLoadID (runsLoad accepts
//     both), else unchanged.
//   - LastConfigFetch: the latest served fetch of SEP<MAC>.cnf.xml with a
//     version, when newer than the remembered one.
//   - LastRegistered: while there is a live contact, now, when it was never
//     set, is a minute old or more, or is older than the last config fetch
//     (so a fetch while registered never looks newer than the registration:
//     Registering and Unreachable split on that order).
func fold(dev drivers.Device, obs drivers.Observations, now time.Time, p drivers.Profile) drivers.Memory {
	m := copyMemory(dev.Memory)
	d := obs.Device
	if d == nil {
		return m
	}
	live := liveContact(d, now, p)

	if m.FirstSeen == nil {
		if f, ok := earliestFetch(d.Fetches); ok {
			m.FirstSeen = timePtr(f.At)
		} else if live {
			m.FirstSeen = timePtr(now)
		}
	}

	if live && d.Registration.Address != "" {
		m.Address = d.Registration.Address
	} else if f, ok := latestFetch(d.Fetches); ok && f.Source != "" {
		m.Address = f.Source
	}

	if w := d.Web; w != nil && w.DeviceInformation != nil {
		if v := w.DeviceInformation.VersionID; v != "" {
			m.Firmware = v
		} else if a := w.DeviceInformation.AppLoadID; a != "" {
			m.Firmware = a
		}
	}

	cfg := ConfigFileName(dev.MAC)
	for _, f := range d.Fetches {
		if f.File == cfg && f.Result == agentapi.FetchServed && f.Version != "" &&
			(m.LastConfigFetch == nil || f.At.After(m.LastConfigFetch.At.Time)) {
			m.LastConfigFetch = &v1alpha1.ConfigFetch{Version: f.Version, At: metav1.NewTime(f.At)}
		}
	}

	if live && (m.LastRegistered == nil ||
		now.Sub(m.LastRegistered.Time) >= registeredResolution ||
		fetchedAfter(m, m.LastRegistered.Time)) {
		m.LastRegistered = timePtr(now)
	}
	return m
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

func latestFetch(fs []agentapi.Fetch) (agentapi.Fetch, bool) {
	var out agentapi.Fetch
	for i, f := range fs {
		if i == 0 || !f.At.Before(out.At) {
			out = f
		}
	}
	return out, len(fs) > 0
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
