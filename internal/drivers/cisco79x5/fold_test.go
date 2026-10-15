package cisco79x5_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// fetch is one fetch-log entry from the desk phone's address.
func fetch(file string, result agentapi.FetchResult, at time.Time) agentapi.Fetch {
	return agentapi.Fetch{File: file, Transport: agentapi.TransportTFTP, Result: result, At: at, Source: deskIP}
}

// sepFetch is a served fetch of the desk phone's config at version v.
func sepFetch(at time.Time, v string) agentapi.Fetch {
	f := fetch(deskSEP, agentapi.FetchServed, at)
	f.Version = v
	return f
}

// fwFetch is a served firmware fetch.
func fwFetch(file string, at time.Time) agentapi.Fetch {
	return fetch(file, agentapi.FetchServed, at)
}

// contact is the desk phone's registration, expiring at expires.
func contact(expires time.Time) *agentapi.Registration {
	return &agentapi.Registration{
		Contact:   deskContact,
		Address:   deskIP,
		UserAgent: deskUA,
		CallID:    "c0ffee00-1@192.168.20.101",
		ExpiresAt: expires,
		Qualify:   agentapi.QualifyUnknown,
	}
}

// seen is the desk phone's observations.
func seen(fetches ...agentapi.Fetch) *agentapi.DeviceObservations {
	return &agentapi.DeviceObservations{Device: "phone-desk", MAC: deskMAC, Fetches: fetches}
}

// web is a successful scrape.
func web(versionID, appLoadID string, log ...string) *agentapi.WebScrape {
	return &agentapi.WebScrape{
		At:                   ago(time.Minute),
		Address:              deskIP,
		DeviceInformation:    &agentapi.DeviceInformation{VersionID: versionID, AppLoadID: appLoadID, ModelNumber: "CP-7945G"},
		NetworkConfiguration: &agentapi.NetworkConfiguration{},
		DeviceLog:            log,
	}
}

func deskProfile(t *testing.T) drivers.Profile {
	t.Helper()
	p, ok := cisco79x5.New().Profile("cisco-7945")
	if !ok {
		t.Fatal("no 7945 profile")
	}
	return p
}

func TestFold(t *testing.T) {
	p := deskProfile(t)
	qualifyOn := p
	qualifyOn.Endpoint.QualifyFrequency = time.Minute
	unreachable := contact(t0.Add(5 * time.Minute))
	unreachable.Qualify = agentapi.QualifyUnreachable
	action := &v1alpha1.LastAction{Kind: v1alpha1.ActionResync, Version: vNow, At: *mt(ago(time.Minute)), Attempts: 1}

	for _, c := range []struct {
		name string
		mem  drivers.Memory
		d    *agentapi.DeviceObservations
		p    drivers.Profile
		want drivers.Memory
	}{
		{
			name: "nothing observed: unchanged",
			mem:  drivers.Memory{Firmware: "x", UpgradeStartedAt: mt(ago(time.Hour)), LastAction: action, HandledRequest: "r1"},
			want: drivers.Memory{Firmware: "x", UpgradeStartedAt: mt(ago(time.Hour)), LastAction: action, HandledRequest: "r1"},
		},
		{
			name: "first seen at the earliest fetch",
			d: seen(
				fetch("CTLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, ago(3*time.Minute)),
				fetch(deskSEP+".sgn", agentapi.FetchNotFound, ago(4*time.Minute)),
			),
			want: drivers.Memory{FirstSeen: mt(ago(4 * time.Minute)), Address: deskIP},
		},
		{
			name: "first seen as a live contact: now",
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Minute))},
			want: drivers.Memory{FirstSeen: mt(t0), Address: deskIP, LastRegistered: mt(t0)},
		},
		{
			name: "first seen is kept",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour))},
			d:    seen(fetch("ITLSEPD0C78915E8C8.tlv", agentapi.FetchNotFound, ago(time.Minute))),
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP},
		},
		{
			name: "address: the live contact's before the latest fetch's",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: "192.168.20.9", LastRegistered: mt(ago(30 * time.Second))},
			d: &agentapi.DeviceObservations{
				Fetches:      []agentapi.Fetch{{File: "x", Result: agentapi.FetchNotFound, At: ago(time.Second), Source: "192.168.20.77"}},
				Registration: contact(t0.Add(time.Minute)),
			},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(ago(30 * time.Second))},
		},
		{
			name: "address: the latest fetch's",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: "192.168.20.9"},
			d: &agentapi.DeviceObservations{Fetches: []agentapi.Fetch{
				{File: "b", Result: agentapi.FetchNotFound, At: ago(time.Second), Source: "192.168.20.77"},
				{File: "a", Result: agentapi.FetchNotFound, At: ago(time.Minute), Source: "192.168.20.66"},
			}},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: "192.168.20.77"},
		},
		{
			name: "firmware from versionID when both are set",
			mem:  drivers.Memory{Firmware: "SCCP45.9-4-2SR4-3S"},
			d:    &agentapi.DeviceObservations{Web: web("SIP45.9-4-2SR4-3S", "jar45sip.9-4-2SR4-3.sbn")},
			want: drivers.Memory{Firmware: "SIP45.9-4-2SR4-3S"},
		},
		{
			name: "firmware from appLoadID when only it is set",
			d:    &agentapi.DeviceObservations{Web: web("", "jar45sip.9-4-2SR4-3.sbn")},
			want: drivers.Memory{Firmware: "jar45sip.9-4-2SR4-3.sbn"},
		},
		{
			name: "firmware unchanged by a failed scrape",
			mem:  drivers.Memory{Firmware: "SCCP45.9-4-2SR4-3S"},
			d:    &agentapi.DeviceObservations{Web: &agentapi.WebScrape{At: t0, Address: deskIP, Error: "connection refused"}},
			want: drivers.Memory{Firmware: "SCCP45.9-4-2SR4-3S"},
		},
		{
			name: "firmware unchanged by a scrape with neither field",
			mem:  drivers.Memory{Firmware: "SCCP45.9-4-2SR4-3S"},
			d:    &agentapi.DeviceObservations{Web: web("", "")},
			want: drivers.Memory{Firmware: "SCCP45.9-4-2SR4-3S"},
		},
		{
			name: "last config fetch: the latest served SEP with a version",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastConfigFetch: &v1alpha1.ConfigFetch{Version: vOld, At: *mt(ago(time.Hour))}},
			d: seen(
				sepFetch(ago(5*time.Minute), vNow),
				sepFetch(ago(9*time.Minute), vOld),
				fetch(deskSEP, agentapi.FetchNotFound, ago(time.Minute)),
				fetch(deskSEP, agentapi.FetchError, ago(time.Minute)),
				sepFetch(ago(time.Minute), ""),
				func() agentapi.Fetch {
					f := sepFetch(ago(time.Minute), "x")
					f.File = "SEPD0C78915EBB5.cnf.xml"
					return f
				}(),
				func() agentapi.Fetch { f := sepFetch(ago(time.Minute), "x"); f.File = "dialplan.xml"; return f }(),
			),
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP,
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNow, At: *mt(ago(5 * time.Minute))},
			},
		},
		{
			name: "last config fetch: an older fetch never replaces a newer memory",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNow, At: *mt(ago(time.Minute))}},
			d:    seen(sepFetch(ago(5*time.Minute), vOld), sepFetch(ago(time.Minute), vOld)),
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP,
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNow, At: *mt(ago(time.Minute))},
			},
		},
		{
			name: "last registered: kept at 59 s",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(59 * time.Second))},
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Minute))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(ago(59 * time.Second))},
		},
		{
			name: "last registered: advanced at 60 s",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Minute))},
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Minute))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(t0)},
		},
		{
			name: "last registered: advanced at 10 s by a config fetch after it",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(10 * time.Second))},
			d: &agentapi.DeviceObservations{
				Fetches:      []agentapi.Fetch{sepFetch(ago(5*time.Second), vNow)},
				Registration: contact(t0.Add(time.Minute)),
			},
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(t0),
				LastConfigFetch: &v1alpha1.ConfigFetch{Version: vNow, At: *mt(ago(5 * time.Second))},
			},
		},
		{
			name: "a contact expiring now is not live",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: contact(t0)},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
		},
		{
			name: "a contact expiring in 1 s is live",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Second))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(t0)},
		},
		{
			name: "qualify unreachable is ignored while qualify is off",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: unreachable},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(t0)},
		},
		{
			name: "qualify unreachable is no live contact while qualify is on",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: unreachable},
			p:    qualifyOn,
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
		},
		{
			name: "upgrade, last action and handled request are the rules'",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), UpgradeStartedAt: mt(ago(time.Hour)), LastAction: action, HandledRequest: "r1"},
			d:    seen(fwFetch("apps45.9-4-2SR4-3.sbn", ago(time.Second))),
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP,
				UpgradeStartedAt: mt(ago(time.Hour)), LastAction: action, HandledRequest: "r1",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dev := desk()
			dev.Memory = c.mem
			prof := c.p
			if prof.Model == "" {
				prof = p
			}
			obs := healthy()
			obs.Device = c.d
			got := cisco79x5.Fold(dev, obs, t0, prof)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("fold:\n got %s\nwant %s", fmtMemory(got), fmtMemory(c.want))
			}
			// The fold never changes the device it was given.
			if !reflect.DeepEqual(dev.Memory, c.mem) {
				t.Error("fold changed dev.Memory")
			}
		})
	}
}

func TestRunsLoad(t *testing.T) {
	loads := cisco79x5.New().Firmware()
	load, _ := drivers.LoadByName(loads, cisco79x5.DefaultLoad)
	for reported, want := range map[string]bool{
		"SIP45.9-4-2SR4-3S":        true,
		"jar45sip.9-4-2SR4-3.sbn":  true,
		"jar45sip.9-4-2SR4-3":      true,
		"apps45.9-4-2SR4-3.sbn":    true,
		"SCCP45.9-4-2SR4-3S":       false,
		"jar45sccp.9-4-2SR4-3.sbn": false,
		"jar45sip.9-4-2ES26.sbn":   false,
		"term45.default.loads":     false,
		"SIP45.9-4-2SR4-3S.loads":  false,
		"sip45.9-4-2sr4-3s":        false,
		"":                         false,
	} {
		if got := cisco79x5.RunsLoad(reported, load); got != want {
			t.Errorf("runsLoad(%q) = %v, want %v", reported, got, want)
		}
	}
}
