package observe_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/ami/amitest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/agent/observe"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

var (
	desk   = drivers.DeviceID(drivers.KindPhone, "desk")
	front  = drivers.DeviceID(drivers.KindPhone, "front")
	laptop = drivers.DeviceID(drivers.KindSoftphone, "chris-laptop")
	t0     = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
)

const (
	deskMAC   = "D0C78915E8C8"
	frontMAC  = "D0C78915E8C9"
	deskIP    = "192.168.20.5"
	frontIP   = "192.168.20.6"
	laptopIP  = "192.168.0.50"
	strangeIP = "192.168.20.99"
)

// install is the house plus a second phone.
func install() *intaketest.Install {
	in := intaketest.House()
	in.AddPhone("front", frontMAC, "101", "Front")
	return in
}

type fixture struct {
	t      *testing.T
	store  *observe.Store
	ami    *amitest.Fake
	m      *metrics.Metrics
	idx    compile.Index // applied
	older  compile.Index // another kept version
	status apply.Status
	now    time.Time
	fw     api.FirmwareStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	in := install()
	older := in.Compile(t).Index()
	in.Phones[0].Spec.Label = "Front desk" // a new desk version
	f := &fixture{t: t, ami: amitest.New(), m: metrics.New(), idx: in.Compile(t).Index(), older: older, now: t0}
	if f.idx.Devices[desk].Version == older.Devices[desk].Version {
		t.Fatal("the desk's version did not change")
	}
	f.store = observe.New(observe.Config{
		AMI: f.ami,
		Applied: func() (string, compile.Index, bool) {
			return f.idx.Version, f.idx, f.idx.Version != ""
		},
		Index: func(v string) (compile.Index, bool) {
			if v == f.older.Version {
				return f.older, true
			}
			return compile.Index{}, false
		},
		Status:   func() apply.Status { return f.status },
		Firmware: fwFunc(func() api.FirmwareStore { return f.fw }),
		Metrics:  f.m,
		Now:      func() time.Time { return f.now },
		Log:      logr.Discard(),
	})
	return f
}

type fwFunc func() api.FirmwareStore

func (fn fwFunc) Status() api.FirmwareStore { return fn() }

// tftp is a served TFTP fetch of a tftp/ file in the applied version.
func (f *fixture) tftp(file, source string, at time.Duration) sensor.FetchEvent {
	return sensor.FetchEvent{
		Transport: api.TransportTFTP, File: file, Result: api.FetchServed, At: t0.Add(at), Source: source,
		BundleVersion: f.idx.Version, BundlePath: "tftp/" + file,
	}
}

func (f *fixture) device(id string) *api.DeviceObservations {
	f.t.Helper()
	doc := f.store.Document(t0)
	for i := range doc.Devices {
		if doc.Devices[i].Device == id {
			return &doc.Devices[i]
		}
	}
	return nil
}

func (f *fixture) fetchesOf(id string) []api.Fetch {
	if d := f.device(id); d != nil {
		return d.Fetches
	}
	return nil
}

func (f *fixture) counter(transport api.Transport, result api.FetchResult) float64 {
	families, _ := f.m.Registry().Gather()
	for _, fam := range families {
		if fam.GetName() != "patchbay_agent_fetches_total" {
			continue
		}
		for _, s := range fam.GetMetric() {
			var tr, res string
			for _, l := range s.GetLabel() {
				switch l.GetName() {
				case "transport":
					tr = l.GetValue()
				case "result":
					res = l.GetValue()
				}
			}
			if tr == string(transport) && res == string(result) {
				return s.GetCounter().GetValue()
			}
		}
	}
	return -1
}

// TestAttribution: each rule, over each file transport.
func TestAttribution(t *testing.T) {
	for _, transport := range []api.Transport{api.TransportTFTP, api.TransportHTTP6970} {
		t.Run(string(transport), func(t *testing.T) {
			f := newFixture(t)
			ev := func(file, source string, at time.Duration) sensor.FetchEvent {
				e := f.tftp(file, source, at)
				e.Transport = transport
				return e
			}
			// Rule 2, by MAC, whatever the source and the case of the MAC.
			f.store.RecordFetch(ev("SEP"+deskMAC+".cnf.xml", deskIP, 1*time.Second))
			f.store.RecordFetch(ev("SEPd0c78915e8c8.cnf.xml", strangeIP, 2*time.Second))
			sgn := ev("SEP"+deskMAC+".cnf.xml.sgn", deskIP, 3*time.Second)
			sgn.Result = api.FetchNotFound
			f.store.RecordFetch(sgn)
			f.store.RecordFetch(ev("CTLSEP"+frontMAC+".tlv", frontIP, 4*time.Second))
			f.store.RecordFetch(ev("ITLSEPd0c78915e8c9.tlv", frontIP, 5*time.Second))
			// Rule 2 first: an unknown MAC from the desk's address is not the desk's.
			f.store.RecordFetch(ev("SEP00000000AAAA.cnf.xml", deskIP, 6*time.Second))
			// Rule 3: the desk's config fetch came from deskIP (a fetch of
			// another spelling of its file is not its config fetch).
			f.store.RecordFetch(ev("dialplan.xml", deskIP, 7*time.Second))
			f.store.RecordFetch(ev("dialplan.xml", strangeIP, 7*time.Second))
			// Rule 3 by a live contact: the front phone registered from frontIP.
			f.ami.SetUp(true, "22.11.0")
			f.ami.SetContacts(ami.Contact{Endpoint: front, AOR: "101", Address: frontIP, ExpiresAt: t0.Add(time.Hour)})
			f.store.Poll(context.Background())
			fw := sensor.FetchEvent{Transport: transport, File: "SIP45.9-4-2SR4-3S.loads", Result: api.FetchServed, At: t0.Add(8 * time.Second), Source: frontIP}
			f.store.RecordFetch(fw)
			// Rule 4: nobody.
			f.store.RecordFetch(ev("dialplan.xml", "10.0.0.1", 9*time.Second))

			wantFiles := map[string][]string{
				desk:  {"SEP" + deskMAC + ".cnf.xml", "SEPd0c78915e8c8.cnf.xml", "SEP" + deskMAC + ".cnf.xml.sgn", "dialplan.xml"},
				front: {"CTLSEP" + frontMAC + ".tlv", "ITLSEPd0c78915e8c9.tlv", "SIP45.9-4-2SR4-3S.loads"},
			}
			for id, want := range wantFiles {
				var got []string
				for _, fe := range f.fetchesOf(id) {
					got = append(got, fe.File)
					if fe.Transport != transport {
						t.Errorf("%s: transport %s", fe.File, fe.Transport)
					}
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s fetches %q, want %q", id, got, want)
				}
			}
			if f.counter(transport, api.FetchServed) != 9 || f.counter(transport, api.FetchNotFound) != 1 {
				t.Errorf("metrics: served %v, notFound %v; every valid event counts",
					f.counter(transport, api.FetchServed), f.counter(transport, api.FetchNotFound))
			}
			doc := f.store.Document(t0)
			if len(doc.UnknownDevices) != 1 || doc.UnknownDevices[0].MAC != "00000000AAAA" {
				t.Errorf("unknown devices %+v", doc.UnknownDevices)
			}
		})
	}
}

// TestAttributionLatestWins: an address goes to whichever device most
// recently fetched its config or registered from it.
func TestAttributionLatestWins(t *testing.T) {
	f := newFixture(t)
	f.ami.SetUp(true, "")
	f.store.RecordFetch(f.tftp("SEP"+deskMAC+".cnf.xml", deskIP, time.Second))
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, 2*time.Second))
	// The front phone registers from the desk's address, later: the address
	// is its now.
	f.now = t0.Add(3 * time.Second)
	f.ami.SetContacts(ami.Contact{Endpoint: front, Address: deskIP, ExpiresAt: t0.Add(time.Hour)})
	f.store.Poll(context.Background())
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, 4*time.Second))
	// A refreshed registration is not newer: first seen stays 3 s.
	f.now = t0.Add(10 * time.Second)
	f.store.Poll(context.Background())
	// The desk fetches its config from there at 5 s: the desk's again.
	f.store.RecordFetch(f.tftp("SEP"+deskMAC+".cnf.xml", deskIP, 5*time.Second))
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, 6*time.Second))
	var got []string
	for _, fe := range f.fetchesOf(desk) {
		got = append(got, fe.At.Sub(t0).String())
	}
	if want := []string{"1s", "2s", "5s", "6s"}; !slices.Equal(got, want) {
		t.Errorf("desk fetches at %v, want %v", got, want)
	}
	if got := f.fetchesOf(front); len(got) != 1 || !got[0].At.Equal(t0.Add(4*time.Second)) {
		t.Errorf("front fetches %+v, want the one at 4 s", got)
	}
	// An expired contact no longer claims the address.
	f.ami.SetContacts(ami.Contact{Endpoint: front, Address: "192.168.20.40", ExpiresAt: t0.Add(time.Second)})
	f.store.Poll(context.Background())
	f.store.RecordFetch(f.tftp("dialplan.xml", "192.168.20.40", 7*time.Second))
	if got := f.fetchesOf(front); len(got) != 1 {
		t.Errorf("an expired contact claimed a fetch: %+v", got)
	}
}

// TestProvisioningByDevice: device HTTP's events carry the device, kept
// even before any index has it; its Version is kept as given.
func TestProvisioningByDevice(t *testing.T) {
	f := newFixture(t)
	v := f.idx.Devices[laptop].Version
	e := sensor.FetchEvent{
		Transport: api.TransportHTTPDevice, File: "provisioning/" + laptop + ".xml", Result: api.FetchServed,
		At: t0, Source: laptopIP, BundleVersion: f.idx.Version, BundlePath: "provisioning/" + laptop + ".xml",
		Device: laptop, Version: v,
	}
	f.store.RecordFetch(e)
	if got := f.fetchesOf(laptop); len(got) != 1 || got[0].Version != v || got[0].File != e.File {
		t.Errorf("laptop fetches %+v", got)
	}
	// A device no index has yet: kept, listed once one does.
	gone := drivers.DeviceID(drivers.KindSoftphone, "den")
	e.Device, e.File, e.BundlePath = gone, "provisioning/"+gone+".xml", "provisioning/"+gone+".xml"
	f.store.RecordFetch(e)
	if f.device(gone) != nil {
		t.Error("a device outside the applied index is listed")
	}
	in := install()
	in.AddSoftphone("den", "151", "Den")
	f.idx = in.Compile(t).Index()
	if got := f.fetchesOf(gone); len(got) != 1 || got[0].Version != v {
		t.Errorf("den fetches once applied: %+v", got)
	}
	// A spent token's notFound names the device too.
	e.Result, e.Version = api.FetchNotFound, ""
	f.store.RecordFetch(e)
	if got := f.fetchesOf(gone); len(got) != 2 || got[1].Result != api.FetchNotFound {
		t.Errorf("den fetches %+v", got)
	}
}

func TestFetchVersion(t *testing.T) {
	f := newFixture(t)
	sep := "SEP" + deskMAC + ".cnf.xml"
	// Served from the applied version: the desk's version there.
	f.store.RecordFetch(f.tftp(sep, deskIP, time.Second))
	// Served from an older version: the desk's version in that one.
	old := f.tftp(sep, deskIP, 2*time.Second)
	old.BundleVersion = f.older.Version
	f.store.RecordFetch(old)
	// A shared file of the desk's: its version too.
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, 3*time.Second))
	// Not found: none.
	nf := f.tftp(sep+".sgn", deskIP, 4*time.Second)
	nf.Result = api.FetchNotFound
	f.store.RecordFetch(nf)
	// Served, but not one of its files: none.
	dir := sensor.FetchEvent{Transport: api.TransportHTTPDevice, File: "directory.xml", Result: api.FetchServed, At: t0.Add(5 * time.Second),
		Source: deskIP, BundleVersion: f.idx.Version, BundlePath: "http/directory.xml"}
	f.store.RecordFetch(dir)
	// A version no longer known: none.
	gone := f.tftp(sep, deskIP, 6*time.Second)
	gone.BundleVersion = intaketest.Version('e')
	f.store.RecordFetch(gone)
	// Firmware: none.
	f.store.RecordFetch(sensor.FetchEvent{Transport: api.TransportTFTP, File: "SIP45.loads", Result: api.FetchServed, At: t0.Add(7 * time.Second), Source: deskIP})

	want := []string{f.idx.Devices[desk].Version, f.older.Devices[desk].Version, f.idx.Devices[desk].Version, "", "", "", ""}
	var got []string
	for _, fe := range f.fetchesOf(desk) {
		got = append(got, fe.Version)
	}
	if !slices.Equal(got, want) {
		t.Errorf("versions %q, want %q", got, want)
	}
}

func TestRingBuffer(t *testing.T) {
	f := newFixture(t)
	for i := range 70 {
		f.store.RecordFetch(f.tftp("SEP"+deskMAC+".cnf.xml", deskIP, time.Duration(i)*time.Second))
	}
	// One late arrival, older than most: kept in At order.
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, 30*time.Second+time.Millisecond))
	got := f.fetchesOf(desk)
	if len(got) != observe.RingSize {
		t.Fatalf("%d fetches kept, want %d", len(got), observe.RingSize)
	}
	if !got[0].At.Equal(t0.Add(7 * time.Second)) {
		t.Errorf("oldest kept at %v, want the 8th", got[0].At)
	}
	if !slices.IsSortedFunc(got, func(a, b api.Fetch) int { return a.At.Compare(b.At) }) {
		t.Error("fetches are not in At order")
	}
	if !slices.ContainsFunc(got, func(fe api.Fetch) bool { return fe.File == "dialplan.xml" }) {
		t.Error("the late fetch was lost")
	}
	// An event older than everything kept is dropped at once.
	f.store.RecordFetch(f.tftp("dialplan.xml", deskIP, -time.Hour))
	if got := f.fetchesOf(desk); len(got) != observe.RingSize || !got[0].At.Equal(t0.Add(7*time.Second)) {
		t.Errorf("after an old event: %d kept, the oldest at %v", len(got), got[0].At)
	}
}

func TestUnknownDevices(t *testing.T) {
	f := newFixture(t)
	for i := range observe.MaxUnknownDevices + 1 {
		e := f.tftp(fmt.Sprintf("SEP%012X.cnf.xml", i+1), strangeIP, time.Duration(i)*time.Second)
		e.Result = api.FetchNotFound
		f.store.RecordFetch(e)
	}
	// The first MAC asks again, later, for its signed file from elsewhere.
	again := f.tftp("SEP000000000002.cnf.xml.sgn", "192.168.20.98", time.Hour)
	again.Result = api.FetchNotFound
	f.store.RecordFetch(again)
	// A trust list of an unknown MAC is not a config request.
	f.store.RecordFetch(f.tftp("CTLSEP0000000000FF.tlv", strangeIP, time.Hour))

	doc := f.store.Document(t0)
	if len(doc.UnknownDevices) != observe.MaxUnknownDevices {
		t.Fatalf("%d unknown devices, want %d", len(doc.UnknownDevices), observe.MaxUnknownDevices)
	}
	if doc.UnknownDevices[0].MAC != "000000000002" {
		t.Errorf("first unknown %s: the least recently seen (…01) should have gone", doc.UnknownDevices[0].MAC)
	}
	u := doc.UnknownDevices[0]
	if u.File != "SEP000000000002.cnf.xml.sgn" || u.Source != "192.168.20.98" || !u.FirstSeen.Equal(t0.Add(time.Second)) || !u.LastSeen.Equal(t0.Add(time.Hour)) {
		t.Errorf("unknown device %+v", u)
	}
	if !slices.IsSortedFunc(doc.UnknownDevices, func(a, b api.UnknownDevice) int { return strings.Compare(a.MAC, b.MAC) }) {
		t.Error("unknown devices are not sorted")
	}
	// Once a Phone has the MAC, it is no longer unknown.
	in := install()
	in.AddPhone("lobby", "000000000002", "102", "Lobby")
	f.idx = in.Compile(t).Index()
	for _, u := range f.store.Document(t0).UnknownDevices {
		if u.MAC == "000000000002" {
			t.Error("a MAC a device has is still unknown")
		}
	}
}

func TestRegistrationAndCalls(t *testing.T) {
	f := newFixture(t)
	f.ami.SetUp(true, "22.11.0")
	deskContact := ami.Contact{
		Endpoint: desk, AOR: "100", URI: "sip:100@192.168.20.5:5060;transport=tcp", Address: deskIP,
		UserAgent: "Cisco-CP7945G/9.4.2", CallID: "abc@192.168.20.5", ExpiresAt: t0.Add(10 * time.Minute), Qualify: api.QualifyReachable,
	}
	f.ami.SetContacts(
		deskContact,
		ami.Contact{AOR: "150", URI: "sip:150@192.168.0.50:5070", Address: laptopIP, ExpiresAt: t0.Add(time.Hour)}, // by AOR
		ami.Contact{Endpoint: desk, AOR: "100", Address: "192.168.20.77", ExpiresAt: t0.Add(time.Minute)},          // an older one
		ami.Contact{AOR: "999", Address: "10.1.1.1"},                                                               // nobody's
	)
	f.ami.SetChannels(
		ami.Channel{Name: "PJSIP/phone-desk-00000001", Endpoint: desk},
		ami.Channel{Name: "PJSIP/phone-desk-00000002", Endpoint: desk},
		ami.Channel{Name: "PJSIP/softphone-chris-laptop-00000003", Endpoint: laptop},
		ami.Channel{Name: "Local/600@x-00000004;1"},
	)
	f.store.Poll(context.Background())
	d := f.device(desk)
	want := &api.Registration{
		Contact: deskContact.URI, Address: deskIP, UserAgent: deskContact.UserAgent, CallID: deskContact.CallID,
		ExpiresAt: deskContact.ExpiresAt, Qualify: api.QualifyReachable,
	}
	if d == nil || d.Registration == nil || *d.Registration != *want || d.Calls != 2 {
		t.Fatalf("desk = %+v", d)
	}
	l := f.device(laptop)
	if l == nil || l.Registration == nil || l.Registration.Address != laptopIP || l.Registration.Qualify != api.QualifyUnknown || l.Calls != 1 {
		t.Errorf("laptop = %+v", l)
	}
	if f.device(front) != nil {
		t.Error("front is listed without anything seen")
	}
	if got := registered(f.m); got != 2 {
		t.Errorf("registered endpoints = %v, want 2", got)
	}

	// AMI down: registrations and calls are cleared.
	f.ami.SetUp(false, "")
	f.store.Poll(context.Background())
	if f.device(desk) != nil || f.device(laptop) != nil || registered(f.m) != 0 {
		t.Error("registrations or calls survived AMI going down")
	}
	// A failing poll clears them too.
	f.ami.SetUp(true, "")
	f.store.Poll(context.Background())
	if f.device(desk) == nil {
		t.Fatal("no registration after AMI came back")
	}
	f.ami.OnChannels(func() ([]ami.Channel, error) { return nil, errors.New("boom") })
	f.store.Poll(context.Background())
	if f.device(desk) != nil {
		t.Error("a failed poll kept the registrations")
	}
}

func registered(m *metrics.Metrics) float64 {
	families, _ := m.Registry().Gather()
	for _, fam := range families {
		if fam.GetName() == "patchbay_agent_registered_endpoints" {
			return fam.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return -1
}

func TestScrapeTargets(t *testing.T) {
	f := newFixture(t)
	if got := f.store.ScrapeTargets(t0); len(got) != 0 {
		t.Errorf("targets with nothing seen: %v", got)
	}
	// front: only a served fetch; desk: a live contact; the softphone is
	// never a target.
	f.store.RecordFetch(f.tftp("SEP"+frontMAC+".cnf.xml", frontIP, time.Second))
	notFound := f.tftp("SEP"+frontMAC+".cnf.xml", "192.168.20.66", 2*time.Second)
	notFound.Result = api.FetchNotFound
	f.store.RecordFetch(notFound) // not served: not an address
	f.ami.SetUp(true, "")
	f.ami.SetContacts(
		ami.Contact{Endpoint: desk, Address: deskIP, ExpiresAt: t0.Add(time.Minute)},
		ami.Contact{Endpoint: laptop, Address: laptopIP, ExpiresAt: t0.Add(time.Minute)},
	)
	f.store.Poll(context.Background())
	want := []sensor.ScrapeTarget{{Device: desk, Address: deskIP, Live: true}, {Device: front, Address: frontIP}}
	if got := f.store.ScrapeTargets(t0); !slices.Equal(got, want) {
		t.Errorf("targets %+v, want %+v", got, want)
	}
	// An expired contact still gives the address, not Live; so does an
	// unreachable one.
	if got := f.store.ScrapeTargets(t0.Add(2 * time.Minute)); got[0].Live || got[0].Address != deskIP {
		t.Errorf("after expiry %+v", got)
	}
	f.ami.SetContacts(ami.Contact{Endpoint: desk, Address: deskIP, ExpiresAt: t0.Add(time.Minute), Qualify: api.QualifyUnreachable})
	f.store.Poll(context.Background())
	if got := f.store.ScrapeTargets(t0); got[0].Live {
		t.Errorf("unreachable is live: %+v", got)
	}
	// Nothing applied: nothing to scrape.
	f.idx = compile.Index{}
	if got := f.store.ScrapeTargets(t0); got != nil {
		t.Errorf("targets with nothing applied: %v", got)
	}
}

func TestDocumentShape(t *testing.T) {
	f := newFixture(t)
	empty := observe.New(observe.Config{})
	raw, err := json.Marshal(empty.Document(t0))
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{`"devices":[]`, `"unknownDevices":[]`, `"files":[]`} {
		if !strings.Contains(string(raw), list) {
			t.Errorf("want %s in %s", list, raw)
		}
	}

	at := t0.Add(-time.Minute)
	f.status = apply.Status{AppliedVersion: f.idx.Version, LastApplyVersion: intaketest.Version('9'), LastApplyError: "verify: x", LastApplyAt: &at}
	f.ami.SetUp(true, "22.11.0")
	f.fw = api.FirmwareStore{Files: []api.FirmwareFileStatus{{Load: "L", Name: "a.sbn", Present: true}}}
	f.store.RecordFetch(f.tftp("SEP"+frontMAC+".cnf.xml", frontIP, time.Second))
	f.store.RecordFetch(f.tftp("SEP"+deskMAC+".cnf.xml", deskIP, time.Second))
	scrape := api.WebScrape{
		At: t0, Address: deskIP,
		DeviceInformation:    &api.DeviceInformation{VersionID: "SIP45.9-4-2SR4-3S"},
		NetworkConfiguration: &api.NetworkConfiguration{CallManager1: "192.168.0.240 Active"},
		DeviceLog:            []string{"a", "b"},
	}
	f.store.RecordScrape(desk, scrape)
	scrape.DeviceInformation.VersionID = "changed by the scraper later"

	doc := f.store.Document(t0)
	if doc.Agent.AppliedVersion != f.idx.Version || doc.Agent.LastApplyVersion != intaketest.Version('9') ||
		doc.Agent.LastApplyError != "verify: x" || !doc.Agent.LastApplyAt.Equal(at) || !doc.Agent.AsteriskUp || doc.Agent.AsteriskVersion != "22.11.0" {
		t.Errorf("agent %+v", doc.Agent)
	}
	if len(doc.Devices) != 2 || doc.Devices[0].Device != desk || doc.Devices[1].Device != front || doc.Devices[0].MAC != deskMAC {
		t.Fatalf("devices %+v", doc.Devices)
	}
	w := doc.Devices[0].Web
	if w == nil || w.DeviceInformation.VersionID != "SIP45.9-4-2SR4-3S" || !slices.Equal(w.DeviceLog, []string{"a", "b"}) {
		t.Errorf("web %+v: stored as given, a copy", w)
	}
	if !slices.Equal(doc.Firmware.Files, f.fw.Files) {
		t.Errorf("firmware %+v", doc.Firmware)
	}
	// A deep copy: changing it changes nothing in the store.
	doc.Devices[0].Fetches[0].File = "x"
	doc.Devices[0].Web.DeviceLog[0] = "x"
	doc.Devices[0].Web.NetworkConfiguration.CallManager1 = "x"
	doc.Firmware.Files[0].Present = false
	again := f.store.Document(t0)
	if again.Devices[0].Fetches[0].File == "x" || again.Devices[0].Web.DeviceLog[0] == "x" ||
		again.Devices[0].Web.NetworkConfiguration.CallManager1 == "x" || !f.fw.Files[0].Present {
		t.Error("the document shares memory with the store")
	}
}

func TestInvalidEventsAreDropped(t *testing.T) {
	f := newFixture(t)
	bad := f.tftp("SEP"+deskMAC+".cnf.xml", "not-an-ip", time.Second)
	f.store.RecordFetch(bad)
	if f.device(desk) != nil || f.counter(api.TransportTFTP, api.FetchServed) != 0 {
		t.Error("an invalid event was recorded or counted")
	}
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	f.ami.SetUp(true, "")
	f.ami.SetContacts(ami.Contact{Endpoint: desk, Address: deskIP, ExpiresAt: t0.Add(time.Hour)})
	s := observe.New(observe.Config{
		AMI: f.ami, Applied: func() (string, compile.Index, bool) { return f.idx.Version, f.idx, true },
		PollEvery: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Document(t0).Devices) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.ami.SetContacts()
	for len(s.Document(t0).Devices) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run polled only once")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

// TestDeviceLeavingTheIndexLosesItsHistory: a device that leaves the
// applied index loses what the store remembered of it, so a Phone deleted
// and created again under the same name starts Unseen, not with the old
// one's fetches; and a Phone whose MAC is edited (a hardware swap) starts
// over too. A device that was never in an applied index keeps what device
// HTTP recorded for it (TestProvisioningByDevice).
func TestDeviceLeavingTheIndexLosesItsHistory(t *testing.T) {
	f := newFixture(t)
	withDesk := f.idx
	f.store.RecordFetch(f.tftp("SEP"+deskMAC+".cnf.xml", deskIP, 0))
	f.store.RecordScrape(desk, api.WebScrape{At: t0, Address: deskIP})
	f.store.RecordFetch(f.tftp("SEP"+frontMAC+".cnf.xml", frontIP, 0))
	if d := f.device(desk); d == nil || len(d.Fetches) != 1 || d.Web == nil {
		t.Fatalf("desk before: %+v", d)
	}

	// desk deleted: a bundle without it is applied.
	in := install()
	in.Phones = in.Phones[1:]
	f.idx = in.Compile(t).Index()
	if _, ok := f.idx.Devices[desk]; ok {
		t.Fatal("desk is still in the index")
	}
	_ = f.store.Document(t0)

	// desk created again.
	f.idx = withDesk
	if d := f.device(desk); d != nil {
		t.Errorf("a re-created desk has the old one's history: %+v", d)
	}
	if got := f.fetchesOf(front); len(got) != 1 {
		t.Errorf("front, which stayed, lost its history: %+v", got)
	}
	if targets := f.store.ScrapeTargets(t0); slices.ContainsFunc(targets, func(st sensor.ScrapeTarget) bool { return st.Device == desk }) {
		t.Errorf("the re-created desk is a scrape target at the old address: %+v", targets)
	}

	// front's MAC edited (a new phone in its place): it starts over.
	in = install()
	in.Phones[1].Spec.MAC = "D0C78915E8CA"
	f.idx = in.Compile(t).Index()
	if d := f.device(front); d != nil {
		t.Errorf("front with a new MAC kept the old phone's history: %+v", d)
	}
}
