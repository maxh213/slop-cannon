package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
)

// deliveryClock is the fixed instant every delivery test stamps its events
// with; nothing here reads the wall clock.
var deliveryClock = time.Date(2026, 9, 3, 9, 30, 0, 0, time.UTC)

// deliveryBead is the bead under delivery in most of these tests.
func deliveryBead(state domain.BeadState) domain.Bead {
	return domain.Bead{
		ID:       "sc-68s",
		Title:    "Add retry to the uploader",
		State:    state,
		Assignee: "eng-sc-68s-r1",
		Round:    1,
	}
}

// deliveryHostFake is the [DeliveryHost] the tests run against: a fixed
// clock, a discarding logger and a recorded event stream.
type deliveryHostFake struct {
	run      domain.RunID
	snapshot domain.ConfigSnapshot
	log      *slog.Logger

	mu     sync.Mutex
	events []events.Event
}

// newDeliveryHost returns a host for a run with the given safety setting.
func newDeliveryHost(safety bool) *deliveryHostFake {
	return &deliveryHostFake{
		run:      "run-2026-09-03",
		snapshot: domain.ConfigSnapshot{Ammo: 10, Safety: safety, Cartridge: "mock"},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (h *deliveryHostFake) ID() domain.RunID                { return h.run }
func (h *deliveryHostFake) Meta() events.Meta               { return events.MetaAt(h.run, deliveryClock) }
func (h *deliveryHostFake) Now() time.Time                  { return deliveryClock }
func (h *deliveryHostFake) Log() *slog.Logger               { return h.log }
func (h *deliveryHostFake) Snapshot() domain.ConfigSnapshot { return h.snapshot }
func (h *deliveryHostFake) Publish(ev events.Event) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

// published returns every event of one kind.
func (h *deliveryHostFake) published(kind events.Kind) []events.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []events.Event
	for _, ev := range h.events {
		if ev.Kind() == kind {
			out = append(out, ev)
		}
	}
	return out
}

// states returns the (from → to) pairs of every BeadStateChanged, in order.
func (h *deliveryHostFake) states() []string {
	var out []string
	for _, ev := range h.published(events.KindBeadStateChanged) {
		c, ok := ev.(events.BeadStateChanged)
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s→%s", c.From, c.To))
	}
	return out
}

// warnings returns the messages of every Warning the delivery published.
func (h *deliveryHostFake) warnings() []string {
	var out []string
	for _, ev := range h.published(events.KindWarning) {
		w, ok := ev.(events.Warning)
		if !ok {
			continue
		}
		out = append(out, w.Message)
	}
	return out
}

// deliveryStoreFake is the [StateStore] the state machine writes through. It
// records every call so a test can assert the exact bd writes a delivery
// made.
type deliveryStoreFake struct {
	mu     sync.Mutex
	calls  []string
	closes []string
	err    error
	failOn string
}

func (s *deliveryStoreFake) record(call string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	if s.failOn != "" && strings.HasPrefix(call, s.failOn) {
		return s.err
	}
	return nil
}

func (s *deliveryStoreFake) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	err := s.record("status:" + string(state))
	return beads.Issue{ID: id, Status: string(state)}, err
}

func (s *deliveryStoreFake) ClaimFor(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	err := s.record("claim:" + string(agent))
	return beads.Issue{ID: id, Assignee: string(agent)}, err
}

func (s *deliveryStoreFake) Reclaim(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	err := s.record("reclaim:" + string(agent))
	return beads.Issue{ID: id, Assignee: string(agent)}, err
}

func (s *deliveryStoreFake) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	err := s.record("release")
	return beads.Issue{ID: id}, err
}

func (s *deliveryStoreFake) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	err := s.record("close")
	s.mu.Lock()
	s.closes = append(s.closes, reason)
	s.mu.Unlock()
	return beads.Issue{ID: id, Status: "closed"}, err
}

// failAfter makes every later call whose name starts with prefix fail with
// err, so a test can let a delivery get part way and then take bd away.
func (s *deliveryStoreFake) failAfter(prefix string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failOn, s.err = prefix, err
}

// recorded returns the calls made so far.
func (s *deliveryStoreFake) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// deliveryBeadsFake is the [gitx.BeadCloser] the merge path is handed. It
// records what the delivery let gitx do in bd.
type deliveryBeadsFake struct {
	mu       sync.Mutex
	updates  []beads.Patch
	closes   int
	mergeSHA string
}

func (b *deliveryBeadsFake) Update(_ context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updates = append(b.updates, p)
	return beads.Issue{ID: id}, nil
}

func (b *deliveryBeadsFake) Close(_ context.Context, id domain.BeadID, _ string) (beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes++
	return beads.Issue{ID: id}, nil
}

func (b *deliveryBeadsFake) Show(_ context.Context, ids ...domain.BeadID) ([]beads.Issue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]beads.Issue, 0, len(ids))
	for _, id := range ids {
		out = append(out, beads.Issue{ID: id, Metadata: beads.Metadata{beads.MetaMergeSHA: b.mergeSHA}})
	}
	return out, nil
}

// deliveryPRFake is the safety-on path.
type deliveryPRFake struct {
	results []*gitx.PRResult
	err     error

	mu    sync.Mutex
	calls []gitx.PRRequest
}

func (p *deliveryPRFake) Deliver(_ context.Context, req gitx.PRRequest) (*gitx.PRResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if p.err != nil {
		return nil, p.err
	}
	i := min(len(p.calls)-1, len(p.results)-1)
	return p.results[i], nil
}

// deliveryMergeFake is the safety-off path. Each call consumes the next
// scripted answer; the last one repeats.
type deliveryMergeFake struct {
	results []*gitx.MergeResult
	errs    []error

	mu    sync.Mutex
	calls []gitx.MergeOptions
}

func (m *deliveryMergeFake) DeliverMerge(ctx context.Context, bead domain.Bead, opts gitx.MergeOptions) (*gitx.MergeResult, error) {
	m.mu.Lock()
	i := len(m.calls)
	m.calls = append(m.calls, opts)
	m.mu.Unlock()
	var err error
	if i < len(m.errs) {
		err = m.errs[i]
	}
	var res *gitx.MergeResult
	if len(m.results) > 0 {
		res = m.results[min(i, len(m.results)-1)]
	}
	if err != nil {
		// gitx.Manager.DeliverMerge returns nil on every error path, so
		// the fake must too: an outcome may only describe a merge that
		// happened.
		return nil, err
	}
	// The real merge records sc_merge_sha and would close; the delivery
	// withholds the close (see Deliverer.mergeBeads).
	if opts.Beads != nil && res != nil {
		if _, uerr := opts.Beads.Update(ctx, bead.ID, beads.Patch{Metadata: beads.MergeMetadata(res.Head)}); uerr != nil {
			return nil, uerr
		}
		if _, cerr := opts.Beads.Close(ctx, bead.ID, "merged"); cerr != nil {
			return nil, cerr
		}
	}
	return res, nil
}

// deliveryGitFake is the worktree side of a delivery.
type deliveryGitFake struct {
	branch    string
	rebase    []RebaseState
	rebaseErr error
	// rebasePartial is what RebaseState reports alongside rebaseErr, the
	// way managerGit does when git says a rebase is in progress but
	// listing the unmerged paths fails.
	rebasePartial RebaseState
	leftovers     *gitx.Leftovers
	leftErr       error
	diffs         []*gitx.Diff
	diffErr       error
	ratio         gitx.ChangeRatio
	ratioErr      error
	branchErr     error

	mu         sync.Mutex
	rebaseN    int
	diffN      int
	leftoverN  int
	leftRounds []int
}

func (g *deliveryGitFake) DefaultBranch(context.Context) (string, error) {
	if g.branchErr != nil {
		return "", g.branchErr
	}
	if g.branch == "" {
		return "main", nil
	}
	return g.branch, nil
}

func (g *deliveryGitFake) RebaseState(context.Context, domain.BeadID) (RebaseState, error) {
	if g.rebaseErr != nil {
		return g.rebasePartial, g.rebaseErr
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.rebaseN
	g.rebaseN++
	if i < len(g.rebase) {
		return g.rebase[i], nil
	}
	return RebaseState{}, nil
}

func (g *deliveryGitFake) CommitLeftovers(_ context.Context, _ domain.Bead, round int) (*gitx.Leftovers, error) {
	g.mu.Lock()
	g.leftoverN++
	g.leftRounds = append(g.leftRounds, round)
	g.mu.Unlock()
	if g.leftErr != nil {
		return nil, g.leftErr
	}
	if g.leftovers != nil {
		return g.leftovers, nil
	}
	return &gitx.Leftovers{}, nil
}

func (g *deliveryGitFake) Diff(context.Context, domain.BeadID) (*gitx.Diff, error) {
	if g.diffErr != nil {
		return nil, g.diffErr
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.diffN
	g.diffN++
	if i < len(g.diffs) {
		return g.diffs[i], nil
	}
	if len(g.diffs) > 0 {
		return g.diffs[len(g.diffs)-1], nil
	}
	return &gitx.Diff{Stats: gitx.DiffStats{Head: "aaaaaaa", Insertions: 10}}, nil
}

func (g *deliveryGitFake) ChangeRatio(context.Context, domain.BeadID, *gitx.Diff, *gitx.Diff) (gitx.ChangeRatio, error) {
	if g.ratioErr != nil {
		return gitx.ChangeRatio{}, g.ratioErr
	}
	return g.ratio, nil
}

// leftoverCalls is how many times leftovers were committed.
func (g *deliveryGitFake) leftoverCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.leftoverN
}

// deliveryHarness is a deliverer with every fake it was built from.
type deliveryHarness struct {
	*Deliverer
	host  *deliveryHostFake
	store *deliveryStoreFake
	pr    *deliveryPRFake
	merge *deliveryMergeFake
	git   *deliveryGitFake
	beads *deliveryBeadsFake
}

// newDeliveryHarness builds a deliverer for a run with the given safety
// setting; opts customises the options before construction.
func newDeliveryHarness(t *testing.T, safety bool, opts ...func(*DeliveryOptions, *deliveryHarness)) *deliveryHarness {
	t.Helper()
	h := &deliveryHarness{
		host:  newDeliveryHost(safety),
		store: &deliveryStoreFake{},
		pr:    &deliveryPRFake{results: []*gitx.PRResult{deliveryPRCreated()}},
		merge: &deliveryMergeFake{results: []*gitx.MergeResult{deliveryMerged()}},
		git:   &deliveryGitFake{},
		beads: &deliveryBeadsFake{},
	}
	o := DeliveryOptions{
		Host:  h.host,
		PR:    h.pr,
		Merge: h.merge,
		Git:   h.git,
		Beads: h.beads,
	}
	for _, fn := range opts {
		fn(&o, h)
	}
	if o.States == nil {
		o.States = NewStateMachine(h.store,
			WithStatePublisher(PublisherFunc(h.host.Publish)),
			WithStateRun(h.host.ID()),
			WithStateClock(h.host.Now))
	}
	d, err := NewDeliverer(o)
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	h.Deliverer = d
	return h
}

// deliveryPRCreated is a successful safety-on delivery.
func deliveryPRCreated() *gitx.PRResult {
	return &gitx.PRResult{
		Status:  gitx.PRCreated,
		Bead:    "sc-68s",
		Branch:  "sc/sc-68s-add-retry",
		Base:    "main",
		Remote:  "origin",
		Pushed:  true,
		PR:      &gitx.PullRequest{Number: 7, URL: "https://example.test/pr/7"},
		PRState: "OPEN",
		Gate:    "sc-gate-1",
		State:   domain.BeadStateDelivering,
	}
}

// deliveryMerged is a successful safety-off delivery.
func deliveryMerged() *gitx.MergeResult {
	return &gitx.MergeResult{
		BeadID:  "sc-68s",
		Branch:  "sc/sc-68s-add-retry",
		Target:  "main",
		Remote:  "origin",
		Before:  "0000000",
		Head:    "abcdef1234567",
		Commits: 2,
		Pushed:  true,
	}
}

// deliveryConflictErr is the error a rebase that stopped on conflicts
// produces.
func deliveryConflictErr(files ...string) error {
	return &gitx.DeliveryError{
		Stage:    gitx.StageRebase,
		Conflict: true,
		Files:    files,
		Output:   "CONFLICT (content): Merge conflict in " + strings.Join(files, ", "),
		Err:      gitx.ErrRebaseConflict,
	}
}

// TestDelivererShipsAnApprovedBead covers the success case with both safety
// values: safety on leaves the bead in delivering behind its gate (D-20),
// safety off closes it with the merge recorded.
func TestDelivererShipsAnApprovedBead(t *testing.T) {
	t.Parallel()
	t.Run("safety on opens a pull request and waits for the gate", func(t *testing.T) {
		t.Parallel()
		h := newDeliveryHarness(t, true)
		out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if out.Kind != DeliveryPending {
			t.Fatalf("kind = %s, want %s (%s)", out.Kind, DeliveryPending, out.Summary())
		}
		if out.State != domain.BeadStateDelivering {
			t.Errorf("state = %s, want delivering", out.State)
		}
		if out.Gate != "sc-gate-1" {
			t.Errorf("gate = %q, want sc-gate-1", out.Gate)
		}
		if got, want := h.store.recorded(), []string{"status:delivering"}; !deliveryEqualStrings(got, want) {
			t.Errorf("bd writes = %v, want %v (the gate closes the bead later, D-20)", got, want)
		}
		opened := h.host.published(events.KindPROpened)
		if len(opened) != 1 {
			t.Fatalf("PROpened events = %d, want 1", len(opened))
		}
		pr, ok := opened[0].(events.PROpened)
		if !ok {
			t.Fatalf("PROpened event = %T", opened[0])
		}
		if pr.Number != 7 || pr.Branch != "sc/sc-68s-add-retry" {
			t.Errorf("PROpened = %+v, want #7 on the bead branch", pr)
		}
		if n := len(h.host.published(events.KindMerged)); n != 0 {
			t.Errorf("Merged events = %d, want 0 with safety on", n)
		}
	})

	t.Run("safety off merges and closes the bead", func(t *testing.T) {
		t.Parallel()
		h := newDeliveryHarness(t, false)
		out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if out.Kind != DeliveryMerged || out.State != domain.BeadStateClosed {
			t.Fatalf("outcome = %s/%s, want merged/closed (%s)", out.Kind, out.State, out.Summary())
		}
		if out.MergeSHA != "abcdef1234567" || !out.Pushed {
			t.Errorf("merge = %q pushed=%v, want abcdef1234567 pushed", out.MergeSHA, out.Pushed)
		}
		if got, want := h.store.recorded(), []string{"status:delivering", "close"}; !deliveryEqualStrings(got, want) {
			t.Errorf("bd writes = %v, want %v", got, want)
		}
		if len(h.store.closes) != 1 || !strings.Contains(h.store.closes[0], "merged into main as abcdef1") {
			t.Errorf("close reason = %v, want the merge sha", h.store.closes)
		}
		if got, want := h.host.states(), []string{"approved→delivering", "delivering→closed"}; !deliveryEqualStrings(got, want) {
			t.Errorf("state changes = %v, want %v", got, want)
		}
		merged := h.host.published(events.KindMerged)
		if len(merged) != 1 {
			t.Fatalf("Merged events = %d, want 1", len(merged))
		}
		m, ok := merged[0].(events.Merged)
		if !ok {
			t.Fatalf("Merged event = %T", merged[0])
		}
		if m.SHA != "abcdef1234567" || m.Branch != "main" || !m.Pushed {
			t.Errorf("Merged = %+v, want the merge on main", m)
		}
	})
}

// TestDelivererRecordsTheMergeSHAButWithholdsTheClose pins the split of
// P6-06: gitx records sc_merge_sha inside the merge (before the close), the
// close itself is the state machine's so it takes the D-07 edge.
func TestDelivererRecordsTheMergeSHAButWithholdsTheClose(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false)
	if _, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(h.beads.updates) != 1 {
		t.Fatalf("sc_merge_sha writes = %d, want 1", len(h.beads.updates))
	}
	if got := h.beads.updates[0].Metadata[beads.MetaMergeSHA]; got != "abcdef1234567" {
		t.Errorf("sc_merge_sha = %v, want the merge sha", got)
	}
	if h.beads.closes != 0 {
		t.Errorf("gitx closed the bead %d times, want 0 (the state machine owns the close)", h.beads.closes)
	}
	if len(h.store.closes) != 1 {
		t.Errorf("state machine closes = %d, want 1", len(h.store.closes))
	}
}

// TestDelivererRetriesAHandOff is P6-16(a): a bead handed to a human by a
// push-only delivery is put back into delivering before the retry, so the
// successful retry's State: delivering describes a state bd really holds.
func TestDelivererRetriesAHandOff(t *testing.T) {
	t.Parallel()
	pushOnly := &gitx.PRResult{
		Status:       gitx.PRPushOnly,
		Bead:         "sc-68s",
		Branch:       "sc/sc-68s-add-retry",
		Base:         "main",
		Remote:       "origin",
		Pushed:       true,
		Reason:       "gh is not installed",
		Instructions: "open a pull request at https://example.test/compare",
		State:        domain.BeadStateNeedsHuman,
	}
	h := newDeliveryHarness(t, true, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.pr.results = []*gitx.PRResult{pushOnly, deliveryPRCreated()}
	})
	ctx := context.Background()

	out, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryHandedOff || out.State != domain.BeadStateNeedsHuman {
		t.Fatalf("first outcome = %s/%s, want handed_off/needs_human", out.Kind, out.State)
	}
	if !strings.Contains(out.Instructions, "compare") {
		t.Errorf("instructions = %q, want the manual instructions", out.Instructions)
	}
	if !deliveryContains(h.host.warnings(), "gh is not installed") {
		t.Errorf("warnings = %v, want the hand-off reason", h.host.warnings())
	}

	// The retry starts from needs_human, exactly as bd holds it.
	retry, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateNeedsHuman)})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.Kind != DeliveryPending || retry.State != domain.BeadStateDelivering {
		t.Fatalf("retry outcome = %s/%s, want pending_gate/delivering", retry.Kind, retry.State)
	}
	want := []string{"status:delivering", "status:needs_human", "status:delivering"}
	if got := h.store.recorded(); !deliveryEqualStrings(got, want) {
		t.Errorf("bd writes = %v, want %v (the retry restores delivering first)", got, want)
	}
	if got, want := h.host.states(), []string{"approved→delivering", "delivering→needs_human", "needs_human→delivering"}; !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
}

// TestDelivererNeverCommitsLeftoversMidRebase is P6-16(b): a worktree left
// mid-rebase must not have its conflict markers committed; the bead goes
// straight back to its engineer as a conflict round instead. It holds
// whatever the safety setting is, because the probe runs before the paths
// diverge.
func TestDelivererNeverCommitsLeftoversMidRebase(t *testing.T) {
	t.Parallel()
	for _, safety := range []bool{true, false} {
		t.Run(fmt.Sprintf("safety=%v", safety), func(t *testing.T) {
			t.Parallel()
			h := newDeliveryHarness(t, safety, func(_ *DeliveryOptions, h *deliveryHarness) {
				h.git.rebase = []RebaseState{{InProgress: true, Branch: "sc/sc-68s-add-retry", Files: []string{"go.mod", "internal/a.go"}}}
			})
			out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if out.Kind != DeliveryConflict || out.State != domain.BeadStateInProgress {
				t.Fatalf("outcome = %s/%s, want conflict/in_progress", out.Kind, out.State)
			}
			if n := h.git.leftoverCalls(); n != 0 {
				t.Errorf("CommitLeftovers ran %d times, want 0 in a worktree mid-rebase", n)
			}
			if len(h.pr.calls) != 0 || len(h.merge.calls) != 0 {
				t.Errorf("a delivery path ran (pr=%d merge=%d), want none", len(h.pr.calls), len(h.merge.calls))
			}
			if got, want := out.Conflict.Files, []string{"go.mod", "internal/a.go"}; !deliveryEqualStrings(got, want) {
				t.Errorf("conflict files = %v, want %v", got, want)
			}
			if out.Conflict.Target != "main" {
				t.Errorf("conflict target = %q, want main", out.Conflict.Target)
			}
			if got, want := h.store.recorded(), []string{"status:delivering", "reclaim:eng-sc-68s-r1"}; !deliveryEqualStrings(got, want) {
				t.Errorf("bd writes = %v, want %v", got, want)
			}
		})
	}
}

// TestDelivererConflictRoundIsOfferedToTheScheduler covers the work source:
// a pending conflict is re-offered every tick as an engineer firing in
// resolve-conflict mode, and stops being offered once it is settled.
func TestDelivererConflictRoundIsOfferedToTheScheduler(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for i := range 2 {
		ds, err := h.Demands(ctx)
		if err != nil {
			t.Fatalf("Demands: %v", err)
		}
		if len(ds) != 1 {
			t.Fatalf("tick %d: demands = %d, want 1 (a source re-offers until the work is done)", i, len(ds))
		}
		d := ds[0]
		if d.Kind != DemandImplement || d.FiringTask() != events.TaskResolveConflict {
			t.Errorf("demand = %s/%s, want implement/resolve_conflict", d.Kind, d.FiringTask())
		}
		if d.Claim != ClaimRework {
			t.Errorf("claim = %q, want %q (bd's atomic claim cannot take this bead)", d.Claim, ClaimRework)
		}
		if d.Round != 1 {
			t.Errorf("round = %d, want 1 (a conflict round keeps the review round's number, D-10)", d.Round)
		}
		p, ok := d.Payload.(PendingConflict)
		if !ok {
			t.Fatalf("payload = %T, want PendingConflict", d.Payload)
		}
		if got, want := p.Conflict.Files, []string{"internal/a.go"}; !deliveryEqualStrings(got, want) {
			t.Errorf("payload conflict files = %v, want %v", got, want)
		}
	}
	if _, err := h.Resolved(ctx, "sc-68s"); err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	ds, err := h.Demands(ctx)
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(ds) != 0 {
		t.Fatalf("demands after the round = %d, want 0", len(ds))
	}
}

// TestDelivererRedeliversAResolvedConflictWithoutANewReview is D-10 plus
// D-09: a small resolution is re-delivered straight away, walking the bead
// back through in_review and approved so the D-07 table stays honest.
func TestDelivererRedeliversAResolvedConflictWithoutANewReview(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go"), nil}
		// Nothing here scripts the ratio: the two change sets differ only
		// in the conflicted line, and the measurement the delivery makes
		// from them is 4 of 22 changed lines, under the 40% threshold.
		h.git.diffs = []*gitx.Diff{
			deliveryDiffOf("aaaaaaa", deliveryPatch("feature line", "three", "branch three")),
			deliveryDiffOf("bbbbbbb", deliveryPatch("feature line", "main three", "branch three and main three")),
		}
	})
	ctx := context.Background()
	first, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if first.Kind != DeliveryConflict {
		t.Fatalf("first outcome = %s, want conflict", first.Kind)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if out.Kind != DeliveryMerged || out.State != domain.BeadStateClosed {
		t.Fatalf("outcome = %s/%s, want merged/closed (%s)", out.Kind, out.State, out.Summary())
	}
	want := []string{
		"approved→delivering", "delivering→in_progress",
		"in_progress→in_review", "in_review→approved", "approved→delivering", "delivering→closed",
	}
	if got := h.host.states(); !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
	if n := h.git.leftoverCalls(); n != 2 {
		t.Errorf("CommitLeftovers ran %d times, want 2 (once per attempt, D-14)", n)
	}
	if out.Ratio.Delta != 4 || out.Ratio.Total != 22 {
		t.Errorf("D-09 measurement = %+v, want 4 of 22 changed lines", out.Ratio)
	}
	if out.Ratio.Ratio > gitx.DefaultReReviewThreshold {
		t.Errorf("ratio = %v, want at most the threshold %v", out.Ratio.Ratio, gitx.DefaultReReviewThreshold)
	}
}

// TestDelivererReReviewsABigConflictResolution is the D-09 half of the same
// rule: a resolution that changed more than the threshold goes back for a
// full review instead of being re-delivered.
func TestDelivererReReviewsABigConflictResolution(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
		// The engineer rewrote the twenty lines that had nothing to do
		// with the conflict as well as settling it.
		h.git.diffs = []*gitx.Diff{
			deliveryDiffOf("aaaaaaa", deliveryPatch("feature line", "three", "branch three")),
			deliveryDiffOf("bbbbbbb", deliveryPatch("rewritten line", "main three", "branch three and main three")),
		}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if out.Kind != DeliveryNeedsReview || out.State != domain.BeadStateInReview {
		t.Fatalf("outcome = %s/%s, want needs_review/in_review (%s)", out.Kind, out.State, out.Summary())
	}
	if len(h.merge.calls) != 1 {
		t.Errorf("merge attempts = %d, want 1 (the re-delivery waits for the review)", len(h.merge.calls))
	}
	want := []string{"approved→delivering", "delivering→in_progress", "in_progress→in_review"}
	if got := h.host.states(); !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
}

// TestDelivererUnmeasurableResolutionIsReviewedAgain: when the diff before
// the conflict round could not be taken, the delivery errs on the side of a
// review rather than shipping something nobody looked at.
func TestDelivererUnmeasurableResolutionIsReviewedAgain(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
		h.git.diffErr = errors.New("git: no such revision")
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	p, ok := h.PendingFor("sc-68s")
	if !ok {
		t.Fatal("no conflict round is pending")
	}
	if p.Diff != nil {
		t.Errorf("diff snapshot = %+v, want nil when it could not be taken", p.Diff)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if out.Kind != DeliveryNeedsReview {
		t.Fatalf("outcome = %s, want needs_review", out.Kind)
	}
}

// TestDelivererCheckFailureIsAQAChangeRequest is P6-06's third case: the
// check command failing at delivery time is an ordinary change-request
// round, with the output as the qa item's detail.
func TestDelivererCheckFailureIsAQAChangeRequest(t *testing.T) {
	t.Parallel()
	check := &gitx.CheckResult{Command: "make check", ExitCode: 2, Output: "FAIL\tslop-cannon/internal/x\n"}
	h := newDeliveryHarness(t, false, func(o *DeliveryOptions, h *deliveryHarness) {
		o.CheckCommand = "make check"
		h.merge.errs = []error{&gitx.DeliveryError{
			Stage:  gitx.StageCheck,
			Output: check.Output,
			Check:  check,
			Err:    gitx.ErrCheckFailed,
		}}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryCheckFailed || out.State != domain.BeadStateChangesRequested {
		t.Fatalf("outcome = %s/%s, want check_failed/changes_requested (%s)", out.Kind, out.State, out.Summary())
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.Items))
	}
	item := out.Items[0]
	if item.ID != "check-r1-1" || item.Severity != domain.SeverityBlocker || item.Status != domain.ItemOpen {
		t.Errorf("item = %+v, want a blocker qa item with a per-round id", item)
	}
	if !strings.Contains(item.Title, "make check") || !strings.Contains(item.Detail, "FAIL") {
		t.Errorf("item = %+v, want the command in the title and its output in the detail", item)
	}
	if got := h.merge.calls[0].CheckCommand; got != "make check" {
		t.Errorf("check command passed to gitx = %q, want make check", got)
	}
	rounds := h.host.published(events.KindRoundStarted)
	if len(rounds) != 1 {
		t.Fatalf("RoundStarted events = %d, want 1", len(rounds))
	}
	r, ok := rounds[0].(events.RoundStarted)
	if !ok {
		t.Fatalf("RoundStarted event = %T", rounds[0])
	}
	if r.RoundKind != events.RoundCheckFailed || len(r.Lenses) != 1 || r.Lenses[0] != domain.LensQA {
		t.Errorf("RoundStarted = %+v, want a qa check_failed round", r)
	}
	if got, want := h.host.states(), []string{"approved→delivering", "delivering→changes_requested"}; !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
}

// TestDelivererSafetyOnRunsNoCheck: the check command belongs to the
// safety-off path (D-10), so a safety-on delivery cannot fail one.
func TestDelivererSafetyOnRunsNoCheck(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, true, func(o *DeliveryOptions, _ *deliveryHarness) {
		o.CheckCommand = "make check"
	})
	if _, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(h.merge.calls) != 0 {
		t.Fatalf("the merge path ran %d times with safety on, want 0", len(h.merge.calls))
	}
}

// TestDelivererHandsOffAfterTooManyConflictRounds keeps a bead that never
// resolves out of an infinite loop.
func TestDelivererHandsOffAfterTooManyConflictRounds(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(o *DeliveryOptions, h *deliveryHarness) {
		o.MaxConflictRounds = 2
		h.merge.errs = []error{deliveryConflictErr("a.go"), deliveryConflictErr("a.go"), deliveryConflictErr("a.go")}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := h.Resolved(ctx, "sc-68s"); err != nil {
		t.Fatalf("second round: %v", err)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("third round: %v", err)
	}
	if out.Kind != DeliveryHandedOff || out.State != domain.BeadStateNeedsHuman {
		t.Fatalf("outcome = %s/%s, want handed_off/needs_human (%s)", out.Kind, out.State, out.Summary())
	}
	if _, ok := h.PendingFor("sc-68s"); ok {
		t.Error("a conflict round is still pending after the hand-off")
	}
}

// TestDelivererDoesNotSpendAConflictRoundOnABDFailure: MaxConflictRounds
// bounds the rounds an engineer was actually given, not the times bd
// hiccuped. A failure to write the conflict edge leaves the round unopened
// and the outcome retryable, so it must leave the budget alone too —
// otherwise three transient bd failures park the bead in needs_human saying
// three conflict rounds did not resolve it when no engineer ever ran, and
// D-10's "conflicts go back to the engineer" never happens.
func TestDelivererDoesNotSpendAConflictRoundOnABDFailure(t *testing.T) {
	t.Parallel()
	bdDown := errors.New("bd: database is locked")
	conflicts := make([]error, 8)
	for i := range conflicts {
		conflicts[i] = deliveryConflictErr("internal/a.go")
	}
	h := newDeliveryHarness(t, false, func(o *DeliveryOptions, h *deliveryHarness) {
		o.MaxConflictRounds = 3
		h.merge.errs = conflicts
	})
	ctx := context.Background()
	// The conflict edge claims the bead for its engineer, so "reclaim:" is
	// the write a hiccup lands on.
	h.store.failAfter("reclaim:", bdDown)
	bead := deliveryBead(domain.BeadStateApproved)
	for i := range 3 {
		out, err := h.Deliver(ctx, DeliveryRequest{Bead: bead})
		if !errors.Is(err, bdDown) {
			t.Fatalf("attempt %d: err = %v, want the bd failure", i+1, err)
		}
		if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
			t.Fatalf("attempt %d: outcome = %s/%s, want retryable/delivering", i+1, out.Kind, out.State)
		}
		if _, ok := h.PendingFor("sc-68s"); ok {
			t.Fatalf("attempt %d: a conflict round is pending although bd refused to open it", i+1)
		}
		// The caller's answer to a retryable outcome is to deliver again.
		bead.State = out.State
	}
	h.store.failAfter("", nil)

	// bd is back: all three rounds must still be there. Each resolution is
	// re-delivered by Resolved itself (D-09 asks for no re-review here) and
	// conflicts again, which is what opens the next round.
	out, err := h.Deliver(ctx, DeliveryRequest{Bead: bead})
	if err != nil {
		t.Fatalf("the first round: %v", err)
	}
	for round := 1; round <= 3; round++ {
		if out.Kind != DeliveryConflict || out.State != domain.BeadStateInProgress {
			t.Fatalf("round %d: outcome = %s/%s, want conflict/in_progress (%s)", round, out.Kind, out.State, out.Summary())
		}
		p, ok := h.PendingFor("sc-68s")
		if !ok {
			t.Fatalf("round %d: no conflict round is pending", round)
		}
		if p.Attempt != round {
			t.Fatalf("round %d: attempt = %d, want %d (a bd hiccup burnt a conflict round)", round, p.Attempt, round)
		}
		if out, err = h.Resolved(ctx, "sc-68s"); err != nil {
			t.Fatalf("round %d: Resolved: %v", round, err)
		}
	}
	// Only now, after three rounds an engineer really was given, is the
	// bead a person's problem.
	if out.Kind != DeliveryHandedOff || out.State != domain.BeadStateNeedsHuman {
		t.Fatalf("the fourth round: outcome = %s/%s, want handed_off/needs_human (%s)", out.Kind, out.State, out.Summary())
	}
}

// TestDelivererConflictRoundIsNeverEmpty: agents refuses a resolve_conflict
// request whose conflict is empty, so a round built from one could be
// offered for ever and never fire. Every path that opens a round therefore
// fills in what it does know, even when git named neither the target branch
// nor a single conflicted file.
func TestDelivererConflictRoundIsNeverEmpty(t *testing.T) {
	t.Parallel()
	t.Run("the mid-rebase probe with no files and no default branch", func(t *testing.T) {
		t.Parallel()
		h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
			h.git.rebase = []RebaseState{{InProgress: true}}
			h.git.branchErr = errors.New("git: no default branch")
		})
		out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if out.Kind != DeliveryConflict {
			t.Fatalf("outcome = %s, want conflict (%s)", out.Kind, out.Summary())
		}
		if out.Conflict.Empty() {
			t.Fatal("the conflict is empty, so agents would refuse the round the deliverer keeps offering")
		}
		p, ok := h.PendingFor("sc-68s")
		if !ok {
			t.Fatal("no conflict round is pending")
		}
		if p.Conflict.Empty() {
			t.Fatal("the pending round's conflict is empty")
		}
		req := agents.EngineerRequest{
			Mode:     agents.EngineerResolveConflict,
			RunID:    h.host.ID(),
			AgentID:  p.Agent,
			Bead:     prompt.BeadContext{Bead: p.Bead, Worktree: t.TempDir(), Round: p.Round},
			Conflict: p.Conflict,
		}
		if err := req.Validate(); err != nil {
			t.Fatalf("the conflict round does not make a valid engineer request: %v", err)
		}
		if _, err := req.Firing(); err != nil {
			t.Fatalf("Firing: %v", err)
		}
	})
	t.Run("the mid-rebase probe names the branch it found", func(t *testing.T) {
		t.Parallel()
		h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
			h.git.rebase = []RebaseState{{InProgress: true, Branch: "sc/sc-68s-add-retry"}}
		})
		out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
		if err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !strings.Contains(out.Conflict.Output, "sc/sc-68s-add-retry") {
			t.Errorf("conflict output = %q, want the rebasing branch named", out.Conflict.Output)
		}
	})
	t.Run("a resume that found a rebase and nothing else", func(t *testing.T) {
		t.Parallel()
		h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
			h.git.branchErr = errors.New("git: no default branch")
		})
		bead := deliveryBead(domain.BeadStateDelivering)
		if err := h.ConflictRound(context.Background(), gitx.ResumeEntry{
			Bead: bead, State: domain.BeadStateDelivering, Conflict: true,
		}); err != nil {
			t.Fatalf("ConflictRound: %v", err)
		}
		p, ok := h.PendingFor(bead.ID)
		if !ok {
			t.Fatal("no conflict round is pending")
		}
		if p.Conflict.Empty() {
			t.Fatal("the resumed round's conflict is empty, so its engineer could never be fired")
		}
	})
}

// TestDelivererOpensAConflictRoundWhenTheConflictedPathsCannotBeListed:
// managerGit.RebaseState reports InProgress with the branch even when
// "git diff --diff-filter=U" failed, and that fact outranks the error — a
// worktree full of conflict markers belongs to its engineer, with or without
// a file list. Retrying the probe instead would leave the bead in delivering
// for ever with nobody working on it.
func TestDelivererOpensAConflictRoundWhenTheConflictedPathsCannotBeListed(t *testing.T) {
	t.Parallel()
	listFailed := errors.New("git: diff --diff-filter=U failed")
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.git.rebaseErr = listFailed
		h.git.rebasePartial = RebaseState{InProgress: true, Branch: "sc/sc-68s-add-retry"}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryConflict || out.State != domain.BeadStateInProgress {
		t.Fatalf("outcome = %s/%s, want conflict/in_progress (%s)", out.Kind, out.State, out.Summary())
	}
	if out.Conflict.Empty() {
		t.Error("the conflict is empty, so agents would refuse the round")
	}
	if len(out.Conflict.Files) != 0 {
		t.Errorf("conflict files = %v, want none (git could not list them)", out.Conflict.Files)
	}
	if n := h.git.leftoverCalls(); n != 0 {
		t.Errorf("CommitLeftovers ran %d times, want 0 in a worktree mid-rebase", n)
	}
	if len(h.merge.calls) != 0 {
		t.Errorf("the merge path ran %d times, want 0", len(h.merge.calls))
	}
	if !deliveryContains(h.host.warnings(), "conflicted paths could not be listed") {
		t.Errorf("warnings = %v, want one about the unlistable paths", h.host.warnings())
	}
	if got, want := h.host.states(), []string{"approved→delivering", "delivering→in_progress"}; !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
}

// TestDelivererProbeFailureWithNoRebaseIsRetryable is the other half of the
// ordering above: an inspection that failed and reported no rebase says
// nothing about the worktree, so it stays a retryable failure with the bead
// in delivering rather than becoming a conflict round out of nothing.
func TestDelivererProbeFailureWithNoRebaseIsRetryable(t *testing.T) {
	t.Parallel()
	probeFailed := errors.New("git: rev-parse failed")
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.git.rebaseErr = probeFailed
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, probeFailed) {
		t.Fatalf("err = %v, want the probe failure", err)
	}
	if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
		t.Fatalf("outcome = %s/%s, want retryable/delivering", out.Kind, out.State)
	}
	if _, ok := h.PendingFor("sc-68s"); ok {
		t.Error("a conflict round is pending although nothing said the worktree is mid-rebase")
	}
}

// TestDelivererRetryableFailureLeavesTheBeadDelivering: a push that failed
// is worth another attempt, so the bead stays in delivering and the error is
// returned.
func TestDelivererRetryableFailureLeavesTheBeadDelivering(t *testing.T) {
	t.Parallel()
	cause := &gitx.DeliveryError{Stage: gitx.StagePush, Err: errors.New("git: remote unreachable"), MergeSHA: "abcdef1"}
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{cause}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the delivery error", err)
	}
	if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
		t.Fatalf("outcome = %s/%s, want retryable/delivering", out.Kind, out.State)
	}
	if got, want := h.store.recorded(), []string{"status:delivering"}; !deliveryEqualStrings(got, want) {
		t.Errorf("bd writes = %v, want %v", got, want)
	}
}

// TestDelivererMergeLandedButTheCloseFailed: the work is on the default
// branch and only bd refused, so the attempt is retryable and the bead stays
// in delivering; a retry finds the merge already landed and finishes it.
func TestDelivererMergeLandedButTheCloseFailed(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd: blockers are still open")
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.store.failOn, h.store.err = "close", boom
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the close failure", err)
	}
	if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
		t.Fatalf("outcome = %s/%s, want retryable/delivering", out.Kind, out.State)
	}
	if n := len(h.host.published(events.KindMerged)); n != 0 {
		// P6-40: the work did land, but the bead is not closed, so the
		// merge is not announced yet — the retry that finishes the close
		// announces it, once. TestDelivererAnnouncesOneMergePerBead drives
		// that retry.
		t.Errorf("Merged events = %d, want 0 until the close lands", n)
	}
	if got := h.host.states(); !deliveryEqualStrings(got, []string{"approved→delivering"}) {
		t.Errorf("state changes = %v, want only the delivering transition", got)
	}
}

// TestDelivererPullRequestFailureIsRetryable: a push that the remote
// rejected is worth another attempt.
func TestDelivererPullRequestFailureIsRetryable(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, true, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.pr.err = gitx.ErrPushRejected
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, gitx.ErrPushRejected) {
		t.Fatalf("err = %v, want the push rejection", err)
	}
	if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
		t.Fatalf("outcome = %s/%s, want retryable/delivering", out.Kind, out.State)
	}
	if !deliveryContains(h.host.warnings(), "pull request could not be delivered") {
		t.Errorf("warnings = %v, want the failure", h.host.warnings())
	}
}

// TestDelivererHandsOffWhatNeedsAPerson: a diverged default branch is not
// something the cannon retries its way out of.
func TestDelivererHandsOffWhatNeedsAPerson(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{&gitx.DeliveryError{Stage: gitx.StageFetch, Err: gitx.ErrDefaultBranchDiverged}}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryHandedOff || out.State != domain.BeadStateNeedsHuman {
		t.Fatalf("outcome = %s/%s, want handed_off/needs_human", out.Kind, out.State)
	}
}

// TestDelivererRefusesABeadThatIsNotDeliverable: only approved, needs_human
// and delivering may be delivered.
func TestDelivererRefusesABeadThatIsNotDeliverable(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false)
	for _, state := range []domain.BeadState{domain.BeadStateOpen, domain.BeadStateInReview, domain.BeadStateClosed, ""} {
		_, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(state)})
		if !errors.Is(err, ErrNotDeliverable) {
			t.Errorf("state %q: err = %v, want ErrNotDeliverable", state, err)
		}
	}
	if n := len(h.merge.calls); n != 0 {
		t.Errorf("merge attempts = %d, want 0", n)
	}
}

// TestNewDelivererNeedsThePathItsSafetySettingUses.
func TestNewDelivererNeedsThePathItsSafetySettingUses(t *testing.T) {
	t.Parallel()
	if _, err := NewDeliverer(DeliveryOptions{}); !errors.Is(err, ErrNoDeliveryHost) {
		t.Errorf("no host: err = %v, want ErrNoDeliveryHost", err)
	}
	if _, err := NewDeliverer(DeliveryOptions{Host: newDeliveryHost(true), Merge: &deliveryMergeFake{}}); !errors.Is(err, ErrNoDeliveryPath) {
		t.Errorf("safety on without a PR path: err = %v, want ErrNoDeliveryPath", err)
	}
	if _, err := NewDeliverer(DeliveryOptions{Host: newDeliveryHost(false), PR: &deliveryPRFake{}}); !errors.Is(err, ErrNoDeliveryPath) {
		t.Errorf("safety off without a merge path: err = %v, want ErrNoDeliveryPath", err)
	}
	d, err := NewDeliverer(DeliveryOptions{Host: newDeliveryHost(true), PR: &deliveryPRFake{}})
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	if !d.Safety() {
		t.Error("Safety() = false, want the host's snapshot")
	}
}

// TestDelivererResumeRedeliversAPullRequest is the P6-15 hand-off: the
// delivery is idempotent, so re-running it recreates the gh:pr gate.
func TestDelivererResumeRedeliversAPullRequest(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, true, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.pr.results = []*gitx.PRResult{{
			Status: gitx.PRExisting, Bead: "sc-68s", Branch: "sc/sc-68s-add-retry", Base: "main",
			PR: &gitx.PullRequest{Number: 7, URL: "https://example.test/pr/7"}, PRState: "OPEN",
			Gate: "sc-gate-1", State: domain.BeadStateDelivering,
		}}
	})
	entry := gitx.ResumeEntry{
		Bead: deliveryBead(domain.BeadStateDelivering), Branch: "sc/sc-68s-add-retry", BranchExists: true,
		PR: &gitx.PullRequest{Number: 7, URL: "https://example.test/pr/7"}, PRState: "OPEN",
		State: domain.BeadStateDelivering,
	}
	if err := h.RedeliverPR(context.Background(), entry); err != nil {
		t.Fatalf("RedeliverPR: %v", err)
	}
	if len(h.pr.calls) != 1 {
		t.Fatalf("pull request deliveries = %d, want 1", len(h.pr.calls))
	}
	if got := h.store.recorded(); len(got) != 0 {
		t.Errorf("bd writes = %v, want none (the bead was already delivering)", got)
	}

	// Safety off has no pull request to redeliver; it warns and does nothing.
	off := newDeliveryHarness(t, false)
	if err := off.RedeliverPR(context.Background(), entry); err != nil {
		t.Fatalf("RedeliverPR with safety off: %v", err)
	}
	if len(off.host.warnings()) != 1 {
		t.Errorf("warnings = %v, want one about the pull request", off.host.warnings())
	}
}

// TestDelivererResumeOpensAConflictRound is the other P6-15 hand-off: a
// worktree left mid-rebase across a restart owes its engineer a round.
func TestDelivererResumeOpensAConflictRound(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false)
	entry := gitx.ResumeEntry{
		Bead: deliveryBead(domain.BeadStateDelivering), Branch: "sc/sc-68s-add-retry", BranchExists: true,
		Conflict: true, ConflictFiles: []string{"internal/a.go"},
		State: domain.BeadStateDelivering, Reason: "worktree left mid-rebase",
	}
	ctx := context.Background()
	if err := h.ConflictRound(ctx, entry); err != nil {
		t.Fatalf("ConflictRound: %v", err)
	}
	p, ok := h.PendingFor("sc-68s")
	if !ok {
		t.Fatal("no conflict round is pending")
	}
	if got, want := p.Conflict.Files, []string{"internal/a.go"}; !deliveryEqualStrings(got, want) {
		t.Errorf("conflict files = %v, want %v", got, want)
	}
	if p.Conflict.Target != "main" {
		t.Errorf("conflict target = %q, want main", p.Conflict.Target)
	}
	if got, want := h.store.recorded(), []string{"reclaim:eng-sc-68s-r1"}; !deliveryEqualStrings(got, want) {
		t.Errorf("bd writes = %v, want %v", got, want)
	}
	// Calling it twice must not open a second round.
	if err := h.ConflictRound(ctx, entry); err != nil {
		t.Fatalf("second ConflictRound: %v", err)
	}
	if ds, _ := h.Demands(ctx); len(ds) != 1 {
		t.Errorf("demands = %d, want 1", len(ds))
	}
}

// TestConflictFromDeliveryError is P5-12: the one place that copies gitx's
// delivery error onto the engineer's resolve-conflict input.
func TestConflictFromDeliveryError(t *testing.T) {
	t.Parallel()
	err := deliveryConflictErr("internal/a.go", " internal/b.go ", "internal/a.go", "")
	got, ok := ConflictFrom(err, "  main  ")
	if !ok {
		t.Fatal("ConflictFrom reported no conflict")
	}
	if got.Target != "main" {
		t.Errorf("target = %q, want main", got.Target)
	}
	if want := []string{"internal/a.go", "internal/b.go"}; !deliveryEqualStrings(got.Files, want) {
		t.Errorf("files = %v, want %v (trimmed and de-duplicated)", got.Files, want)
	}
	if !strings.Contains(got.Output, "CONFLICT") {
		t.Errorf("output = %q, want git's message", got.Output)
	}
	if got.Empty() {
		t.Error("the conflict is empty, so the engineer request would be refused")
	}

	if _, ok := ConflictFrom(errors.New("boom"), "main"); ok {
		t.Error("a plain error was reported as a conflict")
	}
	if _, ok := ConflictFrom(&gitx.DeliveryError{Stage: gitx.StagePush}, "main"); ok {
		t.Error("a non-conflict delivery error was reported as a conflict")
	}
	entry := gitx.ResumeEntry{Conflict: true, ConflictFiles: []string{"a.go"}, Reason: "left mid-rebase"}
	c, ok := ConflictFromResume(entry, "main")
	if !ok || c.Files[0] != "a.go" || c.Output != "left mid-rebase" {
		t.Errorf("ConflictFromResume = %+v (%v), want the entry's files and reason", c, ok)
	}
	if _, ok := ConflictFromResume(gitx.ResumeEntry{}, "main"); ok {
		t.Error("an entry with no conflict was reported as one")
	}
}

// TestDelivererConflictBuildsTheEngineerRequest is the "done when" of P5-12:
// the conflict a round carries is exactly what an EngineerResolveConflict
// firing needs, with no gitx type crossing into internal/agents.
func TestDelivererConflictBuildsTheEngineerRequest(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	p, ok := h.PendingFor("sc-68s")
	if !ok {
		t.Fatal("no conflict round is pending")
	}
	req := agents.EngineerRequest{
		Mode:     agents.EngineerResolveConflict,
		RunID:    h.host.ID(),
		AgentID:  p.Agent,
		Bead:     prompt.BeadContext{Bead: p.Bead, Worktree: t.TempDir(), Round: p.Round},
		Conflict: out.Conflict,
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("the conflict round does not make a valid engineer request: %v", err)
	}
	firing, err := req.Firing()
	if err != nil {
		t.Fatalf("Firing: %v", err)
	}
	if firing.Task != events.TaskResolveConflict {
		t.Errorf("task = %s, want resolve_conflict", firing.Task)
	}
	if body := firing.Prompt.String(); !strings.Contains(body, "internal/a.go") {
		t.Errorf("the prompt does not name the conflicted file:\n%s", body)
	}
}

// TestDelivererAbandonsAConflictRound: a round nobody could run hands the
// bead to a person.
func TestDelivererAbandonsAConflictRound(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("a.go")}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := h.AbandonConflict(ctx, "sc-68s", "the engineer crashed twice", errors.New("boom")); err != nil {
		t.Fatalf("AbandonConflict: %v", err)
	}
	if _, ok := h.PendingFor("sc-68s"); ok {
		t.Error("the round is still pending")
	}
	if got, want := h.host.states(), []string{"approved→delivering", "delivering→in_progress", "in_progress→needs_human"}; !deliveryEqualStrings(got, want) {
		t.Errorf("state changes = %v, want %v", got, want)
	}
	if err := h.AbandonConflict(ctx, "sc-68s", "", nil); !errors.Is(err, ErrNoConflictRound) {
		t.Errorf("second AbandonConflict: err = %v, want ErrNoConflictRound", err)
	}
	if _, err := h.Resolved(ctx, "sc-68s"); !errors.Is(err, ErrNoConflictRound) {
		t.Errorf("Resolved with nothing pending: err = %v, want ErrNoConflictRound", err)
	}
}

// TestDeliveryGitAgainstARealRepository covers the *gitx.Manager adapter:
// the mid-rebase probe, the leftovers commit and the diffs the D-09 rule
// compares, against a real git repository (no network, no user config).
func TestDeliveryGitAgainstARealRepository(t *testing.T) {
	t.Parallel()
	if g := NewDeliveryGit(nil); g != nil {
		t.Fatalf("NewDeliveryGit(nil) = %v, want nil", g)
	}
	f := testutil.NewFixtureRepo(t)
	ctx := context.Background()
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	repo, err := gitx.Open(ctx, f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := gitx.NewManager(repo, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	g := NewDeliveryGit(m)

	if branch, err := g.DefaultBranch(ctx); err != nil || branch != f.DefaultBranch {
		t.Fatalf("DefaultBranch = %q, %v, want %q", branch, err, f.DefaultBranch)
	}
	bead := deliveryBead(domain.BeadStateApproved)
	if _, err := g.RebaseState(ctx, bead.ID); !errors.Is(err, gitx.ErrWorktreeMissing) {
		t.Errorf("RebaseState without a worktree: err = %v, want ErrWorktreeMissing", err)
	}
	wt, err := m.Create(ctx, bead)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rs, err := g.RebaseState(ctx, bead.ID)
	if err != nil {
		t.Fatalf("RebaseState: %v", err)
	}
	if rs.InProgress {
		t.Errorf("RebaseState = %+v, want no rebase in a fresh worktree", rs)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "delivery.txt"), []byte("left behind\n"), 0o644); err != nil {
		t.Fatalf("writing a leftover: %v", err)
	}
	lo, err := g.CommitLeftovers(ctx, bead, 1)
	if err != nil {
		t.Fatalf("CommitLeftovers: %v", err)
	}
	if !lo.Committed || lo.Stats.Empty() {
		t.Errorf("leftovers = %+v, want a commit with stats", lo)
	}
	if _, err := g.CommitLeftovers(ctx, domain.Bead{}, 1); err == nil ||
		errors.Is(err, ErrRebaseInProgress) || !strings.Contains(err.Error(), "no id") {
		t.Errorf("CommitLeftovers without a bead id: err = %v, want gitx's own complaint", err)
	}
	diff, err := g.Diff(ctx, bead.ID)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff.Empty() {
		t.Error("the branch diff is empty after the leftovers commit")
	}
	ratio, err := g.ChangeRatio(ctx, bead.ID, diff, diff)
	if err != nil {
		t.Fatalf("ChangeRatio: %v", err)
	}
	if ratio.Delta != 0 || ratio.Ratio != 0 {
		t.Errorf("ChangeRatio of a diff with itself = %+v, want no movement", ratio)
	}
}

// TestDeliveryOutcomeSummary renders one line per outcome kind.
func TestDeliveryOutcomeSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		out  DeliveryOutcome
		want string
	}{
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryPending, PR: &gitx.PullRequest{Number: 7}}, "pull request #7"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryPending}, "waiting for the pull request gate"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryMerged, Target: "main", MergeSHA: "abcdef1234"}, "merged into main as abcdef1"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryHandedOff, Reason: "no remote"}, "needs a human: no remote"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryConflict, Round: 2, Reason: "conflicts"}, "conflict round 2"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryCheckFailed}, "qa change request"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryNeedsReview, Ratio: gitx.ChangeRatio{Ratio: 0.8}}, "changed 80%"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryRetryable, Reason: "remote unreachable"}, "did not finish: remote unreachable"},
		{DeliveryOutcome{Bead: "sc-1", Kind: DeliveryKind("odd")}, "sc-1: odd"},
	}
	for _, c := range cases {
		if got := c.out.Summary(); !strings.Contains(got, c.want) {
			t.Errorf("Summary() = %q, want it to contain %q", got, c.want)
		}
		if c.out.Kind.String() != string(c.out.Kind) {
			t.Errorf("String() = %q, want %q", c.out.Kind.String(), c.out.Kind)
		}
	}
}

// TestDelivererFallsBackToTheTipMeasurement: gitx gives an oversized diff no
// patch at all, so the two change sets cannot be compared line by line. The
// delivery then falls back to gitx's own tip-to-tip ChangeRatio, which counts
// the upstream commits of the rebase as movement too and therefore errs
// towards a review.
func TestDelivererFallsBackToTheTipMeasurement(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
		big := deliveryDiffOf("aaaaaaa", deliveryPatch("feature line", "three", "branch three"))
		big.Patch, big.Truncated = "", true
		h.git.diffs = []*gitx.Diff{big}
		h.git.ratio = gitx.ChangeRatio{Previous: "aaaaaaa", Current: "bbbbbbb", Delta: 900, Total: 1000, Ratio: 0.9}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if out.Kind != DeliveryNeedsReview {
		t.Fatalf("outcome = %s, want needs_review", out.Kind)
	}
	if out.Ratio.Delta != 900 {
		t.Errorf("ratio = %+v, want the tip-to-tip measurement", out.Ratio)
	}
}

// TestDelivererMeasuresAnUnfinishedRebaseAsAFullRewrite: an engineer that
// resolved the markers but never ran "git rebase --continue" has no resolved
// change set to measure, so D-09 sends the bead back for a review rather
// than shipping something nobody could compare.
func TestDelivererMeasuresAnUnfinishedRebaseAsAFullRewrite(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
		// The probes in order: the delivery's own (clean), then the one
		// Resolved makes (still mid-rebase).
		h.git.rebase = []RebaseState{{}, {InProgress: true, Files: []string{"internal/a.go"}}}
	})
	ctx := context.Background()
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	out, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if out.Kind != DeliveryNeedsReview {
		t.Fatalf("outcome = %s, want needs_review", out.Kind)
	}
	if !deliveryContains(h.host.warnings(), "rebase was left unfinished") {
		t.Errorf("warnings = %v, want the unfinished rebase", h.host.warnings())
	}
}

// TestDelivererKeepsTheConflictRoundWhenABDWriteFails: the pending round is
// the only thing that makes a bead in a conflict round reachable — the
// scheduler offers it and AbandonConflict settles it — so a bd write that
// fails half way must leave it registered. Every other failure in the
// delivery leaves the bead recoverable; this one used to drop the round
// first and strand the bead in in_progress with no work source.
func TestDelivererKeepsTheConflictRoundWhenABDWriteFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd: connection refused")

	t.Run("the submit fails", func(t *testing.T) {
		t.Parallel()
		h := deliveryConflictHarness(t)
		ctx := context.Background()
		h.store.failAfter("status:in_review", boom)
		out, err := h.Resolved(ctx, "sc-68s")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the bd failure", err)
		}
		if out.Kind != DeliveryRetryable {
			t.Fatalf("outcome = %s, want retryable", out.Kind)
		}
		if _, ok := h.PendingFor("sc-68s"); !ok {
			t.Fatal("the conflict round is gone: the bead is in_progress with nothing offering it")
		}
		ds, err := h.Demands(ctx)
		if err != nil || len(ds) != 1 {
			t.Fatalf("demands = %d, %v, want the round still offered", len(ds), err)
		}
		// bd comes back and the same call finishes the round.
		h.store.failAfter("", nil)
		res, err := h.Resolved(ctx, "sc-68s")
		if err != nil {
			t.Fatalf("Resolved: %v", err)
		}
		if res.Kind != DeliveryMerged {
			t.Fatalf("outcome = %s, want merged (%s)", res.Kind, res.Summary())
		}
	})

	t.Run("the approval fails", func(t *testing.T) {
		t.Parallel()
		h := deliveryConflictHarness(t)
		ctx := context.Background()
		h.store.failAfter("status:approved", boom)
		out, err := h.Resolved(ctx, "sc-68s")
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the bd failure", err)
		}
		if out.Kind != DeliveryRetryable || out.State != domain.BeadStateInReview {
			t.Fatalf("outcome = %s/%s, want retryable/in_review", out.Kind, out.State)
		}
		if _, ok := h.PendingFor("sc-68s"); !ok {
			t.Fatal("the conflict round is gone, so AbandonConflict cannot settle the bead either")
		}
		ds, err := h.Demands(ctx)
		if err != nil || len(ds) != 0 {
			t.Fatalf("demands = %d, %v, want none: the engineer already finished", len(ds), err)
		}
		h.store.failAfter("", nil)
		res, err := h.Resolved(ctx, "sc-68s")
		if err != nil {
			t.Fatalf("Resolved: %v", err)
		}
		if res.Kind != DeliveryMerged {
			t.Fatalf("outcome = %s, want merged (%s)", res.Kind, res.Summary())
		}
		// The retry resumes at the transition that failed rather than
		// walking the bead through in_review a second time.
		want := []string{
			"approved→delivering", "delivering→in_progress", "in_progress→in_review",
			"in_review→approved", "approved→delivering", "delivering→closed",
		}
		if got := h.host.states(); !deliveryEqualStrings(got, want) {
			t.Errorf("state changes = %v, want %v", got, want)
		}
	})
}

// TestDelivererMergeWithNeitherResultNorError: a merge path that answers
// nothing at all is a retryable failure, not a nil dereference.
func TestDelivererMergeWithNeitherResultNorError(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.results = nil
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, errNoMergeResult) {
		t.Fatalf("err = %v, want errNoMergeResult", err)
	}
	if out.Kind != DeliveryRetryable || out.State != domain.BeadStateDelivering {
		t.Fatalf("outcome = %s/%s, want retryable/delivering", out.Kind, out.State)
	}
}

// TestConflictChangeRatioIgnoresUpstreamDrift pins the arithmetic D-09 needs
// for a conflict round: the two change sets are both measured against their
// own merge-base, so the commits the branch was rebased onto are in neither
// of them and only what the engineer did counts.
func TestConflictChangeRatioIgnoresUpstreamDrift(t *testing.T) {
	t.Parallel()
	before := deliveryDiffOf("aaaaaaa", deliveryPatch("feature line", "three", "branch three"))
	cases := []struct {
		name  string
		after *gitx.Diff
		delta int
		total int
		ok    bool
	}{
		{
			name:  "the same change set after a rebase over a busy branch",
			after: deliveryDiffOf("bbbbbbb", deliveryPatch("feature line", "three", "branch three")),
			delta: 0, total: 22, ok: true,
		},
		{
			name:  "only the conflicted line was settled",
			after: deliveryDiffOf("bbbbbbb", deliveryPatch("feature line", "main three", "branch three and main three")),
			delta: 4, total: 22, ok: true,
		},
		{
			name:  "the whole change set was rewritten",
			after: deliveryDiffOf("bbbbbbb", deliveryPatch("rewritten line", "main three", "branch three and main three")),
			delta: 44, total: 22, ok: true,
		},
		{
			name:  "the change set vanished",
			after: &gitx.Diff{Stats: gitx.DiffStats{Head: "bbbbbbb"}},
			delta: 22, total: 0, ok: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := gitx.ChangeSetRatio(before, c.after)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if got.Delta != c.delta || got.Total != c.total {
				t.Fatalf("measurement = %+v, want delta %d of %d", got, c.delta, c.total)
			}
			if want := gitx.RatioOf(c.delta, c.total); got.Ratio != want {
				t.Errorf("ratio = %v, want %v", got.Ratio, want)
			}
			if got.Previous != "aaaaaaa" || got.Current != "bbbbbbb" {
				t.Errorf("heads = %q..%q, want aaaaaaa..bbbbbbb", got.Previous, got.Current)
			}
		})
	}
	truncated := deliveryDiffOf("bbbbbbb", deliveryPatch("feature line", "three", "branch three"))
	truncated.Patch, truncated.Truncated = "", true
	for _, c := range []struct {
		name              string
		previous, current *gitx.Diff
	}{
		{"a truncated current diff", before, truncated},
		{"a truncated previous diff", truncated, before},
		{"no diff at all", nil, before},
	} {
		if _, ok := gitx.ChangeSetRatio(c.previous, c.current); ok {
			t.Errorf("%s: ok = true, want the tip-to-tip fallback", c.name)
		}
	}
}

// TestDelivererResolvesARealRebaseConflict drives the whole D-10 conflict
// path against real git: the delivery rebases the bead's branch onto a
// default branch that moved, git stops on a conflict and leaves the worktree
// mid-rebase, the engineer resolves it, and D-09 decides whether the bead is
// re-delivered or reviewed again.
//
// This is the case the fakes cannot express, and it is why the D-09 baseline
// is taken before the delivery rather than when the conflict is found: a
// worktree stopped mid-rebase has the default branch's own tip as HEAD, so a
// diff taken then is empty, every resolution scores as a full rewrite, and
// the "re-deliver without a new review" half of D-10 can never happen.
func TestDelivererResolvesARealRebaseConflict(t *testing.T) {
	t.Parallel()

	t.Run("a resolution that only settles the conflict is re-delivered", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newDeliveryRealRig(t)
		r.conflictingBranch()
		out := r.deliverExpectingAConflict(ctx)

		p, ok := r.PendingFor(r.bead.ID)
		if !ok {
			t.Fatal("no conflict round is pending")
		}
		if p.Diff == nil || p.Diff.Empty() {
			t.Fatalf("the D-09 baseline = %+v, want the branch's change set (a diff of a mid-rebase HEAD is empty)", p.Diff)
		}
		if n := p.Diff.ChangedLines(); n != 22 {
			t.Errorf("baseline changed lines = %d, want 22", n)
		}
		if got := out.Conflict.Files; !deliveryEqualStrings(got, []string{"conflict.txt"}) {
			t.Errorf("conflict files = %v, want [conflict.txt]", got)
		}

		// P6-16(b) at the adapter: the leftovers commit is refused here,
		// and asking for it leaves the rebase exactly as git left it.
		if _, err := r.git.CommitLeftovers(ctx, r.bead, 1); !errors.Is(err, ErrRebaseInProgress) {
			t.Errorf("CommitLeftovers mid-rebase: err = %v, want ErrRebaseInProgress", err)
		}
		if !r.wt.RebaseInProgress() {
			t.Fatal("the rebase state is gone")
		}
		if body := r.read("conflict.txt"); !strings.Contains(body, "<<<<<<<") {
			t.Errorf("conflict.txt = %q, want git's markers still there", body)
		}

		r.resolve("feature line")
		res, err := r.Resolved(ctx, r.bead.ID)
		if err != nil {
			t.Fatalf("Resolved: %v", err)
		}
		if res.Kind != DeliveryMerged || res.State != domain.BeadStateClosed {
			t.Fatalf("outcome = %s/%s, want merged/closed (%s)", res.Kind, res.State, res.Summary())
		}
		if res.Ratio.Delta == 0 || res.Ratio.Total == 0 {
			t.Errorf("D-09 measurement = %+v, want the resolution measured, not assumed", res.Ratio)
		}
		if res.Ratio.Ratio > gitx.DefaultReReviewThreshold {
			t.Errorf("D-09 ratio = %v (%+v), want at most %v: settling one conflicted line is not a rewrite",
				res.Ratio.Ratio, res.Ratio, gitx.DefaultReReviewThreshold)
		}
		if got := r.fixture.Git(t, "show", "main:conflict.txt"); !strings.Contains(got, "branch three and main three") {
			t.Errorf("main:conflict.txt = %q, want the resolved line", got)
		}
		want := []string{
			"approved→delivering", "delivering→in_progress", "in_progress→in_review",
			"in_review→approved", "approved→delivering", "delivering→closed",
		}
		if got := r.host.states(); !deliveryEqualStrings(got, want) {
			t.Errorf("state changes = %v, want %v", got, want)
		}
	})

	t.Run("the default branch moving underneath is not the engineer's work", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newDeliveryRealRig(t)
		r.conflictingBranch()
		// Somebody else lands twenty unrelated lines on the default
		// branch, which the rebase carries over the bead's own commit.
		r.fixture.WriteFile(t, "unrelated.txt", deliveryFeature("unrelated line"))
		r.fixture.Commit(t, "Someone else lands twenty lines")

		r.deliverExpectingAConflict(ctx)
		p, ok := r.PendingFor(r.bead.ID)
		if !ok {
			t.Fatal("no conflict round is pending")
		}
		r.resolve("feature line")

		// The two measurements of the very same resolution, side by side.
		current, err := r.git.Diff(ctx, r.bead.ID)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		tips, err := r.git.ChangeRatio(ctx, r.bead.ID, p.Diff, current)
		if err != nil {
			t.Fatalf("ChangeRatio: %v", err)
		}
		if !tips.Exceeds(gitx.DefaultReReviewThreshold) {
			t.Fatalf("tip-to-tip ratio = %+v, want it above the threshold: the point of this case is that it misjudges", tips)
		}
		sets, ok := gitx.ChangeSetRatio(p.Diff, current)
		if !ok {
			t.Fatal("the change sets could not be compared")
		}
		if sets.Exceeds(gitx.DefaultReReviewThreshold) {
			t.Errorf("change-set ratio = %+v, want at most the threshold: the engineer only settled one line", sets)
		}

		res, err := r.Resolved(ctx, r.bead.ID)
		if err != nil {
			t.Fatalf("Resolved: %v", err)
		}
		if res.Kind != DeliveryMerged {
			t.Fatalf("outcome = %s, want merged: a busy default branch must not cost a three-lens re-review (%s)",
				res.Kind, res.Summary())
		}
		if res.Ratio.Delta != sets.Delta || res.Ratio.Total != sets.Total {
			t.Errorf("D-09 measurement = %+v, want the change-set comparison %+v", res.Ratio, sets)
		}
	})

	t.Run("a resolution that rewrote the change set is reviewed again", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r := newDeliveryRealRig(t)
		r.conflictingBranch()
		r.deliverExpectingAConflict(ctx)

		// The engineer settles the conflict and rewrites the twenty lines
		// that had nothing to do with it.
		r.resolve("rewritten line")
		before := r.fixture.Git(t, "rev-parse", "main")
		res, err := r.Resolved(ctx, r.bead.ID)
		if err != nil {
			t.Fatalf("Resolved: %v", err)
		}
		if res.Kind != DeliveryNeedsReview || res.State != domain.BeadStateInReview {
			t.Fatalf("outcome = %s/%s, want needs_review/in_review (%s)", res.Kind, res.State, res.Summary())
		}
		if res.Ratio.Ratio <= gitx.DefaultReReviewThreshold {
			t.Errorf("D-09 ratio = %v (%+v), want above %v", res.Ratio.Ratio, res.Ratio, gitx.DefaultReReviewThreshold)
		}
		if after := r.fixture.Git(t, "rev-parse", "main"); after != before {
			t.Errorf("the default branch moved to %s, want %s: nothing ships before the review", after, before)
		}
	})
}

// deliveryRealRig is a real git repository with a bead worktree and a
// Deliverer wired to a real *gitx.Manager on both git seams: the safety-off
// path as production runs it, with no network, no user git configuration and
// no bd (the state machine writes to the recording store fake).
type deliveryRealRig struct {
	*Deliverer
	t       *testing.T
	fixture *testutil.FixtureRepo
	manager *gitx.Manager
	git     DeliveryGit
	wt      *gitx.Repo
	host    *deliveryHostFake
	store   *deliveryStoreFake
	bead    domain.Bead
}

// newDeliveryRealRig builds the fixture repository, commits the file both
// sides of the conflict will edit, and creates the bead's worktree from it.
func newDeliveryRealRig(t *testing.T) *deliveryRealRig {
	t.Helper()
	ctx := context.Background()
	f := testutil.NewFixtureRepo(t)
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	f.WriteFile(t, "conflict.txt", deliveryConflictFile("three"))
	f.Commit(t, "Add the file both sides edit")

	repo, err := gitx.Open(ctx, f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := gitx.NewManager(repo, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	bead := deliveryBead(domain.BeadStateApproved)
	if _, err := m.Create(ctx, bead); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wt, err := m.Open(ctx, bead.ID)
	if err != nil {
		t.Fatalf("Open the worktree: %v", err)
	}
	host, store, g := newDeliveryHost(false), &deliveryStoreFake{}, NewDeliveryGit(m)
	d, err := NewDeliverer(DeliveryOptions{
		Host:         host,
		Merge:        m,
		Git:          g,
		CheckCommand: "true",
		States: NewStateMachine(store,
			WithStatePublisher(PublisherFunc(host.Publish)),
			WithStateRun(host.ID()),
			WithStateClock(host.Now)),
	})
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	return &deliveryRealRig{
		Deliverer: d, t: t, fixture: f, manager: m, git: g, wt: wt,
		host: host, store: store, bead: bead,
	}
}

// conflictingBranch puts a twenty-line feature file and an edit of line three
// on the bead's branch, then edits the same line differently on the default
// branch, so the delivery's rebase must stop.
func (r *deliveryRealRig) conflictingBranch() {
	r.t.Helper()
	r.write("feature.txt", deliveryFeature("feature line"))
	r.write("conflict.txt", deliveryConflictFile("branch three"))
	if _, err := r.wt.CommitAll(context.Background(), "Add the feature and edit line three"); err != nil {
		r.t.Fatalf("CommitAll: %v", err)
	}
	r.fixture.WriteFile(r.t, "conflict.txt", deliveryConflictFile("main three"))
	r.fixture.Commit(r.t, "Edit line three on main")
}

// deliverExpectingAConflict delivers the bead and fails the test unless git
// stopped on the conflict.
func (r *deliveryRealRig) deliverExpectingAConflict(ctx context.Context) *DeliveryOutcome {
	r.t.Helper()
	out, err := r.Deliver(ctx, DeliveryRequest{Bead: r.bead})
	if err != nil {
		r.t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryConflict || out.State != domain.BeadStateInProgress {
		r.t.Fatalf("outcome = %s/%s, want conflict/in_progress (%s)", out.Kind, out.State, out.Summary())
	}
	return out
}

// resolve is the engineer's conflict round: it settles the conflicted line,
// writes the feature file with the given line prefix (the same one leaves the
// change set alone, a different one rewrites it), and finishes the rebase the
// way the resolve-conflict prompt asks.
func (r *deliveryRealRig) resolve(feature string) {
	r.t.Helper()
	ctx := context.Background()
	r.write("conflict.txt", deliveryConflictFile("branch three and main three"))
	r.write("feature.txt", deliveryFeature(feature))
	if _, err := r.wt.Git(ctx, "add", "conflict.txt", "feature.txt"); err != nil {
		r.t.Fatalf("git add: %v", err)
	}
	if _, err := r.wt.Git(ctx, "rebase", "--continue"); err != nil {
		r.t.Fatalf("git rebase --continue: %v", err)
	}
	if r.wt.RebaseInProgress() {
		r.t.Fatal("the rebase is still in progress after --continue")
	}
}

// write puts content in the bead's worktree.
func (r *deliveryRealRig) write(rel, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.wt.Root, rel), []byte(content), 0o644); err != nil {
		r.t.Fatalf("writing %s: %v", rel, err)
	}
}

// read returns a file from the bead's worktree.
func (r *deliveryRealRig) read(rel string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.wt.Root, rel))
	if err != nil {
		r.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// deliveryConflictHarness is a deliverer whose first delivery conflicted and
// whose second would merge, with a conflict round pending: the state the
// tests of what happens after the engineer finishes start from.
func deliveryConflictHarness(t *testing.T) *deliveryHarness {
	t.Helper()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go"), nil}
		h.git.diffs = []*gitx.Diff{
			deliveryDiffOf("aaaaaaa", deliveryPatch("feature line", "three", "branch three")),
			deliveryDiffOf("bbbbbbb", deliveryPatch("feature line", "main three", "branch three and main three")),
		}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryConflict {
		t.Fatalf("outcome = %s, want conflict", out.Kind)
	}
	return h
}

// deliveryConflictFile is the five-line file both sides of a conflict edit,
// with line three replaced by three.
func deliveryConflictFile(three string) string {
	return "one\ntwo\n" + three + "\nfour\nfive\n"
}

// deliveryFeature is the twenty-line file a bead's branch adds, numbered
// after prefix; two calls with different prefixes share no line.
func deliveryFeature(prefix string) string {
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

// deliveryPatch renders the unified diff of that change set: a new
// twenty-line feature file plus one edited line of a five-line file, where
// base is the line the change set replaces and resolved the line it puts
// there. Two calls that differ only in base and resolved are what a conflict
// resolution that touched nothing else looks like after a rebase — base
// moves because the merge-base did.
func deliveryPatch(feature, base, resolved string) string {
	var b strings.Builder
	b.WriteString("diff --git a/feature.txt b/feature.txt\n" +
		"new file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1,20 @@\n")
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "+%s %d\n", feature, i)
	}
	b.WriteString("diff --git a/conflict.txt b/conflict.txt\n" +
		"index 2222222..3333333 100644\n--- a/conflict.txt\n+++ b/conflict.txt\n@@ -1,5 +1,5 @@\n one\n two\n")
	fmt.Fprintf(&b, "-%s\n+%s\n four\n five\n", base, resolved)
	return b.String()
}

// deliveryDiffOf builds a *gitx.Diff around a unified diff, counting the
// patch's own files and changed lines into the stats, so a test's diff is as
// internally consistent as git's and the D-09 arithmetic divides by a real
// denominator.
func deliveryDiffOf(head, patch string) *gitx.Diff {
	d := &gitx.Diff{Stats: gitx.DiffStats{Base: "0000000", Head: head}, Patch: patch, MaxBytes: len(patch) + 1}
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			d.Stats.Files++
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			d.Stats.Insertions++
		case strings.HasPrefix(line, "-"):
			d.Stats.Deletions++
		}
	}
	d.Bytes = len(patch)
	return d
}

// deliveryEqualStrings compares two string slices, treating nil and empty as equal.
func deliveryEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// deliveryContains reports whether any element contains sub.
func deliveryContains(all []string, sub string) bool {
	for _, s := range all {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ------------------------------------------- one merge, one event (P6-40)

// TestDelivererAnnouncesOneMergePerBead is P6-40: a merge whose close
// failed is retried, and the retry must not announce the same commit a
// second time. Nothing is published until the bead is closed, and a stale
// caller that delivers the bead again afterwards announces nothing new.
func TestDelivererAnnouncesOneMergePerBead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	boom := errors.New("bd: blockers are still open")
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.store.failAfter("close", boom)
	})

	out, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if !errors.Is(err, boom) {
		t.Fatalf("Deliver: err = %v, want the close failure", err)
	}
	if out.Kind != DeliveryRetryable {
		t.Fatalf("outcome = %s, want retryable", out.Kind)
	}
	if n := len(h.host.published(events.KindMerged)); n != 0 {
		t.Fatalf("Merged events after the failed close = %d, want 0: the bead is not closed yet", n)
	}
	if h.announced("sc-68s") {
		t.Error("the merge counts as announced before any event was published")
	}

	// bd comes back and the caller retries the delivery it was told was
	// retryable; gitx finds the merge already landed and the close lands.
	h.store.failAfter("", nil)
	retry := DeliveryRequest{Bead: deliveryBead(domain.BeadStateDelivering)}
	out, err = h.Deliver(ctx, retry)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if out.Kind != DeliveryMerged || out.State != domain.BeadStateClosed {
		t.Fatalf("retry outcome = %s/%s, want merged/closed (%s)", out.Kind, out.State, out.Summary())
	}
	if n := len(h.host.published(events.KindMerged)); n != 1 {
		t.Fatalf("Merged events after the retry closed the bead = %d, want exactly 1", n)
	}
	if !h.announced("sc-68s") {
		t.Error("the merge was published but is not recorded as announced")
	}
	if !deliveryEventOrder(h.host, events.KindMerged, events.KindBeadStateChanged) {
		t.Error("Merged was published before the close transition; it must follow it (P6-40)")
	}

	// A caller still holding its "delivering" view delivers again. The
	// state machine writes the (idempotent) close a second time — that is
	// the caller's mistake, not the announcement's — but the merge is
	// announced only the once.
	if _, err := h.Deliver(ctx, retry); err != nil {
		t.Fatalf("the stale retry: %v", err)
	}
	if n := len(h.host.published(events.KindMerged)); n != 1 {
		t.Errorf("Merged events after a stale re-delivery = %d, want 1", n)
	}
}

// deliveryEventOrder reports whether the last event of kind a was
// published after at least one event of kind b.
func deliveryEventOrder(h *deliveryHostFake, a, b events.Kind) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, first := -1, -1
	for i, ev := range h.events {
		if ev.Kind() == a {
			last = i
		}
		if ev.Kind() == b && first < 0 {
			first = i
		}
	}
	return last > first && first >= 0
}

// ------------------------------- the gate poller ends a delivery (P6-41)

// deliveryGateStore is the [GateStore] the P6-41 test polls: one gh:pr gate
// that bd reports as resolved.
type deliveryGateStore struct {
	gate    beads.Gate
	results []beads.GateResult

	mu       sync.Mutex
	checks   int
	resolved []domain.BeadID
}

func (s *deliveryGateStore) CheckGates(context.Context, beads.GateCheckOptions) (beads.GateCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	if s.checks > 1 {
		// bd hands a merge verdict out exactly once; later polls see
		// nothing.
		return beads.GateCheck{}, nil
	}
	return beads.GateCheck{Checked: len(s.results), Resolved: len(s.results), Results: s.results}, nil
}

func (s *deliveryGateStore) ShowGate(_ context.Context, id domain.BeadID) (beads.Gate, error) {
	if id != s.gate.ID {
		return beads.Gate{}, fmt.Errorf("no such gate %s", id)
	}
	return s.gate, nil
}

func (s *deliveryGateStore) ResolveGate(_ context.Context, id domain.BeadID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, id)
	return nil
}

// deliveryBeadStatus is the [GateBeadReader] and [ResumeBeadReader] of
// these tests: the status bd holds for each bead, and an optional failure.
type deliveryBeadStatus struct {
	mu       sync.Mutex
	statuses map[domain.BeadID]string
	err      error
	shown    []domain.BeadID
}

func (r *deliveryBeadStatus) Show(_ context.Context, ids ...domain.BeadID) ([]beads.Issue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shown = append(r.shown, ids...)
	if r.err != nil {
		return nil, r.err
	}
	out := make([]beads.Issue, 0, len(ids))
	for _, id := range ids {
		status, ok := r.statuses[id]
		if !ok {
			continue
		}
		out = append(out, beads.Issue{ID: id, Status: status})
	}
	return out, nil
}

// deliveryGateClock is a [beads.Clock] that never advances: nothing in a
// single poll reads the wall clock.
type deliveryGateClock struct{}

func (deliveryGateClock) Now() time.Time { return deliveryClock }

func (deliveryGateClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// TestDelivererForgetsABeadTheGatePollerClosed is P6-41: a safety-on
// delivery correctly stops at delivering behind its gh:pr gate, so nothing
// inside the deliverer ends it. The gate poller that closes the bead is
// what tells it, through [Deliverer.Observe], and the per-bead bookkeeping
// of that delivery goes with it.
func TestDelivererForgetsABeadTheGatePollerClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newDeliveryHarness(t, true, func(_ *DeliveryOptions, h *deliveryHarness) {
		// The first delivery finds the worktree mid-rebase, which is the
		// one way a safety-on delivery raises a conflict round; the probes
		// after it are clean.
		h.git.rebase = []RebaseState{{InProgress: true, Branch: "sc/sc-68s-add-retry", Files: []string{"internal/a.go"}}}
	})

	first, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if first.Kind != DeliveryConflict {
		t.Fatalf("first outcome = %s, want conflict (%s)", first.Kind, first.Summary())
	}
	// The engineer resolves it; with no baseline to measure against D-09
	// errs on the side of a review, so the bead goes back through one.
	again, err := h.Resolved(ctx, "sc-68s")
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if again.Kind != DeliveryNeedsReview {
		t.Fatalf("resolution outcome = %s, want needs_review (%s)", again.Kind, again.Summary())
	}
	out, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)})
	if err != nil {
		t.Fatalf("the delivery after the review: %v", err)
	}
	if out.Kind != DeliveryPending || out.Gate == "" {
		t.Fatalf("outcome = %s, gate = %q, want pending behind a gate", out.Kind, out.Gate)
	}
	if n := h.tracking(); n != 1 {
		t.Fatalf("beads tracked after the delivery = %d, want 1 (the conflict round it had)", n)
	}

	// The human merges the pull request and the gate poller sees it.
	tee := PublisherFunc(func(ev events.Event) {
		h.host.Publish(ev)
		h.Observe(ev)
	})
	gates := &deliveryGateStore{
		gate: beads.Gate{
			ID: out.Gate, Status: "closed", Type: "gate", AwaitType: beads.GateTypePR,
			AwaitID: "7", Blocks: "sc-68s",
		},
		results: []beads.GateResult{{GateID: out.Gate, Outcome: beads.GateResolved, Detail: "PR 'x' was merged"}},
	}
	reader := &deliveryBeadStatus{statuses: map[domain.BeadID]string{"sc-68s": string(domain.BeadStateDelivering)}}
	poller, err := NewGatePoller(GatePollerOptions{
		Gates:  gates,
		Beads:  reader,
		Sink:   tee,
		Run:    h.host.ID(),
		Branch: "main",
		Clock:  deliveryGateClock{},
		States: NewStateMachine(&deliveryStoreFake{},
			WithStatePublisher(tee), WithStateRun(h.host.ID()), WithStateClock(h.host.Now)),
	})
	if err != nil {
		t.Fatalf("NewGatePoller: %v", err)
	}
	res, err := poller.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got, want := len(res.Closed), 1; got != want {
		t.Fatalf("beads closed by the poll = %d, want %d (%+v)", got, want, res)
	}
	if n := h.tracking(); n != 0 {
		t.Errorf("beads tracked after the gate poller closed the bead = %d, want 0 (P6-41)", n)
	}
	if _, ok := h.PendingFor("sc-68s"); ok {
		t.Error("a conflict round is still registered for a closed bead")
	}
}

// TestDelivererSettleIsSelectiveAndIdempotent covers the rest of the P6-41
// seam: an escalated bead is still the delivery's business, a bead nobody
// tracked is a no-op, and Settle can be called twice.
func TestDelivererSettleIsSelectiveAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
	})
	if _, err := h.Deliver(ctx, DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if n := h.tracking(); n != 1 {
		t.Fatalf("beads tracked = %d, want 1", n)
	}
	meta := events.MetaAt(h.host.ID(), deliveryClock)
	h.Observe(events.BeadStateChanged{Meta: meta, Bead: "sc-68s", From: domain.BeadStateInReview, To: domain.BeadStateEscalated})
	if n := h.tracking(); n != 1 {
		t.Errorf("beads tracked after an escalation = %d, want 1: the ruling may yet send it back (D-08)", n)
	}
	h.Observe(events.BeadStateChanged{Meta: meta, Bead: "", To: domain.BeadStateClosed})
	h.Observe(events.RunStarted{Meta: meta})
	if n := h.tracking(); n != 1 {
		t.Errorf("beads tracked after events naming no bead = %d, want 1", n)
	}
	h.Settle("sc-other")
	if n := h.tracking(); n != 1 {
		t.Errorf("beads tracked after settling a bead nobody delivered = %d, want 1", n)
	}
	h.Observe(events.Merged{Meta: meta, Bead: "sc-68s", SHA: "abcdef1234567"})
	h.Settle("sc-68s")
	if n := h.tracking(); n != 0 {
		t.Errorf("beads tracked after the delivery ended = %d, want 0", n)
	}
}

// ------------------------------------------- the resume guard (P6-37)

// deliveryResumeStore is the [gitx.ResumeTracker] the guard writes through
// once it has allowed a write.
type deliveryResumeStore struct {
	mu       sync.Mutex
	calls    []string
	statuses map[domain.BeadID]domain.BeadState
	patches  []beads.Patch
	err      error
	closeErr error
}

func (s *deliveryResumeStore) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "status "+string(id)+" "+string(state))
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	if s.statuses == nil {
		s.statuses = map[domain.BeadID]domain.BeadState{}
	}
	s.statuses[id] = state
	return beads.Issue{ID: id, Status: string(state)}, nil
}

func (s *deliveryResumeStore) Update(_ context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "update "+string(id))
	s.patches = append(s.patches, p)
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	return beads.Issue{ID: id}, nil
}

func (s *deliveryResumeStore) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "close "+string(id))
	if s.closeErr != nil {
		return beads.Issue{}, s.closeErr
	}
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	return beads.Issue{ID: id, Status: "closed", CloseReason: reason}, nil
}

func (s *deliveryResumeStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// newDeliveryResumeTracker builds a guard over a store and a reader that
// says every named bead is in the given state.
func newDeliveryResumeTracker(t *testing.T, statuses map[domain.BeadID]string) (*ResumeTracker, *deliveryResumeStore, *deliveryBeadStatus) {
	t.Helper()
	store, reader := &deliveryResumeStore{}, &deliveryBeadStatus{statuses: statuses}
	tr, err := NewResumeTracker(ResumeTrackerOptions{
		Store: store, Beads: reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewResumeTracker: %v", err)
	}
	return tr, store, reader
}

// TestNewResumeTrackerNeedsAStoreAndAReader: a guard that cannot read the
// bead could not check anything, so it is refused rather than built.
func TestNewResumeTrackerNeedsAStoreAndAReader(t *testing.T) {
	t.Parallel()
	if _, err := NewResumeTracker(ResumeTrackerOptions{Beads: &deliveryBeadStatus{}}); !errors.Is(err, ErrNoResumeStore) {
		t.Errorf("without a store: err = %v, want ErrNoResumeStore", err)
	}
	if _, err := NewResumeTracker(ResumeTrackerOptions{Store: &deliveryResumeStore{}}); !errors.Is(err, ErrNoResumeReader) {
		t.Errorf("without a reader: err = %v, want ErrNoResumeReader", err)
	}
}

// TestResumeTrackerChecksEveryEdgeAgainstTheD07Table walks the writes a
// resume can make: the two it is allowed, the states it may not write at
// all, a foreign status, an already-applied write and a bd read failure.
func TestResumeTrackerChecksEveryEdgeAgainstTheD07Table(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a landed delivery closes", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateDelivering)})
		if _, err := tr.Update(ctx, "sc-1", beads.Patch{Metadata: beads.MergeMetadata("abcdef1234567")}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, err := tr.Close(ctx, "sc-1", "merged on resume"); err != nil {
			t.Fatalf("Close: %v", err)
		}
		want := []string{"update sc-1", "close sc-1"}
		if got := store.recorded(); !deliveryEqualStrings(got, want) {
			t.Errorf("bd calls = %v, want %v", got, want)
		}
	})

	t.Run("a lost pull request parks the bead", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateDelivering)})
		if _, err := tr.SetStatus(ctx, "sc-1", domain.BeadStateNeedsHuman); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		if got, want := store.recorded(), []string{"status sc-1 needs_human"}; !deliveryEqualStrings(got, want) {
			t.Errorf("bd calls = %v, want %v", got, want)
		}
	})

	t.Run("a state no resume writes is refused", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateDelivering)})
		_, err := tr.SetStatus(ctx, "sc-1", domain.BeadStateInProgress)
		if !errors.Is(err, ErrNotAResumeEdge) {
			t.Errorf("err = %v, want ErrNotAResumeEdge", err)
		}
		if got := store.recorded(); len(got) != 0 {
			t.Errorf("bd calls = %v, want none", got)
		}
	})

	t.Run("a bead already in the state is written again", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateNeedsHuman)})
		if _, err := tr.SetStatus(ctx, "sc-1", domain.BeadStateNeedsHuman); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		if got, want := store.recorded(), []string{"status sc-1 needs_human"}; !deliveryEqualStrings(got, want) {
			t.Errorf("bd calls = %v, want %v (bd is idempotent about a status it holds)", got, want)
		}
	})

	t.Run("a foreign status is left alone", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": "triaging"})
		_, err := tr.SetStatus(ctx, "sc-1", domain.BeadStateNeedsHuman)
		if !errors.Is(err, ErrForeignStatus) {
			t.Errorf("err = %v, want ErrForeignStatus (P6-12)", err)
		}
		if got := store.recorded(); len(got) != 0 {
			t.Errorf("bd calls = %v, want none", got)
		}
	})

	t.Run("a bead bd cannot describe is not written", func(t *testing.T) {
		t.Parallel()
		tr, store, reader := newDeliveryResumeTracker(t, nil)
		_, err := tr.SetStatus(ctx, "sc-1", domain.BeadStateNeedsHuman)
		if !errors.Is(err, ErrResumeStateUnknown) {
			t.Errorf("err = %v, want ErrResumeStateUnknown", err)
		}
		reader.err = errors.New("bd is down")
		_, err = tr.Close(ctx, "sc-1", "merged")
		if !errors.Is(err, ErrResumeStateUnknown) {
			t.Errorf("with bd down: err = %v, want ErrResumeStateUnknown", err)
		}
		if got := store.recorded(); len(got) != 0 {
			t.Errorf("bd calls = %v, want none", got)
		}
	})

	t.Run("a status patch is guarded like a status write", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateFailed)})
		_, err := tr.Update(ctx, "sc-1", beads.Patch{Status: domain.BeadStateNeedsHuman})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("err = %v, want ErrInvalidTransition", err)
		}
		if got := store.recorded(); len(got) != 0 {
			t.Errorf("bd calls = %v, want none", got)
		}
	})

	t.Run("the store's own failures come back unwrapped", func(t *testing.T) {
		t.Parallel()
		tr, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-1": string(domain.BeadStateDelivering)})
		store.closeErr = fmt.Errorf("beads: close: %w", beads.ErrBlockedOpen)
		if _, err := tr.Close(ctx, "sc-1", "merged"); !errors.Is(err, beads.ErrBlockedOpen) {
			t.Errorf("err = %v, want beads.ErrBlockedOpen so gitx leaves the bead to the gate poller", err)
		}
	})
}

// TestResumeEventNamesOnlyTheEdgesAResumeTakes pins the little table the
// guard picks its event from.
func TestResumeEventNamesOnlyTheEdgesAResumeTakes(t *testing.T) {
	t.Parallel()
	want := map[domain.BeadState]BeadEvent{
		domain.BeadStateClosed:     BeadEventDelivered,
		domain.BeadStateNeedsHuman: BeadEventBlocked,
		domain.BeadStateFailed:     BeadEventFailed,
	}
	for _, state := range domain.BeadStates() {
		ev, ok := ResumeEvent(state)
		wantEv, wantOK := want[state]
		if ok != wantOK || ev != wantEv {
			t.Errorf("ResumeEvent(%s) = %q, %v, want %q, %v", state, ev, ok, wantEv, wantOK)
		}
	}
}

// TestResumeRefusesAnEdgeTheD07TableHasNot is the P6-37 exit criterion,
// driven through the real gitx.Resumer: a bead somebody failed by hand
// whose pull request was closed without merging reconciles to needs_human,
// which D-07 has no edge to from failed, so nothing is written and the
// entry says why.
//
// It cannot run in parallel: the gh shim is installed with t.Setenv.
func TestResumeRefusesAnEdgeTheD07TableHasNot(t *testing.T) {
	ctx := context.Background()
	// gh answers "closed without merging" for pull request 7, which is
	// what gitx reads as "the recorded pull request is gone".
	testutil.FakeBin(t, "gh", `
case "$1 $2" in
  "pr view") echo '{"number":7,"url":"https://example.test/pull/7","state":"CLOSED","title":"abandoned","headRefName":"sc/sc-68s-add-retry"}' ;;
  *) echo "gh shim: unexpected command: $*" >&2; exit 1 ;;
esac
`)
	f := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	repo, err := gitx.Open(ctx, f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := gitx.NewManager(repo, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tracker, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{"sc-68s": string(domain.BeadStateFailed)})
	resumer, err := gitx.NewResumer(m, tracker, gitx.WithResumeLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewResumer: %v", err)
	}
	bead := deliveryBead(domain.BeadStateFailed)
	bead.PRNumber, bead.PRURL = 7, "https://example.test/pull/7"

	res, err := resumer.Resume(ctx, []domain.Bead{bead})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.State != domain.BeadStateNeedsHuman {
		t.Fatalf("reconciled state = %s, want needs_human (%s)", e.State, e.Reason)
	}
	if e.Applied {
		t.Error("the resume applied an edge the D-07 table has not got")
	}
	if !errors.Is(e.Err, ErrInvalidTransition) {
		t.Errorf("entry error = %v, want ErrInvalidTransition", e.Err)
	}
	if got := store.recorded(); len(got) != 0 {
		t.Errorf("bd calls = %v, want none: nothing may be written", got)
	}
	if !strings.Contains(res.Summary(), "not applied") {
		t.Errorf("summary = %q, want it to say the change was not applied", res.Summary())
	}
}

// TestResumeAppliesTheEdgesTheTableHasGot is the other half: the same rig
// with a delivering bead whose branch landed writes the close it owes, so
// the guard blocks nothing a resume legitimately does.
func TestResumeAppliesTheEdgesTheTableHasGot(t *testing.T) {
	ctx := context.Background()
	f := testutil.NewFixtureRepo(t)
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	repo, err := gitx.Open(ctx, f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, err := gitx.NewManager(repo, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	bead := deliveryBead(domain.BeadStateDelivering)
	if _, err := m.Create(ctx, bead); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wt, err := m.Open(ctx, bead.ID)
	if err != nil {
		t.Fatalf("Open the worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Root, "landed.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatalf("writing the work: %v", err)
	}
	if _, err := wt.CommitAll(ctx, "the work"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	f.Git(t, "merge", "--ff-only", "-q", gitx.BranchName(bead))

	tracker, store, _ := newDeliveryResumeTracker(t, map[domain.BeadID]string{bead.ID: string(domain.BeadStateDelivering)})
	resumer, err := gitx.NewResumer(m, tracker, gitx.WithoutGH(),
		gitx.WithResumeLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewResumer: %v", err)
	}
	res, err := resumer.Resume(ctx, []domain.Bead{bead})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	e := res.Entries[0]
	if e.State != domain.BeadStateClosed || !e.Applied {
		t.Fatalf("entry = %s, applied %v, want closed and applied (%s: %v)", e.State, e.Applied, e.Reason, e.Err)
	}
	want := []string{"update " + string(bead.ID), "close " + string(bead.ID)}
	if got := store.recorded(); !deliveryEqualStrings(got, want) {
		t.Errorf("bd calls = %v, want %v", got, want)
	}
}
