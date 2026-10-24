package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// The D-31 auto-merge, driven through the gate poller against the same
// fake bd as gates_test.go and in-memory GitHub, git, ledger and feedback
// stand-ins, so every decision can be asserted call by call.

const (
	amBead   = domain.BeadID("fx-byi")
	amGate   = domain.BeadID("fx-qba")
	amPR     = 7
	amBranch = "sc/fx-byi-fix-max"
	amLogin  = "sc-bot"
	amHead   = "1111111111111111111111111111111111111111"
)

// amStarted is when the run first started; amOwnedView's pull request was
// created after it.
var amStarted = gateEpoch.Add(-time.Hour)

// amFakeGH is GitHub: one view per pull request number, and a record of
// every merge and branch update asked for.
type amFakeGH struct {
	mu       sync.Mutex
	views    map[int]*gitx.PRView
	login    string
	viewErr  error
	loginErr error
	mergeErr error
	updErr   error
	nReads   int
	Merges   []string // "<n>@<head>"
	Updates  []int
}

func (g *amFakeGH) set(v *gitx.PRView) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := *v
	cp.Checks = append([]gitx.PRCheck(nil), v.Checks...)
	g.views[v.Number] = &cp
}

func (g *amFakeGH) ViewPR(_ context.Context, n int) (*gitx.PRView, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nReads++
	if g.viewErr != nil {
		return nil, g.viewErr
	}
	v, ok := g.views[n]
	if !ok {
		return nil, fmt.Errorf("no pull request #%d", n)
	}
	cp := *v
	return &cp, nil
}

func (g *amFakeGH) Login(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.login, g.loginErr
}

func (g *amFakeGH) MergePR(_ context.Context, n int, head string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mergeErr != nil {
		return g.mergeErr
	}
	g.Merges = append(g.Merges, fmt.Sprintf("%d@%s", n, head))
	if v, ok := g.views[n]; ok {
		v.State = "MERGED" // as GitHub will answer from now on
	}
	return nil
}

func (g *amFakeGH) UpdateBranch(_ context.Context, n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Updates = append(g.Updates, n)
	return g.updErr
}

func (g *amFakeGH) reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nReads
}

// amFakeBranches is git: the bead's branch, and a record of every sync.
type amFakeBranches struct {
	mu      sync.Mutex
	branch  string
	others  map[domain.BeadID]string
	syncErr error
	Syncs   []string // "<bead>@<head>"

	// merges maps a merge commit to the head GitHub merged the base into
	// to make it: what `gh pr update-branch` makes, whoever asked for it.
	merges    map[string]string
	reviewErr error
	Reviews   []string // "<reviewed>..<head> on <base>"
	// tip is the bead's local branch tip; "" is a commit nobody pushed.
	tip string
}

func (b *amFakeBranches) BranchFor(_ context.Context, bead domain.Bead) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if br, ok := b.others[bead.ID]; ok {
		return br, true, nil
	}
	return b.branch, b.branch != "", nil
}

func (b *amFakeBranches) SyncBranch(_ context.Context, bead domain.Bead, head string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.syncErr != nil {
		return b.syncErr
	}
	b.Syncs = append(b.Syncs, fmt.Sprintf("%s@%s", bead.ID, head))
	return nil
}

func (b *amFakeBranches) TrackingRef(_ context.Context, branch string) (string, error) {
	return "origin/" + branch, nil
}

// ReviewedHead applies the gitx rule to the fake history: walking back from
// head, every commit before reviewed must be an update merge (markUpdate)
// of a head the run asked GitHub to update (updatedFrom).
func (b *amFakeBranches) ReviewedHead(_ context.Context, reviewed, head, base string, updatedFrom []string) (bool, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Reviews = append(b.Reviews, fmt.Sprintf("%s..%s on %s", reviewed, head, base))
	if b.reviewErr != nil {
		return false, "", b.reviewErr
	}
	asked := map[string]bool{}
	for _, h := range updatedFrom {
		asked[h] = true
	}
	for cur := head; cur != reviewed; {
		from, ok := b.merges[cur]
		switch {
		case !ok:
			return false, "commit " + cur[:7] + " is neither the approved commit nor a merge GitHub made when slop-cannon asked it to update the branch", nil
		case !asked[from]:
			return false, "merge commit " + cur[:7] + " is not an update slop-cannon asked for", nil
		}
		cur = from
	}
	return true, "", nil
}

func (b *amFakeBranches) BranchTip(context.Context, domain.Bead) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tip == "" {
		return amHead, nil
	}
	return b.tip, nil
}

// markUpdate makes head GitHub's merge of the base into from.
func (b *amFakeBranches) markUpdate(head, from string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.merges == nil {
		b.merges = map[string]string{}
	}
	b.merges[head] = from
}

// amFakeLedger is the run's state.json half.
type amFakeLedger struct {
	mu       sync.Mutex
	last     time.Time
	err      error
	Records  []string
	sentBack []string
	updated  []string
}

func (l *amFakeLedger) AutoMergeUpdates() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.updated...)
}

func (l *amFakeLedger) RecordAutoMergeUpdate(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.updated = append(l.updated, key)
	return nil
}

func (l *amFakeLedger) LastAutoMerge() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *amFakeLedger) AutoMergeSentBack() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sentBack...)
}

func (l *amFakeLedger) RecordAutoMergeSentBack(keys []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.sentBack = append(l.sentBack, keys...)
	return nil
}

func (l *amFakeLedger) RecordAutoMerge(at time.Time, bead domain.BeadID, pr int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.last = at
	l.Records = append(l.Records, fmt.Sprintf("%s#%d@%s", bead, pr, at.UTC().Format(time.RFC3339)))
	return nil
}

// amFakeOutcomes is the conductor.
type amFakeOutcomes struct {
	mu   sync.Mutex
	Outs []*DeliveryOutcome
}

func (o *amFakeOutcomes) ApplyOutcome(_ context.Context, _ domain.BeadID, out *DeliveryOutcome) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Outs = append(o.Outs, out)
	return nil
}

func (o *amFakeOutcomes) all() []*DeliveryOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*DeliveryOutcome(nil), o.Outs...)
}

// amHarness is a gate harness with auto-merge on.
type amHarness struct {
	*gateHarness
	GH       *amFakeGH
	Branches *amFakeBranches
	Ledger   *amFakeLedger
	Outcomes *amFakeOutcomes
}

func newAMHarness(t *testing.T, tweak func(*AutoMergeOptions)) *amHarness {
	t.Helper()
	gh := &amFakeGH{views: map[int]*gitx.PRView{}, login: amLogin}
	br := &amFakeBranches{branch: amBranch}
	led := &amFakeLedger{}
	outs := &amFakeOutcomes{}
	h := newGateHarness(t, func(o *GatePollerOptions) {
		o.AutoMerge = &AutoMergeOptions{
			GH: gh, Branches: br, Ledger: led, Outcomes: outs,
			Spacing: 10 * time.Minute, RunStarted: amStarted,
		}
		if tweak != nil {
			tweak(o.AutoMerge)
		}
	})
	h.BD.AddGate(amGate, amBead, amPR)
	h.BD.AddBead(amBead, domain.BeadStateDelivering, amPR)
	h.BD.SetMeta(amBead, beads.MetaPRHead, amHead)
	return &amHarness{gateHarness: h, GH: gh, Branches: br, Ledger: led, Outcomes: outs}
}

// redeliver puts the bead back in delivering, as the next delivery of it
// does.
func (h *amHarness) redeliver() {
	h.t.Helper()
	_, err := h.BD.SetStatus(context.Background(), amBead, domain.BeadStateDelivering)
	require.NoError(h.t, err)
}

// pending queues `bd gate check` answers that keep the gate pending.
func (h *amHarness) pending() {
	h.BD.Answer(gateCheck(h.t, gateOutPending, ""))
}

// amOwnedView is the run's own pull request for amBead at head, green and
// CLEAN unless the test changes it.
func amOwnedView(head string, checks ...gitx.PRCheck) *gitx.PRView {
	if len(checks) == 0 {
		checks = []gitx.PRCheck{amCheck("test", "SUCCESS")}
	}
	return &gitx.PRView{
		PullRequest: gitx.PullRequest{
			Number: amPR, HeadRef: amBranch, BaseRef: gateBranch, HeadSHA: head,
			Labels:    []string{gitx.CannonPRLabel},
			Body:      fmt.Sprintf("<!-- slop-cannon bead=%s run=%s branch=%s -->\n## Bead\n", amBead, gateRun, amBranch),
			Author:    amLogin,
			CreatedAt: amStarted.Add(30 * time.Minute),
		},
		State: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
		Checks: checks,
	}
}

func amCheck(name, conclusion string) gitx.PRCheck {
	status := "COMPLETED"
	if conclusion == "" {
		status = "IN_PROGRESS"
	}
	return gitx.PRCheck{Kind: gitx.CheckKindRun, Name: name, Status: status, Conclusion: conclusion, URL: "https://ci.example/" + name}
}

// status is auto-merge's current decision about amBead.
func (h *amHarness) status() AutoMergeStatus {
	h.t.Helper()
	for _, st := range h.P.Stats().AutoMerge {
		if st.Bead == amBead {
			return st
		}
	}
	return AutoMergeStatus{}
}

func TestAutoMergeMergesTheRunsOwnGreenPullRequest(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.GH.set(amOwnedView(amHead))
	h.pending()

	h.poll()

	assert.Equal(t, []string{"7@" + amHead}, h.GH.Merges, "exactly one squash merge, at the head this poll read")
	assert.Equal(t, []string{"fx-byi#7@" + gateEpoch.UTC().Format(time.RFC3339)}, h.Ledger.Records, "recorded before the merge")
	assert.Empty(t, h.GH.Updates)
	assert.Empty(t, h.Branches.Syncs)
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead), "the bead closes when bd sees the merge, not before")
	st := h.status()
	assert.Equal(t, AutoMergeMerging, st.State)
	assert.Contains(t, st.Reason, "squash-merged pull request #7")
	stats := h.P.Stats()
	assert.True(t, stats.AutoMergeOn)
	assert.Equal(t, 1, stats.AutoMerged)
	assert.Equal(t, gateEpoch, stats.LastAutoMergeAt)

	// The merge lands; bd resolves the gate and the poller closes the bead
	// exactly as it does for a human merge.
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	res := h.poll()
	assert.Equal(t, []domain.BeadID{amBead}, res.Closed)
	assert.Len(t, h.GH.Merges, 1)
	assert.Empty(t, h.P.Stats().AutoMerge, "a closed bead has no auto-merge status")
}

func TestAutoMergeWaitsWhileChecksAreUnfinishedOrIncomplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		checks []gitx.PRCheck
		named  []string
		edit   func(*gitx.PRView)
		want   string
		state  AutoMergeDecision
	}{
		{name: "a check still running", checks: []gitx.PRCheck{amCheck("test", ""), amCheck("build", "SUCCESS")}, want: `"test" still running`},
		{name: "no checks at all", edit: func(v *gitx.PRView) { v.Checks = nil }, want: "no checks yet"},
		{name: "a named check not reported", named: []string{"test", "build"}, want: "required build not reported"},
		{name: "a named check only skipped", checks: []gitx.PRCheck{amCheck("test", "SKIPPED"), amCheck("x", "SUCCESS")}, named: []string{"test"}, want: "required test"},
		{name: "a draft", edit: func(v *gitx.PRView) { v.IsDraft = true }, want: "is a draft"},
		{name: "blocked by a review", edit: func(v *gitx.PRView) { v.MergeStateStatus = "BLOCKED" }, want: "BLOCKED", state: AutoMergeBlocked},
		{name: "unstable", edit: func(v *gitx.PRView) { v.MergeStateStatus = "UNSTABLE" }, want: "UNSTABLE", state: AutoMergeBlocked},
		{name: "unknown", edit: func(v *gitx.PRView) { v.MergeStateStatus = "UNKNOWN" }, want: "UNKNOWN", state: AutoMergeBlocked},
		{name: "closed already", edit: func(v *gitx.PRView) { v.State = "CLOSED" }, want: "is closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newAMHarness(t, func(o *AutoMergeOptions) { o.Checks = tc.named })
			v := amOwnedView(amHead, tc.checks...)
			if tc.edit != nil {
				tc.edit(v)
			}
			h.GH.set(v)
			h.pending()
			h.poll()
			h.poll()

			assert.Empty(t, h.GH.Merges, "never merged")
			assert.Empty(t, h.GH.Updates)
			assert.Empty(t, h.Outcomes.all(), "nothing sent back")
			assert.Empty(t, h.Ledger.Records)
			assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead))
			st := h.status()
			want := tc.state
			if want == "" {
				want = AutoMergeWaiting
			}
			assert.Equal(t, want, st.State)
			assert.Contains(t, st.Reason, tc.want)
		})
	}
}

// A required review is surfaced once, as a Warning a person can act on,
// and never bypassed.
func TestAutoMergeSurfacesABlockedPullRequestOnce(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	v := amOwnedView(amHead)
	v.MergeStateStatus = "BLOCKED"
	h.GH.set(v)
	h.pending()
	for range 3 {
		h.poll()
	}
	var blocked []string
	for _, w := range h.Rec.warnings() {
		if strings.Contains(w.Message, "BLOCKED") {
			blocked = append(blocked, w.Message)
		}
	}
	assert.Len(t, blocked, 1)
	assert.Empty(t, h.GH.Merges)
	lines := strings.Join(Vitals(gateEpoch, nil, h.P, nil).Lines(), "\n")
	assert.Contains(t, lines, "auto-merge: on · fx-byi blocked: GitHub reports pull request #7 BLOCKED")
	assert.Contains(t, Vitals(gateEpoch, nil, h.P, nil).Header(), "auto-merge 1 blocked")
}

func TestAutoMergeSendsAFailedCheckBackToTheEngineerOnce(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.GH.set(amOwnedView(amHead, amCheck("test", "FAILURE"), amCheck("build", "SUCCESS")))
	h.pending()

	h.poll()

	assert.Empty(t, h.GH.Merges)
	assert.Equal(t, []string{"fx-byi@" + amHead}, h.Branches.Syncs, "the branch is brought up to the pull request's head first")
	assert.Equal(t, string(domain.BeadStateChangesRequested), h.BD.State(amBead), "the BeadEventCheckFailed edge")
	outs := h.Outcomes.all()
	require.Len(t, outs, 1)
	out := outs[0]
	assert.Equal(t, DeliveryCheckFailed, out.Kind)
	assert.Equal(t, domain.BeadStateChangesRequested, out.State)
	assert.Equal(t, 2, out.Round)
	require.Len(t, out.Items, 1)
	item := out.Items[0]
	assert.Equal(t, domain.SeverityBlocker, item.Severity)
	assert.Equal(t, DefaultCheckItemCategory, item.Category)
	assert.Contains(t, item.Title, `check "test" failed`)
	assert.Contains(t, item.Detail, "https://ci.example/test")
	assert.Contains(t, item.Detail, "failure")
	assert.Contains(t, h.Rec.kinds(), events.KindRoundStarted)
	assert.Equal(t, AutoMergeSentBack, h.status().State)
	assert.Equal(t, 1, h.P.Stats().AutoMergeSentBack)

	// While the engineer has it, auto-merge leaves it alone.
	h.poll()
	assert.Len(t, h.Outcomes.all(), 1)

	// Re-delivered with the same head and the same failure: it is not sent
	// round again but handed to a person.
	h.redeliver()
	h.poll()
	assert.Len(t, h.Outcomes.all(), 1, "at most once per head and check")
	assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
	assert.Equal(t, AutoMergeHandedOff, h.status().State)

	// A new head with the same check failing is a new round.
	h.redeliver()
	h.BD.SetMeta(amBead, beads.MetaPRHead, "2222222222222222222222222222222222222222")
	h.GH.set(amOwnedView("2222222222222222222222222222222222222222", amCheck("test", "FAILURE")))
	h.poll()
	assert.Len(t, h.Outcomes.all(), 2)
	assert.Empty(t, h.GH.Merges)
}

func TestAutoMergeUpdatesABranchThatIsBehindOncePerHead(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	v := amOwnedView(amHead)
	v.MergeStateStatus = "BEHIND"
	h.GH.set(v)
	h.pending()

	h.poll()
	h.poll()
	assert.Equal(t, []int{amPR}, h.GH.Updates, "gh pr update-branch, once for this head")
	assert.Empty(t, h.GH.Merges)
	assert.Equal(t, AutoMergeUpdating, h.status().State)

	// GitHub merged the base in: a new head, checks running again.
	assert.Equal(t, []string{AutoMergeUpdateKey(amBead, amHead)}, h.Ledger.AutoMergeUpdates(), "recorded before it was asked for")
	h.Branches.markUpdate("3333333333333333333333333333333333333333", amHead)
	next := amOwnedView("3333333333333333333333333333333333333333", amCheck("test", ""))
	h.GH.set(next)
	h.poll()
	assert.Empty(t, h.GH.Merges, "waits for the checks on the new head")
	next.Checks = []gitx.PRCheck{amCheck("test", "SUCCESS")}
	h.GH.set(next)
	h.poll()
	assert.Equal(t, []string{"7@3333333333333333333333333333333333333333"}, h.GH.Merges)
	assert.Len(t, h.GH.Updates, 1)
}

// A failed `gh pr update-branch` does not count as the one update for the
// head: the next poll asks again.
func TestAutoMergeRetriesAnUpdateGitHubRefused(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	v := amOwnedView(amHead)
	v.MergeStateStatus = "BEHIND"
	h.GH.set(v)
	h.GH.updErr = errors.New("HTTP 502: Bad Gateway")
	h.pending()

	h.poll()
	assert.Equal(t, []int{amPR}, h.GH.Updates)
	assert.Contains(t, h.status().Reason, "gh could not update pull request #7")

	h.GH.mu.Lock()
	h.GH.updErr = nil
	h.GH.mu.Unlock()
	h.poll()
	h.poll()
	assert.Equal(t, []int{amPR, amPR}, h.GH.Updates, "asked again after the failure, then once only")
	assert.Equal(t, AutoMergeUpdating, h.status().State)
	assert.Equal(t, 1, h.P.Stats().AutoMergeUpdates)
}

func TestAutoMergeSendsAConflictBackToTheEngineer(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	v := amOwnedView(amHead)
	v.MergeStateStatus, v.Mergeable, v.Checks = "DIRTY", "CONFLICTING", nil
	h.GH.set(v)
	h.pending()

	h.poll()

	assert.Empty(t, h.GH.Merges)
	assert.Empty(t, h.GH.Updates)
	assert.Equal(t, []string{"fx-byi@" + amHead}, h.Branches.Syncs)
	assert.Equal(t, string(domain.BeadStateChangesRequested), h.BD.State(amBead))
	outs := h.Outcomes.all()
	require.Len(t, outs, 1)
	require.Len(t, outs[0].Items, 1)
	item := outs[0].Items[0]
	assert.Contains(t, item.Title, "merge origin/main into "+amBranch)
	assert.Contains(t, item.Detail, "`git merge origin/main`")
	assert.Contains(t, item.Detail, "Do not rebase, reset or amend")

	// Still conflicting on the same head after its round: a person.
	h.redeliver()
	h.poll()
	assert.Len(t, h.Outcomes.all(), 1)
	assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
}

// A bead whose branch and pull request have both moved is not sent back:
// the engineer's push could not land, so a person brings them together.
func TestAutoMergeHandsADivergedBranchToAPerson(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.Branches.syncErr = fmt.Errorf("%w: test", gitx.ErrBranchDiverged)
	h.GH.set(amOwnedView(amHead, amCheck("test", "FAILURE")))
	h.pending()

	h.poll()

	assert.Empty(t, h.Outcomes.all())
	assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
	assert.Contains(t, h.status().Reason, "have both moved")
}

// TestAutoMergeNeverTouchesAPullRequestThatIsNotTheRunsOwn breaks each
// ownership clause in turn: the pull request is never merged, updated or
// sent back — even when it is green, behind, conflicting or failing — and
// the bead stays where it is for a human.
func TestAutoMergeNeverTouchesAPullRequestThatIsNotTheRunsOwn(t *testing.T) {
	t.Parallel()
	clauses := []struct {
		name string
		edit func(*gitx.PRView)
		want string
	}{
		{"another head branch", func(v *gitx.PRView) { v.HeadRef = "fix-max" }, "head branch"},
		{"another base", func(v *gitx.PRView) { v.BaseRef = "release" }, "base branch"},
		{"no label", func(v *gitx.PRView) { v.Labels = nil }, "label"},
		{"no marker", func(v *gitx.PRView) { v.Body = "hand-written" }, "marker"},
		{"another run's marker", func(v *gitx.PRView) {
			v.Body = strings.Replace(v.Body, "run="+string(gateRun), "run=20260101-000000-0000", 1)
		}, "names run"},
		{"another bead's marker", func(v *gitx.PRView) { v.Body = strings.Replace(v.Body, "bead=fx-byi", "bead=fx-zzz", 1) }, "names bead"},
		{"another number", func(v *gitx.PRView) { v.Number = 8 }, "#8"},
		{"created before the run", func(v *gitx.PRView) { v.CreatedAt = amStarted.Add(-time.Second) }, "before this run started"},
		{"another author", func(v *gitx.PRView) { v.Author = "mallory" }, "author"},
	}
	states := []struct {
		name string
		edit func(*gitx.PRView)
	}{
		{"green", func(*gitx.PRView) {}},
		{"behind", func(v *gitx.PRView) { v.MergeStateStatus = "BEHIND" }},
		{"conflicting", func(v *gitx.PRView) { v.MergeStateStatus = "DIRTY" }},
		{"failing", func(v *gitx.PRView) { v.Checks = []gitx.PRCheck{amCheck("test", "FAILURE")} }},
	}
	for _, c := range clauses {
		for _, s := range states {
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				t.Parallel()
				h := newAMHarness(t, nil)
				v := amOwnedView(amHead)
				s.edit(v)
				c.edit(v)
				// The view is served for the bead's recorded number whatever
				// number it claims, as GitHub would answer a query by number.
				h.GH.set(v)
				h.GH.mu.Lock()
				h.GH.views[amPR] = h.GH.views[v.Number]
				h.GH.mu.Unlock()
				h.pending()
				h.poll()
				h.poll()

				assert.Empty(t, h.GH.Merges, "never merged")
				assert.Empty(t, h.GH.Updates, "never updated")
				assert.Empty(t, h.Outcomes.all(), "never sent back")
				assert.Empty(t, h.Branches.Syncs, "its branch is never touched")
				assert.Empty(t, h.Ledger.Records)
				assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead), "left for a human")
				st := h.status()
				assert.Equal(t, AutoMergeNotOwned, st.State)
				assert.Contains(t, st.Reason, c.want)
				assert.Contains(t, st.Reason, "left for a human to merge")
			})
		}
	}

	t.Run("a gate that waits on another number than the bead records", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.BD.AddBead(amBead, domain.BeadStateDelivering, 9)
		h.GH.set(amOwnedView(amHead))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Zero(t, h.GH.reads(), "not even read")
		assert.Equal(t, AutoMergeNotOwned, h.status().State)
	})

	t.Run("a login gh cannot read", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.GH.loginErr = errors.New("HTTP 401")
		h.GH.set(amOwnedView(amHead))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, AutoMergeWaiting, h.status().State)
	})
}

// Only a bead of this run, in delivering, is ever looked at.
func TestAutoMergeOnlyActsOnTheRunsDeliveringBeads(t *testing.T) {
	t.Parallel()
	for _, st := range []domain.BeadState{domain.BeadStateInReview, domain.BeadStateChangesRequested, domain.BeadStateNeedsHuman, domain.BeadStateApproved} {
		h := newAMHarness(t, nil)
		h.BD.AddBead(amBead, st, amPR)
		h.GH.set(amOwnedView(amHead))
		h.pending()
		h.poll()
		assert.Zero(t, h.GH.reads(), "%s: not read", st)
		assert.Empty(t, h.GH.Merges)
	}
	h := newAMHarness(t, nil)
	h.BD.AddBead(amBead, domain.BeadStateDelivering, amPR, beads.RunLabel("20260101-000000-0000"))
	h.GH.set(amOwnedView(amHead))
	h.pending()
	h.poll()
	assert.Zero(t, h.GH.reads(), "another run's bead is not read")
	assert.Empty(t, h.GH.Merges)
}

func TestAutoMergeSpacesMergesAndMergesOnePerPoll(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	// A second green bead behind a second gate.
	const other = domain.BeadID("fx-two")
	h.BD.AddGate("fx-gt2", other, 9)
	h.BD.AddBead(other, domain.BeadStateDelivering, 9)
	h.BD.SetMeta(other, beads.MetaPRHead, "4444444444444444444444444444444444444444")
	h.GH.set(amOwnedView(amHead))
	v2 := amOwnedView("4444444444444444444444444444444444444444")
	v2.Number, v2.HeadRef = 9, "sc/fx-two-second"
	v2.Body = "<!-- slop-cannon bead=fx-two run=" + string(gateRun) + " branch=sc/fx-two-second -->"
	h.GH.set(v2)
	two := beads.GateCheck{Checked: 2, Results: []beads.GateResult{
		{GateID: amGate, Outcome: beads.GatePending}, {GateID: "fx-gt2", Outcome: beads.GatePending},
	}}
	h.BD.Answer(two)
	h.Branches.others = map[domain.BeadID]string{other: "sc/fx-two-second"}

	h.poll()
	require.Len(t, h.GH.Merges, 1, "one merge per poll, though both are green and owned")
	assert.Equal(t, "7@"+amHead, h.GH.Merges[0])
	assert.Contains(t, h.P.Stats().AutoMerge[1].Reason, "the next merge is allowed at")

	// Within the spacing nothing else merges, however many polls.
	h.Clock.Advance(9 * time.Minute)
	h.poll()
	assert.Len(t, h.GH.Merges, 1)
	lines := strings.Join(Vitals(h.Clock.Now(), nil, h.P, nil).Lines(), "\n")
	assert.Contains(t, lines, "fx-two waiting: green; the next merge is allowed at 2026-09-03 12:10:00Z (auto_merge_spacing 10m)")

	// Once it has passed, the second one merges, and only it.
	h.Clock.Advance(time.Minute)
	h.poll()
	assert.Equal(t, []string{"7@" + amHead, "9@4444444444444444444444444444444444444444"}, h.GH.Merges)
}

// The one-merge-per-poll guard holds on its own, even with a ledger that
// does not report the merge it was just told about.
func TestAutoMergeMergesAtMostOncePerPollWhateverTheLedgerSays(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, func(o *AutoMergeOptions) { o.Ledger = forgetfulLedger{} })
	const other = domain.BeadID("fx-two")
	h.BD.AddGate("fx-gt2", other, 9)
	h.BD.AddBead(other, domain.BeadStateDelivering, 9)
	h.BD.SetMeta(other, beads.MetaPRHead, "4444444444444444444444444444444444444444")
	h.Branches.others = map[domain.BeadID]string{other: "sc/fx-two-second"}
	h.GH.set(amOwnedView(amHead))
	v2 := amOwnedView("4444444444444444444444444444444444444444")
	v2.Number, v2.HeadRef = 9, "sc/fx-two-second"
	v2.Body = "<!-- slop-cannon bead=fx-two run=" + string(gateRun) + " branch=sc/fx-two-second -->"
	h.GH.set(v2)
	h.BD.Answer(beads.GateCheck{Checked: 2, Results: []beads.GateResult{
		{GateID: amGate, Outcome: beads.GatePending}, {GateID: "fx-gt2", Outcome: beads.GatePending},
	}})

	h.poll()
	assert.Len(t, h.GH.Merges, 1)
	assert.Equal(t, "green; another pull request was merged in this poll", h.P.Stats().AutoMerge[1].Reason)
	h.poll()
	assert.Len(t, h.GH.Merges, 2, "the next poll may merge the other one")
}

// forgetfulLedger accepts every record and never reports one.
type forgetfulLedger struct{}

func (forgetfulLedger) LastAutoMerge() time.Time                            { return time.Time{} }
func (forgetfulLedger) RecordAutoMerge(time.Time, domain.BeadID, int) error { return nil }
func (forgetfulLedger) AutoMergeSentBack() []string                         { return nil }
func (forgetfulLedger) RecordAutoMergeSentBack([]string) error              { return nil }
func (forgetfulLedger) AutoMergeUpdates() []string                          { return nil }
func (forgetfulLedger) RecordAutoMergeUpdate(string) error                  { return nil }

// The spacing is measured from the ledger, which is the run's state.json:
// a resumed run's poller waits out the rest of it.
func TestAutoMergeSpacingSurvivesAResume(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.Ledger.last = gateEpoch.Add(-4 * time.Minute) // merged by the previous process
	h.GH.set(amOwnedView(amHead))
	h.pending()

	h.poll()
	assert.Empty(t, h.GH.Merges, "six minutes of spacing left")
	h.Clock.Advance(5*time.Minute + 59*time.Second)
	h.poll()
	assert.Empty(t, h.GH.Merges)
	h.Clock.Advance(time.Second)
	h.poll()
	assert.Equal(t, []string{"7@" + amHead}, h.GH.Merges)
}

func TestAutoMergeDoesNotMergeWhatItCouldNotRecord(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.Ledger.err = errors.New("disk full")
	h.GH.set(amOwnedView(amHead))
	h.pending()
	h.poll()
	assert.Empty(t, h.GH.Merges, "no record, no merge")
	assert.Contains(t, h.status().Reason, "could not record the merge")
}

func TestAutoMergeReportsAMergeGitHubRefused(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.GH.mergeErr = errors.New("gh pr merge: Head branch was modified")
	h.GH.set(amOwnedView(amHead))
	h.pending()
	h.poll()
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead))
	assert.Contains(t, h.status().Reason, "gh could not merge pull request #7")
	var found bool
	for _, w := range h.Rec.warnings() {
		found = found || strings.Contains(w.Message, "gh could not merge")
	}
	assert.True(t, found)
}

// Auto-merge refuses to be switched on with any guard missing, and is off
// unless it is asked for.
func TestAutoMergeNeedsEveryPart(t *testing.T) {
	t.Parallel()
	full := func() GatePollerOptions {
		bd := newGateFakeBD()
		return GatePollerOptions{
			Gates: bd, Beads: bd, Run: gateRun, Scope: gateRun, Branch: gateBranch,
			AutoMerge: &AutoMergeOptions{
				GH: &amFakeGH{}, Branches: &amFakeBranches{}, Ledger: &amFakeLedger{}, Outcomes: &amFakeOutcomes{},
				RunStarted: amStarted,
			},
		}
	}
	p, err := NewGatePoller(full())
	require.NoError(t, err)
	assert.True(t, p.Stats().AutoMergeOn)

	breaks := map[string]func(*GatePollerOptions){
		"no scope":       func(o *GatePollerOptions) { o.Scope = "" },
		"no branch":      func(o *GatePollerOptions) { o.Branch = " " },
		"no run start":   func(o *GatePollerOptions) { o.AutoMerge.RunStarted = time.Time{} },
		"no gh":          func(o *GatePollerOptions) { o.AutoMerge.GH = nil },
		"no branches":    func(o *GatePollerOptions) { o.AutoMerge.Branches = nil },
		"no ledger":      func(o *GatePollerOptions) { o.AutoMerge.Ledger = nil },
		"no outcomes":    func(o *GatePollerOptions) { o.AutoMerge.Outcomes = nil },
		"no bead reader": func(o *GatePollerOptions) { o.Beads, o.Scope = nil, "" },
	}
	for name, br := range breaks {
		o := full()
		br(&o)
		_, err := NewGatePoller(o)
		assert.ErrorIs(t, err, ErrAutoMergeIncomplete, name)
	}

	off := full()
	off.AutoMerge = nil
	p, err = NewGatePoller(off)
	require.NoError(t, err)
	assert.False(t, p.Stats().AutoMergeOn)
	assert.Empty(t, Vitals(gateEpoch, nil, p, nil).AutoMergeLine())

	// A zero spacing is the default, never "no spacing".
	assert.Equal(t, 10*time.Minute, newAutoMerger(&AutoMergeOptions{}).spacing)
}

// With auto-merge off a green pull request is left for the human: the
// poller has no GitHub to merge through at all.
func TestAutoMergeOffNeverMerges(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate(amGate, amBead, amPR)
	h.BD.AddBead(amBead, domain.BeadStateDelivering, amPR)
	h.BD.Answer(gateCheck(t, gateOutPending, ""))
	h.poll()
	assert.Nil(t, h.P.auto)
	assert.Empty(t, h.P.Stats().AutoMerge)
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead))
}

// TestAutoMergeNeverMergesCommitsTheRunDidNotReview: ownership is judged
// per pull request, so the head is judged too (the approved-tip guard) — a
// commit pushed onto the run's own branch after delivery is never merged,
// updated or sent back, whatever GitHub says about it: the bead goes to a
// person with what to do.
func TestAutoMergeNeverMergesCommitsTheRunDidNotReview(t *testing.T) {
	t.Parallel()
	const pushed = "5555555555555555555555555555555555555555"
	for _, tc := range []struct {
		name string
		edit func(*gitx.PRView)
	}{
		{"green and clean", func(*gitx.PRView) {}},
		{"behind", func(v *gitx.PRView) { v.MergeStateStatus = "BEHIND" }},
		{"conflicting", func(v *gitx.PRView) { v.MergeStateStatus, v.Mergeable = "DIRTY", "CONFLICTING" }},
		{"failing", func(v *gitx.PRView) { v.Checks = []gitx.PRCheck{amCheck("test", "FAILURE")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newAMHarness(t, nil)
			v := amOwnedView(pushed)
			tc.edit(v)
			h.GH.set(v)
			h.pending()

			h.poll()
			assert.Empty(t, h.GH.Merges)
			assert.Empty(t, h.GH.Updates)
			assert.Empty(t, h.Branches.Syncs)
			assert.Empty(t, h.Outcomes.all())
			assert.Equal(t, []string{amHead + ".." + pushed + " on " + gateBranch}, h.Branches.Reviews)
			assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead), "a person decides")
			st := h.status()
			assert.Equal(t, AutoMergeHandedOff, st.State)
			assert.Contains(t, st.Reason, "holds commits the run did not approve")
			assert.Contains(t, st.Reason, "git log 1111111..5555555")

			h.pending()
			h.poll()
			assert.Empty(t, h.GH.Merges, "a bead in needs_human is never merged")
		})
	}

	t.Run("an update merge the run never asked for", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.Branches.markUpdate(pushed, amHead) // somebody pressed "Update branch"
		h.GH.set(amOwnedView(pushed))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
		assert.Contains(t, h.status().Reason, "is not an update slop-cannon asked for")
	})

	t.Run("a commit pushed on top of the update the run asked for", func(t *testing.T) {
		t.Parallel()
		const update = "3333333333333333333333333333333333333333"
		h := newAMHarness(t, nil)
		require.NoError(t, h.Ledger.RecordAutoMergeUpdate(AutoMergeUpdateKey(amBead, amHead)))
		h.Branches.markUpdate(update, amHead)
		h.GH.set(amOwnedView(pushed))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
		assert.Contains(t, h.status().Reason, "is neither the approved commit")
	})

	t.Run("the bead's own tip, pushed by a re-delivery that has not recorded it yet, waits", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.Branches.tip = pushed
		h.GH.set(amOwnedView(pushed))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead), "not handed off")
		assert.Equal(t, AutoMergeWaiting, h.status().State)
		assert.Contains(t, h.status().Reason, "which its delivery has not recorded yet")

		h.BD.SetMeta(amBead, beads.MetaPRHead, pushed) // the delivery records it
		h.pending()
		h.poll()
		assert.Equal(t, []string{"7@" + pushed}, h.GH.Merges)
	})

	t.Run("nothing recorded", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.BD.SetMeta(amBead, beads.MetaPRHead, "")
		h.GH.set(amOwnedView(amHead))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Empty(t, h.Branches.Reviews)
		assert.Contains(t, h.status().Reason, "no reviewed commit is recorded")
	})

	t.Run("git cannot tell", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		h.Branches.reviewErr = errors.New("fetch failed")
		h.GH.set(amOwnedView(pushed))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, AutoMergeWaiting, h.status().State)
		assert.Contains(t, h.status().Reason, "fetch failed")
		assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead), "an unanswered question is not a hand-off")
	})

	t.Run("the update merge the run asked for is merged, and judged once", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		require.NoError(t, h.Ledger.RecordAutoMergeUpdate(AutoMergeUpdateKey(amBead, amHead)))
		h.Branches.markUpdate(pushed, amHead)
		v := amOwnedView(pushed, amCheck("test", ""))
		h.GH.set(v)
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges, "checks still running")
		v.Checks = []gitx.PRCheck{amCheck("test", "SUCCESS")}
		h.GH.set(v)
		h.pending()
		h.poll()
		assert.Equal(t, []string{"7@" + pushed}, h.GH.Merges)
		assert.Len(t, h.Branches.Reviews, 1, "a head shown to be reviewed is remembered")
	})

	t.Run("another bead's update request does not cover this one", func(t *testing.T) {
		t.Parallel()
		h := newAMHarness(t, nil)
		require.NoError(t, h.Ledger.RecordAutoMergeUpdate(AutoMergeUpdateKey("fx-other", amHead)))
		h.Branches.markUpdate(pushed, amHead)
		h.GH.set(amOwnedView(pushed))
		h.pending()
		h.poll()
		assert.Empty(t, h.GH.Merges)
		assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State(amBead))
	})
}

// The approved-tip guard holds across a resume: the update a run asked for
// before it stopped is still the run's own work to the resumed poller, and
// one it never asked for still is not.
func TestAutoMergeApprovedTipGuardSurvivesAResume(t *testing.T) {
	t.Parallel()
	const update = "3333333333333333333333333333333333333333"
	first := newAMHarness(t, nil)
	v := amOwnedView(amHead)
	v.MergeStateStatus = "BEHIND"
	first.GH.set(v)
	first.pending()
	first.poll()
	require.Equal(t, []int{amPR}, first.GH.Updates)

	resumed := newAMHarness(t, func(o *AutoMergeOptions) { o.Ledger = first.Ledger })
	resumed.Branches.markUpdate(update, amHead)
	resumed.GH.set(amOwnedView(update))
	resumed.pending()
	resumed.poll()
	assert.Equal(t, []string{"7@" + update}, resumed.GH.Merges)

	fresh := newAMHarness(t, nil) // the same update, but no record of asking for it
	fresh.Branches.markUpdate(update, amHead)
	fresh.GH.set(amOwnedView(update))
	fresh.pending()
	fresh.poll()
	assert.Empty(t, fresh.GH.Merges)
	assert.Equal(t, string(domain.BeadStateNeedsHuman), fresh.BD.State(amBead))
}

// An update request that state.json could not record is not asked for:
// GitHub's merge for it could never pass the guard.
func TestAutoMergeDoesNotUpdateWhatItCouldNotRecord(t *testing.T) {
	t.Parallel()
	h := newAMHarness(t, nil)
	h.Ledger.err = errors.New("disk full")
	v := amOwnedView(amHead)
	v.MergeStateStatus = "BEHIND"
	h.GH.set(v)
	h.pending()
	h.poll()
	assert.Empty(t, h.GH.Updates)
	assert.Contains(t, h.status().Reason, "could not record in state.json that pull request #7 is to be updated")
}

// The once-per-head send-back holds across a resume: a new poller (a new
// invocation) over the same ledger hands a head that comes back with the
// same failure to a person instead of sending it round again.
func TestAutoMergeSendBackSurvivesAResume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		view func() *gitx.PRView
	}{
		{"a failed check", func() *gitx.PRView { return amOwnedView(amHead, amCheck("test", "FAILURE")) }},
		{"a conflict", func() *gitx.PRView {
			v := amOwnedView(amHead)
			v.MergeStateStatus, v.Mergeable, v.Checks = "DIRTY", "CONFLICTING", nil
			return v
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := newAMHarness(t, nil)
			first.GH.set(tc.view())
			first.pending()
			first.poll()
			require.Len(t, first.Outcomes.all(), 1)
			require.Len(t, first.Ledger.AutoMergeSentBack(), 1)

			resumed := newAMHarness(t, func(o *AutoMergeOptions) { o.Ledger = first.Ledger })
			resumed.GH.set(tc.view())
			resumed.pending()
			resumed.poll()
			assert.Empty(t, resumed.Outcomes.all(), "not sent round again")
			assert.Equal(t, string(domain.BeadStateNeedsHuman), resumed.BD.State(amBead))
			assert.Equal(t, AutoMergeHandedOff, resumed.status().State)
		})
	}
}
