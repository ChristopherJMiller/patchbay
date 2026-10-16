package compile

import (
	"strconv"
	"strings"
)

// Dialplan contexts.
const (
	// DevicesContext is every device's context: the whole number plan.
	DevicesContext = "patchbay-devices"

	// UnavailableContext plays the SIT tone and "That number is not
	// available." (extension s).
	UnavailableContext = "patchbay-unavailable"
)

// DialTimeout is how long a Dial rings before NOANSWER, in seconds.
const DialTimeout = 30

// sitTone is the special information tone (950, 1400 and 1800 Hz, 330 ms
// each, once), as indications.conf's "info" without its trailing silence.
const sitTone = "!950/330,!1400/330,!1800/330"

// dialTarget is one routed number in the dialplan.
type dialTarget struct {
	number  string
	comment string
	// ring are the device IDs Dial rings at once, in order; empty means
	// the number plays "not available" (unless steps are set).
	ring []string
	// steps, when set, are the extension's own steps (a stream's), in
	// place of ringing.
	steps []string
}

// extensionsConf renders the dialplan. devices, people and streams are
// sorted by number; reserved numbers and unrouted numbers play "not
// available", and a stream goes to the stream bridge (streamTargets).
// Labels, person names and stream display names never appear: only
// numbers, device IDs, stream UUIDs and resource names.
func extensionsConf(plan NumberPlan, devices, people, streams []dialTarget) []byte {
	w := newConf()
	w.section("general")
	w.set("static", "yes")
	w.set("writeprotect", "yes")

	w.section(DevicesContext, "Every device dials from here (pjsip endpoint context).")
	w.b.WriteString("\n")
	w.comment("Built in: the echo test")
	exten(w, plan.EchoTest, "Answer()", "Echo()", "Hangup()")
	w.b.WriteString("\n")
	w.comment("Built in: there is no outside line, so say so, three times")
	emergency := []string{"Answer()", "Playtones(" + sitTone + ")", "Wait(1)", "StopPlaytones()"}
	for i := range 3 {
		if i > 0 {
			emergency = append(emergency, "Wait(1)")
		}
		emergency = append(emergency, "Playback("+SoundsDir+"/"+SoundNoEmergency+")")
	}
	exten(w, plan.Emergency, append(emergency, "Hangup()")...)

	for _, group := range [][]dialTarget{devices, people, streams} {
		for _, t := range group {
			w.b.WriteString("\n")
			w.comment(t.comment)
			if len(t.steps) > 0 {
				exten(w, t.number, t.steps...)
				continue
			}
			if len(t.ring) == 0 {
				exten(w, t.number, unavailable)
				continue
			}
			dial := make([]string, len(t.ring))
			for i, id := range t.ring {
				dial[i] = "PJSIP/" + id
			}
			w.arrow("exten", t.number+",1,Dial("+strings.Join(dial, "&")+","+strconv.Itoa(DialTimeout)+")")
			for _, line := range []string{
				`n,GotoIf($["${DIALSTATUS}" = "BUSY"]?busy)`,
				`n,GotoIf($["${DIALSTATUS}" = "NOANSWER"]?noanswer)`,
				"n," + unavailable,
				"n(busy),Busy(5)",
				"n(noanswer),Hangup()",
			} {
				w.b.WriteString(" same => " + line + "\n")
			}
		}
	}

	w.b.WriteString("\n")
	w.comment("Reserved: not available")
	for _, r := range plan.Reserved {
		exten(w, r, unavailable)
	}
	w.b.WriteString("\n")
	w.comment("Anything else: not available")
	exten(w, "_X!", unavailable)
	exten(w, "_[*#]!", unavailable)

	w.section(UnavailableContext, "The SIT tone, then \"That number is not available.\"")
	exten(w, "s", "Answer()", "Playtones("+sitTone+")", "Wait(1)", "StopPlaytones()",
		"Playback("+SoundsDir+"/"+SoundNotAvailable+")", "Hangup()")
	return w.bytes()
}

// unavailable is the dialplan step that hands a call to UnavailableContext.
const unavailable = "Goto(" + UnavailableContext + ",s,1)"

// exten writes one extension: its first step on the exten line, the rest
// as "same => n" lines. A number-plan pattern becomes an Asterisk pattern.
func exten(w *confWriter, number string, steps ...string) {
	w.arrow("exten", asteriskExten(number)+",1,"+steps[0])
	for _, s := range steps[1:] {
		w.b.WriteString(" same => n," + s + "\n")
	}
}

// asteriskExten writes a number or a number-plan pattern as an Asterisk
// extension: a pattern with X or a trailing "." gets the "_" prefix
// (Asterisk's "." is one or more of anything, a superset of the plan's).
// Extensions that already start with "_" (the catch-alls) are kept.
func asteriskExten(n string) string {
	if strings.HasPrefix(n, "_") || n == "s" || !strings.ContainsAny(n, "X.") {
		return n
	}
	return "_" + n
}
