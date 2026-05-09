// Package backend defines the resolved-state types that backends consume.
// "Resolved" means selectors have already been turned into concrete
// container lists by the discovery service. Backends never call Docker
// themselves — they only see container PIDs, IPs, and port catalogs.
package backend

import "github.com/ethpandaops/disruptoor/internal/discovery"

// ResolvedPartition is a partition with its groups already expanded to
// containers. Symmetric and Scope are copied from the source state for
// convenience so backends don't need to look anything up.
type ResolvedPartition struct {
	Name      string
	Groups    [][]discovery.Container
	Scope     []string
	Symmetric bool
}

// ResolvedShaping is a shaping rule with its target expanded. v0 only
// supports single-target mode; "between" is rejected by state.Validate
// before reaching backends.
type ResolvedShaping struct {
	Name      string
	Target    []discovery.Container
	Direction string
	Delay     string
	Jitter    string
	Loss      string
	Bandwidth string
}

// AllTargets returns every container that appears in any group of any
// partition in plan. Used by clear/reconcile paths to know which netnses
// to visit.
func AllTargets(plan []ResolvedPartition) []discovery.Container {
	seen := make(map[string]struct{}, 16)
	out := make([]discovery.Container, 0, 16)
	for _, p := range plan {
		for _, g := range p.Groups {
			for _, c := range g {
				if _, dup := seen[c.ID]; dup {
					continue
				}
				seen[c.ID] = struct{}{}
				out = append(out, c)
			}
		}
	}
	return out
}
