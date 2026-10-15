package scrape

import (
	"net/http"
	"slices"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

// Due is the scheduling step of one tick: the IDs of the targets due now.
func (s *Scraper) Due(now time.Time, targets []sensor.ScrapeTarget) []string {
	var ids []string
	for _, t := range s.due(now, targets) {
		ids = append(ids, t.Device)
	}
	return ids
}

// Begin marks an attempt as started at now; End as finished.
func (s *Scraper) Begin(t sensor.ScrapeTarget, now time.Time) { s.begin(t, now) }

func (s *Scraper) End(device string) {
	s.mu.Lock()
	d := s.devices[device]
	s.mu.Unlock()
	if d != nil {
		s.end(device, d)
	}
}

// Known lists the devices the scraper remembers, sorted.
func (s *Scraper) Known() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.devices {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Scraper) Client() *http.Client           { return s.client }
func (s *Scraper) PageTimeout() time.Duration     { return s.pageTimeout }
func (s *Scraper) SetPageTimeout(d time.Duration) { s.pageTimeout = d }
