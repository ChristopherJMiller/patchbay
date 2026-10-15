package apply

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Names in the live directory that are never a version: a version being
// written, a version being removed, and the symlink about to become
// current. Readers ignore them (layout.VersionFromDir refuses them), and
// New removes any a crash left behind.
const (
	stagingPrefix = ".staging-"
	trashPrefix   = ".trash-"
	linkPrefix    = ".current-"
)

// write lays out v<version> completely under a staging name, then renames
// it into place. An existing v<version> (written before: same version,
// same bytes) is reused; its modification time is set to now, so it
// counts as the newest when versions are pruned.
func (a *Applier) write(r intake.Resolved) error {
	dir, err := layout.VersionDir(r.Bundle.Version)
	if err != nil {
		return err
	}
	final := filepath.Join(a.cfg.LiveDir, dir)
	if fi, err := os.Lstat(final); err == nil && fi.IsDir() {
		now := time.Now()
		return os.Chtimes(final, now, now)
	}
	for p := range r.Files {
		if _, err := compile.TreeOf(p); err != nil {
			return err
		}
	}
	index, err := json.Marshal(r.Bundle.Index())
	if err != nil {
		return fmt.Errorf("encoding the index: %w", err)
	}

	staging, err := os.MkdirTemp(a.cfg.LiveDir, stagingPrefix)
	if err != nil {
		return fmt.Errorf("writing %s: %w", dir, err)
	}
	if err := a.fill(staging, r.Files, index); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("writing %s: %w", dir, err)
	}
	if err := os.Rename(staging, final); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("writing %s: %w", dir, err)
	}
	return syncDir(a.cfg.LiveDir)
}

// fill writes every tree, file and the index into the staging directory,
// each with its mode set explicitly and its group set to FSGroup.
func (a *Applier) fill(staging string, files map[string][]byte, index []byte) error {
	root, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	dirs := map[string]bool{}
	mkdir := func(name string, mode fs.FileMode) error {
		if dirs[name] {
			return nil
		}
		if err := root.Mkdir(name, mode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		dirs[name] = true
		return a.own(root, name, mode)
	}
	for _, tree := range compile.Trees {
		dirMode, _, _ := layout.TreeModes(tree)
		if err := mkdir(tree, dirMode); err != nil {
			return err
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		tree, _, _ := strings.Cut(p, "/")
		dirMode, fileMode, _ := layout.TreeModes(tree)
		// Every directory between the tree and the file.
		parts := strings.Split(p, "/")
		for i := 2; i < len(parts); i++ {
			if err := mkdir(path.Join(parts[:i]...), dirMode); err != nil {
				return err
			}
		}
		if err := a.writeFile(root, p, files[p], fileMode); err != nil {
			return err
		}
	}
	if err := a.writeFile(root, layout.IndexFile, index, layout.IndexFileMode); err != nil {
		return err
	}
	for d := range dirs {
		if err := syncRootDir(root, d); err != nil {
			return err
		}
	}
	if err := a.own(root, ".", layout.VersionDirMode); err != nil {
		return err
	}
	return syncRootDir(root, ".")
}

func (a *Applier) writeFile(root *os.Root, name string, content []byte, mode fs.FileMode) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return a.own(root, name, mode)
}

// own sets a name's mode (never left to the umask) and its group (never
// left to setgid inheritance).
func (a *Applier) own(root *os.Root, name string, mode fs.FileMode) error {
	if err := root.Chmod(name, mode); err != nil {
		return err
	}
	if a.cfg.FSGroup >= 0 {
		return root.Lchown(name, -1, a.cfg.FSGroup)
	}
	return nil
}

// swap points current at v<version>: a temporary symlink renamed over it,
// so a reader always finds either the old or the new version.
func (a *Applier) swap(version string) error {
	dir, err := layout.VersionDir(version)
	if err != nil {
		return err
	}
	name := filepath.Join(a.cfg.LiveDir, linkPrefix+rand.Text())
	if err := os.Symlink(dir, name); err != nil {
		return fmt.Errorf("swapping to %s: %w", dir, err)
	}
	if a.cfg.FSGroup >= 0 {
		if err := os.Lchown(name, -1, a.cfg.FSGroup); err != nil {
			_ = os.Remove(name)
			return fmt.Errorf("swapping to %s: %w", dir, err)
		}
	}
	if err := os.Rename(name, filepath.Join(a.cfg.LiveDir, compile.CurrentLink)); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("swapping to %s: %w", dir, err)
	}
	return syncDir(a.cfg.LiveDir)
}

// prune removes every version but current, previous and the newest ones up
// to Keep (by modification time). A version is renamed away before it is
// removed, so a crash never leaves a partial v<version>.
func (a *Applier) prune(current, previous string) {
	entries, err := os.ReadDir(a.cfg.LiveDir)
	if err != nil {
		a.cfg.Log.Info("cannot list the live directory to prune it", "error", err.Error())
		return
	}
	type version struct {
		v   string
		mod time.Time
	}
	var versions []version
	for _, e := range entries {
		v, ok := layout.VersionFromDir(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		versions = append(versions, version{v, info.ModTime()})
	}
	slices.SortFunc(versions, func(x, y version) int {
		return cmp.Or(y.mod.Compare(x.mod), strings.Compare(x.v, y.v))
	})
	kept := 0
	for _, keep := range []string{current, previous} {
		if keep != "" && slices.ContainsFunc(versions, func(x version) bool { return x.v == keep }) {
			kept++
		}
	}
	for _, x := range versions {
		if x.v == current || x.v == previous {
			continue
		}
		if kept < a.cfg.Keep {
			kept++
			continue
		}
		a.removeVersion(x.v)
	}
}

func (a *Applier) removeVersion(v string) {
	dir, _ := layout.VersionDir(v)
	trash, err := os.MkdirTemp(a.cfg.LiveDir, trashPrefix)
	if err == nil {
		err = os.Rename(filepath.Join(a.cfg.LiveDir, dir), filepath.Join(trash, dir))
	}
	if err == nil {
		err = os.RemoveAll(trash)
	}
	if err != nil {
		a.cfg.Log.Info("cannot remove an old version", "version", v, "error", err.Error())
		return
	}
	a.mu.Lock()
	delete(a.indexes, v)
	a.mu.Unlock()
	a.cfg.Log.V(1).Info("removed an old version", "version", v)
}

// cleanup removes what a crash left in the live directory.
func cleanup(liveDir string) error {
	entries, err := os.ReadDir(liveDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, stagingPrefix) || strings.HasPrefix(name, trashPrefix) || strings.HasPrefix(name, linkPrefix) {
			if err := os.RemoveAll(filepath.Join(liveDir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// readIndex reads v<version>/bundle.json.
func readIndex(liveDir, version string) (compile.Index, error) {
	dir, err := layout.VersionDir(version)
	if err != nil {
		return compile.Index{}, err
	}
	root, err := os.OpenRoot(liveDir)
	if err != nil {
		return compile.Index{}, err
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(dir + "/" + layout.IndexFile)
	if err != nil {
		return compile.Index{}, err
	}
	var idx compile.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return compile.Index{}, fmt.Errorf("%s/%s: %w", dir, layout.IndexFile, err)
	}
	if idx.Version != version {
		return compile.Index{}, fmt.Errorf("%s/%s holds version %q", dir, layout.IndexFile, idx.Version)
	}
	return idx, nil
}

// asteriskFiles reads a live version's asterisk/ tree: name -> bytes.
func asteriskFiles(liveDir, version string) (map[string][]byte, error) {
	root, err := layout.OpenTree(liveDir, version, compile.TreeAsterisk)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := map[string][]byte{}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := root.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = b
		return nil
	})
	return out, err
}

func syncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // the live directory, from the agent's own configuration
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func syncRootDir(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// mkdirLive creates the live directory when it is missing (it is a mount
// in the PBX pod, a temporary directory in tests).
func mkdirLive(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	return os.MkdirAll(dir, layout.VersionDirMode)
}
