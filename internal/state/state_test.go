package state

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectorUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantAll bool
		wantMap map[string][]string
		wantErr bool
	}{
		{
			name:    "all string",
			input:   `"all"`,
			wantAll: true,
		},
		{
			name:    "single string value",
			input:   `{"client-type":"execution"}`,
			wantMap: map[string][]string{"client-type": {"execution"}},
		},
		{
			name:    "number value",
			input:   `{"node-index":1}`,
			wantMap: map[string][]string{"node-index": {"1"}},
		},
		{
			name:    "array of numbers",
			input:   `{"node-index":[1,2,3]}`,
			wantMap: map[string][]string{"node-index": {"1", "2", "3"}},
		},
		{
			name:    "mixed array",
			input:   `{"node-index":["1",2]}`,
			wantMap: map[string][]string{"node-index": {"1", "2"}},
		},
		{
			name:    "multi-key AND",
			input:   `{"client-type":"execution","client":"geth"}`,
			wantMap: map[string][]string{"client-type": {"execution"}, "client": {"geth"}},
		},
		{name: "wrong string", input: `"everything"`, wantErr: true},
		{name: "empty object", input: `{}`, wantErr: true},
		{name: "boolean value", input: `{"x":true}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s Selector
			err := json.Unmarshal([]byte(tt.input), &s)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantAll, s.All)
			assert.Equal(t, tt.wantMap, s.Match)
		})
	}
}

func TestSelectorRoundTrip(t *testing.T) {
	original := Selector{Match: map[string][]string{"node-index": {"1", "2"}}}
	bytes, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded Selector
	require.NoError(t, json.Unmarshal(bytes, &decoded))
	assert.Equal(t, original, decoded)

	all := Selector{All: true}
	bytes, err = json.Marshal(all)
	require.NoError(t, err)
	assert.Equal(t, `"all"`, string(bytes))
}

func TestStateValidate(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		wantErr string
	}{
		{
			name: "valid partition",
			state: State{Partitions: []Partition{{
				Name: "split", Groups: []Selector{
					{Match: map[string][]string{"node-index": {"1"}}},
					{Match: map[string][]string{"node-index": {"2"}}},
				},
			}}},
		},
		{
			name: "partition with one group",
			state: State{Partitions: []Partition{{
				Name:   "bad",
				Groups: []Selector{{All: true}},
			}}},
			wantErr: "need at least 2 groups",
		},
		{
			name: "partition empty selector",
			state: State{Partitions: []Partition{{
				Name:   "bad",
				Groups: []Selector{{}, {All: true}},
			}}},
			wantErr: "empty selector",
		},
		{
			name: "asymmetric partition rejected",
			state: State{Partitions: []Partition{{
				Name:      "bad",
				Groups:    []Selector{{All: true}, {Match: map[string][]string{"id": {"alpha"}}}},
				Symmetric: boolPtr(false),
			}}},
			wantErr: "asymmetric partitions are not supported",
		},
		{
			name: "duplicate names",
			state: State{
				Partitions: []Partition{{
					Name:   "x",
					Groups: []Selector{{All: true}, {All: true}},
				}},
				Shaping: []Shaping{{
					Name:   "x",
					Target: &Selector{All: true},
					Scope:  []string{ScopeControl},
					Delay:  "100ms",
				}},
			},
			wantErr: "duplicate name",
		},
		{
			name: "valid shaping",
			state: State{Shaping: []Shaping{{
				Name:   "ok",
				Target: &Selector{All: true},
				Scope:  []string{ScopeControl},
				Delay:  "100ms",
			}}},
		},
		{
			name: "shaping with both target and between",
			state: State{Shaping: []Shaping{{
				Name:    "bad",
				Target:  &Selector{All: true},
				Between: []Selector{{All: true}, {All: true}},
				Scope:   []string{ScopeControl},
				Delay:   "1s",
			}}},
			wantErr: "between mode not supported",
		},
		{
			name: "shaping between rejected",
			state: State{Shaping: []Shaping{{
				Name:    "bad",
				Between: []Selector{{All: true}, {All: true}},
				Scope:   []string{ScopeControl},
				Delay:   "1s",
			}}},
			wantErr: "target required",
		},
		{
			name: "shaping with no parameters",
			state: State{Shaping: []Shaping{{
				Name:   "bad",
				Target: &Selector{All: true},
				Scope:  []string{ScopeControl},
			}}},
			wantErr: "at least one of delay",
		},
		{
			name: "shaping ingress rejected",
			state: State{Shaping: []Shaping{{
				Name:      "bad",
				Target:    &Selector{All: true},
				Scope:     []string{ScopeControl},
				Direction: "ingress",
				Delay:     "1s",
			}}},
			wantErr: "ingress",
		},
		{
			name: "shaping both rejected",
			state: State{Shaping: []Shaping{{
				Name:      "bad",
				Target:    &Selector{All: true},
				Scope:     []string{ScopeControl},
				Direction: "both",
				Delay:     "1s",
			}}},
			wantErr: "both",
		},
		{
			name: "shaping jitter without delay",
			state: State{Shaping: []Shaping{{
				Name:   "bad",
				Target: &Selector{All: true},
				Scope:  []string{ScopeControl},
				Loss:   "1%",
				Jitter: "10ms",
			}}},
			wantErr: "jitter requires delay",
		},
		{
			name: "shaping missing scope acknowledgement",
			state: State{Shaping: []Shaping{{
				Name:   "bad",
				Target: &Selector{All: true},
				Delay:  "100ms",
			}}},
			wantErr: "include_control",
		},
		{
			name: "shaping with p2p scope rejected",
			state: State{Shaping: []Shaping{{
				Name:   "bad",
				Target: &Selector{All: true},
				Scope:  []string{ScopeCLP2P},
				Delay:  "100ms",
			}}},
			wantErr: "include_control",
		},
		{
			name: "unknown scope",
			state: State{Partitions: []Partition{{
				Name:   "bad",
				Groups: []Selector{{All: true}, {All: true}},
				Scope:  []string{"made_up"},
			}}},
			wantErr: "unknown scope",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.state.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPartitionDefaults(t *testing.T) {
	p := Partition{Name: "x"}
	assert.True(t, p.IsSymmetric(), "symmetric default is true")
	def := []string{ScopeCLP2P, ScopeELP2P}
	assert.Equal(t, def, p.EffectiveScope(def))

	override := []string{ScopeCLP2P}
	p2 := Partition{Name: "y", Scope: override}
	assert.Equal(t, override, p2.EffectiveScope(def))
}

func boolPtr(v bool) *bool { return &v }
