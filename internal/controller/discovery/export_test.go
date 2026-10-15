package discovery

// Remembered is how many MACs r remembers as reported.
func Remembered(r *Reporter) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reported)
}
