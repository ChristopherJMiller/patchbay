package ami

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
)

// Config configures a Conn.
type Config struct {
	// Addr is AMI's address, "127.0.0.1:5038" (compile.AMIPort).
	Addr string
	// Dial opens the TCP connection; nil uses a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Backoff is how long to wait before reconnect attempt n (1, 2, ...);
	// nil waits 1 s, doubling up to 30 s.
	Backoff func(attempt int) time.Duration
	// Now is the clock (State.Since); nil means time.Now.
	Now func() time.Time
	// Log receives session changes. Never the password.
	Log logr.Logger
}

// Timeouts the client applies on its own.
const (
	// dialTimeout bounds connecting, reading the banner and logging in.
	dialTimeout = 10 * time.Second
	// callTimeout bounds a call whose context has no deadline, so a reply
	// that never comes is an error, never a hang.
	callTimeout = 30 * time.Second
	// minSession is how long a session must last for the next login to
	// follow at once rather than after a backoff.
	minSession = 5 * time.Second
	// waiterBuffer is how many messages a call can hold before the reader
	// waits for it to take them.
	waiterBuffer = 32
)

// bannerPrefix starts the line Asterisk greets every connection with.
const bannerPrefix = "Asterisk Call Manager/"

// defaultBackoff is 1 s, doubling, at most 30 s.
func defaultBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}

// Conn is the AMI client: one session to Asterisk, kept up by Run as the
// account SetAccount last named. It implements Client.
type Conn struct {
	cfg Config
	ids atomic.Uint64

	accountChanged chan struct{} // capacity 1: SetAccount's wake-up for Run

	mu       sync.Mutex
	username string
	password string
	gen      uint64 // the account's generation: 0 until SetAccount, then +1 per change
	state    State
	sess     *session
}

// New returns a client that does nothing until SetAccount and Run.
func New(cfg Config) *Conn {
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{}).DialContext
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &Conn{cfg: cfg, accountChanged: make(chan struct{}, 1)}
	c.state = State{Since: cfg.Now(), Error: "no AMI account yet"}
	return c
}

// SetAccount sets the account to log in as. A change ends the current
// session, and Run logs in again with the new account at once.
func (c *Conn) SetAccount(username, password string) {
	c.mu.Lock()
	changed := c.gen == 0 || username != c.username || password != c.password
	if changed {
		c.username, c.password = username, password
		c.gen++
	}
	c.mu.Unlock()
	if changed {
		select {
		case c.accountChanged <- struct{}{}:
		default:
		}
	}
}

// State is the session's health.
func (c *Conn) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Conn) account() (username, password string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.username, c.password, c.gen
}

func (c *Conn) setUp(s *session, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess = s
	c.state = State{Up: true, Version: version, Since: c.cfg.Now()}
}

func (c *Conn) setDown(why error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess = nil
	if c.state.Up {
		c.state.Since = c.cfg.Now()
	}
	c.state.Up = false
	c.state.Error = why.Error()
}

// session returns the session that is up, or ErrNotConnected.
func (c *Conn) session() (*session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return nil, ErrNotConnected
	}
	return c.sess, nil
}

// Run keeps one session up until ctx is done: it waits for an account,
// connects, logs in, reads the version (CoreSettings), and reconnects with
// backoff whenever the session ends or the account changes. It returns nil
// once ctx is done. Call it once.
func (c *Conn) Run(ctx context.Context) error {
	attempt := 0
	for {
		username, password, gen := c.account()
		if gen == 0 {
			c.waitForChange(ctx, gen, nil, nil)
			if stopped(ctx) {
				return nil
			}
			continue
		}
		s, version, err := c.connect(ctx, username, password)
		if stopped(ctx) {
			if s != nil {
				s.close(errors.New("ami: stopped"))
			}
			c.setDown(errors.New("ami: stopped"))
			return nil
		}
		if err != nil {
			attempt++
			c.setDown(err)
			wait := c.cfg.Backoff(attempt)
			c.cfg.Log.Info("AMI session not established; retrying", "error", err.Error(), "in", wait.String())
			if c.waitForChange(ctx, gen, nil, time.After(wait)) {
				attempt = 0 // a new account: try it at once
			}
			if stopped(ctx) {
				return nil
			}
			continue
		}
		attempt = 0
		c.setUp(s, version)
		upAt := time.Now()
		c.cfg.Log.Info("AMI session up", "asteriskVersion", version)

		changed := c.waitForChange(ctx, gen, s.done, nil)
		switch {
		case stopped(ctx):
			s.close(errors.New("ami: stopped"))
			c.setDown(errors.New("ami: stopped"))
			return nil
		case changed:
			s.close(errors.New("ami: the account changed"))
			c.setDown(errors.New("ami: logging in again: the account changed"))
			c.cfg.Log.Info("AMI account changed; logging in again")
		default:
			c.setDown(s.cause())
			c.cfg.Log.Info("AMI session ended", "error", s.cause().Error())
			// A session that ends as soon as it starts is not a reason to
			// log in again at once, over and over.
			if time.Since(upAt) < minSession {
				attempt++
				c.waitForChange(ctx, gen, nil, time.After(c.cfg.Backoff(attempt)))
			}
		}
	}
}

// waitForChange waits until ctx is done or done is closed or timer fires
// (returning false), or until the account is no longer generation gen
// (returning true). A wake-up for the account already in use is ignored.
func (c *Conn) waitForChange(ctx context.Context, gen uint64, done <-chan struct{}, timer <-chan time.Time) bool {
	for {
		if _, _, now := c.account(); now != gen {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return false
		case <-timer:
			return false
		case <-c.accountChanged:
		}
	}
}

// connect dials, reads the banner, logs in and asks for the version. The
// password goes into the Login action only: never into an error or a log.
func (c *Conn) connect(ctx context.Context, username, password string) (*session, string, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	nc, err := c.cfg.Dial(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		return nil, "", fmt.Errorf("ami: connecting to %s: %w", c.cfg.Addr, err)
	}
	deadline, _ := ctx.Deadline()
	_ = nc.SetReadDeadline(deadline)
	br := bufio.NewReaderSize(nc, maxLine)
	banner, err := readLine(br)
	if err != nil {
		_ = nc.Close()
		return nil, "", fmt.Errorf("ami: reading the banner from %s: %w", c.cfg.Addr, err)
	}
	if !strings.HasPrefix(banner, bannerPrefix) {
		_ = nc.Close()
		return nil, "", fmt.Errorf("ami: %s is not AMI: banner %.64q", c.cfg.Addr, banner)
	}
	_ = nc.SetReadDeadline(time.Time{})
	s := newSession(nc, br, c.cfg.Log)

	login := action{name: "Login", fields: []field{{"Username", username}, {"Secret", password}, {"Events", "off"}}}
	if _, err := s.do(ctx, c.nextID(), login, nil); err != nil {
		s.close(errors.New("ami: login failed"))
		var re *responseError
		if errors.As(err, &re) {
			return nil, "", fmt.Errorf("ami: login as %s refused: %s", username, re.message)
		}
		if errors.Is(err, ErrUnsafeValue) {
			return nil, "", fmt.Errorf("ami: login as %q: %w", username, ErrUnsafeValue)
		}
		return nil, "", fmt.Errorf("ami: login as %s: %w", username, err)
	}
	version := ""
	if msgs, err := s.do(ctx, c.nextID(), action{name: "CoreSettings"}, nil); err != nil {
		c.cfg.Log.Info("AMI CoreSettings failed; the Asterisk version stays unknown", "error", err.Error())
	} else {
		version = msgs[0].get("AsteriskVersion")
	}
	return s, version, nil
}

func (c *Conn) nextID() string { return strconv.FormatUint(c.ids.Add(1), 10) }

// call runs one action on the session that is up.
func (c *Conn) call(ctx context.Context, a action, list *listSpec) ([]message, error) {
	if _, err := a.encode("0"); err != nil {
		return nil, err
	}
	s, err := c.session()
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	return s.do(ctx, c.nextID(), a, list)
}

// session is one TCP connection: a reader goroutine hands each message to
// the call waiting for its ActionID.
type session struct {
	conn net.Conn
	log  logr.Logger

	wmu sync.Mutex // one action is written at a time

	mu      sync.Mutex
	waiters map[string]*waiter
	err     error
	done    chan struct{} // closed once the reader has stopped
	once    sync.Once
}

type waiter struct {
	ch   chan message
	gone chan struct{} // closed when the call stops listening
}

func newSession(nc net.Conn, br *bufio.Reader, log logr.Logger) *session {
	s := &session{conn: nc, log: log, waiters: map[string]*waiter{}, done: make(chan struct{})}
	go s.read(br)
	return s
}

func (s *session) read(br *bufio.Reader) {
	for {
		m, err := readMessage(br)
		if err != nil {
			s.close(fmt.Errorf("ami: session ended: %w", err))
			return
		}
		if len(m) == 0 {
			continue
		}
		id := m.get("ActionID")
		s.mu.Lock()
		w := s.waiters[id]
		s.mu.Unlock()
		if w == nil {
			s.log.V(1).Info("AMI message for no pending action", "event", m.get("Event"), "response", m.get("Response"))
			continue
		}
		select {
		case w.ch <- m:
		case <-w.gone:
		case <-s.done:
			return
		}
	}
}

// close ends the session once, recording why.
func (s *session) close(why error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = why
		s.mu.Unlock()
		_ = s.conn.Close()
		close(s.done)
	})
}

func (s *session) cause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// listSpec describes a list action's reply: Success with "EventList:
// start", then item events, then the complete event.
type listSpec struct {
	item, complete string
	// empty is the Error message Asterisk answers when the list is empty
	// instead of an empty list ("No endpoints found").
	empty string
}

// do writes one action and waits for its reply: the response, or for a
// list, the item events up to the complete event.
func (s *session) do(ctx context.Context, id string, a action, list *listSpec) ([]message, error) {
	raw, err := a.encode(id)
	if err != nil {
		return nil, err
	}
	w := &waiter{ch: make(chan message, waiterBuffer), gone: make(chan struct{})}
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w (%w)", ErrNotConnected, s.err)
	}
	s.waiters[id] = w
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, id)
		s.mu.Unlock()
		close(w.gone)
	}()

	if err := s.write(ctx, raw); err != nil {
		s.close(fmt.Errorf("ami: writing %s: %w", a.name, err))
		return nil, fmt.Errorf("%w: writing %s: %w", ErrNotConnected, a.name, err)
	}

	var items []message
	started := false
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ami: %s: no reply: %w", a.name, ctx.Err())
		case <-s.done:
			return nil, fmt.Errorf("%w: %s: %w", ErrNotConnected, a.name, s.cause())
		case m := <-w.ch:
			if !started {
				if r := m.get("Response"); r != "" {
					if !strings.EqualFold(r, "Success") {
						msg := m.get("Message")
						if list != nil && list.empty != "" && strings.EqualFold(msg, list.empty) {
							return nil, nil
						}
						return nil, &responseError{action: a.name, response: r, message: msg}
					}
					if list == nil {
						return []message{m}, nil
					}
					started = true
					continue
				}
				// An event before the response: Asterisk does not do that,
				// but a list item is still an item.
				started = list != nil
				if !started {
					continue
				}
			}
			switch ev := m.get("Event"); {
			case strings.EqualFold(ev, list.complete):
				return items, nil
			case strings.EqualFold(ev, list.item):
				items = append(items, m)
			}
		}
	}
}

func (s *session) write(ctx context.Context, raw []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	deadline, _ := ctx.Deadline()
	_ = s.conn.SetWriteDeadline(deadline)
	defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	_, err := s.conn.Write(raw)
	return err
}

// stopped reports whether ctx is done: Run then returns nil.
func stopped(ctx context.Context) bool { return ctx.Err() != nil }
