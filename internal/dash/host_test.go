package dash

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func hostTestSnapshot() HostSnapshot {
	return HostSnapshot{
		Name: "host-a", CPU: &HostCPUSnapshot{
			UsedPct: 42.5, Cores: []HostCoreSnapshot{{ID: 0, UsedPct: 40}, {ID: 1, UsedPct: 45}},
		},
		Memory: HostUsageSnapshot{Used: 300, Total: 1000},
		Swap:   &HostUsageSnapshot{Used: 20, Total: 100},
		Disk:   HostUsageSnapshot{Used: 500, Total: 2000},
	}
}

func TestHostSnapshotJSONContractAndDeepCopy(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-host-copy"), RunDir: t.TempDir()})
	withoutOptional := hostTestSnapshot()
	withoutOptional.CPU = nil
	withoutOptional.Swap = nil
	o.applyHostEvent(hostEvent{sample: withoutOptional, success: true})
	o.publish()
	encoded, err := json.Marshal(o.Snapshot())
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"host":{"name":"host-a","collected_at":"0001-01-01T00:00:00Z","memory":{"used":300,"total":1000},"disk":{"used":500,"total":2000}}`)
	require.NotContains(t, string(encoded), `"cpu"`)
	require.NotContains(t, string(encoded), `"swap"`)

	sample := hostTestSnapshot()
	for i := 2; i < maxHostCores+44; i++ {
		sample.CPU.Cores = append(sample.CPU.Cores, HostCoreSnapshot{ID: i})
	}
	collectedAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("test", 3600))
	o.applyHostEvent(hostEvent{at: collectedAt, sample: sample, success: true})
	o.publish()
	first := o.Snapshot()
	require.NotNil(t, first.Host)
	require.Len(t, first.Host.CPU.Cores, maxHostCores)
	require.Equal(t, 44, first.Host.CPU.CoresOmitted)
	require.Equal(t, collectedAt.UTC(), first.Host.CollectedAt)
	require.Equal(t, time.UTC, first.Host.CollectedAt.Location())

	first.Host.Name = "mutated"
	first.Host.CPU.UsedPct = 100
	first.Host.CPU.Cores[0].UsedPct = 99
	first.Host.Swap.Used = 99
	first.Host.Memory.Used = 99
	second := o.Snapshot()
	require.Equal(t, "host-a", second.Host.Name)
	require.Equal(t, 42.5, second.Host.CPU.UsedPct)
	require.Equal(t, float64(40), second.Host.CPU.Cores[0].UsedPct)
	require.Equal(t, uint64(20), second.Host.Swap.Used)
	require.Equal(t, uint64(300), second.Host.Memory.Used)
}

func TestHostCollectionRetainsLastGoodAndUsesIndependentTicker(t *testing.T) {
	run := observerTestRun("run-host-last-good")
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour,
		HostRefreshInterval: 5 * time.Millisecond, FollowPoll: observerTestPoll,
	})
	require.Equal(t, 2*time.Second, DefaultHostRefreshInterval)
	var calls atomic.Int64
	o.sampleHost = func() (HostSnapshot, error) {
		if calls.Add(1) == 1 {
			return hostTestSnapshot(), nil
		}
		return HostSnapshot{}, errors.New("transient read failure")
	}
	observerTestStart(t, o)
	first := observerTestWaitFor(t, o, func(snapshot Snapshot) bool { return snapshot.Host != nil })
	require.NotNil(t, first.Host)
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, observerTestWait, observerTestPoll)
	second := o.Snapshot()
	require.Equal(t, first.Host, second.Host)
	require.Equal(t, "ok", second.Observer.Status)
}

func TestHostErrorsBeforeFirstGoodSampleLeaveHostUnsupported(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-host-error"), RunDir: t.TempDir()})
	o.applyHostEvent(hostEvent{success: false, sample: hostTestSnapshot()})
	o.publish()
	require.Nil(t, o.Snapshot().Host)
	encoded, err := json.Marshal(o.Snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"host"`)
}

func TestHostSnapshotAndHTTPPerformNoCollection(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-host-http"), RunDir: t.TempDir()})
	var calls atomic.Int64
	o.sampleHost = func() (HostSnapshot, error) {
		calls.Add(1)
		return hostTestSnapshot(), nil
	}
	o.applyHostEvent(hostEvent{sample: hostTestSnapshot(), success: true})
	o.publish()
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	for range 10 {
		request := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
		request.Host = "localhost"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.NotNil(t, o.Snapshot().Host)
	}
	require.Zero(t, calls.Load(), "HTTP and Snapshot must only read the cached value")
}
