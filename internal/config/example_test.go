package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExampleConfigLoads guards against drift between examples/ and the
// loader/validator. If this test breaks, either the example or the schema
// changed; reconcile both.
func TestExampleConfigLoads(t *testing.T) {
	path, err := filepath.Abs("../../examples/disruption.yaml")
	require.NoError(t, err)
	s, err := Load(path)
	require.NoError(t, err)
	require.Len(t, s.Partitions, 1)
	require.Len(t, s.Shaping, 1)
}
