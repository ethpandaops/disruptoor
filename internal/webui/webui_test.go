package webui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/disruptoor/internal/api"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/state"
)

// TestRoutesRenderWithoutError walks every UI page + JSON endpoint with a
// faked StateProvider / DiscoveryProvider / EventProvider. It does not
// assert on page contents — its only job is to catch template-time errors
// (bad funcs, wrong field access, scope bugs) before runtime.
func TestRoutesRenderWithoutError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	stateImpl := &fakeState{}
	discImpl := &fakeDisc{enclaveID: "test-enclave"}
	eventsImpl := &fakeEvents{}

	svc, err := NewService(logger, Config{
		SiteName:     "disruptoor",
		Version:      "test",
		AssetVersion: "0",
		Debug:        false,
		State:        stateImpl,
		Discovery:    discImpl,
		Events:       eventsImpl,
	})
	require.NoError(t, err)

	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Cases: each page renders 200 with some sentinel HTML.
	cases := []struct {
		path     string
		sentinel string // substring expected in the response body
	}{
		{"/", "Dashboard"},
		{"/partitions", "Partitions"},
		{"/shaping", "Shaping"},
		{"/containers", "Containers"},
		{"/events", "Events"},
		{"/state", "Apply"},
		{"/css/layout.css", "header-bar"},
		{"/js/disruptoor.js", "disruptoor.js"},
		{"/webui/api/events", "applied"}, // fake ring returns one applied event
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			res, err := http.Get(srv.URL + c.path)
			require.NoError(t, err)
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.Equalf(t, http.StatusOK, res.StatusCode,
				"path=%s body=%s", c.path, string(body))
			require.Containsf(t, string(body), c.sentinel,
				"sentinel %q not found in %s", c.sentinel, c.path)
		})
	}
}

// TestRoutesRenderWithEmptyState exercises the same routes but with no
// partitions, no shaping, and no events recorded. This is the path users
// hit on first boot; it's the easiest to break with conditional logic.
func TestRoutesRenderWithEmptyState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewService(logger, Config{
		SiteName:  "disruptoor",
		State:     &fakeState{empty: true},
		Discovery: &fakeDisc{},
		Events:    &fakeEvents{empty: true},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/", "/partitions", "/shaping", "/containers", "/events", "/state"} {
		res, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		_ = res.Body.Close()
		require.Equalf(t, http.StatusOK, res.StatusCode, "path=%s", path)
	}
}

func TestAPIResolveRequiresPOST(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewService(logger, Config{
		SiteName:     "disruptoor",
		AssetVersion: "0",
		State:        &fakeState{empty: true},
		Discovery:    &fakeDisc{},
		Events:       &fakeEvents{empty: true},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	getRes, err := http.Get(srv.URL + "/webui/api/resolve")
	require.NoError(t, err)
	_ = getRes.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, getRes.StatusCode)

	postRes, err := http.Post(srv.URL+"/webui/api/resolve", "application/json", bytes.NewBufferString(`"all"`))
	require.NoError(t, err)
	defer postRes.Body.Close()
	body, err := io.ReadAll(postRes.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, postRes.StatusCode, "body=%s", string(body))
	require.JSONEq(t, `{"matched":[],"count":0}`, string(body))
}

type fakeState struct{ empty bool }

func (f *fakeState) GetState() state.State {
	if f.empty {
		return state.State{}
	}
	return state.State{
		Partitions: []state.Partition{{
			Name: "alpha-vs-bravo",
			Groups: []state.Selector{
				{Match: map[string][]string{"id": {"alpha"}}},
				{Match: map[string][]string{"id": {"bravo"}}},
			},
		}},
		Shaping: []state.Shaping{{
			Name:   "jitter-all",
			Target: &state.Selector{All: true},
			Scope:  []string{state.ScopeControl},
			Delay:  "50ms",
			Jitter: "10ms",
		}},
	}
}

type fakeDisc struct{ enclaveID string }

func (f *fakeDisc) EnclaveID() string { return f.enclaveID }
func (f *fakeDisc) Resolve(_ context.Context, _ state.Selector) ([]discovery.Container, error) {
	return nil, nil
}
func (f *fakeDisc) ResolveGroups(_ context.Context, _ []state.Selector) ([][]discovery.Container, error) {
	return nil, nil
}

type fakeEvents struct{ empty bool }

func (f *fakeEvents) Snapshot() []api.Event {
	if f.empty {
		return nil
	}
	return []api.Event{{
		Kind:   api.EventApplied,
		Source: "test",
	}}
}

// testWriter routes slog output into t.Log so test failures include the
// rendered error from the engine.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
