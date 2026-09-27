package cli

// The production half of P9-03's "--resume reconciles": a run `sc` assembles
// must be given something to act on what the resume found (P6-15).
//
// internal/orchestrator's crash-recovery test proves what the two hand-offs
// do — a bead left `delivering` with an open pull request and no gh:pr gate
// gets its gate and closes, a worktree left mid-rebase goes back to its
// engineer — but it proves it over a rig that wires the handler itself. This
// test is the other half: the run `sc <repo>` actually builds has one, and it
// is the very deliverer that run delivers with. Without it
// orchestrator.Run.reconcileResume returns after announcing what it found and
// both hand-offs are dead code in the shipped binary.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
)

// TestRunWiresTheResumeHandoffsToTheDeliverer assembles a run the way
// `sc <repo>` does and checks that [orchestrator.Options.OnResume] reached
// it, that it forwards to the run's own delivery handoff, and that calling
// it really does run [orchestrator.Deliverer.RedeliverPR] — read back from
// the run's event log rather than from anything the test holds.
func TestRunWiresTheResumeHandoffsToTheDeliverer(t *testing.T) {
	// Safety off, with a bead whose branch does not exist: the settler
	// cannot compare it with its pull request, so RedeliverPR warns that
	// the pull request is a human's to finish and returns before reaching
	// gh or bd. The warning is the proof the call arrived.
	env := runFireSetup(t, false)
	ctx := context.Background()

	rc, err := ResolveRun(RunOptions{Path: env.Repo.Dir, Brief: runFireBrief, Headless: true})
	require.NoError(t, err)

	filter, err := agents.NewSecretFilter(rc.Effective.Exclude...)
	require.NoError(t, err)
	logOpts := LogOptions(false)
	logOpts.Redactor = filter.Redactor()
	runLog, err := logging.OpenRunLog(rc.Run.StateDir, rc.Run.ID, logOpts)
	require.NoError(t, err)
	defer func() { _ = runLog.Close() }()

	c, err := assembleRun(ctx, rc, runSetup{
		Logger: slog.New(logging.NewTerminalHandler(io.Discard, logOpts)), Out: io.Discard,
		Filter: filter, RunLog: runLog, Headless: true,
	})
	require.NoError(t, err)
	defer c.close()

	handler := c.run.ResumeHandler()
	require.NotNil(t, handler,
		"the run must be given a resume handler, or --resume reconciles the worktrees and does nothing about what it found (P6-15)")
	wiring, ok := handler.(orchestrator.DeliveryResumeHandler)
	require.True(t, ok, "the handler is the shared production one: %T", handler)
	// assembleRun builds exactly one deliverer and gives it to the
	// scheduler, the conductor and this handler, so a non-nil one here is
	// the very deliverer the run delivers with.
	require.NotNil(t, wiring.Deliverer,
		"the resume hands off to the deliverer the run delivers with, which is built after the run")
	require.NotNil(t, wiring.Deliverer(),
		"and it is there by the time the resume calls it")

	// The hand-off runs the deliverer, not a stub: with safety off the
	// re-delivery declines and says so through the run.
	bead := domain.Bead{ID: "sc-crash", Title: "Left delivering by a crash", State: domain.BeadStateDelivering}
	require.NoError(t, handler.RedeliverPR(ctx, gitx.ResumeEntry{
		Bead: bead, State: domain.BeadStateDelivering, PRState: "OPEN",
		PR: &gitx.PullRequest{Number: 7, URL: "https://github.com/acme/widgets/pull/7"},
	}))

	c.closeLog()
	evs, err := orchestrator.ReadEventLog(rc.Run.StateDir)
	require.NoError(t, err)
	var warned []string
	for _, ev := range evs {
		w, isWarning := ev.(events.Warning)
		if !isWarning || w.Bead != bead.ID {
			continue
		}
		assert.Equal(t, orchestrator.SourceDelivery, w.Source)
		warned = append(warned, w.Message)
	}
	require.Lenf(t, warned, 1, "the delivery handoff answered the resume once (log: %d events)", len(evs))
	assert.Contains(t, warned[0], "pull request #7 could not be compared with the local branch; leaving it to a human")
}
