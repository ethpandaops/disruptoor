// Package api exposes the disruptoor HTTP API and reconciles desired
// state against the live system. Every PUT /v1/state replaces the entire
// applied state: backends are cleared, the new state is resolved against
// Docker, and applied in order (conntrack flush → iptables → tc).
//
// The flush-before-iptables ordering is intentional. `ss -K` inside
// conntrack.Flush kills established sockets, which makes the kernel emit
// FIN/RST to the peer. If we installed OUTPUT DROP rules first, those
// teardown packets would be dropped by our own chain and the peer would
// keep us in its connected-peer list until app-layer keepalives expire
// (minutes on most CL/EL clients). Flushing first lets the teardown
// escape; OUTPUT DROP then closes the door against new connections.
//
// Failure mode: on any apply error, all backends are cleared so the
// system ends up in a known-empty state; the user is expected to re-PUT.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ethpandaops/disruptoor/internal/backend"
	"github.com/ethpandaops/disruptoor/internal/backend/conntrack"
	"github.com/ethpandaops/disruptoor/internal/backend/iptables"
	"github.com/ethpandaops/disruptoor/internal/backend/tc"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/state"
)

// Config holds dependencies and tunables for the HTTP service.
type Config struct {
	Addr      string // listen address, e.g. ":7700"
	Discovery discovery.Service
	Iptables  iptables.Service
	TC        tc.Service
	Conntrack conntrack.Service
	// ExtraRoutes, if non-nil, is invoked after the v1 API routes are
	// registered on the internal mux. Lets a caller (e.g. cmd/disruptoor) mount
	// the webui on the same listener so the browser can call /v1/* same-origin.
	ExtraRoutes func(mux *http.ServeMux)
	// OnEvent, if non-nil, is invoked synchronously after every state-mutation
	// attempt (apply success/failure or clear). Implementations must not block.
	OnEvent func(Event)
}

// EventKind classifies state-mutation events emitted via Config.OnEvent.
type EventKind string

const (
	// EventApplied means the desired state was successfully applied.
	EventApplied EventKind = "applied"
	// EventCleared means the kernel state was cleared.
	EventCleared EventKind = "cleared"
	// EventApplyFailed means an apply was attempted but failed; the kernel
	// has been rolled back to empty.
	EventApplyFailed EventKind = "apply_failed"
)

// Event describes one observed state-mutation.
type Event struct {
	Kind       EventKind   `json:"kind"`
	At         time.Time   `json:"at"`
	Source     string      `json:"source,omitempty"` // "http", "config", "internal"
	RemoteAddr string      `json:"remote_addr,omitempty"`
	State      state.State `json:"state,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Service runs the HTTP listener and owns the reconciliation lock.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	// Apply pushes a desired state through the same validate-then-apply
	// pipeline as PUT /v1/state. Used at startup for --config; on failure
	// the kernel is rolled back to empty.
	Apply(ctx context.Context, desired state.State) error
	// GetState returns the currently applied state. Safe for concurrent use.
	GetState() state.State
}

// NewService validates cfg and returns a Service. Start does the actual
// listen.
func NewService(logger *slog.Logger, cfg Config) (Service, error) {
	if logger == nil {
		return nil, errors.New("logger required")
	}
	if cfg.Addr == "" {
		return nil, errors.New("addr required")
	}
	if cfg.Discovery == nil || cfg.Iptables == nil || cfg.TC == nil || cfg.Conntrack == nil {
		return nil, errors.New("all backends and discovery required")
	}
	return &service{
		logger: logger.With(slog.String("component", "api")),
		cfg:    cfg,
	}, nil
}

type service struct {
	logger *slog.Logger
	cfg    Config
	srv    *http.Server

	mu      sync.Mutex
	applied state.State
}

func (s *service) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/state", s.handleGetState)
	mux.HandleFunc("PUT /v1/state", s.handlePutState)
	mux.HandleFunc("POST /v1/state/clear", s.handleClear)
	if s.cfg.ExtraRoutes != nil {
		s.cfg.ExtraRoutes(mux)
	}

	s.srv = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	s.logger.InfoContext(ctx, "listening", slog.String("addr", ln.Addr().String()))

	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.ErrorContext(ctx, "http server died", slog.String("error", err.Error()))
		}
	}()
	return nil
}

func (s *service) Stop() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// Apply validates and applies desired state outside the HTTP path. Used by
// the --config startup flag; on failure the kernel is rolled back to empty
// (same contract as PUT /v1/state).
func (s *service) Apply(ctx context.Context, desired state.State) error {
	if err := desired.Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyLocked(ctx, desired); err != nil {
		s.applied = state.State{}
		s.emit(Event{Kind: EventApplyFailed, At: time.Now(), Source: "config", Error: err.Error()})
		return err
	}
	s.applied = desired
	s.emit(Event{Kind: EventApplied, At: time.Now(), Source: "config", State: desired})
	return nil
}

// GetState returns a copy of the currently applied state. Safe for concurrent
// callers; the returned State shares no mutable slices with the service.
func (s *service) GetState() state.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.applied)
}

func (s *service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *service) handleGetState(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	current := s.applied
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, current)
}

func (s *service) handlePutState(w http.ResponseWriter, r *http.Request) {
	var desired state.State
	if err := json.NewDecoder(r.Body).Decode(&desired); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := desired.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("validate: %w", err))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.applyLocked(r.Context(), desired); err != nil {
		// applyLocked invokes clearLocked on every failure path so the
		// kernel ends up in a known-empty state; reflect that in s.applied
		// so GET /v1/state stops advertising the previous desired state.
		s.applied = state.State{}
		s.logger.ErrorContext(r.Context(), "apply failed; rolled back to empty",
			slog.String("error", err.Error()))
		s.emit(Event{Kind: EventApplyFailed, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr, Error: err.Error()})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.applied = desired
	s.emit(Event{Kind: EventApplied, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr, State: desired})
	writeJSON(w, http.StatusOK, desired)
}

func (s *service) handleClear(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clearLocked(r.Context()); err != nil {
		// Same truthfulness reasoning as handlePutState: the kernel may now
		// hold a partially-cleared mix that doesn't match s.applied, so
		// stop advertising the prior state.
		s.applied = state.State{}
		s.emit(Event{Kind: EventApplyFailed, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr, Error: err.Error()})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.applied = state.State{}
	s.emit(Event{Kind: EventCleared, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr})
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (s *service) emit(ev Event) {
	if s.cfg.OnEvent == nil {
		return
	}
	s.cfg.OnEvent(ev)
}

// cloneState returns a shallow-deep copy: top-level slices are fresh so callers
// can safely range/sort the result without seeing future mutations under our
// lock. Selector maps inside groups are not deep-copied because no caller
// mutates them in practice.
func cloneState(s state.State) state.State {
	out := state.State{}
	if len(s.Partitions) > 0 {
		out.Partitions = make([]state.Partition, len(s.Partitions))
		copy(out.Partitions, s.Partitions)
	}
	if len(s.Shaping) > 0 {
		out.Shaping = make([]state.Shaping, len(s.Shaping))
		copy(out.Shaping, s.Shaping)
	}
	return out
}

// applyLocked runs the full apply pipeline. Caller must hold s.mu.
func (s *service) applyLocked(ctx context.Context, desired state.State) error {
	resolvedPartitions, err := s.resolvePartitions(ctx, desired.Partitions)
	if err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("resolve partitions: %w", err)
	}
	resolvedShaping, err := s.resolveShaping(ctx, desired.Shaping)
	if err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("resolve shaping: %w", err)
	}

	// Flush BEFORE installing drop rules so `ss -K`'s FIN/RST can escape
	// to the peer; see package comment. Errors are non-fatal — partitions
	// still take effect, they just bite less cleanly.
	if err := s.cfg.Conntrack.Flush(ctx, resolvedPartitions); err != nil {
		s.logger.WarnContext(ctx, "conntrack flush errored; partitions may take time to bite",
			slog.String("error", err.Error()))
	}
	if err := s.cfg.Iptables.Apply(ctx, resolvedPartitions); err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("iptables apply: %w", err)
	}
	if err := s.cfg.TC.Apply(ctx, resolvedShaping); err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("tc apply: %w", err)
	}
	return nil
}

// clearLocked clears every backend, swallowing per-backend errors so a
// failure in one doesn't prevent the others from being cleaned. Returns
// the first error observed.
func (s *service) clearLocked(ctx context.Context) error {
	var firstErr error
	if err := s.cfg.TC.Clear(ctx); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("tc clear: %w", err)
	}
	if err := s.cfg.Iptables.Clear(ctx); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("iptables clear: %w", err)
	}
	return firstErr
}

func (s *service) resolvePartitions(ctx context.Context, ps []state.Partition) ([]backend.ResolvedPartition, error) {
	defaultScope := []string{"cl_p2p", "el_p2p"}
	out := make([]backend.ResolvedPartition, 0, len(ps))
	for _, p := range ps {
		groups, err := s.cfg.Discovery.ResolveGroups(ctx, p.Groups)
		if err != nil {
			return nil, fmt.Errorf("partition %q: %w", p.Name, err)
		}
		out = append(out, backend.ResolvedPartition{
			Name:      p.Name,
			Groups:    groups,
			Scope:     p.EffectiveScope(defaultScope),
			Symmetric: p.IsSymmetric(),
		})
	}
	return out, nil
}

func (s *service) resolveShaping(ctx context.Context, sh []state.Shaping) ([]backend.ResolvedShaping, error) {
	out := make([]backend.ResolvedShaping, 0, len(sh))
	for _, r := range sh {
		// Validate guarantees r.Target is non-nil and Between is empty.
		matched, err := s.cfg.Discovery.Resolve(ctx, *r.Target)
		if err != nil {
			return nil, fmt.Errorf("shaping %q: %w", r.Name, err)
		}
		out = append(out, backend.ResolvedShaping{
			Name:      r.Name,
			Target:    matched,
			Direction: r.Direction,
			Delay:     r.Delay,
			Jitter:    r.Jitter,
			Loss:      r.Loss,
			Bandwidth: r.Bandwidth,
		})
	}
	return out, nil
}

// statusRecorder wraps http.ResponseWriter to capture the status code that
// downstream handlers wrote, so the request-log middleware can include it.
// WriteHeader is the only hook needed because handlers that never call it
// implicitly write 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests is HTTP middleware that emits one INFO log per processed
// request. Symmetric with the backend install/remove INFO lines: every
// state-mutation call site is now visible in logs without per-handler
// boilerplate.
func (s *service) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.InfoContext(r.Context(), "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
