// Package actions sends the controller's resync and restart actions to
// phones (W2.5): a Cisco service-control NOTIFY over AMI PJSIPNotify,
// quoting the phone's REGISTER Call-ID, as usecallmanager's
// [cisco-restart] stanza does (its Asterisk 20.20.0 patch, sip_notify.conf).
//
// The agent decides nothing: an action names a device, and the answer says
// only whether the NOTIFY went out. Whether the phone acted on it shows up
// later, in its fetches.
package actions

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/go-logr/logr"

	"github.com/ChristopherJMiller/patchbay/internal/agent/ami"
	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Config configures the actions.
type Config struct {
	// AMI sends the NOTIFY.
	AMI ami.Client
	// Applied is the live version and its index (apply.Applier.Applied).
	Applied func() (string, compile.Index, bool)
	// Timeout bounds one action; 0 means 10 s.
	Timeout time.Duration
	// Log receives each action and its outcome.
	Log logr.Logger
}

// DefaultTimeout is Config.Timeout's default.
const DefaultTimeout = 10 * time.Second

// Actions implements agent.Actions.
type Actions struct {
	cfg Config
}

// New returns the actions.
func New(cfg Config) *Actions {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Actions{cfg: cfg}
}

// The service-control NOTIFY (usecallmanager's [cisco-restart]): these
// headers in this order, then the body lines below with the action and
// the Call-ID.
var headers = []ami.Header{
	{Name: "Event", Value: "service-control"},
	{Name: "Subscription-State", Value: "active"},
	{Name: "Content-Type", Value: "text/plain"},
}

// stamps are the body's lines after RegisterCallId: zero version stamps,
// so the phone takes every part of its config as changed.
var stamps = []string{
	"ConfigVersionStamp={00000000-0000-0000-0000-000000000000}",
	"DialplanVersionStamp={00000000-0000-0000-0000-000000000000}",
	"SoftkeyVersionStamp={00000000-0000-0000-0000-000000000000}",
	"FeatureControlVersionStamp={00000000-0000-0000-0000-000000000000}",
	"HeadsetVersionStamp={0-0000000000}",
}

// Resync makes a phone re-read its config (action=restart).
func (a *Actions) Resync(ctx context.Context, req api.ActionRequest) api.ActionResponse {
	return a.send(ctx, req, api.ServiceControlRestart)
}

// Restart reboots a phone (action=reset).
func (a *Actions) Restart(ctx context.Context, req api.ActionRequest) api.ActionResponse {
	return a.send(ctx, req, api.ServiceControlReset)
}

func (a *Actions) send(ctx context.Context, req api.ActionRequest, action string) api.ActionResponse {
	log := a.cfg.Log.WithValues("device", req.Device, "action", action, "version", req.Version)
	refuse := func(why string) api.ActionResponse {
		log.Info("service-control NOTIFY not sent", "reason", why)
		return api.ActionResponse{Error: why}
	}
	var idx compile.Index
	ok := false
	if a.cfg.Applied != nil {
		_, idx, ok = a.cfg.Applied()
	}
	dev, known := idx.Devices[req.Device]
	if !ok || !known {
		return refuse("unknown device " + req.Device)
	}

	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	contacts, err := a.cfg.AMI.Contacts(ctx)
	if err != nil {
		return refuse(err.Error())
	}
	c, found := contactOf(contacts, req.Device, dev.Number)
	switch {
	case !found:
		return refuse("no live contact")
	case c.CallID == "":
		return refuse("no Call-ID")
	case !safeCallID(c.CallID):
		return refuse("unsafe Call-ID")
	}
	body := append([]string{"action=" + action, "RegisterCallId={" + c.CallID + "}"}, stamps...)
	n := ami.Notify{Endpoint: req.Device, Headers: append([]ami.Header(nil), headers...), Body: body}
	if err := a.cfg.AMI.Notify(ctx, n); err != nil {
		return refuse(err.Error())
	}
	log.Info("service-control NOTIFY sent", "contact", c.URI)
	return api.ActionResponse{Sent: true}
}

// contactOf finds the device's contact: by endpoint, else by its number as
// the AOR. With several, the one that expires last.
func contactOf(contacts []ami.Contact, id, number string) (ami.Contact, bool) {
	var best ami.Contact
	found := false
	pick := func(match func(ami.Contact) bool) {
		for _, c := range contacts {
			if match(c) && (!found || c.ExpiresAt.After(best.ExpiresAt)) {
				best, found = c, true
			}
		}
	}
	pick(func(c ami.Contact) bool { return c.Endpoint == id })
	if !found && number != "" {
		pick(func(c ami.Contact) bool { return c.Endpoint == "" && c.AOR == number })
	}
	return best, found
}

// safeCallID refuses a Call-ID that the NOTIFY body cannot quote as given:
// the braces around it, AMI's Variable separators and quoting, and any
// control or space character (a Call-ID is a SIP word).
func safeCallID(id string) bool {
	if strings.ContainsAny(id, "{},\"\\") {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
