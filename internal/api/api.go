// Package api exposes the disruptoor HTTP API and reconciles desired
// state against the live system. Every PUT /v1/state replaces the entire
// applied state: the new state is resolved against Docker, stale backend
// rules are cleared, then the new state is applied in order (conntrack
// flush → iptables → tc).
//
// The clear-before-flush and flush-before-iptables ordering is intentional.
// Old drop rules must be removed before `ss -K` runs, and `ss -K` inside
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
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
	s.applied = cloneState(desired)
	s.emit(Event{Kind: EventApplied, At: time.Now(), Source: "config", State: cloneState(desired)})
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
	current := s.GetState()
	etag, err := StateETag(current)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, current)
}

func (s *service) handlePutState(w http.ResponseWriter, r *http.Request) {
	var desired state.State
	if err := decodeState(r.Body, &desired); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := desired.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("validate: %w", err))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	currentETag, err := StateETag(s.applied)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ifMatchAllows(r.Header.Get("If-Match"), currentETag) {
		w.Header().Set("ETag", currentETag)
		writeError(w, http.StatusPreconditionFailed, errors.New("state changed since it was read; reload and retry"))
		return
	}

	nextApplied := cloneState(desired)
	newETag, err := StateETag(nextApplied)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := s.applyLocked(r.Context(), desired); err != nil {
		// applyLocked invokes clearLocked on every failure path so the
		// kernel ends up in a known-empty state; reflect that in s.applied
		// so GET /v1/state stops advertising the previous desired state.
		s.applied = state.State{}
		s.logger.ErrorContext(r.Context(), "apply failed; rolled back to empty",
			slog.String("error", err.Error()))
		s.emit(Event{Kind: EventApplyFailed, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr, Error: err.Error()})
		writeError(w, http.StatusInternalServerError, errors.New("apply failed; rolled back to empty state"))
		return
	}
	s.applied = nextApplied
	s.emit(Event{Kind: EventApplied, At: time.Now(), Source: "http", RemoteAddr: r.RemoteAddr, State: cloneState(desired)})
	w.Header().Set("ETag", newETag)
	writeJSON(w, http.StatusOK, s.applied)
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

// cloneState returns a deep copy of State's mutable fields.
func cloneState(s state.State) state.State {
	out := state.State{}
	if len(s.Partitions) > 0 {
		out.Partitions = make([]state.Partition, len(s.Partitions))
		for i, p := range s.Partitions {
			out.Partitions[i] = state.Partition{
				Name:      p.Name,
				Groups:    cloneSelectors(p.Groups),
				Scope:     cloneStrings(p.Scope),
				Symmetric: cloneBoolPtr(p.Symmetric),
			}
		}
	}
	if len(s.Isolations) > 0 {
		out.Isolations = make([]state.Isolation, len(s.Isolations))
		for i, iso := range s.Isolations {
			out.Isolations[i] = state.Isolation{
				Name:   iso.Name,
				Target: cloneSelectorPtr(iso.Target),
				Scope:  cloneStrings(iso.Scope),
			}
		}
	}
	if len(s.Shaping) > 0 {
		out.Shaping = make([]state.Shaping, len(s.Shaping))
		for i, sh := range s.Shaping {
			out.Shaping[i] = state.Shaping{
				Name:      sh.Name,
				Target:    cloneSelectorPtr(sh.Target),
				Between:   cloneSelectors(sh.Between),
				Scope:     cloneStrings(sh.Scope),
				Direction: sh.Direction,
				Delay:     sh.Delay,
				Jitter:    sh.Jitter,
				Loss:      sh.Loss,
				Bandwidth: sh.Bandwidth,
			}
		}
	}
	return out
}

func cloneSelectors(in []state.Selector) []state.Selector {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.Selector, len(in))
	for i, sel := range in {
		out[i] = cloneSelector(sel)
	}
	return out
}

func cloneSelectorPtr(in *state.Selector) *state.Selector {
	if in == nil {
		return nil
	}
	out := cloneSelector(*in)
	return &out
}

func cloneSelector(in state.Selector) state.Selector {
	out := state.Selector{All: in.All}
	if len(in.Match) > 0 {
		out.Match = make(map[string][]string, len(in.Match))
		for k, values := range in.Match {
			out.Match[k] = cloneStrings(values)
		}
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// StateETag returns a strong ETag for the canonical JSON form of s.
func StateETag(s state.State) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal state etag: %w", err)
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`, nil
}

func ifMatchAllows(header, current string) bool {
	if header == "" {
		return true
	}
	for _, token := range strings.Split(header, ",") {
		token = strings.TrimSpace(token)
		if token == "*" || token == current {
			return true
		}
	}
	return false
}

func decodeState(r io.Reader, out *state.State) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values in request body")
		}
		return err
	}
	return nil
}

// applyLocked runs the full apply pipeline. Caller must hold s.mu.
func (s *service) applyLocked(ctx context.Context, desired state.State) error {
	resolvedPartitions, err := s.resolvePartitions(ctx, desired.Partitions)
	if err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("resolve partitions: %w", err)
	}
	resolvedIsolations, err := s.resolveIsolations(ctx, desired.Isolations)
	if err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("resolve isolations: %w", err)
	}
	resolvedPartitions = append(resolvedPartitions, resolvedIsolations...)
	resolvedShaping, err := s.resolveShaping(ctx, desired.Shaping)
	if err != nil {
		_ = s.clearLocked(ctx)
		return fmt.Errorf("resolve shaping: %w", err)
	}

	// Clear stale rules before conntrack teardown. Old partition drops can
	// otherwise block the FIN/RST packets emitted by the flush step below.
	if err := s.clearLocked(ctx); err != nil {
		return fmt.Errorf("clear previous state: %w", err)
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
		for i, group := range groups {
			if len(group) == 0 {
				return nil, fmt.Errorf("partition %q group %d: selector matched no containers", p.Name, i)
			}
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

// resolveIsolations expands each isolation into a two-group partition:
// the target selector vs the complement of its match set. Reusing
// ResolvedPartition means the conntrack and iptables backends need no
// isolation-specific code paths.
func (s *service) resolveIsolations(ctx context.Context, isos []state.Isolation) ([]backend.ResolvedPartition, error) {
	defaultScope := []string{"cl_p2p", "el_p2p"}
	out := make([]backend.ResolvedPartition, 0, len(isos))
	for _, iso := range isos {
		// Validate guarantees iso.Target is non-nil, non-empty, and not "all".
		target, err := s.cfg.Discovery.Resolve(ctx, *iso.Target)
		if err != nil {
			return nil, fmt.Errorf("isolation %q: %w", iso.Name, err)
		}
		if len(target) == 0 {
			return nil, fmt.Errorf("isolation %q: target matched no containers", iso.Name)
		}
		everyone, err := s.cfg.Discovery.Resolve(ctx, state.Selector{All: true})
		if err != nil {
			return nil, fmt.Errorf("isolation %q: resolve enclave containers: %w", iso.Name, err)
		}
		rest := subtractContainers(everyone, target)
		if len(rest) == 0 {
			return nil, fmt.Errorf("isolation %q: target matches every container in the enclave; nothing to isolate from", iso.Name)
		}
		out = append(out, backend.ResolvedPartition{
			Name:      iso.Name,
			Groups:    [][]discovery.Container{target, rest},
			Scope:     iso.EffectiveScope(defaultScope),
			Symmetric: true,
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

// subtractContainers returns the containers in from that are not in remove,
// keyed by container ID. Order of from is preserved.
func subtractContainers(from, remove []discovery.Container) []discovery.Container {
	removeIDs := make(map[string]struct{}, len(remove))
	for _, c := range remove {
		removeIDs[c.ID] = struct{}{}
	}
	out := make([]discovery.Container, 0, len(from))
	for _, c := range from {
		if _, drop := removeIDs[c.ID]; drop {
			continue
		}
		out = append(out, c)
	}
	return out
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
