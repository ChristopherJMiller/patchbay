package compile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

func TestTreeOf(t *testing.T) {
	for _, tc := range []struct {
		path, tree string
	}{
		{"asterisk/pjsip.conf", compile.TreeAsterisk},
		{"tftp/SEPD0C78915E8C8.cnf.xml", compile.TreeTFTP},
		{"tftp/Desktops/320x212x16/List.xml", compile.TreeTFTP},
		{"http/directory.xml", compile.TreeHTTP},
		{"provisioning/chris-laptop.xml", compile.TreeProvisioning},
	} {
		got, err := compile.TreeOf(tc.path)
		if err != nil || got != tc.tree {
			t.Errorf("TreeOf(%q) = %q, %v; want %q", tc.path, got, err, tc.tree)
		}
	}
	for _, bad := range []string{
		"",
		"pjsip.conf",                  // no tree
		"asterisk",                    // the tree itself, not a file in it
		"asterisk/",                   // ditto, unclean
		"/asterisk/pjsip.conf",        // absolute
		"etc/asterisk/pjsip.conf",     // not a tree
		"tftp/../asterisk/pjsip.conf", // escapes its tree
		"../tftp/x",
		"./tftp/x",
		"tftp//x",
		`tftp\x`,
	} {
		if tree, err := compile.TreeOf(bad); err == nil {
			t.Errorf("TreeOf(%q) = %q, nil; want an error", bad, tree)
		}
	}
}

func TestBundleValidate(t *testing.T) {
	pw := compile.SecretRef{Name: "phone-desk", Key: "sip-password"}.Placeholder()
	good := compile.Bundle{Files: map[string][]byte{
		"asterisk/asterisk.conf":        []byte("[directories]\n"),
		"asterisk/pjsip.conf":           []byte("password = " + pw + "\n"),
		"tftp/SEPD0C78915E8C8.cnf.xml":  []byte("<authPassword>" + pw + "</authPassword>"),
		"http/directory.xml":            []byte("<CiscoIPPhoneDirectory/>"),
		"provisioning/chris-laptop.xml": []byte(pw),
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed bundle: %v", err)
	}

	for name, files := range map[string]map[string][]byte{
		"no asterisk.conf":         {"asterisk/pjsip.conf": nil},
		"a path outside the trees": {"asterisk/asterisk.conf": nil, "pjsip.conf": nil},
		"a placeholder in http/":   {"asterisk/asterisk.conf": nil, "http/directory.xml": []byte(pw)},
		"an escape from tftp/":     {"asterisk/asterisk.conf": nil, "tftp/../asterisk/manager.conf": nil},
	} {
		if err := (compile.Bundle{Files: files}).Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func TestLiveLayout(t *testing.T) {
	if compile.AsteriskConfigDir != "/var/lib/patchbay/config/current/asterisk" {
		t.Fatalf("AsteriskConfigDir = %q", compile.AsteriskConfigDir)
	}
}

// TestChartUsesTheLiveLayout keeps the chart's hard-coded paths in step with
// the layout: the live-config volume mounted at LiveDir in both containers,
// and Asterisk started with -C through the current symlink.
func TestChartUsesTheLiveLayout(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "patchbay", "templates", "pbx.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := string(raw)
	for _, want := range []string{
		"mountPath: " + compile.LiveDir,
		compile.AsteriskConfigDir + "/asterisk.conf",
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("charts/patchbay/templates/pbx.yaml does not contain %q", want)
		}
	}
	if n := strings.Count(chart, "mountPath: "+compile.LiveDir+"\n"); n != 2 {
		t.Errorf("the live-config volume is mounted at %s %d times, want 2 (agent and Asterisk)", compile.LiveDir, n)
	}
}
