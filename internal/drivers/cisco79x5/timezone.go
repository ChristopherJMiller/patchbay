package cisco79x5

// windowsTimeZones maps IANA zone names to the Windows-style names the 79x5
// takes in dateTimeSetting/timeZone. Every value is in the time-zone list of
// https://usecallmanager.nz/sepmac-cnf-xml.html (checked 2026-10-15); a name
// added later must be too (TestWindowsTimeZone checks against that list).
//
// The canonical zones come first; then their aliases from tzdata's
// "backward" file, which map like the zone they link to.
var windowsTimeZones = map[string]string{
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

	// Aliases.
	"US/Pacific":       "Pacific Standard/Daylight Time",
	"US/Mountain":      "Mountain Standard/Daylight Time",
	"America/Shiprock": "Mountain Standard/Daylight Time",
	"Navajo":           "Mountain Standard/Daylight Time",
	"US/Arizona":       "US Mountain Standard Time",
	"US/Central":       "Central Standard/Daylight Time",
	"US/Eastern":       "Eastern Standard/Daylight Time",
	"US/Alaska":        "Alaskan Standard/Daylight Time",
	"US/Hawaii":        "Hawaiian Standard Time",
	"Pacific/Johnston": "Hawaiian Standard Time",
	"GB":               "GMT Standard/Daylight Time",
	"GB-Eire":          "GMT Standard/Daylight Time",
	"Europe/Belfast":   "GMT Standard/Daylight Time",
	"Etc/UCT":          "Greenwich Standard Time",
	"UCT":              "Greenwich Standard Time",
	"Etc/Universal":    "Greenwich Standard Time",
	"Universal":        "Greenwich Standard Time",
	"Etc/Zulu":         "Greenwich Standard Time",
	"Zulu":             "Greenwich Standard Time",
	"Etc/GMT":          "Greenwich Standard Time",
	"GMT":              "Greenwich Standard Time",
	"Etc/GMT+0":        "Greenwich Standard Time",
	"Etc/GMT-0":        "Greenwich Standard Time",
	"Etc/GMT0":         "Greenwich Standard Time",
	"GMT+0":            "Greenwich Standard Time",
	"GMT-0":            "Greenwich Standard Time",
	"GMT0":             "Greenwich Standard Time",
	"Etc/Greenwich":    "Greenwich Standard Time",
	"Greenwich":        "Greenwich Standard Time",
}

// WindowsTimeZone maps an IANA zone name (settings' timezone) to the name the
// 79x5 wants, e.g. America/Los_Angeles to "Pacific Standard/Daylight Time".
// ok is false for a zone the driver cannot map; Render then fails, because
// the setting is install-wide.
func WindowsTimeZone(iana string) (string, bool) {
	name, ok := windowsTimeZones[iana]
	return name, ok
}
