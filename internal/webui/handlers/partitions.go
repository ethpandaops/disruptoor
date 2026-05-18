package handlers

import (
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// PartitionsPage is the payload for /partitions.
type PartitionsPage struct {
	Partitions []PartitionView
}

// PartitionView is a UI-friendly projection of state.Partition. It computes
// the effective scope and symmetric flag so the template doesn't have to.
type PartitionView struct {
	Name      string
	Groups    []state.Selector
	Scope     []string
	Symmetric bool
}

// Partitions renders the partitions page at /partitions.
func (h *Handler) Partitions(w http.ResponseWriter, r *http.Request) {
	cur := h.state.GetState()
	views := make([]PartitionView, 0, len(cur.Partitions))
	for _, p := range cur.Partitions {
		views = append(views, PartitionView{
			Name:      p.Name,
			Groups:    p.Groups,
			Scope:     p.EffectiveScope([]string{state.ScopeCLP2P, state.ScopeELP2P}),
			Symmetric: p.IsSymmetric(),
		})
	}
	data := h.engine.InitPageData(r, "partitions", "/partitions", "Partitions")
	data.Data = &PartitionsPage{Partitions: views}
	h.engine.Render(w, r, []string{"partitions/partitions.html"}, data)
}
