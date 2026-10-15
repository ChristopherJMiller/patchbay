package scrape

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// The three pages' XML shapes. They are written from the documented field
// lists of the 79x5's XML twins of its web pages
// (docs/research/2026-10-14-cisco-7945-capabilities.md §6; the plan's
// W2.7), never copied from a phone, and hold only what this package reads.
// The e2e fake phone serves them (MarshalPage), so it serves exactly what
// the parsers read. Which element names a real 79x5 uses is a bench check
// (docs/plans/release-a.md §8): the parsers match names case-insensitively,
// accept the aliases listed on each field, and accept the root element
// with or without a trailing "X".

// DeviceInformationX is /DeviceInformationX.
type DeviceInformationX struct {
	XMLName xml.Name `xml:"DeviceInformation"`
	// VersionID is the running load's ID (SIP45.9-4-2SR4-3S).
	VersionID string `xml:"versionID"`
	// AppLoadID is the running application image (jar45sip.9-4-2SR4-3.sbn).
	AppLoadID string `xml:"appLoadID"`
	// BootLoadID is the boot load.
	BootLoadID       string `xml:"bootLoadID"`
	HardwareRevision string `xml:"hardwareRevision"`
	SerialNumber     string `xml:"serialNumber"`
	ModelNumber      string `xml:"modelNumber"`
}

// NetworkConfigurationX is /NetworkConfigurationX.
type NetworkConfigurationX struct {
	XMLName xml.Name `xml:"NetworkConfiguration"`
	// AdminVLANID is the Admin. VLAN ID, element AdminVLANId (also read as
	// AdminVLANID: names are matched case-insensitively).
	AdminVLANID string `xml:"AdminVLANId"`
	// CallManager1 is the "Unified CM 1" line, e.g. "192.168.0.240  Active"
	// (also read as UnifiedCM1 or CUCM1).
	CallManager1 string `xml:"CallManager1"`
}

// DeviceLogX is /DeviceLogX: the status messages, oldest first.
type DeviceLogX struct {
	XMLName xml.Name `xml:"DeviceLog"`
	Status  []string `xml:"status"`
}

// MarshalPage renders one of the page shapes as a UTF-8 XML document, as
// the e2e fake phone serves it.
func MarshalPage(v any) ([]byte, error) {
	b, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("scrape: marshalling a page: %w", err)
	}
	return append([]byte(xml.Header), append(b, '\n')...), nil
}

// Root element names, lower case.
const (
	rootDeviceInformation    = "deviceinformation"
	rootNetworkConfiguration = "networkconfiguration"
	rootDeviceLog            = "devicelog"
)

// Field element names, lower case, each with its aliases in order.
var (
	fieldVersionID        = []string{"versionid"}
	fieldAppLoadID        = []string{"apploadid"}
	fieldBootLoadID       = []string{"bootloadid"}
	fieldHardwareRevision = []string{"hardwarerevision"}
	fieldSerialNumber     = []string{"serialnumber"}
	fieldModelNumber      = []string{"modelnumber"}
	fieldAdminVLAN        = []string{"adminvlanid"} // AdminVLANId and AdminVLANID alike
	fieldCallManager1     = []string{"callmanager1", "unifiedcm1", "cucm1"}
	elementStatus         = "status"
)

// maxLogLines is how many of the newest /DeviceLogX lines a scrape keeps.
const maxLogLines = 20

// ParseDeviceInformation reads /DeviceInformationX.
func ParseDeviceInformation(b []byte) (*api.DeviceInformation, error) {
	root, err := parsePage(b, rootDeviceInformation)
	if err != nil {
		return nil, err
	}
	return &api.DeviceInformation{
		VersionID:        root.field(fieldVersionID),
		AppLoadID:        root.field(fieldAppLoadID),
		BootLoadID:       root.field(fieldBootLoadID),
		HardwareRevision: root.field(fieldHardwareRevision),
		SerialNumber:     root.field(fieldSerialNumber),
		ModelNumber:      root.field(fieldModelNumber),
	}, nil
}

// ParseNetworkConfiguration reads /NetworkConfigurationX.
func ParseNetworkConfiguration(b []byte) (*api.NetworkConfiguration, error) {
	root, err := parsePage(b, rootNetworkConfiguration)
	if err != nil {
		return nil, err
	}
	return &api.NetworkConfiguration{
		AdminVLAN:    root.field(fieldAdminVLAN),
		CallManager1: root.field(fieldCallManager1),
	}, nil
}

// ParseDeviceLog reads /DeviceLogX: the text of its status elements (at
// any depth), or of every leaf element when it has none, one line per line
// of text, trimmed, empty lines dropped; the last maxLogLines, oldest
// first (in document order).
func ParseDeviceLog(b []byte) ([]string, error) {
	root, err := parsePage(b, rootDeviceLog)
	if err != nil {
		return nil, err
	}
	var status, leaves []*node
	root.walk(func(n *node) {
		if n == root {
			return
		}
		if n.name == elementStatus {
			status = append(status, n)
		}
		if len(n.children) == 0 {
			leaves = append(leaves, n)
		}
	})
	from := status
	if len(status) == 0 {
		from = leaves
	}
	var out []string
	for _, n := range from {
		for l := range strings.Lines(n.innerText()) {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	return out[max(0, len(out)-maxLogLines):], nil
}

// node is one element of a parsed page.
type node struct {
	name     string // local name, lower case
	text     strings.Builder
	parts    []any // the element's content in order: string or *node
	children []*node
}

// field is the trimmed text of the first child element whose name is one
// of names and whose text is not blank, or "".
func (n *node) field(names []string) string {
	for _, c := range n.children {
		for _, name := range names {
			if c.name == name {
				if v := strings.TrimSpace(c.text.String()); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// innerText is all the text inside the element, its descendants' included.
func (n *node) innerText() string {
	var b strings.Builder
	var add func(*node)
	add = func(n *node) {
		for _, p := range n.parts {
			switch p := p.(type) {
			case string:
				b.WriteString(p)
			case *node:
				add(p)
			}
		}
	}
	add(n)
	return b.String()
}

// walk visits n and its descendants in document order.
func (n *node) walk(f func(*node)) {
	f(n)
	for _, c := range n.children {
		c.walk(f)
	}
}

// parsePage parses one page: a well-formed XML document whose single root
// element is root (case-insensitively, with or without a trailing "x"),
// in UTF-8, ISO-8859-1 or US-ASCII. Anything else, an HTML page included,
// is an error.
func parsePage(b []byte, root string) (*node, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	d.CharsetReader = charsetReader
	var (
		top   *node
		stack []*node
	)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not XML: %w", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			n := &node{name: strings.ToLower(tok.Name.Local)}
			if len(stack) == 0 {
				if top != nil {
					return nil, errors.New("more than one root element")
				}
				if n.name != root && n.name != root+"x" {
					return nil, fmt.Errorf("the root element is <%s>, not <%s>", sanitize(tok.Name.Local, 64), root)
				}
				top = n
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
				parent.parts = append(parent.parts, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(bytes.ReplaceAll(tok, []byte("\ufeff"), nil))) > 0 {
					return nil, errors.New("text outside the root element")
				}
				continue
			}
			n := stack[len(stack)-1]
			n.text.Write(tok)
			n.parts = append(n.parts, string(tok))
		}
	}
	if top == nil {
		return nil, errors.New("no root element")
	}
	return top, nil
}

// charsetReader decodes the character sets a phone's pages declare. UTF-8
// needs nothing (encoding/xml reads it itself; "utf8" comes here);
// ISO-8859-1 maps each byte to the code point of the same value; US-ASCII
// is read as ISO-8859-1, its superset, so a stray high byte cannot fail a
// scrape. Anything else is refused.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8":
		return input, nil
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "latin-1", "l1",
		"us-ascii", "ascii":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(b)+len(b)/4)
		for _, c := range b {
			out = utf8.AppendRune(out, rune(c))
		}
		return bytes.NewReader(out), nil
	}
	return nil, fmt.Errorf("unsupported character set %q", sanitize(label, 64))
}

// sanitize makes text a phone influenced safe to keep: valid UTF-8, no
// control characters, at most limit bytes.
func sanitize(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	cut := limit - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
