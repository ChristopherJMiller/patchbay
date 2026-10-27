package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// registration is the phone's registration, guarded by runner.mu.
type registration struct {
	// callID and cseq are the REGISTERs' Call-ID and last CSeq: a phone
	// keeps one Call-ID for its registrations (RFC 3261 §10.2.4).
	callID string
	cseq   uint32
	// acc and cfgExpires are what the last successful REGISTER used: the
	// account, and the expiry the config asked for then.
	acc        account
	cfgExpires int
	// until is when the last successful registration lapses.
	until time.Time

	// refreshing is whether a refresher runs; stop ends it, done closes
	// when it has, kick makes it REGISTER at once.
	refreshing  bool
	stepExpires int // the Register step's own expiry (0: the config's)
	stop        context.CancelFunc
	done        chan struct{}
	kick        chan struct{}
}

// registerStep is a Register step.
func (r *runner) registerStep(ctx context.Context, st *script.Register) error {
	cfg := r.currentConfig()
	if cfg == nil {
		return errors.New("no config has been read: a register needs a fetchConfig first")
	}
	want := st.Expect
	if want == 0 {
		want = sip.StatusOK
	}
	acc := cfg.account()
	if st.Password != "" {
		acc.Password = secret(st.Password)
	}
	expires := st.Expires
	if expires == 0 {
		expires = cfg.Public.Expires
	}
	if want != sip.StatusOK {
		// A check that a REGISTER is refused: its own Call-ID, and the
		// registration (and its refresher) is left alone.
		status, _, err := r.sendRegister(ctx, acc, expires, 0, false)
		if err != nil {
			return err
		}
		if status != want {
			return fmt.Errorf("REGISTER answered %d, want %d", status, want)
		}
		return nil
	}
	r.stopRefreshing()
	status, granted, err := r.sendRegister(ctx, acc, expires, cfg.Public.Expires, true)
	if err != nil {
		return err
	}
	if status != sip.StatusOK {
		return fmt.Errorf("REGISTER answered %d, want 200", status)
	}
	if !st.NoRefresh {
		r.startRefreshing(granted, st.Expires)
	}
	return nil
}

// unregister is an Unregister step: stop refreshing, then REGISTER with
// Expires: 0 as the account last registered (else the config's).
func (r *runner) unregister(ctx context.Context) error {
	r.stopRefreshing()
	r.mu.Lock()
	acc, cfg := r.reg.acc, r.config
	r.mu.Unlock()
	if acc.User == "" {
		if cfg == nil {
			return errors.New("no config has been read: nothing to unregister")
		}
		acc = cfg.account()
	}
	status, _, err := r.sendRegister(ctx, acc, 0, 0, true)
	if err != nil {
		return err
	}
	if status != sip.StatusOK {
		return fmt.Errorf("REGISTER (unregister) answered %d, want 200", status)
	}
	return nil
}

// sendRegister sends one REGISTER and records it. With shared it is the
// registration's own (its Call-ID and next CSeq; a 200 updates when it
// lapses); otherwise it has a Call-ID of its own and changes nothing.
// cfgExpires is the config's expiry it was sent under. It returns the
// final status (0 when there was none) and the granted expiry.
func (r *runner) sendRegister(ctx context.Context, acc account, expires, cfgExpires int, shared bool) (int, int, error) {
	req := registerRequest{Account: acc, Expires: expires}
	if shared {
		r.regMu.Lock()
		defer r.regMu.Unlock()
		r.mu.Lock()
		if r.reg.callID == "" {
			r.reg.callID = sip.GenerateTagN(32)
		}
		r.reg.cseq++
		req.CallID, req.CSeq = r.reg.callID, r.reg.cseq
		r.mu.Unlock()
	}
	res, err := r.phone.registerAs(ctx, req)
	rec := script.RegistrationRecord{
		At: time.Now(), Expires: expires, AuthUser: acc.authUser(), PasswordSHA256: acc.Password.sha256Hex(),
	}
	if err == nil {
		rec.Status = res.Response.StatusCode
		if h := res.Response.CallID(); h != nil {
			rec.CallID = h.Value()
		}
	}
	r.mu.Lock()
	if shared && res.CSeq > r.reg.cseq {
		r.reg.cseq = res.CSeq
	}
	if rec.Status == sip.StatusOK && shared {
		if expires > 0 {
			rec.Granted = res.Granted
			r.reg.until = rec.At.Add(time.Duration(res.Granted) * time.Second)
			r.reg.acc, r.reg.cfgExpires = acc, cfgExpires
		} else {
			r.reg.until = time.Time{}
		}
	}
	r.state.Registrations = append(r.state.Registrations, rec)
	r.changedLocked()
	r.mu.Unlock()
	if err != nil {
		r.log.Warn("registration", "record", rec, "error", err.Error())
		return 0, 0, err
	}
	r.log.Info("registration", "record", rec)
	return rec.Status, rec.Granted, nil
}

// startRefreshing starts the refresher: it re-REGISTERs at refreshAfter
// the grant, and at once when kicked (a NOTIFY brought new credentials).
func (r *runner) startRefreshing(granted, stepExpires int) {
	ctx, cancel := context.WithCancel(r.ctx)
	done, kick := make(chan struct{}), make(chan struct{}, 1)
	r.mu.Lock()
	r.reg.refreshing, r.reg.stepExpires = true, stepExpires
	r.reg.stop, r.reg.done, r.reg.kick = cancel, done, kick
	r.changedLocked()
	r.mu.Unlock()
	go r.refresh(ctx, done, kick, r.refreshAfter(granted))
}

// stopRefreshing stops the refresher, if one runs, and waits for it: the
// registration then lapses at its expiry.
func (r *runner) stopRefreshing() {
	r.mu.Lock()
	if !r.reg.refreshing {
		r.mu.Unlock()
		return
	}
	stop, done := r.reg.stop, r.reg.done
	r.reg.refreshing = false
	r.changedLocked()
	r.mu.Unlock()
	stop()
	<-done
	r.log.Info("refreshing", "stopped", true)
}

// ciscoRegisterDelta is how long before its registration expires a 79x5
// refreshes it: the timerRegisterDelta its SEP file sets (cisco79x5's
// render). Linphone refreshes earlier, which half the grant stands for.
const ciscoRegisterDelta = 5 * time.Second

// refreshAfter is when to refresh a registration granted for granted
// seconds: in cisco mode ciscoRegisterDelta before it expires, as the real
// phone does (so the controller's grace for a refresh it has not seen yet,
// drivers.RegistrationGrace, is exercised), otherwise, or for a grant of
// twice the delta or less, half the grant; the retry delay when nothing
// was granted.
func (r *runner) refreshAfter(granted int) time.Duration {
	if granted <= 0 {
		return r.opts.RetryAfter
	}
	g := time.Duration(granted) * time.Second
	if r.sc.Mode == script.ModeCisco && g > 2*ciscoRegisterDelta {
		return g - ciscoRegisterDelta
	}
	return g / 2
}

func (r *runner) refresh(ctx context.Context, done chan<- struct{}, kick <-chan struct{}, next time.Duration) {
	defer close(done)
	timer := time.NewTimer(next)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-timer.C:
		}
		r.mu.Lock()
		cfg, expires := r.config, r.reg.stepExpires
		r.mu.Unlock()
		if expires == 0 {
			expires = cfg.Public.Expires
		}
		status, granted, err := r.sendRegister(ctx, cfg.account(), expires, cfg.Public.Expires, true)
		if ctx.Err() != nil {
			return
		}
		if err != nil || status != sip.StatusOK {
			timer.Reset(r.opts.RetryAfter)
			continue
		}
		timer.Reset(r.refreshAfter(granted))
	}
}

// reregisterIfChanged kicks the refresher when the config now holds other
// credentials, another server or another expiry than the registration.
func (r *runner) reregisterIfChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reg.refreshing || r.config == nil {
		return
	}
	if r.config.account() == r.reg.acc && r.config.Public.Expires == r.reg.cfgExpires {
		return
	}
	select {
	case r.reg.kick <- struct{}{}:
	default:
	}
}

// notifyEntry is a NOTIFY the phone received: its record, the dialog it
// came in (a REFER's progress), and whether its handling is over.
type notifyEntry struct {
	rec    script.NotifyRecord
	callID string
	done   bool
}

// onNotify handles a NOTIFY the phone has answered 200: it is recorded and,
// in cisco mode, a service-control NOTIFY asking for action=restart or
// action=reset restarts the phone as a 79x5 restarts (restart), and a
// check-sync makes it fetch its config again and, if the credentials,
// server or expiry changed while it refreshes, REGISTER with the new ones
// at once. Its handling is over (waitNotify) once the phone is back.
func (r *runner) onNotify(req *sip.Request) {
	e := &notifyEntry{
		rec: script.NotifyRecord{
			At: time.Now(), Event: eventName(req),
			SubscriptionState: subscriptionState(req), Body: string(req.Body()),
		},
		callID: req.CallID().Value(),
	}
	r.mu.Lock()
	r.notifies = append(r.notifies, e)
	r.changedLocked()
	r.mu.Unlock()
	if r.sc.Mode != script.ModeCisco || !asksRefetch(e.rec.Event, e.rec.Body) {
		r.finishNotify(e, false)
		return
	}
	if asksRestart(e.rec.Event, e.rec.Body) {
		go func() {
			r.restart(r.ctx)
			r.finishNotify(e, true)
		}()
		return
	}
	go func() {
		if _, err := r.fetchConfig(r.ctx); err != nil {
			r.log.Warn("refetch", "error", err.Error())
		}
		r.finishNotify(e, true)
		r.reregisterIfChanged()
	}()
}

// restart is what a 79x5 does on a service-control restart or reset (and
// what the Desk 7945 showed Asterisk, 2026-10-27: its contact removed "due
// to request", then a new one): it unregisters, boots, fetches its config
// and registers anew under a Call-ID it has not used, which the agent
// takes for a restart (its screen is idle; observe.Store.Poll). A phone
// that was not refreshing a registration only fetches its config again.
func (r *runner) restart(ctx context.Context) {
	r.mu.Lock()
	registered, stepExpires := r.reg.refreshing, r.reg.stepExpires
	r.mu.Unlock()
	if registered {
		// unregister stops the refresher first. Its REGISTER may be refused
		// (the credentials changed under it); the phone boots anyway.
		if err := r.unregister(ctx); err != nil {
			r.log.Warn("restart: unregister", "error", err.Error())
		}
		r.regMu.Lock()
		r.mu.Lock()
		r.reg.callID, r.reg.cseq = "", 0 // a booted phone's REGISTERs are a new series
		r.mu.Unlock()
		r.regMu.Unlock()
	}
	if _, err := r.fetchConfig(ctx); err != nil {
		r.log.Warn("refetch", "error", err.Error())
	}
	cfg := r.currentConfig()
	if !registered || cfg == nil || ctx.Err() != nil {
		return
	}
	expires := stepExpires
	if expires == 0 {
		expires = cfg.Public.Expires
	}
	status, granted, err := r.sendRegister(ctx, cfg.account(), expires, cfg.Public.Expires, true)
	if err != nil || status != sip.StatusOK {
		granted = 0 // the refresher retries after RetryAfter
	}
	r.startRefreshing(granted, stepExpires)
}

func (r *runner) finishNotify(e *notifyEntry, refetched bool) {
	r.mu.Lock()
	e.rec.Refetched, e.done = refetched, true
	rec := e.rec
	r.changedLocked()
	r.mu.Unlock()
	r.log.Info("notify", "record", rec)
}

// asksRefetch is whether a NOTIFY makes a 79x5 read its config again: a
// service-control one with action=restart or action=reset (the agent's
// resync and restart), or a check-sync (the alternative §8 keeps on the
// bench).
func asksRefetch(event, body string) bool {
	switch event {
	case "check-sync":
		return true
	case "service-control":
		for line := range strings.Lines(body) {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			if strings.EqualFold(strings.TrimSpace(k), "action") {
				v = strings.ToLower(strings.TrimSpace(v))
				return v == "restart" || v == "reset"
			}
		}
	}
	return false
}

// asksRestart is whether a NOTIFY restarts a 79x5: a service-control one
// with action=restart or action=reset (a check-sync only refetches).
func asksRestart(event, body string) bool {
	return event == "service-control" && asksRefetch(event, body)
}

func (r *runner) waitNotify(ctx context.Context, w *script.WaitNotify) error {
	event := w.Event
	if event == "" {
		event = "service-control"
	}
	r.mu.Lock()
	from := len(r.notifies)
	r.mu.Unlock()
	return r.waitUntil(ctx, w.Within.D(), "NOTIFY with Event "+event, func() bool {
		for _, n := range r.notifies[from:] {
			if n.rec.Event == event && n.done {
				return true
			}
		}
		return false
	})
}

// sipfragStatus is the status line of a REFER NOTIFY's message/sipfrag
// body ("SIP/2.0 200 OK" is 200), or 0.
func sipfragStatus(body string) int {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "SIP/") {
		return 0
	}
	n, _ := strconv.Atoi(fields[1])
	return n
}
