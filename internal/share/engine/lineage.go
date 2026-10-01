package engine

import (
	"crypto/tls"
	"sync"

	"github.com/idyl-labs/hyperplane-go/generation"
)

// Lineage is what the docks of one share hand on to each other: the latest
// dock's generation, which the next dock names as its predecessor so that
// the edge treats the redock as a succession and the share's URL stays up,
// and one TLS session cache, so a
// redock resumes rather than repeating the full handshake. It lives in
// daemon memory only and ends with the share. The generation's nonce is a
// credential: nothing logs it.
type Lineage struct {
	sessions tls.ClientSessionCache

	mu  sync.Mutex
	gen *generation.DockGen
}

// NewLineage returns the lineage of a share that has not docked yet.
func NewLineage() *Lineage {
	return &Lineage{sessions: tls.NewLRUClientSessionCache(4)}
}

// Sessions is the session cache every dock of the share uses; nil for a nil
// lineage.
func (l *Lineage) Sessions() tls.ClientSessionCache {
	if l == nil {
		return nil
	}
	return l.sessions
}

// Predecessor is the latest dock's generation: nil before the first dock
// and for a nil lineage.
func (l *Lineage) Predecessor() *generation.DockGen {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen == nil {
		return nil
	}
	gen := *l.gen
	return &gen
}

// Docked records the generation of a dock the edge admitted.
func (l *Lineage) Docked(gen generation.DockGen) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.gen = &gen
	l.mu.Unlock()
}
