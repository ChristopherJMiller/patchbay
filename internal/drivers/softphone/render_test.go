package softphone_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// placeholder is the password placeholder Compile hands Render
// (compile.PasswordRef(id).Placeholder(): @@secret:patchbay-<ID>/sip-password@@).
// Written out here so these tests do not depend on the compile package.
func placeholder(id string) string { return "@@secret:patchbay-" + id + "/sip-password@@" }

// houseCtx is the release A install's RenderContext.
func houseCtx(id string) drivers.RenderContext {
	return drivers.RenderContext{
		PBXAddress:    "192.168.0.240",
		SIPPort:       5060,
		DeviceHTTPURL: "http://192.168.0.240:8088",
		DirectoryURL:  "http://192.168.0.240:8088/directory.xml",
		ServicesURL:   "http://192.168.0.240:8088/services.xml",
		ContactsURL:   "http://192.168.0.240:8088/directory/contacts.vcf",
		NTPServer:     "192.168.20.1",
		Timezone:      "America/Los_Angeles",
		Signalling:    drivers.SignallingPJSIP,
		DialPatterns:  []string{"600", "911", "1XX", "3XX", "7XX", "2XX", "0", "9.", "*8"},
		SIPPassword:   placeholder(id),
	}
}

func softphoneDevice(name, number, label string) drivers.Device {
	return drivers.Device{
		Kind:   drivers.KindSoftphone,
		Name:   name,
		ID:     drivers.DeviceID(drivers.KindSoftphone, name),
		Model:  drivers.ModelSoftphone,
		Number: number,
		Label:  label,
	}
}

type renderCase struct {
	name string
	dev  drivers.Device
	ctx  drivers.RenderContext
}

func renderCases() []renderCase {
	laptop := softphoneDevice("chris-laptop", "150", "Chris (laptop)")
	hostile := softphoneDevice("lab", "151", `R&D; "Lab" <1> Küche`)
	long := softphoneDevice("the-upstairs-guest-bedroom-tablet", "152", "Guest tablet")
	other := softphoneDevice("chris-laptop", "150", "Chris (laptop)")
	otherCtx := houseCtx(other.ID)
	otherCtx.PBXAddress, otherCtx.SIPPort = "10.0.0.5", 5070
	otherCtx.ContactsURL = "http://10.0.0.5:8089/directory/contacts.vcf"
	return []renderCase{
		{"chris-laptop", laptop, houseCtx(laptop.ID)},
		{"hostile-label", hostile, houseCtx(hostile.ID)},
		{"long-name", long, houseCtx(long.ID)},
		{"settings-variant", other, otherCtx},
	}
}

// TestRenderGolden pins each case's files byte for byte against
// testdata/render/<case>/<bundle path>. Regenerate with
// `go test ./internal/drivers/softphone -run Golden -update` and review the
// diff like code.
func TestRenderGolden(t *testing.T) {
	for _, tc := range renderCases() {
		t.Run(tc.name, func(t *testing.T) {
			files, err := softphone.New().Render(tc.dev, tc.ctx)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("testdata", "render", tc.name)
			if *update {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				for p, b := range files {
					path := filepath.Join(dir, filepath.FromSlash(p))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, b, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := map[string][]byte{}
			err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				b, err := os.ReadFile(path)
				want[filepath.ToSlash(rel)] = b
				return err
			})
			if err != nil {
				t.Fatalf("reading goldens (run with -update to create them): %v", err)
			}
			for p, w := range want {
				if got, ok := files[p]; !ok {
					t.Errorf("%s: in the golden but not rendered", p)
				} else if !bytes.Equal(got, w) {
					t.Errorf("%s differs from the golden (rerun with -update and review):\n--- got\n%s\n--- want\n%s", p, got, w)
				}
			}
			for p := range files {
				if _, ok := want[p]; !ok {
					t.Errorf("%s: rendered but not in the golden", p)
				}
			}
		})
	}
}

// lpDoc is a parsed provisioning document, kept in document order.
type lpDoc struct {
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

// entry is one key of a parsed document.
type entry struct{ section, key, value string }

func parseDoc(t *testing.T, b []byte) (lpDoc, []entry) {
	t.Helper()
	var doc lpDoc
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the document does not parse as XML: %v\n%s", err, b)
	}
	var out []entry
	for _, s := range doc.Sections {
		for _, e := range s.Entries {
			out = append(out, entry{s.Name, e.Name, e.Value})
		}
	}
	return doc, out
}

// TestProvisioningDocumentKeys pins every key the document writes and its
// value for the house softphone. Each key was confirmed in liblinphone's
// source, linphone-sdk commit b5cd87b648f394126506145c3cc62b820f088fb8
// (2026-10-14; https://gitlab.linphone.org/BC/public/linphone-sdk), paths
// under liblinphone/, and the reading code is cited beside it. The format
// itself (root <config>, <section name>, <entry name overwrite>, the value
// as text, overwrite="true" to replace a value, an empty value deleting the
// key) is coreapi/xml2lpc.cpp processEntry (line 134) and coreapi/lpconfig.c
// linphone_config_set_string (line 859).
func TestProvisioningDocumentKeys(t *testing.T) {
	dev := softphoneDevice("chris-laptop", "150", "Chris (laptop)")
	files, err := softphone.New().Render(dev, houseCtx(dev.ID))
	if err != nil {
		t.Fatal(err)
	}
	doc, got := parseDoc(t, files["provisioning/softphone-chris-laptop.xml"])
	if doc.XMLName.Local != "config" || doc.XMLName.Space != "http://www.linphone.org/xsds/lpconfig.xsd" {
		t.Errorf("root = %v, want config in the lpconfig.xsd namespace (share/xml/lpconfig.xsd)", doc.XMLName)
	}
	want := []entry{
		// coreapi/linphonecore.c:2098 reads sip/default_proxy.
		{"sip", "default_proxy", "0"},
		// coreapi/linphonecore.c:4340 (use_rfc2833, default 1) and :4330
		// (use_info, default 0).
		{"sip", "use_rfc2833", "1"},
		{"sip", "use_info", "0"},
		// src/account/account-params.cpp AccountParams(lc, index), line 246:
		// reg_proxy :256, reg_route :258, reg_identity :252, reg_expires :286,
		// reg_sendregister :287, publish :288.
		{"proxy_0", "reg_proxy", "<sip:192.168.0.240:5060;transport=udp>"},
		{"proxy_0", "reg_route", ""},
		{"proxy_0", "reg_identity", "<sip:150@192.168.0.240>"},
		{"proxy_0", "reg_expires", "600"},
		{"proxy_0", "reg_sendregister", "1"},
		{"proxy_0", "publish", "0"},
		// src/auth-info/auth-info.cpp AuthInfo(LpConfig*, key), line 105:
		// username :111, userid :112, passwd :113, domain :116. userid is
		// the digest username when set: AuthInfo::fillBelleSipEvent (:487)
		// and coreapi/callbacks.c fill_auth_info (:886). Every auth_info_N
		// is deleted before a document is applied
		// (coreapi/remote_provisioning.c:37-47).
		{"auth_info_0", "username", "150"},
		{"auth_info_0", "userid", "softphone-chris-laptop"},
		{"auth_info_0", "passwd", "@@secret:patchbay-softphone-chris-laptop/sip-password@@"},
		{"auth_info_0", "domain", "192.168.0.240"},
		// coreapi/linphonecore.c get_codec, line 2321: mime :2330, rate
		// :2333, bitrate :2334, recv_fmtp :2335, channels :2336, enabled
		// :2337; the list ends at the first section without a mime (:2331).
		// G722 is registered disabled by default (:2987).
		{"audio_codec_0", "mime", "G722"},
		{"audio_codec_0", "rate", "8000"},
		{"audio_codec_0", "channels", "1"},
		{"audio_codec_0", "enabled", "1"},
		{"audio_codec_0", "recv_fmtp", ""},
		{"audio_codec_0", "bitrate", ""},
		{"audio_codec_1", "mime", "PCMU"},
		{"audio_codec_1", "rate", "8000"},
		{"audio_codec_1", "channels", "1"},
		{"audio_codec_1", "enabled", "1"},
		{"audio_codec_1", "recv_fmtp", ""},
		{"audio_codec_1", "bitrate", ""},
		{"audio_codec_2", "mime", ""},
		// coreapi/linphonecore.c video_config_read: automatically_initiate
		// :2558, automatically_accept :2560, capture :2564, display :2565.
		{"video", "capture", "0"},
		{"video", "display", "0"},
		{"video", "automatically_initiate", "0"},
		{"video", "automatically_accept", "0"},
		// coreapi/remote_provisioning.c: config-uri is the URL fetched at
		// every start (linphone_core_get_provisioning_uri :236, used by
		// coreapi/linphonecore.c:3778); transient_provisioning=1 removes it
		// after a successful apply (:240, coreapi/linphonecore.c:2895).
		{"misc", "config-uri", ""},
		{"misc", "transient_provisioning", "1"},
		// linphone-sdk 79aefb33 (2026-10-26), coreapi/linphonecore.c
		// linphone_configuring_terminated: contacts-vcard-list :2917,
		// fetch_contacts_vcard_list_at_startup :2944 (default TRUE).
		{"misc", "contacts-vcard-list", "http://192.168.0.240:8088/directory/contacts.vcf"},
		{"misc", "fetch_contacts_vcard_list_at_startup", "1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("document keys:\n got %q\nwant %q", got, want)
	}
	for _, s := range doc.Sections {
		for _, e := range s.Entries {
			if e.Overwrite != "true" {
				t.Errorf("%s/%s: overwrite=%q, want \"true\" (Linphone keeps an existing value otherwise)", s.Name, e.Name, e.Overwrite)
			}
		}
	}
}

func TestRenderProperties(t *testing.T) {
	d := softphone.New()
	p, _ := d.Profile(drivers.ModelSoftphone)
	long := softphoneDevice(strings.Repeat("a", 253), "153", "x")
	for _, tc := range append(renderCases(), renderCase{"253-character name", long, houseCtx(long.ID)}) {
		t.Run(tc.name, func(t *testing.T) {
			files, err := d.Render(tc.dev, tc.ctx)
			if err != nil {
				t.Fatal(err)
			}
			path := softphone.ProvisioningFile(tc.dev.ID)
			if len(files) != 1 || files[path] == nil {
				t.Fatalf("files = %v, want only %s", slices.Collect(mapsKeys(files)), path)
			}
			if !strings.HasPrefix(path, "provisioning/") || len(tc.dev.ID) > drivers.DeviceIDMaxLength {
				t.Errorf("path %s: want provisioning/<ID of at most %d>.xml", path, drivers.DeviceIDMaxLength)
			}
			b := files[path]
			if !bytes.HasPrefix(b, []byte(`<?xml version="1.0" encoding="UTF-8"?>`)) {
				t.Errorf("no UTF-8 XML declaration:\n%s", b)
			}
			_, entries := parseDoc(t, b)
			get := func(section, key string) (string, bool) {
				for _, e := range entries {
					if e.section == section && e.key == key {
						return e.value, true
					}
				}
				return "", false
			}

			// The placeholder appears once, unescaped, and nothing else
			// looks like one.
			ph := tc.ctx.SIPPassword
			if n := bytes.Count(b, []byte(ph)); n != 1 {
				t.Errorf("placeholder %q appears %d times in the raw bytes, want 1", ph, n)
			}
			if n := bytes.Count(b, []byte("@@")); n != 2 {
				t.Errorf(`"@@" appears %d times, want only the placeholder's 2`, n)
			}
			// No label, no display name: the label never reaches the
			// document (the caller ID is Asterisk's).
			if v, _ := get("proxy_0", "reg_identity"); v != "<sip:"+tc.dev.Number+"@"+tc.ctx.PBXAddress+">" {
				t.Errorf("reg_identity = %q, want the bare SIP address", v)
			}
			if v, _ := get("auth_info_0", "userid"); v != tc.dev.ID {
				t.Errorf("userid = %q, want the device ID %q", v, tc.dev.ID)
			}
			if v, _ := get("auth_info_0", "username"); v != tc.dev.Number {
				t.Errorf("username = %q, want the number %q", v, tc.dev.Number)
			}
			// The expiry Linphone asks for is the one Asterisk grants.
			if v, _ := get("proxy_0", "reg_expires"); v != "600" || p.Endpoint.RegistrationExpiry.Seconds() != 600 {
				t.Errorf("reg_expires = %q, profile expiry %s; want both 600 s", v, p.Endpoint.RegistrationExpiry)
			}
			// The spent URL is forgotten.
			if v, ok := get("misc", "config-uri"); !ok || v != "" {
				t.Errorf("misc/config-uri = %q (present %t), want present and empty", v, ok)
			}
			// The contacts are the install's, fetched at every start.
			if v, _ := get("misc", "contacts-vcard-list"); v != tc.ctx.ContactsURL {
				t.Errorf("misc/contacts-vcard-list = %q, want %q", v, tc.ctx.ContactsURL)
			}
			// No key is written twice.
			seen := map[[2]string]bool{}
			for _, e := range entries {
				k := [2]string{e.section, e.key}
				if seen[k] {
					t.Errorf("%s/%s written twice", e.section, e.key)
				}
				seen[k] = true
			}

			// Only the number, ID, placeholder, PBX address, SIP port and
			// contacts URL matter: the label, ConfigVersion (Compile's two
			// passes) and every other setting leave the bytes alone.
			for name, mutate := range map[string]func(*drivers.Device, *drivers.RenderContext){
				"label":         func(d *drivers.Device, _ *drivers.RenderContext) { d.Label = "something else entirely" },
				"ConfigVersion": func(d *drivers.Device, _ *drivers.RenderContext) { d.ConfigVersion = strings.Repeat("ab", 32) },
				"other settings": func(_ *drivers.Device, c *drivers.RenderContext) {
					c.DeviceHTTPURL, c.DirectoryURL, c.ServicesURL, c.NTPServer, c.Timezone = "http://x", "http://x/d.xml", "http://x/s.xml", "10.9.9.9", "Europe/London"
					c.DialPatterns = nil
				},
			} {
				dev, ctx := tc.dev, tc.ctx
				mutate(&dev, &ctx)
				again, err := d.Render(dev, ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(again[path], b) {
					t.Errorf("changing %s changed the document", name)
				}
			}
		})
	}
}

func mapsKeys(m drivers.Files) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestRenderErrors(t *testing.T) {
	base := softphoneDevice("chris-laptop", "150", "Chris (laptop)")
	for _, tc := range []struct {
		name    string
		mutate  func(*drivers.Device, *drivers.RenderContext)
		invalid bool // wraps drivers.ErrInvalidSpec
	}{
		{"a phone model", func(d *drivers.Device, _ *drivers.RenderContext) { d.Model = "cisco-7945" }, true},
		{"no model", func(d *drivers.Device, _ *drivers.RenderContext) { d.Model = "" }, true},
		{"empty placeholder", func(_ *drivers.Device, c *drivers.RenderContext) { c.SIPPassword = "" }, false},
		{"placeholder XML would escape", func(_ *drivers.Device, c *drivers.RenderContext) { c.SIPPassword = "@@secret:a&b/c@@" }, false},
		{"placeholder with a space", func(_ *drivers.Device, c *drivers.RenderContext) { c.SIPPassword = "@@secret:a b/c@@" }, false},
		{"empty address", func(_ *drivers.Device, c *drivers.RenderContext) { c.PBXAddress = "" }, false},
		{"IPv6 address", func(_ *drivers.Device, c *drivers.RenderContext) { c.PBXAddress = "fd00::240" }, false},
		{"address with URI syntax", func(_ *drivers.Device, c *drivers.RenderContext) { c.PBXAddress = "pbx>;transport=tcp" }, false},
		{"port 0", func(_ *drivers.Device, c *drivers.RenderContext) { c.SIPPort = 0 }, false},
		{"port 65536", func(_ *drivers.Device, c *drivers.RenderContext) { c.SIPPort = 65536 }, false},
		{"no contacts URL", func(_ *drivers.Device, c *drivers.RenderContext) { c.ContactsURL = "" }, false},
		{"https contacts URL", func(_ *drivers.Device, c *drivers.RenderContext) { c.ContactsURL = "https://192.168.0.240/c.vcf" }, false},
		{"contacts URL without a host", func(_ *drivers.Device, c *drivers.RenderContext) { c.ContactsURL = "http:///c.vcf" }, false},
		{"contacts URL with a space", func(_ *drivers.Device, c *drivers.RenderContext) { c.ContactsURL = "http://pbx/a b.vcf" }, false},
		{"empty number", func(d *drivers.Device, _ *drivers.RenderContext) { d.Number = "" }, false},
		{"non-digit number", func(d *drivers.Device, _ *drivers.RenderContext) { d.Number = "15a" }, false},
		{"empty ID", func(d *drivers.Device, _ *drivers.RenderContext) { d.ID = "" }, false},
		{"ID with a slash", func(d *drivers.Device, _ *drivers.RenderContext) { d.ID = "softphone-a/b" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, ctx := base, houseCtx(base.ID)
			tc.mutate(&dev, &ctx)
			files, err := softphone.New().Render(dev, ctx)
			if err == nil {
				t.Fatalf("Render succeeded with %v", slices.Collect(mapsKeys(files)))
			}
			if got := errors.Is(err, drivers.ErrInvalidSpec); got != tc.invalid {
				t.Errorf("errors.Is(%v, ErrInvalidSpec) = %t, want %t", err, got, tc.invalid)
			}
		})
	}

	// A host name is a usable PBX address.
	ctx := houseCtx(base.ID)
	ctx.PBXAddress = "pbx.home.arpa"
	if _, err := softphone.New().Render(base, ctx); err != nil {
		t.Errorf("host name address: %v", err)
	}
}
