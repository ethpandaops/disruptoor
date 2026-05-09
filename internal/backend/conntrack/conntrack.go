// Package conntrack tears down already-open sessions to peers that a
// freshly applied partition has now blocked. Without this, DROP rules
// only affect new packets — existing sessions linger in the kernel for
// 60+ seconds, making partitions look "soft" for that window.
//
// Teardown is scoped to the same (peer-ip, port, proto) tuples the
// partition itself blocks. Killing every socket to a peer IP would also
// drop in-flight RPC/engine/metrics traffic to that peer for partitions
// scoped to p2p only — exactly the kind of "soft control-plane damage"
// the partition scope was designed to avoid.
//
// We do two things per (member, peer-ip, port) tuple, inside the
// member's netns:
//
//  1. ss -K dst <peer-ip>:<port>                      — kills sockets
//  2. conntrack -D --orig-dst <ip> -p <proto> --dport — clears NAT/CT
//
// Both are best-effort; failures are logged but don't fail the apply.
// This module never installs anything that needs cleanup later, so there
// is no Clear method.
package conntrack

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/ethpandaops/disruptoor/internal/backend"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/netns"
	"github.com/ethpandaops/disruptoor/internal/scope"
)

// Service runs teardown for the just-applied partition plan.
type Service interface {
	Flush(ctx context.Context, plan []backend.ResolvedPartition) error
}

// NewService constructs a conntrack service.
func NewService(logger *slog.Logger, enterer netns.Enterer) Service {
	return &service{
		logger:  logger.With(slog.String("component", "conntrack")),
		enterer: enterer,
	}
}

type service struct {
	logger  *slog.Logger
	enterer netns.Enterer
}

func (s *service) Flush(ctx context.Context, plan []backend.ResolvedPartition) error {
	for _, p := range plan {
		for groupIdx, group := range p.Groups {
			peers := otherGroupContainers(p.Groups, groupIdx)
			for _, member := range group {
				for _, peer := range peers {
					ports := scope.FilterPorts(peer, p.Scope)
					if len(ports) == 0 {
						continue
					}
					for _, port := range ports {
						for _, ip := range peer.IPs {
							s.killSocket(ctx, member, ip.String(), port)
							s.deleteConntrack(ctx, member, ip.String(), port)
						}
					}
				}
			}
		}
	}
	return nil
}

func (s *service) killSocket(ctx context.Context, member discovery.Container, peerIP string, port discovery.Port) {
	// ss -K only kills TCP sockets meaningfully; UDP "sockets" are stateless
	// from netfilter's point of view, so for UDP we rely on the conntrack -D
	// teardown below.
	if !strings.EqualFold(port.Protocol, "TCP") {
		return
	}
	// JoinHostPort brackets IPv6 addresses so ss parses them correctly.
	dst := net.JoinHostPort(peerIP, strconv.Itoa(int(port.Number)))
	if _, _, err := s.enterer.Run(ctx, member.PID, "ss", "-K", "dst", dst); err != nil {
		s.logger.DebugContext(ctx, "ss -K failed (likely no matching sockets)",
			slog.String("container", member.Name),
			slog.String("peer", dst),
			slog.String("error", err.Error()))
	}
}

func (s *service) deleteConntrack(ctx context.Context, member discovery.Container, peerIP string, port discovery.Port) {
	proto := strings.ToLower(port.Protocol)
	args := []string{
		"-D",
		"--orig-dst", peerIP,
		"-p", proto,
		"--dport", strconv.Itoa(int(port.Number)),
	}
	if _, _, err := s.enterer.Run(ctx, member.PID, "conntrack", args...); err != nil {
		s.logger.DebugContext(ctx, "conntrack -D failed (likely no matching entries)",
			slog.String("container", member.Name),
			slog.String("peer_ip", peerIP),
			slog.Int("port", int(port.Number)),
			slog.String("proto", proto),
			slog.String("error", err.Error()))
	}
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
