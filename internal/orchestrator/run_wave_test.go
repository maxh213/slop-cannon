package orchestrator

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// Tests for the run-level round that follows P8-14: the graph events the
// conductor publishes (P6-45), the agent bus relay (P6-46), the resume
// handler the conductor hands the run (P6-54) and the queue its hand-offs
// run through (P6-55), the epic that no longer writes through the caller's
// configuration (P8-23), the coalesced state.json writes (P9-11) and the
// replay that gets a run's first events onto the screen (P9-28).
//
// Every helper here is prefixed "wave" so it cannot collide with another
// lane's file in this package.

// --- P6-45: the graph is snapshotted when its shape moves ----------------

// waveGraphBeads is how many work beads the P6-45 test's run has: enough
// that one snapshot of them is worth avoiding.
const waveGraphBeads = 100

// waveGraphChanges is how many single-field changes the test makes after
// the graph has settled. Each of them published a whole snapshot before
// P6-45.
const waveGraphChanges = 40

// TestGraphSnapshotIsPublishedOnlyWhenTheShapeMoves is P6-45. A hundred-bead
// run that then makes forty ordinary status changes used to write
// forty-one full graph snapshots into events.jsonl — every work bead, with
// its title, description and acceptance criteria, once per bd write of any
// kind. It now writes one snapshot and forty one-bead updates, which carry
// the same information (both are folded by [Summarise] and by the TUI) in a
// small fraction of the bytes.
func TestGraphSnapshotIsPublishedOnlyWhenTheShapeMoves(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	ids := make([]domain.BeadID, 0, waveGraphBeads)
	for i := range waveGraphBeads {
		id := domain.BeadID(fmt.Sprintf("sc-%03d", i))
		ids = append(ids, id)
		is := glueIssue(id, domain.BeadTypeTask, "open", "work bead "+string(id))
		is.Description = strings.Repeat("the description a planner wrote. ", 6)
		bd.add(is)
	}

	rig := glueSetup(t, bd, ConductorOptions{}, config.RunSettings{})
	ctx := context.Background()
	require.NoError(t, rig.Run.Start(ctx))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))

	// The first pass is the shape appearing: one snapshot.
	require.NoError(t, rig.Cond.Sync(ctx))
	require.Equal(t, 1, rig.Cond.Stats().Graphs)

	// Then forty ordinary changes, one bead each. None of them moves an
	// edge, so none of them is a change of shape.
	want := map[domain.BeadID]domain.BeadState{}
	for i := range waveGraphChanges {
		id := ids[i]
		state := domain.BeadStateInProgress
		if i%2 == 1 {
			state = domain.BeadStateInReview
		}
		bd.set(id, state)
		want[id] = state
		require.NoError(t, rig.Cond.Sync(ctx))
	}
	// A pass that changes nothing says nothing at all.
	require.NoError(t, rig.Cond.Sync(ctx))

	stats := rig.Cond.Stats()
	assert.Equal(t, 1, stats.Graphs, "the graph's shape moved once, so one snapshot")
	assert.Equal(t, waveGraphChanges, stats.Updates, "and one bead update per change")

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
	require.NoError(t, rig.Log.Close())

	evs, err := ReadEventLog(rig.Config.Run.StateDir)
	require.NoError(t, err)
	var (
		graphs  []events.GraphChanged
		updates []events.BeadUpdated
	)
	for _, ev := range evs {
		switch e := ev.(type) {
		case events.GraphChanged:
			graphs = append(graphs, e)
		case events.BeadUpdated:
			updates = append(updates, e)
		}
	}
	require.Len(t, graphs, 1, "one snapshot in the whole run log")
	assert.Len(t, graphs[0].Beads, waveGraphBeads, "and it is the whole work graph")
	assert.Equal(t, domain.BeadID("sc-epic"), graphs[0].Epic)
	require.Len(t, updates, waveGraphChanges)
	for _, u := range updates {
		assert.Equal(t, []string{"state"}, u.Fields, "%s moved one field", u.Bead.ID)
	}

	// Materially fewer bytes: the whole run log, inventory and all, is
	// smaller than the snapshots alone would have been.
	logBytes := waveFileSize(t, EventLogPath(rig.Config.Run.StateDir))
	snapshot := waveEventBytes(t, graphs[0])
	before := snapshot * (waveGraphChanges + 1)
	assert.Lessf(t, logBytes, before/4,
		"the run log is %d bytes; the graph snapshots alone were %d before P6-45", logBytes, before)

	// The same graph information: folding the log gives every bead the
	// state bd holds it in, with its title, from the snapshot plus the
	// per-bead updates.
	summary := Summarise(evs, 0)
	for _, id := range ids {
		b, ok := summary.Bead(id)
		require.Truef(t, ok, "%s is in the folded inventory", id)
		assert.Equalf(t, "work bead "+string(id), b.Title, "%s kept its title", id)
		state, changed := want[id]
		if !changed {
			state = domain.BeadStateOpen
		}
		assert.Equalf(t, state, b.State, "%s folded back to the state bd holds", id)
	}
}

// TestGraphSnapshotIsPublishedWhenAnEdgeOrHalfTheGraphMoves pins the two
// rules that still ask for a whole snapshot (P6-45): a dependency edge is
// shape, and a pass in which half the graph moved is cheaper as one
// snapshot than as a bead update each.
func TestGraphSnapshotIsPublishedWhenAnEdgeOrHalfTheGraphMoves(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "open", "one"),
		glueIssue("sc-2", domain.BeadTypeTask, "open", "two"),
		glueIssue("sc-3", domain.BeadTypeTask, "open", "three"),
		glueIssue("sc-4", domain.BeadTypeTask, "open", "four"),
	)
	rig := glueSetup(t, bd, ConductorOptions{}, config.RunSettings{})
	ctx := context.Background()
	require.NoError(t, rig.Run.Start(ctx))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))

	require.NoError(t, rig.Cond.Sync(ctx))
	require.Equal(t, 1, rig.Cond.Stats().Graphs, "the graph appeared")

	// One bead of four: an update, not a snapshot.
	bd.set("sc-1", domain.BeadStateInProgress)
	require.NoError(t, rig.Cond.Sync(ctx))
	assert.Equal(t, 1, rig.Cond.Stats().Graphs)
	assert.Equal(t, 1, rig.Cond.Stats().Updates)

	// Two of four: half the graph, so the snapshot is the smaller record.
	bd.set("sc-2", domain.BeadStateInProgress)
	bd.set("sc-3", domain.BeadStateInProgress)
	require.NoError(t, rig.Cond.Sync(ctx))
	assert.Equal(t, 2, rig.Cond.Stats().Graphs, "half the graph moved")
	assert.Equal(t, 1, rig.Cond.Stats().Updates)

	// A new edge is shape, however few beads it touches.
	waveDepend(bd, "sc-4", "sc-1")
	require.NoError(t, rig.Cond.Sync(ctx))
	assert.Equal(t, 3, rig.Cond.Stats().Graphs, "a dependency is the graph's shape")

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
}

// --- P6-46: the agent bus relay cannot drop a burst ----------------------

// waveBurst is how many events the P6-46 test publishes through the
// forwarding bus: five times [DefaultAgentBusBuffer], so the single
// forwarder the buffer used to feed could not have kept up with it.
const waveBurst = 5 * DefaultAgentBusBuffer

// TestAgentBusForwardsABurstWithoutDroppingALine is P6-46. Everything an
// agent prints goes through [Run.AgentBus], and the one forwarder
// republished each line through [Run.Publish], which writes the run log —
// so a producer faster than the disk filled the 8192-event subscription and
// everything past that was thrown away, counted and warned about at the
// end. The relay now moves events off the subscription into an unbounded
// queue, so a burst is absorbed rather than lost.
func TestAgentBusForwardsABurstWithoutDroppingALine(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	// The run log is what makes the republish slow, which is the whole
	// point: the relay has to outrun a producer that a file cannot.
	rc := f.runConfig(t, config.RunSettings{})
	log, err := TeeRunLog(&opts, rc.Run.StateDir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	bus := r.AgentBus()
	meta := r.Meta()
	for i := range waveBurst {
		bus.Publish(events.AgentOutput{
			Meta: meta, Agent: "eng-sc-1", Stream: "stdout",
			Line: fmt.Sprintf("line %d of a very chatty agent", i),
		})
		if i%256 == 0 {
			// A real producer reads a pipe and yields constantly; this is
			// the only concession the burst makes to it.
			runtime.Gosched()
		}
	}

	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.NoError(t, log.Close())

	assert.Equal(t, uint64(0), bus.Stats().Dropped, "the relay kept the subscription drained")

	evs, err := ReadEventLog(rc.Run.StateDir)
	require.NoError(t, err)
	lines := 0
	for _, ev := range evs {
		if _, ok := ev.(events.AgentOutput); ok {
			lines++
		}
	}
	assert.Equal(t, waveBurst, lines, "every line the agent printed is in the run log")
	assert.Equal(t, int(r.State().LastSeq), len(evs),
		"and state.json still counts exactly what the log holds")
	for _, ev := range evs {
		w, ok := ev.(events.Warning)
		require.Falsef(t, ok && strings.Contains(w.Message, "were dropped"),
			"nothing was dropped: %q", w.Message)
	}
}

// --- P6-54 / P6-55: the resume handler and the queue it runs through -----

// waveDelivery is a delivery handoff that is also a [ResumeHandler], which
// is what *Deliverer is. It records what it was asked to do and can be told
// to fail, block or take its time.
type waveDelivery struct {
	mu       sync.Mutex
	calls    []string
	failures map[string]int
	// hold, when set, is waited on by every hand-off, so a test can sample
	// what runs at the same time as what.
	hold chan struct{}
	// live is how many hand-offs are running now, and peak the most that
	// ever were at once.
	live, peak int
	// dwell is how long each hand-off takes, so an overlap has time to be
	// noticed.
	dwell time.Duration
}

var (
	_ ConductorDelivery = (*waveDelivery)(nil)
	_ ResumeHandler     = (*waveDelivery)(nil)
)

func newWaveDelivery() *waveDelivery {
	return &waveDelivery{failures: map[string]int{}, dwell: 2 * time.Millisecond}
}

func (d *waveDelivery) Deliver(ctx context.Context, req DeliveryRequest) (*DeliveryOutcome, error) {
	d.enter("deliver:" + string(req.Bead.ID))
	defer d.leave()
	d.wait(ctx)
	return nil, nil
}

func (d *waveDelivery) Resolved(context.Context, domain.BeadID) (*DeliveryOutcome, error) {
	return nil, nil
}

func (d *waveDelivery) RedeliverPR(ctx context.Context, e gitx.ResumeEntry) error {
	return d.handoff(ctx, "redeliver_pr:"+string(e.Bead.ID))
}

func (d *waveDelivery) Redeliver(ctx context.Context, e gitx.ResumeEntry) error {
	return d.handoff(ctx, "redeliver:"+string(e.Bead.ID))
}

func (d *waveDelivery) ConflictRound(ctx context.Context, e gitx.ResumeEntry) error {
	return d.handoff(ctx, "conflict:"+string(e.Bead.ID))
}

func (d *waveDelivery) handoff(ctx context.Context, what string) error {
	d.enter(what)
	defer d.leave()
	d.wait(ctx)
	d.mu.Lock()
	left := d.failures[what]
	if left > 0 {
		d.failures[what] = left - 1
	}
	d.mu.Unlock()
	if left > 0 {
		return fmt.Errorf("gh could not be reached (%d left)", left)
	}
	return nil
}

func (d *waveDelivery) enter(what string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, what)
	d.live++
	if d.live > d.peak {
		d.peak = d.live
	}
}

func (d *waveDelivery) leave() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.live--
}

func (d *waveDelivery) wait(ctx context.Context) {
	d.mu.Lock()
	hold, dwell := d.hold, d.dwell
	d.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	if dwell > 0 {
		time.Sleep(dwell)
	}
}

func (d *waveDelivery) made() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *waveDelivery) at() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak
}

func (d *waveDelivery) fail(what string, times int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures[what] = times
}

// waveResumer is a [GitResumer] that reports what a crash left behind.
type waveResumer struct{ res *gitx.ResumeResult }

func (r waveResumer) Resume(context.Context, []domain.Bead) (*gitx.ResumeResult, error) {
	return r.res, nil
}

// TestConductorAttachGivesTheRunItsResumeHandler is P6-54: a run assembled
// without an explicit [Options.OnResume] still acts on what a resume found,
// because the conductor is handed both the run and the delivery handoff and
// so is the one part of the assembly that can wire them together. Before
// this both P6-15 hand-offs were dead in the shipped binary for a whole
// phase and nothing failed.
func TestConductorAttachGivesTheRunItsResumeHandler(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-pr", domain.BeadTypeTask, "delivering", "its pull request is open"),
		glueIssue("sc-conflict", domain.BeadTypeTask, "in_progress", "left mid-rebase"),
	)
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Resume: true, ID: "20260903-120000-abcd"})
	opts := f.options()
	opts.SkipLock = true
	require.Nil(t, opts.OnResume, "the assembler forgot the resume handler, as `sc run` did for a phase")
	opts.Resumer = waveResumer{res: &gitx.ResumeResult{Entries: []gitx.ResumeEntry{
		{Bead: domain.Bead{ID: "sc-pr", State: domain.BeadStateDelivering},
			State: domain.BeadStateDelivering, PRState: "OPEN",
			PR: &gitx.PullRequest{Number: 7, URL: "https://example.invalid/pull/7"}},
		{Bead: domain.Bead{ID: "sc-conflict", State: domain.BeadStateInProgress},
			State: domain.BeadStateInProgress, Conflict: true},
	}}}

	r, err := New(rc, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.Nil(t, r.ResumeHandler(), "nothing has wired one yet")

	cond, err := NewConductor(ConductorOptions{Run: r, Beads: bd, Interval: 5 * time.Millisecond,
		DeliveryBackoff: time.Millisecond})
	require.NoError(t, err)
	require.NotNil(t, r.ResumeQueue(), "building the glue routes the hand-offs through its queue (P6-55)")

	delivery := newWaveDelivery()
	cond.Attach(ConductorParts{Delivery: delivery})
	require.NotNil(t, r.ResumeHandler(), "attaching the delivery handoff gives the run its resume handler")

	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"redeliver_pr:sc-pr", "conflict:sc-conflict"}, delivery.made(),
		"a resumed pull request is re-delivered and a mid-rebase worktree goes back to its engineer")
}

// TestConductorAttachKeepsAnExplicitResumeHandler is the other half of
// P6-54: [Run.UseResumeHandler] fills a gap, it does not overwrite. An
// assembly that passes its own handler — `sc run` does, since P9-03 — keeps
// the one it passed, and a test can still assert on its type.
func TestConductorAttachKeepsAnExplicitResumeHandler(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	mine := DeliveryResumeHandler{Deliverer: func() *Deliverer { return nil }}
	opts.OnResume = mine

	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	cond, err := NewConductor(ConductorOptions{Run: r, Beads: bd})
	require.NoError(t, err)
	cond.Attach(ConductorParts{Delivery: newWaveDelivery()})

	got, ok := r.ResumeHandler().(DeliveryResumeHandler)
	require.True(t, ok, "the explicit handler is still the run's: %T", r.ResumeHandler())
	assert.NotNil(t, got.Deliverer)
}

// TestResumeDeliveriesAreSerialisedByTheConductorsQueue is P6-55's first
// half: [Deliverer.Redeliver] and [Deliverer.RedeliverPR] rebase and
// fast-forward the same default branch an ordinary delivery does, so two
// beads reconciled at resume time must not do it at once — nor at the same
// time as a delivery the conductor is already making.
func TestResumeDeliveriesAreSerialisedByTheConductorsQueue(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	rig := glueSetup(t, bd, ConductorOptions{}, config.RunSettings{})
	delivery := newWaveDelivery()
	delivery.dwell = 20 * time.Millisecond
	rig.Cond.Attach(ConductorParts{Delivery: delivery})
	ctx := context.Background()
	require.NoError(t, rig.Run.Start(ctx))

	var wg sync.WaitGroup
	for _, id := range []domain.BeadID{"sc-1", "sc-2", "sc-3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bead := domain.Bead{ID: id, State: domain.BeadStateDelivering}
			assert.NoError(t, rig.Cond.QueueResume(ctx, bead, "the pull request re-delivery",
				func(ctx context.Context) error {
					return delivery.RedeliverPR(ctx, gitx.ResumeEntry{Bead: bead, PRState: "OPEN"})
				}))
		}()
	}
	// And an ordinary delivery of a fourth bead at the same time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		assert.NoError(t, rig.Cond.Deliver(ctx, domain.Bead{ID: "sc-4", State: domain.BeadStateApproved}))
	}()
	wg.Wait()
	rig.Cond.WaitDeliveries()

	assert.Equal(t, 1, delivery.at(),
		"one repository-mutating hand-off at a time, resume or not: %v", delivery.made())
	assert.Len(t, delivery.made(), 4)

	require.NoError(t, rig.Run.Stop("test"))
	_, err := rig.Run.Wait()
	require.NoError(t, err)
}

// TestAResumeDeliveryThatFailsIsRetried is P6-55's second half: a hand-off
// that failed on a `gh` that could not be reached was stranded, because it
// was called on the deliverer directly and nothing owed it another attempt.
// The queue retries it on the conductor's backoff; the resume itself is not
// held open for the retries, so [Run.Start] still returns.
func TestAResumeDeliveryThatFailsIsRetried(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	rig := glueSetup(t, bd, ConductorOptions{}, config.RunSettings{})
	delivery := newWaveDelivery()
	delivery.dwell = 0
	delivery.fail("redeliver:sc-1", 2)
	rig.Cond.Attach(ConductorParts{Delivery: delivery})
	ctx := context.Background()
	require.NoError(t, rig.Run.Start(ctx))

	bead := domain.Bead{ID: "sc-1", State: domain.BeadStateDelivering}
	err := rig.Cond.QueueResume(ctx, bead, "the interrupted delivery", func(ctx context.Context) error {
		return delivery.Redeliver(ctx, gitx.ResumeEntry{Bead: bead})
	})
	require.Error(t, err, "the resume is told about the first attempt rather than waiting out the backoff")

	rig.Cond.WaitDeliveries()
	assert.Equal(t, []string{"redeliver:sc-1", "redeliver:sc-1", "redeliver:sc-1"}, delivery.made(),
		"two failures and the attempt that worked")

	require.NoError(t, rig.Run.Stop("test"))
	_, err = rig.Run.Wait()
	require.NoError(t, err)
}

// TestAResumeWithoutAQueueStillActsOnWhatItFound keeps the fallback honest:
// a run assembled without a conductor calls its handler directly, which is
// what every resume did before P6-55.
func TestAResumeWithoutAQueueStillActsOnWhatItFound(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	delivery := newWaveDelivery()
	opts.OnResume = delivery
	opts.Resumer = waveResumer{res: &gitx.ResumeResult{Entries: []gitx.ResumeEntry{
		{Bead: domain.Bead{ID: "sc-1", State: domain.BeadStateDelivering},
			State: domain.BeadStateDelivering},
	}}}

	r, err := New(f.runConfig(t, config.RunSettings{Resume: true, ID: "20260903-120000-abcd"}), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.Nil(t, r.ResumeQueue())

	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)
	assert.Equal(t, []string{"redeliver:sc-1"}, delivery.made())
}

// --- P8-23: SetEpic no longer writes through the caller's config ---------

// TestSetEpicDoesNotMutateTheCallersRunConfig is P8-23. The planner records
// the run's epic the moment the first plan lands, and until now that wrote
// r.cfg.Run.Epic — the same struct the command layer resolved and still
// holds — so every reader of rc.Run after Start raced with it under -race.
func TestSetEpicDoesNotMutateTheCallersRunConfig(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	rc := f.runConfig(t, config.RunSettings{})
	r, err := New(rc, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background()))

	// The reader is the command layer's: it holds the *config.RunConfig it
	// resolved and reads it while the run is going, without copying first.
	stop := make(chan struct{})
	var reads atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = rc.Run.Epic
			_ = rc.Run.Mode
			_ = rc.Run.Brief
			_ = rc.Run.Config.Ammo
			reads.Add(1)
		}
	}()

	for i := range 200 {
		require.NoError(t, r.SetEpic(domain.BeadID(fmt.Sprintf("sc-epic-%d", i))))
	}
	close(stop)
	<-done

	assert.Positive(t, reads.Load(), "the reader really ran")
	assert.Empty(t, rc.Run.Epic, "the run keeps its own copy of domain.Run")
	assert.Equal(t, domain.BeadID("sc-epic-199"), r.Epic(), "and its own is current")
	require.NoError(t, r.FlushState())
	assert.Equal(t, domain.BeadID("sc-epic-199"), lifecycleRead(t, r).Run.Epic,
		"state.json carries it, which is where a resume reads it back")

	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)
}

// --- P9-11: one state.json flush per dispatch ----------------------------

// TestAgentBookkeepingIsCoalescedIntoOneWrite is P9-11. A cold tick fires
// every slot in one dispatch, and each [Run.AgentStarted] used to marshal
// and write the whole run state — a list one agent longer each time, two
// thirds of the dispatch by profile and quadratic in the ammo. state.json
// is a crash-recovery record, not a transaction log.
func TestAgentBookkeepingIsCoalescedIntoOneWrite(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background()))

	const dispatch = 50
	for i := range dispatch {
		require.NoError(t, r.AgentStarted(domain.Agent{
			ID: domain.AgentID(fmt.Sprintf("eng-%02d", i)), Role: domain.RoleEngineer,
			Bead: domain.BeadID(fmt.Sprintf("sc-%02d", i)), State: domain.AgentStateRunning,
		}))
	}
	assert.Len(t, r.Agents(), dispatch, "the run knows them all at once")
	assert.Less(t, len(lifecycleRead(t, r).Agents), dispatch,
		"and did not write the file once per agent")

	// The flush catches up on its own, without anybody asking.
	glueEventually(t, "the coalesced state write to land", func() bool {
		return len(lifecycleRead(t, r).Agents) == dispatch
	})

	// And a transition is still synchronous: nothing is left owing when
	// the run ends.
	for i := range dispatch {
		require.NoError(t, r.AgentStopped(domain.AgentID(fmt.Sprintf("eng-%02d", i))))
	}
	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)
	st := lifecycleRead(t, r)
	assert.Empty(t, st.Agents)
	assert.Equal(t, PhaseFinished, st.Phase)
}

// --- P9-28: the run's first events reach a late subscriber ---------------

// TestTheRunsFirstEventsReachALateSubscriber is P9-28. `sc run` starts the
// run and only then renders it, because the renderer wants the header
// [Run.Start] settles, and [events.Bus] does not replay — so everything
// published in the run's first milliseconds reached sc.log, events.jsonl,
// `sc logs` and `sc status`, and never the user's screen. The one that
// matters is P9-01's, because it is about money.
func TestTheRunsFirstEventsReachALateSubscriber(t *testing.T) {
	f := lifecycleSetup(t)
	// A bus of this test's own: the fixture's has a subscriber already,
	// and the whole question is what happens when it has none.
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	opts := f.options()
	opts.Bus, opts.SkipLock = nil, true
	rc := f.runConfig(t, config.RunSettings{})
	log, err := TeeRunLog(&opts, rc.Run.StateDir, bus)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	r, err := New(rc, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.Start(context.Background()))
	// The budget announces the headroom a resumed run was granted on the
	// first dispatch, which is inside this window (P9-01).
	const budget = "the run was resumed over its budget: it will pause again at 2000 tokens"
	r.Publish(events.Warning{Meta: r.Meta(), Source: SourceLimits, Message: budget})
	assert.Equal(t, 0, bus.Stats().Subscribers, "nothing is rendering yet")

	// Now the renderer attaches, exactly as `sc run` does after Start.
	sub := bus.Subscribe(context.Background(), 64)
	t.Cleanup(sub.Unsubscribe)

	require.NoError(t, r.Stop("test"))
	_, err = r.Wait()
	require.NoError(t, err)

	var (
		kinds    []events.Kind
		warnings []string
	)
	for {
		select {
		case ev := <-sub.Events():
			kinds = append(kinds, ev.Kind())
			if w, ok := ev.(events.Warning); ok {
				warnings = append(warnings, w.Message)
			}
			continue
		default:
		}
		break
	}
	assert.Contains(t, warnings, budget, "the budget notice reached the screen")
	assert.Equal(t, events.KindRunStarted, kinds[0], "in order, starting with the run itself")
	assert.Equal(t, events.KindRunFinished, kinds[len(kinds)-1])

	// Once and only once: the run log has each of them exactly one time,
	// and the replay did not double anything up.
	require.NoError(t, log.Close())
	evs, err := ReadEventLog(rc.Run.StateDir)
	require.NoError(t, err)
	assert.Len(t, evs, len(kinds), "the screen saw the whole log")
	seen := 0
	for _, ev := range evs {
		if w, ok := ev.(events.Warning); ok && w.Message == budget {
			seen++
		}
	}
	assert.Equal(t, 1, seen)
}

// TestTheStartupReplayIsBoundedAndCountsWhatItCouldNotHold pins the
// buffer's edges: it holds at most its limit and says how many it had to
// let go, and a publisher that cannot say whether anyone is listening is
// never wrapped at all.
func TestTheStartupReplayIsBoundedAndCountsWhatItCouldNotHold(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	p := NewStartupReplay(bus, 2)
	replay, ok := p.(*StartupReplay)
	require.True(t, ok, "an events.Bus can say whether anyone is listening: %T", p)

	meta := events.MetaAt("20260903-120000-abcd", time.Unix(0, 0).UTC())
	for i := range 5 {
		p.Publish(events.Warning{Meta: meta, Source: SourceRun, Message: fmt.Sprintf("held %d", i)})
	}
	held, dropped := replay.Held()
	assert.Equal(t, 2, held)
	assert.Equal(t, 3, dropped)

	sub := bus.Subscribe(context.Background(), 8)
	t.Cleanup(sub.Unsubscribe)
	p.Publish(events.Warning{Meta: meta, Source: SourceRun, Message: "live"})
	var msgs []string
	for range 3 {
		w, isWarning := (<-sub.Events()).(events.Warning)
		require.True(t, isWarning)
		msgs = append(msgs, w.Message)
	}
	assert.Equal(t, []string{"held 0", "held 1", "live"}, msgs, "the held ones come first, in order")
	held, dropped = replay.Held()
	assert.Equal(t, 0, held)
	assert.Equal(t, 3, dropped, "what was let go is still countable")

	// A sink that is always listening is handed back unwrapped.
	sink := PublisherFunc(func(events.Event) {})
	assert.NotNil(t, NewStartupReplay(sink, 0))
	_, wrapped := NewStartupReplay(sink, 0).(*StartupReplay)
	assert.False(t, wrapped, "nothing to wait for")
	assert.Nil(t, NewStartupReplay(nil, 0))
}

// --- helpers -------------------------------------------------------------

// waveFileSize is how many bytes a file holds.
func waveFileSize(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return int(info.Size())
}

// waveEventBytes is how many bytes one event takes as a run log line.
func waveEventBytes(t *testing.T, ev events.Event) int {
	t.Helper()
	b, err := events.Marshal(ev)
	require.NoError(t, err)
	return len(b) + 1
}

// waveDepend adds a "blocks" edge to the in-memory bd: id depends on
// blocker, which is the direction D-20 and `bd dep add <issue>
// <depends-on>` use.
func waveDepend(bd *glueBD, id, blocker domain.BeadID) {
	bd.mu.Lock()
	defer bd.mu.Unlock()
	for i := range bd.issues {
		if bd.issues[i].ID == id {
			bd.issues[i].BlockedBy = append(bd.issues[i].BlockedBy, blocker)
			return
		}
	}
}
