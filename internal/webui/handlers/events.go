package handlers

import (
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/api"
)

// EventsPage is the payload for /events.
type EventsPage struct {
	Events []api.Event
}

// Events renders the event log page at /events.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	page := &EventsPage{}
	if h.events != nil {
		page.Events = h.events.Snapshot()
	}
	data := h.engine.InitPageData(r, "events", "/events", "Events")
	data.Data = page
	h.engine.Render(w, r, []string{"events/events.html"}, data)
}

// APIEvents is the JSON twin used by the dashboard's auto-refresh logic (and
// anything else that wants to poll without parsing HTML).
func (h *Handler) APIEvents(w http.ResponseWriter, r *http.Request) {
	if h.events == nil {
		writeJSON(w, http.StatusOK, []api.Event{})
		return
	}
	writeJSON(w, http.StatusOK, h.events.Snapshot())
}
