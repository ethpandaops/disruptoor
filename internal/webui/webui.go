// Package webui mounts a small Bootstrap-based control panel for disruptoor.
// It does not run its own listener — instead it registers routes on the same
// mux as the v1 API (via api.Config.ExtraRoutes) so the browser-side fetches
// against /v1/* stay same-origin.
//
// Layout follows spamoor's webui module: a server sub-package owns templates
// and the static-asset handler; a handlers sub-package owns per-page logic.
package webui

import (
	"embed"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ethpandaops/disruptoor/internal/webui/handlers"
	"github.com/ethpandaops/disruptoor/internal/webui/server"
)

// The "all:" prefix is required because the layout templates live under
// templates/_layout/, and Go's embed skips files and directories starting
// with `_` (or `.`) by default.
//
//go:embed all:static
var staticFS embed.FS

//go:embed all:templates
var templatesFS embed.FS

// Config bundles everything the webui needs to render pages and answer its own
// JSON helper endpoints.
type Config struct {
	SiteName     string
	Version      string
	AssetVersion string
	Debug        bool

	// State is the live api.Service (or any stand-in for tests). Used for
	// reading the applied state and resolving partitions/shaping.
	State handlers.StateProvider
	// Discovery resolves selectors and lists enclave containers.
	Discovery handlers.DiscoveryProvider
	// Events provides recent state-mutation events (in-memory ring).
	Events handlers.EventProvider
}

// Service registers the webui's routes on a caller-supplied mux. It's not a
// long-running service in the api.Service sense: registration is one-shot and
// requests are served by the parent listener.
type Service interface {
	RegisterRoutes(mux *http.ServeMux)
}

// NewService validates cfg and returns a Service ready to mount.
func NewService(logger *slog.Logger, cfg Config) (Service, error) {
	if logger == nil {
		return nil, errors.New("logger required")
	}
	if cfg.State == nil {
		return nil, errors.New("state provider required")
	}
	if cfg.Discovery == nil {
		return nil, errors.New("discovery provider required")
	}
	// Events is optional — if unset, the events page just shows an empty list.
	if cfg.Events == nil {
		logger.Warn("web UI event provider missing; events page will be empty")
	}

	engine, err := server.NewEngine(logger, server.Config{
		SiteName:     cfg.SiteName,
		Version:      cfg.Version,
		AssetVersion: cfg.AssetVersion,
		Debug:        cfg.Debug,
	}, staticFS, templatesFS)
	if err != nil {
		return nil, err
	}
	frontend := server.NewFrontend(logger, engine)
	h := handlers.New(logger, handlers.Deps{
		Engine:    engine,
		State:     cfg.State,
		Discovery: cfg.Discovery,
		Events:    cfg.Events,
	})
	return &service{
		logger:   logger.With(slog.String("component", "webui")),
		frontend: frontend,
		handlers: h,
	}, nil
}

type service struct {
	logger   *slog.Logger
	frontend *server.Frontend
	handlers *handlers.Handler
}

// RegisterRoutes mounts every webui page + helper API endpoint on mux. Pages
// live at the root paths; the static-asset handler is registered last so it
// only matches files (e.g. /css/..., /js/..., /favicon.ico) and never shadows
// a page route.
func (s *service) RegisterRoutes(mux *http.ServeMux) {
	// HTML pages.
	mux.HandleFunc("GET /{$}", s.handlers.Index)
	mux.HandleFunc("GET /partitions", s.handlers.Partitions)
	mux.HandleFunc("GET /shaping", s.handlers.Shaping)
	mux.HandleFunc("GET /containers", s.handlers.Containers)
	mux.HandleFunc("GET /events", s.handlers.Events)
	mux.HandleFunc("GET /state", s.handlers.StateEditor)

	// JSON helpers used by the UI's JS but not exposed in the public v1 API
	// (because they don't fit the desired-state model — they're read-only
	// projections enriched with discovery data).
	mux.HandleFunc("GET /webui/api/containers", s.handlers.APIContainers)
	mux.HandleFunc("GET /webui/api/events", s.handlers.APIEvents)
	mux.HandleFunc("POST /webui/api/resolve", s.handlers.APIResolve)

	// Static assets. The frontend handler internally calls the engine's
	// 404 for unknown paths, so we mount it on the catch-all GET / route.
	mux.Handle("GET /css/", s.frontend)
	mux.Handle("GET /js/", s.frontend)
	mux.Handle("GET /img/", s.frontend)
	mux.Handle("GET /webfonts/", s.frontend)
	mux.Handle("GET /favicon.ico", s.frontend)
}
