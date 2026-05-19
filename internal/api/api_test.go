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
