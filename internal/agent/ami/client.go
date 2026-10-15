package ami

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// The list actions: their item event, complete event, and the Error message
// Asterisk 22 answers when there is nothing to list (instead of an empty
// list; checked by TestRealAsterisk).
var (
	listEndpoints = &listSpec{item: "EndpointList", complete: "EndpointListComplete", empty: "No endpoints found"}
	listDialplan  = &listSpec{item: "ListDialplan", complete: "ShowDialPlanComplete"}
	listContacts  = &listSpec{item: "ContactList", complete: "ContactListComplete", empty: "No Contacts found"}
	listChannels  = &listSpec{item: "CoreShowChannel", complete: "CoreShowChannelsComplete"}
)

// Reload reloads one module (ModuleLoad, LoadType reload). Its error does
// not repeat the module's name: the caller says which reload failed.
func (c *Conn) Reload(ctx context.Context, module string) error {
	_, err := c.call(ctx, action{name: "ModuleLoad", fields: []field{{"Module", module}, {"LoadType", "reload"}}}, nil)
	return err
}

// Endpoints lists every PJSIP endpoint's name, sorted.
func (c *Conn) Endpoints(ctx context.Context) ([]string, error) {
	items, err := c.call(ctx, action{name: "PJSIPShowEndpoints"}, listEndpoints)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, m := range items {
		names = append(names, m.get("ObjectName"))
	}
	slices.Sort(names)
	return names, nil
}

// DialplanContexts lists every dialplan context, sorted and unique.
func (c *Conn) DialplanContexts(ctx context.Context) ([]string, error) {
	items, err := c.call(ctx, action{name: "ShowDialPlan"}, listDialplan)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, m := range items {
		if ctxName := m.get("Context"); ctxName != "" {
			names = append(names, ctxName)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// Contacts lists every registered contact, in Asterisk's order.
func (c *Conn) Contacts(ctx context.Context) ([]Contact, error) {
	items, err := c.call(ctx, action{name: "PJSIPShowContacts"}, listContacts)
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(items))
	for _, m := range items {
		out = append(out, contactOf(m))
	}
	return out, nil
}

// contactOf reads one ContactList event.
func contactOf(m message) Contact {
	c := Contact{
		Endpoint:  m.get("Endpoint"),
		AOR:       m.get("AOR"),
		URI:       m.get("Uri"),
		UserAgent: m.get("UserAgent"),
		CallID:    m.get("CallId"),
		Qualify:   qualifyOf(m.get("Status")),
	}
	if c.AOR == "" {
		// A contact's sorcery ID is "<aor>;@<hash>".
		c.AOR, _, _ = strings.Cut(m.get("ObjectName"), ";@")
	}
	c.Address = canonicalIP(m.get("ViaAddr"))
	if c.Address == "" {
		c.Address = canonicalIP(uriHost(c.URI))
	}
	if secs, err := strconv.ParseInt(m.get("ExpirationTime"), 10, 64); err == nil && secs > 0 {
		c.ExpiresAt = time.Unix(secs, 0).UTC()
	}
	return c
}

func qualifyOf(status string) api.QualifyStatus {
	switch {
	case strings.EqualFold(status, "Reachable"):
		return api.QualifyReachable
	case strings.EqualFold(status, "Unreachable"):
		return api.QualifyUnreachable
	}
	return api.QualifyUnknown
}

// canonicalIP is s as a canonical IP address (unmapped, no zone), or "".
func canonicalIP(s string) string {
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil || a.Zone() != "" {
		return ""
	}
	return a.Unmap().String()
}

// uriHost is a SIP URI's host: sip:100@192.168.20.5:5060;transport=tcp
// gives 192.168.20.5, sip:100@[2001:db8::1]:5060 gives 2001:db8::1.
func uriHost(uri string) string {
	s := strings.Trim(strings.TrimSpace(uri), "<>")
	for _, scheme := range []string{"sip:", "sips:"} {
		s = strings.TrimPrefix(s, scheme)
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, ";?>"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i >= 0 {
			return s[1:i]
		}
		return ""
	}
	host, _, _ := strings.Cut(s, ":")
	return host
}

// Channels lists every channel up now.
func (c *Conn) Channels(ctx context.Context) ([]Channel, error) {
	items, err := c.call(ctx, action{name: "CoreShowChannels"}, listChannels)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(items))
	for _, m := range items {
		name := m.get("Channel")
		out = append(out, Channel{Name: name, Endpoint: channelEndpoint(name)})
	}
	return out, nil
}

// channelEndpoint is the PJSIP endpoint of a channel name: PJSIP/phone-desk-0000002a
// gives phone-desk; a channel of another technology gives "".
func channelEndpoint(name string) string {
	rest, ok := strings.CutPrefix(name, "PJSIP/")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 {
		return rest
	}
	if _, err := strconv.ParseUint(rest[i+1:], 16, 64); err != nil {
		return rest
	}
	return rest[:i]
}

// Notify sends one NOTIFY (PJSIPNotify): Endpoint, then one Variable per
// header, then one "Content=<line>" Variable per body line, in order.
func (c *Conn) Notify(ctx context.Context, n Notify) error {
	if n.Endpoint == "" {
		return fmt.Errorf("ami: PJSIPNotify: no endpoint")
	}
	fields := []field{{"Endpoint", n.Endpoint}}
	for _, h := range n.Headers {
		if h.Name == "" || strings.ContainsAny(h.Name, "=:") || !variableSafe(h.Name) || !variableSafe(h.Value) {
			return fmt.Errorf("%w: header %q", ErrUnsafeValue, h.Name)
		}
		fields = append(fields, field{"Variable", h.Name + "=" + h.Value})
	}
	for i, line := range n.Body {
		if !variableSafe(line) {
			return fmt.Errorf("%w: body line %d", ErrUnsafeValue, i+1)
		}
		fields = append(fields, field{"Variable", "Content=" + line})
	}
	_, err := c.call(ctx, action{name: "PJSIPNotify", fields: fields}, nil)
	return err
}

// variableSafe reports whether Asterisk carries s in a "Variable:" field as
// given: it splits the field at commas and unquotes it.
func variableSafe(s string) bool { return !strings.ContainsAny(s, "\r\n,\"\\") }
