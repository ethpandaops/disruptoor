// Package handlers implements the per-page request handlers for the disruptoor
// webui. Each .go file in the package owns one page (index, partitions, …).
// Handlers depend only on small interfaces (StateProvider, DiscoveryProvider,
// EventProvider) so they're easy to test without spinning up real backends.
package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/api"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/state"
	"github.com/ethpandaops/disruptoor/internal/webui/server"
)

// StateProvider is the slice of api.Service we need: read-only access to the
// applied state.
type StateProvider interface {
	GetState() state.State
}

// DiscoveryProvider is the slice of discovery.Service we need.
type DiscoveryProvider interface {
	EnclaveID() string
	Resolve(ctx context.Context, sel state.Selector) ([]discovery.Container, error)
	ResolveGroups(ctx context.Context, sels []state.Selector) ([][]discovery.Container, error)
}

// EventProvider exposes the recent state-mutation log to the UI.
type EventProvider interface {
	Snapshot() []api.Event
}

// Deps is everything a Handler needs. Pass via New.
type Deps struct {
	Engine    *server.Engine
	State     StateProvider
	Discovery DiscoveryProvider
	Events    EventProvider
}

// Handler is the single struct each page handler hangs off. It does not store
// per-request state; methods are pure projections over Deps + ctx.
type Handler struct {
	logger    *slog.Logger
	engine    *server.Engine
	state     StateProvider
	discovery DiscoveryProvider
	events    EventProvider
}

// New constructs a Handler.
func New(logger *slog.Logger, deps Deps) *Handler {
	return &Handler{
		logger:    logger.With(slog.String("component", "webui-handlers")),
		engine:    deps.Engine,
		state:     deps.State,
		discovery: deps.Discovery,
		events:    deps.Events,
	}
}

// writeJSON is the small JSON-response helper used by /webui/api/* endpoints.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONError is the error twin of writeJSON.
func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
