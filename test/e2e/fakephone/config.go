package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The config documents a phone reads: the SEP<MAC>.cnf.xml a 79x5 TFTPs
// (internal/drivers/cisco79x5) and the lpconfig XML Linphone GETs from its
// provisioning URL (internal/drivers/softphone). fakephone parses them with
// its own encoding/xml structs, never the drivers' code, so it reads what
// the renderers wrote the way a phone would; config_test.go runs both
// parsers on the drivers' render goldens.

// redacted is how a secret prints.
const redacted = "<redacted>"

// secret is a password: it prints, marshals and logs as "<redacted>", so no
// format verb, log line or State can carry it by accident.
type secret string

func (secret) String() string               { return redacted }
func (secret) GoString() string             { return strconv.Quote(redacted) }
func (secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(redacted)), nil } //nolint:unparam // json.Marshaler
func (secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s secret) reveal() string             { return string(s) }
func (s secret) sha256Hex() string          { return hex.EncodeToString(sha256Of([]byte(s))) }
func (secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }

// account is what a phone registers and calls as.
type account struct {
	// Server is the PBX: SIP goes to Server over Transport ("tcp", "udp").
	Server    netip.AddrPort
	Transport string
	// User is the SIP user (the number: AOR, From, Contact); AuthUser the
	// digest username (the device ID), "" meaning User.
	User     string
	AuthUser string
	Password secret
}

// authUser is the digest username.
func (a account) authUser() string {
	if a.AuthUser != "" {
		return a.AuthUser
	}
	return a.User
}

// deviceConfig is a parsed config document: what State shows (Public), and
// the account it gives, whose password State never shows.
type deviceConfig struct {
	Public    script.Config
	Server    netip.AddrPort
	Transport string
	Password  secret
}

func (c deviceConfig) String() string {
	return fmt.Sprintf("config %s version=%q user=%s authUser=%s server=%s/%s expires=%d password=%s",
		c.Public.Source, c.Public.Version, c.Public.User, c.Public.AuthUser, c.Server, c.Transport,
		c.Public.Expires, redacted)
}

func (c deviceConfig) GoString() string     { return c.String() }
func (c deviceConfig) LogValue() slog.Value { return slog.AnyValue(c.Public) }

// account is the account the config gives.
func (c *deviceConfig) account() account {
	return account{
		Server: c.Server, Transport: c.Transport,
		User: c.Public.User, AuthUser: c.Public.AuthUser, Password: c.Password,
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// errPlaceholder: the agent resolves every @@secret:…@@ before a phone sees
// a file, so one reaching a phone is a bug the e2e suite must see.
var errPlaceholder = errors.New("holds an unresolved secret placeholder")

// checkPassword refuses an empty or unresolved password; the error names
// the field only.
func checkPassword(field, value string) (secret, error) {
	switch {
	case value == "":
		return "", fmt.Errorf("%s is empty", field)
	case strings.Contains(value, "@@secret:"):
		return "", fmt.Errorf("%s %w", field, errPlaceholder)
	}
	return secret(value), nil
}

// sepXML is the part of SEP<MAC>.cnf.xml a phone needs to register.
type sepXML struct {
	XMLName xml.Name    `xml:"device"`
	Members []sepMember `xml:"devicePool>callManagerGroup>members>member"`
	Profile struct {
		TimerRegisterExpires string    `xml:"sipStack>timerRegisterExpires"`
		PhoneLabel           string    `xml:"phoneLabel"`
		Lines                []sepLine `xml:"sipLines>line"`
	} `xml:"sipProfile"`
	LoadInformation        string `xml:"loadInformation"`
	VersionStamp           string `xml:"versionStamp"`
	TransportLayerProtocol string `xml:"transportLayerProtocol"`
}

type sepMember struct {
	Priority        string `xml:"priority,attr"`
	SIPPort         string `xml:"callManager>ports>sipPort"`
	ProcessNodeName string `xml:"callManager>processNodeName"`
}

type sepLine struct {
	Button       string `xml:"button,attr"`
	Name         string `xml:"name"`
	DisplayName  string `xml:"displayName"`
	AuthName     string `xml:"authName"`
	AuthPassword string `xml:"authPassword"`
}

// transportLayerProtocol's values: 1 TCP, 2 UDP (what the cisco79x5 driver
// renders); anything else is refused.
var sepTransports = map[string]string{"1": "tcp", "2": "udp"}

// parseSEP reads a SEP<MAC>.cnf.xml named source: the first call manager's
// processNodeName and ports/sipPort, sipStack/timerRegisterExpires,
// phoneLabel, line 1's name, authName, authPassword and displayName,
// loadInformation, versionStamp and transportLayerProtocol.
func parseSEP(doc []byte, source string) (*deviceConfig, error) {
	var x sepXML
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&x); err != nil {
		return nil, fmt.Errorf("%s: not a <device> XML document: %w", source, err)
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	var member sepMember
	for i, m := range x.Members {
		if i == 0 || strings.TrimSpace(m.Priority) == "0" {
			member = m
			if strings.TrimSpace(m.Priority) == "0" {
				break
			}
		}
	}
	server, err := addrPort(member.ProcessNodeName, member.SIPPort)
	if err != nil {
		bad("callManager processNodeName/sipPort: %w", err)
	}
	expires, err := positive(x.Profile.TimerRegisterExpires)
	if err != nil {
		bad("sipStack/timerRegisterExpires: %w", err)
	}
	transport, ok := sepTransports[strings.TrimSpace(x.TransportLayerProtocol)]
	if !ok {
		bad("transportLayerProtocol %q is not 1 (TCP) or 2 (UDP)", strings.TrimSpace(x.TransportLayerProtocol))
	}
	var line sepLine
	for i, l := range x.Profile.Lines {
		if i == 0 || strings.TrimSpace(l.Button) == "1" {
			line = l
			if strings.TrimSpace(l.Button) == "1" {
				break
			}
		}
	}
	user, authName := strings.TrimSpace(line.Name), strings.TrimSpace(line.AuthName)
	if user == "" {
		bad("line 1 has no name")
	}
	if authName == "" {
		bad("line 1 has no authName")
	}
	password, err := checkPassword("line 1 authPassword", line.AuthPassword)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", source, errors.Join(errs...))
	}
	return &deviceConfig{
		Public: script.Config{
			Source:          source,
			SHA256:          hex.EncodeToString(sha256Of(doc)),
			Version:         strings.TrimSpace(x.VersionStamp),
			PhoneLabel:      x.Profile.PhoneLabel,
			LineLabel:       line.DisplayName,
			LoadInformation: strings.TrimSpace(x.LoadInformation),
			User:            user,
			AuthUser:        authName,
			PasswordSHA256:  password.sha256Hex(),
			Proxy:           server.String(),
			Transport:       transport,
			Expires:         expires,
		},
		Server: server, Transport: transport, Password: password,
	}, nil
}

// addrPort joins an IP address and a port; a host name is refused (the
// compiled config always carries the PBX's address).
func addrPort(host, port string) (netip.AddrPort, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q is not an IP address", strings.TrimSpace(host))
	}
	p, err := strconv.ParseUint(strings.TrimSpace(port), 10, 16)
	if err != nil || p == 0 {
		return netip.AddrPort{}, fmt.Errorf("port %q is not 1-65535", strings.TrimSpace(port))
	}
	return netip.AddrPortFrom(a, uint16(p)), nil
}

// positive parses a positive number of seconds.
func positive(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a positive number", strings.TrimSpace(s))
	}
	return n, nil
}

// lpXML is Linphone's remote provisioning document: sections of entries.
type lpXML struct {
	XMLName  xml.Name `xml:"config"`
	Sections []struct {
		Name    string `xml:"name,attr"`
		Entries []struct {
			Name      string `xml:"name,attr"`
			Overwrite string `xml:"overwrite,attr"`
			Value     string `xml:",chardata"`
		} `xml:"entry"`
	} `xml:"section"`
}

type lpEntry struct {
	value     string
	overwrite bool
}

// parseLPConfig reads the lpconfig XML: proxy_0/reg_proxy
// (<sip:host:port;transport=…>), reg_identity (<sip:user@host>),
// reg_expires, auth_info_0/username, userid and passwd, whether
// misc/config-uri is cleared (present, empty and overwritten), and the
// contacts: misc/contacts-vcard-list, downloaded at start unless
// misc/fetch_contacts_vcard_list_at_startup is 0 (liblinphone's default
// is 1).
func parseLPConfig(doc []byte) (*deviceConfig, error) {
	var x lpXML
	if err := xml.NewDecoder(bytes.NewReader(doc)).Decode(&x); err != nil {
		return nil, fmt.Errorf("provisioning: not a <config> XML document: %w", err)
	}
	entries := map[string]lpEntry{}
	for _, s := range x.Sections {
		for _, e := range s.Entries {
			entries[s.Name+"/"+e.Name] = lpEntry{value: e.Value, overwrite: e.Overwrite == "true"}
		}
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	server, transport, err := parseSIPProxy(entries["proxy_0/reg_proxy"].value)
	if err != nil {
		bad("proxy_0/reg_proxy: %w", err)
	}
	identity := strings.TrimSpace(entries["proxy_0/reg_identity"].value)
	user, _, ok := strings.Cut(strings.TrimPrefix(strings.Trim(identity, "<>"), "sip:"), "@")
	if !ok || user == "" || !strings.HasPrefix(strings.Trim(identity, "<>"), "sip:") {
		bad("proxy_0/reg_identity %q is not <sip:user@host>", identity)
	}
	expires, err := positive(entries["proxy_0/reg_expires"].value)
	if err != nil {
		bad("proxy_0/reg_expires: %w", err)
	}
	authUser := strings.TrimSpace(entries["auth_info_0/userid"].value)
	if authUser == "" {
		authUser = strings.TrimSpace(entries["auth_info_0/username"].value)
	}
	if authUser == "" {
		bad("auth_info_0 has neither userid nor username")
	}
	password, err := checkPassword("auth_info_0/passwd", entries["auth_info_0/passwd"].value)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("provisioning: %w", errors.Join(errs...))
	}
	uri, present := entries["misc/config-uri"]
	atStartup := true
	if e, ok := entries["misc/fetch_contacts_vcard_list_at_startup"]; ok {
		n, _ := strconv.Atoi(strings.TrimSpace(e.value)) // linphone_config_get_bool: a number, else 0
		atStartup = n != 0
	}
	return &deviceConfig{
		Public: script.Config{
			Source:            "provisioning",
			SHA256:            hex.EncodeToString(sha256Of(doc)),
			User:              user,
			AuthUser:          authUser,
			PasswordSHA256:    password.sha256Hex(),
			Proxy:             server.String(),
			Transport:         transport,
			Expires:           expires,
			ConfigURICleared:  present && strings.TrimSpace(uri.value) == "" && uri.overwrite,
			ContactsURL:       strings.TrimSpace(entries["misc/contacts-vcard-list"].value),
			ContactsAtStartup: atStartup,
		},
		Server: server, Transport: transport, Password: password,
	}, nil
}

// parseSIPProxy reads <sip:host[:port][;transport=udp|tcp]>; the port
// defaults to 5060 and the transport to udp, as in Linphone.
func parseSIPProxy(v string) (netip.AddrPort, string, error) {
	v = strings.TrimSpace(v)
	rest, ok := strings.CutPrefix(strings.Trim(v, "<>"), "sip:")
	if !ok {
		return netip.AddrPort{}, "", fmt.Errorf("%q is not <sip:host:port;transport=…>", v)
	}
	hostPort, params, _ := strings.Cut(rest, ";")
	transport := "udp"
	for p := range strings.SplitSeq(params, ";") {
		if k, val, _ := strings.Cut(p, "="); strings.EqualFold(k, "transport") {
			transport = strings.ToLower(val)
		}
	}
	if transport != "udp" && transport != "tcp" {
		return netip.AddrPort{}, "", fmt.Errorf("transport %q is not udp or tcp", transport)
	}
	if _, err := netip.ParseAddr(hostPort); err == nil {
		hostPort += ":5060"
	}
	ap, err := netip.ParseAddrPort(hostPort)
	if err != nil {
		return netip.AddrPort{}, "", fmt.Errorf("%q is not an IP address and port", hostPort)
	}
	return ap, transport, nil
}
