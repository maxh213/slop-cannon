package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
)

// Tests for the per-agent log files (P8-07). The read side (`sc logs`,
// P8-05) and the first-run notice (P8-06) both point at
// <run-dir>/agents/<agent-id>.log; these are the tests that the run writes
// them. Every helper is prefixed "agentLog" so it cannot collide with
// another task's file in this package.

// agentLogEvents is the agent-scoped slice of a run's event stream: one
// spawn, two output lines, a failure and a finish, for two different
// agents plus one event that names no agent at all.
func agentLogEvents() []events.Event {
	return []events.Event{
		events.AgentSpawned{
			Agent: domain.Agent{ID: "eng-sc-1-r1", Role: domain.RoleEngineer, Bead: "sc-1", PID: 4242},
			Task:  events.TaskImplement, Round: 1, Attempt: 1,
		},
		events.AgentOutput{Agent: "eng-sc-1-r1", Stream: events.StreamStdout, Line: "Reading the repository."},
		events.AgentOutput{Agent: "eng-sc-1-r1", Stream: events.StreamStderr, Line: "[tool Write] docs/NOTES.md"},
		events.AgentFinished{
			Agent: "eng-sc-1-r1", Role: domain.RoleEngineer, Bead: "sc-1",
			Duration: 2 * time.Second, ExitCode: 0, Produced: domain.MessageImplReport,
		},
		events.AgentOutput{Agent: "tester-qa-sc-1-r1", Stream: events.StreamStdout, Line: "Reading the diff."},
		events.AgentFailed{
			Agent: "tester-qa-sc-1-r1", Role: domain.RoleTester, Bead: "sc-1",
			Failure: events.FailureBadOutput, Err: "the reply ended in a broken result block",
			Duration: time.Second, Attempt: 1,
		},
		// Nothing names an agent here, so nothing is written for it.
		events.RunFinished{Reason: events.FinishCompleted},
	}
}

// TestAgentLogSinkWritesOneFilePerAgent is the unit half of P8-07's log
// gap: the sink opens <run-dir>/agents/<id>.log on first use and appends
// every agent-scoped event to the right file.
func TestAgentLogSinkWritesOneFilePerAgent(t *testing.T) {
	dir := t.TempDir()
	logs, err := logging.OpenRunLog(dir, "20260904-120000-abcd", logging.Options{})
	require.NoError(t, err)
	sink := newAgentLogSink(logs, nil)
	for _, ev := range agentLogEvents() {
		sink.Observe(ev)
	}
	require.NoError(t, logs.Close())

	paths := logging.PathsFor(dir)
	eng := agentLogRead(t, paths.AgentLog("eng-sc-1-r1"))
	assert.Contains(t, eng, `"msg":"spawned"`)
	assert.Contains(t, eng, `"role":"engineer"`)
	assert.Contains(t, eng, `"pid":4242`)
	assert.Contains(t, eng, `"msg":"Reading the repository."`)
	assert.Contains(t, eng, `"stream":"stderr"`)
	assert.Contains(t, eng, `"msg":"finished"`)
	assert.Contains(t, eng, `"agent":"eng-sc-1-r1"`)
	assert.NotContains(t, eng, "Reading the diff.", "one agent's lines never reach another's log")

	qa := agentLogRead(t, paths.AgentLog("tester-qa-sc-1-r1"))
	assert.Contains(t, qa, `"msg":"Reading the diff."`)
	assert.Contains(t, qa, `"msg":"failed"`)
	assert.Contains(t, qa, `"failure":"bad_output"`)

	// The run's own events wrote no stray file.
	entries, err := os.ReadDir(paths.AgentsDir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"eng-sc-1-r1.log", "tester-qa-sc-1-r1.log"}, names)
}

// TestAgentLogSinkSurvivesAClosedLogger proves the sink cannot take a run
// down: after Close the logger refuses to open files, and Observe must
// swallow that (and a nil sink, and a nil event).
func TestAgentLogSinkSurvivesAClosedLogger(t *testing.T) {
	dir := t.TempDir()
	logs, err := logging.OpenRunLog(dir, "20260904-120000-abcd", logging.Options{})
	require.NoError(t, err)
	require.NoError(t, logs.Close())

	sink := newAgentLogSink(logs, logging.NewFileLogger(io.Discard, logging.Options{}))
	require.NotPanics(t, func() {
		for _, ev := range agentLogEvents() {
			sink.Observe(ev)
		}
		var nilSink *agentLogSink
		nilSink.Observe(events.AgentOutput{Agent: "eng-sc-1-r1", Line: "x"})
		sink.Observe(nil)
	})
	assert.Nil(t, newAgentLogSink(nil, nil), "no run logger, no sink")
}

// TestRunWritesAnAgentLogPerAgent is the end of the gap: a real headless
// run leaves a log for every agent it fired, and `sc logs <agent-id>`
// prints it. Before this, <run-dir>/agents was empty and `sc logs` for an
// agent that had run answered "no log".
func TestRunWritesAnAgentLogPerAgent(t *testing.T) {
	env := runFireSetup(t, true)

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	agents := agentLogIDs(t, logging.PathsFor(dir).AgentsDir)
	require.NotEmpty(t, agents, "the run left no agent logs")

	// Every agent the run fired has a file, and the engineer's holds the
	// lines the mock cartridge printed.
	var engineer domain.AgentID
	for _, id := range agents {
		require.Contains(t, stdout, string(id), "an agent log names an agent the run never fired")
		if strings.HasPrefix(string(id), "eng-") {
			engineer = id
		}
	}
	require.NotEmpty(t, engineer, "no engineer log among %v", agents)
	body := agentLogRead(t, logging.PathsFor(dir).AgentLog(engineer))
	assert.Contains(t, body, `"msg":"spawned"`)
	assert.Contains(t, body, `"msg":"finished"`)
	assert.Contains(t, body, `"agent":"`+string(engineer)+`"`)

	// P8-05 reads it back: this is the command the first-run notice points
	// the user at.
	out, errOut, code := runFireExec(t, "logs", string(engineer), "--repo", env.Repo.Dir)
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", out, errOut)
	assert.Contains(t, out, "spawned")
	assert.NotContains(t, errOut, "no log for")
}

// agentLogRead returns the whole of one log file.
func agentLogRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a temp run directory
	require.NoError(t, err, "no agent log at %s", path)
	return string(data)
}

// agentLogIDs lists the agents an agents directory holds logs for.
func agentLogIDs(t *testing.T, dir string) []domain.AgentID {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	ids := make([]domain.AgentID, 0, len(entries))
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())); name != e.Name() {
			ids = append(ids, domain.AgentID(name))
		}
	}
	return ids
}
