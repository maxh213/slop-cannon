package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
)

// Tests for `sc logs` (P8-05). The fixture is a run directory with a
// handful of log files in it: the command reads files and nothing else, so
// nothing here needs bd, git, an orchestrator or a live run. Every helper
// is prefixed "logs" so it cannot collide with another task's file in this
// package; the isolated home comes from statusEnv (status_test.go), which
// is the same shape every command test needs.

// logsRunID is the recorded run these tests read.
const logsRunID = "20260903-140000-abcd"

// logsWrite* fixtures ---------------------------------------------------

// logsFixture creates a run directory with a run log and three agent logs:
// two agents for bead sc-1 and one for sc-12, which is what proves a bead
// id selects on whole segments rather than on a substring.
func logsFixture(t *testing.T, repo, runID string) string {
	t.Helper()
	dir := config.RunDir(repo, runID)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)
	logsWriteLog(t, logging.PathsFor(dir).Log, logsLines("sc", 60))
	logsWriteAgent(t, dir, "eng-sc-1-r1", logsLines("eng", 3))
	logsWriteAgent(t, dir, "tester-qa-sc-1-r1", logsLines("qa", 2))
	logsWriteAgent(t, dir, "eng-sc-12-r1", logsLines("other", 2))
	return dir
}

// logsLines makes n numbered lines with the given prefix.
func logsLines(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s line %d", prefix, i+1)
	}
	return out
}

// logsWriteLog writes lines to path, newline-terminated as a logger does.
func logsWriteLog(t *testing.T, path string, lines []string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// logsWriteAgent writes one agent's log inside the run directory.
func logsWriteAgent(t *testing.T, runDir, agent string, lines []string) string {
	t.Helper()
	path := logging.PathsFor(runDir).AgentLog(domain.AgentID(agent))
	logsWriteLog(t, path, lines)
	return path
}

// logsAppend adds a line to an existing log, as a running agent does.
func logsAppend(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a temp run directory
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// The command ----------------------------------------------------------

func TestLogsHelpGolden(t *testing.T) {
	out, err := rootRun(t, "logs", "--help")
	require.NoError(t, err)
	rootGolden(t, "logs-help", out)
}

// TestLogsTailsTheRunLog: with no argument the command tails sc.log, and
// --lines chooses how much of it.
func TestLogsTailsTheRunLog(t *testing.T) {
	_, repo := statusEnv(t)
	logsFixture(t, repo, logsRunID)

	out, err := rootRun(t, "--repo", repo, "logs")
	require.NoError(t, err)
	lines := logsOutputLines(out)
	require.Len(t, lines, DefaultLogLines, "the default is the last screenful")
	require.Equal(t, "sc line 11", lines[0])
	require.Equal(t, "sc line 60", lines[len(lines)-1])
	require.NotContains(t, out, "==>", "one log needs no header")

	out, err = rootRun(t, "--repo", repo, "logs", "-n", "2")
	require.NoError(t, err)
	require.Equal(t, []string{"sc line 59", "sc line 60"}, logsOutputLines(out))

	out, err = rootRun(t, "--repo", repo, "logs", "--lines", "0")
	require.NoError(t, err)
	require.Len(t, logsOutputLines(out), 60, "--lines 0 is the whole file")
}

// TestLogsTailsOneAgent: an exact agent id selects exactly that log.
func TestLogsTailsOneAgent(t *testing.T) {
	_, repo := statusEnv(t)
	logsFixture(t, repo, logsRunID)

	out, err := rootRun(t, "--repo", repo, "logs", "eng-sc-1-r1")
	require.NoError(t, err)
	require.Equal(t, []string{"eng line 1", "eng line 2", "eng line 3"}, logsOutputLines(out))
	require.NotContains(t, out, "qa line")
}

// TestLogsTailsEveryAgentOfABead is the form a user actually types: a bead
// id, not an agent id. Every agent that worked the bead is printed under a
// header, and a bead whose id is a prefix of another's is not caught.
func TestLogsTailsEveryAgentOfABead(t *testing.T) {
	_, repo := statusEnv(t)
	logsFixture(t, repo, logsRunID)

	out, err := rootRun(t, "--repo", repo, "logs", "sc-1")
	require.NoError(t, err)
	require.Contains(t, out, "==> eng-sc-1-r1 <==")
	require.Contains(t, out, "==> tester-qa-sc-1-r1 <==")
	require.Contains(t, out, "eng line 3")
	require.Contains(t, out, "qa line 2")
	require.NotContains(t, out, "other line", "sc-12 is a different bead")
	require.Less(t, strings.Index(out, "eng-sc-1-r1"), strings.Index(out, "tester-qa-sc-1-r1"),
		"logs are printed in agent-id order, so a bead's rounds read in order")

	out, err = rootRun(t, "--repo", repo, "logs", "sc-12")
	require.NoError(t, err)
	require.Contains(t, out, "other line 2")
	require.NotContains(t, out, "eng line")
}

// TestLogsUnknownTargetListsWhatThereIs: the error a mistyped id gets is
// the only place a user can learn which agents a run had.
func TestLogsUnknownTargetListsWhatThereIs(t *testing.T) {
	_, repo := statusEnv(t)
	logsFixture(t, repo, logsRunID)

	_, err := rootRun(t, "--repo", repo, "logs", "sc-99")
	require.Error(t, err)
	require.Contains(t, err.Error(), `no log for "sc-99"`)
	require.Contains(t, err.Error(), "eng-sc-1-r1")
	require.Contains(t, err.Error(), "tester-qa-sc-1-r1")

	// A run that has not spawned an agent yet says so instead.
	_, other := statusEnv(t)
	dir := config.RunDir(other, logsRunID)
	_, err = orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)
	logsWriteLog(t, logging.PathsFor(dir).Log, logsLines("sc", 1))
	_, err = rootRun(t, "--repo", other, "logs", "sc-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no agent logs yet")
}

// TestLogsSelectsARun: the most recent run by default, --run for an older
// one, and a run that does not exist is an error naming the directory.
func TestLogsSelectsARun(t *testing.T) {
	_, repo := statusEnv(t)
	older := "20260101-000000-0001"
	dir := config.RunDir(repo, older)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)
	logsWriteLog(t, logging.PathsFor(dir).Log, []string{"the older run"})
	logsFixture(t, repo, logsRunID)

	out, err := rootRun(t, "--repo", repo, "logs", "-n", "1")
	require.NoError(t, err)
	require.Equal(t, []string{"sc line 60"}, logsOutputLines(out))

	out, err = rootRun(t, "--repo", repo, "logs", "--run", older)
	require.NoError(t, err)
	require.Equal(t, []string{"the older run"}, logsOutputLines(out))

	_, err = rootRun(t, "--repo", repo, "logs", "--run", "20991231-235959-ffff")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no run directory")
}

// TestLogsWithoutAnyRun: a repository sc has never run in.
func TestLogsWithoutAnyRun(t *testing.T) {
	_, repo := statusEnv(t)
	_, err := rootRun(t, "--repo", repo, "logs")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no runs recorded for "+repo)
}

// TestLogsWithoutARunLog: a run directory whose orchestrator log is not
// there yet (the run died before its first line).
func TestLogsWithoutARunLog(t *testing.T) {
	_, repo := statusEnv(t)
	dir := config.RunDir(repo, logsRunID)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)

	_, err = rootRun(t, "--repo", repo, "logs")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no "+logging.RunLogName)
}

// TestLogsScrubsUntrustedText: an agent log is a verbatim copy of what a
// cartridge wrote, so it may contain anything a model chose to emit. None
// of it may repaint the terminal (the same guarantee `sc status` makes for
// the events it prints).
func TestLogsScrubsUntrustedText(t *testing.T) {
	_, repo := statusEnv(t)
	dir := logsFixture(t, repo, logsRunID)
	logsWriteAgent(t, dir, "eng-sc-2-r1", []string{
		"\x1b[1;41m spoofed \x1b[0m\rERASED\x07\x1b]0;title\x07 and\ttabbed",
	})

	out, err := rootRun(t, "--repo", repo, "logs", "eng-sc-2-r1")
	require.NoError(t, err)
	require.Contains(t, out, " spoofed ERASED and tabbed", "the text itself survives")
	for _, line := range strings.Split(out, "\n") {
		require.False(t, strings.ContainsFunc(line, func(r rune) bool {
			return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		}), "control character in %q", line)
	}
}

// TestLogsIgnoresALineStillBeingWritten: a log whose last line has no
// newline is one an agent is writing right now; printing half of it would
// be wrong, and --follow prints it once it is whole.
func TestLogsIgnoresALineStillBeingWritten(t *testing.T) {
	_, repo := statusEnv(t)
	dir := logsFixture(t, repo, logsRunID)
	path := logging.PathsFor(dir).AgentLog("eng-sc-1-r1")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a temp run directory
	require.NoError(t, err)
	_, err = f.WriteString("half a li")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	out, err := rootRun(t, "--repo", repo, "logs", "eng-sc-1-r1")
	require.NoError(t, err)
	require.Equal(t, []string{"eng line 1", "eng line 2", "eng line 3"}, logsOutputLines(out))
}

func TestLogsJSON(t *testing.T) {
	_, repo := statusEnv(t)
	dir := logsFixture(t, repo, logsRunID)

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "logs", "sc-1", "-n", "1")
	require.Equal(t, 0, code, stderr)
	var rep LogsReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.Equal(t, repo, rep.Repo)
	require.Equal(t, domain.RunID(logsRunID), rep.Run)
	require.Equal(t, dir, rep.Dir)
	require.Len(t, rep.Logs, 2)
	require.Equal(t, "eng-sc-1-r1", rep.Logs[0].Name)
	require.Equal(t, domain.AgentID("eng-sc-1-r1"), rep.Logs[0].Agent)
	require.Equal(t, []string{"eng line 3"}, rep.Logs[0].Lines)
	require.Equal(t, 3, rep.Logs[0].Total, "the tail is one line of three")
	require.Equal(t, "tester-qa-sc-1-r1", rep.Logs[1].Name)

	_, stderr, code = runExec(t, "--json", "--repo", repo, "logs", "--follow")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "--follow cannot be combined with --json")
}

// TestLogsFollowPrintsNewLinesAndNewAgents drives --follow through the real
// command: it prints the tail, then everything appended after it, and it
// picks up an agent that did not exist when it started — which is the whole
// point of following a bead rather than a file.
func TestLogsFollowPrintsNewLinesAndNewAgents(t *testing.T) {
	_, repo := statusEnv(t)
	dir := logsFixture(t, repo, logsRunID)
	logsFastPoll(t)

	w := &logsBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- ExecuteContext(ctx, []string{"--repo", repo, "logs", "sc-1", "-n", "1", "--follow"}, w, w)
	}()

	logsWaitFor(t, w, "eng line 3")
	logsAppend(t, logging.PathsFor(dir).AgentLog("eng-sc-1-r1"), "eng line 4")
	logsWaitFor(t, w, "eng line 4")

	// A second round starts while we watch: its log appears on its own.
	logsWriteAgent(t, dir, "tester-ux-sc-1-r2", []string{"ux line 1"})
	logsWaitFor(t, w, "ux line 1")
	require.Contains(t, w.String(), "==> tester-ux-sc-1-r2 <==")

	cancel()
	select {
	case code := <-done:
		require.Equal(t, 0, code, "an interrupted follow is not a failure")
	case <-time.After(5 * time.Second):
		t.Fatal("sc logs --follow did not stop when the context was cancelled")
	}
}

// Unit tests -------------------------------------------------------------

// TestLogsMentions is the segment matcher a bead id is resolved with.
func TestLogsMentions(t *testing.T) {
	for _, tc := range []struct {
		name, agent, id string
		want            bool
	}{
		{"exact", "sc-1", "sc-1", true},
		{"engineer", "eng-sc-1-r1", "sc-1", true},
		{"tester with a lens", "tester-qa-sc-1-r2", "sc-1", true},
		{"product owner arbitrating", "po-sc-1-r2-qa-r1-1", "sc-1", true},
		{"longer bead", "eng-sc-12-r1", "sc-1", false},
		{"prefix of a segment", "eng-xsc-1-r1", "sc-1", false},
		{"absent", "eng-sc-2-r1", "sc-1", false},
		{"empty id", "eng-sc-1-r1", "", false},
		{"trailing", "eng-sc-1", "sc-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, logsMentions(tc.agent, tc.id))
		})
	}
}

// TestLogsTailReadsTheEndOfALongLog covers the ring buffer directly,
// including the offset --follow carries on from.
func TestLogsTailReadsTheEndOfALongLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	logsWriteLog(t, path, logsLines("x", 1000))

	lines, total, off, err := logsTail(path, 3)
	require.NoError(t, err)
	require.Equal(t, []string{"x line 998", "x line 999", "x line 1000"}, lines)
	require.Equal(t, 1000, total)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), off)

	// A file that is not there yet is empty, not an error: an agent is
	// spawned before it writes its first line.
	lines, total, off, err = logsTail(filepath.Join(dir, "missing.log"), 3)
	require.NoError(t, err)
	require.Empty(t, lines)
	require.Zero(t, total)
	require.Zero(t, off)
}

// TestLogsSinceRestartsOnATruncatedFile: a log that shrank was rotated or
// replaced, so following it starts again from the beginning rather than
// seeking past the end.
func TestLogsSinceRestartsOnATruncatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	logsWriteLog(t, path, logsLines("x", 10))
	_, _, off, err := logsTail(path, 1)
	require.NoError(t, err)

	logsWriteLog(t, path, []string{"fresh"})
	lines, newOff, err := logsSince(path, off)
	require.NoError(t, err)
	require.Equal(t, []string{"fresh"}, lines)
	require.Equal(t, int64(len("fresh\n")), newOff)
}

// Helpers ----------------------------------------------------------------

// logsOutputLines splits command output into its non-empty lines.
func logsOutputLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// logsFastPoll shortens the --follow poll for the duration of the test.
func logsFastPoll(t *testing.T) {
	t.Helper()
	saved := logsPollInterval
	t.Cleanup(func() { logsPollInterval = saved })
	logsPollInterval = 5 * time.Millisecond
}

// logsBuffer is a writer the command and the test can share: --follow
// writes from the command's goroutine while the test reads.
type logsBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *logsBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *logsBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// logsWaitFor blocks until want has been printed, or fails the test.
func logsWaitFor(t *testing.T, w *logsBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("sc logs --follow never printed %q; output so far:\n%s", want, w.String())
}
