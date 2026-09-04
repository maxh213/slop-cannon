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
		"the engineer wrote first, so it prints first")

	out, err = rootRun(t, "--repo", repo, "logs", "sc-12")
	require.NoError(t, err)
	require.Contains(t, out, "other line 2")
	require.NotContains(t, out, "eng line")
}

// logsEpoch is the fixed time the ordering fixtures are stamped from.
var logsEpoch = time.Date(2026, 9, 3, 21, 41, 0, 0, time.UTC)

// logsWriteAgentAt writes one agent log whose first line is a real log
// record — one JSON object with a "time" key, which is what
// [logging.NewFileHandler] writes — stamped at first, and then backdates
// the file's modification time to mtime.
//
// The two are set apart on purpose: the modification time is only the
// fallback, so a fixture whose mtimes disagree with its records proves
// which of the two the command actually orders by.
func logsWriteAgentAt(t *testing.T, runDir, agent string, first, mtime time.Time, lines []string) string {
	t.Helper()
	rec := fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"agent spawned","agent":%q}`,
		first.Format(time.RFC3339Nano), agent)
	path := logsWriteAgent(t, runDir, agent, append([]string{rec}, lines...))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	return path
}

// TestLogsPrintsABeadsAgentsOldestFirst is the ordering `sc logs --help`
// promises. The ids are minted from the bead, the lens and the round, so
// name order puts the engineer's second round ahead of the testers that
// judged the first — which is what a real run printed before this was
// fixed (P8-05). The fixture's modification times run the other way, so
// only the records' own timestamps can produce the order asserted here.
func TestLogsPrintsABeadsAgentsOldestFirst(t *testing.T) {
	_, repo := statusEnv(t)
	dir := config.RunDir(repo, logsRunID)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)
	logsWriteLog(t, logging.PathsFor(dir).Log, logsLines("sc", 1))

	// Written in name order, stamped in the order the agents really ran,
	// and given modification times in the reverse of that order.
	at := func(sec int) time.Time { return logsEpoch.Add(time.Duration(sec) * time.Second) }
	logsWriteAgentAt(t, dir, "eng-sc-1-r1", at(31), at(90), logsLines("eng r1", 1))
	logsWriteAgentAt(t, dir, "eng-sc-1-r2", at(35), at(80), logsLines("eng r2", 1))
	logsWriteAgentAt(t, dir, "tester-qa-sc-1-r1", at(32), at(70), logsLines("qa", 1))
	logsWriteAgentAt(t, dir, "tester-test-sc-1-r1", at(33), at(60), logsLines("test", 1))

	out, err := rootRun(t, "--repo", repo, "logs", "sc-1")
	require.NoError(t, err)
	require.Equal(t,
		[]string{"eng-sc-1-r1", "tester-qa-sc-1-r1", "tester-test-sc-1-r1", "eng-sc-1-r2"},
		logsHeaders(out), "oldest agent first, whatever the ids sort like")

	// --json prints the same order, because it prints the same list.
	out, err = rootRun(t, "--repo", repo, "--json", "logs", "sc-1")
	require.NoError(t, err)
	var rep LogsReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	names := make([]string, 0, len(rep.Logs))
	for _, l := range rep.Logs {
		names = append(names, l.Name)
	}
	require.Equal(t,
		[]string{"eng-sc-1-r1", "tester-qa-sc-1-r1", "tester-test-sc-1-r1", "eng-sc-1-r2"}, names)
}

// TestLogsOrdersByModificationTimeWhenARecordCannotBeRead: a log whose
// first line is not a record the command can read — an agent that has
// written nothing yet, or a format that changes one day — falls back to the
// file's own time rather than to an arbitrary order.
func TestLogsOrdersByModificationTimeWhenARecordCannotBeRead(t *testing.T) {
	_, repo := statusEnv(t)
	dir := config.RunDir(repo, logsRunID)
	_, err := orchestrator.EnsureRunDir(dir)
	require.NoError(t, err)
	logsWriteLog(t, logging.PathsFor(dir).Log, logsLines("sc", 1))

	// Plain lines, no records: only the modification times can order these.
	early := logsWriteAgent(t, dir, "tester-qa-sc-1-r1", logsLines("qa", 1))
	late := logsWriteAgent(t, dir, "eng-sc-1-r1", logsLines("eng", 1))
	require.NoError(t, os.Chtimes(early, logsEpoch, logsEpoch))
	require.NoError(t, os.Chtimes(late, logsEpoch.Add(time.Minute), logsEpoch.Add(time.Minute)))

	out, err := rootRun(t, "--repo", repo, "logs", "sc-1")
	require.NoError(t, err)
	require.Equal(t, []string{"tester-qa-sc-1-r1", "eng-sc-1-r1"}, logsHeaders(out))
}

// logsHeaders is the agent names the output announced, in print order.
func logsHeaders(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "==> ") && strings.HasSuffix(line, " <==") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(line, "==> "), " <=="))
		}
	}
	return names
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

// TestLogsErrorsAreOnTheTroubleshootingPage closes the other half of P9-08
// for this command. That test walks `errors.New` sentinels, so a message
// built with fmt.Errorf — which is every user-facing error `sc logs` has —
// can never fail it, and none of them were documented. Each stem below is
// asserted in both directions: it is still the text logs.go prints, and it
// still keys a row on docs/troubleshooting.md.
func TestLogsErrorsAreOnTheTroubleshootingPage(t *testing.T) {
	stems := []string{
		"no runs recorded for",
		"no run directory at",
		"the run has no agent logs yet",
		"the run wrote no log yet",
		"this run has:",
		"a followed log has no end to encode",
	}
	src, err := os.ReadFile("logs.go")
	require.NoError(t, err)
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	require.NoError(t, err)
	for _, stem := range stems {
		require.Contains(t, string(src), stem,
			"logs.go no longer prints %q: reword docs/troubleshooting.md with it", stem)
		require.Contains(t, string(doc), "| `"+stem+"` |",
			"docs/troubleshooting.md has no row keyed %q, and it is a message sc logs prints", stem)
	}
}

// TestLogsAdviceForAPrunedRunDirectoryIsTrue guards the row that used to
// tell a user to run "`sc status` without arguments", which "lists the runs
// that are still on disk". It does not: bare `sc status` reports exactly
// one run, the most recent, and no command lists them. The row now points
// at the runs directory itself, so this checks the directory it names is
// the one runs actually live in.
func TestLogsAdviceForAPrunedRunDirectoryIsTrue(t *testing.T) {
	_, repo := statusEnv(t)
	dir := logsFixture(t, repo, logsRunID)
	require.Equal(t, filepath.Dir(dir), filepath.Join(config.StateDir(repo), config.RunsDirName),
		"a run directory lives under <state-dir>/runs, which is what the page tells the user to list")

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	require.NoError(t, err)
	row := logsDocRow(t, string(doc), "no run directory at")
	require.Contains(t, row, config.RunsDirName+"/`",
		"the row must name the directory the runs are in")
	require.NotContains(t, row, "lists the runs that are still on disk",
		"sc status reports one run, never a list")
}

// logsDocRow returns the troubleshooting row keyed by stem.
func logsDocRow(t *testing.T, doc, stem string) string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| `"+stem+"` |") {
			return line
		}
	}
	t.Fatalf("docs/troubleshooting.md has no row keyed %q", stem)
	return ""
}
