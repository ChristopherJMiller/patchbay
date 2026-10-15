package layout_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

const v1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestVersionDir(t *testing.T) {
	dir, err := layout.VersionDir(v1)
	if err != nil || dir != "v"+v1 {
		t.Fatalf("VersionDir(v1) = %q, %v", dir, err)
	}
	if v, ok := layout.VersionFromDir(dir); !ok || v != v1 {
		t.Errorf("VersionFromDir(%q) = %q, %t", dir, v, ok)
	}
	for _, bad := range []string{"", "../etc", strings.ToUpper(v1), v1[:63], v1 + "0", v1[:63] + "/"} {
		if got, err := layout.VersionDir(bad); err == nil {
			t.Errorf("VersionDir(%q) = %q, want an error", bad, got)
		}
		if got, ok := layout.VersionFromDir("v" + bad); ok {
			t.Errorf("VersionFromDir(%q) = %q, want false", "v"+bad, got)
		}
	}
	if _, ok := layout.VersionFromDir(v1); ok {
		t.Error("VersionFromDir accepted a name without the prefix")
	}
}

// TestOnlyTFTPIsWorldReadable pins the rule that keeps the file server
// (another uid, no groups) away from every resolved secret but the SEP
// files it serves anyway.
func TestOnlyTFTPIsWorldReadable(t *testing.T) {
	for _, tree := range compile.Trees {
		dir, file, ok := layout.TreeModes(tree)
		if !ok {
			t.Fatalf("TreeModes(%q) not ok", tree)
		}
		worldReadable := file&0o004 != 0 && dir&0o005 == 0o005
		if worldReadable != (tree == compile.TreeTFTP) {
			t.Errorf("tree %s: dir %v file %v: world-readable=%t", tree, dir, file, worldReadable)
		}
		if file&0o022 != 0 || dir&0o022 != 0 {
			t.Errorf("tree %s: dir %v file %v writable by group or others", tree, dir, file)
		}
	}
	if _, _, ok := layout.TreeModes("firmware"); ok {
		t.Error("TreeModes accepted a name that is not a tree")
	}
	if layout.VersionDirMode&0o005 != 0o005 || layout.IndexFileMode&0o004 != 0 {
		t.Errorf("version dir %v must be traversable by others, index %v must not be readable by them",
			layout.VersionDirMode, layout.IndexFileMode)
	}
	_, prov, _ := layout.TreeModes(compile.TreeProvisioning)
	if prov&0o044 != 0 {
		t.Errorf("provisioning files %v must be the agent's alone", prov)
	}
	if layout.FileserverUID == layout.AgentUID || layout.FileserverGID == layout.DefaultFSGroup {
		t.Error("the file server must not share the agent's uid or the pod's group")
	}
	if !strings.HasPrefix(layout.FetchSocket, layout.FetchSocketDir+"/") {
		t.Errorf("FetchSocket %s is not in FetchSocketDir", layout.FetchSocket)
	}
}

func TestCurrentAndOpenTree(t *testing.T) {
	live := t.TempDir()
	if _, err := layout.Current(live); !errors.Is(err, layout.ErrNoCurrent) {
		t.Fatalf("Current(empty) error = %v, want ErrNoCurrent", err)
	}
	vdir, _ := layout.VersionDir(v1)
	for _, tree := range compile.Trees {
		if err := os.MkdirAll(filepath.Join(live, vdir, tree), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, vdir, "asterisk", "manager.conf"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, vdir, "tftp", "SEPD0C78915E8C8.cnf.xml"), []byte("<device/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vdir, filepath.Join(live, compile.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	got, err := layout.Current(live)
	if err != nil || got != v1 {
		t.Fatalf("Current = %q, %v; want %q", got, err, v1)
	}

	root, err := layout.OpenTree(live, got, compile.TreeTFTP)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.ReadFile("SEPD0C78915E8C8.cnf.xml"); err != nil {
		t.Errorf("reading a tftp file through the root: %v", err)
	}
	for _, escape := range []string{"../asterisk/manager.conf", "/etc/passwd", "../../" + vdir + "/asterisk/manager.conf"} {
		if b, err := root.ReadFile(escape); err == nil {
			t.Errorf("the tftp root read %q: %q", escape, b)
		}
	}
	if _, err := layout.OpenTree(live, got, "firmware"); err == nil {
		t.Error("OpenTree accepted a name that is not a tree")
	}
	if _, err := layout.OpenTree(live, "../x", compile.TreeTFTP); err == nil {
		t.Error("OpenTree accepted an invalid version")
	}

	if err := os.Remove(filepath.Join(live, compile.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../elsewhere", filepath.Join(live, compile.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if v, err := layout.Current(live); err == nil {
		t.Errorf("Current followed a link to %q: %q", "../elsewhere", v)
	}
}

func TestPersistentDirs(t *testing.T) {
	for _, d := range []string{layout.FirmwareDir, layout.StateDir} {
		if !strings.HasPrefix(d, layout.PersistentDir+"/") {
			t.Errorf("%s is not inside PersistentDir %s", d, layout.PersistentDir)
		}
	}
	if layout.StateDirMode&0o077 != 0 {
		t.Errorf("StateDirMode %v lets others in", layout.StateDirMode)
	}
}
