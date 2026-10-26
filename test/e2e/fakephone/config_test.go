package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The drivers' render goldens and compile's, read-only: a render change
// that would break the fake phone fails here.
const (
	ciscoGoldens     = "../../../internal/drivers/cisco79x5/testdata/render"
	softphoneGoldens = "../../../internal/drivers/softphone/testdata/render"
	compileGoldens   = "../../../internal/compile/testdata/golden"
)

// placeholder is the agent's secret placeholder, which it resolves before a
// phone ever sees the file.
var placeholder = regexp.MustCompile(`@@secret:[^@]+@@`)

// resolve replaces every secret placeholder with password, as the agent does.
func resolve(doc []byte, password string) []byte {
	return placeholder.ReplaceAll(doc, []byte(password))
}

func readGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestParseSEP(t *testing.T) {
	const password = "not-a-secret-sep-password"
	for name, want := range map[string]script.Config{
		"desk": {
			Source: "SEPD0C78915E8C8.cnf.xml", Version: "adc3b7d635474c7ea93068dadb6ffa35",
			PhoneLabel: "Desk", LineLabel: "Desk", LoadInformation: "SIP45.9-4-2SR4-3S",
			User: "100", AuthUser: "phone-desk", Proxy: "192.168.0.240:5060", Transport: "tcp", Expires: 600,
		},
		"hostile-label": {
			Source: "SEPD0C78915E8C8.cnf.xml", Version: "adc3b7d635474c7ea93068dadb6ffa35",
			PhoneLabel: `R&D;"Lab"<1>`, LineLabel: `R&D; "Lab" <1> Küche`, LoadInformation: "SIP45.9-4-2SR4-3S",
			User: "100", AuthUser: "phone-desk", Proxy: "192.168.0.240:5060", Transport: "tcp", Expires: 600,
		},
		"long-label": {
			Source: "SEPD0C78915E8C8.cnf.xml", Version: "adc3b7d635474c7ea93068dadb6ffa35",
			PhoneLabel: "UpstairsOffi", LineLabel: "Upstairs Office Phone", LoadInformation: "SIP45.9-4-2SR4-3S",
			User: "100", AuthUser: "phone-desk", Proxy: "192.168.0.240:5060", Transport: "tcp", Expires: 600,
		},
		"lab-7965": {Source: "SEPD0C78915EBB5.cnf.xml"},
	} {
		t.Run(name, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join(ciscoGoldens, name, "tftp", "SEP*.cnf.xml"))
			if err != nil || len(files) != 1 {
				t.Fatalf("golden: %v %v", files, err)
			}
			doc := resolve(readGolden(t, files[0]), password)
			cfg, err := parseSEP(doc, filepath.Base(files[0]))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Public
			if got.SHA256 != hex.EncodeToString(sha256Of(doc)) {
				t.Errorf("sha256 %s", got.SHA256)
			}
			if got.PasswordSHA256 != sha(password) || string(cfg.Password) != password {
				t.Errorf("password: sha %s", got.PasswordSHA256)
			}
			if cfg.Server != netip.MustParseAddrPort("192.168.0.240:5060") || cfg.Transport != "tcp" {
				t.Errorf("server %s transport %s", cfg.Server, cfg.Transport)
			}
			if want.User == "" { // only the fields every golden shares
				if got.Source != want.Source || got.User == "" || got.AuthUser == "" || got.Expires != 600 {
					t.Errorf("parsed %+v", got)
				}
				return
			}
			want.SHA256, want.PasswordSHA256 = got.SHA256, got.PasswordSHA256
			if got != want {
				t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
			}
		})
	}

	// The golden as rendered, its placeholder unresolved, is refused; so is
	// a transport the 79x5 driver never renders (3).
	desk := readGolden(t, filepath.Join(ciscoGoldens, "desk/tftp/SEPD0C78915E8C8.cnf.xml"))
	if _, err := parseSEP(desk, "SEPD0C78915E8C8.cnf.xml"); err == nil || !errors.Is(err, errPlaceholder) {
		t.Errorf("an unresolved password: %v", err)
	}
	tlp3 := strings.Replace(string(resolve(desk, password)), "<transportLayerProtocol>1<", "<transportLayerProtocol>3<", 1)
	if _, err := parseSEP([]byte(tlp3), "SEPD0C78915E8C8.cnf.xml"); err == nil || !strings.Contains(err.Error(), "transportLayerProtocol \"3\"") {
		t.Errorf("transportLayerProtocol 3: %v", err)
	}
}

func TestParseSEPRefuses(t *testing.T) {
	desk := string(resolve(readGolden(t, filepath.Join(ciscoGoldens, "desk/tftp/SEPD0C78915E8C8.cnf.xml")), "pw"))
	for name, tc := range map[string]struct{ old, new, want string }{
		"transport 3":         {"<transportLayerProtocol>1<", "<transportLayerProtocol>3<", "transportLayerProtocol"},
		"no transport":        {"<transportLayerProtocol>1</transportLayerProtocol>", "", "transportLayerProtocol"},
		"udp is fine":         {"<transportLayerProtocol>1<", "<transportLayerProtocol>2<", ""},
		"unresolved password": {"<authPassword>pw<", "<authPassword>@@secret:patchbay-phone-desk/sip-password@@<", "placeholder"},
		"empty password":      {"<authPassword>pw<", "<authPassword><", "authPassword"},
		"no auth name":        {"<authName>phone-desk<", "<authName><", "authName"},
		"no line name":        {"<name>100</name>", "<name></name>", "name"},
		"hostname server":     {"<processNodeName>192.168.0.240<", "<processNodeName>pbx.example<", "processNodeName"},
		"bad port":            {"<sipPort>5060<", "<sipPort>0<", "sipPort"},
		"bad expiry":          {"<timerRegisterExpires>600<", "<timerRegisterExpires>soon<", "timerRegisterExpires"},
		"not xml":             {"<device>", "<device", "XML"},
		"another root":        {"<device>", "<config>", "device"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(desk, tc.old, tc.new, 1)
			if doc == desk {
				t.Fatalf("%q not in the golden", tc.old)
			}
			cfg, err := parseSEP([]byte(doc), "SEPD0C78915E8C8.cnf.xml")
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want == "" && cfg.Transport != "udp":
				t.Fatalf("transport %q", cfg.Transport)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v, want one naming %q", err, tc.want)
			case err != nil && strings.Contains(err.Error(), "@@secret"):
				t.Fatalf("the error quotes the placeholder: %v", err)
			}
		})
	}
}

func TestParseLPConfig(t *testing.T) {
	const (
		password = "not-a-secret-lp-password"
		contacts = "http://192.168.0.240:8088/directory/contacts.vcf"
	)
	for name, want := range map[string]script.Config{
		"chris-laptop": {
			Source: "provisioning", User: "150", AuthUser: "softphone-chris-laptop",
			Proxy: "192.168.0.240:5060", Transport: "udp", Expires: 600, ConfigURICleared: true,
			ContactsURL: contacts, ContactsAtStartup: true,
		},
		"settings-variant": {
			Source: "provisioning", User: "150", AuthUser: "softphone-chris-laptop",
			Proxy: "10.0.0.5:5070", Transport: "udp", Expires: 600, ConfigURICleared: true,
			ContactsURL: "http://10.0.0.5:8089/directory/contacts.vcf", ContactsAtStartup: true,
		},
		"hostile-label": {
			Source: "provisioning", User: "151", AuthUser: "softphone-lab",
			Proxy: "192.168.0.240:5060", Transport: "udp", Expires: 600, ConfigURICleared: true,
			ContactsURL: contacts, ContactsAtStartup: true,
		},
		"long-name": {
			Source: "provisioning", User: "152", AuthUser: "softphone-the-upstair-03847320",
			Proxy: "192.168.0.240:5060", Transport: "udp", Expires: 600, ConfigURICleared: true,
			ContactsURL: contacts, ContactsAtStartup: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join(softphoneGoldens, name, "provisioning", "*.xml"))
			if err != nil || len(files) != 1 {
				t.Fatalf("golden: %v %v", files, err)
			}
			doc := resolve(readGolden(t, files[0]), password)
			cfg, err := parseLPConfig(doc)
			if err != nil {
				t.Fatal(err)
			}
			want.SHA256, want.PasswordSHA256 = hex.EncodeToString(sha256Of(doc)), sha(password)
			if cfg.Public != want {
				t.Errorf("parsed\n%+v\nwant\n%+v", cfg.Public, want)
			}
			if string(cfg.Password) != password || cfg.Server.String() != want.Proxy || cfg.Transport != "udp" {
				t.Errorf("account %s %s", cfg.Server, cfg.Transport)
			}
		})
	}
}

func TestParseLPConfigRefuses(t *testing.T) {
	golden := string(resolve(readGolden(t,
		filepath.Join(softphoneGoldens, "chris-laptop/provisioning/softphone-chris-laptop.xml")), "pw"))
	entry := func(name, value string) string {
		return `<entry name="` + name + `" overwrite="true">` + value + `</entry>`
	}
	for name, tc := range map[string]struct{ old, new, want string }{
		"no proxy":            {entry("reg_proxy", "&lt;sip:192.168.0.240:5060;transport=udp&gt;"), "", "reg_proxy"},
		"sips proxy":          {"&lt;sip:192.168.0.240:5060;", "&lt;sips:192.168.0.240:5060;", "reg_proxy"},
		"tls proxy":           {"transport=udp&gt;", "transport=tls&gt;", "transport"},
		"hostname proxy":      {"&lt;sip:192.168.0.240:5060;", "&lt;sip:pbx.example:5060;", "reg_proxy"},
		"no identity user":    {"&lt;sip:150@192.168.0.240&gt;", "&lt;sip:192.168.0.240&gt;", "reg_identity"},
		"bad expiry":          {entry("reg_expires", "600"), entry("reg_expires", "-1"), "reg_expires"},
		"unresolved password": {entry("passwd", "pw"), entry("passwd", "@@secret:x/sip-password@@"), "placeholder"},
		"no password":         {entry("passwd", "pw"), entry("passwd", ""), "passwd"},
		"another root":        {"<config ", "<device ", "config"},
		"not xml":             {"</config>", "", "XML"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(golden, tc.old, tc.new, 1)
			if doc == golden {
				t.Fatalf("%q not in the golden", tc.old)
			}
			if _, err := parseLPConfig([]byte(doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %q", err, tc.want)
			}
		})
	}

	// config-uri: cleared only when present, empty and overwritten.
	for name, tc := range map[string]struct {
		old, new string
		cleared  bool
	}{
		"kept":           {entry("config-uri", ""), entry("config-uri", "http://pbx/p/x"), false},
		"not overwrite":  {entry("config-uri", ""), `<entry name="config-uri"></entry>`, false},
		"absent":         {entry("config-uri", ""), "", false},
		"userid absent":  {entry("userid", "softphone-chris-laptop"), "", true},
		"default port":   {"192.168.0.240:5060;transport", "192.168.0.240;transport", true},
		"udp by default": {";transport=udp&gt;", "&gt;", true},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(golden, tc.old, tc.new, 1)
			cfg, err := parseLPConfig([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Public.ConfigURICleared != tc.cleared {
				t.Errorf("configURICleared %t", cfg.Public.ConfigURICleared)
			}
			if name == "userid absent" && cfg.Public.AuthUser != "150" {
				t.Errorf("auth user %q, want the username", cfg.Public.AuthUser)
			}
			if cfg.Server.String() != "192.168.0.240:5060" || cfg.Transport != "udp" {
				t.Errorf("server %s transport %s", cfg.Server, cfg.Transport)
			}
		})
	}
}

// TestSecretsNeverPrint: a parsed config and an account print and marshal
// without the password, whichever way they are formatted.
func TestSecretsNeverPrint(t *testing.T) {
	const password = "not-a-secret-printed"
	doc := resolve(readGolden(t, filepath.Join(ciscoGoldens, "desk/tftp/SEPD0C78915E8C8.cnf.xml")), password)
	cfg, err := parseSEP(doc, "SEPD0C78915E8C8.cnf.xml")
	if err != nil {
		t.Fatal(err)
	}
	acc := cfg.account()
	for _, s := range []string{
		sprintf("%v", cfg), sprintf("%+v", cfg), sprintf("%#v", cfg), sprintf("%s", cfg),
		sprintf("%v", *cfg), sprintf("%+v", acc), sprintf("%#v", acc), sprintf("%v", acc.Password),
		marshal(t, acc), marshal(t, cfg), logged(acc), logged(cfg),
	} {
		if strings.Contains(s, password) {
			t.Errorf("printed the password: %s", s)
		}
	}
	if acc.Password.reveal() != password {
		t.Error("reveal lost the password")
	}
}

func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// logged is v as a JSON log line would carry it.
func logged(v any) string {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "v", v, "direct", v)
	return buf.String()
}
