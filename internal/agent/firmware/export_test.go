package firmware

// Internals the external tests (package firmware_test) reach.

// SetHashHook makes d call f each time it hashes a file.
func SetHashHook(d *Disk, f func(load, name string)) { d.onHash = f }

// SetMaxSize lowers d's upload limit, so a test can overflow it cheaply.
func SetMaxSize(d *Disk, n int64) { d.maxSize = n }

// Unserved lists the Shared names m never serves.
func Unserved(m *Manifest) []string { return m.unserved }
