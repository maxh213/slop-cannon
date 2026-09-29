package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

// Tests for `sc watch`: the run TUI on a run another process is firing. The
// follower itself is tested in internal/orchestrator; these pin what the
// command resolves and what it hands the TUI.

// watchWrite records a run with evs in repo's state directory, the way `sc
// run` lays one out, and returns its directory.
func watchWrite(t *testing.T, repo string, id domain.RunID, evs ...events.Event) string {
	t.Helper()
	dir := config.RunDir(repo, string(id))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	log, err := orchestrator.OpenEventLog(dir, nil)
	require.NoError(t, err)
	for _, ev := range evs {
		require.NoError(t, log.Write(ev))
	}
	require.NoError(t, log.Close())
	return dir
}

// watchStarted and watchFinished are the two ends of a recorded run.
func watchStarted(id domain.RunID) events.Event {
	return events.RunStarted{Meta: events.MetaAt(id, time.Date(2026, 9, 24, 21, 0, 0, 0, time.UTC)),
		Info: domain.Run{ID: id, Mode: domain.RunModeBrief}}
}

func watchFinished(id domain.RunID) events.Event {
	return events.RunFinished{Meta: events.MetaAt(id, time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC)),
		Reason: events.FinishCompleted}
}

// watchCapture stands in for the terminal: it claims stdin and stdout are
// one, and records the options the command gave the TUI along with every
// event the stream carried until it closed.
type watchCapture struct {
	opts tui.Options
	got  []events.Event
}

func watchStub(t *testing.T, alive bool) *watchCapture {
	t.Helper()
	c := &watchCapture{}
	savedTUI, savedTerm, savedLive := watchTUI, isTerminal, watchLiveness
	t.Cleanup(func() { watchTUI, isTerminal, watchLiveness = savedTUI, savedTerm, savedLive })
	isTerminal = func(*os.File) bool { return true }
	watchLiveness = func(string, domain.RunID) bool { return alive }
	watchTUI = func(ctx context.Context, opts tui.Options) error {
		c.opts = opts
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-opts.Events:
				if !ok {
					return nil
				}
				c.got = append(c.got, ev)
			case <-timeout:
				t.Error("the event stream never closed")
				return nil
			case <-ctx.Done():
				return nil
			}
		}
	}
	return c
}

func TestWatchHelpGolden(t *testing.T) {
	out, err := rootRun(t, "watch", "--help")
	require.NoError(t, err)
	rootGolden(t, "watch-help", out)
}

func TestWatchShowsTheMostRecentRunReadOnly(t *testing.T) {
	_, repo := runTestEnv(t)
	older, newer := domain.RunID("20260924-100000-aaaa"), domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, older, watchStarted(older), watchFinished(older))
	watchWrite(t, repo, newer, watchStarted(newer), watchFinished(newer))
	c := watchStub(t, false)

	_, stderr, code := runExec(t, "watch", repo)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, newer, c.opts.Run.ID)
	require.Equal(t, []events.Event{watchStarted(newer), watchFinished(newer)}, c.got)
	require.True(t, c.opts.Watch, "q must leave the TUI, not stop the run")
	require.Nil(t, c.opts.Controller, "a watcher must not be able to pause, stop or re-fire the run")
	require.Nil(t, c.opts.Bus)
	require.True(t, c.opts.AltScreen)
}

func TestWatchNamesARunAndTakesTheTUIFlags(t *testing.T) {
	_, repo := runTestEnv(t)
	older, newer := domain.RunID("20260924-100000-aaaa"), domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, older, watchStarted(older), watchFinished(older))
	watchWrite(t, repo, newer, watchStarted(newer))
	c := watchStub(t, false)

	_, stderr, code := runExec(t, "watch", "--repo", repo, "--run", string(older), "--no-anim", "--alt-screen=false")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, older, c.opts.Run.ID)
	require.Len(t, c.got, 2)
	require.True(t, c.opts.NoAnim)
	require.False(t, c.opts.AltScreen)
}

// TestWatchLetsGoOfARunWhoseProcessIsGone: a run that crashed never writes
// its RunFinished, so the stream ends when nothing holds the lock and the
// log has been read to the end.
func TestWatchLetsGoOfARunWhoseProcessIsGone(t *testing.T) {
	_, repo := runTestEnv(t)
	id := domain.RunID("20260924-200000-cccc")
	warn := events.Warning{Meta: events.MetaAt(id, time.Date(2026, 9, 24, 21, 1, 0, 0, time.UTC)), Message: "cartridge crashed"}
	watchWrite(t, repo, id, watchStarted(id), warn)
	c := watchStub(t, false)

	_, stderr, code := runExec(t, "watch", repo)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, []events.Event{watchStarted(id), warn}, c.got)
}

func TestWatchRefusals(t *testing.T) {
	_, repo := runTestEnv(t)

	_, stderr, code := runExec(t, "watch", repo)
	require.Equal(t, 1, code)
	require.Equal(t, "sc: no runs recorded for "+repo+"\n", stderr)

	_, stderr, code = runExec(t, "watch", repo, "--run", "20260101-000000-none")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "run 20260101-000000-none: no run directory at ")

	id := domain.RunID("20260924-200000-dddd")
	watchWrite(t, repo, id, watchStarted(id))
	saved := isTerminal
	t.Cleanup(func() { isTerminal = saved })
	isTerminal = func(*os.File) bool { return false }
	_, stderr, code = runExec(t, "watch", repo)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "watch needs a terminal")
	require.Contains(t, stderr, "sc logs --follow")
}
