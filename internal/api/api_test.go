package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/disruptoor/internal/backend"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/state"
)

func TestStateETagPreventsStalePut(t *testing.T) {
	svc := newTestService()
	staleETag := getStateETag(t, svc)

	putState(t, svc, namedPartitionState("first"), staleETag, http.StatusOK)
	putState(t, svc, namedPartitionState("second"), staleETag, http.StatusPreconditionFailed)

	current := svc.GetState()
	require.Len(t, current.Partitions, 1)
	require.Equal(t, "first", current.Partitions[0].Name)
}

func TestStatePutWithoutIfMatchStillWorks(t *testing.T) {
	svc := newTestService()

	putState(t, svc, namedPartitionState("first"), "", http.StatusOK)

	current := svc.GetState()
	require.Len(t, current.Partitions, 1)
	require.Equal(t, "first", current.Partitions[0].Name)
}

func TestStatePutRejectsUnknownFields(t *testing.T) {
	svc := newTestService()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/state", bytes.NewBufferString(`{"partitions":[],"typo":true}`))
	svc.handlePutState(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
}

func TestGetStateDeepCopiesMutableFields(t *testing.T) {
	svc := newTestService()
	require.NoError(t, svc.Apply(context.Background(), state.State{
		Partitions: []state.Partition{{
			Name:      "partition",
			Groups:    []state.Selector{{Match: map[string][]string{"id": {"alpha"}}}, {Match: map[string][]string{"id": {"bravo"}}}},
			Scope:     []string{state.ScopeCLP2P},
			Symmetric: boolPtr(true),
		}},
		Shaping: []state.Shaping{{
			Name:   "shape",
			Target: &state.Selector{Match: map[string][]string{"id": {"alpha"}}},
			Scope:  []string{state.ScopeControl},
			Delay:  "50ms",
		}},
	}))

	got := svc.GetState()
	got.Partitions[0].Groups[0].Match["id"][0] = "mutated"
	got.Partitions[0].Scope[0] = state.ScopeControl
	*got.Partitions[0].Symmetric = false
	got.Shaping[0].Target.Match["id"][0] = "mutated"

	current := svc.GetState()
	require.Equal(t, "alpha", current.Partitions[0].Groups[0].Match["id"][0])
	require.Equal(t, state.ScopeCLP2P, current.Partitions[0].Scope[0])
	require.True(t, current.Partitions[0].IsSymmetric())
	require.Equal(t, "alpha", current.Shaping[0].Target.Match["id"][0])
}

func TestApplyRejectsEmptyPartitionGroups(t *testing.T) {
	svc := newTestServiceWithDiscovery(emptyGroupDiscovery{})

	err := svc.Apply(context.Background(), namedPartitionState("empty-group"))

	require.ErrorContains(t, err, "selector matched no containers")
}

func TestStatePutApplyErrorUsesStableResponse(t *testing.T) {
	svc := newTestServiceWithDiscovery(emptyGroupDiscovery{})
	body, err := json.Marshal(namedPartitionState("empty-group"))
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/state", bytes.NewReader(body))
	svc.handlePutState(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	respBody, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)
	require.JSONEq(t, `{"error":"apply failed; rolled back to empty state"}`, string(respBody))
	require.NotContains(t, string(respBody), "selector matched no containers")
}

func TestApplyIsolationResolvesComplement(t *testing.T) {
	ipt := &recordingIptables{}
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo", "charlie"})
	svc.cfg.Iptables = ipt

	require.NoError(t, svc.Apply(context.Background(), state.State{
		Isolations: []state.Isolation{{
			Name:   "blackout-alpha",
			Target: &state.Selector{Match: map[string][]string{"id": {"alpha"}}},
			Scope:  []string{state.ScopeCLP2P, state.ScopeELP2P, state.ScopeControl},
		}},
	}))

	require.Len(t, ipt.partitions, 1)
	part := ipt.partitions[0]
	require.Equal(t, "blackout-alpha", part.Name)
	require.Len(t, part.Groups, 2)
	require.Equal(t, []string{"alpha"}, containerNames(part.Groups[0]))
	require.Equal(t, []string{"bravo", "charlie"}, containerNames(part.Groups[1]))
	require.Equal(t, []string{state.ScopeCLP2P, state.ScopeELP2P, state.ScopeControl}, part.Scope)
	require.True(t, part.Symmetric)
}

// A target matching multiple containers is isolated as a group: its members
// end up in the same partition group, so traffic among them is unaffected.
// This is load-bearing API semantics — callers wanting per-container
// blackouts declare one isolation each.
func TestApplyIsolationKeepsMultiMatchTargetAsOneGroup(t *testing.T) {
	ipt := &recordingIptables{}
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo", "charlie", "delta"})
	svc.cfg.Iptables = ipt

	require.NoError(t, svc.Apply(context.Background(), state.State{
		Isolations: []state.Isolation{{
			Name:   "island",
			Target: &state.Selector{Match: map[string][]string{"id": {"alpha", "bravo"}}},
		}},
	}))

	require.Len(t, ipt.partitions, 1)
	part := ipt.partitions[0]
	require.Len(t, part.Groups, 2)
	require.Equal(t, []string{"alpha", "bravo"}, containerNames(part.Groups[0]),
		"matched containers must share one group so intra-target traffic keeps flowing")
	require.Equal(t, []string{"charlie", "delta"}, containerNames(part.Groups[1]))
}

func TestApplyAppendsIsolationsAfterPartitions(t *testing.T) {
	ipt := &recordingIptables{}
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo", "charlie"})
	svc.cfg.Iptables = ipt

	require.NoError(t, svc.Apply(context.Background(), state.State{
		Partitions: []state.Partition{{
			Name: "split",
			Groups: []state.Selector{
				{Match: map[string][]string{"id": {"alpha"}}},
				{Match: map[string][]string{"id": {"bravo"}}},
			},
		}},
		Isolations: []state.Isolation{{
			Name:   "blackout-charlie",
			Target: &state.Selector{Match: map[string][]string{"id": {"charlie"}}},
		}},
	}))

	require.Len(t, ipt.partitions, 2)
	require.Equal(t, "split", ipt.partitions[0].Name)
	require.Equal(t, "blackout-charlie", ipt.partitions[1].Name)
	// Isolation with no scope inherits the partition default.
	require.Equal(t, []string{state.ScopeCLP2P, state.ScopeELP2P}, ipt.partitions[1].Scope)
}

func TestApplyIsolationTargetMatchingNothingFails(t *testing.T) {
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo"})

	err := svc.Apply(context.Background(), state.State{
		Isolations: []state.Isolation{{
			Name:   "ghost",
			Target: &state.Selector{Match: map[string][]string{"id": {"missing"}}},
		}},
	})

	require.ErrorContains(t, err, "target matched no containers")
}

func TestApplyIsolationTargetMatchingEverythingFails(t *testing.T) {
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo"})

	err := svc.Apply(context.Background(), state.State{
		Isolations: []state.Isolation{{
			Name:   "everyone",
			Target: &state.Selector{Match: map[string][]string{"id": {"alpha", "bravo"}}},
		}},
	})

	require.ErrorContains(t, err, "nothing to isolate from")
}

func TestGetStateDeepCopiesIsolations(t *testing.T) {
	svc := newTestServiceWithDiscovery(inventoryDiscovery{"alpha", "bravo"})
	require.NoError(t, svc.Apply(context.Background(), state.State{
		Isolations: []state.Isolation{{
			Name:   "blackout",
			Target: &state.Selector{Match: map[string][]string{"id": {"alpha"}}},
			Scope:  []string{state.ScopeCLP2P},
		}},
	}))

	got := svc.GetState()
	got.Isolations[0].Target.Match["id"][0] = "mutated"
	got.Isolations[0].Scope[0] = state.ScopeControl

	current := svc.GetState()
	require.Equal(t, "alpha", current.Isolations[0].Target.Match["id"][0])
	require.Equal(t, state.ScopeCLP2P, current.Isolations[0].Scope[0])
}

func TestApplyClearsPreviousStateBeforeConntrackFlush(t *testing.T) {
	ops := &opLog{}
	svc := newTestServiceWithOps(ops)

	require.NoError(t, svc.Apply(context.Background(), namedPartitionState("ordered")))

	require.Equal(t, []string{
		"tc.clear",
		"iptables.clear",
		"conntrack.flush",
		"iptables.apply",
		"tc.apply",
	}, ops.ops)
}

func getStateETag(t *testing.T, svc *service) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	svc.handleGetState(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	etag := res.Header.Get("ETag")
	require.NotEmpty(t, etag)
	return etag
}

func putState(t *testing.T, svc *service, desired state.State, etag string, wantStatus int) {
	t.Helper()
	body, err := json.Marshal(desired)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/state", bytes.NewReader(body))
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	svc.handlePutState(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	respBody, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equalf(t, wantStatus, res.StatusCode, "body=%s", string(respBody))
}

func namedPartitionState(name string) state.State {
	return state.State{
		Partitions: []state.Partition{{
			Name: name,
			Groups: []state.Selector{
				{Match: map[string][]string{"id": {"alpha"}}},
				{Match: map[string][]string{"id": {"bravo"}}},
			},
		}},
	}
}

func newTestService() *service {
	return newTestServiceWithOps(nil)
}

func newTestServiceWithDiscovery(d discovery.Service) *service {
	return &service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: Config{
			Discovery: d,
			Iptables:  fakeIptables{},
			TC:        fakeTC{},
			Conntrack: fakeConntrack{},
		},
	}
}

func newTestServiceWithOps(ops *opLog) *service {
	return &service{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: Config{
			Discovery: fakeDiscovery{},
			Iptables:  fakeIptables{ops: ops},
			TC:        fakeTC{ops: ops},
			Conntrack: fakeConntrack{ops: ops},
		},
	}
}

func boolPtr(v bool) *bool { return &v }

type opLog struct {
	ops []string
}

func (l *opLog) add(op string) {
	if l == nil {
		return
	}
	l.ops = append(l.ops, op)
}

type fakeDiscovery struct{}

func (fakeDiscovery) Start(context.Context) error { return nil }
func (fakeDiscovery) Stop() error                 { return nil }
func (fakeDiscovery) EnclaveID() string           { return "test" }
func (fakeDiscovery) Resolve(context.Context, state.Selector) ([]discovery.Container, error) {
	return []discovery.Container{{ID: "target", Name: "target"}}, nil
}
func (fakeDiscovery) ResolveGroups(_ context.Context, sels []state.Selector) ([][]discovery.Container, error) {
	out := make([][]discovery.Container, len(sels))
	for i := range sels {
		out[i] = []discovery.Container{{ID: fmt.Sprintf("group-%d", i), Name: fmt.Sprintf("group-%d", i)}}
	}
	return out, nil
}

// inventoryDiscovery resolves selectors against a fixed container inventory:
// the All selector matches everything, and Match selectors are honoured for
// the "id" key only (values name containers directly).
type inventoryDiscovery []string

func (inventoryDiscovery) Start(context.Context) error { return nil }
func (inventoryDiscovery) Stop() error                 { return nil }
func (inventoryDiscovery) EnclaveID() string           { return "test" }
func (d inventoryDiscovery) Resolve(_ context.Context, sel state.Selector) ([]discovery.Container, error) {
	out := make([]discovery.Container, 0, len(d))
	for _, name := range d {
		if sel.All || containsValue(sel.Match["id"], name) {
			out = append(out, discovery.Container{ID: name, Name: name})
		}
	}
	return out, nil
}

func (d inventoryDiscovery) ResolveGroups(ctx context.Context, sels []state.Selector) ([][]discovery.Container, error) {
	out := make([][]discovery.Container, len(sels))
	for i, sel := range sels {
		matched, err := d.Resolve(ctx, sel)
		if err != nil {
			return nil, err
		}
		out[i] = matched
	}
	return out, nil
}

func containsValue(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func containerNames(cs []discovery.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// recordingIptables captures the resolved partitions passed to Apply.
type recordingIptables struct {
	partitions []backend.ResolvedPartition
}

func (*recordingIptables) Start(context.Context) error { return nil }
func (*recordingIptables) Stop() error                 { return nil }
func (r *recordingIptables) Apply(_ context.Context, ps []backend.ResolvedPartition) error {
	r.partitions = ps
	return nil
}
func (*recordingIptables) Clear(context.Context) error { return nil }

type emptyGroupDiscovery struct {
	fakeDiscovery
}

func (emptyGroupDiscovery) ResolveGroups(_ context.Context, sels []state.Selector) ([][]discovery.Container, error) {
	return make([][]discovery.Container, len(sels)), nil
}

type fakeIptables struct {
	ops *opLog
}

func (fakeIptables) Start(context.Context) error { return nil }
func (fakeIptables) Stop() error                 { return nil }
func (f fakeIptables) Apply(context.Context, []backend.ResolvedPartition) error {
	f.ops.add("iptables.apply")
	return nil
}
func (f fakeIptables) Clear(context.Context) error {
	f.ops.add("iptables.clear")
	return nil
}

type fakeTC struct {
	ops *opLog
}

func (fakeTC) Start(context.Context) error { return nil }
func (fakeTC) Stop() error                 { return nil }
func (f fakeTC) Apply(context.Context, []backend.ResolvedShaping) error {
	f.ops.add("tc.apply")
	return nil
}
func (f fakeTC) Clear(context.Context) error {
	f.ops.add("tc.clear")
	return nil
}

type fakeConntrack struct {
	ops *opLog
}

func (f fakeConntrack) Flush(context.Context, []backend.ResolvedPartition) error {
	f.ops.add("conntrack.flush")
	return nil
}
