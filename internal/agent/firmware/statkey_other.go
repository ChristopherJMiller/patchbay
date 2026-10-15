//go:build !linux

package firmware

import "io/fs"

// keyOf is the stat key of a file: its size and modification time. The PBX
// runs on Linux; elsewhere the device and inode are not part of the key.
func keyOf(fi fs.FileInfo) statKey {
	return statKey{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
}
