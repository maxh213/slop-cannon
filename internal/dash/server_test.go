package dash

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

func serverTestRequest(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = "localhost"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestHandlerRoutingBasePathAndMethods(t *testing.T) {
	run := observerTestRun("run-routes")
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	o.apply(observerTestStarted(run))
	o.publish()
	handler, err := NewHandler(o, "/sc/")
	require.NoError(t, err)

	for _, target := range []string{"/sc", "/sc/", "/sc/api/snapshot", "/sc/health"} {
		w := serverTestRequest(t, handler, http.MethodGet, target)
		require.Equal(t, http.StatusOK, w.Code, target)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"), target)
		require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), target)
	}
	page := serverTestRequest(t, handler, http.MethodGet, "/sc")
	require.Contains(t, page.Header().Get("Content-Type"), "text/html")
	require.Contains(t, page.Header().Get("Content-Security-Policy"), "default-src 'none'")
	require.Contains(t, page.Body.String(), `data-base-path="/sc"`)
	require.Contains(t, page.Body.String(), "SLOP//CANNON")
	require.NotContains(t, page.Body.String(), "<script src=")

	var snap Snapshot
	require.NoError(t, json.Unmarshal(serverTestRequest(t, handler, http.MethodGet, "/sc/api/snapshot").Body.Bytes(), &snap))
	require.Equal(t, run.ID, snap.Run.ID)
	var health Health
	require.NoError(t, json.Unmarshal(serverTestRequest(t, handler, http.MethodGet, "/sc/health").Body.Bytes(), &health))
	require.Equal(t, "ok", health.Status)
	require.Equal(t, snap.Revision, health.Revision)

	for _, target := range []string{"/", "/api/snapshot", "/sc/unknown", "/sc/api/snapshot/", "/sc/api/stop", "/sc/run/../state"} {
		w := serverTestRequest(t, handler, http.MethodGet, target)
		require.Equal(t, http.StatusNotFound, w.Code, target)
	}
	for _, target := range []string{"/sc", "/sc/api/snapshot", "/sc/health"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
			w := serverTestRequest(t, handler, method, target)
			require.Equal(t, http.StatusMethodNotAllowed, w.Code, "%s %s", method, target)
			require.Equal(t, http.MethodGet, w.Header().Get("Allow"), "%s %s", method, target)
		}
	}
	// Unknown control endpoints are absent, not method-aware run controls.
	require.Equal(t, http.StatusNotFound, serverTestRequest(t, handler, http.MethodPost, "/sc/api/stop").Code)

	// Query parameters cannot select another run or file.
	w := serverTestRequest(t, handler, http.MethodGet, "/sc/api/snapshot?run=other&path=/etc/passwd&runDir=/tmp")
	require.Equal(t, http.StatusOK, w.Code)
	var queried Snapshot
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &queried))
	require.Equal(t, run.ID, queried.Run.ID)
	require.Equal(t, snap.Revision, queried.Revision)
}

func TestHandlerRootBasePath(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-root"), RunDir: t.TempDir()})
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	for _, target := range []string{"/", "/api/snapshot", "/health"} {
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, target).Code, target)
	}
	for _, target := range []string{"//", "/health/", "/api/snapshot/other"} {
		require.Equal(t, http.StatusNotFound, serverTestRequest(t, handler, http.MethodGet, target).Code, target)
	}
}

func TestHandlerRejectsDNSRebindingAndAllowsExplicitHosts(t *testing.T) {
	run := observerTestRun("secret-run-snapshot")
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	o.apply(observerTestStarted(run))
	o.publish()
	handler, err := NewHandler(o, "/", "dash.demo.otwarte.xyz")
	require.NoError(t, err)

	for _, host := range []string{
		"localhost", "localhost:8790", "127.0.0.1", "127.0.0.1:8790", "::1", "[::1]", "[::1]:8790",
		"dash.demo.otwarte.xyz", "DASH.DEMO.OTWARTE.XYZ.:443",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		r.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		require.Equal(t, http.StatusOK, recorder.Code, host)
	}
	for _, host := range []string{
		"attacker.example", "localhost.attacker.example", "127.0.0.1.attacker.example", "[::2]:8790",
		"localhost:bad", "localhost@attacker.example", "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		r.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		require.Equal(t, http.StatusForbidden, recorder.Code, host)
		require.NotContains(t, recorder.Body.String(), string(run.ID), host)
		require.NotContains(t, recorder.Body.String(), "live_agents", host)
	}
}

func TestNormalizeAllowedHosts(t *testing.T) {
	hosts, err := NormalizeAllowedHosts([]string{"DASH.DEMO.OTWARTE.XYZ.", "dash.demo.otwarte.xyz", "[::1]", "other.example"})
	require.NoError(t, err)
	require.Equal(t, []string{"dash.demo.otwarte.xyz", "::1", "other.example"}, hosts)
	for _, host := range []string{"", "https://dash.example", "dash.example:443", "*.example", "dash_example", "dash.example/path"} {
		_, err := NormalizeAllowedHosts([]string{host})
		require.Error(t, err, host)
	}
	_, err = NewHandler(NewObserver(Options{Run: observerTestRun("run-hosts"), RunDir: t.TempDir()}), "/", "https://dash.example")
	require.Error(t, err)
}

func TestNormalizeBasePath(t *testing.T) {
	for input, want := range map[string]string{"": "/", "/": "/", "/sc": "/sc", "/sc/": "/sc", "/a//b/": "/a/b"} {
		got, err := NormalizeBasePath(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}
	for _, input := range []string{"relative", "/sc/../admin", "/sc/./admin", "/sc?run=other", "/sc#admin"} {
		_, err := NormalizeBasePath(input)
		require.Error(t, err, input)
	}
}

func TestHealthIgnoresQuietFinishedAndCrashedRunStates(t *testing.T) {
	run := observerTestRun("run-health")
	for _, tc := range []struct {
		name       string
		wantRun    string
		wantStream string
		quiet      bool
		prepare    func(*Observer)
	}{
		{name: "quiet", wantRun: "running", wantStream: "following", quiet: true, prepare: func(o *Observer) {
			o.lock = orchestrator.LockProbe{State: orchestrator.LockLive, Record: orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{Run: run.ID, PID: 42}}}
			o.stream = "following"
		}},
		{name: "finished", wantRun: "finished", wantStream: "complete", prepare: func(o *Observer) {
			o.apply(events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishCompleted})
			o.stream = "complete"
		}},
		{name: "crashed", wantRun: "crashed", wantStream: "closed", prepare: func(o *Observer) {
			o.stream = "closed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewObserver(Options{Run: run, RunDir: t.TempDir(), QuietAfter: time.Minute,
				Now: func() time.Time { return run.StartedAt.Add(10 * time.Minute) }})
			o.apply(observerTestStarted(run))
			tc.prepare(o)
			o.publish()
			handler, err := NewHandler(o, "/")
			require.NoError(t, err)
			healthBody := serverTestRequest(t, handler, http.MethodGet, "/health").Body.Bytes()
			var health Health
			require.NoError(t, json.Unmarshal(healthBody, &health))
			require.Equal(t, "ok", health.Status)
			require.Equal(t, tc.wantStream, health.Stream)
			require.NotContains(t, string(healthBody), "quiet")
			require.NotContains(t, string(healthBody), "crash")

			var snap Snapshot
			require.NoError(t, json.Unmarshal(serverTestRequest(t, handler, http.MethodGet, "/api/snapshot").Body.Bytes(), &snap))
			require.Equal(t, tc.wantRun, snap.Run.Status)
			require.Equal(t, tc.quiet, snap.Run.Quiet)
			require.Equal(t, "ok", snap.Observer.Status)
		})
	}
}

func TestHTTPRequestsUseCacheAndNeverReplayFollower(t *testing.T) {
	run := observerTestRun("run-cache")
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour,
		LaunchLogInterval: time.Hour, HostRefreshInterval: time.Hour,
	})
	var follows, loads atomic.Int32
	eventsCh := make(chan events.Event, 1)
	o.follow = func(context.Context, string, orchestrator.FollowOptions) <-chan events.Event {
		follows.Add(1)
		return eventsCh
	}
	o.loadState = func(string) (orchestrator.State, error) {
		loads.Add(1)
		return orchestrator.State{}, orchestrator.ErrNoState
	}
	o.apply(observerTestStarted(run))
	o.publish()
	observerTestStart(t, o)
	first := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Events == 1 && s.Revision >= 5 })
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	loadsBefore := loads.Load()
	for i := 0; i < 25; i++ {
		w := serverTestRequest(t, handler, http.MethodGet, "/api/snapshot")
		require.Equal(t, http.StatusOK, w.Code)
		var snap Snapshot
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &snap))
		require.Equal(t, first.Revision, snap.Revision)
	}
	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/health").Code)
	}
	require.Equal(t, int32(1), follows.Load())
	require.Equal(t, loadsBefore, loads.Load(), "HTTP requests must not re-read durable state")
	o.Run(context.Background())
	require.Equal(t, int32(1), follows.Load(), "an observer can start only one follower")
}

func TestEmbeddedUIUsesDOMAndTwoSecondPolling(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-ui"), RunDir: t.TempDir()})
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	page := serverTestRequest(t, handler, http.MethodGet, "/").Body.String()
	for _, want := range []string{
		"Bead flow", "Agents", "Review circuit", "Delivery", "Warnings &amp; notices", "Lifecycle signal",
		"textContent", "replaceChildren", "POLL_INTERVAL_MS = 2000", "AbortController", "Promise.race",
		"id=\"run-status\"", "aria-live=\"polite\"", "previous attempt failed", "lenses_omitted", "verdicts_omitted",
		"notice.category", "noopener noreferrer",
	} {
		require.Contains(t, page, want)
	}
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "document.write", "<script src=", "<link rel="} {
		require.NotContains(t, page, forbidden)
	}
}

type serverTestAddressWriter struct {
	address chan string
}

func (w serverTestAddressWriter) Write(p []byte) (int, error) {
	w.address <- string(p)
	return len(p), nil
}

func TestListenAndServeServesCompletedRunOverLoopback(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-real-server")
	finished := events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishCompleted}
	observerTestAppend(t, dir, observerTestStarted(run), finished)
	writer := serverTestAddressWriter{address: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- ListenAndServe(ctx, Options{
			Run: run, RunDir: dir, Host: "127.0.0.1", Port: 0, BasePath: "/sc", Out: writer,
			RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll,
		})
	}()
	var address string
	select {
	case address = <-writer.address:
	case <-time.After(observerTestWait):
		t.Fatal("server did not print its address")
	}
	address = strings.TrimSpace(address)
	address = address[strings.LastIndex(address, " ")+1:]
	require.Contains(t, address, "http://127.0.0.1:")
	require.True(t, strings.HasSuffix(address, "/sc"), address)
	client := &http.Client{Timeout: 2 * time.Second}
	var snap Snapshot
	require.Eventually(t, func() bool {
		response, err := client.Get(address + "/api/snapshot")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(response.Body).Decode(&snap) == nil && snap.Run.Status == "finished"
	}, observerTestWait, observerTestPoll)
	require.Equal(t, run.ID, snap.Run.ID)
	require.Equal(t, "ok", snap.Observer.Status)

	response, err := client.Get(address + "/health")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	response, err = client.Post(address+"/api/snapshot", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusMethodNotAllowed, response.StatusCode)
	response, err = client.Get(address + "/api/control")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNotFound, response.StatusCode)

	cancel()
	select {
	case err := <-served:
		require.NoError(t, err)
	case <-time.After(observerTestWait):
		t.Fatal("server did not shut down with its context")
	}
}

func TestListenAndServeRejectsInvalidOptions(t *testing.T) {
	run := observerTestRun("run-invalid-server")
	for _, opts := range []Options{
		{Run: run},
		{Run: run, RunDir: t.TempDir(), Port: -1},
		{Run: run, RunDir: t.TempDir(), Port: 65536},
		{Run: run, RunDir: t.TempDir(), BasePath: "relative"},
	} {
		err := ListenAndServe(context.Background(), opts)
		require.Error(t, err)
	}
}
