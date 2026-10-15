// Package amitest is a fake ami.Client for the agent's tests: a session
// that is up or down at the test's word, answers it sets, and a record of
// every call in order.
package amitest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
)

// Fake is an ami.Client. It starts down: every call fails with
// ami.ErrNotConnected, as the real client's do, until SetUp(true, ...).
type Fake struct {
	mu        sync.Mutex
	state     ami.State
	user      string
	pass      string
	calls     []string
	notifies  []ami.Notify
	endpoints []string
	contexts  []string
	contacts  []ami.Contact
	channels  []ami.Channel

	reload      func(module string) error
	endpointsFn func() ([]string, error)
	contextsFn  func() ([]string, error)
	contactsFn  func() ([]ami.Contact, error)
	channelsFn  func() ([]ami.Channel, error)
	notify      func(ami.Notify) error
}

var _ ami.Client = (*Fake)(nil)

// New returns a fake whose session is down.
func New() *Fake {
	return &Fake{state: ami.State{Since: time.Unix(0, 0).UTC(), Error: "fake: down"}}
}

// SetUp brings the session up (with version) or down. Since moves to now
// on every call, as a new session's would.
func (f *Fake) SetUp(up bool, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = ami.State{Up: up, Version: version, Since: time.Now()}
	if !up {
		f.state.Error = "fake: down"
	}
}

// SetEndpoints sets what Endpoints answers (sorted, as Asterisk's are).
func (f *Fake) SetEndpoints(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints = slices.Sorted(slices.Values(names))
}

// SetContexts sets what DialplanContexts answers.
func (f *Fake) SetContexts(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts = slices.Sorted(slices.Values(names))
}

// SetContacts sets what Contacts answers.
func (f *Fake) SetContacts(cs ...ami.Contact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contacts = slices.Clone(cs)
}

// SetChannels sets what Channels answers.
func (f *Fake) SetChannels(chs ...ami.Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = slices.Clone(chs)
}

// OnReload makes Reload call fn (while the session is up) and return its
// error: a test's Asterisk, which may re-read the live directory.
func (f *Fake) OnReload(fn func(module string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reload = fn
}

// OnEndpoints, OnContexts, OnContacts, OnChannels and OnNotify replace the
// set answers with a function's.
func (f *Fake) OnEndpoints(fn func() ([]string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointsFn = fn
}

// OnContexts: see OnEndpoints.
func (f *Fake) OnContexts(fn func() ([]string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contextsFn = fn
}

// OnContacts: see OnEndpoints.
func (f *Fake) OnContacts(fn func() ([]ami.Contact, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contactsFn = fn
}

// OnChannels: see OnEndpoints.
func (f *Fake) OnChannels(fn func() ([]ami.Channel, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channelsFn = fn
}

// OnNotify: see OnEndpoints.
func (f *Fake) OnNotify(fn func(ami.Notify) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notify = fn
}

// Calls lists every call so far, in order: "SetAccount <user>",
// "Reload <module>", "Endpoints", "DialplanContexts", "Contacts",
// "Channels", "Notify <endpoint>". Calls made while down are listed too.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// ResetCalls forgets the calls so far.
func (f *Fake) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.notifies = nil
}

// Notifies lists every NOTIFY sent while up.
func (f *Fake) Notifies() []ami.Notify {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notifies)
}

// Account is the last account SetAccount named.
func (f *Fake) Account() (username, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user, f.pass
}

// SetAccount records the account.
func (f *Fake) SetAccount(username, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user, f.pass = username, password
	f.calls = append(f.calls, "SetAccount "+username)
}

// State is the session's state.
func (f *Fake) State() ami.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// begin records a call and reports whether the session is up.
func (f *Fake) begin(ctx context.Context, call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !f.state.Up {
		return ami.ErrNotConnected
	}
	return nil
}

// Reload calls the OnReload function, if any.
func (f *Fake) Reload(ctx context.Context, module string) error {
	if err := f.begin(ctx, "Reload "+module); err != nil {
		return err
	}
	f.mu.Lock()
	fn := f.reload
	f.mu.Unlock()
	if fn != nil {
		return fn(module)
	}
	return nil
}

// Endpoints answers what was set.
func (f *Fake) Endpoints(ctx context.Context) ([]string, error) {
	return answer(ctx, f, "Endpoints", func() (func() ([]string, error), []string) { return f.endpointsFn, f.endpoints })
}

// DialplanContexts answers what was set.
func (f *Fake) DialplanContexts(ctx context.Context) ([]string, error) {
	return answer(ctx, f, "DialplanContexts", func() (func() ([]string, error), []string) { return f.contextsFn, f.contexts })
}

// Contacts answers what was set.
func (f *Fake) Contacts(ctx context.Context) ([]ami.Contact, error) {
	return answer(ctx, f, "Contacts", func() (func() ([]ami.Contact, error), []ami.Contact) { return f.contactsFn, f.contacts })
}

// Channels answers what was set.
func (f *Fake) Channels(ctx context.Context) ([]ami.Channel, error) {
	return answer(ctx, f, "Channels", func() (func() ([]ami.Channel, error), []ami.Channel) { return f.channelsFn, f.channels })
}

// answer picks the function and the set answer under the lock, and calls
// the function outside it.
func answer[T any](ctx context.Context, f *Fake, call string, pick func() (func() ([]T, error), []T)) ([]T, error) {
	if err := f.begin(ctx, call); err != nil {
		return nil, err
	}
	f.mu.Lock()
	fnNow, set := pick()
	setNow := slices.Clone(set)
	f.mu.Unlock()
	if fnNow != nil {
		return fnNow()
	}
	return setNow, nil
}

// Notify records the NOTIFY and calls the OnNotify function, if any.
func (f *Fake) Notify(ctx context.Context, n ami.Notify) error {
	if err := f.begin(ctx, "Notify "+n.Endpoint); err != nil {
		return err
	}
	f.mu.Lock()
	f.notifies = append(f.notifies, ami.Notify{
		Endpoint: n.Endpoint, Headers: slices.Clone(n.Headers), Body: slices.Clone(n.Body),
	})
	fn := f.notify
	f.mu.Unlock()
	if fn != nil {
		return fn(n)
	}
	return nil
}
