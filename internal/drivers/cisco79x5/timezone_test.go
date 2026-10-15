package cisco79x5_test

import (
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // the zone database from the binary, not the host

	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// usecallmanagerZones is the time-zone list of
// https://usecallmanager.nz/sepmac-cnf-xml.html (timeZone), copied
// 2026-10-15. Every name WindowsTimeZone returns must be in it.
var usecallmanagerZones = []string{
	"Dateline Standard Time", "Samoa Standard Time", "Hawaiian Standard Time",
	"Alaskan Standard/Daylight Time", "Pacific Standard/Daylight Time", "Mountain Standard/Daylight Time",
	"US Mountain Standard Time", "Central Standard/Daylight Time", "Mexico Standard/Daylight Time",
	"Canada Central Standard Time", "SA Pacific Standard Time", "Eastern Standard/Daylight Time",
	"US Eastern Standard Time", "Atlantic Standard/Daylight Time", "SA Western Standard Time",
	"Pacific SA Standard Time", "Newfoundland Standard/Daylight Time", "E. South America Standard/Daylight Time",
	"SA Eastern Standard Time", "Pacific SA Daylight Time", "Mid-Atlantic Standard/Daylight Time",
	"Azores Standard/Daylight Time", "GMT Standard/Daylight Time", "Greenwich Standard Time",
	"W. Europe Standard/Daylight Time", "GTB Standard/Daylight Time", "Egypt Standard/Daylight Time",
	"E. Europe Standard/Daylight Time", "Romance Standard/Daylight Time", "Central Europe Standard/Daylight Time",
	"South Africa Standard Time", "Jerusalem Standard/Daylight Time", "Saudi Arabia Standard Time",
	"Russian Standard/Daylight Time", "Iran Standard/Daylight Time", "Caucasus Standard/Daylight Time",
	"Arabian Standard Time", "Afghanistan Standard Time", "West Asia Standard Time",
	"Ekaterinburg Standard Time", "India Standard Time", "Central Asia Standard Time",
	"SE Asia Standard Time", "China Standard/Daylight Time", "Taipei Standard Time",
	"Tokyo Standard Time", "Cen. Australia Standard/Daylight Time", "AUS Central Standard Time",
	"E. Australia Standard Time", "AUS Eastern Standard/Daylight Time", "West Pacific Standard Time",
	"Tasmania Standard/Daylight Time", "Central Pacific Standard Time", "Fiji Standard Time",
	"New Zealand Standard/Daylight Time",
}

func TestWindowsTimeZone(t *testing.T) {
	// Plan §4 T1's table.
	canonical := map[string]string{
		"America/Los_Angeles": "Pacific Standard/Daylight Time",
		"America/Denver":      "Mountain Standard/Daylight Time",
		"America/Phoenix":     "US Mountain Standard Time",
		"America/Chicago":     "Central Standard/Daylight Time",
		"America/New_York":    "Eastern Standard/Daylight Time",
		"America/Anchorage":   "Alaskan Standard/Daylight Time",
		"Pacific/Honolulu":    "Hawaiian Standard Time",
		"Europe/London":       "GMT Standard/Daylight Time",
		"UTC":                 "Greenwich Standard Time",
		"Etc/UTC":             "Greenwich Standard Time",
	}
	for iana, want := range canonical {
		if got, ok := cisco79x5.WindowsTimeZone(iana); !ok || got != want {
			t.Errorf("WindowsTimeZone(%q) = %q, %v; want %q", iana, got, ok, want)
		}
	}

	// Aliases (tzdata "backward" links) map like their zone: checked by
	// offsets in winter and summer, from the zone database itself.
	aliases := map[string]string{
		"US/Pacific": "America/Los_Angeles", "US/Mountain": "America/Denver",
		"America/Shiprock": "America/Denver", "Navajo": "America/Denver",
		"US/Arizona": "America/Phoenix", "US/Central": "America/Chicago",
		"US/Eastern": "America/New_York", "US/Alaska": "America/Anchorage",
		"US/Hawaii": "Pacific/Honolulu", "Pacific/Johnston": "Pacific/Honolulu",
		"GB": "Europe/London", "GB-Eire": "Europe/London", "Europe/Belfast": "Europe/London",
		"Etc/UCT": "Etc/UTC", "UCT": "Etc/UTC", "Etc/Universal": "Etc/UTC", "Universal": "Etc/UTC",
		"Etc/Zulu": "Etc/UTC", "Zulu": "Etc/UTC", "Etc/GMT": "Etc/UTC", "GMT": "Etc/UTC",
		"Etc/GMT+0": "Etc/UTC", "Etc/GMT-0": "Etc/UTC", "Etc/GMT0": "Etc/UTC", "GMT+0": "Etc/UTC",
		"GMT-0": "Etc/UTC", "GMT0": "Etc/UTC", "Etc/Greenwich": "Etc/UTC", "Greenwich": "Etc/UTC",
	}
	for alias, zone := range aliases {
		got, ok := cisco79x5.WindowsTimeZone(alias)
		if want := canonical[zone]; !ok || got != want {
			t.Errorf("WindowsTimeZone(%q) = %q, %v; want %q like %s", alias, got, ok, want, zone)
		}
		la, err := time.LoadLocation(alias)
		if err != nil {
			t.Errorf("%s: %v", alias, err)
			continue
		}
		lz, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{
			time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		} {
			_, oa := at.In(la).Zone()
			_, oz := at.In(lz).Zone()
			if oa != oz {
				t.Errorf("%s is not an alias of %s: offset %d vs %d on %s", alias, zone, oa, oz, at.Format(time.DateOnly))
			}
		}
	}

	// Every name the driver writes is one the phone knows (aliases map to
	// the canonical zones' names, checked above).
	for iana := range canonical {
		name, _ := cisco79x5.WindowsTimeZone(iana)
		if !slices.Contains(usecallmanagerZones, name) {
			t.Errorf("%q (for %s) is not in usecallmanager's time-zone list", name, iana)
		}
	}

	for _, unknown := range []string{"", "Europe/Berlin", "america/los_angeles", "PST8PDT", "Pacific Standard/Daylight Time"} {
		if got, ok := cisco79x5.WindowsTimeZone(unknown); ok || got != "" {
			t.Errorf("WindowsTimeZone(%q) = %q, %v; want \"\", false", unknown, got, ok)
		}
	}
}
