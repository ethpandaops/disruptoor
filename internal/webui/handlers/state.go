package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// StatePage is the payload for /state.
type StatePage struct {
	StateJSON string // pretty-printed for the editor textarea
}

// StateEditor renders the raw state editor page at /state.
func (h *Handler) StateEditor(w http.ResponseWriter, r *http.Request) {
	cur := h.state.GetState()
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		// extremely unlikely — state is well-typed — but fall back to "{}"
		// so the editor still renders.
		b = []byte("{}")
	}
	data := h.engine.InitPageData(r, "state", "/state", "State")
	data.Data = &StatePage{StateJSON: string(b)}
	h.engine.Render(w, r, []string{"state/state.html"}, data)
}

// APIResolve resolves a single Selector (POSTed as JSON body) against the
// enclave. Used by the partitions/shaping forms to preview how many
// containers a candidate selector matches before submitting.
func (h *Handler) APIResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var sel state.Selector
	if err := json.NewDecoder(r.Body).Decode(&sel); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	matched, err := h.discovery.Resolve(ctx, sel)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"matched": projectContainers(matched),
		"count":   len(matched),
	})
}
