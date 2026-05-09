// Package api exposes the disruptoor HTTP API and reconciles desired
// state against the live system. Every PUT /v1/state replaces the entire
// applied state: backends are cleared, the new state is resolved against
// Docker, and applied in order (iptables → conntrack flush → tc).
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
}

// Service runs the HTTP listener and owns the reconciliation lock.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	// Apply pushes a desired state through the same validate-then-apply
	// pipeline as PUT /v1/state. Used at startup for --config; on failure
	// the kernel is rolled back to empty.
	Apply(ctx context.Context, desired state.State) error
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

	s.srv = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           mux,
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
		return err
	}
	s.applied = desired
	return nil
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
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.applied = desired
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
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.applied = state.State{}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
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

	if err := s.cfg.Iptables.Apply(ctx, resolvedPartitions); err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("iptables apply: %w", err)
	}
	if err := s.cfg.Conntrack.Flush(ctx, resolvedPartitions); err != nil {
		s.logger.WarnContext(ctx, "conntrack flush errored; partitions may take time to bite",
			slog.String("error", err.Error()))
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
