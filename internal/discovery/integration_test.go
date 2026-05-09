//go:build integration

package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/disruptoor/internal/state"
)

// TestLiveEnclave hits the local Docker daemon. Requires a running Kurtosis
// enclave with ethereum-package participants. Skip when no enclave is up.
//
// Run with: go test -tags=integration ./internal/discovery/...
func TestLiveEnclave(t *testing.T) {
	enclaveID := os.Getenv("DISRUPTOOR_TEST_ENCLAVE_ID")
	if enclaveID == "" {
		t.Skip("set DISRUPTOOR_TEST_ENCLAVE_ID to run")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := NewService(logger, Config{EnclaveLabelValue: enclaveID})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	defer svc.Stop()

	all, err := svc.Resolve(ctx, state.Selector{All: true})
	require.NoError(t, err)
	require.NotEmpty(t, all, "expected user-service containers in enclave")

	bts, _ := json.MarshalIndent(all, "", "  ")
	t.Logf("resolved %d containers:\n%s", len(all), bts)

	for _, c := range all {
		require.NotZero(t, c.PID, "container %s has zero PID", c.Name)
		require.NotEmpty(t, c.IPs, "container %s has no IPs", c.Name)
	}

	el, err := svc.Resolve(ctx, state.Selector{
		Match: map[string][]string{"client-type": {"execution"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, el, "expected at least one EL participant")
	for _, c := range el {
		require.Equal(t, "execution",
			c.Labels["com.kurtosistech.custom.ethereum-package.client-type"])
	}
}
