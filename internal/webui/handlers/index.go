package handlers

import (
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/api"
	"github.com/ethpandaops/disruptoor/internal/state"
)

// IndexPage is the dashboard payload.
type IndexPage struct {
	EnclaveID       string
	PartitionsCount int
	ShapingCount    int
	Partitions      []state.Partition
	Shaping         []state.Shaping
	RecentEvents    []api.Event // up to 5, newest first
}

// Index renders the dashboard at /.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	cur := h.state.GetState()
	page := &IndexPage{
		EnclaveID:       h.discovery.EnclaveID(),
		PartitionsCount: len(cur.Partitions),
		ShapingCount:    len(cur.Shaping),
		Partitions:      cur.Partitions,
		Shaping:         cur.Shaping,
	}
	if h.events != nil {
		evs := h.events.Snapshot()
		if len(evs) > 5 {
			evs = evs[:5]
		}
		page.RecentEvents = evs
	}
	data := h.engine.InitPageData(r, "dashboard", "/", "Dashboard")
	data.Data = page
	h.engine.Render(w, r, []string{"index/index.html"}, data)
}
