package handlers

import (
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// PartitionsPage is the payload for /partitions.
type PartitionsPage struct {
	Partitions []PartitionView
	Isolations []IsolationView
}

// PartitionView is a UI-friendly projection of state.Partition. It computes
// the effective scope and symmetric flag so the template doesn't have to.
type PartitionView struct {
	Name      string
	Groups    []state.Selector
	Scope     []string
	Symmetric bool
}

// IsolationView is a UI-friendly projection of state.Isolation with the
// effective scope pre-computed.
type IsolationView struct {
	Name   string
	Target state.Selector
	Scope  []string
}

// Partitions renders the partitions page at /partitions.
func (h *Handler) Partitions(w http.ResponseWriter, r *http.Request) {
	cur := h.state.GetState()
	defaultScope := []string{state.ScopeCLP2P, state.ScopeELP2P}
	views := make([]PartitionView, 0, len(cur.Partitions))
	for _, p := range cur.Partitions {
		views = append(views, PartitionView{
			Name:      p.Name,
			Groups:    p.Groups,
			Scope:     p.EffectiveScope(defaultScope),
			Symmetric: p.IsSymmetric(),
		})
	}
	isolations := make([]IsolationView, 0, len(cur.Isolations))
	for _, iso := range cur.Isolations {
		view := IsolationView{
			Name:  iso.Name,
			Scope: iso.EffectiveScope(defaultScope),
		}
		if iso.Target != nil {
			view.Target = *iso.Target
		}
		isolations = append(isolations, view)
	}
	data := h.engine.InitPageData(r, "partitions", "/partitions", "Partitions")
	data.Data = &PartitionsPage{Partitions: views, Isolations: isolations}
	h.engine.Render(w, r, []string{"partitions/partitions.html"}, data)
}
