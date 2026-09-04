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
		return res, err
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
	leftovers *gitx.Leftovers
	leftErr   error
	diffs     []*gitx.Diff
	diffErr   error
	ratio     gitx.ChangeRatio
	ratioErr  error
	branchErr error

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
		return RebaseState{}, g.rebaseErr
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
		h.git.ratio = gitx.ChangeRatio{Previous: "aaaaaaa", Current: "bbbbbbb", Delta: 4, Total: 100, Ratio: 0.04}
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
}

// TestDelivererReReviewsABigConflictResolution is the D-09 half of the same
// rule: a resolution that changed more than the threshold goes back for a
// full review instead of being re-delivered.
func TestDelivererReReviewsABigConflictResolution(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.errs = []error{deliveryConflictErr("internal/a.go")}
		h.git.ratio = gitx.ChangeRatio{Previous: "aaaaaaa", Current: "bbbbbbb", Delta: 80, Total: 100, Ratio: 0.8}
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
		h.git.ratio = gitx.ChangeRatio{Ratio: 0.01}
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
	if n := len(h.host.published(events.KindMerged)); n != 1 {
		t.Errorf("Merged events = %d, want 1: the work did land", n)
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
