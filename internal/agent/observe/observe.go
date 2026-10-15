// Package observe is the agent's observation store (W2.6): it folds what
// the four sensors report (design §8.2) into GET /v1/observations.
//
//   - Sensor 1, the fetch log: every transfer the file server (over its
//     socket, ServeFetchSocket) and device HTTP report, attributed to a
//     device and kept in a ring of the 64 latest per device.
//   - Sensor 2, AMI: Run polls the contacts (Registration) and channels
//     (Calls) every PollEvery; both are cleared while AMI is down.
//   - Sensor 3, the phones' web pages: the scraper asks ScrapeTargets which
//     phones to read and hands each result to RecordScrape.
//   - Sensor 4, the firmware store, is read on each Document.
//
// Attribution of a fetch, the first rule that applies:
//  1. the event's Device (only device HTTP sets it, from the provisioning
//     grant the token matched; kept even before any index has the device,
//     because device HTTP re-records persisted spends at start);
//  2. a file named for a MAC (SEP<MAC>.cnf.xml, SEP<MAC>.cnf.xml.sgn,
//     CTLSEP<MAC>.tlv, ITLSEP<MAC>.tlv): the applied device with that MAC,
//     or nobody (a MAC no device has is another phone's, whoever its address
//     was before; its config requests are UnknownDevices);
//  3. the source address: the device whose latest served config fetch, or
//     live contact, came from it (the latest wins);
//  4. nobody: counted in the metrics, not listed.
//
// A fetch's Version is the event's (device HTTP's), else, for a served
// event whose bundle path is one of the device's files in the index of the
// version it was served from, that device's version there.
//
// A device that leaves the applied index, or whose MAC changes in it,
// loses everything the store remembered of it (its fetch log, scrape and
// addresses): a Phone deleted and created again under the same name starts
// Unseen, and a phone swapped in by editing spec.mac does not show the old
// one's history. A device that was never in an applied index keeps what
// was recorded for it (device HTTP re-records persisted spends at start).
//
// The store decides nothing; it only remembers, and its memory is
// disposable.
package observe

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/apply"
	"github.com/ChristopherJMiller/patchbay/internal/agent/metrics"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Config configures a Store. Every function may be nil (nothing applied,
// no status, no firmware sensor).
type Config struct {
	// AMI is polled for contacts and channels.
	AMI ami.Client
	// Applied is the live version and its index (apply.Applier.Applied).
	Applied func() (string, compile.Index, bool)
	// Index is any kept version's index (apply.Applier.Index).
	Index func(version string) (compile.Index, bool)
	// Status is the apply status (apply.Applier.Status).
	Status func() apply.Status
	// Firmware is the firmware store sensor.
	Firmware sensor.FirmwareSensor
	// Metrics may be nil.
	Metrics *metrics.Metrics
	// PollEvery is how often Run polls AMI; 0 means 5 s.
	PollEvery time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives dropped events and poll failures.
	Log logr.Logger
}

// Limits.
const (
	// RingSize is how many fetches each device keeps.
	RingSize = 64
	// MaxUnknownDevices is how many unknown MACs are kept; the least
	// recently seen goes first.
	MaxUnknownDevices = 64
	// DefaultPollEvery is Config.PollEvery's default.
	DefaultPollEvery = 5 * time.Second
)

// macFile matches the files named for a phone's MAC: group 1 for the trust
// lists, group 2 for the config file (group 3 set for the signed one).
var macFile = regexp.MustCompile(`^(?:(?:CTL|ITL)SEP([0-9A-Fa-f]{12})\.tlv|SEP([0-9A-Fa-f]{12})\.cnf\.xml(\.sgn)?)$`)

// Store is the observation store. It implements sensor.FetchRecorder,
// sensor.ScrapeRecorder and sensor.ScrapeTargets.
type Store struct {
	cfg Config

	mu      sync.Mutex
	devices map[string]*device
	unknown map[string]*api.UnknownDevice
	// The latest AMI poll: each device's contact, when it was first seen
	// at its address, and its channels.
	contacts  map[string]ami.Contact
	firstSeen map[contactKey]time.Time
	calls     map[string]int
	// indexVersion and indexMACs are the applied index last seen, by
	// device ID and MAC (syncIndex).
	indexVersion string
	indexMACs    map[string]string
}

type device struct {
	fetches    []api.Fetch
	web        *api.WebScrape
	lastServed *seen // the latest served fetch: the scrape address without a contact
	lastConfig *seen // the latest served config fetch: attribution by address
}

type seen struct {
	source string
	at     time.Time
}

type contactKey struct{ device, address string }

// New returns an empty store.
func New(cfg Config) *Store {
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = DefaultPollEvery
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Store{
		cfg: cfg, devices: map[string]*device{}, unknown: map[string]*api.UnknownDevice{},
		contacts: map[string]ami.Contact{}, firstSeen: map[contactKey]time.Time{}, calls: map[string]int{},
	}
}

func (s *Store) applied() (string, compile.Index, bool) {
	if s.cfg.Applied == nil {
		return "", compile.Index{}, false
	}
	return s.cfg.Applied()
}

// syncIndex forgets the devices that left the applied index since the
// last one seen, or whose MAC changed in it (package documentation). The
// caller holds mu.
func (s *Store) syncIndex(version string, idx compile.Index, ok bool) {
	if !ok || version == s.indexVersion {
		return
	}
	for id, mac := range s.indexMACs {
		if d, still := idx.Devices[id]; !still || d.MAC != mac {
			delete(s.devices, id)
			s.cfg.Log.V(1).Info("forgetting a device that left the applied index or changed its MAC", "device", id)
		}
	}
	s.indexVersion = version
	s.indexMACs = make(map[string]string, len(idx.Devices))
	for id, d := range idx.Devices {
		s.indexMACs[id] = d.MAC
	}
}

// indexOf is a version's index: the applied one's, else Index's.
func (s *Store) indexOf(version, appliedVersion string, applied compile.Index) (compile.Index, bool) {
	switch {
	case version == "":
		return compile.Index{}, false
	case version == appliedVersion:
		return applied, true
	case s.cfg.Index != nil:
		return s.cfg.Index(version)
	}
	return compile.Index{}, false
}

// RecordFetch records one fetch event. It drops (and logs) an invalid one.
func (s *Store) RecordFetch(e sensor.FetchEvent) {
	if err := e.Validate(); err != nil {
		s.cfg.Log.V(1).Info("dropping an invalid fetch event", "error", err.Error())
		return
	}
	s.cfg.Metrics.Fetch(e.Transport, e.Result)
	appliedVersion, applied, isApplied := s.applied()
	served, hasServed := s.indexOf(e.BundleVersion, appliedVersion, applied)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncIndex(appliedVersion, applied, isApplied)
	id, mac := s.attribute(e, applied)
	if mac != "" && id == "" && strings.HasPrefix(e.File, "SEP") {
		s.noteUnknown(mac, e)
	}
	if id == "" {
		s.cfg.Log.V(2).Info("fetch attributed to no device", "file", e.File, "source", e.Source)
		return
	}
	f := api.Fetch{
		File: e.File, Transport: e.Transport, Result: e.Result, At: e.At.UTC(), Source: e.Source,
		Version: e.Version, Error: e.Error,
	}
	var own compile.BundleDevice
	if hasServed {
		own = served.Devices[id]
	} else {
		own = applied.Devices[id]
	}
	if f.Version == "" && e.Result == api.FetchServed && hasServed && e.BundlePath != "" && slices.Contains(own.Files, e.BundlePath) {
		f.Version = own.Version
	}
	d := s.device(id)
	d.add(f)
	if e.Result == api.FetchServed {
		at := seen{source: e.Source, at: f.At}
		if d.lastServed == nil || !at.at.Before(d.lastServed.at) {
			d.lastServed = &at
		}
		if e.BundlePath != "" && e.BundlePath == own.ConfigFile && (d.lastConfig == nil || !at.at.Before(d.lastConfig.at)) {
			d.lastConfig = &at
		}
	}
}

// attribute names the device a fetch belongs to, and the MAC its file
// names (rules in the package documentation). The caller holds mu.
func (s *Store) attribute(e sensor.FetchEvent, idx compile.Index) (id, mac string) {
	if e.Device != "" {
		return e.Device, ""
	}
	if m := macFile.FindStringSubmatch(e.File); m != nil {
		mac = strings.ToUpper(m[1] + m[2])
		for _, devID := range slices.Sorted(maps.Keys(idx.Devices)) {
			if idx.Devices[devID].MAC == mac {
				delete(s.unknown, mac)
				return devID, mac
			}
		}
		return "", mac
	}
	return s.ownerOf(e.Source, e.At), ""
}

// ownerOf is the device whose latest served config fetch, or live
// contact, came from address. The caller holds mu.
func (s *Store) ownerOf(address string, now time.Time) string {
	best, bestAt := "", time.Time{}
	consider := func(id string, at time.Time) {
		if best == "" || at.After(bestAt) || at.Equal(bestAt) && id < best {
			best, bestAt = id, at
		}
	}
	for id, d := range s.devices {
		if d.lastConfig != nil && d.lastConfig.source == address {
			consider(id, d.lastConfig.at)
		}
	}
	for id, c := range s.contacts {
		if c.Address == address && c.ExpiresAt.After(now) {
			consider(id, s.firstSeen[contactKey{id, address}])
		}
	}
	return best
}

func (s *Store) device(id string) *device {
	d := s.devices[id]
	if d == nil {
		d = &device{}
		s.devices[id] = d
	}
	return d
}

// add keeps the fetches in At order, the newest RingSize.
func (d *device) add(f api.Fetch) {
	i := len(d.fetches)
	for i > 0 && d.fetches[i-1].At.After(f.At) {
		i--
	}
	d.fetches = slices.Insert(d.fetches, i, f)
	if over := len(d.fetches) - RingSize; over > 0 {
		d.fetches = slices.Delete(d.fetches, 0, over)
	}
}

// noteUnknown records a config request of a MAC no device has. The caller
// holds mu.
func (s *Store) noteUnknown(mac string, e sensor.FetchEvent) {
	u := s.unknown[mac]
	if u == nil {
		if len(s.unknown) >= MaxUnknownDevices {
			var oldest *api.UnknownDevice
			for _, x := range s.unknown {
				if oldest == nil || x.LastSeen.Before(oldest.LastSeen) || x.LastSeen.Equal(oldest.LastSeen) && x.MAC < oldest.MAC {
					oldest = x
				}
			}
			delete(s.unknown, oldest.MAC)
		}
		u = &api.UnknownDevice{MAC: mac, FirstSeen: e.At.UTC(), LastSeen: e.At.UTC()}
		s.unknown[mac] = u
	}
	if e.At.Before(u.FirstSeen) {
		u.FirstSeen = e.At.UTC()
	}
	if !e.At.Before(u.LastSeen) {
		u.LastSeen, u.File, u.Source = e.At.UTC(), e.File, e.Source
	}
}

// RecordScrape stores a scrape as given (a deep copy).
func (s *Store) RecordScrape(device string, w api.WebScrape) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device(device).web = copyScrape(&w)
}

// ScrapeTargets lists the applied Phones with a known address: their live
// contact's, else the source of their latest attributed served fetch.
func (s *Store) ScrapeTargets(now time.Time) []sensor.ScrapeTarget {
	version, idx, ok := s.applied()
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncIndex(version, idx, ok)
	var out []sensor.ScrapeTarget
	for _, id := range slices.Sorted(maps.Keys(idx.Devices)) {
		if idx.Devices[id].Kind != drivers.KindPhone {
			continue
		}
		t := sensor.ScrapeTarget{Device: id}
		if c, ok := s.contacts[id]; ok && c.Address != "" {
			t.Address = c.Address
			t.Live = c.ExpiresAt.After(now) && c.Qualify != api.QualifyUnreachable
		} else if d := s.devices[id]; d != nil && d.lastServed != nil {
			t.Address = d.lastServed.source
		}
		if t.Address != "" {
			out = append(out, t)
		}
	}
	return out
}

// Run polls AMI every PollEvery until ctx is done; it returns nil then.
func (s *Store) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.PollEvery)
	defer t.Stop()
	for {
		s.Poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Poll reads the contacts and channels once.
func (s *Store) Poll(ctx context.Context) {
	now := s.cfg.Now()
	_, idx, _ := s.applied()
	var contacts []ami.Contact
	var channels []ami.Channel
	var err error
	switch {
	case s.cfg.AMI == nil:
		err = ami.ErrNotConnected
	case !s.cfg.AMI.State().Up:
		err = ami.ErrNotConnected
	default:
		pctx, cancel := context.WithTimeout(ctx, max(s.cfg.PollEvery, DefaultPollEvery))
		contacts, err = s.cfg.AMI.Contacts(pctx)
		if err == nil {
			channels, err = s.cfg.AMI.Channels(pctx)
		}
		cancel()
	}

	s.mu.Lock()
	byDevice := map[string]ami.Contact{}
	calls := map[string]int{}
	if err != nil {
		s.cfg.Log.V(1).Info("no AMI poll: registrations and calls cleared", "error", err.Error())
	} else {
		for _, c := range contacts {
			id := contactOwner(c, idx)
			if id == "" {
				continue
			}
			if prev, dup := byDevice[id]; !dup || c.ExpiresAt.After(prev.ExpiresAt) {
				byDevice[id] = c
			}
		}
		for _, ch := range channels {
			if ch.Endpoint != "" {
				calls[ch.Endpoint]++
			}
		}
	}
	firstSeen := map[contactKey]time.Time{}
	for id, c := range byDevice {
		k := contactKey{id, c.Address}
		if at, ok := s.firstSeen[k]; ok {
			firstSeen[k] = at
		} else {
			firstSeen[k] = now
		}
	}
	s.contacts, s.calls, s.firstSeen = byDevice, calls, firstSeen
	n := len(byDevice)
	s.mu.Unlock()
	s.cfg.Metrics.SetRegisteredEndpoints(n)
}

// contactOwner is the device a contact belongs to: its endpoint, else the
// device whose number is its AOR.
func contactOwner(c ami.Contact, idx compile.Index) string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	if c.AOR == "" {
		return ""
	}
	for _, id := range slices.Sorted(maps.Keys(idx.Devices)) {
		if idx.Devices[id].Number == c.AOR {
			return id
		}
	}
	return ""
}

// Document is GET /v1/observations: a deep copy, every list non-nil,
// devices and unknown devices sorted.
func (s *Store) Document(now time.Time) api.Observations {
	var agent api.AgentStatus
	if s.cfg.Status != nil {
		st := s.cfg.Status()
		agent.AppliedVersion, agent.LastApplyVersion, agent.LastApplyError = st.AppliedVersion, st.LastApplyVersion, st.LastApplyError
		if st.LastApplyAt != nil {
			at := st.LastApplyAt.UTC()
			agent.LastApplyAt = &at
		}
	}
	if s.cfg.AMI != nil {
		st := s.cfg.AMI.State()
		agent.AsteriskUp, agent.AsteriskVersion = st.Up, st.Version
	}
	doc := api.Empty(now.UTC(), agent)
	if s.cfg.Firmware != nil {
		if files := s.cfg.Firmware.Status().Files; files != nil {
			doc.Firmware.Files = slices.Clone(files)
		}
	}
	version, idx, ok := s.applied()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncIndex(version, idx, ok)
	for _, id := range slices.Sorted(maps.Keys(idx.Devices)) {
		obs := api.DeviceObservations{Device: id, MAC: idx.Devices[id].MAC, Calls: s.calls[id]}
		seen := obs.Calls > 0
		if d := s.devices[id]; d != nil {
			if len(d.fetches) > 0 {
				obs.Fetches, seen = slices.Clone(d.fetches), true
			}
			if d.web != nil {
				obs.Web, seen = copyScrape(d.web), true
			}
		}
		if c, ok := s.contacts[id]; ok {
			obs.Registration, seen = registrationOf(c), true
		}
		if seen {
			doc.Devices = append(doc.Devices, obs)
		}
	}
	macs := map[string]bool{}
	for _, d := range idx.Devices {
		if d.MAC != "" {
			macs[d.MAC] = true
		}
	}
	for _, mac := range slices.Sorted(maps.Keys(s.unknown)) {
		if macs[mac] {
			delete(s.unknown, mac)
			continue
		}
		doc.UnknownDevices = append(doc.UnknownDevices, *s.unknown[mac])
	}
	return doc
}

func registrationOf(c ami.Contact) *api.Registration {
	q := c.Qualify
	if q == "" {
		q = api.QualifyUnknown
	}
	return &api.Registration{
		Contact: c.URI, Address: c.Address, UserAgent: c.UserAgent, CallID: c.CallID,
		ExpiresAt: c.ExpiresAt.UTC(), Qualify: q,
	}
}

func copyScrape(w *api.WebScrape) *api.WebScrape {
	out := *w
	if w.DeviceInformation != nil {
		di := *w.DeviceInformation
		out.DeviceInformation = &di
	}
	if w.NetworkConfiguration != nil {
		nc := *w.NetworkConfiguration
		out.NetworkConfiguration = &nc
	}
	out.DeviceLog = slices.Clone(w.DeviceLog)
	return &out
}
