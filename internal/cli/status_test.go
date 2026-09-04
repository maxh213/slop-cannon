package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

// Golden tests for `sc status` (P6-08) and the bead timeline (P6-15). The
// run reported on is a recorded events file plus a state.json written into
// a temp run directory, so nothing here needs bd, git or a live run. Every
// helper is prefixed "status" so it cannot collide with another task's file
// in this package.

// statusEpoch is the fixed time the recorded run starts at.
var statusEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// statusRunID is the recorded run's id.
const statusRunID = "20260903-120000-abcd"

// statusEnv isolates HOME and the XDG directories under a temp base and
// creates a fake git repository in it.
func statusEnv(t *testing.T) (base, repo string) {
	t.Helper()
	base = t.TempDir()
	home := filepath.Join(base, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	require.NoError(t, os.Unsetenv("NO_COLOR"))
	repo = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	return base, repo
}

// statusAt stamps an event n seconds into the recorded run.
func statusAt(n int) events.Meta {
	return events.MetaAt(statusRunID, statusEpoch.Add(time.Duration(n)*time.Second))
}

// statusSnapshot is the frozen configuration of the recorded run.
func statusSnapshot() domain.ConfigSnapshot {
	return domain.ConfigSnapshot{Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable"}
}

// statusDomainRun is the recorded run's domain record.
func statusDomainRun(repo, dir string) domain.Run {
	return domain.Run{
		ID: statusRunID, RepoPath: repo, StateDir: dir, Mode: domain.RunModeBrief,
		Brief: "add retries to the client", Epic: "sc-0",
		StartedAt: statusEpoch, Config: statusSnapshot(),
	}
}

// statusEvents is the recorded run: a plan, two beads, one delivered and
// merged behind a PR, one still under review by a live tester, plus a
// warning and a line of agent output.
func statusEvents(repo, dir string) []events.Event {
	po := domain.Agent{ID: "po-plan-1", Role: domain.RolePO, Cartridge: "claude", Model: "fable",
		State: domain.AgentStateRunning, StartedAt: statusEpoch.Add(time.Second), PID: 4241}
	eng := domain.Agent{ID: "eng-sc-1-r1", Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable",
		Bead: "sc-1", State: domain.AgentStateRunning, StartedAt: statusEpoch.Add(20 * time.Second), PID: 4242}
	tester := domain.Agent{ID: "tester-qa-sc-2-r1", Role: domain.RoleTester, Lens: domain.LensQA,
		Cartridge: "claude", Model: "fable", Bead: "sc-2", State: domain.AgentStateRunning,
		StartedAt: statusEpoch.Add(60 * time.Second), PID: 4243}
	return []events.Event{
		events.RunStarted{Meta: statusAt(0), Info: statusDomainRun(repo, dir)},
		events.AgentSpawned{Meta: statusAt(1), Agent: po, Task: events.TaskPlan},
		events.AgentOutput{Meta: statusAt(2), Agent: po.ID, Stream: events.StreamStdout, Line: "reading the repository"},
		events.PlanProduced{Meta: statusAt(9), Agent: po.ID, Epic: "sc-0",
			Created: map[string]domain.BeadID{"retry": "sc-1", "docs": "sc-2"}},
		events.AgentFinished{Meta: statusAt(10), Agent: po.ID, Role: domain.RolePO,
			Duration: 9 * time.Second, Produced: domain.MessagePlan},
		events.BeadCreated{Meta: statusAt(11), Bead: domain.Bead{ID: "sc-1", Title: "Add retry", State: domain.BeadStateOpen, Priority: domain.PriorityDefault}},
		events.BeadCreated{Meta: statusAt(12), Bead: domain.Bead{ID: "sc-2", Title: "Document retries", State: domain.BeadStateOpen, Priority: domain.PriorityDefault}},
		events.AgentSpawned{Meta: statusAt(20), Agent: eng, Task: events.TaskImplement, BeadTitle: "Add retry", Round: 1},
		events.BeadStateChanged{Meta: statusAt(21), Bead: "sc-1", From: domain.BeadStateOpen, To: domain.BeadStateInProgress, Round: 1},
		events.AgentFinished{Meta: statusAt(40), Agent: eng.ID, Role: domain.RoleEngineer, Bead: "sc-1",
			Duration: 20 * time.Second, Produced: domain.MessageImplReport},
		events.RoundStarted{Meta: statusAt(41), Bead: "sc-1", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.BeadStateChanged{Meta: statusAt(42), Bead: "sc-1", From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1},
		events.BeadStateChanged{Meta: statusAt(50), Bead: "sc-1", From: domain.BeadStateInReview, To: domain.BeadStateApproved, Round: 1, Reason: "all testers approved"},
		events.PROpened{Meta: statusAt(51), Bead: "sc-1", URL: "https://example.test/pr/7", Number: 7, Branch: "sc/sc-1-add-retry", Base: "main"},
		events.BeadStateChanged{Meta: statusAt(52), Bead: "sc-1", From: domain.BeadStateApproved, To: domain.BeadStateDelivering, Round: 1},
		events.Merged{Meta: statusAt(55), Bead: "sc-1", SHA: "0123456789abcdef", Branch: "main", PRNumber: 7, Pushed: true},
		events.BeadStateChanged{Meta: statusAt(56), Bead: "sc-1", From: domain.BeadStateDelivering, To: domain.BeadStateClosed, Round: 1},
		events.Warning{Meta: statusAt(58), Source: "gates", Message: "gh is unavailable; gate polling skipped"},
		events.AgentSpawned{Meta: statusAt(60), Agent: tester, Task: events.TaskReview, BeadTitle: "Document retries", Round: 1},
		events.BeadStateChanged{Meta: statusAt(61), Bead: "sc-2", From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1},
	}
}

// statusRecord writes the recorded run into the repository's run directory
// and returns that directory. state is written only when writeState is set,
// so the "crashed before the first transition" path is testable.
func statusRecord(t *testing.T, repo string, writeState bool, extra ...events.Event) string {
	t.Helper()
	dir := config.RunDir(repo, statusRunID)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)

	evs := append(statusEvents(repo, dir), extra...)
	log, err := orchestrator.OpenEventLog(dir, nil)
	require.NoError(t, err)
	for _, ev := range evs {
		require.NoError(t, log.Write(ev))
	}
	require.NoError(t, log.Close())

	if writeState {
		st := orchestrator.State{
			Version:       orchestrator.StateFileVersion,
			Run:           statusDomainRun(repo, dir),
			Phase:         orchestrator.PhaseRunning,
			UpdatedAt:     statusEpoch.Add(61 * time.Second),
			LastSeq:       uint64(len(evs)),
			LastEventKind: evs[len(evs)-1].Kind(),
			LastEventAt:   evs[len(evs)-1].At(),
		}
		require.NoError(t, orchestrator.WriteState(dir, st))
	}
	return dir
}

// statusNormalise replaces the run state directory with <state> and the
// temp base with <tmp> so goldens are stable: the state directory embeds a
// hash of the repository path, which changes on every run.
func statusNormalise(out, base, repo string) string {
	out = strings.ReplaceAll(out, config.StateDir(repo), "<state>")
	return strings.ReplaceAll(out, base, "<tmp>")
}

func TestStatusHelpGolden(t *testing.T) {
	out, err := rootRun(t, "status", "--help")
	require.NoError(t, err)
	rootGolden(t, "status-help", out)
}

func TestStatusRunningGolden(t *testing.T) {
	base, repo := statusEnv(t)
	statusRecord(t, repo, true)

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	rootGolden(t, "status-running", statusNormalise(out, base, repo))
}

func TestStatusFinishedGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true,
		events.BeadStateChanged{Meta: statusAt(70), Bead: "sc-2", From: domain.BeadStateInReview, To: domain.BeadStateClosed, Round: 1},
		events.AgentFinished{Meta: statusAt(71), Agent: "tester-qa-sc-2-r1", Role: domain.RoleTester, Bead: "sc-2", Duration: 11 * time.Second, Produced: domain.MessageVerdict},
		events.RunStopped{Meta: statusAt(72), Reason: "SIGINT"},
		events.RunFinished{Meta: statusAt(73), Reason: events.FinishCompleted,
			Summary: "2 beads landed; kept 1 worktree", Duration: 73 * time.Second},
	)
	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	st.Phase = orchestrator.PhaseFinished
	st.StopReason = "SIGINT"
	st.Finish = events.FinishCompleted
	st.FinishSummary = "2 beads landed; kept 1 worktree"
	st.FinishedAt = statusEpoch.Add(73 * time.Second)
	st.Resumes = 1
	require.NoError(t, orchestrator.WriteState(dir, st))

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	rootGolden(t, "status-finished", statusNormalise(out, base, repo))
}

func TestStatusNoStateFileGolden(t *testing.T) {
	base, repo := statusEnv(t)
	statusRecord(t, repo, false)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "3")
	require.NoError(t, err)
	rootGolden(t, "status-no-state", statusNormalise(out, base, repo))
}

func TestStatusNoRunsGolden(t *testing.T) {
	base, repo := statusEnv(t)
	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	rootGolden(t, "status-no-runs", statusNormalise(out, base, repo))
}

func TestStatusJSON(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)

	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.True(t, rep.Found)
	require.Equal(t, domain.RunID(statusRunID), rep.Run)
	require.Equal(t, dir, rep.Dir)
	require.Equal(t, repo, rep.Repo)
	require.NotNil(t, rep.State)
	require.Equal(t, "add retries to the client", rep.State.Run.Brief)
	require.NotNil(t, rep.Summary)
	require.Equal(t, map[domain.BeadState]int{
		domain.BeadStateClosed: 1, domain.BeadStateInReview: 1,
	}, rep.Summary.Counts)
	require.Len(t, rep.Summary.Agents, 1)
	require.Len(t, rep.Recent, orchestrator.DefaultRecentEvents)
	require.Empty(t, rep.Notes)
	require.Equal(t, orchestrator.PhaseRunning, rep.Phase())
}

func TestStatusEventsFlag(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "events     0 of 20")
	require.NotContains(t, out, "run started")

	out, err = rootRun(t, "--repo", repo, "status", "--events", "2")
	require.NoError(t, err)
	require.Contains(t, out, "events     2 of 20")
}

func TestStatusSelectsTheLatestRunAndNamedRuns(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	// An older run must not win, an unrelated newer one must.
	older := config.RunDir(repo, "20260101-000000-0001")
	_, err := orchestrator.EnsureRunDir(older)
	require.NoError(t, err)

	rep, err := RunStatus(repo, "", 0)
	require.NoError(t, err)
	require.Equal(t, domain.RunID(statusRunID), rep.Run)

	rep, err = RunStatus(repo, "20260101-000000-0001", 0)
	require.NoError(t, err)
	require.True(t, rep.Found)
	require.Nil(t, rep.State)
	require.Nil(t, rep.Summary)
	require.Len(t, rep.Notes, 2, "one note for the missing state file, one for the missing run log")

	_, err = RunStatus(repo, "20991231-235959-ffff", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no run directory")
}

func TestStatusRejectsANonRepository(t *testing.T) {
	base, _ := statusEnv(t)
	plain := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	_, err := rootRun(t, "--repo", plain, "status")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a git repository")
}

func TestStatusRejectsTwoArguments(t *testing.T) {
	_, repo := statusEnv(t)
	_, err := rootRun(t, "--repo", repo, "status", "sc-1", "sc-2")
	require.Error(t, err)
}

// statusFakeBeads is a statusBeadReader that answers from a fixture.
type statusFakeBeads struct {
	issues []beads.Issue
	err    error
	asked  []domain.BeadID
}

func (f *statusFakeBeads) Show(_ context.Context, ids ...domain.BeadID) ([]beads.Issue, error) {
	f.asked = append(f.asked, ids...)
	return f.issues, f.err
}

// statusInstallBeads points `sc status <bead-id>` at a fake reader.
func statusInstallBeads(t *testing.T, repo string, fake *statusFakeBeads) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".beads"), 0o755))
	saved := statusBeads
	t.Cleanup(func() { statusBeads = saved })
	statusBeads = func(string) (statusBeadReader, error) { return fake, nil }
}

// statusIssue is the bead `sc status <bead-id>` reports on: created,
// claimed, reviewed and closed, with the structured messages sc records.
func statusIssue(t *testing.T) beads.Issue {
	t.Helper()
	verdict := domain.Verdict{
		Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges,
		Summary: "the retry budget is untested",
		Items: []domain.FeedbackItem{{
			ID: "qa-r1-1", Severity: domain.SeverityMajor, Category: "tests",
			Title: "No test for the retry budget", File: "retry.go", Line: 42,
			Status: domain.ItemOpen,
		}},
	}
	body, err := beads.EncodeMessage(verdict)
	require.NoError(t, err)
	return beads.Issue{
		ID: "sc-1", Title: "Add retry", Status: string(domain.BeadStateClosed),
		Priority: domain.PriorityDefault, Type: string(domain.BeadTypeTask),
		CreatedBy: "sc:orchestrator", Assignee: "sc:eng-sc-1-r1",
		CreatedAt: statusEpoch, StartedAt: statusEpoch.Add(20 * time.Second),
		ClosedAt: statusEpoch.Add(56 * time.Second), CloseReason: "merged as 0123456",
		Metadata: beads.Metadata{"sc_run": statusRunID, "sc_round": float64(1)},
		Comments: []beads.Comment{{
			ID: "c1", IssueID: "sc-1", Author: "sc:tester-qa-sc-1-r1",
			CreatedAt: statusEpoch.Add(45 * time.Second),
			Text:      body,
		}},
	}
}

func TestStatusBeadTimelineGolden(t *testing.T) {
	base, repo := statusEnv(t)
	statusInstallBeads(t, repo, &statusFakeBeads{issues: []beads.Issue{statusIssue(t)}})

	out, err := rootRun(t, "--repo", repo, "status", "sc-1")
	require.NoError(t, err)
	rootGolden(t, "status-bead", statusNormalise(out, base, repo))
}

func TestStatusBeadTimelineMatchesFormatTimeline(t *testing.T) {
	_, repo := statusEnv(t)
	issue := statusIssue(t)
	fake := &statusFakeBeads{issues: []beads.Issue{issue}}
	statusInstallBeads(t, repo, fake)

	out, err := rootRun(t, "--repo", repo, "status", "sc-1")
	require.NoError(t, err)
	require.Equal(t, beads.FormatTimeline(issue, beads.ParseMessages(issue.Comments)), out,
		"P6-15: the timeline is beads.FormatTimeline, not a second derivation")
	require.Equal(t, []domain.BeadID{"sc-1"}, fake.asked)
}

func TestStatusBeadJSON(t *testing.T) {
	_, repo := statusEnv(t)
	issue := statusIssue(t)
	statusInstallBeads(t, repo, &statusFakeBeads{issues: []beads.Issue{issue}})

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status", "sc-1")
	require.Equal(t, 0, code, stderr)
	var rep StatusBeadReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.Equal(t, repo, rep.Repo)
	require.Equal(t, domain.BeadID("sc-1"), rep.Bead.ID)
	require.Len(t, rep.Timeline, len(beads.Timeline(issue, beads.ParseMessages(issue.Comments))))
	require.Equal(t, beads.EventCreated, rep.Timeline[0].Event)
	require.Equal(t, beads.EventClosed, rep.Timeline[len(rep.Timeline)-1].Event)
}

func TestStatusBeadErrors(t *testing.T) {
	_, repo := statusEnv(t)

	// No .beads directory at all.
	_, err := rootRun(t, "--repo", repo, "status", "sc-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no beads database yet")

	// bd answers with nothing.
	statusInstallBeads(t, repo, &statusFakeBeads{})
	_, err = rootRun(t, "--repo", repo, "status", "sc-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// bd fails.
	boom := errors.New("bd exploded")
	statusInstallBeads(t, repo, &statusFakeBeads{err: boom})
	_, err = rootRun(t, "--repo", repo, "status", "sc-1")
	require.ErrorIs(t, err, boom)

	// An empty id is refused before bd is consulted.
	_, err = rootRun(t, "--repo", repo, "status", "  ")
	require.Error(t, err)
}
