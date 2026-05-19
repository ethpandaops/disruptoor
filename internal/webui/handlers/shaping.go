package handlers

import (
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// ShapingPage is the payload for /shaping.
type ShapingPage struct {
	Rules []state.Shaping
}

// Shaping renders the shaping rules page at /shaping.
func (h *Handler) Shaping(w http.ResponseWriter, r *http.Request) {
	cur := h.state.GetState()
	data := h.engine.InitPageData(r, "shaping", "/shaping", "Shaping")
	data.Data = &ShapingPage{Rules: cur.Shaping}
	h.engine.Render(w, r, []string{"shaping/shaping.html"}, data)
}
