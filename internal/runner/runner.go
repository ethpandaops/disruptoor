// Package runner shells out to external commands. The interface exists so
// backends can be unit-tested without a real Linux kernel underneath.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Runner runs an external command and returns captured stdout/stderr.
// Implementations must respect ctx cancellation.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// New returns a Runner backed by os/exec.
func New() Runner {
	return real{}
}

// ExitCode returns the exit status of err if it was an *exec.ExitError, or
// -1 when the error was something else. Useful for distinguishing expected
// non-zero exits ("rule does not exist") from genuine command failures.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var xerr *exec.ExitError
	if !errors.As(err, &xerr) {
		return -1
	}
	return xerr.ExitCode()
}

type real struct{}

func (real) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), stderr.Bytes(),
			fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
