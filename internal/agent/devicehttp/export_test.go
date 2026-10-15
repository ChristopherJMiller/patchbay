package devicehttp

import (
	"context"
	"net"
	"net/http"
	"time"
)

// SetCompareHook makes every token comparison call f with the grant's
// index; restore puts the previous hook back.
func SetCompareHook(f func(i int)) (restore func()) {
	prev := compareHook.Swap(&f)
	return func() { compareHook.Store(prev) }
}

// Prune runs one prune now, as Run's ticker does.
func (s *Server) Prune() { s.prune() }

// SetPruneEvery sets Run's prune interval; call it before Serve.
func (s *Server) SetPruneEvery(d time.Duration) { s.pruneEvery = d }

// Serve is Run on a listener the test bound.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error { return s.serve(ctx, ln) }

// HTTPServer is the server Run starts.
func (s *Server) HTTPServer() *http.Server { return s.httpServer() }

// Match is the token comparison.
var Match = match
