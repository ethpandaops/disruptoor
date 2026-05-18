// Package eventlog is a tiny, fixed-size in-memory ring of api.Event values.
// The api service writes via Append (its OnEvent callback); the webui reads
// via Snapshot to render the /events page.
//
// We keep this small on purpose: persistent audit lives outside disruptoor.
// The ring exists so users hitting /events right after a curl PUT can see the
// transition without spelunking logs.
package eventlog

import (
	"sync"

	"github.com/ethpandaops/disruptoor/internal/api"
)

// DefaultSize is the ring capacity used when Config.Size is zero.
const DefaultSize = 200

// Config controls the ring capacity. Zero Size means DefaultSize.
type Config struct {
	Size int
}

// Ring is a thread-safe bounded log of api.Event values.
type Ring struct {
	mu   sync.Mutex
	buf  []api.Event
	next int
	full bool
}

// New constructs a Ring.
func New(cfg Config) *Ring {
	n := cfg.Size
	if n <= 0 {
		n = DefaultSize
	}
	return &Ring{buf: make([]api.Event, n)}
}

// Append records ev. Safe for concurrent callers; matches the api.Config
// OnEvent signature so cmd/disruptoor can pass it directly.
func (r *Ring) Append(ev api.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Snapshot returns the events newest-first. Each call allocates a fresh slice;
// the caller may modify it freely.
func (r *Ring) Snapshot() []api.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.buf)
	}
	out := make([]api.Event, 0, size)
	// Walk backwards from the most recent.
	if r.full {
		for i := 0; i < len(r.buf); i++ {
			idx := (r.next - 1 - i + len(r.buf)) % len(r.buf)
			out = append(out, r.buf[idx])
		}
		return out
	}
	for i := 0; i < r.next; i++ {
		idx := r.next - 1 - i
		out = append(out, r.buf[idx])
	}
	return out
}
