package softphone

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// linphoneCodecs maps an Asterisk codec name (drivers.KnownCodecs) to the
// payload type Linphone lists it as: its MIME name and RTP clock rate
// (ortp/src/avprofile.c; G.722's RTP clock is 8000 Hz by RFC 3551).
var linphoneCodecs = map[string]struct {
	mime string
	rate int
}{
	"g722": {"G722", 8000},
	"ulaw": {"PCMU", 8000},
	"alaw": {"PCMA", 8000},
}

// Render produces the softphone's Linphone remote-provisioning document,
// provisioning/<ID>.xml (see the package documentation for the format and
// where each key was checked). The document depends on the device's
// number, ID and password placeholder and on the install's PBX address, SIP
// port and contacts URL only: never on the label (a relabel must not ask
// anyone to scan again), the token or dev.ConfigVersion, so Compile's two
// passes render the same bytes.
//
// An error wraps drivers.ErrInvalidSpec when dev.Model is not
// drivers.ModelSoftphone. A missing or unusable password placeholder, PBX
// address, SIP port or contacts URL, or a malformed number or ID, is a
// plain error: the caller (Compile) or the CRD schema should have
// prevented it.
func (*Driver) Render(dev drivers.Device, ctx drivers.RenderContext) (drivers.Files, error) {
	if dev.Model != drivers.ModelSoftphone {
		return nil, fmt.Errorf("%w: softphone %s: model %q is not %q",
			drivers.ErrInvalidSpec, dev.Name, dev.Model, drivers.ModelSoftphone)
	}
	if err := checkRenderInputs(dev, ctx); err != nil {
		return nil, fmt.Errorf("softphone %s: %w", dev.Name, err)
	}
	doc, err := provisioningDocument(dev, ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("softphone %s: %w", dev.Name, err)
	}
	return drivers.Files{ProvisioningFile(dev.ID): doc}, nil
}

var (
	digits = regexp.MustCompile(`^[0-9]+$`)
	// portableID is a device ID usable as a bundle file name (compile.TreeOf
	// has the full rule; DeviceID always satisfies it).
	portableID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	// hostName is an RFC 1123 host name.
	hostName = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?)*$`)
)

func checkRenderInputs(dev drivers.Device, ctx drivers.RenderContext) error {
	var errs []error
	if !portableID.MatchString(dev.ID) || len(dev.ID) > drivers.DeviceIDMaxLength {
		errs = append(errs, fmt.Errorf("device ID %q is not a portable name of at most %d characters",
			dev.ID, drivers.DeviceIDMaxLength))
	}
	if !digits.MatchString(dev.Number) {
		errs = append(errs, fmt.Errorf("number %q is not digits", dev.Number))
	}
	switch {
	case ctx.SIPPassword == "":
		errs = append(errs, errors.New("no password placeholder"))
	case strings.ContainsFunc(ctx.SIPPassword, needsEscape):
		// The agent finds the placeholder byte for byte; an escaped one
		// would never be resolved.
		errs = append(errs, fmt.Errorf("password placeholder %q has a character XML would escape", ctx.SIPPassword))
	}
	if err := checkHost(ctx.PBXAddress); err != nil {
		errs = append(errs, err)
	}
	if ctx.SIPPort < 1 || ctx.SIPPort > 65535 {
		errs = append(errs, fmt.Errorf("SIP port %d is not 1-65535", ctx.SIPPort))
	}
	if u, err := url.Parse(ctx.ContactsURL); err != nil || u.Scheme != "http" || u.Host == "" ||
		strings.ContainsFunc(ctx.ContactsURL, needsEscape) {
		errs = append(errs, fmt.Errorf("contacts URL %q is not a plain http URL", ctx.ContactsURL))
	}
	return errors.Join(errs...)
}

func needsEscape(r rune) bool {
	return r <= ' ' || r == 0x7f || strings.ContainsRune(`&<>"'`, r)
}

// checkHost accepts an IPv4 address or a host name: what can stand as the
// host of a SIP URI without brackets or escaping.
func checkHost(addr string) error {
	if addr == "" {
		return errors.New("no PBX address")
	}
	if a, err := netip.ParseAddr(addr); err == nil {
		if !a.Is4() {
			return fmt.Errorf("PBX address %q is not IPv4", addr)
		}
		return nil
	}
	if len(addr) > 253 || !hostName.MatchString(addr) {
		return fmt.Errorf("PBX address %q is not an IPv4 address or host name", addr)
	}
	return nil
}

// lpConfig is Linphone's XML configuration document: <config>, then
// <section name="..."> holding <entry name="..." overwrite="true">value</entry>.
// The namespace is lpconfig.xsd's (liblinphone share/xml/lpconfig.xsd).
type lpConfig struct {
	XMLName  xml.Name    `xml:"http://www.linphone.org/xsds/lpconfig.xsd config"`
	Sections []lpSection `xml:"section"`
}

type lpSection struct {
	Name    string    `xml:"name,attr"`
	Entries []lpEntry `xml:"entry"`
}

type lpEntry struct {
	Name string `xml:"name,attr"`
	// Overwrite is always true: Linphone keeps an existing value otherwise.
	Overwrite bool   `xml:"overwrite,attr"`
	Value     string `xml:",chardata"`
}

// set is an entry that replaces the key's value; an empty value deletes
// the key (liblinphone's linphone_config_set_string).
func set(name, value string) lpEntry { return lpEntry{Name: name, Overwrite: true, Value: value} }

// unset deletes the key.
func unset(name string) lpEntry { return set(name, "") }

func provisioningDocument(dev drivers.Device, ctx drivers.RenderContext, p drivers.Profile) ([]byte, error) {
	if p.Endpoint.Transport != drivers.SIPTransportUDP {
		return nil, fmt.Errorf("profile transport %q: the document only renders udp", p.Endpoint.Transport)
	}
	pbx := ctx.PBXAddress
	sections := []lpSection{
		{Name: "sip", Entries: []lpEntry{
			// The account below is the default one.
			set("default_proxy", "0"),
			// DTMF as RFC 4733 events (Asterisk's dtmf_mode = rfc4733), not
			// SIP INFO: both are Linphone's defaults, written so a changed
			// install is put back.
			set("use_rfc2833", "1"),
			set("use_info", "0"),
		}},
		{Name: "proxy_0", Entries: []lpEntry{
			set("reg_proxy", fmt.Sprintf("<sip:%s:%d;transport=%s>", pbx, ctx.SIPPort, p.Endpoint.Transport)),
			// An account Linphone already had in slot 0 may carry an
			// outbound route, which would send the REGISTER elsewhere.
			unset("reg_route"),
			// No display name: see the package documentation.
			set("reg_identity", fmt.Sprintf("<sip:%s@%s>", dev.Number, pbx)),
			set("reg_expires", strconv.Itoa(int(p.Endpoint.RegistrationExpiry/time.Second))),
			set("reg_sendregister", "1"),
			set("publish", "0"),
		}},
		{Name: "auth_info_0", Entries: []lpEntry{
			// username is the SIP user (the number, the From user Linphone
			// matches a challenge with); userid is the digest username, the
			// device ID, which identifies the PJSIP endpoint.
			set("username", dev.Number),
			set("userid", dev.ID),
			set("passwd", ctx.SIPPassword),
			set("domain", pbx),
		}},
	}
	for i, c := range p.Endpoint.Codecs {
		lc, ok := linphoneCodecs[c]
		if !ok {
			return nil, fmt.Errorf("profile codec %q has no Linphone payload type", c)
		}
		sections = append(sections, lpSection{Name: "audio_codec_" + strconv.Itoa(i), Entries: []lpEntry{
			set("mime", lc.mime),
			set("rate", strconv.Itoa(lc.rate)),
			set("channels", "1"),
			set("enabled", "1"),
			unset("recv_fmtp"),
			unset("bitrate"),
		}})
	}
	sections = append(sections,
		// Linphone reads audio_codec_N up to the first section without a
		// mime: end the list here, so no older section can list (and
		// disable) the codecs above again. Linphone adds its other codecs
		// after them, as it ships them.
		lpSection{Name: "audio_codec_" + strconv.Itoa(len(p.Endpoint.Codecs)), Entries: []lpEntry{unset("mime")}},
		lpSection{Name: "video", Entries: []lpEntry{
			set("capture", "0"),
			set("display", "0"),
			set("automatically_initiate", "0"),
			set("automatically_accept", "0"),
		}},
		lpSection{Name: "misc", Entries: []lpEntry{
			// The URL this document came from is spent once it is served:
			// forget it, so Linphone does not fetch it again at every start.
			unset("config-uri"),
			set("transient_provisioning", "1"),
			// The PBX's directory as read-only contacts, downloaded again
			// at every start (compile.ContactsFile).
			set("contacts-vcard-list", ctx.ContactsURL),
			set("fetch_contacts_vcard_list_at_startup", "1"),
		}},
	)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(lpConfig{Sections: sections}); err != nil {
		return nil, fmt.Errorf("encoding the provisioning document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding the provisioning document: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
