package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// Tests for the run-level glue (P8-14) and everything wired through it: the
// resumed run's feedback threads (P8-16), the bead source the shutdown
// release needs (P8-17), the leftovers commit through DeliveryGit (P8-18),
// the run log teed into the run (P8-20) and the bead inventory the run log
// never had (P6-32). Every helper here is prefixed "glue" so it cannot
// collide with another lane's file in this package.

// --- fakes ---------------------------------------------------------------

// glueBD is an in-memory bd: the run's beads, their comments, and the calls
// the glue made. Everything is deterministic and nothing spawns a process.
type glueBD struct {
	mu       sync.Mutex
	issues   []beads.Issue
	msgs     map[domain.BeadID][]beads.Message
	calls    map[string]int
	listErr  error
	readyErr error
	msgErr   error
}

func newGlueBD(issues ...beads.Issue) *glueBD {
	return &glueBD{
		issues: issues,
		msgs:   map[domain.BeadID][]beads.Message{},
		calls:  map[string]int{},
	}
}

// glueIssue builds one bd issue.
func glueIssue(id domain.BeadID, kind domain.BeadType, status, title string) beads.Issue {
	return beads.Issue{ID: id, Title: title, Status: status, Type: string(kind),
		Priority: domain.PriorityDefault}
}

func (b *glueBD) ListRun(_ context.Context, _ domain.RunID, _ bool) ([]beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["list"]++
	if b.listErr != nil {
		return nil, b.listErr
	}
	return append([]beads.Issue(nil), b.issues...), nil
}

func (b *glueBD) Ready(_ context.Context, _ domain.BeadID) ([]beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["ready"]++
	if b.readyErr != nil {
		return nil, b.readyErr
	}
	var out []beads.Issue
	for _, is := range b.issues {
		if is.Status == string(domain.BeadStateOpen) && !is.IsEpic() {
			out = append(out, is)
		}
	}
	return out, nil
}

func (b *glueBD) Show(_ context.Context, ids ...domain.BeadID) ([]beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["show"]++
	var out []beads.Issue
	for _, id := range ids {
		for _, is := range b.issues {
			if is.ID == id {
				out = append(out, is)
			}
		}
	}
	return out, nil
}

func (b *glueBD) Messages(_ context.Context, id domain.BeadID) ([]beads.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["messages"]++
	if b.msgErr != nil {
		return nil, b.msgErr
	}
	return append([]beads.Message(nil), b.msgs[id]...), nil
}

// set moves a bead to a new bd status, the way an agent or the state
// machine would.
func (b *glueBD) set(id domain.BeadID, status domain.BeadState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.issues {
		if b.issues[i].ID == id {
			b.issues[i].Status = string(status)
			return
		}
	}
}

// add creates a bead outside any plan (D-11's external review bead).
func (b *glueBD) add(is beads.Issue) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issues = append(b.issues, is)
}

// state reads a bead's status back.
func (b *glueBD) state(id domain.BeadID) domain.BeadState {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, is := range b.issues {
		if is.ID == id {
			return domain.BeadState(is.Status)
		}
	}
	return ""
}

// count returns how many calls of one kind the glue made.
func (b *glueBD) count(kind string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[kind]
}

// glueFeedback records what the glue asked of the feedback loop, and can
// stand in for the loop itself through onTrack.
type glueFeedback struct {
	mu        sync.Mutex
	tracked   []domain.BeadID
	restored  []domain.BeadID
	observed  map[domain.BeadID]domain.BeadState
	checks    []domain.BeadID
	reviews   []domain.BeadID
	order     []string
	rounds    map[domain.BeadID]int
	onTrack   func(bs []domain.Bead)
	restoreEr error
}

func newGlueFeedback() *glueFeedback {
	return &glueFeedback{observed: map[domain.BeadID]domain.BeadState{}, rounds: map[domain.BeadID]int{}}
}

func (f *glueFeedback) Track(bs ...domain.Bead) {
	f.mu.Lock()
	for _, b := range bs {
		f.tracked = append(f.tracked, b.ID)
		f.order = append(f.order, "track:"+b.ID.String())
	}
	hook := f.onTrack
	f.mu.Unlock()
	if hook != nil {
		hook(bs)
	}
}

func (f *glueFeedback) Restore(_ context.Context, bead domain.Bead, _ []beads.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreEr != nil {
		return f.restoreEr
	}
	f.restored = append(f.restored, bead.ID)
	f.order = append(f.order, "restore:"+bead.ID.String())
	return nil
}

func (f *glueFeedback) Thread(bead domain.BeadID) (FeedbackThread, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rounds[bead]
	if !ok {
		return FeedbackThread{}, false
	}
	return FeedbackThread{Bead: bead, Round: r}, true
}

func (f *glueFeedback) Observe(bead domain.BeadID, state domain.BeadState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed[bead] = state
	f.order = append(f.order, "observe:"+bead.String())
}

func (f *glueFeedback) CheckFailed(_ context.Context, bead domain.Bead, _ int, _ []domain.FeedbackItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, bead.ID)
	f.order = append(f.order, "check_failed:"+bead.ID.String())
	return nil
}

func (f *glueFeedback) ReviewAgain(_ context.Context, bead domain.Bead, _ int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, bead.ID)
	f.order = append(f.order, "review_again:"+bead.ID.String())
	return nil
}

func (f *glueFeedback) snapshot() (tracked, restored []domain.BeadID, order []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.BeadID(nil), f.tracked...),
		append([]domain.BeadID(nil), f.restored...),
		append([]string(nil), f.order...)
}

func (f *glueFeedback) seen(bead domain.BeadID) domain.BeadState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observed[bead]
}

// glueDelivery is the delivery handoff: whatever the test says it did.
type glueDelivery struct {
	mu       sync.Mutex
	requests []DeliveryRequest
	deliver  func(req DeliveryRequest) (*DeliveryOutcome, error)
	resolve  func(id domain.BeadID) (*DeliveryOutcome, error)
}

func (d *glueDelivery) Deliver(_ context.Context, req DeliveryRequest) (*DeliveryOutcome, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	fn := d.deliver
	d.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(req)
}

func (d *glueDelivery) Resolved(_ context.Context, id domain.BeadID) (*DeliveryOutcome, error) {
	d.mu.Lock()
	fn := d.resolve
	d.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(id)
}

func (d *glueDelivery) sent() []DeliveryRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DeliveryRequest(nil), d.requests...)
}

// glueGates counts the kicks the delivery earned.
type glueGates struct{ n int64 }

func (g *glueGates) Kick() { g.n++ }

// glueGit is a DeliveryGit that records the leftovers commit and can refuse
// it the way a mid-rebase worktree does (P6-16(b)).
type glueGit struct {
	mu        sync.Mutex
	leftovers []domain.BeadID
	diffs     int
	commitErr error
	diff      *gitx.Diff
}

func (g *glueGit) DefaultBranch(context.Context) (string, error) { return "main", nil }

func (g *glueGit) RebaseState(context.Context, domain.BeadID) (RebaseState, error) {
	return RebaseState{}, nil
}

func (g *glueGit) CommitLeftovers(_ context.Context, bead domain.Bead, _ int) (*gitx.Leftovers, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leftovers = append(g.leftovers, bead.ID)
	if g.commitErr != nil {
		return nil, g.commitErr
	}
	return &gitx.Leftovers{}, nil
}

func (g *glueGit) Diff(context.Context, domain.BeadID) (*gitx.Diff, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.diffs++
	return g.diff, nil
}

func (g *glueGit) ChangeRatio(context.Context, domain.BeadID, *gitx.Diff, *gitx.Diff) (gitx.ChangeRatio, error) {
	return gitx.ChangeRatio{}, nil
}

func (g *glueGit) committed() []domain.BeadID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]domain.BeadID(nil), g.leftovers...)
}

// glueRig is a started run with the glue over it.
type glueRig struct {
	Fixture *lifecycleFixture
	Config  *config.RunConfig
	Run     *Run
	BD      *glueBD
	Log     *EventLog
	Cond    *Conductor
}

// glueSetup builds a run with the run log teed in front of the bus (P8-20)
// and the conductor over it, without starting either.
func glueSetup(t *testing.T, bd *glueBD, opts ConductorOptions, settings config.RunSettings) *glueRig {
	t.Helper()
	f := lifecycleSetup(t)
	rc := f.runConfig(t, settings)

	runOpts := f.options()
	runOpts.SkipLock = true
	log, err := TeeRunLog(&runOpts, rc.Run.StateDir, f.Bus)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	r, err := New(rc, runOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	opts.Run, opts.Beads = r, bd
	if opts.Interval == 0 {
		opts.Interval = 5 * time.Millisecond
	}
	c, err := NewConductor(opts)
	require.NoError(t, err)

	return &glueRig{Fixture: f, Config: rc, Run: r, BD: bd, Log: log, Cond: c}
}

// glueYes is ConductorOptions.Restore for a test that wants the resume
// behaviour without a state file to resume from.
func glueYes() *bool { yes := true; return &yes }

// glueEventually waits for a condition the glue is driving, without
// sleeping for a fixed time.
func glueEventually(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// glueLogLines counts the records in the run's log file.
func glueLogLines(t *testing.T, runDir string) int {
	t.Helper()
	data, err := os.ReadFile(EventLogPath(runDir)) //nolint:gosec // the test's own temp directory.
	require.NoError(t, err)
	return bytes.Count(data, []byte("\n"))
}

// --- a whole job, driven by the production glue alone --------------------

// TestGlueDrivesAJobFromReadyToClosed is P8-14's exit criterion without the
// CLI: bd reports two ready beads, the glue offers them to the feedback
// loop, the loop approves them, the glue ships them, and the delivery
// outcome comes back through Observe. Nothing here is the e2e test's
// conductor: every step is Conductor's own.
//
// It also pins P8-20 (the run left an events.jsonl `sc status` can fold),
// P6-32 (the inventory and the graph reached the log) and P6-27 (the agent
// output went through Run.Publish, so state.json's sequence number is the
// number of lines in the log).
func TestGlueDrivesAJobFromReadyToClosed(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "ship the retry"),
		glueIssue("sc-1", domain.BeadTypeTask, "open", "add the retry"),
		glueIssue("sc-2", domain.BeadTypeTask, "open", "test the retry"),
	)
	feedback := newGlueFeedback()
	// The loop stands in for the engineer and the testers: a bead offered
	// to it comes back approved.
	feedback.onTrack = func(bs []domain.Bead) {
		for _, b := range bs {
			if b.State == domain.BeadStateOpen {
				bd.set(b.ID, domain.BeadStateApproved)
			}
		}
	}
	delivery := &glueDelivery{}
	delivery.deliver = func(req DeliveryRequest) (*DeliveryOutcome, error) {
		bd.set(req.Bead.ID, domain.BeadStateClosed)
		return &DeliveryOutcome{Bead: req.Bead.ID, Round: req.Round, Kind: DeliveryMerged,
			State: domain.BeadStateClosed, Target: "main", MergeSHA: "abc1234"}, nil
	}
	gates := &glueGates{}

	rig := glueSetup(t, bd, ConductorOptions{
		Feedback: feedback, Delivery: delivery, Gates: gates,
	}, config.RunSettings{})

	shipped := func() bool {
		return bd.state("sc-1") == domain.BeadStateClosed && bd.state("sc-2") == domain.BeadStateClosed
	}
	loopCtx, stopGlue := context.WithCancel(context.Background())
	glueDone := make(chan struct{})
	rig.Run.opts.Loop = LoopFunc(func(_ context.Context, r *Run) (Outcome, error) {
		require.NoError(t, r.SetEpic("sc-epic"))
		go func() { defer close(glueDone); _ = rig.Cond.Run(loopCtx) }()
		// Agent output reaches the run the way a fired agent's does
		// (P6-27): through the forwarding bus, not past it.
		for i := 0; i < 3; i++ {
			r.AgentBus().Publish(events.AgentOutput{Meta: r.Meta(), Agent: "eng-sc-1-r1", Line: "working"})
		}
		glueEventually(t, "both beads to be shipped", shipped)
		// The glue stops before the run finishes, so nothing publishes
		// after the final state.json is written.
		stopGlue()
		<-glueDone
		// One last pass, so the log's picture of the beads is the one bd
		// ends with rather than the one before the final delivery.
		require.NoError(t, rig.Cond.Sync(context.Background()))
		return Outcome{Summary: "both beads shipped"}, nil
	})

	require.NoError(t, rig.Run.Start(context.Background()))
	out, err := rig.Run.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishCompleted, out.Reason)
	require.NoError(t, rig.Log.Close())

	// The job really moved.
	assert.Equal(t, domain.BeadStateClosed, bd.state("sc-1"))
	assert.Equal(t, domain.BeadStateClosed, bd.state("sc-2"))
	tracked, _, _ := feedback.snapshot()
	assert.Subset(t, tracked, []domain.BeadID{"sc-1", "sc-2"}, "both ready beads were offered to the loop")
	assert.Equal(t, domain.BeadStateClosed, feedback.seen("sc-1"), "the loop is told where the bead went")
	assert.Equal(t, domain.BeadStateClosed, feedback.seen("sc-2"))
	require.Len(t, delivery.sent(), 2, "each approved bead is delivered exactly once")
	assert.Zero(t, gates.n, "a merge needs no gate poll")

	stats := rig.Cond.Stats()
	assert.Equal(t, 2, stats.Shipped)
	assert.Equal(t, 2, stats.Announced, "one BeadCreated per work bead, the epic excluded")
	assert.Positive(t, stats.Graphs)
	assert.NoError(t, stats.LastError)

	// P8-20: the run left a log `sc status` can fold, and P6-32 put the
	// inventory in it.
	summary, err := SummariseLog(rig.Config.Run.StateDir, 0)
	require.NoError(t, err)
	assert.Equal(t, PhaseFinished, summary.Phase)
	assert.Equal(t, 3, summary.Output, "the agent output lines reached the log")
	require.Len(t, summary.Beads, 2)
	assert.Equal(t, 2, summary.Counts[domain.BeadStateClosed])
	assert.Equal(t, 2, summary.Settled())

	kinds := map[events.Kind]int{}
	evs, err := ReadEventLog(rig.Config.Run.StateDir)
	require.NoError(t, err)
	for _, ev := range evs {
		kinds[ev.Kind()]++
	}
	assert.Equal(t, 2, kinds[events.KindBeadCreated], "the run publishes its bead inventory (P6-32)")
	assert.Positive(t, kinds[events.KindGraphChanged], "and the dependency graph the TUI relays out of")

	// P6-27: every event was counted, agent output included.
	st, err := LoadState(rig.Config.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, glueLogLines(t, rig.Config.Run.StateDir), int(st.LastSeq),
		"state.json's LastSeq is the number of lines in events.jsonl")
	assert.Equal(t, len(evs), int(st.LastSeq))
}

// --- delivery outcomes route back into the loops -------------------------

func TestGlueRoutesDeliveryOutcomesBackIntoTheLoops(t *testing.T) {
	cases := []struct {
		name      string
		outcome   DeliveryOutcome
		wantKick  bool
		wantCall  string
		redeliver bool
	}{
		{
			name:     "a pull request waits behind its gate and kicks the poller",
			outcome:  DeliveryOutcome{Kind: DeliveryPending, State: domain.BeadStateDelivering, Round: 2},
			wantKick: true,
		},
		{
			name:      "a failed check comes back as a change request",
			outcome:   DeliveryOutcome{Kind: DeliveryCheckFailed, State: domain.BeadStateChangesRequested, Round: 2},
			wantCall:  "check_failed:sc-1",
			redeliver: true,
		},
		{
			name:      "a big conflict resolution goes back to the testers",
			outcome:   DeliveryOutcome{Kind: DeliveryNeedsReview, State: domain.BeadStateInReview, Round: 2},
			wantCall:  "review_again:sc-1",
			redeliver: true,
		},
		{
			name:      "a conflict is the engineer's again",
			outcome:   DeliveryOutcome{Kind: DeliveryConflict, State: domain.BeadStateInProgress, Round: 2},
			redeliver: true,
		},
		{
			name:      "a retryable failure may be delivered again",
			outcome:   DeliveryOutcome{Kind: DeliveryRetryable, State: domain.BeadStateDelivering, Round: 2},
			redeliver: true,
		},
		{
			name:    "a hand-off belongs to a person now",
			outcome: DeliveryOutcome{Kind: DeliveryHandedOff, State: domain.BeadStateNeedsHuman, Round: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bd := newGlueBD(
				glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
				glueIssue("sc-1", domain.BeadTypeTask, "approved", "add the retry"),
			)
			feedback := newGlueFeedback()
			feedback.rounds["sc-1"] = 2
			gates := &glueGates{}
			outcome := tc.outcome
			outcome.Bead = "sc-1"
			delivery := &glueDelivery{deliver: func(DeliveryRequest) (*DeliveryOutcome, error) {
				return &outcome, nil
			}}
			rig := glueSetup(t, bd, ConductorOptions{
				Feedback: feedback, Delivery: delivery, Gates: gates,
			}, config.RunSettings{})
			require.NoError(t, rig.Run.Start(context.Background()))
			require.NoError(t, rig.Run.SetEpic("sc-epic"))

			require.NoError(t, rig.Cond.Sync(context.Background()))
			require.Len(t, delivery.sent(), 1)
			assert.Equal(t, 2, delivery.sent()[0].Round, "the thread's round names the delivery")

			_, _, order := feedback.snapshot()
			if tc.wantCall != "" {
				assert.Contains(t, order, tc.wantCall)
			}
			assert.Equal(t, tc.outcome.State, feedback.seen("sc-1"), "the loop is always told where the bead went")
			assert.Equal(t, tc.wantKick, gates.n > 0)

			// A second pass must not ship the same bead twice unless the
			// outcome released it.
			require.NoError(t, rig.Cond.Sync(context.Background()))
			want := 1
			if tc.redeliver {
				want = 2
			}
			assert.Len(t, delivery.sent(), want)

			require.NoError(t, rig.Run.Stop("test"))
			_, err := rig.Run.Wait()
			require.NoError(t, err)
		})
	}
}

// --- a resumed run keeps its feedback threads (P8-16) --------------------

// TestGlueResumeRebuildsTheThreadsFromTheBeadsComments drives the real
// feedback loop: a run killed at round 3 comes back, and the glue hands the
// bead's own comments to Feedback.Restore before anything can Track it back
// to round one.
func TestGlueResumeRebuildsTheThreadsFromTheBeadsComments(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "changes_requested", "add the retry"),
	)
	item := domain.FeedbackItem{ID: "qa-r3-1", Title: "the retry loop never terminates",
		Severity: domain.SeverityMajor, Status: domain.ItemOpen, File: "retry.go"}
	bd.msgs["sc-1"] = []beads.Message{
		{Type: domain.MessageImplReport, IssueID: "sc-1", Payload: domain.ImplReport{Summary: "added the retry"}},
		{Type: domain.MessageVerdict, IssueID: "sc-1", Payload: domain.Verdict{
			Lens: domain.LensQA, Round: 3, Outcome: domain.VerdictChanges,
			Summary: "still spins", Items: []domain.FeedbackItem{item},
		}},
	}

	rig := glueSetup(t, bd, ConductorOptions{Restore: glueYes()}, config.RunSettings{})
	loop, err := NewFeedback(FeedbackOptions{
		Context: rig.Cond.BeadContext,
		Run:     rig.Config.Run.ID,
		Now:     rig.Fixture.Clock.Now,
	})
	require.NoError(t, err)
	rig.Cond.Attach(ConductorParts{Feedback: loop})

	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))
	require.NoError(t, rig.Cond.Sync(context.Background()))

	th, ok := loop.Thread("sc-1")
	require.True(t, ok, "the resumed run adopted the bead")
	// The replay concludes round 3 — the killed run's last verdict asked
	// for changes — and opens round 4 for the engineer. What matters is
	// that the bead does not start again at round 1 with an empty thread.
	assert.Equal(t, 4, th.Round, "the thread carries on from where the killed run left it")
	assert.Equal(t, FeedbackAddressing, th.Phase)
	require.Len(t, th.Items, 1, "the items the testers raised survived the kill")
	assert.Equal(t, "qa-r3-1", th.Items[0].Item.ID)
	assert.Equal(t, 1, rig.Cond.Stats().Restored)

	// The restore happens once per bead, whatever the glue is asked again.
	require.NoError(t, rig.Cond.Sync(context.Background()))
	assert.Equal(t, 1, bd.count("messages"), "a restored thread is not re-read every pass")
	assert.Equal(t, 1, rig.Cond.Stats().Restored)

	require.NoError(t, rig.Run.Stop("test"))
	_, err = rig.Run.Wait()
	require.NoError(t, err)
}

// A fresh run rebuilds nothing: Restore concludes the thread it replays, so
// a bead with no history must not go through it.
func TestGlueAFreshRunTracksRatherThanRestores(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "open", "add the retry"),
	)
	feedback := newGlueFeedback()
	rig := glueSetup(t, bd, ConductorOptions{Feedback: feedback}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))
	require.NoError(t, rig.Cond.Sync(context.Background()))

	tracked, restored, _ := feedback.snapshot()
	assert.Equal(t, []domain.BeadID{"sc-1"}, tracked)
	assert.Empty(t, restored)
	assert.Zero(t, bd.count("messages"), "a fresh run reads no comments")

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
}

// A resumed bead with no comments at all is adopted, not concluded: there is
// no thread to replay, and Restore would settle a bead that never left the
// state bd holds it in.
func TestGlueResumeTracksABeadWithNoComments(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "in_progress", "add the retry"),
	)
	feedback := newGlueFeedback()
	rig := glueSetup(t, bd, ConductorOptions{Feedback: feedback, Restore: glueYes()}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))
	require.NoError(t, rig.Cond.Sync(context.Background()))

	tracked, restored, _ := feedback.snapshot()
	assert.Contains(t, tracked, domain.BeadID("sc-1"))
	assert.Empty(t, restored)

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
}

// --- the prompt context (P8-18) ------------------------------------------

// TestGlueBeadContextCommitsLeftoversThroughDeliveryGit is P8-18: the
// production context provider commits D-14's leftovers through the
// DeliveryGit seam, and a worktree left mid-rebase is not committed at all.
func TestGlueBeadContextCommitsLeftoversThroughDeliveryGit(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-1", domain.BeadTypeTask, "in_progress", "add the retry"))
	git := &glueGit{diff: &gitx.Diff{Patch: "diff --git a/retry.go b/retry.go\n"}}
	rig := glueSetup(t, bd, ConductorOptions{
		Git: git, Brief: "make it retry", CheckCommand: "go test ./...",
		Repo: prompt.RepoContext{Root: "/repo", DefaultBranch: "main"},
	}, config.RunSettings{})

	bc, err := rig.Cond.BeadContext(context.Background(), "sc-1")
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"sc-1"}, git.committed(), "the leftovers are committed before the review (D-14)")
	assert.Equal(t, git.diff, bc.Diff)
	assert.Equal(t, "make it retry", bc.Brief)
	assert.Equal(t, "go test ./...", bc.CheckCommand)
	assert.Equal(t, "main", bc.Repo.DefaultBranch)
	assert.Empty(t, bc.Bead.Notes)

	// Mid-rebase: the adapter refuses, which is not a failure of the round.
	git.commitErr = ErrRebaseInProgress
	git.diffs = 0
	bc, err = rig.Cond.BeadContext(context.Background(), "sc-1")
	require.NoError(t, err, "a mid-rebase worktree still produces a prompt context")
	assert.Nil(t, bc.Diff, "mid-rebase HEAD is not the branch, so there is nothing to review")
	assert.Zero(t, git.diffs, "and nothing to diff")
	require.NotEmpty(t, bc.Bead.Notes)
	assert.Contains(t, bc.Bead.Notes[0], "middle of a rebase")

	// Any other git failure is still a failure.
	git.commitErr = errors.New("git is broken")
	_, err = rig.Cond.BeadContext(context.Background(), "sc-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "committing sc-1's leftovers")
}

func TestGlueBeadContextRefusesABeadBDDoesNotKnow(t *testing.T) {
	rig := glueSetup(t, newGlueBD(), ConductorOptions{}, config.RunSettings{})
	_, err := rig.Cond.BeadContext(context.Background(), "sc-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bd knows no bead sc-missing")
}

// --- the bead inventory and the graph (P6-32) ----------------------------

func TestGluePublishesABeadCreatedOutsideAPlan(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "open", "add the retry"),
	)
	rig := glueSetup(t, bd, ConductorOptions{}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))

	require.NoError(t, rig.Cond.Sync(context.Background()))
	require.NoError(t, rig.Cond.Sync(context.Background()))
	assert.Equal(t, 1, rig.Cond.Stats().Graphs, "an unchanged listing publishes no graph")

	// D-11's external-PR review bead: created by nothing the run planned.
	bd.add(glueIssue("sc-ext", domain.BeadTypeTask, "open", "review the outside PR"))
	require.NoError(t, rig.Cond.Sync(context.Background()))
	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
	require.NoError(t, rig.Log.Close())

	var created []domain.BeadID
	var graphs []events.GraphChanged
	evs, err := ReadEventLog(rig.Config.Run.StateDir)
	require.NoError(t, err)
	for _, ev := range evs {
		switch e := ev.(type) {
		case events.BeadCreated:
			created = append(created, e.Bead.ID)
		case events.GraphChanged:
			graphs = append(graphs, e)
		}
	}
	assert.Equal(t, []domain.BeadID{"sc-1", "sc-ext"}, created, "the epic is not work")
	require.Len(t, graphs, 2)
	assert.Equal(t, domain.BeadID("sc-epic"), graphs[1].Epic)
	require.Len(t, graphs[1].Beads, 2, "the graph snapshot is the run's work beads")

	summary := Summarise(evs, 0)
	_, ok := summary.Bead("sc-ext")
	assert.True(t, ok, "a bead created outside a plan reaches sc status (P6-32)")
}

// --- the bead source the run needs (P8-17) -------------------------------

// TestGlueGivesTheRunItsBeadSource is the other half of P8-17: assembling
// the glue is what wires Options.Beads, so a run built with Options.Leases
// and no listing cannot happen by accident. The D-07 half — a bead in
// review stays in review — is TestLifecycleStopKeepsABeadThatCannotTake...
func TestGlueGivesTheRunItsBeadSource(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "in_review", "add the retry"),
	)
	f := lifecycleSetup(t)
	leases := &lifecycleLeases{}
	opts := f.options()
	opts.SkipLock, opts.Leases = true, leases
	require.Nil(t, opts.Beads, "the assembler wired the leases and not the listing")

	rc := f.runConfig(t, config.RunSettings{})
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.Nil(t, r.BeadSource())

	_, err = NewConductor(ConductorOptions{Run: r, Beads: bd})
	require.NoError(t, err)
	src := r.BeadSource()
	require.NotNil(t, src, "assembling the glue gives the run its bead source")
	list, err := src(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, domain.BeadStateInReview, list[1].State)

	// And the run now sees the state bd holds, so a bead a tester was
	// reviewing keeps it.
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "eng-1", Role: domain.RoleEngineer,
		Bead: "sc-1", State: domain.AgentStateRunning}))
	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)
	assert.Empty(t, leases.ids(), "an in_review bead has no released edge out of it (D-07)")
}

func TestNewConductorNeedsARunAndBeads(t *testing.T) {
	_, err := NewConductor(ConductorOptions{Beads: newGlueBD()})
	require.Error(t, err)
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	_, err = NewConductor(ConductorOptions{Run: r})
	require.Error(t, err)
}

// --- failures are warnings, not the end of the run -----------------------

func TestGlueSyncFailuresWarnOnceAndCarryOn(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	bd.listErr = errors.New("bd is down")
	rig := glueSetup(t, bd, ConductorOptions{Feedback: newGlueFeedback()}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = rig.Cond.Run(ctx) }()
	glueEventually(t, "the glue to keep trying", func() bool { return rig.Cond.Stats().Failures >= 3 })
	cancel()
	<-done

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
	require.NoError(t, rig.Log.Close())

	warnings := 0
	evs, err := ReadEventLog(rig.Config.Run.StateDir)
	require.NoError(t, err)
	for _, ev := range evs {
		if w, ok := ev.(events.Warning); ok && w.Source == SourceConductor {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings, "a bd that stays down is one warning, not one per pass")
	require.Error(t, rig.Cond.Stats().LastError)
}

func TestGlueReadySetFailureStillDeliversTheApprovedBeads(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "approved", "add the retry"),
	)
	bd.readyErr = errors.New("bd ready failed")
	feedback := newGlueFeedback()
	delivery := &glueDelivery{deliver: func(req DeliveryRequest) (*DeliveryOutcome, error) {
		return &DeliveryOutcome{Bead: req.Bead.ID, Kind: DeliveryMerged, State: domain.BeadStateClosed}, nil
	}}
	rig := glueSetup(t, bd, ConductorOptions{Feedback: feedback, Delivery: delivery}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))

	err := rig.Cond.Sync(context.Background())
	require.Error(t, err, "the pass reports what it could not do")
	assert.Len(t, delivery.sent(), 1, "and still does the rest of it")

	require.NoError(t, rig.Run.Stop("test"))
	_, err = rig.Run.Wait()
	require.NoError(t, err)
}

// The conductor stops with the run even when nothing cancels its context.
func TestGlueRunStopsWithTheRun(t *testing.T) {
	rig := glueSetup(t, newGlueBD(), ConductorOptions{}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	done := make(chan error, 1)
	go func() { done <- rig.Cond.Run(context.Background()) }()
	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the conductor did not stop with the run")
	}
}

// --- the planning shim goes when the run does (P6-34) --------------------

func TestGlueFinishRemovesThePlanningShim(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	opts := f.options()
	opts.SkipLock = true
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	shim := filepath.Join(r.Dirs().Shims, PlanShimDirName)
	require.NoError(t, os.MkdirAll(shim, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(shim, "bd"), []byte("#!/bin/sh\n"), 0o700)) //nolint:gosec // a fixture shim.
	// A per-bead shim beside it belongs to the supervisor and must survive.
	perBead := filepath.Join(r.Dirs().Shims, "sc-1")
	require.NoError(t, os.MkdirAll(perBead, 0o700))

	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)

	_, err = os.Stat(shim)
	require.ErrorIs(t, err, os.ErrNotExist, "a finished run leaves no <run-dir>/bin/po-plan")
	_, err = os.Stat(perBead)
	require.NoError(t, err, "the per-bead shims are the supervisor's (P6-17)")
}
