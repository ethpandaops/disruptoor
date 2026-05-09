// Package tc installs egress shaping (delay, jitter, loss, bandwidth) on
// target containers' eth0 via tc qdisc add. v0 supports "target" mode
// only — single container, blanket egress shaping. "between" (per-edge)
// requires tc filters and ifb devices and is rejected by state.Validate
// before reaching this package; the equivalent guard here is defence in
// depth.
package tc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ethpandaops/disruptoor/internal/backend"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/netns"
	"github.com/ethpandaops/disruptoor/internal/runner"
)

// DefaultIface is the in-container interface name we attach qdiscs to.
// Docker default for the bridge driver is eth0 on every container; we
// expose this as a constant rather than scanning ip-link to keep apply
// fast and predictable.
const DefaultIface = "eth0"

// Service applies and clears tc shaping across many target netnses.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	Apply(ctx context.Context, plan []backend.ResolvedShaping) error
	Clear(ctx context.Context) error
}

// NewService constructs a tc backend. recoverTargets is the set of
// containers whose root qdiscs should be cleared at startup (typical:
// every user-service in the enclave) so a stale disruptoor restart
// doesn't leave orphan shaping in place.
func NewService(logger *slog.Logger, enterer netns.Enterer, recoverTargets func(ctx context.Context) ([]discovery.Container, error)) Service {
	return &service{
		logger:         logger.With(slog.String("component", "tc")),
		enterer:        enterer,
		recoverTargets: recoverTargets,
		iface:          DefaultIface,
	}
}

type service struct {
	logger         *slog.Logger
	enterer        netns.Enterer
	recoverTargets func(ctx context.Context) ([]discovery.Container, error)
	iface          string

	mu          sync.Mutex
	lastTargets []discovery.Container
}

func (s *service) Start(ctx context.Context) error {
	if s.recoverTargets == nil {
		return nil
	}
	targets, err := s.recoverTargets(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "startup recovery: list targets failed",
			slog.String("error", err.Error()))
		return nil
	}
	for _, c := range targets {
		s.delRootBestEffort(ctx, c)
	}
	s.logger.InfoContext(ctx, "startup recovery complete",
		slog.Int("containers", len(targets)))
	return nil
}

func (s *service) Stop() error { return nil }

func (s *service) Apply(ctx context.Context, plan []backend.ResolvedShaping) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.lastTargets {
		s.delRootBestEffort(ctx, c)
	}
	s.lastTargets = nil

	if len(plan) == 0 {
		return nil
	}

	// Pre-flight: collect every container we're about to shape and bail
	// before any kernel mutation if the plan is internally inconsistent.
	newTargets := make([]discovery.Container, 0, len(plan))
	seen := make(map[string]struct{}, len(plan))
	for _, sh := range plan {
		if len(sh.Target) == 0 {
			// state.Validate rejects between mode and missing Target, so an
			// empty resolved Target here means the selector matched no
			// containers in the live enclave.
			return fmt.Errorf("shaping %q: target selector matched no containers", sh.Name)
		}
		for _, c := range sh.Target {
			if _, dup := seen[c.ID]; dup {
				return fmt.Errorf("shaping %q: container %s already shaped by another rule",
					sh.Name, c.Name)
			}
			seen[c.ID] = struct{}{}
			newTargets = append(newTargets, c)
		}
	}

	// Record the cleanup surface BEFORE installing any qdiscs. If
	// applyToContainer fails partway through, Clear() still needs to know
	// which containers may have a partial qdisc tree.
	s.lastTargets = newTargets

	for _, sh := range plan {
		for _, c := range sh.Target {
			if err := s.applyToContainer(ctx, c, sh); err != nil {
				return fmt.Errorf("shaping %q on %s: %w", sh.Name, c.Name, err)
			}
		}
	}

	return nil
}

func (s *service) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.lastTargets {
		s.delRootBestEffort(ctx, c)
	}
	s.lastTargets = nil
	return nil
}

// applyToContainer installs the qdisc tree:
//
//	tbf (root, handle 1:)  →  netem (parent 1:1, handle 10:)
//
// when bandwidth is set; otherwise just netem at root. Order matters:
// tbf shapes by token bucket, netem then injects delay/loss on whatever
// the bucket releases.
func (s *service) applyToContainer(ctx context.Context, c discovery.Container, sh backend.ResolvedShaping) error {
	hasBandwidth := sh.Bandwidth != ""
	hasNetem := sh.Delay != "" || sh.Jitter != "" || sh.Loss != ""

	if !hasBandwidth && !hasNetem {
		return fmt.Errorf("no shaping parameters set")
	}

	// Always start clean (idempotent under repeated applies).
	s.delRootBestEffort(ctx, c)

	if hasBandwidth {
		args := []string{"qdisc", "add", "dev", s.iface,
			"root", "handle", "1:", "tbf",
			"rate", sh.Bandwidth,
			"burst", "32kbit",
			"latency", "400ms",
		}
		if _, _, err := s.enterer.Run(ctx, c.PID, "tc", args...); err != nil {
			return fmt.Errorf("install tbf: %w", err)
		}
		if hasNetem {
			netemArgs := append([]string{"qdisc", "add", "dev", s.iface,
				"parent", "1:1", "handle", "10:", "netem"}, netemSpec(sh)...)
			if _, _, err := s.enterer.Run(ctx, c.PID, "tc", netemArgs...); err != nil {
				return fmt.Errorf("install netem under tbf: %w", err)
			}
		}
	} else {
		netemArgs := append([]string{"qdisc", "add", "dev", s.iface,
			"root", "handle", "1:", "netem"}, netemSpec(sh)...)
		if _, _, err := s.enterer.Run(ctx, c.PID, "tc", netemArgs...); err != nil {
			return fmt.Errorf("install netem: %w", err)
		}
	}

	s.logger.InfoContext(ctx, "applied shaping",
		slog.String("rule", sh.Name),
		slog.String("container", c.Name),
		slog.String("delay", sh.Delay),
		slog.String("loss", sh.Loss),
		slog.String("bandwidth", sh.Bandwidth))
	return nil
}

func netemSpec(sh backend.ResolvedShaping) []string {
	args := make([]string, 0, 6)
	if sh.Delay != "" {
		args = append(args, "delay", sh.Delay)
		if sh.Jitter != "" {
			args = append(args, sh.Jitter)
		}
	}
	if sh.Loss != "" {
		args = append(args, "loss", strings.TrimSuffix(sh.Loss, "%")+"%")
	}
	return args
}

// delRootBestEffort removes whatever root qdisc is in place, ignoring the
// "no such file" error tc returns when there's nothing to delete.
func (s *service) delRootBestEffort(ctx context.Context, c discovery.Container) {
	if c.PID == 0 {
		return
	}
	_, _, err := s.enterer.Run(ctx, c.PID, "tc", "qdisc", "del", "dev", s.iface, "root")
	if err == nil {
		return
	}
	// tc exits 2 when there's no qdisc to delete on Linux.
	exit := runner.ExitCode(err)
	if exit == 2 {
		return
	}
	s.logger.DebugContext(ctx, "tc qdisc del failed",
		slog.String("container", c.Name),
		slog.Int("exit_code", exit),
		slog.String("error", err.Error()))
}
