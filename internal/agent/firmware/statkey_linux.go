//go:build linux

package firmware

import (
	"io/fs"
	"syscall"
)

// keyOf is the stat key of a file: its device and inode, size and
// modification time.
func keyOf(fi fs.FileInfo) statKey {
	k := statKey{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.dev, k.ino = st.Dev, st.Ino
	}
	return k
}
