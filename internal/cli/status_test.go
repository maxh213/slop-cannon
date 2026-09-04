package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// statusPlan is the plan the recorded run's PO produced: three beads, of
// which the scheduler has reached two. It is what a real run publishes —
// nothing in production emits BeadCreated or GraphChanged (P6-32) — so it
// is what tells `sc status` about the bead nothing has touched yet.
func statusPlan() domain.Plan {
	return domain.Plan{
		Brief: "add retries to the client",
		Beads: []domain.PlanBead{
			{Key: "retry", Title: "Add retry", Acceptance: "retries are bounded", Priority: domain.PriorityDefault},
			{Key: "docs", Title: "Document retries", Acceptance: "the README says so", Priority: domain.PriorityDefault},
			{Key: "retire", Title: "Retire the old client", Acceptance: "no caller is left", Priority: domain.PriorityDefault},
		},
	}
}

// statusEvents is the recorded run: a three-bead plan, one bead delivered
// and merged behind a PR, one still under review by a live tester, one the
// run has not started, plus a warning and a line of agent output.
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
		events.PlanProduced{Meta: statusAt(9), Agent: po.ID, Epic: "sc-0", Plan: statusPlan(),
			Created: map[string]domain.BeadID{"retry": "sc-1", "docs": "sc-2", "retire": "sc-3"}},
		events.AgentFinished{Meta: statusAt(10), Agent: po.ID, Role: domain.RolePO,
			Duration: 9 * time.Second, Produced: domain.MessagePlan},
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
		// A live run holds the repository's run lock; `sc status` asks it
		// whether the phase in state.json is still true.
		statusLock(t, dir, os.Getpid())
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

// statusLock writes the repository's run lock as if pid were running the
// recorded run. The lock lives outside the run directory (D-23), which is
// why it is written through orchestrator.LockPath rather than by hand.
func statusLock(t *testing.T, runDir string, pid int) {
	t.Helper()
	path := orchestrator.LockPath(runDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	data, err := json.Marshal(orchestrator.LockHolder{
		Run: statusRunID, PID: pid, TakenAt: statusEpoch,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// statusUnlock drops the run lock, as the end of a run does.
func statusUnlock(t *testing.T, runDir string) {
	t.Helper()
	require.NoError(t, os.Remove(orchestrator.LockPath(runDir)))
}

// statusNormalise replaces the run state directory with <state>, the temp
// base with <tmp> and this process's id with <pid> so goldens are stable:
// the state directory embeds a hash of the repository path and the run lock
// names whichever process the test runs in.
func statusNormalise(out, base, repo string) string {
	out = strings.ReplaceAll(out, config.StateDir(repo), "<state>")
	out = strings.ReplaceAll(out, base, "<tmp>")
	// Only the lock holder's pid, never a fixture agent's: the test process
	// could in principle be pid 4243 itself.
	return strings.ReplaceAll(out, "pid "+strconv.Itoa(os.Getpid())+" holds", "pid <pid> holds")
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

// statusFinishSummary is the shape a real run records: gitx's worktree
// cleanup report, which names every worktree it kept on a line of its own
// (internal/gitx.CleanupResult.Summary). It is multi-line on purpose — that
// is what the header block has to print without running the lines together.
const statusFinishSummary = "removed 1 worktree, kept 1:\n  sc-2 (in_progress)"

func TestStatusFinishedGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true,
		events.BeadStateChanged{Meta: statusAt(70), Bead: "sc-2", From: domain.BeadStateInReview, To: domain.BeadStateClosed, Round: 1},
		events.AgentFinished{Meta: statusAt(71), Agent: "tester-qa-sc-2-r1", Role: domain.RoleTester, Bead: "sc-2", Duration: 11 * time.Second, Produced: domain.MessageVerdict},
		events.RunStopped{Meta: statusAt(72), Reason: "SIGINT"},
		events.RunFinished{Meta: statusAt(73), Reason: events.FinishCompleted,
			Summary: statusFinishSummary, Duration: 73 * time.Second},
	)
	statusUnlock(t, dir) // a run that ended released the lock
	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	st.Phase = orchestrator.PhaseFinished
	st.StopReason = "SIGINT"
	st.Finish = events.FinishCompleted
	st.FinishSummary = statusFinishSummary
	st.FinishedAt = statusEpoch.Add(73 * time.Second)
	st.Resumes = 1
	require.NoError(t, orchestrator.WriteState(dir, st))

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	rootGolden(t, "status-finished", statusNormalise(out, base, repo))
}

// TestStatusHeaderKeepsAMultiLineValueOnItsOwnLines pins the fix for a
// header row that ran two sentences together: the finish summary a run
// records is legitimately several lines, and dropping the newline as a
// control character printed "kept 1:  sc-2" as one word-jam. Every line of
// a value is now its own row, indented under the label.
func TestStatusHeaderKeepsAMultiLineValueOnItsOwnLines(t *testing.T) {
	var b strings.Builder
	statusHeader(&b, &StatusReport{
		Run: statusRunID,
		State: &orchestrator.State{
			Phase:         orchestrator.PhaseFinished,
			Finish:        events.FinishCompleted,
			FinishSummary: "2 beads (2 closed)\nremoved 2 worktrees",
			Run:           domain.Run{ID: statusRunID, Brief: "first line\nsecond line"},
		},
	})
	lines := strings.Split(b.String(), "\n")
	require.Contains(t, lines, "outcome    2 beads (2 closed)")
	require.Contains(t, lines, "           removed 2 worktrees")
	require.Contains(t, lines, "brief      first line")
	require.Contains(t, lines, "           second line")
	require.NotContains(t, b.String(), "(2 closed)removed", "the two lines must not run together")

	// The error half still joins with "; " on the summary's last line, and
	// a value that is blank once its control characters are gone prints no
	// row at all.
	b.Reset()
	statusHeader(&b, &StatusReport{
		Run: statusRunID,
		State: &orchestrator.State{
			Phase:         orchestrator.PhaseFinished,
			FinishSummary: "removed 1 worktree\n\n",
			FinishError:   "bd was unreachable",
			StopReason:    "\x1b[31m",
			Run:           domain.Run{ID: statusRunID},
		},
	})
	require.Contains(t, b.String(), "outcome    removed 1 worktree; error: bd was unreachable\n")
	require.NotContains(t, b.String(), "stopped by")
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
		domain.BeadStateClosed: 1, domain.BeadStateInReview: 1, domain.BeadStateOpen: 1,
	}, rep.Summary.Counts, "the planned bead nothing has claimed is counted too")
	require.True(t, rep.Running(), "the lock is held by this very process")
	require.False(t, rep.Crashed())
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
	require.Contains(t, out, "events     0 of 18")
	require.NotContains(t, out, "run started")

	out, err = rootRun(t, "--repo", repo, "status", "--events", "2")
	require.NoError(t, err)
	require.Contains(t, out, "events     2 of 18")
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
	require.Len(t, rep.Notes, 3,
		"the run lock (held by the newer run), the missing state file and the missing run log")
	require.Contains(t, rep.Notes[0], "the run lock is held by run "+statusRunID)

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

// TestStatusCrashedRunGolden is the report a user gets after the cannon was
// killed: state.json still says `running` and the run log still ends on a
// live tester, but nothing holds the run lock any more, so neither is true.
func TestStatusCrashedRunGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	statusUnlock(t, dir) // SIGKILL leaves the state file behind, not the lock

	out, err := rootRun(t, "--repo", repo, "status", "--events", "3")
	require.NoError(t, err)
	require.Contains(t, out, "phase      running (no live process: the run was killed or crashed)")
	require.Contains(t, out, "agents     1 last seen of 10 ammo")
	rootGolden(t, "status-crashed", statusNormalise(out, base, repo))
}

// TestStatusReportsWhoIsRunningIt covers the three answers the run lock can
// give: this run, another run, and nobody.
func TestStatusReportsWhoIsRunningIt(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)

	rep, err := RunStatus(repo, "", 0)
	require.NoError(t, err)
	require.True(t, rep.Running())
	require.False(t, rep.Crashed())
	require.Equal(t, os.Getpid(), rep.Liveness.Holder.PID)
	require.Empty(t, rep.Notes)

	// A later run of the same repository took the lock: this one is over.
	path := orchestrator.LockPath(dir)
	data, err := json.Marshal(orchestrator.LockHolder{Run: "20260904-000000-ffff", PID: os.Getpid(), TakenAt: statusEpoch})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	rep, err = RunStatus(repo, statusRunID, 0)
	require.NoError(t, err)
	require.False(t, rep.Running())
	require.True(t, rep.Crashed())
	require.Contains(t, rep.Notes, "the run lock is held by run 20260904-000000-ffff (pid "+strconv.Itoa(os.Getpid())+")")

	// The lock is this run's, but the process that took it is gone: what a
	// SIGKILL leaves behind.
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, dead.Run())
	statusLock(t, dir, dead.Process.Pid)
	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, fmt.Sprintf("process    none; pid %d took the run lock and is gone", dead.Process.Pid))
	require.Contains(t, out, "phase      running (no live process: the run was killed or crashed)")

	// Nobody at all.
	require.NoError(t, os.Remove(path))
	rep, err = RunStatus(repo, statusRunID, 0)
	require.NoError(t, err)
	require.True(t, rep.Crashed())
	require.False(t, rep.Liveness.Held)
	out, err = rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "process    none; nothing holds the run lock")
}

// TestStatusAgentsFallBackToTheStateFile: a run directory whose log is gone
// still knows which agents were live, because state.json records them.
func TestStatusAgentsComeFromTheStateFileWithoutARunLog(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	require.NoError(t, os.Remove(orchestrator.EventLogPath(dir)))
	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	st.Agents = []domain.Agent{{ID: "tester-qa-sc-2-r1", Role: domain.RoleTester, Lens: domain.LensQA,
		Bead: "sc-2", State: domain.AgentStateRunning, PID: 4243}}
	require.NoError(t, orchestrator.WriteState(dir, st))

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.Contains(t, out, "agents     1 live of 10 ammo")
	require.Contains(t, out, "tester-qa-sc-2-r1  tester/qa  sc-2  running  pid 4243")
	require.Contains(t, out, "note: no events.jsonl")
}

// TestStatusSurvivesARecordWithoutABeadID is the regression for the crash
// `sc status` used to take on a run log line that parses as an event but
// names no bead: the reviewer's exact line, through the real command.
func TestStatusSurvivesARecordWithoutABeadID(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	f, err := os.OpenFile(orchestrator.EventLogPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"bead_state_changed","run_id":"20260903-120000-abcd","at":"2026-09-03T12:00:00Z","to":"in_review"}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.Contains(t, out, "beads      3", "the damaged record costs nothing but itself")
	require.Contains(t, out, "note: 1 record in the run log names no bead and was skipped (bead_state_changed)")
}

// TestStatusScrubsUntrustedEventText: a run log quotes what a cartridge and
// bd printed, and `sc status` writes it to a terminal. No escape, carriage
// return or other control character may reach it (P7-18 fixes the renderer
// itself; this is the guarantee at the printing end).
func TestStatusScrubsUntrustedEventText(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true,
		events.Warning{Meta: statusAt(62), Source: "cartridge",
			Message: "\x1b[1;41m spoofed \x1b[0m\rERASED\x07\x1b]0;title\x07 and\ttabbed"},
		events.Error{Meta: statusAt(63), Source: "bd", Message: "\x1b[2J", Err: "\x9b31m"},
	)

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.NotContains(t, out, "\x1b")
	require.NotContains(t, out, "\x07")
	require.NotContains(t, out, "\x9b")
	require.NotContains(t, out, "\r")
	require.Contains(t, out, " spoofed ERASED and tabbed", "the text itself survives")
	for _, line := range strings.Split(out, "\n") {
		require.False(t, strings.ContainsFunc(line, func(r rune) bool {
			return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		}), "control character in %q", line)
	}
}

// TestStatusScrubsTheAgentsAndBeadsSections: the two sections that print
// ids, roles and state names borrowed from bd and from a plan a model
// wrote. They are as untrusted as an event message, and the reviewer's
// repro plants escapes in exactly those fields.
func TestStatusScrubsTheAgentsAndBeadsSections(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true,
		events.AgentSpawned{Meta: statusAt(62), Task: events.TaskImplement, Round: 1,
			Agent: domain.Agent{
				ID: "eng-\x1b[31mRED\x1b[0m-1", Role: domain.RoleEngineer,
				Bead: "sc-\x1b[1;41mX\x1b[0m", State: domain.AgentStateRunning, PID: 42,
			}},
		events.BeadStateChanged{Meta: statusAt(63), Bead: "sc-\x1b[1;41mX\x1b[0m",
			From: domain.BeadStateOpen, To: domain.BeadStateInReview},
	)

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.Contains(t, out, "eng-RED-1", "the agent id is printed, without its escapes")
	require.Contains(t, out, "sc-X", "so is the bead id")
	require.Contains(t, out, "pid 42")
	// A state name is a closed set (domain.BeadState refuses to marshal
	// anything else), so scrubbing it is belt and braces; the ids are not.
	require.Contains(t, out, "in_review")
	for _, line := range strings.Split(out, "\n") {
		require.False(t, strings.ContainsFunc(line, func(r rune) bool {
			return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		}), "control character in %q", line)
	}
}

// TestStatusScrubsTheBeadTimeline: `sc status <bead-id>` prints a bead
// title a product owner invented and the headline of every message an
// agent wrote. None of it may repaint the terminal, and the report's line
// structure survives the scrubbing.
func TestStatusScrubsTheBeadTimeline(t *testing.T) {
	_, repo := statusEnv(t)
	issue := statusIssue(t)
	issue.Title = "Add \x1b[2Jretry"
	issue.Assignee = "sc:eng-\x1b[31mRED\x1b[0m"
	report := domain.ImplReport{
		Summary: "\x1b[1;41m spoofed \x1b[0m\rERASED\x07 and\tdone",
	}
	body, err := beads.EncodeMessage(report)
	require.NoError(t, err)
	issue.Comments = append(issue.Comments, beads.Comment{
		ID: "c2", IssueID: "sc-1", Author: "sc:eng-sc-1-r1",
		CreatedAt: statusEpoch.Add(40 * time.Second), Text: body,
	})
	statusInstallBeads(t, repo, &statusFakeBeads{issues: []beads.Issue{issue}})

	out, err := rootRun(t, "--repo", repo, "status", "sc-1")
	require.NoError(t, err)
	require.Contains(t, out, "Add retry", "the title survives, its escape does not")
	require.Contains(t, out, " spoofed ERASED and done")
	raw := beads.FormatTimeline(issue, beads.ParseMessages(issue.Comments))
	require.Equal(t, strings.Count(raw, "\n"), strings.Count(out, "\n"),
		"scrubbing is per line: the timeline keeps its shape")
	for _, line := range strings.Split(out, "\n") {
		require.False(t, strings.ContainsFunc(line, func(r rune) bool {
			return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		}), "control character in %q", line)
	}
}

// TestStatusReadsPastADamagedRunLogLine is the command-level regression for
// a fold that stopped at the first unreadable line: a run log torn by a
// short write (the run then carries on appending, D-15) used to make
// `sc status` describe the run as it stood at the tear. It must describe
// the run as it stands now, and say the line was skipped.
func TestStatusReadsPastADamagedRunLogLine(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	path := orchestrator.EventLogPath(dir)
	whole, err := os.ReadFile(path) //nolint:gosec // a temp run directory
	require.NoError(t, err)
	lines := strings.SplitAfter(string(whole), "\n")
	require.Greater(t, len(lines), 6)
	// Tear the third line in half, exactly as a short write would.
	lines[2] = lines[2][:len(lines[2])/2]
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600))

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.Contains(t, out, "closed", "the states the log records after the damage still count")
	require.Contains(t, out, "in_review")
	require.Contains(t, out, "note: 1 line of the run log did not parse and was skipped")
	require.NotContains(t, out, "events     0 of")
}

// TestStatusScrubBlock covers the multi-line scrubber: every line is
// scrubbed, the newlines between them are not.
func TestStatusScrubBlock(t *testing.T) {
	require.Equal(t, "a\nb\n", statusScrubBlock("a\nb\n"), "clean text is returned unchanged")
	require.Equal(t, "sc-1  title\n  red\n", statusScrubBlock("sc-1  \x1b[2Jtitle\n  \x1b[31mred\x1b[0m\n"))
	require.Equal(t, "\n\n", statusScrubBlock("\r\n\x07\n"))
}

// TestStatusScrub covers the scrubber directly, including the sequences a
// line of cartridge output is most likely to carry.
func TestStatusScrub(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "nothing to scrub", "nothing to scrub"},
		{"csi", "\x1b[1;41mred\x1b[0m", "red"},
		{"osc bel", "\x1b]0;window title\x07kept", "kept"},
		{"osc st", "\x1b]8;;https://example.test\x1b\\kept", "kept"},
		{"lone escape", "a\x1bZb", "ab"},
		{"trailing escape", "a\x1b", "a"},
		{"unterminated csi", "a\x1b[1;2", "a"},
		{"carriage return", "first\rsecond", "firstsecond"},
		{"tab", "a\tb", "a b"},
		{"c1", "a\u009bb", "ab"},
		{"del", "a\u007fb", "ab"},
		{"unicode kept", "héllo → ✔", "héllo → ✔"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, statusScrub(tc.in))
		})
	}
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
