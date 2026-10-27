// Package scrape is the agent's sensor 3 (design §8.2): it reads each
// phone's own web pages, /DeviceInformationX, /NetworkConfigurationX and
// /DeviceLogX, and hands each result to the observation store.
//
// The scraper senses; it decides nothing. The observation store says which
// phones to read (sensor.ScrapeTargets: a phone with an address, and
// whether it has a live SIP contact) and keeps what each scrape found
// (sensor.ScrapeRecorder). The scraper reads a phone without a live contact
// every 60 s and one with a live contact every 5 min (docs/plans/release-a.md
// §5.1: the agent approximates "Ready"), and at once when its address
// changes.
//
// The results follow api.WebScrape's rule, which the cisco79x5 driver's
// fold relies on: /DeviceInformationX, which carries the load, decides. A
// scrape succeeds when it was read and parsed, and then At is when the
// last page was read or given up on; a failed scrape carries its own At
// and Error and adopts nothing it read, only the previous success's
// fields. /NetworkConfigurationX and /DeviceLogX are best effort (a real
// CP-7945G breaks off the first and serves the second empty): one that
// cannot be read leaves its field nil and is logged, never failing the
// scrape, and a /NetworkConfigurationX that breaks off keeps the fields
// completed before the break.
//
// The phones are on the voice VLAN and answer whatever they like, so every
// request is bounded: 5 s per page, at most 256 KiB per page, no redirects,
// no proxy, and an address that cannot be a phone (loopback, unspecified,
// multicast, link-local, broadcast) is refused without a request.
package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
	"github.com/ChristopherJMiller/patchbay/internal/version"
)

// The pages, in the order a scrape reads them.
const (
	PathDeviceInformation    = "/DeviceInformationX"
	PathNetworkConfiguration = "/NetworkConfigurationX"
	PathDeviceLog            = "/DeviceLogX"
)

// Defaults for Config's zero values.
const (
	DefaultPort         = 80
	DefaultNotLiveEvery = 60 * time.Second
	DefaultLiveEvery    = 5 * time.Minute
	DefaultTick         = 5 * time.Second
	DefaultConcurrency  = 4
	// PageTimeout bounds each page's request, whatever client is given.
	PageTimeout = 5 * time.Second
	// MaxPageSize is the largest page a scrape reads; a larger one fails it.
	MaxPageSize = 256 << 10
)

// maxErrorLength bounds WebScrape.Error: it can quote a phone's own text.
const maxErrorLength = 256

// Config configures a Scraper.
type Config struct {
	// Targets says which phones to read (the observation store).
	Targets sensor.ScrapeTargets
	// Recorder keeps each result (the observation store).
	Recorder sensor.ScrapeRecorder
	// Client makes the requests. Nil: a client with a 5 s timeout, no
	// proxy and no keep-alives. Whatever the client, redirects are never
	// followed (a 3xx fails the scrape) and each page has PageTimeout.
	Client *http.Client
	// Port is the phones' web server port; 0 means 80.
	Port int
	// NotLiveEvery is how often a phone without a live contact is read;
	// 0 means 60 s.
	NotLiveEvery time.Duration
	// LiveEvery is how often a phone with a live contact is read; 0 means
	// 5 min.
	LiveEvery time.Duration
	// Tick is how often Run looks for phones that are due; 0 means 5 s.
	Tick time.Duration
	// Concurrency is how many phones Run reads at once; 0 means 4.
	Concurrency int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives each failed scrape (V(1)) and success (V(2)).
	Log logr.Logger
}

// Scraper reads phones' web pages.
type Scraper struct {
	targets     sensor.ScrapeTargets
	recorder    sensor.ScrapeRecorder
	client      *http.Client
	port        int
	notLive     time.Duration
	live        time.Duration
	tick        time.Duration
	concurrency int
	pageTimeout time.Duration
	now         func() time.Time
	log         logr.Logger

	mu      sync.Mutex
	devices map[string]*device
}

// device is what the scraper remembers of one phone.
type device struct {
	address   string         // the address of the last attempt
	attempted time.Time      // when the last attempt started; zero: never
	inFlight  bool           // an attempt is running
	success   *api.WebScrape // the last successful scrape, nil if none
}

// New returns a Scraper. It does no I/O.
func New(cfg Config) *Scraper {
	s := &Scraper{
		targets:     cfg.Targets,
		recorder:    cfg.Recorder,
		port:        orDefault(cfg.Port, DefaultPort),
		notLive:     orDefault(cfg.NotLiveEvery, DefaultNotLiveEvery),
		live:        orDefault(cfg.LiveEvery, DefaultLiveEvery),
		tick:        orDefault(cfg.Tick, DefaultTick),
		concurrency: orDefault(cfg.Concurrency, DefaultConcurrency),
		pageTimeout: PageTimeout,
		now:         cfg.Now,
		log:         cfg.Log,
		devices:     map[string]*device{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if cfg.Client == nil {
		s.client = defaultClient()
	} else {
		c := *cfg.Client
		c.CheckRedirect = noRedirects
		s.client = &c
	}
	return s
}

func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func defaultClient() *http.Client {
	return &http.Client{
		Timeout:       PageTimeout,
		CheckRedirect: noRedirects,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            (&net.Dialer{Timeout: PageTimeout}).DialContext,
			DisableKeepAlives:      true,
			ResponseHeaderTimeout:  PageTimeout,
			MaxResponseHeaderBytes: 64 << 10,
		},
	}
}

// Run reads every due phone on each tick, at most Concurrency at once,
// and records each result, until ctx is done. It then waits for the
// scrapes in flight, which the cancelled context ends, and records none
// of them.
func (s *Scraper) Run(ctx context.Context) error {
	if s.targets == nil || s.recorder == nil {
		return errors.New("scrape: Run needs Targets and a Recorder")
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	slots := make(chan struct{}, s.concurrency)
	for {
		now := s.now()
	launch:
		for _, t := range s.due(now, s.targets.ScrapeTargets(now)) {
			select {
			case slots <- struct{}{}:
			default:
				// Every slot is busy: the rest stay due for the next tick.
				break launch
			}
			d := s.begin(t, now)
			wg.Go(func() {
				defer func() { <-slots }()
				w, lacking := s.scrape(ctx, t, d)
				if s.end(t.Device, d) && ctx.Err() == nil {
					s.logResult(t, w, lacking)
					s.recorder.RecordScrape(t.Device, w)
				}
			})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// logResult logs a failed scrape and a success that went without part of
// a best-effort page (V(1)), and any other success (V(2)).
func (s *Scraper) logResult(t sensor.ScrapeTarget, w api.WebScrape, lacking []string) {
	switch {
	case w.Error != "":
		s.log.V(1).Info("scrape failed", "device", t.Device, "address", t.Address, "error", w.Error)
	case len(lacking) > 0:
		s.log.V(1).Info("scraped without part of a page", "device", t.Device, "address", t.Address, "lacking", lacking)
	default:
		s.log.V(2).Info("scraped", "device", t.Device, "address", t.Address)
	}
}

// due returns the targets to read now, never scraped first and then the
// oldest attempt first, and forgets every device that is not a target.
func (s *Scraper) due(now time.Time, targets []sensor.ScrapeTarget) []sensor.ScrapeTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		t         sensor.ScrapeTarget
		attempted time.Time
	}
	var out []candidate
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.Device == "" || seen[t.Device] {
			continue
		}
		seen[t.Device] = true
		d := s.devices[t.Device]
		if d == nil {
			d = &device{}
			s.devices[t.Device] = d
		}
		every := s.notLive
		if t.Live {
			every = s.live
		}
		if !d.inFlight && (d.attempted.IsZero() || d.address != t.Address || now.Sub(d.attempted) >= every) {
			out = append(out, candidate{t, d.attempted})
		}
	}
	for id := range s.devices {
		if !seen[id] {
			delete(s.devices, id)
		}
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		if c := a.attempted.Compare(b.attempted); c != 0 {
			return c
		}
		return strings.Compare(a.t.Device, b.t.Device)
	})
	ts := make([]sensor.ScrapeTarget, len(out))
	for i, c := range out {
		ts[i] = c.t
	}
	return ts
}

// begin marks an attempt on t as started at now.
func (s *Scraper) begin(t sensor.ScrapeTarget, now time.Time) *device {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[t.Device]
	if d == nil {
		d = &device{}
		s.devices[t.Device] = d
	}
	d.address, d.attempted, d.inFlight = t.Address, now, true
	return d
}

// end marks d's attempt as finished and reports whether d is still
// remembered (its device was not forgotten meanwhile).
func (s *Scraper) end(id string, d *device) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.inFlight = false
	return s.devices[id] == d
}

// Scrape reads t's pages once and returns the result merged per
// api.WebScrape's rule: on success every field is from this scrape (nil
// for a best-effort page it could not read) and At is when the last page
// was read or given up on; on failure At is when it failed, Error says
// why, and the fields are the previous success's (nil if none). It records
// nothing; it remembers a success for the next merge.
func (s *Scraper) Scrape(ctx context.Context, t sensor.ScrapeTarget) api.WebScrape {
	s.mu.Lock()
	d := s.devices[t.Device]
	if d == nil {
		d = &device{}
		s.devices[t.Device] = d
	}
	s.mu.Unlock()
	w, _ := s.scrape(ctx, t, d)
	return w
}

// scrape is Scrape; it also returns what a success went without (read's
// lacking).
func (s *Scraper) scrape(ctx context.Context, t sensor.ScrapeTarget, d *device) (api.WebScrape, []string) {
	w, lacking, err := s.read(ctx, t.Address)
	at := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		w.At, w.Address = at, t.Address
		kept := clone(w)
		d.success = &kept
		return w, lacking
	}
	out := api.WebScrape{At: at, Address: t.Address, Error: sanitize(err.Error(), maxErrorLength)}
	if d.success != nil {
		prev := clone(*d.success)
		out.DeviceInformation, out.NetworkConfiguration, out.DeviceLog =
			prev.DeviceInformation, prev.NetworkConfiguration, prev.DeviceLog
	}
	return out, nil
}

// clone deep-copies a scrape, so what the scraper keeps and what it hands
// out never share memory.
func clone(w api.WebScrape) api.WebScrape {
	if w.DeviceInformation != nil {
		v := *w.DeviceInformation
		w.DeviceInformation = &v
	}
	if w.NetworkConfiguration != nil {
		v := *w.NetworkConfiguration
		w.NetworkConfiguration = &v
	}
	w.DeviceLog = slices.Clone(w.DeviceLog)
	return w
}

// read fetches and parses the pages. /DeviceInformationX decides: when it
// cannot be read the scrape fails, and nothing else is fetched. The other
// two are best effort: one that cannot be read is left nil, one that
// breaks off keeps what came before the break, and lacking says (bounded
// text) what the scrape went without. An empty /DeviceLogX is an empty
// log, not something lacking. A context that ends during the scrape fails
// it, since its best-effort pages were not given their chance.
func (s *Scraper) read(ctx context.Context, address string) (w api.WebScrape, lacking []string, err error) {
	addr, err := scrapeable(address)
	if err != nil {
		return api.WebScrape{}, nil, err
	}
	host := addr.String()
	if addr.Is6() {
		host = "[" + host + "]"
	}
	if s.port != DefaultPort {
		host = net.JoinHostPort(addr.String(), strconv.Itoa(s.port))
	}
	b, err := s.get(ctx, host, PathDeviceInformation)
	if err == nil {
		w.DeviceInformation, err = ParseDeviceInformation(b)
	}
	if err != nil {
		return api.WebScrape{}, nil, fmt.Errorf("%s: %w", PathDeviceInformation, err)
	}
	lack := func(path string, err error) {
		lacking = append(lacking, sanitize(path+": "+err.Error(), maxErrorLength))
	}
	if b, err := s.get(ctx, host, PathNetworkConfiguration); err != nil {
		lack(PathNetworkConfiguration, err)
	} else if w.NetworkConfiguration, err = ParseNetworkConfiguration(b); err != nil {
		lack(PathNetworkConfiguration, err)
	}
	if b, err := s.get(ctx, host, PathDeviceLog); err != nil {
		lack(PathDeviceLog, err)
	} else if w.DeviceLog, err = ParseDeviceLog(b); err != nil {
		lack(PathDeviceLog, err)
	}
	if err := ctx.Err(); err != nil {
		return api.WebScrape{}, nil, err
	}
	return w, lacking, nil
}

// scrapeable parses a target address and refuses one that cannot be a
// phone's: the scraper must not be pointed at the PBX node itself, a
// multicast group or a link-local neighbour.
func scrapeable(address string) (netip.Addr, error) {
	a, err := netip.ParseAddr(address)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an IP address", sanitize(address, 64))
	}
	a = a.Unmap()
	why := ""
	switch {
	case a.Zone() != "":
		why = "it has a zone"
	case a.IsLoopback():
		why = "loopback"
	case a.IsUnspecified():
		why = "unspecified"
	case a.IsMulticast():
		why = "multicast"
	case a.IsLinkLocalUnicast():
		why = "link-local"
	case a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		why = "broadcast"
	}
	if why != "" {
		return netip.Addr{}, fmt.Errorf("refusing to scrape %s: %s", a, why)
	}
	return a, nil
}

// get fetches one page: 2xx only, at most MaxPageSize bytes.
func (s *Scraper) get(ctx context.Context, host, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.pageTimeout)
	defer cancel()
	u := url.URL{Scheme: "http", Host: host, Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "patchbay-agent/"+version.Version)
	req.Header.Set("Accept", "text/xml, application/xml")
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxPageSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the page: %w", err)
	}
	if len(b) > MaxPageSize {
		return nil, fmt.Errorf("the page is larger than %d KiB", MaxPageSize>>10)
	}
	return b, nil
}

// Scrapeable parses a phone's address and refuses one that cannot be a
// phone's (loopback, unspecified, multicast, link-local, broadcast, or with
// a zone). The push uses the same rule, so the agent talks to a phone's web
// server only at an address it would scrape.
func Scrapeable(address string) (netip.Addr, error) { return scrapeable(address) }
