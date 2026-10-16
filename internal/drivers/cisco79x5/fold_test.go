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

	// scrapeAt is a successful scrape at `at` reporting versionID.
	scrapeAt := func(at time.Time, versionID string) *agentapi.WebScrape {
		w := web(versionID, "")
		w.At = at
		return w
	}
	// upgraded is a phone that fetched its config 12 minutes ago, downloaded
	// the SIP load 10 to 6 minutes ago and fetched its config again 20 s
	// ago, with w as its latest scrape and a live contact.
	upgraded := func(w *agentapi.WebScrape) *agentapi.DeviceObservations {
		return &agentapi.DeviceObservations{
			Fetches: []agentapi.Fetch{
				sepFetch(ago(12*time.Minute), vNow), fwFetch("SIP45.9-4-2SR4-3S.loads", ago(10*time.Minute)),
				fwFetch("apps45.9-4-2SR4-3.sbn", ago(6*time.Minute)), sepFetch(ago(20*time.Second), vNow),
			},
			Registration: contact(t0.Add(5 * time.Minute)),
			Web:          w,
		}
	}
	beforeUpgrade := drivers.Memory{
		FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sccp45,
		LastConfigFetch: cfgFetch(vNow, ago(12*time.Minute)), UpgradeStartedAt: mt(ago(10 * time.Minute)),
	}
	afterUpgrade := func(firmware string) drivers.Memory {
		return drivers.Memory{
			FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: firmware,
			LastConfigFetch: cfgFetch(vNow, ago(20*time.Second)), LastRegistered: mt(t0),
			UpgradeStartedAt: mt(ago(10 * time.Minute)),
		}
	}
	sub := func(d time.Duration) time.Time { return ago(d).Add(300 * time.Millisecond) }

	for _, c := range []struct {
		name string
		mem  drivers.Memory
		d    *agentapi.DeviceObservations
		p    drivers.Profile
		now  time.Time // zero: t0
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
		// What a real CP-7945G on SIP 9.4(2)SR4-3 reports (2026-10-15):
		// versionID wrapped in asterisks, appLoadID its jar image.
		{
			name: "firmware: the asterisks around versionID are not the load's",
			mem:  drivers.Memory{Firmware: sccp45},
			d:    &agentapi.DeviceObservations{Web: web("*SIP45.9-4-2SR4-3S*", jarSIP)},
			want: drivers.Memory{Firmware: sip45},
		},
		{
			name: "firmware: nor is white space",
			d:    &agentapi.DeviceObservations{Web: web(" *SIP45.9-4-2SR4-3S*\t", "")},
			want: drivers.Memory{Firmware: sip45},
		},
		{
			name: "firmware: another load in asterisks is that load",
			d:    &agentapi.DeviceObservations{Web: web("*SIP45.9-4-2ES26S*", "jar45sip.9-4-2ES26.sbn")},
			want: drivers.Memory{Firmware: "SIP45.9-4-2ES26S"},
		},
		{
			name: "firmware: appLoadID naming the target load counts when versionID does not",
			d:    &agentapi.DeviceObservations{Web: web("*9.4(2)SR4.3*", jarSIP)},
			want: drivers.Memory{Firmware: jarSIP},
		},
		{
			name: "firmware: appLoadID in asterisks too",
			d:    &agentapi.DeviceObservations{Web: web("", "*jar45sip.9-4-2SR4-3.sbn*")},
			want: drivers.Memory{Firmware: jarSIP},
		},
		{
			name: "firmware: neither naming the target load: versionID",
			d:    &agentapi.DeviceObservations{Web: web("*SCCP45.9-4-2SR4-3S*", "jar45sccp.9-4-2SR4-3.sbn")},
			want: drivers.Memory{Firmware: sccp45},
		},
		{
			name: "firmware: a remembered load in asterisks is normalised",
			mem:  drivers.Memory{Firmware: "*SIP45.9-4-2SR4-3S*"},
			d:    &agentapi.DeviceObservations{},
			want: drivers.Memory{Firmware: sip45},
		},
		{
			name: "firmware: a live SIP registration rules out an SCCP load in asterisks",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(30 * time.Second))},
			d: &agentapi.DeviceObservations{
				Registration: contact(t0.Add(time.Minute)),
				Web:          web("*SCCP45.9-4-2SR4-3S*", ""),
			},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(ago(30 * time.Second))},
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
			d:    seen(sepFetch(ago(5*time.Minute), vOld), sepFetch(ago(time.Minute+time.Second), vOld)),
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
			// The phone refreshes 5 s before expiry, and the controller sees the
			// refresh up to two polls later: a contact just past its expiry is
			// still live (drivers.RegistrationGrace).
			name: "a contact that expired 14 s ago is live: its refresh is not seen yet",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: contact(ago(14 * time.Second))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(t0)},
		},
		{
			name: "a contact that expired the grace ago is not live",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: contact(ago(drivers.RegistrationGrace))},
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

		// Which scrape counts: a successful one, no older than the phone's
		// last config fetch or firmware download (either means it restarted
		// or changed its load since).
		{
			name: "firmware: a failed scrape's fields (an older success's) are not read",
			mem:  drivers.Memory{Firmware: sccp45},
			d: &agentapi.DeviceObservations{Web: func() *agentapi.WebScrape {
				w := web(sip45, jarSIP)
				w.Error = "context deadline exceeded"
				return w
			}()},
			want: drivers.Memory{Firmware: sccp45},
		},
		{
			name: "firmware: a scrape older than the last config fetch is not read",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45},
			d: &agentapi.DeviceObservations{
				Fetches: []agentapi.Fetch{sepFetch(ago(2*time.Minute), vNow)},
				Web:     scrapeAt(ago(3*time.Minute), sip45),
			},
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sccp45,
				LastConfigFetch: cfgFetch(vNow, ago(2*time.Minute)),
			},
		},
		{
			name: "firmware: a scrape at the last config fetch's second is read",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45},
			d: &agentapi.DeviceObservations{
				Fetches: []agentapi.Fetch{sepFetch(sub(2*time.Minute), vNow)},
				Web:     scrapeAt(ago(2*time.Minute), sip45),
			},
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sip45,
				LastConfigFetch: cfgFetch(vNow, ago(2*time.Minute)),
			},
		},
		{
			name: "firmware: a scrape older than the last firmware download is not read",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45},
			d: &agentapi.DeviceObservations{
				Fetches: []agentapi.Fetch{fwFetch("apps45.9-4-2SR4-3.sbn", ago(time.Minute))},
				Web:     scrapeAt(ago(2*time.Minute), sip45),
			},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, Firmware: sccp45},
		},
		{
			name: "firmware: back from a download, the load from before it is forgotten",
			mem:  beforeUpgrade,
			d:    upgraded(nil),
			want: afterUpgrade(""),
		},
		{
			name: "firmware: back from a download, a failed scrape is still not read",
			mem:  beforeUpgrade,
			d: upgraded(func() *agentapi.WebScrape {
				w := scrapeAt(ago(5*time.Second), sccp45)
				w.Error = "timeout"
				return w
			}()),
			want: afterUpgrade(""),
		},
		{
			name: "firmware: back from a download, a scrape before its reboot is not read",
			mem:  beforeUpgrade,
			d:    upgraded(scrapeAt(ago(5*time.Minute), sccp45)),
			want: afterUpgrade(""),
		},
		{
			name: "firmware: back from a download, a scrape since is read",
			mem:  beforeUpgrade,
			d:    upgraded(scrapeAt(ago(5*time.Second), sip45)),
			want: afterUpgrade(sip45),
		},
		{
			name: "firmware: the agent's log lost the download; UpgradeStartedAt marks it",
			mem:  beforeUpgrade,
			d: &agentapi.DeviceObservations{
				Fetches:      []agentapi.Fetch{sepFetch(ago(20*time.Second), vNow)},
				Registration: contact(t0.Add(5 * time.Minute)),
			},
			want: afterUpgrade(""),
		},
		{
			name: "firmware: a load remembered after the reboot is kept",
			mem: func() drivers.Memory {
				m := afterUpgrade(sip45)
				m.LastRegistered = mt(ago(10 * time.Second))
				return m
			}(),
			d: upgraded(nil),
			want: func() drivers.Memory {
				m := afterUpgrade(sip45)
				m.LastRegistered = mt(ago(10 * time.Second))
				return m
			}(),
		},
		{
			name: "firmware: a live SIP registration rules out an SCCP load",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45, LastRegistered: mt(ago(30 * time.Second))},
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Minute))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(ago(30 * time.Second))},
		},
		{
			name: "firmware: and an SCCP image",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastRegistered: mt(ago(30 * time.Second))},
			d: &agentapi.DeviceObservations{
				Registration: contact(t0.Add(time.Minute)),
				Web:          web("", "jar45sccp.9-4-2SR4-3.sbn"),
			},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastRegistered: mt(ago(30 * time.Second))},
		},
		{
			name: "firmware: without a contact an SCCP load stands",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45, LastRegistered: mt(ago(time.Hour))},
			d:    &agentapi.DeviceObservations{Registration: contact(ago(drivers.RegistrationGrace))},
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Firmware: sccp45, LastRegistered: mt(ago(time.Hour))},
		},

		// Status stores whole seconds; the agent reports nanoseconds. Events
		// round down (as storing them would), marks round up.
		{
			name: "times: the first fetch rounds down",
			d:    seen(fetch(deskSEP+".sgn", agentapi.FetchNotFound, sub(4*time.Minute))),
			want: drivers.Memory{FirstSeen: mt(ago(4 * time.Minute)), Address: deskIP},
		},
		{
			name: "times: first seen as a contact between seconds rounds down",
			d:    &agentapi.DeviceObservations{Registration: contact(t0.Add(time.Minute))},
			now:  t0.Add(500 * time.Millisecond),
			want: drivers.Memory{FirstSeen: mt(t0), Address: deskIP, LastRegistered: mt(t0.Add(time.Second))},
		},
		{
			name: "times: a config fetch rounds down",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour))},
			d:    seen(sepFetch(sub(5*time.Minute), vNow)),
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(5*time.Minute))},
		},
		{
			name: "times: the stored config fetch is not news",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastConfigFetch: cfgFetch(vNow, ago(5*time.Minute))},
			d:    seen(sepFetch(sub(5*time.Minute), vNow)),
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(5*time.Minute))},
		},
		{
			name: "times: another version in the stored second is news",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour)), LastConfigFetch: cfgFetch(vOld, ago(5*time.Minute))},
			d:    seen(sepFetch(sub(5*time.Minute), vNow)),
			want: drivers.Memory{FirstSeen: mt(ago(time.Hour)), Address: deskIP, LastConfigFetch: cfgFetch(vNow, ago(5*time.Minute))},
		},
		{
			name: "times: last registered rounds up, after a config fetch in its second",
			mem:  drivers.Memory{FirstSeen: mt(ago(time.Hour))},
			d: &agentapi.DeviceObservations{
				Fetches:      []agentapi.Fetch{sepFetch(t0.Add(200*time.Millisecond), vNow)},
				Registration: contact(t0.Add(time.Minute)),
			},
			now: t0.Add(500 * time.Millisecond),
			want: drivers.Memory{
				FirstSeen: mt(ago(time.Hour)), Address: deskIP,
				LastConfigFetch: cfgFetch(vNow, t0), LastRegistered: mt(t0.Add(time.Second)),
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
			now := c.now
			if now.IsZero() {
				now = t0
			}
			got := cisco79x5.Fold(dev, obs, now, prof, defaultLoad(t))
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

// defaultLoad is the 7945's default load (the desk phone's target).
func defaultLoad(t *testing.T) drivers.FirmwareLoad {
	t.Helper()
	load, ok := drivers.LoadByName(cisco79x5.New().Firmware(), cisco79x5.DefaultLoad)
	if !ok {
		t.Fatal("no default load")
	}
	return load
}

func TestRunsLoad(t *testing.T) {
	load := defaultLoad(t)
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
		// A real 7945's versionID (2026-10-15), and the like.
		"*SIP45.9-4-2SR4-3S*":        true,
		" *SIP45.9-4-2SR4-3S* ":      true,
		"*SIP45.9-4-2SR4-3S":         true,
		"**SIP45.9-4-2SR4-3S**":      true,
		"*jar45sip.9-4-2SR4-3.sbn*":  true,
		"*SCCP45.9-4-2SR4-3S*":       false,
		"*jar45sccp.9-4-2SR4-3.sbn*": false,
		"*SIP45.9-4-2ES26S*":         false,
		"*SIP45.9-4-2SR4-3S.loads*":  false,
		"SIP45.*9-4-2SR4-3S":         false,
		"*":                          false,
		"**":                         false,
		" ":                          false,
	} {
		if got := cisco79x5.RunsLoad(reported, load); got != want {
			t.Errorf("runsLoad(%q) = %v, want %v", reported, got, want)
		}
	}
}
