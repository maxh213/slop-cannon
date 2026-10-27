package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

const launchLogTestRun = domain.RunID("run-launch")

var launchLogTestStart = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func launchLogTestRunStarted(t *testing.T, run domain.RunID, resumed bool, at time.Time) []byte {
	t.Helper()
	line, err := json.Marshal(struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Message string `json:"msg"`
		Run     string `json:"run"`
		Dir     string `json:"dir"`
		Mode    string `json:"mode"`
		Resumed bool   `json:"resumed"`
		Ammo    int    `json:"ammo"`
		Safety  bool   `json:"safety"`
	}{
		Time: at.Format(time.RFC3339Nano), Level: "INFO", Message: launchLogRunStarted,
		Run: string(run), Dir: "/tmp/SANITIZED-RUN-DIR", Mode: "autopilot", Resumed: resumed, Ammo: 4, Safety: true,
	})
	require.NoError(t, err)
	return line
}

func launchLogTestFired(t *testing.T, run domain.RunID, agent string, attempt int, cartridge string, at time.Time) []byte {
	t.Helper()
	line, err := json.Marshal(struct {
		Time      string `json:"time"`
		Level     string `json:"level"`
		Message   string `json:"msg"`
		Run       string `json:"run"`
		Agent     string `json:"agent"`
		Cartridge string `json:"cartridge"`
		Task      string `json:"task"`
		Attempt   int    `json:"attempt"`
		Workdir   string `json:"workdir"`
	}{
		Time: at.Format(time.RFC3339Nano), Level: "INFO", Message: launchLogAgentFired,
		Run: string(run), Agent: agent, Cartridge: cartridge, Task: "implement", Attempt: attempt,
		Workdir: "/tmp/SANITIZED-WORKDIR-CANARY",
	})
	require.NoError(t, err)
	return line
}

func launchLogTestRoute(t *testing.T, run domain.RunID, agent, account, model, effort string, cached bool, at time.Time) []byte {
	t.Helper()
	line, err := json.Marshal(struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Message string `json:"msg"`
		Run     string `json:"run"`
		Agent   string `json:"agent"`
		Account string `json:"account"`
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Mode    string `json:"mode"`
		Policy  string `json:"policy"`
		Cached  bool   `json:"cached"`
		Notes   string `json:"notes"`
	}{
		Time: at.Format(time.RFC3339Nano), Level: "INFO", Message: launchLogDandelionRoute,
		Run: string(run), Agent: agent, Account: account, Model: model, Effort: effort,
		Mode: "high", Policy: "read_write", Cached: cached, Notes: "SANITIZED-UNKNOWN-FIELD-CANARY",
	})
	require.NoError(t, err)
	return line
}

func launchLogTestAppend(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	for _, line := range lines {
		observerTestAppendRaw(t, path, append(append([]byte(nil), line...), '\n'))
	}
}

func launchLogTestParseFired(t *testing.T, agent string, attempt int) launchLogEvent {
	t.Helper()
	return parseLaunchLogLine(launchLogTestFired(t, launchLogTestRun, agent, attempt, "dandelion", launchLogTestStart), launchLogTestRun)
}

func launchLogTestParseRunStarted(t *testing.T, resumed bool) launchLogEvent {
	t.Helper()
	return launchLogTestParseRunStartedAt(t, resumed, launchLogTestStart)
}

func launchLogTestParseRunStartedAt(t *testing.T, resumed bool, at time.Time) launchLogEvent {
	t.Helper()
	return parseLaunchLogLine(launchLogTestRunStarted(t, launchLogTestRun, resumed, at), launchLogTestRun)
}

func launchLogTestParseRoute(t *testing.T, agent, model string, ordinal int) launchLogEvent {
	t.Helper()
	return parseLaunchLogLine(launchLogTestRoute(t, launchLogTestRun, agent, "claude", model, "max", ordinal%2 == 0,
		launchLogTestStart.Add(time.Duration(ordinal)*time.Second)), launchLogTestRun)
}

func TestLaunchLogParserAcceptsOnlyDirectSelectedRunRecords(t *testing.T) {
	fired := parseLaunchLogLine(launchLogTestFired(t, launchLogTestRun, "eng-1", 3, "dandelion", launchLogTestStart), launchLogTestRun)
	require.Equal(t, launchLogEventFired, fired.kind)
	require.Equal(t, "eng-1", fired.agent)
	require.Equal(t, 3, fired.attempt)
	require.Equal(t, "dandelion", fired.cartridge)

	route := parseLaunchLogLine(launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude-work", "claude-fable-5-1", "max", true,
		launchLogTestStart.Add(time.Second)), launchLogTestRun)
	require.Equal(t, launchLogEventRouted, route.kind)
	require.Equal(t, "eng-1", route.agent)
	require.Equal(t, "claude-work", route.route.account)
	require.Equal(t, "claude-fable-5-1", route.route.model)
	require.Equal(t, "max", route.route.effort)
	require.Equal(t, launchLogTestStart.Add(time.Second), route.route.at)

	for _, line := range [][]byte{
		launchLogTestFired(t, "another-run", "eng-1", 3, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, "another-run", "eng-1", "claude", "wrong-run-model", "max", false, launchLogTestStart),
		[]byte(`{"time":"2026-10-24T12:00:00Z","level":"INFO","msg":"dandelion quota snapshot","run":"run-launch","route":"claude/global","model":"global-model"}`),
		[]byte(`{"time":"2026-10-24T12:00:00Z","level":"INFO","msg":"agent output","run":"run-launch","agent":"eng-1","line":"{\"msg\":\"dandelion routed a firing\",\"account\":\"nested\",\"model\":\"nested-model\"}"}`),
		[]byte(`{"time":"2026-10-24T12:00:00Z","level":"INFO","msg":"agent output","run":"run-launch","agent":"eng-1","output":{"msg":"agent fired","attempt":99,"cartridge":"dandelion"}}`),
	} {
		require.Equal(t, launchLogEventNone, parseLaunchLogLine(line, launchLogTestRun).kind, string(line))
	}
}

func TestLaunchLogParserRejectsMalformedRecordsWithoutRawProse(t *testing.T) {
	validFired := launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart)
	validRoute := launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "claude-opus-5-5", "max", false, launchLogTestStart)
	tests := []struct {
		name   string
		line   []byte
		record launchLogEventKind
		agent  string
	}{
		{name: "broken JSON", line: []byte(`{"msg":"agent fired","run":"run-launch","agent":"eng-1"`), record: launchLogEventNone},
		{name: "trailing JSON", line: append(append([]byte(nil), validFired...), []byte(` {}`)...), record: launchLogEventNone},
		{name: "duplicate key", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","msg":"agent fired","run":"run-launch","agent":"eng-1","cartridge":"dandelion","attempt":1}`), record: launchLogEventNone},
		{name: "missing run", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","agent":"eng-1","cartridge":"dandelion","attempt":1}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "wrong run type", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":7,"agent":"eng-1","cartridge":"dandelion","attempt":1}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "missing agent", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":"run-launch","cartridge":"dandelion","attempt":1}`), record: launchLogEventFired},
		{name: "missing fired time", line: []byte(`{"msg":"agent fired","run":"run-launch","agent":"eng-1","cartridge":"dandelion","attempt":1}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "missing cartridge", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":"run-launch","agent":"eng-1","attempt":1}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "zero attempt", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":"run-launch","agent":"eng-1","cartridge":"dandelion","attempt":0}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "fractional attempt", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":"run-launch","agent":"eng-1","cartridge":"dandelion","attempt":1.5}`), record: launchLogEventFired, agent: "eng-1"},
		{name: "missing account", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","model":"model","effort":"max"}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "empty model", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"","effort":"max"}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "missing effort", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model"}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "null effort", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model","effort":null}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "bad route time", line: []byte(`{"time":"yesterday","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model","effort":"max"}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "bad cached type", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model","effort":"max","cached":"yes"}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "null cached", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model","effort":"max","cached":null}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "bad nested policy", line: []byte(`{"time":"2026-10-24T12:00:00Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"model","effort":"max","policy":{"secret":"RAW-PROSE-CANARY"}}`), record: launchLogEventRouted, agent: "eng-1"},
		{name: "oversize agent", line: []byte(fmt.Sprintf(`{"time":"2026-10-24T12:00:00Z","msg":"agent fired","run":"run-launch","agent":%q,"cartridge":"dandelion","attempt":1}`, strings.Repeat("a", maxIDText+1))), record: launchLogEventFired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := parseLaunchLogLine(tc.line, launchLogTestRun)
			require.Equal(t, launchLogEventDamaged, event.kind)
			require.Equal(t, tc.record, event.record)
			require.Equal(t, tc.agent, event.agent)
			require.NotContains(t, fmt.Sprintf("%+v", event), "RAW-PROSE-CANARY")
		})
	}

	for _, valid := range [][]byte{validFired, validRoute} {
		require.NotEqual(t, launchLogEventDamaged, parseLaunchLogLine(valid, launchLogTestRun).kind)
	}
}

func TestLaunchLogHistorySelectsLatestSessionInLogOrder(t *testing.T) {
	var history launchLogHistory
	history.apply(launchLogTestParseFired(t, "eng-1", 1))
	history.apply(launchLogTestParseRoute(t, "eng-1", "claude-fable-5-1", 1))
	first := launchLogTestParseRoute(t, "eng-1", "claude-opus-5-5", 2)
	first.route.account = "claude-work"
	first.route.at = launchLogTestStart.Add(-time.Hour)
	history.apply(first)

	attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
	require.True(t, ok)
	require.False(t, attempt.ambiguous)
	require.Len(t, attempt.routes, 2)
	require.Equal(t, "claude-opus-5-5", attempt.routes[1].model)
	require.Equal(t, "claude-work", attempt.routes[1].account)
	require.Equal(t, 2, attempt.routes[1].ordinal)
	require.Equal(t, launchLogTestStart.Add(-time.Hour), attempt.routes[1].at,
		"record order, not a timestamp comparison, selects the latest session")

	history.apply(launchLogTestParseFired(t, "eng-1", 2))
	second, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
	require.True(t, ok)
	require.Empty(t, second.routes, "a new attempt starts with no inherited route")
	history.apply(launchLogTestParseRoute(t, "eng-1", "claude-new-model", 3))
	require.Equal(t, "claude-new-model", second.routes[0].model)
	require.Equal(t, 1, second.routes[0].ordinal)
	require.Len(t, attempt.routes, 2, "the old exact-attempt history remains independent")
}

func TestLaunchLogHistoryRequiresFiredContextAndMarksDuplicateAttemptsUnknown(t *testing.T) {
	var history launchLogHistory
	history.apply(launchLogTestParseRoute(t, "eng-1", "orphan-model", 1))
	_, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
	require.False(t, ok, "a route cannot establish an attempt by agent name alone")

	history.apply(launchLogTestParseFired(t, "eng-1", 1))
	history.apply(launchLogTestParseRoute(t, "eng-other", "wrong-agent-model", 2))
	attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
	require.True(t, ok)
	require.Empty(t, attempt.routes)

	history.apply(launchLogTestParseRoute(t, "eng-1", "first-model", 3))
	history.apply(launchLogTestParseFired(t, "eng-1", 1))
	history.apply(launchLogTestParseRoute(t, "eng-1", "duplicate-model", 4))
	require.True(t, attempt.ambiguous, "a repeated fired key on resume is not a new session")
}

func TestLaunchLogHistoryDamageScopesAndCaps(t *testing.T) {
	t.Run("malformed route damages its current context", func(t *testing.T) {
		var history launchLogHistory
		history.apply(launchLogTestParseFired(t, "eng-1", 1))
		history.apply(launchLogEvent{kind: launchLogEventDamaged, record: launchLogEventRouted, agent: "eng-1"})
		attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
		require.True(t, ok)
		require.True(t, attempt.ambiguous)
	})

	t.Run("malformed context cannot produce pending later", func(t *testing.T) {
		var history launchLogHistory
		history.apply(launchLogEvent{kind: launchLogEventDamaged, record: launchLogEventFired, agent: "eng-1", attempt: 7})
		attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 7})
		require.True(t, ok)
		require.True(t, attempt.ambiguous)
		history.apply(launchLogTestParseRoute(t, "eng-1", "must-not-attach", 1))
		require.Empty(t, attempt.routes)
	})

	t.Run("session overflow is bounded and ambiguous", func(t *testing.T) {
		var history launchLogHistory
		history.apply(launchLogTestParseFired(t, "eng-1", 1))
		for i := 0; i < maxLaunchLogSessions+1; i++ {
			history.apply(launchLogTestParseRoute(t, "eng-1", fmt.Sprintf("model-%d", i), i+1))
		}
		attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
		require.True(t, ok)
		require.True(t, attempt.ambiguous)
		require.Empty(t, attempt.routes)
		require.LessOrEqual(t, len(attempt.routes), maxLaunchLogSessions)
	})

	t.Run("context overflow is bounded and missing is unknown", func(t *testing.T) {
		var history launchLogHistory
		history.startIncarnation(launchLogTestStart)
		for i := 0; i < maxLaunchLogContexts/2; i++ {
			history.apply(launchLogEvent{kind: launchLogEventFired, agent: fmt.Sprintf("eng-%d", i), attempt: 1, cartridge: "dandelion"})
		}
		history.startIncarnation(launchLogTestStart.Add(time.Second))
		for i := maxLaunchLogContexts / 2; i < maxLaunchLogContexts+1; i++ {
			history.apply(launchLogEvent{kind: launchLogEventFired, agent: fmt.Sprintf("eng-%d", i), attempt: 1, cartridge: "dandelion"})
		}
		require.Len(t, history.attempts, maxLaunchLogContexts)
		require.LessOrEqual(t, len(history.current), maxLaunchLogContexts)
		_, ok := history.attempt(launchAttemptKey{agent: fmt.Sprintf("eng-%d", maxLaunchLogContexts), attempt: 1, incarnation: 2})
		require.False(t, ok)
		oldest, ok := history.attempt(launchAttemptKey{agent: "eng-0", attempt: 1, incarnation: 1})
		require.True(t, ok)
		require.False(t, oldest.ambiguous, "older generations stay intact when the shared cap is reached")
	})
}

func launchLogTestAgent(id domain.AgentID, attempt int, state domain.AgentState) tui.AgentView {
	return tui.AgentView{
		Agent: domain.Agent{ID: id, Role: domain.RoleEngineer, Cartridge: "dandelion", Model: "high", State: state, StartedAt: launchLogTestStart},
		Task:  events.TaskImplement, Attempt: attempt,
	}
}

func launchLogTestObserveSpawn(o *Observer, view tui.AgentView) {
	if o.launchIncarnation == 0 {
		o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(o.opts.Run.ID, launchLogTestStart)})
		o.launchLog.apply(launchLogEvent{kind: launchLogEventRunStarted, at: launchLogTestStart})
	}
	launchLogTestSpawn(o, view, launchLogTestStart)
}

func launchLogTestSpawn(o *Observer, view tui.AgentView, at time.Time) {
	o.trackAgentFailure(events.AgentSpawned{Meta: events.MetaAt(o.opts.Run.ID, at), Agent: view.Agent})
}

func TestObserverAgentRouteProjectionStatusesAndStaleClearing(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Now: func() time.Time { return launchLogTestStart }})
	view := launchLogTestAgent("eng-1", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, view)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-1", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-1", "claude-fable-5-1", 1))

	routed := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusRouted, routed.RouteStatus)
	require.Equal(t, "high", routed.Model, "the configured mode remains the requested model")
	require.Equal(t, "claude-fable-5-1", routed.RoutedModel)
	require.Equal(t, "claude", routed.RoutedAccount)
	require.Equal(t, "max", routed.RoutedEffort)
	require.Equal(t, 1, routed.RouteAttempt)
	require.Equal(t, 1, routed.RouteOrdinal)
	require.NotNil(t, routed.RoutedAt)

	secondSession := launchLogTestParseRoute(t, "eng-1", "claude-opus-5-5", 2)
	secondSession.route.account = "claude-work"
	o.launchLog.apply(secondSession)
	latest := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, "claude-opus-5-5", latest.RoutedModel)
	require.Equal(t, "claude-work", latest.RoutedAccount)
	require.Equal(t, 2, latest.RouteOrdinal)
	require.Equal(t, secondSession.route.at, *latest.RoutedAt)

	view.Attempt = 2
	missing := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusUnknown, missing.RouteStatus)
	require.Empty(t, missing.RoutedModel, "attempt two cannot inherit attempt one's model")
	require.Zero(t, missing.RouteAttempt, "missing history does not establish a route attempt")
	o.launchLog.apply(launchLogTestParseFired(t, "eng-1", 2))
	pending := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusPending, pending.RouteStatus)
	require.Empty(t, pending.RoutedModel)
	require.Equal(t, 2, pending.RouteAttempt)

	view.State = domain.AgentStateFailed
	o.trackAgentFailure(events.AgentFailed{Agent: view.ID, Attempt: 2, Failure: events.FailureRateLimited, Err: "RAW-ERROR-PROSE"})
	noRoute := o.projectAgent(view, launchLogTestStart, false)
	require.Equal(t, routeStatusNoRoute, noRoute.RouteStatus)
	require.Empty(t, noRoute.RoutedModel)
	require.Equal(t, 2, noRoute.RouteAttempt)

	view.Attempt = 3
	launchLogTestSpawn(o, view, launchLogTestStart)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-1", 3))
	view.State = domain.AgentStateRunning
	retry := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusPending, retry.RouteStatus)
	require.Empty(t, retry.RoutedModel)

	view.State = domain.AgentStateFailed
	o.trackAgentFailure(events.AgentFailed{Agent: view.ID, Attempt: 2, Failure: events.FailureRateLimited})
	mismatchedFailure := o.projectAgent(view, launchLogTestStart, false)
	require.Equal(t, routeStatusUnknown, mismatchedFailure.RouteStatus, "a typed failure for another attempt is not no_route")
}

func TestObserverAgentRouteProjectionFallbacks(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun(launchLogTestRun), RunDir: t.TempDir()})
	plain := launchLogTestAgent("plain-1", 1, domain.AgentStateRunning)
	plain.Cartridge = "claude"
	launchLogTestObserveSpawn(o, plain)
	o.launchLog.apply(launchLogEvent{kind: launchLogEventFired, agent: "plain-1", attempt: 1, cartridge: "claude"})
	o.launchLog.apply(launchLogTestParseRoute(t, "plain-1", "must-not-project", 1))
	nonDandelion := o.projectAgent(plain, launchLogTestStart, true)
	require.Empty(t, nonDandelion.RouteStatus)
	require.Empty(t, nonDandelion.RoutedModel)
	require.Empty(t, nonDandelion.RoutedAccount)
	require.Zero(t, nonDandelion.RouteAttempt)

	missingView := launchLogTestAgent("missing-1", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, missingView)
	missing := o.projectAgent(missingView, launchLogTestStart, true)
	require.Equal(t, routeStatusUnknown, missing.RouteStatus)

	crashed := launchLogTestAgent("crash-1", 1, domain.AgentStateFailed)
	launchLogTestObserveSpawn(o, crashed)
	o.launchLog.apply(launchLogTestParseFired(t, "crash-1", 1))
	o.trackAgentFailure(events.AgentFailed{Agent: crashed.ID, Attempt: 1, Failure: events.FailureCrash, Err: "rate_limited in prose only"})
	require.Equal(t, routeStatusUnknown, o.projectAgent(crashed, launchLogTestStart, false).RouteStatus)

	rateProse := launchLogTestAgent("prose-1", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, rateProse)
	o.launchLog.apply(launchLogTestParseFired(t, "prose-1", 1))
	o.apply(events.Warning{Meta: events.MetaAt(launchLogTestRun, launchLogTestStart), Agent: rateProse.ID,
		Source: "supervisor", Message: "rate_limited: no route in message prose"})
	require.Equal(t, routeStatusPending, o.projectAgent(rateProse, launchLogTestStart, true).RouteStatus)

	duplicate := launchLogTestAgent("duplicate-1", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, duplicate)
	o.launchLog.apply(launchLogTestParseFired(t, "duplicate-1", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "duplicate-1", "first-model", 1))
	o.launchLog.apply(launchLogTestParseFired(t, "duplicate-1", 1))
	unknown := o.projectAgent(duplicate, launchLogTestStart, true)
	require.Equal(t, routeStatusUnknown, unknown.RouteStatus)
	require.Empty(t, unknown.RoutedModel)
	require.Nil(t, unknown.RoutedAt)
	require.Equal(t, 1, unknown.RouteAttempt, "an ambiguous but exact key still prevents cross-attempt rendering")
	require.Zero(t, missing.RouteAttempt)
	require.Equal(t, 1, o.projectAgent(crashed, launchLogTestStart, false).RouteAttempt)
}

func TestObserverRouteDTOsRenderProductionFallbacks(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("production JavaScript rendering requires node")
	}
	o := NewObserver(Options{Run: observerTestRun(launchLogTestRun), RunDir: t.TempDir()})
	pendingView := launchLogTestAgent("eng-pending", 2, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, pendingView)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-pending", 2))
	pending := o.projectAgent(pendingView, launchLogTestStart, true)
	noRouteView := launchLogTestAgent("eng-no-route", 16, domain.AgentStateFailed)
	launchLogTestObserveSpawn(o, noRouteView)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-no-route", 16))
	o.trackAgentFailure(events.AgentFailed{Agent: noRouteView.ID, Attempt: 16, Failure: events.FailureRateLimited})
	noRoute := o.projectAgent(noRouteView, launchLogTestStart, false)
	require.Equal(t, routeStatusPending, pending.RouteStatus)
	require.Equal(t, 2, pending.RouteAttempt)
	require.Equal(t, routeStatusNoRoute, noRoute.RouteStatus)
	require.Equal(t, 16, noRoute.RouteAttempt)

	pendingJSON, err := json.Marshal(pending)
	require.NoError(t, err)
	noRouteJSON, err := json.Marshal(noRoute)
	require.NoError(t, err)
	appPath, err := filepath.Abs(filepath.Join("static", "app.js"))
	require.NoError(t, err)
	script := `
class Element {
  constructor() { this.children = []; this.className = ""; this.textContent = ""; this.title = ""; this.style = {}; this.attributes = {}; }
  append(...children) { this.children.push(...children); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
}
global.document = {
  body: { dataset: { basePath: "/" } },
  createElement: () => new Element(),
  getElementById: () => null,
};
const app = require(process.argv[1]);
const subtitles = [2, 3].map((index) => {
  const agent = JSON.parse(process.argv[index]);
  return app.agentCard(agent, agent.live).children[1].textContent;
});
process.stdout.write(JSON.stringify(subtitles));
`
	command := exec.Command("node", "-e", script, appPath, string(pendingJSON), string(noRouteJSON)) //nolint:gosec // fixed renderer and test DTOs
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	var subtitles []string
	require.NoError(t, json.Unmarshal(output, &subtitles), string(output))
	require.Equal(t, []string{"engineer · dandelion · routing…", "engineer · dandelion · no route"}, subtitles)
}

func launchLogTestFindAgent(t *testing.T, snapshot Snapshot, id domain.AgentID) AgentSnapshot {
	t.Helper()
	for _, agents := range [][]AgentSnapshot{snapshot.LiveAgents, snapshot.RecentAgents} {
		for _, agent := range agents {
			if agent.ID == id {
				return agent
			}
		}
	}
	return AgentSnapshot{}
}

func TestObserverIncarnationHistoryAcrossThreeGenerations(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Now: func() time.Time { return launchLogTestStart }})
	view := launchLogTestAgent("eng-reuse", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, view)
	o.runState.Apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart), Agent: view.Agent, Task: view.Task, Attempt: 1})
	o.launchLog.apply(launchLogTestParseFired(t, "eng-reuse", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-reuse", "model-one", 1))
	o.publish()
	first := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, routeStatusRouted, first.RouteStatus)
	require.Equal(t, "model-one", first.RoutedModel)
	require.Equal(t, 1, first.RouteAttempt)
	originalTime := *first.RoutedAt
	*first.RoutedAt = launchLogTestStart.Add(24 * time.Hour)
	first.RoutedModel = "MUTATED-COPY"
	again := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, "model-one", again.RoutedModel)
	require.Equal(t, originalTime, *again.RoutedAt)
	require.NotSame(t, first.RoutedAt, again.RoutedAt)

	o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(time.Second)), Info: run, Resumed: true})
	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, launchLogTestStart.Add(time.Second)))
	require.Equal(t, "model-one", o.projectAgent(view, launchLogTestStart, false).RoutedModel,
		"an old last-seen card keeps its exact history until a new spawn replaces its generation")
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(2*time.Second)), Agent: view.Agent, Task: view.Task, Attempt: 1})
	o.launchLog.apply(launchLogTestParseFired(t, "eng-reuse", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-reuse", "model-two", 2))
	o.publish()
	second := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, "model-two", second.RoutedModel)
	require.Equal(t, 1, second.RouteAttempt)

	o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(3*time.Second)), Info: run, Resumed: true})
	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, launchLogTestStart.Add(3*time.Second)))
	require.Equal(t, "model-two", o.projectAgent(view, launchLogTestStart, false).RoutedModel)
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(4*time.Second)), Agent: view.Agent, Task: view.Task, Attempt: 1})
	o.launchLog.apply(launchLogTestParseFired(t, "eng-reuse", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-reuse", "model-three", 3))
	third := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, "model-three", third.RoutedModel)
	require.Equal(t, 1, third.RouteAttempt)

	o.launchLog.apply(launchLogTestParseFired(t, "eng-reuse", 1))
	duplicate := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusUnknown, duplicate.RouteStatus, "duplicates within one generation remain ambiguous")
	require.Empty(t, duplicate.RoutedModel)
	require.Equal(t, 1, duplicate.RouteAttempt)
	for _, incarnation := range []uint64{1, 2} {
		attempt, ok := o.launchLog.attempt(launchAttemptKey{agent: string(view.ID), attempt: 1, incarnation: incarnation})
		require.True(t, ok)
		require.False(t, attempt.ambiguous, "a current-generation duplicate must not damage older history")
	}
}

func TestObserverIncarnationArrivalRacesAndNoRouteAcrossResume(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	view := launchLogTestAgent("eng-resume", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, view)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-resume", 1))
	o.trackAgentFailure(events.AgentFailed{Agent: view.ID, Attempt: 1, Failure: events.FailureRateLimited})
	view.State = domain.AgentStateFailed
	require.Equal(t, routeStatusNoRoute, o.projectAgent(view, launchLogTestStart, false).RouteStatus)

	o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(time.Second)), Info: run, Resumed: true})
	require.Equal(t, routeStatusNoRoute, o.projectAgent(view, launchLogTestStart, false).RouteStatus,
		"resume itself does not clear the old generation's typed failure")
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(2*time.Second)), Agent: view.Agent, Task: view.Task, Attempt: 1})
	view.State = domain.AgentStateRunning
	racing := o.projectAgent(view, launchLogTestStart, true)
	require.Equal(t, routeStatusUnknown, racing.RouteStatus, "a spawn ahead of its log boundary has no confirmed generation")
	require.Empty(t, racing.RoutedModel)

	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, launchLogTestStart.Add(time.Second)))
	o.launchLog.apply(launchLogTestParseFired(t, "eng-resume", 1))
	require.Equal(t, routeStatusPending, o.projectAgent(view, launchLogTestStart, true).RouteStatus)
	o.trackAgentFailure(events.AgentFailed{Agent: view.ID, Attempt: 1, Failure: events.FailureRateLimited})
	view.State = domain.AgentStateFailed
	require.Equal(t, routeStatusNoRoute, o.projectAgent(view, launchLogTestStart, false).RouteStatus)
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-resume", "model-two", 2))
	require.Equal(t, "model-two", o.projectAgent(view, launchLogTestStart, false).RoutedModel)

	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, launchLogTestStart.Add(2*time.Second)))
	o.launchLog.apply(launchLogTestParseFired(t, "eng-resume", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-resume", "model-three", 3))
	require.Equal(t, "model-two", o.projectAgent(view, launchLogTestStart, false).RoutedModel,
		"a log-only advance cannot move an old visible card to a new generation")
	o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(3*time.Second)), Info: run, Resumed: true})
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(4*time.Second)), Agent: view.Agent, Task: view.Task, Attempt: 1})
	view.State = domain.AgentStateRunning
	require.Equal(t, "model-three", o.projectAgent(view, launchLogTestStart, true).RoutedModel)
}

func TestObserverIncarnationMalformedAndOutOfOrderStarts(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	view := launchLogTestAgent("eng-out-of-order", 1, domain.AgentStateRunning)
	launchLogTestSpawn(o, view, launchLogTestStart)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-out-of-order", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-out-of-order", "model-zero", 1))
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, launchLogTestStart, true).RouteStatus,
		"a fired record before the first run boundary is not confirmed history")

	o.launchLog.apply(launchLogTestParseRunStarted(t, false))
	require.Equal(t, uint64(1), o.launchLog.incarnation)
	require.Empty(t, o.launchLog.current)
	o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart)})
	launchLogTestSpawn(o, view, launchLogTestStart)
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, launchLogTestStart, true).RouteStatus)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-out-of-order", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-out-of-order", "model-one", 1))
	require.Equal(t, "model-one", o.projectAgent(view, launchLogTestStart, true).RoutedModel)

	wrongRun := parseLaunchLogLine(launchLogTestRunStarted(t, "another-run", true, launchLogTestStart), launchLogTestRun)
	require.Equal(t, launchLogEventNone, wrongRun.kind)
	malformed := parseLaunchLogLine([]byte(`{"time":"2026-10-24T12:00:00Z","msg":"run started","run":"run-launch","resumed":"yes"}`), launchLogTestRun)
	require.Equal(t, launchLogEventDamaged, malformed.kind)
	o.launchLog.apply(malformed)
	require.Equal(t, uint64(1), o.launchLog.incarnation, "a malformed start cannot advance the ordinal")
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, launchLogTestStart, true).RouteStatus)

	o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart.Add(time.Second))})
	launchLogTestSpawn(o, view, launchLogTestStart.Add(time.Second))
	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, launchLogTestStart.Add(time.Second)))
	o.launchLog.apply(launchLogTestParseFired(t, "eng-out-of-order", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-out-of-order", "model-two", 2))
	require.Equal(t, "model-two", o.projectAgent(view, launchLogTestStart, true).RoutedModel)
	previous, ok := o.launchLog.attempt(launchAttemptKey{agent: string(view.ID), attempt: 1, incarnation: 1})
	require.True(t, ok)
	require.True(t, previous.ambiguous, "malformed current-generation history does not silently recover")
}

func TestObserverRouteGenerationSurvivesEventStartRotation(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Now: func() time.Time { return launchLogTestStart.Add(time.Minute) }})
	view := launchLogTestAgent("eng-rotated", 1, domain.AgentStateRunning)
	var launchLines [][]byte
	for generation := 1; generation <= 5; generation++ {
		eventAt := launchLogTestStart.Add(time.Duration(generation) * 10 * time.Second)
		launchLines = append(launchLines,
			launchLogTestRunStarted(t, run.ID, generation > 1, eventAt.Add(-500*time.Millisecond)),
			launchLogTestFired(t, run.ID, string(view.ID), 1, "dandelion", eventAt),
			launchLogTestRoute(t, run.ID, string(view.ID), "claude", fmt.Sprintf("model-%d", generation), "max", false, eventAt.Add(time.Second)),
		)
	}
	content := launchLogTestContent(launchLines...)
	o.launchLog = *parseLaunchLogHistory(content, run.ID)
	require.Equal(t, uint64(5), o.launchLog.incarnation)

	for generation := 2; generation <= 5; generation++ {
		eventAt := launchLogTestStart.Add(time.Duration(generation) * 10 * time.Second)
		o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, eventAt), Info: run, Resumed: generation > 2})
		if generation == 5 {
			old := o.projectAgent(view, eventAt, false)
			require.Equal(t, routeStatusRouted, old.RouteStatus)
			require.Equal(t, "model-4", old.RoutedModel, "a last-seen card retains its frozen interval")
			require.Equal(t, 1, old.RouteOrdinal)
		}
		spawnAt := eventAt.Add(2 * time.Second)
		view.StartedAt = spawnAt
		o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, spawnAt), Agent: view.Agent, Task: view.Task, Attempt: 1})
		projected := o.projectAgent(view, spawnAt, true)
		require.Equal(t, routeStatusRouted, projected.RouteStatus, "generation %d", generation)
		require.Equal(t, fmt.Sprintf("model-%d", generation), projected.RoutedModel)
		require.Equal(t, 1, projected.RouteOrdinal)
	}
	require.Equal(t, uint64(4), o.launchIncarnation, "only four event starts survived rotation")

	o.publish()
	first := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, "model-5", first.RoutedModel)
	require.NotNil(t, first.RoutedAt)
	originalRouteTime := *first.RoutedAt
	*first.RoutedAt = first.RoutedAt.Add(24 * time.Hour)
	first.RoutedModel = "MUTATED-COPY"

	o.launchLog.apply(launchLogEvent{kind: launchLogEventHistory, history: parseLaunchLogHistory(content, run.ID)})
	o.publish()
	replayed := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, routeStatusRouted, replayed.RouteStatus)
	require.Equal(t, "model-5", replayed.RoutedModel, "current-run replay recovers the fifth launch generation")
	require.Equal(t, "claude", replayed.RoutedAccount)
	require.Equal(t, "max", replayed.RoutedEffort)
	require.Equal(t, 1, replayed.RouteOrdinal)
	require.Equal(t, originalRouteTime, *replayed.RoutedAt)
	require.NotSame(t, first.RoutedAt, replayed.RoutedAt)
}

func TestObserverRouteGenerationRejectsSkewAndInvalidIntervals(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	view := launchLogTestAgent("eng-skew", 1, domain.AgentStateRunning)
	launchLogTestObserveSpawn(o, view)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-skew", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-skew", "model-one", 1))

	secondStart := launchLogTestStart.Add(10 * time.Second)
	o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(run.ID, secondStart), Info: run, Resumed: true})
	launchLogTestSpawn(o, view, secondStart.Add(time.Second))
	racing := o.projectAgent(view, secondStart.Add(time.Second), true)
	require.Equal(t, routeStatusUnknown, racing.RouteStatus)
	require.Empty(t, racing.RoutedModel, "an event start ahead of sc.log must not select the previous generation")

	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, secondStart.Add(-time.Millisecond)))
	o.launchLog.apply(launchLogTestParseFired(t, "eng-skew", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-skew", "model-two", 2))
	require.Equal(t, "model-two", o.projectAgent(view, secondStart.Add(time.Second), true).RoutedModel)

	launch := o.agentLaunches[view.ID]
	launch.spawnAt = secondStart.Add(-time.Second)
	o.agentLaunches[view.ID] = launch
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, secondStart, true).RouteStatus, "spawn-before-start is invalid")
	launch = o.agentLaunches[view.ID]
	launch.spawnAt = secondStart.Add(time.Second)
	launch.previous.at = launch.current.at
	o.agentLaunches[view.ID] = launch
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, secondStart, true).RouteStatus, "equal interval boundaries are invalid")
	launch.previous.at = launch.current.at.Add(time.Second)
	o.agentLaunches[view.ID] = launch
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, secondStart, true).RouteStatus, "reversed interval boundaries are invalid")

	thirdStart := secondStart.Add(10 * time.Second)
	o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(run.ID, thirdStart), Info: run, Resumed: true})
	launchLogTestSpawn(o, view, thirdStart.Add(time.Second))
	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, thirdStart.Add(-2*time.Millisecond)))
	o.launchLog.apply(launchLogTestParseRunStartedAt(t, true, thirdStart.Add(-time.Millisecond)))
	_, ok := o.launchLog.resolveGeneration(o.agentLaunches[view.ID])
	require.False(t, ok, "two direct starts in one event interval are ambiguous")
	require.Equal(t, routeStatusUnknown, o.projectAgent(view, thirdStart, true).RouteStatus)
}

func TestObserverIncarnationMapCapFailsUnknown(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	o.trackAgentFailure(events.RunStarted{Meta: events.MetaAt(run.ID, launchLogTestStart)})
	o.launchLog.apply(launchLogTestParseRunStarted(t, false))
	for i := 0; i < maxLaunchLogContexts; i++ {
		o.trackAgentFailure(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart), Agent: domain.Agent{ID: domain.AgentID(fmt.Sprintf("eng-%d", i))}})
	}
	target := launchLogTestAgent("eng-overflow", 1, domain.AgentStateRunning)
	o.trackAgentFailure(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart), Agent: target.Agent})
	o.launchLog.apply(launchLogTestParseFired(t, "eng-overflow", 1))
	o.launchLog.apply(launchLogTestParseRoute(t, "eng-overflow", "must-not-attach", 1))
	projected := o.projectAgent(target, launchLogTestStart, true)
	require.Len(t, o.agentLaunches, maxLaunchLogContexts)
	require.Equal(t, routeStatusUnknown, projected.RouteStatus)
	require.Empty(t, projected.RoutedModel)
}

func TestObserverLaunchLogBacklogLiveRetriesAndStaleModelClearing(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun(launchLogTestRun)
	agent := domain.Agent{ID: "eng-live", Role: domain.RoleEngineer, Cartridge: "dandelion", Model: "high",
		Bead: "sc-1", State: domain.AgentStateRunning, StartedAt: run.StartedAt}
	spawn := func(attempt int) events.AgentSpawned {
		return events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Duration(attempt)*time.Second)),
			Agent: agent, Task: events.TaskImplement, Attempt: attempt}
	}
	observerTestAppend(t, dir, observerTestStarted(run), spawn(1))
	launchPath := filepath.Join(dir, launchLogName)
	launchLogTestAppend(t, launchPath,
		launchLogTestRunStarted(t, run.ID, false, run.StartedAt),
		launchLogTestFired(t, run.ID, string(agent.ID), 1, "dandelion", run.StartedAt),
		launchLogTestRoute(t, run.ID, string(agent.ID), "claude-work", "claude-fable-5-1", "max", false, run.StartedAt.Add(time.Second)),
	)
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll, LaunchLogInterval: observerTestPoll})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)

	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		agent := launchLogTestFindAgent(t, snapshot, agent.ID)
		return agent.RouteStatus == routeStatusRouted && agent.RouteAttempt == 1
	})
	first := launchLogTestFindAgent(t, o.Snapshot(), agent.ID)
	require.Equal(t, "claude-fable-5-1", first.RoutedModel)
	require.Equal(t, "claude-work", first.RoutedAccount)
	require.Equal(t, "high", first.Model)
	require.Equal(t, first.Attempt, first.RouteAttempt)

	observerTestAppend(t, dir,
		events.AgentFailed{Meta: events.MetaAt(run.ID, run.StartedAt.Add(2*time.Second)), Agent: agent.ID,
			Failure: events.FailureCrash, Attempt: 1},
		spawn(2),
	)
	launchLogTestAppend(t, launchPath, launchLogTestFired(t, run.ID, string(agent.ID), 2, "dandelion", run.StartedAt.Add(3*time.Second)))
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		agent := launchLogTestFindAgent(t, snapshot, agent.ID)
		return agent.Attempt == 2 && agent.RouteStatus == routeStatusPending
	})
	second := launchLogTestFindAgent(t, o.Snapshot(), agent.ID)
	require.Empty(t, second.RoutedModel)
	require.Empty(t, second.RoutedAccount)
	require.Nil(t, second.RoutedAt)
	require.Equal(t, 2, second.RouteAttempt)

	launchLogTestAppend(t, launchPath, launchLogTestRoute(t, run.ID, string(agent.ID), "claude", "claude-opus-5-5", "max", true, run.StartedAt.Add(4*time.Second)))
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return launchLogTestFindAgent(t, snapshot, agent.ID).RouteStatus == routeStatusRouted &&
			launchLogTestFindAgent(t, snapshot, agent.ID).RouteAttempt == 2
	})
	require.Equal(t, "claude-opus-5-5", launchLogTestFindAgent(t, o.Snapshot(), agent.ID).RoutedModel)

	observerTestAppend(t, dir,
		events.AgentFailed{Meta: events.MetaAt(run.ID, run.StartedAt.Add(5*time.Second)), Agent: agent.ID,
			Failure: events.FailureRateLimited, Attempt: 2, WillRetry: true},
		spawn(3),
	)
	launchLogTestAppend(t, launchPath, launchLogTestFired(t, run.ID, string(agent.ID), 3, "dandelion", run.StartedAt.Add(6*time.Second)))
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return launchLogTestFindAgent(t, snapshot, agent.ID).Attempt == 3 &&
			launchLogTestFindAgent(t, snapshot, agent.ID).RouteStatus == routeStatusPending
	})
	observerTestAppend(t, dir, events.AgentFailed{Meta: events.MetaAt(run.ID, run.StartedAt.Add(7*time.Second)), Agent: agent.ID,
		Failure: events.FailureRateLimited, Attempt: 3, WillRetry: true, Err: "RAW-RATE-LIMIT-PROSE-CANARY"})
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return launchLogTestFindAgent(t, snapshot, agent.ID).RouteStatus == routeStatusNoRoute
	})
	noRoute := launchLogTestFindAgent(t, o.Snapshot(), agent.ID)
	require.Equal(t, 3, noRoute.Attempt)
	require.Empty(t, noRoute.RoutedModel)
	require.Equal(t, 3, noRoute.RouteAttempt)

	observerTestAppend(t, dir, spawn(4))
	launchLogTestAppend(t, launchPath, launchLogTestFired(t, run.ID, string(agent.ID), 4, "dandelion", run.StartedAt.Add(8*time.Second)))
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return launchLogTestFindAgent(t, snapshot, agent.ID).Attempt == 4 &&
			launchLogTestFindAgent(t, snapshot, agent.ID).RouteStatus == routeStatusPending
	})
	require.Empty(t, launchLogTestFindAgent(t, o.Snapshot(), agent.ID).RoutedModel,
		"a no-route retry cannot expose an older attempt's model")
}

func TestObserverRouteSnapshotIsBoundedRedactedAndDeepCopied(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Now: func() time.Time { return launchLogTestStart }})
	view := launchLogTestAgent("eng-safe", 1, domain.AgentStateRunning)
	view.BeadTitle = "safe title"
	o.runState.Apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, launchLogTestStart), Agent: view.Agent, Task: view.Task, Attempt: 1})
	launchLogTestObserveSpawn(o, view)
	o.launchLog.apply(launchLogTestParseFired(t, "eng-safe", 1))
	huge := strings.Repeat("x", 8*1024)
	selected := launchLogTestParseRoute(t, "eng-safe", "model", 1)
	selected.route.account = "claude\naccount"
	selected.route.model = huge
	selected.route.effort = "max\teffort"
	selected.route.at = launchLogTestStart
	o.launchLog.apply(selected)
	o.apply(events.Warning{Meta: events.MetaAt(run.ID, launchLogTestStart), Agent: view.ID, Source: "supervisor",
		Message: "RAW-RATE-LIMIT-PROSE-CANARY", Err: "RAW-RATE-LIMIT-PROSE-CANARY"})
	o.publish()
	snapshot := o.Snapshot()
	agent := launchLogTestFindAgent(t, snapshot, view.ID)
	require.Equal(t, routeStatusRouted, agent.RouteStatus)
	require.Equal(t, maxIDText, utf8.RuneCountInString(agent.RoutedModel))
	require.NotContains(t, agent.RoutedAccount, "\n")
	require.NotContains(t, agent.RoutedEffort, "\t")

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, canary := range []string{"SANITIZED-WORKDIR-CANARY", "SANITIZED-UNKNOWN-FIELD-CANARY", "RAW-RATE-LIMIT-PROSE-CANARY"} {
		require.NotContains(t, string(encoded), canary)
	}
	require.Less(t, len(encoded), 100_000)
	var raw struct {
		LiveAgents   []map[string]json.RawMessage `json:"live_agents"`
		RecentAgents []map[string]json.RawMessage `json:"recent_agents"`
	}
	require.NoError(t, json.Unmarshal(encoded, &raw))
	agents := append(raw.LiveAgents, raw.RecentAgents...)
	require.Len(t, agents, 1)
	for _, forbidden := range []string{"workdir", "cached", "policy", "notes", "line", "record"} {
		require.NotContains(t, agents[0], forbidden)
	}
	require.Contains(t, string(agents[0]["route_status"]), routeStatusRouted)

	*agent.RoutedAt = launchLogTestStart.Add(24 * time.Hour)
	agent.RoutedModel = "MUTATED-COPY"
	agent.RouteStatus = "MUTATED-COPY"
	again := launchLogTestFindAgent(t, o.Snapshot(), view.ID)
	require.Equal(t, launchLogTestStart, *again.RoutedAt)
	require.NotEqual(t, "MUTATED-COPY", again.RoutedModel)
	require.Equal(t, routeStatusRouted, again.RouteStatus)

	plain := AgentSnapshot{}
	plainJSON, err := json.Marshal(plain)
	require.NoError(t, err)
	require.Contains(t, string(plainJSON), `"route_status":""`)
}

type launchLogTestAtomic struct {
	t         *testing.T
	collector *launchLogCollector
	out       chan launchLogEvent
	history   launchLogHistory
	reads     []launchLogReadResult
	calls     int
}

func newLaunchLogTestAtomic(t *testing.T, reads ...launchLogReadResult) *launchLogTestAtomic {
	t.Helper()
	a := &launchLogTestAtomic{t: t, out: make(chan launchLogEvent, 16), reads: reads}
	a.collector = &launchLogCollector{runID: launchLogTestRun, out: a.out, read: a.read}
	return a
}

func launchLogTestResult(data []byte) launchLogReadResult {
	return launchLogReadResult{data: data, status: launchLogReadOK, readBytes: int64(len(data))}
}

func (a *launchLogTestAtomic) read(ctx context.Context, _ string) launchLogReadResult {
	if ctx.Err() != nil {
		return launchLogReadResult{status: launchLogReadCanceled}
	}
	if len(a.reads) == 0 {
		a.t.Fatal("unexpected launch-log read")
	}
	result := a.reads[0]
	a.reads = a.reads[1:]
	a.calls++
	return result
}

func (a *launchLogTestAtomic) refresh() {
	a.t.Helper()
	require.True(a.t, a.collector.refresh(context.Background()))
	launchLogTestDrainHistory(a.out, &a.history)
}

type launchLogTestBlockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *launchLogTestBlockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return len(p), nil
}

func launchLogTestNextEvent(t *testing.T, out <-chan launchLogEvent) launchLogEvent {
	t.Helper()
	select {
	case event := <-out:
		return event
	case <-time.After(observerTestWait):
		t.Fatal("timed out waiting for launch-log event")
		return launchLogEvent{}
	}
}

func launchLogTestStartCollector(t *testing.T, path string, run domain.RunID) (<-chan launchLogEvent, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan launchLogEvent, 512)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		collectLaunchLog(ctx, path, run, observerTestPoll, out)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(observerTestWait):
			t.Error("launch-log collector did not stop")
		}
	})
	return out, cancel, done
}

func launchLogTestDrainHistory(out <-chan launchLogEvent, history *launchLogHistory) []launchLogEvent {
	var drained []launchLogEvent
	for {
		select {
		case event, ok := <-out:
			if !ok {
				return drained
			}
			drained = append(drained, event)
			history.apply(event)
		default:
			return drained
		}
	}
}

func launchLogTestWaitHistory(t *testing.T, out <-chan launchLogEvent, history *launchLogHistory, condition func() bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		launchLogTestDrainHistory(out, history)
		return condition()
	}, observerTestWait, observerTestPoll)
}

func TestLaunchLogCollectorBackfillLivePartialAndMissingFile(t *testing.T) {
	t.Run("backlog then complete live appends", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), launchLogName)
		launchLogTestAppend(t, path,
			launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
			launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-backlog", "max", true, launchLogTestStart.Add(time.Second)),
		)
		out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
		var history launchLogHistory
		launchLogTestWaitHistory(t, out, &history, func() bool {
			attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
			return ok && len(attempt.routes) == 1 && attempt.routes[0].model == "model-backlog"
		})

		fired := launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second))
		observerTestAppendRaw(t, path, fired[:len(fired)/2])
		time.Sleep(40 * time.Millisecond)
		launchLogTestDrainHistory(out, &history)
		_, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
		require.False(t, ok, "a partial JSON line is not a fired context")
		observerTestAppendRaw(t, path, append(fired[len(fired)/2:], '\n'))
		launchLogTestWaitHistory(t, out, &history, func() bool {
			_, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
			return ok
		})
		launchLogTestAppend(t, path, launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-live", "max", false, launchLogTestStart.Add(3*time.Second)))
		launchLogTestWaitHistory(t, out, &history, func() bool {
			attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
			return ok && len(attempt.routes) == 1 && attempt.routes[0].model == "model-live"
		})
	})

	t.Run("missing file appears without being created by the reader", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), launchLogName)
		out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
		time.Sleep(20 * time.Millisecond)
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		launchLogTestAppend(t, path, launchLogTestFired(t, launchLogTestRun, "eng-later", 1, "dandelion", launchLogTestStart))
		var history launchLogHistory
		launchLogTestWaitHistory(t, out, &history, func() bool {
			_, ok := history.attempt(launchAttemptKey{agent: "eng-later", attempt: 1})
			return ok
		})
	})
}

func TestLaunchLogCollectorOrdinaryAppendDoesNotReplayOrInvalidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), launchLogName)
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-one", "max", false, launchLogTestStart.Add(time.Second)),
	)
	out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
	var history launchLogHistory
	var observed []launchLogEvent
	waitFor := func(condition func() bool) {
		t.Helper()
		require.Eventually(t, func() bool {
			observed = append(observed, launchLogTestDrainHistory(out, &history)...)
			return condition()
		}, observerTestWait, observerTestPoll)
	}
	routed := func(attempt int, model string) func() bool {
		return func() bool {
			context, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: attempt})
			return ok && !context.ambiguous && len(context.routes) == 1 && context.routes[0].model == model
		}
	}
	waitFor(routed(1, "model-one"))
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-two", "max", true, launchLogTestStart.Add(3*time.Second)),
	)
	waitFor(routed(2, "model-two"))
	require.True(t, routed(1, "model-one")(), "an append must not invalidate the already-consumed attempt")
	for _, event := range observed {
		require.Equal(t, launchLogEventHistory, event.kind, "ordinary appends publish only fresh complete histories")
	}

	time.Sleep(20 * time.Millisecond)
	observed = append(observed, launchLogTestDrainHistory(out, &history)...)
	beforeTouch := len(observed)
	changed := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, changed, changed))
	time.Sleep(40 * time.Millisecond)
	observed = append(observed, launchLogTestDrainHistory(out, &history)...)
	require.Len(t, observed, beforeTouch, "an mtime-only change with identical content publishes no new history")

	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 3, "dandelion", launchLogTestStart.Add(4*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-three", "max", false, launchLogTestStart.Add(5*time.Second)),
	)
	waitFor(routed(3, "model-three"))
	require.True(t, routed(1, "model-one")())
	require.True(t, routed(2, "model-two")())
	for _, event := range observed {
		require.Equal(t, launchLogEventHistory, event.kind)
	}
}

func TestLaunchLogCollectorRewriteDuringPartialLineNeverCrossesAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), launchLogName)
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-old", "max", false, launchLogTestStart.Add(time.Second)),
	)
	out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
	var history launchLogHistory
	firstKey := launchAttemptKey{agent: "eng-1", attempt: 1}
	secondKey := launchAttemptKey{agent: "eng-1", attempt: 2}
	launchLogTestWaitHistory(t, out, &history, func() bool {
		attempt, ok := history.attempt(firstKey)
		return ok && len(attempt.routes) == 1 && attempt.routes[0].model == "model-old"
	})

	partial := launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second))
	observerTestAppendRaw(t, path, partial[:len(partial)*2/3])
	time.Sleep(60 * time.Millisecond)
	launchLogTestDrainHistory(out, &history)
	_, ok := history.attempt(secondKey)
	require.False(t, ok, "the incomplete second attempt is not established")

	newLines := [][]byte{
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-new", "max", false, launchLogTestStart.Add(time.Second)),
		launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-second", "max", false, launchLogTestStart.Add(3*time.Second)),
	}
	var replacement []byte
	for _, line := range newLines {
		replacement = append(replacement, line...)
		replacement = append(replacement, '\n')
	}
	require.NoError(t, os.WriteFile(path, replacement, 0o600))

	wrongAttemptRoute := false
	require.Eventually(t, func() bool {
		for {
			select {
			case event := <-out:
				history.apply(event)
				for key, attempt := range history.attempts {
					for _, route := range attempt.routes {
						if key.attempt == 2 && route.model != "model-second" {
							wrongAttemptRoute = true
						}
						if key.attempt == 1 && route.model != "model-old" && route.model != "model-new" {
							wrongAttemptRoute = true
						}
					}
				}
				first, firstOK := history.attempt(firstKey)
				second, secondOK := history.attempt(secondKey)
				if firstOK && secondOK && !first.ambiguous && !second.ambiguous &&
					len(first.routes) == 1 && first.routes[0].model == "model-new" &&
					len(second.routes) == 1 && second.routes[0].model == "model-second" {
					return true
				}
			default:
				return false
			}
		}
	}, observerTestWait, observerTestPoll)
	require.False(t, wrongAttemptRoute, "an uncertain rewrite must not attach a route to another attempt")
}

func TestLaunchLogCollectorDetectsTruncationAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
		larger  bool
	}{
		{name: "same size truncation and rewrite"},
		{name: "larger truncation and rewrite", larger: true},
		{name: "replacement", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, launchLogName)
			oldLines := [][]byte{
				launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
				launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-old", "max", false, launchLogTestStart.Add(time.Second)),
			}
			launchLogTestAppend(t, path, oldLines...)
			out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
			var history launchLogHistory
			key := launchAttemptKey{agent: "eng-1", attempt: 1}
			launchLogTestWaitHistory(t, out, &history, func() bool {
				attempt, ok := history.attempt(key)
				return ok && len(attempt.routes) == 1 && attempt.routes[0].model == "model-old"
			})

			newLines := [][]byte{
				launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
				launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-new", "max", false, launchLogTestStart.Add(time.Second)),
			}
			if tc.larger {
				newLines = append(newLines,
					launchLogTestFired(t, launchLogTestRun, "eng-2", 1, "dandelion", launchLogTestStart.Add(2*time.Second)),
					launchLogTestRoute(t, launchLogTestRun, "eng-2", "claude", "model-second", "max", false, launchLogTestStart.Add(3*time.Second)),
				)
			}
			if tc.replace {
				require.NoError(t, os.Rename(path, filepath.Join(dir, "old.log")))
				time.Sleep(20 * time.Millisecond)
				launchLogTestAppend(t, path, newLines...)
			} else {
				var data []byte
				for _, line := range newLines {
					data = append(data, line...)
					data = append(data, '\n')
				}
				var oldData []byte
				for _, line := range oldLines {
					oldData = append(oldData, line...)
					oldData = append(oldData, '\n')
				}
				const anchorSize = 64
				require.Equal(t, oldData[:anchorSize], data[:anchorSize], "the rewritten prefix keeps the old first anchor")
				require.Equal(t, oldData[len(oldData)-anchorSize:], data[len(oldData)-anchorSize:len(oldData)],
					"the rewritten prefix keeps the old tail anchor while changing the model elsewhere")
				if tc.larger {
					require.Greater(t, len(data), len(oldData))
				} else {
					require.Len(t, data, len(oldData), "this test exercises modification detection at an unchanged size")
				}
				oldInfo, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
				require.NoError(t, os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime()), "restored metadata must not affect rewrite detection")
			}
			launchLogTestWaitHistory(t, out, &history, func() bool {
				attempt, ok := history.attempt(key)
				return ok && !attempt.ambiguous && len(attempt.routes) == 1 && attempt.routes[0].model == "model-new"
			})
			if tc.larger {
				second, ok := history.attempt(launchAttemptKey{agent: "eng-2", attempt: 1})
				require.True(t, ok)
				require.Len(t, second.routes, 1)
				require.Equal(t, "model-second", second.routes[0].model)
			}
		})
	}
}

func TestLaunchLogCollectorTruncatesToEmptyAndRebuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), launchLogName)
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-old", "max", false, launchLogTestStart.Add(time.Second)),
	)
	out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
	var history launchLogHistory
	key := launchAttemptKey{agent: "eng-1", attempt: 1}
	launchLogTestWaitHistory(t, out, &history, func() bool {
		attempt, ok := history.attempt(key)
		return ok && len(attempt.routes) == 1 && attempt.routes[0].model == "model-old"
	})
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	launchLogTestWaitHistory(t, out, &history, func() bool { return len(history.attempts) == 0 })
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-new", "max", false, launchLogTestStart.Add(time.Second)),
	)
	launchLogTestWaitHistory(t, out, &history, func() bool {
		attempt, ok := history.attempt(key)
		return ok && !attempt.ambiguous && len(attempt.routes) == 1 && attempt.routes[0].model == "model-new"
	})
}

func TestLaunchLogCollectorOversizedLineIsBoundedAndDamagesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), launchLogName)
	launchLogTestAppend(t, path,
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-known", "max", false, launchLogTestStart.Add(time.Second)),
	)
	out, _, _ := launchLogTestStartCollector(t, path, launchLogTestRun)
	var history launchLogHistory
	key := launchAttemptKey{agent: "eng-1", attempt: 1}
	launchLogTestWaitHistory(t, out, &history, func() bool {
		attempt, ok := history.attempt(key)
		return ok && len(attempt.routes) == 1
	})

	oversized := []byte(fmt.Sprintf(`{"time":"2026-10-24T12:00:02Z","msg":"dandelion routed a firing","run":"run-launch","agent":"eng-1","account":"claude","model":"oversize-model","effort":"max","padding":%q}`, strings.Repeat("p", maxLaunchLogLineBytes+1)))
	observerTestAppendRaw(t, path, oversized[:len(oversized)/2])
	time.Sleep(40 * time.Millisecond)
	launchLogTestDrainHistory(out, &history)
	attempt, ok := history.attempt(key)
	require.True(t, ok)
	require.False(t, attempt.ambiguous, "an incomplete oversized tail is still held")
	observerTestAppendRaw(t, path, append(oversized[len(oversized)/2:], '\n'))
	launchLogTestWaitHistory(t, out, &history, func() bool {
		attempt, ok := history.attempt(key)
		return ok && attempt.ambiguous
	})
	require.LessOrEqual(t, len(history.attempts[key].routes), maxLaunchLogSessions)
	require.True(t, history.attempts[key].ambiguous, "the damaged context must be projected as unknown, not routed")

	bounded := parseLaunchLogHistory(launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart), oversized), launchLogTestRun)
	attempt, ok = bounded.attempt(key)
	require.True(t, ok)
	require.True(t, attempt.ambiguous, "the per-line cap is enforced while parsing a whole snapshot")
}

func launchLogTestContent(lines ...[]byte) []byte {
	var content []byte
	for _, line := range lines {
		content = append(content, line...)
		content = append(content, '\n')
	}
	return content
}

func TestLaunchLogAtomicSnapshotRejectsStableMetadataEOFRace(t *testing.T) {
	oldContent := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-old", "max", false, launchLogTestStart.Add(time.Second)),
	)
	newContent := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-new", "max", false, launchLogTestStart.Add(time.Second)),
	)
	require.Len(t, newContent, len(oldContent))
	a := newLaunchLogTestAtomic(t,
		launchLogTestResult(oldContent), launchLogTestResult(oldContent),
		launchLogTestResult(newContent), launchLogTestResult(oldContent),
		launchLogTestResult(newContent), launchLogTestResult(newContent),
	)
	a.refresh()
	a.refresh()
	require.Equal(t, 6, a.calls)
	attempt, ok := a.history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
	require.True(t, ok)
	require.False(t, attempt.ambiguous)
	require.Len(t, attempt.routes, 1)
	require.Equal(t, "model-new", attempt.routes[0].model, "the stale EOF-race candidate must not publish")
}

func TestLaunchLogAtomicSnapshotRewriteBudgetUnknownAndRecovery(t *testing.T) {
	for _, larger := range []bool{false, true} {
		name := "same-size and mtime-restoring rewrite"
		if larger {
			name = "larger anchor-preserving rewrite"
		}
		t.Run(name, func(t *testing.T) {
			oldContent := launchLogTestContent(
				launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
				launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-old", "max", false, launchLogTestStart.Add(time.Second)),
			)
			newLines := [][]byte{
				launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
				launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-new", "max", false, launchLogTestStart.Add(time.Second)),
			}
			if larger {
				newLines = append(newLines,
					launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
					launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-second", "max", false, launchLogTestStart.Add(3*time.Second)),
				)
			}
			newContent := launchLogTestContent(newLines...)
			const anchorSize = 64
			require.Equal(t, oldContent[:anchorSize], newContent[:anchorSize])
			require.Equal(t, oldContent[len(oldContent)-anchorSize:], newContent[len(oldContent)-anchorSize:len(oldContent)])
			if larger {
				require.Greater(t, len(newContent), len(oldContent))
			} else {
				require.Len(t, newContent, len(oldContent))
			}

			a := newLaunchLogTestAtomic(t,
				launchLogTestResult(oldContent), launchLogTestResult(oldContent),
				launchLogTestResult(newContent), launchLogTestResult(oldContent),
				launchLogTestResult(newContent), launchLogTestResult(oldContent),
				launchLogTestResult(newContent), launchLogTestResult(newContent),
			)
			a.refresh()
			a.refresh()
			require.Equal(t, 6, a.calls, "two inconsistent pairs are the fixed per-cycle budget")
			require.Empty(t, a.history.attempts, "an inconsistent file must replace stale history with unknown")
			a.refresh()
			require.Equal(t, 8, a.calls)
			first, ok := a.history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
			require.True(t, ok)
			require.False(t, first.ambiguous)
			require.Len(t, first.routes, 1)
			require.Equal(t, "model-new", first.routes[0].model)
			if larger {
				second, ok := a.history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
				require.True(t, ok)
				require.Len(t, second.routes, 1)
				require.Equal(t, "model-second", second.routes[0].model)
			}
		})
	}
}

func TestLaunchLogAtomicSnapshotContinuousAppendsDoNotDuplicate(t *testing.T) {
	first := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-one", "max", false, launchLogTestStart.Add(time.Second)),
	)
	second := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-one", "max", false, launchLogTestStart.Add(time.Second)),
		launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
	)
	third := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-one", "max", false, launchLogTestStart.Add(time.Second)),
		launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-two", "max", false, launchLogTestStart.Add(3*time.Second)),
	)
	fourth := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-one", "max", false, launchLogTestStart.Add(time.Second)),
		launchLogTestFired(t, launchLogTestRun, "eng-1", 2, "dandelion", launchLogTestStart.Add(2*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-two", "max", false, launchLogTestStart.Add(3*time.Second)),
		launchLogTestFired(t, launchLogTestRun, "eng-1", 3, "dandelion", launchLogTestStart.Add(4*time.Second)),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-three", "max", false, launchLogTestStart.Add(5*time.Second)),
	)
	a := newLaunchLogTestAtomic(t,
		launchLogTestResult(first), launchLogTestResult(second),
		launchLogTestResult(second), launchLogTestResult(third),
		launchLogTestResult(third), launchLogTestResult(fourth),
		launchLogTestResult(fourth), launchLogTestResult(fourth),
	)
	a.refresh()
	require.Len(t, a.history.attempts, 1)
	a.refresh()
	pending, ok := a.history.attempt(launchAttemptKey{agent: "eng-1", attempt: 2})
	require.True(t, ok)
	require.Empty(t, pending.routes, "a new append does not inherit the previous attempt's route")
	a.refresh()
	a.refresh()
	require.Equal(t, 8, a.calls, "each ordinary-append cycle uses exactly two reads")
	require.Len(t, a.history.attempts, 3)
	for attemptNumber, model := range map[int]string{1: "model-one", 2: "model-two", 3: "model-three"} {
		attempt, ok := a.history.attempt(launchAttemptKey{agent: "eng-1", attempt: attemptNumber})
		require.True(t, ok)
		require.False(t, attempt.ambiguous)
		require.Len(t, attempt.routes, 1)
		require.Equal(t, model, attempt.routes[0].model)
	}
}

func TestReadLaunchLogContentCapAndCancellation(t *testing.T) {
	t.Run("sparse over-cap content is bounded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), launchLogName)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test temp file
		require.NoError(t, err)
		defer func() { require.NoError(t, file.Close()) }()
		require.NoError(t, file.Truncate(maxLaunchLogBytes+1))
		result := readLaunchLogContent(context.Background(), file)
		require.Equal(t, launchLogReadUnsupported, result.status)
		require.Nil(t, result.data)
		require.LessOrEqual(t, result.readBytes, int64(maxLaunchLogBytes+launchLogReadBuffer))
	})
	t.Run("cancellation interrupts a complete read", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		reader := &launchLogTestBlockingReader{started: started, release: release}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan launchLogReadResult, 1)
		go func() { result <- readLaunchLogContent(ctx, reader) }()
		select {
		case <-started:
		case <-time.After(observerTestWait):
			t.Fatal("complete read did not start")
		}
		cancel()
		close(release)
		select {
		case got := <-result:
			require.Equal(t, launchLogReadCanceled, got.status)
		case <-time.After(time.Second):
			t.Fatal("complete read did not observe cancellation")
		}
	})
}

func TestLaunchLogAtomicOverCapNoReadsAndUnderCapRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), launchLogName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test temp file
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxLaunchLogBytes+1))
	require.NoError(t, file.Close())
	out := make(chan launchLogEvent, 8)
	collector := &launchLogCollector{path: path, runID: launchLogTestRun, out: out}
	var readCalls, readBytes atomic.Int64
	collector.read = func(ctx context.Context, path string) launchLogReadResult {
		result := readLaunchLogFile(ctx, path)
		readCalls.Add(1)
		readBytes.Add(result.readBytes)
		return result
	}
	var history launchLogHistory
	for range 3 {
		require.True(t, collector.refresh(context.Background()))
		launchLogTestDrainHistory(out, &history)
	}
	require.Equal(t, int64(3), readCalls.Load())
	require.Zero(t, readBytes.Load(), "stat-rejected over-cap polls perform no content reads")
	require.Empty(t, history.attempts)
	select {
	case event := <-out:
		t.Fatalf("unknown was published repeatedly: %#v", event)
	default:
	}

	content := launchLogTestContent(
		launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart),
		launchLogTestRoute(t, launchLogTestRun, "eng-1", "claude", "model-recovered", "max", false, launchLogTestStart.Add(time.Second)),
	)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	require.True(t, collector.refresh(context.Background()))
	launchLogTestDrainHistory(out, &history)
	require.Equal(t, int64(5), readCalls.Load())
	require.Equal(t, int64(2*len(content)), readBytes.Load())
	attempt, ok := history.attempt(launchAttemptKey{agent: "eng-1", attempt: 1})
	require.True(t, ok)
	require.Len(t, attempt.routes, 1)
	require.Equal(t, "model-recovered", attempt.routes[0].model)
}

func TestObserverOverCapLaunchLogUnknownHealthyAndNoInheritance(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun(launchLogTestRun)
	agent := domain.Agent{ID: "eng-cap", Role: domain.RoleEngineer, Cartridge: "dandelion", Model: "high",
		State: domain.AgentStateRunning, StartedAt: run.StartedAt}
	spawn := func(attempt int) events.AgentSpawned {
		return events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Duration(attempt)*time.Second)),
			Agent: agent, Task: events.TaskImplement, Attempt: attempt}
	}
	observerTestAppend(t, dir, observerTestStarted(run), spawn(1))
	launchPath := filepath.Join(dir, launchLogName)
	launchLogTestAppend(t, launchPath,
		launchLogTestRunStarted(t, run.ID, false, run.StartedAt),
		launchLogTestFired(t, run.ID, string(agent.ID), 1, "dandelion", run.StartedAt),
		launchLogTestRoute(t, run.ID, string(agent.ID), "claude", "model-before-cap", "max", false, run.StartedAt.Add(time.Second)),
	)
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll, LaunchLogInterval: observerTestPoll})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return launchLogTestFindAgent(t, snapshot, agent.ID).RouteStatus == routeStatusRouted
	})

	require.NoError(t, os.Truncate(launchPath, maxLaunchLogBytes+1))
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		projected := launchLogTestFindAgent(t, snapshot, agent.ID)
		return projected.RouteStatus == routeStatusUnknown && projected.RoutedModel == ""
	})
	require.Equal(t, "ok", o.Health().Status)

	observerTestAppend(t, dir,
		events.AgentFailed{Meta: events.MetaAt(run.ID, run.StartedAt.Add(2*time.Second)), Agent: agent.ID,
			Failure: events.FailureCrash, Attempt: 1},
		spawn(2),
	)
	snapshot := observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		projected := launchLogTestFindAgent(t, snapshot, agent.ID)
		return snapshot.Counts.Events == 4 && projected.Attempt == 2 && projected.RouteStatus == routeStatusUnknown
	})
	projected := launchLogTestFindAgent(t, snapshot, agent.ID)
	require.Empty(t, projected.RoutedModel, "an unsupported suffix cannot lend its predecessor's model to a new attempt")
	require.Zero(t, projected.RouteAttempt, "no exact new-attempt fired record exists inside the supported prefix")
	require.Equal(t, "ok", o.Health().Status)
}

func TestLaunchLogCollectorCancellationReleasesBlockedSendAndDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, launchLogName)
	line := launchLogTestFired(t, launchLogTestRun, "eng-1", 1, "dandelion", launchLogTestStart)
	launchLogTestAppend(t, path, line)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan launchLogEvent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		collectLaunchLog(ctx, path, launchLogTestRun, observerTestPoll, out)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(observerTestWait):
		t.Fatal("collector remained blocked on an unread event after cancellation")
	}
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestObserverLaunchLogCancellationIsAwaitedAndHTTPNeverReadsLogs(t *testing.T) {
	run := observerTestRun(launchLogTestRun)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour, FollowPoll: observerTestPoll})
	var calls atomic.Int32
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, cancelOnce, releaseOnce sync.Once
	releaseCollector := func() { releaseOnce.Do(func() { close(release) }) }
	o.collectLaunchLog = func(ctx context.Context, _ string, _ domain.RunID, _ time.Duration, _ chan<- launchLogEvent) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		cancelOnce.Do(func() { close(cancelObserved) })
		<-release
	}
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		releaseCollector()
		select {
		case <-done:
		case <-time.After(observerTestWait):
			t.Error("observer did not stop after releasing launch-log collection")
		}
	})
	select {
	case <-started:
	case <-time.After(observerTestWait):
		t.Fatal("launch-log collection did not start")
	}
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/api/snapshot").Code)
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/health").Code)
	}
	require.Equal(t, int32(1), calls.Load(), "HTTP requests must not read or replay sc.log")
	require.Equal(t, "ok", o.Health().Status)

	cancel()
	select {
	case <-cancelObserved:
	case <-time.After(observerTestWait):
		t.Fatal("launch-log collector did not receive cancellation")
	}
	select {
	case <-done:
		t.Fatal("observer returned before launch-log collection finished")
	case <-time.After(20 * time.Millisecond):
	}
	releaseCollector()
	select {
	case <-done:
	case <-time.After(observerTestWait):
		t.Fatal("observer did not finish after launch-log collection returned")
	}
	require.Equal(t, "ok", o.Health().Status)
}
