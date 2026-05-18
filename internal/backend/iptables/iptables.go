// Package iptables installs hard partitions inside target containers'
// network namespaces. It owns a single named chain (DISRUPTOOR) per
// netns; clear flushes that chain and nothing else, so unrelated rules
// the user added by hand survive.
package iptables

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/ethpandaops/disruptoor/internal/backend"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/netns"
	"github.com/ethpandaops/disruptoor/internal/runner"
	"github.com/ethpandaops/disruptoor/internal/scope"
)

// ChainName is the netfilter chain disruptoor owns. We never touch rules
// outside this chain, and clear is just a flush of this chain.
const ChainName = "DISRUPTOOR"

// Service applies and clears partition rules across many target netnses.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
	Apply(ctx context.Context, plan []backend.ResolvedPartition) error
	Clear(ctx context.Context) error
}

// NewService constructs an iptables backend. enterer runs commands inside
// target netnses; recoverTargets is the set of containers whose chains we
// should attempt to flush at startup (typical: every user-service in the
// enclave) so a stale disruptoor restart doesn't leave orphaned rules.
func NewService(logger *slog.Logger, enterer netns.Enterer, recoverTargets func(ctx context.Context) ([]discovery.Container, error)) Service {
	return &service{
		logger:         logger.With(slog.String("component", "iptables")),
		enterer:        enterer,
		recoverTargets: recoverTargets,
	}
}

type service struct {
	logger         *slog.Logger
	enterer        netns.Enterer
	recoverTargets func(ctx context.Context) ([]discovery.Container, error)

	mu          sync.Mutex
	lastTargets []discovery.Container // containers we touched on the last apply
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
		if err := s.flushChainBestEffort(ctx, c); err != nil {
			s.logger.DebugContext(ctx, "startup recovery: flush failed (likely no chain)",
				slog.String("container", c.Name),
				slog.String("error", err.Error()))
		}
	}
	s.logger.InfoContext(ctx, "startup recovery complete",
		slog.Int("containers", len(targets)))
	return nil
}

func (s *service) Stop() error { return nil }

func (s *service) Apply(ctx context.Context, plan []backend.ResolvedPartition) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.clearTargetsLocked(ctx, s.lastTargets); err != nil {
		return fmt.Errorf("clear previous: %w", err)
	}
	s.lastTargets = nil

	if len(plan) == 0 {
		return nil
	}

	// Record the cleanup surface BEFORE we mutate any container's netfilter
	// state. ensureChain and applyPartition can fail partway through; if they
	// do, we still need Clear() to be able to flush the chains we touched.
	newTargets := backend.AllTargets(plan)
	s.lastTargets = newTargets

	for _, target := range newTargets {
		if err := s.ensureChain(ctx, target); err != nil {
			return fmt.Errorf("ensure chain on %s: %w", target.Name, err)
		}
	}

	for _, p := range plan {
		if !p.Symmetric {
			return fmt.Errorf("partition %q: asymmetric not supported in v0", p.Name)
		}
		if err := s.applyPartition(ctx, p); err != nil {
			return fmt.Errorf("partition %q: %w", p.Name, err)
		}
	}

	return nil
}

func (s *service) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clearTargetsLocked(ctx, s.lastTargets); err != nil {
		return err
	}
	s.lastTargets = nil
	return nil
}

func (s *service) clearTargetsLocked(ctx context.Context, targets []discovery.Container) error {
	var firstErr error
	for _, c := range targets {
		if err := s.flushChainBestEffort(ctx, c); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.logger.InfoContext(ctx, "removed partition rules",
			slog.String("member", c.Name))
	}
	return firstErr
}

// ensureChain idempotently creates the DISRUPTOOR chain in the target's
// filter table and a jump from OUTPUT into it. Both calls are safe to
// repeat: -N exits with code 1 when the chain already exists, which we
// treat as success; -C is a check that's used to gate the -A.
func (s *service) ensureChain(ctx context.Context, c discovery.Container) error {
	// Create chain (ignore "already exists" failure).
	_, _, err := s.enterer.Run(ctx, c.PID, "iptables", "-N", ChainName)
	if err != nil && runner.ExitCode(err) != 1 {
		return fmt.Errorf("create chain: %w", err)
	}

	// Flush so we start from a clean slate.
	if _, _, err := s.enterer.Run(ctx, c.PID, "iptables", "-F", ChainName); err != nil {
		return fmt.Errorf("flush chain: %w", err)
	}

	// Add jump from OUTPUT if not present.
	_, _, checkErr := s.enterer.Run(ctx, c.PID, "iptables", "-C", "OUTPUT", "-j", ChainName)
	if checkErr != nil {
		// not present (or some other error); try to add. Rely on the add to
		// surface a real failure.
		var xerr *exec.ExitError
		if !errors.As(checkErr, &xerr) {
			return fmt.Errorf("check OUTPUT jump: %w", checkErr)
		}
		if _, _, err := s.enterer.Run(ctx, c.PID, "iptables", "-A", "OUTPUT", "-j", ChainName); err != nil {
			return fmt.Errorf("add OUTPUT jump: %w", err)
		}
	}
	return nil
}

// flushChainBestEffort flushes the DISRUPTOOR chain in c's netns, ignoring
// "no such chain" errors (exit code 1 + specific stderr). The OUTPUT jump
// is left in place — having an empty chain to jump to costs nothing.
func (s *service) flushChainBestEffort(ctx context.Context, c discovery.Container) error {
	if c.PID == 0 {
		return nil
	}
	_, _, err := s.enterer.Run(ctx, c.PID, "iptables", "-F", ChainName)
	if err == nil {
		return nil
	}
	if runner.ExitCode(err) == 1 {
		return nil // chain didn't exist; that's fine
	}
	return err
}

// applyPartition installs egress drop rules on every container in every
// group, blocking traffic to peers in *other* groups on scope-matching
// peer ports. Symmetric semantics fall out: each side blocks its own
// egress, so the other side never sees a reply either.
func (s *service) applyPartition(ctx context.Context, p backend.ResolvedPartition) error {
	for groupIdx, group := range p.Groups {
		others := otherGroupContainers(p.Groups, groupIdx)
		for _, member := range group {
			if err := s.installMemberRules(ctx, member, others, p); err != nil {
				return fmt.Errorf("install on %s: %w", member.Name, err)
			}
		}
	}
	return nil
}

func (s *service) installMemberRules(ctx context.Context, member discovery.Container, peers []discovery.Container, p backend.ResolvedPartition) error {
	comment := "disruptoor:" + p.Name
	for _, peer := range peers {
		ports := scope.FilterPorts(peer, p.Scope)
		if len(ports) == 0 {
			s.logger.DebugContext(ctx, "no in-scope ports for peer; skipping",
				slog.String("partition", p.Name),
				slog.String("peer", peer.Name))
			continue
		}
		for _, port := range ports {
			for _, ip := range peer.IPs {
				args := []string{
					"-A", ChainName,
					"-p", strings.ToLower(port.Protocol),
					"-d", ip.String(),
					"--dport", strconv.Itoa(int(port.Number)),
					"-j", "DROP",
					"-m", "comment", "--comment", comment,
				}
				if _, _, err := s.enterer.Run(ctx, member.PID, "iptables", args...); err != nil {
					return fmt.Errorf("add rule peer=%s ip=%s port=%d/%s: %w",
						peer.Name, ip, port.Number, port.Protocol, err)
				}
			}
		}
	}
	s.logger.InfoContext(ctx, "installed partition rules",
		slog.String("partition", p.Name),
		slog.String("member", member.Name),
		slog.Int("peers", len(peers)))
	return nil
}

func otherGroupContainers(groups [][]discovery.Container, exceptIdx int) []discovery.Container {
	out := make([]discovery.Container, 0, 16)
	for i, g := range groups {
		if i == exceptIdx {
			continue
		}
		out = append(out, g...)
	}
	return out
}
