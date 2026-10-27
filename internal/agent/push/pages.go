package push

import (
	"slices"
	"sync"
)

// Pages holds the message page of each device, in memory, and the version
// the phone last acknowledged. The agent's memory is disposable: the
// controller pushes again when the acknowledged version
// (api.DeviceObservations.Page) is not the one it wants. The zero value is
// ready to use; it is safe for concurrent use.
//
// Stored bytes and acknowledgement differ on purpose. Set stores the bytes
// before the push, so the phone's fetch finds them, and they stay after a
// failed push; only Acknowledge (the phone answered Status 0) changes what
// is reported as being on the phone, and Unacknowledge (the phone
// restarted, so its screen is idle) takes that back.
type Pages struct {
	mu    sync.Mutex
	m     map[string]page
	acked map[string]string
}

type page struct {
	version string
	body    []byte
}

// Set stores the device's page, replacing any earlier one. It keeps its own
// copy of the bytes and leaves the acknowledged version alone.
func (p *Pages) Set(device, version string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]page{}
	}
	p.m[device] = page{version: version, body: slices.Clone(body)}
}

// Get returns a copy of the device's stored page and its version; ok is
// false when the device has none.
func (p *Pages) Get(device string) (version string, body []byte, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[device]
	if !ok {
		return "", nil, false
	}
	return e.version, slices.Clone(e.body), true
}

// Acknowledge records that the phone accepted the page of this version.
func (p *Pages) Acknowledge(device, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acked == nil {
		p.acked = map[string]string{}
	}
	p.acked[device] = version
}

// Unacknowledge forgets that the phone holds a page and keeps the stored
// bytes: the phone restarted (observe.Store saw it register under a new
// Call-ID), so it shows its idle screen, and the controller pushes the page
// again.
func (p *Pages) Unacknowledge(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.acked, device)
}

// Acknowledged is the page version the phone last acknowledged, "" for none.
func (p *Pages) Acknowledged(device string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked[device]
}

// Clear drops the device's stored page and its acknowledged version.
func (p *Pages) Clear(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, device)
	delete(p.acked, device)
}
