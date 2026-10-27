package push

import (
	"slices"
	"sync"
)

// Pages holds the message page of each device, in memory: the agent's
// memory is disposable, and the controller pushes again when the observed
// version (api.DeviceObservations.Page) is not the one it wants. The zero
// value is ready to use; it is safe for concurrent use.
type Pages struct {
	mu sync.Mutex
	m  map[string]page
}

type page struct {
	version string
	body    []byte
}

// Set stores the device's page, replacing any earlier one. It keeps its own
// copy of the bytes.
func (p *Pages) Set(device, version string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]page{}
	}
	p.m[device] = page{version: version, body: slices.Clone(body)}
}

// Get returns a copy of the device's page and its version; ok is false when
// the device has none.
func (p *Pages) Get(device string) (version string, body []byte, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[device]
	if !ok {
		return "", nil, false
	}
	return e.version, slices.Clone(e.body), true
}

// Clear drops the device's page.
func (p *Pages) Clear(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, device)
}
