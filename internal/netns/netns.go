// Package netns runs commands inside a target container's network
// namespace via nsenter. The mount namespace stays the disruptoor
// container's own, so iptables/tc/conntrack binaries are resolved from
// disruptoor's image — targets don't need any tooling installed.
package netns

import (
	"context"
	"fmt"

	"github.com/ethpandaops/disruptoor/internal/runner"
)

// Enterer wraps a runner so callers can run commands inside a netns.
type Enterer interface {
	// Run executes name+args inside the network namespace of the process
	// identified by pid. Returns captured stdout, stderr, and the underlying
	// error (which carries an *exec.ExitError when name returned non-zero).
	Run(ctx context.Context, pid int, name string, args ...string) (stdout, stderr []byte, err error)
}

// New returns an Enterer backed by r.
func New(r runner.Runner) Enterer {
	return enterer{r: r}
}

type enterer struct {
	r runner.Runner
}

func (e enterer) Run(ctx context.Context, pid int, name string, args ...string) ([]byte, []byte, error) {
	if pid <= 0 {
		return nil, nil, fmt.Errorf("invalid pid %d", pid)
	}
	full := make([]string, 0, len(args)+3)
	full = append(full, fmt.Sprintf("--net=/proc/%d/ns/net", pid), "--", name)
	full = append(full, args...)
	return e.r.Run(ctx, "nsenter", full...)
}
