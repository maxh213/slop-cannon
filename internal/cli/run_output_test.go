package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

// ---------------------------------------------------------------------------
// P8-01: one lock over the writer every producer shares
// ---------------------------------------------------------------------------

// TestRunSyncWriterSerialisesConcurrentWriters is the unit half of the fix
// for the run's output race: the wrapper hands the underlying writer one
// call at a time, so a *bytes.Buffer — which is what this package's own
// tests give `sc run` — is safe under it. Under -race the unwrapped form of
// this test is exactly the failure the reviewer reproduced.
func TestRunSyncWriterSerialisesConcurrentWriters(t *testing.T) {
	var buf bytes.Buffer
	w := newSyncWriter(&buf)

	const writers, each = 8, 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			for range each {
				_, _ = fmt.Fprintf(w, "line from %d\n", i)
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	require.Len(t, lines, writers*each)
	for _, line := range lines {
		require.Regexp(t, `^line from \d$`, line, "a write landed inside another one")
	}
}

// TestRunSyncWriterDoesNotWrapItself: the run hands the same writer to
// several producers, and wrapping an already guarded writer twice would put
// two locks in front of one stream — safe, but the second is a lie about
// which lock protects it.
func TestRunSyncWriterDoesNotWrapItself(t *testing.T) {
	w := newSyncWriter(io.Discard)
	require.Same(t, w, newSyncWriter(w))
	require.Equal(t, io.Discard, w.unwrap())
}

// TestRunScreenSharesTheLockWhenOneWriterTakesBoth covers [runScreen]: the
// renderer draws on stdout, which is a different stream in production but
// the very same writer when the caller merged them (cli.NewRootCommand
// does). Then it must share the lock. The TUI is the exception — Bubble Tea
// reads the terminal off the writer it is given.
func TestRunScreenSharesTheLockWhenOneWriterTakesBoth(t *testing.T) {
	var one bytes.Buffer
	cmd := NewRootCommand(&one) // sets out and err to the same writer
	guard := newSyncWriter(cmd.ErrOrStderr())
	require.Same(t, guard, runScreen(cmd, guard, true))
	require.NotSame(t, guard, runScreen(cmd, guard, false), "the TUI needs the raw stdout")

	var out, errOut bytes.Buffer
	split := NewRootCommand(&out)
	split.SetErr(&errOut)
	require.Same(t, &out, runScreen(split, newSyncWriter(&errOut), true),
		"two streams need no shared lock")
}

// TestRunSameWriterSurvivesAnUncomparableWriter: `sc run` writes wherever
// the caller of ExecuteContext pointed it, and comparing two interfaces
// whose dynamic type is identical and uncomparable panics. sameWriter must
// answer instead of taking the process down.
func TestRunSameWriterSurvivesAnUncomparableWriter(t *testing.T) {
	a, b := uncomparableWriter{}, uncomparableWriter{}
	require.NotPanics(t, func() { require.False(t, sameWriter(a, b)) })
	require.NotPanics(t, func() { require.False(t, sameWriter(nil, nil)) })

	var buf bytes.Buffer
	require.True(t, sameWriter(&buf, &buf))
	require.False(t, sameWriter(&buf, io.Discard))
}

// uncomparableWriter is a value type with an uncomparable field, so `==` on
// two of them panics.
type uncomparableWriter struct{ notes []string }

func (w uncomparableWriter) Write(p []byte) (int, error) {
	if len(w.notes) > 0 {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// TestRunGuardsTheStderrEveryProducerShares is the regression for the data
// race the reviewer reproduced: `sc run` handed the same unsynchronised
// io.Writer to two concurrent producers — the headless terminal log
// handler, which every component and every helper goroutine logs through,
// and orchestrator.Options.Out, where the run prints its P6-18 worktree
// cleanup report from the finish path, before Run.Done closes and so while
// the conductor and the gate poller are still logging.
//
// It fires the real command so the writers under test are the ones
// fireCannon built, stops at the assembly seam, and then holds the report
// writer inside a single Write while a log line is attempted. Sharing one
// lock is the only thing that can make the second one wait.
func TestRunGuardsTheStderrEveryProducerShares(t *testing.T) {
	env := runFireSetup(t, true)

	const mark = "cleanup-report"
	gate := newGateWriter(mark)
	var set runSetup
	saved := runOnAssemble
	t.Cleanup(func() { runOnAssemble = saved })
	runOnAssemble = func(c *cannon) error { set = c.set; return errUnassembled }

	ctx, cancel := context.WithTimeout(context.Background(), runFireTimeout)
	defer cancel()
	var stdout bytes.Buffer
	code := ExecuteContext(ctx, []string{env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock"}, &stdout, gate)
	require.Equal(t, 1, code, "the seam failed the assembly on purpose")
	require.NotNil(t, set.Logger, "the seam saw the setup fireCannon built")

	report := set.reportWriter()
	require.NotNil(t, report, "a headless run prints its own report")
	require.IsType(t, &syncWriter{}, set.Out, "the run's writer is not guarded at all")

	// One producer is inside Write and staying there.
	go func() { _, _ = fmt.Fprintln(report, mark+": removed 1 worktree") }()
	<-gate.entered

	// The other must not get in behind it.
	logged, going := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(logged)
		close(going)
		set.Logger.Warn("a helper logged during the report")
	}()
	<-going
	select {
	case <-logged:
		t.Fatal("a log line was written while the run's report was inside Write: " +
			"the terminal handler and orchestrator.Options.Out do not share a lock")
	case <-time.After(500 * time.Millisecond):
	}

	close(gate.release)
	select {
	case <-logged:
	case <-time.After(runFireTimeout):
		t.Fatal("the log line never landed after the report was let go")
	}
	require.Contains(t, gate.String(), "a helper logged during the report")
	require.Contains(t, gate.String(), mark)
}

// gateWriter is a writer that holds the goroutine writing a marked line
// inside Write until it is released, so a second producer either blocks
// (one shared lock) or slips past (the race).
type gateWriter struct {
	mark    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

func newGateWriter(mark string) *gateWriter {
	return &gateWriter{mark: mark, entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *gateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.mark) {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *gateWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// ---------------------------------------------------------------------------
// P8-01: neither wait for the helpers is unbounded
// ---------------------------------------------------------------------------

// TestRunHelpersStopWhatTheyStarted: the helpers run under a context stop
// cancels, and stop reports that they all returned.
func TestRunHelpersStopWhatTheyStarted(t *testing.T) {
	var ran atomic.Int32
	task := func(ctx context.Context) error {
		ran.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	h := startRunHelpers(context.Background(), task, task, task)
	require.True(t, h.stop(5*time.Second), "three well-behaved helpers stopped")
	require.EqualValues(t, 3, ran.Load())
	require.True(t, h.stop(5*time.Second), "stop is idempotent; the run calls it twice")
}

// TestRunHelpersSurviveTheirCallersContext: a SIGINT cancels the command's
// context, but the helpers keep going until the run itself is over — the
// conductor is what notices that the last bead settled.
func TestRunHelpersSurviveTheirCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	going, gone := make(chan struct{}), make(chan struct{})
	h := startRunHelpers(ctx, func(hctx context.Context) error {
		close(going)
		<-hctx.Done()
		close(gone)
		return nil
	})
	<-going
	cancel()
	select {
	case <-gone:
		t.Fatal("cancelling the caller's context stopped a helper: only the run's own end may")
	case <-time.After(20 * time.Millisecond):
	}
	require.True(t, h.stop(5*time.Second))
	<-gone
}

// TestRunHelpersGiveUpOnAWedgedHelper is the fix for the second, unbounded
// wait: a helper stuck inside bd, git or gh must not hang `sc run` after
// the renderer has gone and with the summary unprinted.
func TestRunHelpersGiveUpOnAWedgedHelper(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := startRunHelpers(context.Background(), func(context.Context) error {
		<-release // ignores the cancellation, exactly like a wedged subprocess
		return nil
	})

	start := time.Now()
	require.False(t, h.stop(50*time.Millisecond), "the wedged helper was reported as stopped")
	require.Less(t, time.Since(start), 5*time.Second, "the wait was not bounded")
}

// ---------------------------------------------------------------------------
// P8-01: the summary names the reviews a D-11 run posted
// ---------------------------------------------------------------------------

// TestRunDeliveryLine: an ordinary run's row is what it always was, and a
// run that reviewed somebody else's pull requests says so.
func TestRunDeliveryLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		sum  orchestrator.RunSummary
		want string
	}{
		{"nothing", orchestrator.RunSummary{}, "0 pull requests, 0 merges"},
		{"one of each", orchestrator.RunSummary{PRs: 1, Merges: 1}, "1 pull request, 1 merge"},
		{"reviewed one", orchestrator.RunSummary{Reviewing: 1},
			"0 pull requests, 0 merges, 1 external pull request reviewed"},
		{"reviewed three", orchestrator.RunSummary{PRs: 2, Merges: 1, Reviewing: 3},
			"2 pull requests, 1 merge, 3 external pull requests reviewed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, runDeliveryLine(tc.sum))
		})
	}
}

// TestRunSummaryNamesTheExternalPullRequestsItReviewed drives the summary
// the run actually prints over a log a D-11 review run would have written:
// the reviewed pull requests used to survive only in `--json`, so the run
// reported "0 pull requests, 0 merges" and said nothing about the reviews
// it posted.
func TestRunSummaryNamesTheExternalPullRequestsItReviewed(t *testing.T) {
	run := domain.Run{ID: domain.RunID("20260904-120000-abcd"), StateDir: t.TempDir()}
	writeSummaryLog(t, run.StateDir,
		events.PROpened{Meta: events.MetaAt(run.ID, time.Unix(0, 0)), Bead: "sc-1",
			URL: "https://example.invalid/o/r/pull/7", Number: 7, External: true},
		events.PROpened{Meta: events.MetaAt(run.ID, time.Unix(1, 0)), Bead: "sc-2",
			URL: "https://example.invalid/o/r/pull/8", Number: 8, External: true},
		events.RunFinished{Meta: events.MetaAt(run.ID, time.Unix(2, 0)),
			Reason: events.FinishCompleted},
	)

	c := &cannon{rc: &config.RunConfig{Run: run}, spend: &runSpend{}}
	var out bytes.Buffer
	require.NoError(t, c.writeSummary(&out, orchestrator.Outcome{Reason: events.FinishCompleted}))
	require.Regexp(t, `(?m)^delivery   0 pull requests, 0 merges, 2 external pull requests reviewed$`,
		out.String())
}

// writeSummaryLog writes a run log the summary can be folded from.
func writeSummaryLog(t *testing.T, dir string, evs ...events.Event) {
	t.Helper()
	var b bytes.Buffer
	for _, e := range evs {
		line, err := events.Marshal(e)
		require.NoError(t, err)
		b.Write(line)
		b.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(orchestrator.EventLogPath(dir), b.Bytes(), 0o600))
}
