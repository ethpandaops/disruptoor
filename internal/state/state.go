// Package state defines the desired-state schema for network disruptions.
//
// State is the full picture: a set of partitions plus a set of shaping rules.
// Callers PUT a complete State; the controller diffs against currently-applied
// state and converges. Selectors target containers by label match; resolution
// to concrete container IDs happens at apply time against the live Docker
// daemon, not at parse time.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Scope values control which port classes a disruption applies to. The
// default scope (cl_p2p, el_p2p) deliberately spares RPC, engine, metrics,
// and the VC/CL beacon API so tests retain visibility into the system.
const (
	ScopeCLP2P   = "cl_p2p"
	ScopeELP2P   = "el_p2p"
	ScopeControl = "include_control" // explicit opt-in for RPC/engine/metrics
)

// DirectionEgress is the only shaping direction supported in v0. The
// schema accepts the field as optional and treats empty as egress.
// "both" and "ingress" require ifb redirects which v0 does not implement
// and will be rejected by Validate with a 400.
const (
	DirectionEgress = "egress"
)

// State is the full desired disruption state for an enclave.
type State struct {
	Partitions []Partition `json:"partitions,omitempty"`
	Shaping    []Shaping   `json:"shaping,omitempty"`
}

// Partition describes a hard split of the enclave into 2+ groups. Traffic
// crossing group boundaries is dropped at netfilter for the configured
// scopes. Symmetric (default true) means both directions are blocked.
type Partition struct {
	Name      string     `json:"name"`
	Groups    []Selector `json:"groups"`
	Scope     []string   `json:"scope,omitempty"`
	Symmetric *bool      `json:"symmetric,omitempty"`
}

// Shaping describes per-target link degradation: delay, jitter, loss,
// bandwidth. v0 only supports Target (single selector, blanket egress
// shaping); Between is parsed for forward compatibility but rejected by
// Validate. Scope must be set to ["include_control"] to acknowledge that
// v0 tc shapes all egress traffic, including RPC/engine/metrics.
type Shaping struct {
	Name      string     `json:"name"`
	Target    *Selector  `json:"target,omitempty"`
	Between   []Selector `json:"between,omitempty"`
	Scope     []string   `json:"scope,omitempty"`
	Direction string     `json:"direction,omitempty"`
	Delay     string     `json:"delay,omitempty"`
	Jitter    string     `json:"jitter,omitempty"`
	Loss      string     `json:"loss,omitempty"`
	Bandwidth string     `json:"bandwidth,omitempty"`
}

// Selector picks a set of containers by label match. The zero value matches
// nothing. All=true is the universal selector ("everything in this enclave").
// Match holds label-key → allowed-values (multiple values OR together,
// multiple keys AND together).
type Selector struct {
	All   bool                `json:"-"`
	Match map[string][]string `json:"-"`
}

// IsSymmetric reports whether the partition blocks traffic in both
// directions (default) or only one way.
func (p Partition) IsSymmetric() bool {
	if p.Symmetric == nil {
		return true
	}
	return *p.Symmetric
}

// EffectiveScope returns the scope list for this partition, applying the
// default if unset.
func (p Partition) EffectiveScope(defaultScope []string) []string {
	if len(p.Scope) > 0 {
		return p.Scope
	}
	return defaultScope
}

// Validate runs structural checks on a State. Returns the first error found
// or nil. Does not check against live Docker state — that happens at apply.
func (s State) Validate() error {
	names := make(map[string]struct{}, len(s.Partitions)+len(s.Shaping))
	for i, p := range s.Partitions {
		if err := p.validate(); err != nil {
			return fmt.Errorf("partitions[%d] (%q): %w", i, p.Name, err)
		}
		if _, dup := names[p.Name]; dup {
			return fmt.Errorf("partitions[%d]: duplicate name %q", i, p.Name)
		}
		names[p.Name] = struct{}{}
	}
	for i, sh := range s.Shaping {
		if err := sh.validate(); err != nil {
			return fmt.Errorf("shaping[%d] (%q): %w", i, sh.Name, err)
		}
		if _, dup := names[sh.Name]; dup {
			return fmt.Errorf("shaping[%d]: duplicate name %q", i, sh.Name)
		}
		names[sh.Name] = struct{}{}
	}
	return nil
}

// UnmarshalJSON accepts either the string "all" or a JSON object whose keys
// are label keys and whose values are strings, numbers, or arrays of either.
// All values are normalised to []string for downstream comparison.
func (s *Selector) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		if asString != "all" {
			return fmt.Errorf("selector string must be %q, got %q", "all", asString)
		}
		s.All = true
		return nil
	}

	var asObject map[string]json.RawMessage
	if err := json.Unmarshal(data, &asObject); err != nil {
		return fmt.Errorf("selector must be %q or an object: %w", "all", err)
	}
	if len(asObject) == 0 {
		return errors.New("selector object must contain at least one label key")
	}

	match := make(map[string][]string, len(asObject))
	for key, raw := range asObject {
		values, err := decodeSelectorValues(raw)
		if err != nil {
			return fmt.Errorf("selector key %q: %w", key, err)
		}
		match[key] = values
	}
	s.Match = match
	return nil
}

// MarshalJSON emits the canonical wire form: either "all" or an object whose
// values are arrays of strings (always arrays, even for single values, so
// round-trips are predictable).
func (s Selector) MarshalJSON() ([]byte, error) {
	if s.All {
		return json.Marshal("all")
	}
	if len(s.Match) == 0 {
		return nil, errors.New("selector is empty: set All or Match")
	}
	return json.Marshal(s.Match)
}

func (p Partition) validate() error {
	if p.Name == "" {
		return errors.New("name required")
	}
	if len(p.Groups) < 2 {
		return fmt.Errorf("need at least 2 groups, got %d", len(p.Groups))
	}
	for i, g := range p.Groups {
		if !g.All && len(g.Match) == 0 {
			return fmt.Errorf("groups[%d]: empty selector", i)
		}
	}
	for _, sc := range p.Scope {
		if sc != ScopeCLP2P && sc != ScopeELP2P && sc != ScopeControl {
			return fmt.Errorf("unknown scope %q", sc)
		}
	}
	return nil
}

func (sh Shaping) validate() error {
	if sh.Name == "" {
		return errors.New("name required")
	}
	if sh.Target == nil {
		return errors.New("target required (between mode not supported in v0)")
	}
	if len(sh.Between) > 0 {
		return errors.New("between mode not supported in v0; remove between and use target")
	}
	if sh.Direction != "" && sh.Direction != DirectionEgress {
		return fmt.Errorf("direction %q not supported in v0; only %q is implemented",
			sh.Direction, DirectionEgress)
	}
	if sh.Delay == "" && sh.Loss == "" && sh.Bandwidth == "" {
		return errors.New("at least one of delay, loss, bandwidth required")
	}
	if sh.Jitter != "" && sh.Delay == "" {
		// netem silently drops jitter when no delay is set; reject to
		// avoid the user thinking jitter is being applied.
		return errors.New("jitter requires delay")
	}
	if len(sh.Scope) != 1 || sh.Scope[0] != ScopeControl {
		return fmt.Errorf("v0 tc shaping affects all egress traffic; set scope=[%q] to acknowledge",
			ScopeControl)
	}
	return nil
}

func decodeSelectorValues(raw json.RawMessage) ([]string, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []string{asString}, nil
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil && len(asNumber) > 0 {
		return []string{asNumber.String()}, nil
	}
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		out := make([]string, 0, len(asArray))
		for i, item := range asArray {
			vals, err := decodeSelectorValues(item)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, vals...)
		}
		return out, nil
	}
	return nil, errors.New("value must be string, number, or array")
}
