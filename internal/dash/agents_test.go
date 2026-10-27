package dash

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

func TestSnapshotLiveAgentsHaveAFixedHardCap(t *testing.T) {
	run := observerTestRun("run-many-live")
	run.Config.Ammo = 10_000
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(),
		Limits: Limits{Beads: 2, RecentAgents: 2, Reviews: 2, Deliveries: 2, Notices: 2, Activity: 3},
		Now:    func() time.Time { return run.StartedAt.Add(time.Hour) },
	})
	o.lock = orchestrator.LockProbe{State: orchestrator.LockLive, Record: orchestrator.LockRecord{
		LockHolder: orchestrator.LockHolder{Run: run.ID, PID: 42},
	}}
	o.liveness = orchestrator.Liveness{Held: true, Mine: true, Alive: true}
	o.apply(observerTestStarted(run))
	const liveTotal = 5_000
	for i := 0; i < liveTotal; i++ {
		o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: domain.Agent{
			ID: domain.AgentID(fmt.Sprintf("eng-%05d", i)), Role: domain.RoleEngineer,
			State: domain.AgentStateRunning, StartedAt: run.StartedAt,
		}, Task: events.TaskImplement})
	}
	const recentTotal = 5
	for i := 0; i < recentTotal; i++ {
		id := domain.AgentID(fmt.Sprintf("done-%d", i))
		o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: domain.Agent{
			ID: id, Role: domain.RoleTester, State: domain.AgentStateRunning, StartedAt: run.StartedAt,
		}, Task: events.TaskReview})
		o.apply(events.AgentFinished{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: id, Role: domain.RoleTester})
	}
	o.publish()
	s := o.Snapshot()

	require.Len(t, s.LiveAgents, maxLiveAgents)
	require.Equal(t, liveTotal, s.Counts.LiveAgents, "the total is the full fold, not the returned window")
	require.Equal(t, liveTotal+recentTotal, s.Counts.Agents)
	require.Equal(t, liveTotal-maxLiveAgents, s.LiveAgentsOmitted)
	require.Equal(t, liveTotal, len(s.LiveAgents)+s.LiveAgentsOmitted)
	require.Len(t, s.RecentAgents, 2, "the existing recent-agent window is independent")
	require.Equal(t, liveTotal-maxLiveAgents+recentTotal-2, s.Omitted.Agents)
	require.Equal(t, domain.AgentID("eng-00000"), s.LiveAgents[0].ID)
	require.Equal(t, domain.AgentID(fmt.Sprintf("eng-%05d", maxLiveAgents-1)), s.LiveAgents[maxLiveAgents-1].ID)
	for _, agent := range s.LiveAgents {
		require.True(t, agent.Live)
	}

	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	require.Less(t, len(encoded), 250_000, "thousands of live agents must not expand the HTTP response")
	var shape map[string]any
	require.NoError(t, json.Unmarshal(encoded, &shape))
	require.Equal(t, float64(liveTotal-maxLiveAgents), shape["live_agents_omitted"])
}
