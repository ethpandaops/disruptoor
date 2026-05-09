// Package scope decides which of a peer container's ports a given disruption
// scope should hit. Scopes are user-facing strings declared in the API
// (cl_p2p, el_p2p, include_control); this package translates them into
// "port matches" against the peer's port catalog and client type.
package scope

import "github.com/ethpandaops/disruptoor/internal/discovery"

// Known scope names. cl_p2p and el_p2p target gossip/discovery on
// beacon and execution clients respectively. include_control opts the
// caller into RPC, engine, metrics, and HTTP API ports as well.
const (
	CLP2P              = "cl_p2p"
	ELP2P              = "el_p2p"
	Control            = "include_control"
	clientTypeLabelKey = "com.kurtosistech.custom.ethereum-package.client-type"
)

// p2pPortNames are Kurtosis port-catalog names that carry libp2p / devp2p
// gossip and discovery traffic. Names match what ethereum-package emits.
var p2pPortNames = map[string]struct{}{
	"tcp-discovery":  {},
	"udp-discovery":  {},
	"quic-discovery": {},
	"libp2p":         {},
	"devp2p":         {},
}

// controlPortNames are the ports tests typically need to keep working:
// JSON-RPC, the EL engine API, websockets, the CL beacon HTTP API, and
// metrics scrapes. They are off-limits unless include_control is in scope.
var controlPortNames = map[string]struct{}{
	"rpc":        {},
	"engine-rpc": {},
	"ws":         {},
	"http":       {},
	"metrics":    {},
}

// Match reports whether port p on peer c is in any of scopes. peer's
// client-type (execution|beacon|validator) is read from its labels.
func Match(peer discovery.Container, p discovery.Port, scopes []string) bool {
	clientType := peer.Labels[clientTypeLabelKey]
	for _, s := range scopes {
		switch s {
		case CLP2P:
			if clientType == "beacon" && isP2P(p.Name) {
				return true
			}
		case ELP2P:
			if clientType == "execution" && isP2P(p.Name) {
				return true
			}
		case Control:
			if isControl(p.Name) {
				return true
			}
		}
	}
	return false
}

// FilterPorts returns the subset of peer.Ports that match any of scopes.
func FilterPorts(peer discovery.Container, scopes []string) []discovery.Port {
	out := make([]discovery.Port, 0, len(peer.Ports))
	for _, p := range peer.Ports {
		if Match(peer, p, scopes) {
			out = append(out, p)
		}
	}
	return out
}

func isP2P(name string) bool {
	_, ok := p2pPortNames[name]
	return ok
}

func isControl(name string) bool {
	_, ok := controlPortNames[name]
	return ok
}
