package scope

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/disruptoor/internal/discovery"
)

func TestMatch(t *testing.T) {
	beacon := discovery.Container{Labels: map[string]string{
		clientTypeLabelKey: "beacon",
	}}
	exec := discovery.Container{Labels: map[string]string{
		clientTypeLabelKey: "execution",
	}}

	tcpDisc := discovery.Port{Name: "tcp-discovery", Number: 9000, Protocol: "TCP"}
	rpc := discovery.Port{Name: "rpc", Number: 8545, Protocol: "TCP"}
	metrics := discovery.Port{Name: "metrics", Number: 9001, Protocol: "TCP"}

	tests := []struct {
		name   string
		peer   discovery.Container
		port   discovery.Port
		scopes []string
		want   bool
	}{
		{"cl p2p hits beacon discovery", beacon, tcpDisc, []string{CLP2P}, true},
		{"cl p2p misses execution discovery", exec, tcpDisc, []string{CLP2P}, false},
		{"el p2p hits execution discovery", exec, tcpDisc, []string{ELP2P}, true},
		{"both p2p hits either", exec, tcpDisc, []string{CLP2P, ELP2P}, true},
		{"control off by default", beacon, rpc, []string{CLP2P, ELP2P}, false},
		{"control on when included", beacon, rpc, []string{CLP2P, ELP2P, Control}, true},
		{"metrics off by default", exec, metrics, []string{ELP2P}, false},
		{"metrics on when included", exec, metrics, []string{Control}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Match(tt.peer, tt.port, tt.scopes))
		})
	}
}

func TestFilterPorts(t *testing.T) {
	exec := discovery.Container{
		Labels: map[string]string{clientTypeLabelKey: "execution"},
		Ports: []discovery.Port{
			{Name: "tcp-discovery", Number: 30303, Protocol: "TCP"},
			{Name: "udp-discovery", Number: 30303, Protocol: "UDP"},
			{Name: "engine-rpc", Number: 8551, Protocol: "TCP"},
			{Name: "rpc", Number: 8545, Protocol: "TCP"},
			{Name: "metrics", Number: 9001, Protocol: "TCP"},
		},
	}

	got := FilterPorts(exec, []string{ELP2P})
	assert.Len(t, got, 2)
	for _, p := range got {
		assert.Contains(t, []string{"tcp-discovery", "udp-discovery"}, p.Name)
	}

	got = FilterPorts(exec, []string{ELP2P, Control})
	assert.Len(t, got, 5)
}
