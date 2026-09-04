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
	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
	"slop-cannon/internal/tui"
)

// Tests for the external pull request reviewer (P8-04, P8-11, D-11). Every
// helper is prefixed "prv" so it cannot collide with another task's file in
// this package.
//
// Nothing here reaches GitHub: the poller and the poster are fakes, except
// in TestPRReviewWithARealGHShim, which drives the production
// gitx.PRWatcher and gitx.GH over a `gh` shim, and in the checkout test,
// which uses a real git fixture with a bare remote. No pull request of any
// kind is ever merged, because there is no code that could.

// prvEpoch is the fake clock's first tick.
var prvEpoch = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// prvClock is a deterministic clock: every read advances one second.
type prvClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *prvClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		c.at = prvEpoch
	}
	c.at = c.at.Add(time.Second)
	return c.at
}

// prvLogger is a logger that writes nowhere.
func prvLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// prvSink collects everything published.
type prvSink struct {
	mu  sync.Mutex
	evs []events.Event
}

func (s *prvSink) Publish(ev events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *prvSink) events() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event(nil), s.evs...)
}

// prvOfKind returns every published event of one concrete type.
func prvOfKind[T events.Event](s *prvSink) []T {
	var out []T
	for _, ev := range s.events() {
		if t, ok := ev.(T); ok {
			out = append(out, t)
		}
	}
	return out
}

// prvWatcher is a scripted [PRPoller].
type prvWatcher struct {
	mu      sync.Mutex
	results []*gitx.PollResult
	errs    []error
	polls   int
}

func (w *prvWatcher) Interval() time.Duration { return 10 * time.Millisecond }

func (w *prvWatcher) Poll(context.Context) (*gitx.PollResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.polls++
	var (
		res *gitx.PollResult
		err error
	)
	if len(w.results) > 0 {
		res, w.results = w.results[0], w.results[1:]
	}
	if len(w.errs) > 0 {
		err, w.errs = w.errs[0], w.errs[1:]
	}
	return res, err
}

func (w *prvWatcher) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.polls
}

// prvPoster records the reviews it was asked to post. It builds the review
// exactly as gitx.GH does, so the outcome the reviewer records is the one
// GitHub would have seen.
type prvPoster struct {
	mu       sync.Mutex
	posted   []gitx.Review
	err      error
	failures int
}

func (p *prvPoster) PostReview(_ context.Context, pr gitx.PullRequest, verdicts []domain.Verdict) (gitx.Review, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		p.failures++
		return gitx.Review{}, p.err
	}
	review, err := gitx.BuildReview(pr, verdicts)
	if err != nil {
		return gitx.Review{}, err
	}
	p.posted = append(p.posted, review)
	return review, nil
}

func (p *prvPoster) reviews() []gitx.Review {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gitx.Review(nil), p.posted...)
}

// prvStore records the bead writes.
type prvStore struct {
	mu       sync.Mutex
	closed   map[domain.BeadID]string
	statuses map[domain.BeadID]domain.BeadState
	err      error
}

func prvNewStore() *prvStore {
	return &prvStore{closed: map[domain.BeadID]string{}, statuses: map[domain.BeadID]domain.BeadState{}}
}

func (s *prvStore) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	s.closed[id] = reason
	return beads.Issue{ID: id, Status: string(domain.BeadStateClosed)}, nil
}

func (s *prvStore) SetStatus(_ context.Context, id domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	s.statuses[id] = st
	return beads.Issue{ID: id, Status: string(st)}, nil
}

func (s *prvStore) closeReason(id domain.BeadID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.closed[id]
	return r, ok
}

func (s *prvStore) status(id domain.BeadID) (domain.BeadState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.statuses[id]
	return st, ok
}

// prvParker records the transitions a [BeadParker] was asked for.
type prvParker struct {
	mu       sync.Mutex
	requests []StateRequest
	err      error
}

func (p *prvParker) Apply(_ context.Context, req StateRequest) (StateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.err != nil {
		return StateResult{}, p.err
	}
	to, err := Transition(req.From, req.Event)
	if err != nil {
		return StateResult{}, err
	}
	return StateResult{From: req.From, To: to, Persisted: true}, nil
}

func (p *prvParker) calls() []StateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]StateRequest(nil), p.requests...)
}

// prvMessages records the bead comments.
type prvMessages struct {
	mu   sync.Mutex
	msgs []any
}

func (m *prvMessages) PostMessage(_ context.Context, _ domain.BeadID, _ string, msg any) (beads.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return beads.Message{}, nil
}

func (m *prvMessages) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

// prvCheckout hands back a canned workspace.
type prvCheckout struct {
	ws    PRWorkspace
	err   error
	mu    sync.Mutex
	calls int
}

func (c *prvCheckout) Checkout(context.Context, domain.Bead, gitx.PullRequest) (PRWorkspace, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return PRWorkspace{}, c.err
	}
	return c.ws, nil
}

func (c *prvCheckout) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// prvPR is the pull request every test reviews.
func prvPR() gitx.PullRequest {
	return gitx.PullRequest{
		Number:  7,
		Title:   "add a retry to the uploader",
		URL:     "https://github.com/acme/widget/pull/7",
		Author:  "octocat",
		HeadRef: "feature/retry",
		BaseRef: "main",
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	}
}

// prvIssue is the review bead the watcher records for pr.
func prvIssue(pr gitx.PullRequest, status domain.BeadState) beads.Issue {
	return beads.Issue{
		ID:     "sc-ext7",
		Title:  gitx.ReviewBeadTitle(pr),
		Type:   string(domain.BeadTypeTask),
		Status: string(status),
		Labels: []string{beads.LabelExternalPR},
		Metadata: beads.Metadata{
			beads.MetaPRNumber: float64(pr.Number),
			beads.MetaPRURL:    pr.URL,
		},
	}
}

// prvResult is a poll that found one new external pull request.
func prvResult(pr gitx.PullRequest) *gitx.PollResult {
	return &gitx.PollResult{
		External: []gitx.PullRequest{pr},
		New:      []gitx.ReviewBead{{Issue: prvIssue(pr, domain.BeadStateOpen), PR: pr}},
	}
}

// prvRig is one reviewer with everything around it.
type prvRig struct {
	review   *PRReview
	sink     *prvSink
	watcher  *prvWatcher
	poster   *prvPoster
	store    *prvStore
	parker   *prvParker
	messages *prvMessages
	checkout *prvCheckout
}

// prvNew builds a reviewer over the scripted parts, with opts applied to
// the options before construction.
func prvNew(t *testing.T, results []*gitx.PollResult, edit func(*PRReviewOptions)) *prvRig {
	t.Helper()
	clock := &prvClock{}
	rig := &prvRig{
		sink:     &prvSink{},
		watcher:  &prvWatcher{results: results},
		poster:   &prvPoster{},
		store:    prvNewStore(),
		parker:   &prvParker{},
		messages: &prvMessages{},
		checkout: &prvCheckout{ws: PRWorkspace{
			Worktree: t.TempDir(),
			Branch:   "feature/retry",
			Base:     "main",
			HeadSHA:  "0123456789abcdef0123456789abcdef01234567",
			Diff:     prvDiff(),
		}},
	}
	opts := PRReviewOptions{
		Watcher:  rig.watcher,
		Poster:   rig.poster,
		Beads:    rig.store,
		States:   rig.parker,
		Messages: rig.messages,
		Checkout: rig.checkout,
		Sink:     rig.sink,
		Run:      "run-prv",
		Clock:    clock.now,
		Logger:   prvLogger(),
	}
	if edit != nil {
		edit(&opts)
	}
	r, err := NewPRReview(opts)
	if err != nil {
		t.Fatalf("NewPRReview: %v", err)
	}
	rig.review = r
	return rig
}

// prvDiff is the change set a tester reviews.
func prvDiff() *gitx.Diff {
	return &gitx.Diff{
		Stats: gitx.DiffStats{
			Base: "aaaaaaa", Head: "bbbbbbb", Files: 1, Insertions: 12, Deletions: 2,
			Changed: []gitx.FileStat{{Path: "uploader.go", Insertions: 12, Deletions: 2}},
		},
		Files: []gitx.DiffFile{{
			FileStat: gitx.FileStat{Path: "uploader.go", Insertions: 12, Deletions: 2},
			Status:   gitx.DiffModified,
		}},
		Patch: "--- a/uploader.go\n+++ b/uploader.go\n@@\n+\tfor attempt := 0; attempt < 3; attempt++ {\n",
	}
}

// prvVerdict is one lens's answer.
func prvVerdict(l domain.Lens, outcome domain.VerdictOutcome) domain.Verdict {
	v := domain.Verdict{Lens: l, Round: PRReviewRound, Outcome: outcome, Summary: "looked at the retry loop"}
	if outcome == domain.VerdictChanges {
		v.Items = []domain.FeedbackItem{{
			ID: string(l) + "-1", Severity: domain.SeverityMajor, Category: "correctness",
			Title: "the retry never backs off", File: "uploader.go", Line: 12, Status: domain.ItemOpen,
		}}
	}
	return v
}

// prvAnswer drives one lens's verdict back through the firing the reviewer
// planned for it, which is the only path a verdict ever takes.
func prvAnswer(t *testing.T, rig *prvRig, d Demand, outcome domain.VerdictOutcome) {
	t.Helper()
	af, err := rig.review.PlanFiring(context.Background(), Firing{
		Agent:  domain.Agent{ID: domain.AgentID("tester-" + string(d.Lens) + "-sc-ext7-r1"), Role: domain.RoleTester, Lens: d.Lens, Bead: d.Bead},
		Demand: d,
	})
	if err != nil {
		t.Fatalf("PlanFiring(%s): %v", d.Lens, err)
	}
	if af.Result == nil {
		t.Fatalf("PlanFiring(%s): no result callback", d.Lens)
	}
	if err := af.Result(prvVerdict(d.Lens, outcome), agents.Transcript{}); err != nil {
		t.Fatalf("Result(%s): %v", d.Lens, err)
	}
}

// prvDemands is the reviewer's outstanding work.
func prvDemands(t *testing.T, r *PRReview) []Demand {
	t.Helper()
	ds, err := r.Demands(context.Background())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	return ds
}

// TestPRReviewAdoptsAPullRequestAndOffersOneDemandPerLens covers the first
// half of the wiring: the watcher's review bead becomes work for the
// ordinary testers, and the run's readers learn that an external pull
// request exists at all.
func TestPRReviewAdoptsAPullRequestAndOffersOneDemandPerLens(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, nil)

	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	created := prvOfKind[events.BeadCreated](rig.sink)
	if len(created) != 1 || created[0].Bead.ID != "sc-ext7" {
		t.Fatalf("BeadCreated = %+v, want one for sc-ext7", created)
	}
	opened := prvOfKind[events.PROpened](rig.sink)
	if len(opened) != 1 {
		t.Fatalf("PROpened = %+v, want one", opened)
	}
	switch {
	case !opened[0].External:
		t.Error("PROpened.External = false, want true for a pull request the cannon did not open")
	case opened[0].Number != 7 || opened[0].URL != pr.URL:
		t.Errorf("PROpened = %+v, want #7 at %s", opened[0], pr.URL)
	case opened[0].Branch != pr.HeadRef || opened[0].Base != pr.BaseRef:
		t.Errorf("PROpened branches = %q -> %q, want %q -> %q", opened[0].Branch, opened[0].Base, pr.HeadRef, pr.BaseRef)
	}
	rounds := prvOfKind[events.RoundStarted](rig.sink)
	if len(rounds) != 1 || len(rounds[0].Lenses) != len(domain.Lenses()) {
		t.Fatalf("RoundStarted = %+v, want one announcing every lens", rounds)
	}
	if rounds[0].Commit != pr.HeadSHA {
		t.Errorf("RoundStarted.Commit = %q, want the pull request head %q", rounds[0].Commit, pr.HeadSHA)
	}

	ds := prvDemands(t, rig.review)
	if len(ds) != len(domain.Lenses()) {
		t.Fatalf("Demands = %d, want one per lens", len(ds))
	}
	seen := map[domain.Lens]bool{}
	for _, d := range ds {
		seen[d.Lens] = true
		switch {
		case d.Kind != DemandReview:
			t.Errorf("demand kind = %s, want %s", d.Kind, DemandReview)
		case d.Bead != "sc-ext7":
			t.Errorf("demand bead = %s, want sc-ext7", d.Bead)
		case d.Round != PRReviewRound:
			t.Errorf("demand round = %d, want %d", d.Round, PRReviewRound)
		case d.Claim != ClaimNone:
			t.Errorf("demand claim = %q, want none: a tester never owns the review bead", d.Claim)
		case d.FiringTask() != events.TaskReview:
			t.Errorf("demand task = %s, want %s", d.FiringTask(), events.TaskReview)
		}
		if !rig.review.Handles(d) {
			t.Errorf("Handles(%s) = false, want true", d.Lens)
		}
	}
	for _, l := range domain.Lenses() {
		if !seen[l] {
			t.Errorf("no demand for lens %s", l)
		}
	}
	if rig.review.Handles(Demand{Kind: DemandImplement, Bead: "sc-other"}) {
		t.Error("Handles(a demand of the run's own work) = true, want false")
	}
	if st := rig.review.Stats(); st.Polls != 1 || st.Created != 1 || st.Tracked != 1 {
		t.Errorf("Stats = %+v, want one poll, one created, one tracked", st)
	}
}

// TestPRReviewLeavesASettledReviewBeadAlone stops a resumed run from
// reviewing (and posting on) a pull request an earlier run already
// finished with.
func TestPRReviewLeavesASettledReviewBeadAlone(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	res := &gitx.PollResult{
		External: []gitx.PullRequest{pr},
		Existing: []gitx.ReviewBead{{Issue: prvIssue(pr, domain.BeadStateClosed), PR: pr}},
	}
	rig := prvNew(t, []*gitx.PollResult{res}, nil)

	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if ds := prvDemands(t, rig.review); len(ds) != 0 {
		t.Fatalf("Demands = %+v, want none for a closed review bead", ds)
	}
	if evs := prvOfKind[events.BeadCreated](rig.sink); len(evs) != 0 {
		t.Errorf("BeadCreated = %+v, want none: the bead already existed", evs)
	}
}

// TestPRReviewPlansATesterAgainstThePullRequestsOwnCode covers the firing:
// the tester runs in the checkout of the pull request, reviews its diff,
// and is told what it is looking at and what the cannon will never do
// with it.
func TestPRReviewPlansATesterAgainstThePullRequestsOwnCode(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)

	af, err := rig.review.PlanFiring(context.Background(), Firing{
		Agent:  domain.Agent{ID: "tester-qa-sc-ext7-r1", Role: domain.RoleTester, Lens: ds[0].Lens, Bead: "sc-ext7"},
		Demand: ds[0],
	})
	if err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	switch {
	case af.Worktree != rig.checkout.ws.Worktree:
		t.Errorf("firing worktree = %q, want the pull request checkout %q", af.Worktree, rig.checkout.ws.Worktree)
	case !af.NoLease:
		t.Error("firing takes a lease on the review bead; a tester never holds one")
	case af.Round != PRReviewRound:
		t.Errorf("firing round = %d, want %d", af.Round, PRReviewRound)
	case af.Agent.Role != domain.RoleTester:
		t.Errorf("firing role = %s, want tester", af.Agent.Role)
	}

	run, err := af.Build(context.Background(), FiringContext{
		Run: "run-prv", Agent: af.Agent, Task: af.Task, Round: af.Round,
		Workdir: af.Worktree, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	prompt := run.Spec.Prompt
	for _, want := range []string{
		"external pull request #7",
		"never merges a pull request it did not open",
		"uploader.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the tester prompt does not mention %q", want)
		}
	}
	if run.Message != domain.MessageVerdict {
		t.Errorf("firing message = %s, want %s", run.Message, domain.MessageVerdict)
	}

	// However many lenses review it, the pull request is fetched once.
	for _, d := range ds[1:] {
		if _, err := rig.review.PlanFiring(context.Background(), Firing{
			Agent:  domain.Agent{ID: domain.AgentID("tester-" + string(d.Lens)), Role: domain.RoleTester, Lens: d.Lens, Bead: "sc-ext7"},
			Demand: d,
		}); err != nil {
			t.Fatalf("PlanFiring(%s): %v", d.Lens, err)
		}
	}
	if n := rig.checkout.count(); n != 1 {
		t.Errorf("checkouts = %d, want 1 for %d testers", n, len(ds))
	}
}

// TestPRReviewRefusesToPlanWithoutACheckout: a tester cannot review code
// nobody fetched, and saying so beats firing an agent at an empty
// directory.
func TestPRReviewRefusesToPlanWithoutACheckout(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, func(o *PRReviewOptions) { o.Checkout = nil })
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	_, err := rig.review.PlanFiring(context.Background(), Firing{
		Agent:  domain.Agent{ID: "tester-qa-sc-ext7-r1", Role: domain.RoleTester, Lens: ds[0].Lens, Bead: "sc-ext7"},
		Demand: ds[0],
	})
	if !errors.Is(err, ErrNoPRCheckout) {
		t.Fatalf("PlanFiring without a checkout: err = %v, want ErrNoPRCheckout", err)
	}
}

// TestPRReviewPostsOneConsolidatedReviewAndClosesTheBead is D-11's second
// half: one review for the whole round, whatever the lenses said, and a
// review bead that does not sit open forever afterwards.
func TestPRReviewPostsOneConsolidatedReviewAndClosesTheBead(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)

	for i, d := range ds {
		prvAnswer(t, rig, d, domain.VerdictApprove)
		if i < len(ds)-1 {
			if got := len(rig.poster.reviews()); got != 0 {
				t.Fatalf("%d review(s) posted after %d of %d verdicts; the review is posted once, when the round is complete", got, i+1, len(ds))
			}
		}
	}

	posted := rig.poster.reviews()
	if len(posted) != 1 {
		t.Fatalf("posted %d reviews, want exactly 1", len(posted))
	}
	if posted[0].Event != gitx.ReviewApprove {
		t.Errorf("review event = %s, want %s", posted[0].Event, gitx.ReviewApprove)
	}
	if len(posted[0].Verdicts) != len(ds) {
		t.Errorf("review carries %d verdicts, want %d", len(posted[0].Verdicts), len(ds))
	}
	if got := len(prvOfKind[events.VerdictReceived](rig.sink)); got != len(ds) {
		t.Errorf("VerdictReceived = %d, want %d", got, len(ds))
	}
	if n := rig.messages.count(); n != len(ds) {
		t.Errorf("bead comments = %d, want one per verdict (D-06)", n)
	}

	reason, closed := rig.store.closeReason("sc-ext7")
	if !closed {
		t.Fatal("the review bead was not closed after the review went out")
	}
	if !strings.Contains(reason, "#7") || !strings.Contains(reason, "never merges") {
		t.Errorf("close reason = %q, want it to name the pull request and the promise D-11 makes", reason)
	}
	changes := prvOfKind[events.BeadStateChanged](rig.sink)
	if len(changes) != 1 || changes[0].To != domain.BeadStateClosed || changes[0].From != domain.BeadStateOpen {
		t.Fatalf("BeadStateChanged = %+v, want one open -> closed", changes)
	}
	if ds := prvDemands(t, rig.review); len(ds) != 0 {
		t.Errorf("Demands after the review went out = %+v, want none", ds)
	}

	// A second pass must not post a second review on the same pull request.
	rig.review.retryPosts(context.Background())
	if got := len(rig.poster.reviews()); got != 1 {
		t.Errorf("posted %d reviews after a retry pass, want 1", got)
	}
	st := rig.review.Stats()
	if st.Posted != 1 || st.Closed != 1 || st.Verdicts != len(domain.Lenses()) {
		t.Errorf("Stats = %+v, want one posted, one closed and %d verdicts", st, len(domain.Lenses()))
	}
	if rs := rig.review.Reviews(); len(rs) != 1 || !rs[0].Posted || rs[0].Outcome != domain.VerdictApprove {
		t.Errorf("Reviews = %+v, want one posted approval", rs)
	}
}

// TestPRReviewRequestsChangesWhenOneLensDoes pins the consolidation rule
// gitx.BuildReview implements, through the reviewer that feeds it.
func TestPRReviewRequestsChangesWhenOneLensDoes(t *testing.T) {
	t.Parallel()
	rig := prvNew(t, []*gitx.PollResult{prvResult(prvPR())}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	for i, d := range ds {
		outcome := domain.VerdictApprove
		if i == 1 {
			outcome = domain.VerdictChanges
		}
		prvAnswer(t, rig, d, outcome)
	}
	posted := rig.poster.reviews()
	if len(posted) != 1 || posted[0].Event != gitx.ReviewRequestChanges {
		t.Fatalf("posted = %+v, want one request-changes review", posted)
	}
	if rs := rig.review.Reviews(); len(rs) != 1 || rs[0].Outcome != domain.VerdictChanges {
		t.Errorf("Reviews = %+v, want the changes outcome", rs)
	}
}

// TestPRReviewRetriesAFailedPostAndThenAsksForAHuman: gh being unreachable
// must not lose the verdicts or leave a bead nobody will look at.
func TestPRReviewRetriesAFailedPostAndThenAsksForAHuman(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), {}, {}}, func(o *PRReviewOptions) { o.PostAttempts = 2 })
	rig.poster.err = errors.New("gh: HTTP 503")
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, d := range prvDemands(t, rig.review) {
		prvAnswer(t, rig, d, domain.VerdictApprove)
	}
	if rig.poster.failures != 1 {
		t.Fatalf("post attempts = %d, want 1 so far", rig.poster.failures)
	}

	// The next poll retries, and the attempt budget runs out.
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rig.poster.failures != 2 {
		t.Fatalf("post attempts = %d, want 2 after the retry", rig.poster.failures)
	}
	calls := rig.parker.calls()
	if len(calls) != 1 || calls[0].Event != BeadEventBlocked || calls[0].Bead != "sc-ext7" {
		t.Fatalf("parking calls = %+v, want one blocked transition for sc-ext7", calls)
	}
	if _, closed := rig.store.closeReason("sc-ext7"); closed {
		t.Error("the review bead was closed although its review never went out")
	}
	// A third poll must not keep hammering GitHub with a bead a human owns.
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if rig.poster.failures != 2 {
		t.Errorf("post attempts = %d, want 2: the bead is parked", rig.poster.failures)
	}
	if st := rig.review.Stats(); st.Parked != 1 {
		t.Errorf("Stats.Parked = %d, want 1", st.Parked)
	}
}

// TestPRReviewStaleReviewBeadIsClosed is P8-11's default: the pull request
// was merged or closed on GitHub while the cannon was reviewing it, so the
// review bead is closed and no tester is offered work on it again.
func TestPRReviewStaleReviewBeadIsClosed(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	vanished := &gitx.PollResult{Stale: []beads.Issue{prvIssue(pr, domain.BeadStateOpen)}}
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), vanished}, nil)
	ctx := context.Background()
	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(prvDemands(t, rig.review)) == 0 {
		t.Fatal("no demands before the pull request vanished")
	}

	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	reason, closed := rig.store.closeReason("sc-ext7")
	if !closed {
		t.Fatal("the review bead of a vanished pull request was left open")
	}
	if !strings.Contains(reason, "no longer open") {
		t.Errorf("close reason = %q, want it to say the pull request is gone", reason)
	}
	changes := prvOfKind[events.BeadStateChanged](rig.sink)
	if len(changes) != 1 || changes[0].To != domain.BeadStateClosed {
		t.Fatalf("BeadStateChanged = %+v, want one move to closed", changes)
	}
	if ds := prvDemands(t, rig.review); len(ds) != 0 {
		t.Errorf("Demands after the pull request vanished = %+v, want none", ds)
	}
	if len(rig.poster.reviews()) != 0 {
		t.Error("a review was posted on a pull request that is no longer open")
	}
	if st := rig.review.Stats(); st.Stale != 1 || st.Closed != 1 || st.Tracked != 0 {
		t.Errorf("Stats = %+v, want one stale, one closed, none tracked", st)
	}
}

// TestPRReviewStaleReviewBeadCanBeHandedToAHuman is P8-11's other choice.
func TestPRReviewStaleReviewBeadCanBeHandedToAHuman(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	vanished := &gitx.PollResult{Stale: []beads.Issue{prvIssue(pr, domain.BeadStateOpen)}}
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), vanished},
		func(o *PRReviewOptions) { o.Stale = StaleReviewNeedsHuman })
	ctx := context.Background()
	for range 2 {
		if err := rig.review.Sync(ctx); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	calls := rig.parker.calls()
	if len(calls) != 1 || calls[0].Event != BeadEventBlocked || calls[0].From != domain.BeadStateOpen {
		t.Fatalf("parking calls = %+v, want one blocked transition from open", calls)
	}
	if _, closed := rig.store.closeReason("sc-ext7"); closed {
		t.Error("the bead was closed although the policy asks for a human")
	}
	if st := rig.review.Stats(); st.Parked != 1 || st.Stale != 1 {
		t.Errorf("Stats = %+v, want one parked and one stale", st)
	}

	// With no state machine the same move goes straight to bd, so a
	// reviewer built without one still parks the bead.
	bare := prvNew(t, []*gitx.PollResult{prvResult(pr), vanished}, func(o *PRReviewOptions) {
		o.Stale, o.States = StaleReviewNeedsHuman, nil
	})
	for range 2 {
		if err := bare.review.Sync(ctx); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	if st, ok := bare.store.status("sc-ext7"); !ok || st != domain.BeadStateNeedsHuman {
		t.Errorf("bd status = %q (set: %t), want needs_human", st, ok)
	}
	changes := prvOfKind[events.BeadStateChanged](bare.sink)
	if len(changes) != 1 || changes[0].To != domain.BeadStateNeedsHuman {
		t.Fatalf("BeadStateChanged = %+v, want one move to needs_human", changes)
	}
}

// TestPRReviewStaleReviewBeadCanBeLeftAlone: the ignore policy writes
// nothing to bd but still stops the testers.
func TestPRReviewStaleReviewBeadCanBeLeftAlone(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	vanished := &gitx.PollResult{Stale: []beads.Issue{prvIssue(pr, domain.BeadStateOpen)}}
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), vanished},
		func(o *PRReviewOptions) { o.Stale = StaleReviewIgnore })
	ctx := context.Background()
	for range 2 {
		if err := rig.review.Sync(ctx); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	if _, closed := rig.store.closeReason("sc-ext7"); closed {
		t.Error("the ignore policy closed a bead")
	}
	if calls := rig.parker.calls(); len(calls) != 0 {
		t.Errorf("the ignore policy moved a bead: %+v", calls)
	}
	if ds := prvDemands(t, rig.review); len(ds) != 0 {
		t.Errorf("Demands = %+v, want none: the pull request is gone whatever the policy", ds)
	}
	var warned bool
	for _, w := range prvOfKind[events.Warning](rig.sink) {
		if w.Source == SourcePRReview && strings.Contains(w.Message, "no longer open") {
			warned = true
		}
	}
	if !warned {
		t.Error("nothing was published about the pull request that vanished")
	}
}

// TestPRReviewRunStopsOnAGitHubProblemAndOtherwiseKeepsPolling mirrors
// gitx.PRWatcher.Run: a problem polling again cannot fix ends the loop.
func TestPRReviewRunStopsOnAGitHubProblemAndOtherwiseKeepsPolling(t *testing.T) {
	t.Parallel()
	t.Run("fatal", func(t *testing.T) {
		t.Parallel()
		rig := prvNew(t, nil, nil)
		rig.watcher.errs = []error{fmt.Errorf("poll: %w", gitx.ErrGHUnauthenticated)}
		err := rig.review.Run(context.Background())
		if !errors.Is(err, gitx.ErrGHUnauthenticated) {
			t.Fatalf("Run = %v, want the gh authentication error", err)
		}
	})
	t.Run("transient", func(t *testing.T) {
		t.Parallel()
		rig := prvNew(t, nil, nil)
		rig.watcher.errs = []error{errors.New("gh: HTTP 502")}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- rig.review.Run(ctx) }()
		deadline := time.After(2 * time.Second)
		for rig.watcher.count() < 2 {
			select {
			case <-deadline:
				t.Fatal("the poll loop did not retry after a transient failure")
			default:
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run after cancellation = %v, want nil", err)
		}
		if st := rig.review.Stats(); st.Failures == 0 {
			t.Error("Stats.Failures = 0, want the transient failure counted")
		}
	})
}

// TestPRReviewIsVisibleInTheTUIPRsView is the last clause of P8-04: the
// events the reviewer publishes are exactly what the PRs view needs to
// show an external pull request, count the verdicts as they arrive and
// call it reviewed when the consolidated review has gone out.
func TestPRReviewIsVisibleInTheTUIPRsView(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	prvAnswer(t, rig, ds[0], domain.VerdictApprove)

	fold := func() []tui.PRRow {
		st := tui.NewState(domain.Run{ID: "run-prv", Config: domain.ConfigSnapshot{Safety: true, ReviewPRs: true}})
		for _, ev := range rig.sink.events() {
			st.Apply(ev)
		}
		return tui.PRRows(st)
	}

	rows := fold()
	if len(rows) != 1 {
		t.Fatalf("PRRows = %+v, want one row for the external pull request", rows)
	}
	switch {
	case !rows[0].External:
		t.Error("the row is not marked external")
	case rows[0].Status != tui.DeliveryReviewing:
		t.Errorf("status = %q, want %q while the round is still collecting verdicts", rows[0].Status, tui.DeliveryReviewing)
	case rows[0].Note() != fmt.Sprintf("1/%d", len(domain.Lenses())):
		t.Errorf("checks column = %q, want the verdict progress", rows[0].Note())
	case rows[0].PR() != "ext #7":
		t.Errorf("PR column = %q, want \"ext #7\"", rows[0].PR())
	}

	for _, d := range ds[1:] {
		prvAnswer(t, rig, d, domain.VerdictApprove)
	}
	rows = fold()
	if len(rows) != 1 || rows[0].Status != tui.DeliveryReviewed {
		t.Fatalf("PRRows after the review went out = %+v, want one reviewed row", rows)
	}
	if rows[0].Note() != string(domain.VerdictApprove) {
		t.Errorf("checks column = %q, want the posted review's outcome", rows[0].Note())
	}
}

// TestNewPRReviewNeedsAWatcher pins the one thing the reviewer refuses to
// be built without.
func TestNewPRReviewNeedsAWatcher(t *testing.T) {
	t.Parallel()
	if _, err := NewPRReview(PRReviewOptions{}); !errors.Is(err, ErrNoPRWatcher) {
		t.Fatalf("NewPRReview without a watcher: err = %v, want ErrNoPRWatcher", err)
	}
	r, err := NewPRReview(PRReviewOptions{Watcher: &prvWatcher{}})
	if err != nil {
		t.Fatalf("NewPRReview: %v", err)
	}
	if got := r.StalePolicy(); got != StaleReviewClose {
		t.Errorf("default stale policy = %q, want %q", got, StaleReviewClose)
	}
	if got, want := len(r.Lenses()), len(domain.Lenses()); got != want {
		t.Errorf("default lenses = %d, want %d", got, want)
	}
	if r.Interval() != (&prvWatcher{}).Interval() {
		t.Errorf("interval = %s, want the watcher's own", r.Interval())
	}
}

// TestPRReviewWithARealGHShim drives the production watcher and poster —
// gitx.PRWatcher over `gh pr list`, gitx.GH over `gh pr review` — so the
// wiring is proved against the command line the cannon really runs, not
// against a fake of it. It is the P8-04 "test with a gh shim".
func TestPRReviewWithARealGHShim(t *testing.T) {
	if testing.Short() {
		t.Skip("the gh shim spawns subprocesses")
	}
	ctx := context.Background()
	f := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
	repo := prvOpen(t, f)
	list := filepath.Join(t.TempDir(), "prs.json")
	if err := os.WriteFile(list, []byte(prvGHList), 0o600); err != nil {
		t.Fatalf("writing the pull request list: %v", err)
	}
	shim := prvGHShim(t, list)

	store := &prvBeadStore{}
	gh := gitx.NewGH(repo)
	watcher := gitx.NewPRWatcher(gh, store, gitx.WithRun("run-prv"), gitx.WithWatcherLogger(prvLogger()))
	sink := &prvSink{}
	clock := &prvClock{}
	checkout := &prvCheckout{ws: PRWorkspace{Worktree: t.TempDir(), Branch: "feature/retry", Base: f.DefaultBranch, Diff: prvDiff()}}
	store2 := prvNewStore()
	review, err := NewPRReview(PRReviewOptions{
		Watcher: watcher, Poster: gh, Beads: store2, Checkout: checkout,
		Sink: sink, Run: "run-prv", Clock: clock.now, Logger: prvLogger(),
	})
	if err != nil {
		t.Fatalf("NewPRReview: %v", err)
	}

	if err := review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d review beads, want 1 (the cannon's own pull request is skipped)", len(store.created))
	}
	if !strings.Contains(store.created[0].Title, "#7") {
		t.Errorf("review bead title = %q, want it to name pull request #7", store.created[0].Title)
	}

	ds := prvDemands(t, review)
	if len(ds) != len(domain.Lenses()) {
		t.Fatalf("Demands = %d, want one per lens", len(ds))
	}
	for _, d := range ds {
		af, err := review.PlanFiring(ctx, Firing{
			Agent:  domain.Agent{ID: domain.AgentID("tester-" + string(d.Lens)), Role: domain.RoleTester, Lens: d.Lens, Bead: d.Bead},
			Demand: d,
		})
		if err != nil {
			t.Fatalf("PlanFiring(%s): %v", d.Lens, err)
		}
		if err := af.Result(prvVerdict(d.Lens, domain.VerdictApprove), agents.Transcript{}); err != nil {
			t.Fatalf("Result(%s): %v", d.Lens, err)
		}
	}

	var reviews []testutil.Invocation
	for _, inv := range shim.Invocations(t) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "review" {
			reviews = append(reviews, inv)
		}
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "merge" {
			t.Fatal("the cannon ran `gh pr merge` on an external pull request (D-11 forbids it)")
		}
	}
	if len(reviews) != 1 {
		t.Fatalf("`gh pr review` ran %d times, want exactly 1", len(reviews))
	}
	if !prvHasArg(reviews[0].Args, "--approve") {
		t.Errorf("gh pr review args = %v, want --approve", reviews[0].Args)
	}
	if !strings.Contains(reviews[0].Stdin, "slop-cannon review of #7") {
		t.Errorf("review body = %q, want the consolidated review", reviews[0].Stdin)
	}
	if _, closed := store2.closeReason(store.created[0].ID); !closed {
		t.Error("the review bead was not closed after the review went out")
	}
}

// prvHasArg reports whether args contains want.
func prvHasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// prvGHList is what `gh pr list --json …` prints: one external pull
// request and one the cannon opened itself.
const prvGHList = `[
 {"number":7,"title":"add a retry to the uploader","url":"https://github.com/acme/widget/pull/7",
  "body":"retries the upload three times","author":{"login":"octocat"},
  "headRefName":"feature/retry","baseRefName":"main","headRefOid":"0123456789abcdef0123456789abcdef01234567",
  "labels":[],"isDraft":false},
 {"number":8,"title":"sc: fix the parser","url":"https://github.com/acme/widget/pull/8",
  "body":"","author":{"login":"slop-cannon"},
  "headRefName":"sc/sc-1a2b-fix-the-parser","baseRefName":"main","headRefOid":"89abcdef0123456789abcdef0123456789abcdef",
  "labels":[{"name":"slop-cannon"}],"isDraft":false}
]`

// prvGHShim installs a `gh` that lists the pull requests from a file,
// accepts a review on stdin, and fails anything else — a merge included.
func prvGHShim(t *testing.T, list string) *testutil.Shim {
	t.Helper()
	script := `case "$1 $2" in
  'pr list') cat ` + "'" + list + "'" + ` ;;
  'pr review') exit 0 ;;
  'auth status') exit 0 ;;
  *) echo "gh: unexpected $*" >&2; exit 1 ;;
esac`
	return testutil.FakeBin(t, "gh", script, testutil.WithStdinCapture())
}

// prvOpen opens the fixture repository with its isolated git identity.
func prvOpen(t *testing.T, f *testutil.FixtureRepo) *gitx.Repo {
	t.Helper()
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	repo, err := gitx.Open(context.Background(), f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("gitx.Open: %v", err)
	}
	return repo
}

// prvBeadStore is the in-memory bd the production watcher records review
// beads in.
type prvBeadStore struct {
	mu      sync.Mutex
	created []beads.Issue
	next    int
}

func (s *prvBeadStore) Create(_ context.Context, in beads.BeadInput) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	is := beads.Issue{
		ID:          domain.BeadID(fmt.Sprintf("sc-ext%d", s.next)),
		Title:       in.Title,
		Description: in.Description,
		Acceptance:  in.Acceptance,
		Type:        string(in.Type),
		Status:      string(domain.BeadStateOpen),
		Labels:      append([]string(nil), in.Labels...),
		Metadata:    beads.Metadata{},
	}
	for k, v := range in.Metadata {
		is.Metadata[k] = v
	}
	s.created = append(s.created, is)
	return is, nil
}

func (s *prvBeadStore) List(context.Context, beads.ListFilter) ([]beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]beads.Issue(nil), s.created...), nil
}

// TestPRWorktreeCheckoutFetchesThePullRequest covers the production
// checkout against a real repository: the pull request's head lands in the
// review bead's own worktree (D-23) and the diff is the pull request's,
// measured against its base — never the repository's own working tree,
// which git would not let us touch from here anyway.
func TestPRWorktreeCheckoutFetchesThePullRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("the checkout runs git")
	}
	t.Parallel()
	ctx := context.Background()
	f := testutil.NewFixtureRepo(t, testutil.WithBareRemote())

	// Somebody else's branch, pushed and then published as a pull request
	// reference, exactly as GitHub does.
	f.Git(t, "checkout", "-b", "feature/retry")
	f.WriteFile(t, "uploader.go", "package main\n\nfunc upload() error { return nil }\n")
	head := f.Commit(t, "add the uploader")
	f.Git(t, "push", "--quiet", "origin", "feature/retry")
	prvBareGit(t, f, "update-ref", "refs/pull/7/head", head)
	f.Git(t, "checkout", "--quiet", f.DefaultBranch)
	f.Git(t, "branch", "-D", "feature/retry")
	prvBareGit(t, f, "update-ref", "-d", "refs/heads/feature/retry")

	repo := prvOpen(t, f)
	m, err := gitx.NewManager(repo, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	c, err := NewPRWorktreeCheckout(m, WithPRCheckoutLogger(prvLogger()))
	if err != nil {
		t.Fatalf("NewPRWorktreeCheckout: %v", err)
	}
	bead := domain.Bead{ID: "sc-ext7", Title: "Review PR #7", Type: domain.BeadTypeTask, State: domain.BeadStateOpen}
	pr := gitx.PullRequest{Number: 7, Title: "add the uploader", HeadRef: "feature/retry", BaseRef: f.DefaultBranch}

	ws, err := c.Checkout(ctx, bead, pr)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if ws.HeadSHA != head {
		t.Errorf("checkout head = %q, want the pull request head %q", ws.HeadSHA, head)
	}
	if _, err := os.Stat(filepath.Join(ws.Worktree, "uploader.go")); err != nil {
		t.Errorf("the pull request's file is not in the checkout: %v", err)
	}
	if ws.Diff == nil || len(ws.Diff.Files) != 1 || ws.Diff.Files[0].Path != "uploader.go" {
		t.Fatalf("diff = %+v, want the one file the pull request adds", ws.Diff)
	}
	if !strings.Contains(ws.Diff.Patch, "func upload()") {
		t.Errorf("the patch does not carry the pull request's change: %q", ws.Diff.Patch)
	}
	// The repository's own working tree is untouched: this is somebody
	// else's code, and it never leaves the throwaway worktree.
	if got := f.Head(t); got == head {
		t.Error("the checkout moved the repository's own HEAD onto the pull request")
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "uploader.go")); err == nil {
		t.Error("the pull request's file landed in the target repository")
	}

	// D-23: nothing of ours is left in the user's repository. The worktree
	// manager mints an sc/<bead> branch to create the worktree with; the
	// checkout takes the worktree off it and deletes it, so reviewing
	// somebody else's pull request adds no branch to their `git branch`.
	if got := strings.TrimSpace(f.Git(t, "branch", "--list", gitx.BranchName(bead))); got != "" {
		t.Errorf("the review left branch %q behind in the repository (D-23)", got)
	}
	wt, err := m.Open(ctx, bead.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := wt.Git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || strings.TrimSpace(got) != "HEAD" {
		t.Errorf("the review worktree is on branch %q (%v), want a detached HEAD", strings.TrimSpace(got), err)
	}
	if got, err := wt.Git(ctx, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(got) != head {
		t.Errorf("the detached worktree is at %q (%v), want the pull request head %q", strings.TrimSpace(got), err, head)
	}

	// A second checkout of the same pull request reuses the worktree.
	ws2, err := c.Checkout(ctx, bead, pr)
	if err != nil {
		t.Fatalf("second Checkout: %v", err)
	}
	if ws2.HeadSHA != head {
		t.Errorf("second checkout head = %q, want %q", ws2.HeadSHA, head)
	}
	if got := strings.TrimSpace(f.Git(t, "branch", "--list", gitx.BranchName(bead))); got != "" {
		t.Errorf("the second checkout left branch %q behind", got)
	}
}

// prvBareGit runs git inside the fixture's bare remote.
func prvBareGit(t *testing.T, f *testutil.FixtureRepo, args ...string) {
	t.Helper()
	if f.RemoteDir == "" {
		t.Fatal("the fixture has no bare remote")
	}
	f.Git(t, append([]string{"-C", f.RemoteDir}, args...)...)
}

// ---------------------------------------------------------------------------
// D-11's "exactly one consolidated review", under a slow gh
// ---------------------------------------------------------------------------

// prvGate is a poster whose first post blocks until the test lets it
// finish, and which records how many posts were ever in flight at once.
// Every later post returns at once, so a reviewer that starts a second `gh
// pr review` fails the test rather than deadlocking it.
type prvGate struct {
	entered chan struct{}
	release chan struct{}

	mu     sync.Mutex
	calls  int
	live   int
	peak   int
	posted []gitx.Review
}

func prvNewGate() *prvGate {
	return &prvGate{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (g *prvGate) PostReview(_ context.Context, pr gitx.PullRequest, verdicts []domain.Verdict) (gitx.Review, error) {
	g.mu.Lock()
	g.calls++
	first := g.calls == 1
	g.live++
	if g.live > g.peak {
		g.peak = g.live
	}
	g.mu.Unlock()

	g.entered <- struct{}{}
	if first {
		<-g.release
	}
	review, err := gitx.BuildReview(pr, verdicts)
	g.mu.Lock()
	g.live--
	if err == nil {
		g.posted = append(g.posted, review)
	}
	g.mu.Unlock()
	return review, err
}

func (g *prvGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *prvGate) highWater() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func (g *prvGate) reviews() []gitx.Review {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gitx.Review(nil), g.posted...)
}

// TestPRReviewNeverPostsTwoReviewsOfOnePullRequest is D-11's headline
// promise where it can actually break: posting is a network call, and it
// can easily outlive the poll interval. The polls that land while the first
// `gh pr review` is still talking to GitHub see a bead with every verdict
// in and no review posted; without the in-flight flag each of them starts
// another review of somebody else's pull request.
func TestPRReviewNeverPostsTwoReviewsOfOnePullRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pr := prvPR()
	gate := prvNewGate()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), {}, {}, {}}, func(o *PRReviewOptions) { o.Poster = gate })
	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	if len(ds) < 2 {
		t.Fatalf("demands = %d, want one per lens", len(ds))
	}
	for _, d := range ds[:len(ds)-1] {
		prvAnswer(t, rig, d, domain.VerdictApprove)
	}
	if got := gate.count(); got != 0 {
		t.Fatalf("gh pr review called %d times before the round was complete, want 0", got)
	}

	// The last verdict starts the one post, on the firing's goroutine.
	last := ds[len(ds)-1]
	af, err := rig.review.PlanFiring(ctx, Firing{
		Agent: domain.Agent{ID: domain.AgentID("tester-" + string(last.Lens) + "-sc-ext7-r1"),
			Role: domain.RoleTester, Lens: last.Lens, Bead: last.Bead},
		Demand: last,
	})
	if err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- af.Result(prvVerdict(last.Lens, domain.VerdictApprove), agents.Transcript{}) }()
	<-gate.entered

	// Three polls land while gh is still being talked to.
	for i := 0; i < 3; i++ {
		if err := rig.review.Sync(ctx); err != nil {
			t.Fatalf("Sync while a review was in flight: %v", err)
		}
	}
	if got := gate.count(); got != 1 {
		t.Fatalf("gh pr review called %d times while one call was in flight, want exactly 1 (D-11)", got)
	}

	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("the last verdict: %v", err)
	}
	if got, peak := gate.count(), gate.highWater(); got != 1 || peak != 1 {
		t.Fatalf("gh pr review called %d times (%d at once), want exactly one review of pull request #%d", got, peak, pr.Number)
	}
	if posted := gate.reviews(); len(posted) != 1 || posted[0].Event != gitx.ReviewApprove {
		t.Fatalf("posted = %+v, want one approving review", posted)
	}
	if _, closed := rig.store.closeReason("sc-ext7"); !closed {
		t.Error("the review bead was not closed after the review went out")
	}
	if st := rig.review.Stats(); st.Posted != 1 || st.Closed != 1 {
		t.Errorf("Stats = %+v, want one posted and one closed", st)
	}

	// And a poll after the review has gone out still posts nothing.
	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync after the review: %v", err)
	}
	if got := gate.count(); got != 1 {
		t.Errorf("gh pr review called %d times in all, want 1", got)
	}
}

// TestPRReviewPostIsBoundedEvenWhenTheFiringIsCancelled: the last verdict
// posts with the firing's cancellation removed, so a stuck `gh pr review`
// would otherwise hold a stopping run open for the whole subprocess
// timeout. The deadline is the reviewer's, not the firing's.
func TestPRReviewPostIsBoundedEvenWhenTheFiringIsCancelled(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	var deadline time.Time
	var had bool
	poster := prvPosterFunc(func(ctx context.Context, _ gitx.PullRequest, _ []domain.Verdict) (gitx.Review, error) {
		deadline, had = ctx.Deadline()
		if err := ctx.Err(); err != nil {
			return gitx.Review{}, err
		}
		return gitx.Review{Event: gitx.ReviewApprove, Body: "ok"}, nil
	})
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, func(o *PRReviewOptions) {
		o.Poster = poster
		o.PostTimeout = 30 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	for i, d := range ds {
		if i == len(ds)-1 {
			// The firing's context is done by the time the verdict comes
			// back, exactly as it is when the run is stopping.
			cancel()
		}
		prvAnswer(t, rig, d, domain.VerdictApprove)
	}
	if !had {
		t.Fatal("the post ran with no deadline: a stuck gh holds the run open")
	}
	if got := time.Until(deadline); got <= 0 || got > 30*time.Second {
		t.Errorf("post deadline in %s, want it inside the 30s the reviewer was given", got)
	}
	if st := rig.review.Stats(); st.Posted != 1 {
		t.Errorf("Stats = %+v, want the review posted although the firing was cancelled", st)
	}
}

// prvPosterFunc is a poster written inline.
type prvPosterFunc func(context.Context, gitx.PullRequest, []domain.Verdict) (gitx.Review, error)

func (f prvPosterFunc) PostReview(ctx context.Context, pr gitx.PullRequest, vs []domain.Verdict) (gitx.Review, error) {
	return f(ctx, pr, vs)
}

// ---------------------------------------------------------------------------
// Settling a review the run cannot carry out (P6-31 + brief mode)
// ---------------------------------------------------------------------------

// TestPRReviewDemandsSayWhereTheReviewBeadIs is the half of the give-up
// path that lives here. A review demand claims nothing, so the scheduler
// has nothing else to go on: without Demand.From it cannot take the bead
// down the D-07 failed edge when it gives up (Scheduler.parkFrom), the
// review bead stays open for ever, and a brief run — which stops when every
// bead of the run has settled — never finishes.
func TestPRReviewDemandsSayWhereTheReviewBeadIs(t *testing.T) {
	t.Parallel()
	rig := prvNew(t, []*gitx.PollResult{prvResult(prvPR())}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ds := prvDemands(t, rig.review)
	if len(ds) == 0 {
		t.Fatal("no demands")
	}
	for _, d := range ds {
		if d.From != domain.BeadStateOpen {
			t.Errorf("demand(%s).From = %q, want the review bead's state %q", d.Lens, d.From, domain.BeadStateOpen)
		}
		from := parkFrom(d)
		if from == "" {
			t.Fatalf("parkFrom(%s) = \"\": the scheduler cannot park a review bead it gave up on", d.Lens)
		}
		to, err := Transition(from, BeadEventFailed)
		if err != nil || to != domain.BeadStateFailed {
			t.Fatalf("Transition(%q, failed) = %q, %v; want the bead to settle", from, to, err)
		}
	}
}

// TestPRReviewLetsGoOfAReviewBeadSomethingElseSettled is the other half:
// once the bead has been settled by somebody else — the scheduler giving up
// on its firings, or a human closing it — the reviewer stops offering work
// on it instead of going on asking for testers no scheduler will fire.
func TestPRReviewLetsGoOfAReviewBeadSomethingElseSettled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pr := prvPR()
	// The second poll reads the bead back as failed, which is where the
	// scheduler's give-up path puts it (P6-31).
	settled := &gitx.PollResult{
		External: []gitx.PullRequest{pr},
		Existing: []gitx.ReviewBead{{Issue: prvIssue(pr, domain.BeadStateFailed), PR: pr}},
	}
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr), settled, {}}, nil)
	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(prvDemands(t, rig.review)) == 0 {
		t.Fatal("the reviewer offered no work for a pull request it had just adopted")
	}

	if err := rig.review.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if ds := prvDemands(t, rig.review); len(ds) != 0 {
		t.Errorf("demands after the bead was settled elsewhere = %+v, want none", ds)
	}
	if rig.review.Handles(Demand{Kind: DemandReview, Bead: "sc-ext7",
		Payload: PRReviewWork{Bead: "sc-ext7", PR: pr, Lens: domain.Lenses()[0], Round: PRReviewRound}}) {
		t.Error("the reviewer still claims a demand for a bead it can no longer review")
	}
	if st := rig.review.Stats(); st.Tracked != 0 {
		t.Errorf("Stats.Tracked = %d, want 0: nothing is being reviewed any more", st.Tracked)
	}
	if rs := rig.review.Reviews(); len(rs) != 1 || !rs[0].Settled || rs[0].Posted {
		t.Errorf("Reviews = %+v, want the one review settled and unposted", rs)
	}
	said := false
	for _, w := range prvOfKind[events.Warning](rig.sink) {
		if strings.Contains(w.Message, "#7") && strings.Contains(w.Message, "failed") {
			said = true
		}
	}
	if !said {
		t.Error("nothing said why the pull request is no longer being reviewed")
	}
	// Nothing was posted and the reviewer wrote nothing to the settled bead.
	if got := len(rig.poster.reviews()); got != 0 {
		t.Errorf("posted %d reviews on a pull request the run gave up on, want 0", got)
	}
	if _, closed := rig.store.closeReason("sc-ext7"); closed {
		t.Error("the reviewer closed a bead somebody else had already settled")
	}
}

// TestReviewedPullRequestsAreNotCountedAsOpened: the run summary and `sc
// status` fold PROpened, and the reviewer publishes one per external pull
// request so the run's readers can see it at all. Counting those as pull
// requests the cannon opened tells a user it opened work it only reviewed —
// and never merges (D-11).
func TestReviewedPullRequestsAreNotCountedAsOpened(t *testing.T) {
	t.Parallel()
	pr := prvPR()
	rig := prvNew(t, []*gitx.PollResult{prvResult(pr)}, nil)
	if err := rig.review.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	evs := rig.sink.events()
	// One pull request the run opened itself, for the contrast.
	evs = append(evs, events.PROpened{
		Meta: events.MetaAt("run-prv", prvEpoch), Bead: "sc-1", Number: 12,
		URL: "https://github.com/acme/widget/pull/12", Branch: "sc/sc-1", Base: "main",
	})
	s := Summarise(evs, 0)
	if s.PRs != 1 {
		t.Errorf("summary.PRs = %d, want 1: only the pull request the run opened", s.PRs)
	}
	if s.Reviewing != 1 {
		t.Errorf("summary.Reviewing = %d, want the one external pull request being reviewed", s.Reviewing)
	}
	if s.Merges != 0 {
		t.Errorf("summary.Merges = %d, want 0", s.Merges)
	}
}
