package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// Auto-merge (D-31): with safety on and auto_merge set, the gate poller does
// not only wait for a human to merge the run's pull requests — on every
// poll that finds a gate pending it may merge one itself. This file is that
// step. It runs behind every check the poller already makes (the gate names
// a bead, the bead is this run's, its status is one slop-cannon registered)
// and only for a bead in `delivering`. Then, in order:
//
//	read      `gh pr view <n> --json …` (one call: state, draft, head,
//	          base, mergeability, the check rollup, labels, body, author,
//	          creation time)
//	own       [gitx.OwnedByRun] with the bead's branch, the default
//	          branch, the bead's sc_pr_number, the run's original start
//	          and the gh login; a pull request that fails any clause is
//	          never merged, updated or sent back — it waits for a human,
//	          exactly as with auto_merge off
//	open      not OPEN, or a draft → wait
//	conflict  DIRTY / CONFLICTING → back to the engineer through the
//	          check-failed edge, one item: merge the default branch in and
//	          resolve (once per head commit)
//	failed    a check failed → back to the engineer through the same edge,
//	          one qa item per failed check with its URL (once per head
//	          commit and check)
//	behind    BEHIND → `gh pr update-branch <n>` (once per head commit),
//	          then wait for the checks again
//	checks    no checks at all, one still running, or a required name
//	          (auto_merge_checks) missing → wait
//	clean     anything but CLEAN (BLOCKED by a required review, UNSTABLE,
//	          HAS_HOOKS, UNKNOWN) → wait, never bypass
//	spacing   auto_merge_spacing not yet passed since the run's last
//	          merge → wait
//	merge     record the merge in state.json, then exactly
//	          `gh pr merge <n> --squash --match-head-commit <head>`, the
//	          head read in this same poll; at most one per poll
//
// Sending a bead back before the engineer has it starts by fast-forwarding
// its local branch onto the pull request's head ([gitx.PRDeliverer.SyncBranch]):
// after a `gh pr update-branch` GitHub's merge commit is on the remote only,
// and without it the engineer's next push would be refused. A branch that
// has diverged from the pull request is handed to a person instead. A head
// that was already sent back for the same reason and comes back unchanged
// is handed to a person too, rather than going round again.
//
// Every decision is kept per bead in [GatePollStats.AutoMerge], so
// `sc status` and the TUI header say what auto-merge is waiting for; the
// ones a person may need to act on (blocked, sent back, handed off, not the
// run's own) are also Warnings, once each.

// AutoMergeGH is the GitHub slice auto-merge drives. *gitx.GH satisfies it.
type AutoMergeGH interface {
	// ViewPR reads one pull request with everything a merge needs.
	ViewPR(ctx context.Context, number int) (*gitx.PRView, error)
	// Login is the authenticated gh login.
	Login(ctx context.Context) (string, error)
	// MergePR squash-merges a pull request at exactly head.
	MergePR(ctx context.Context, number int, head string) error
	// UpdateBranch has GitHub merge the base into the pull request.
	UpdateBranch(ctx context.Context, number int) error
}

// AutoMergeBranches is the git slice auto-merge needs. *gitx.PRDeliverer
// satisfies it.
type AutoMergeBranches interface {
	// BranchFor is the branch the bead's work is on.
	BranchFor(ctx context.Context, bead domain.Bead) (string, bool, error)
	// SyncBranch fast-forwards the bead's branch onto the pull request's
	// head before the engineer gets the bead back.
	SyncBranch(ctx context.Context, bead domain.Bead, head string) error
	// TrackingRef names a branch as the remote's tracking ref.
	TrackingRef(ctx context.Context, branch string) (string, error)
}

// AutoMergeLedger is where the time of the run's last merge lives, so the
// spacing holds across a resume. *Run satisfies it.
type AutoMergeLedger interface {
	// LastAutoMerge is when the run last merged; zero for never.
	LastAutoMerge() time.Time
	// RecordAutoMerge durably records a merge about to be asked for.
	RecordAutoMerge(at time.Time, bead domain.BeadID, pr int) error
}

// DeliveryOutcomeApplier hands a delivery outcome to whoever owns the
// bead next. *Conductor satisfies it: a [DeliveryCheckFailed] outcome puts
// the bead in front of its engineer with the items to answer.
type DeliveryOutcomeApplier interface {
	ApplyOutcome(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) error
}

// The production implementations.
var (
	_ AutoMergeGH            = (*gitx.GH)(nil)
	_ AutoMergeBranches      = (*gitx.PRDeliverer)(nil)
	_ AutoMergeLedger        = (*Run)(nil)
	_ DeliveryOutcomeApplier = (*Conductor)(nil)
)

// AutoMergeOptions turns auto-merge on for a [GatePoller] (D-31). Every
// field but Checks is required; [NewGatePoller] refuses an incomplete set
// with [ErrAutoMergeIncomplete] rather than merge with a guard missing.
type AutoMergeOptions struct {
	GH       AutoMergeGH
	Branches AutoMergeBranches
	Ledger   AutoMergeLedger
	Outcomes DeliveryOutcomeApplier
	// Checks are the auto_merge_checks names.
	Checks []string
	// Spacing is auto_merge_spacing; zero or negative means
	// [config.DefaultAutoMergeSpacing], never "no spacing".
	Spacing time.Duration
	// RunStarted is the run's original start: a pull request created
	// before it is never the run's own.
	RunStarted time.Time
}

// ErrAutoMergeIncomplete is returned by [NewGatePoller] for an auto-merge
// that lacks a part it needs to be safe: a scope, a bead reader, the
// default branch, the run's start, or one of the [AutoMergeOptions]
// interfaces.
var ErrAutoMergeIncomplete = errors.New("orchestrator: auto-merge needs a run scope, a bead reader, the default branch, the run's start, gh, the branches, a ledger and an outcome applier")

// AutoMergeDecision names what auto-merge last decided about one bead.
type AutoMergeDecision string

// The decisions.
const (
	AutoMergeWaiting   AutoMergeDecision = "waiting"
	AutoMergeBlocked   AutoMergeDecision = "blocked"
	AutoMergeUpdating  AutoMergeDecision = "updating"
	AutoMergeSentBack  AutoMergeDecision = "sent_back"
	AutoMergeHandedOff AutoMergeDecision = "handed_off"
	AutoMergeNotOwned  AutoMergeDecision = "not_owned"
	AutoMergeMerging   AutoMergeDecision = "merging"
)

// AutoMergeStatus is what auto-merge last decided about one bead, for
// `sc status` and the TUI.
type AutoMergeStatus struct {
	Bead   domain.BeadID     `json:"bead"`
	PR     int               `json:"pr,omitempty"`
	State  AutoMergeDecision `json:"state"`
	Reason string            `json:"reason"`
	At     time.Time         `json:"at"`
}

// autoMergeOnceLimit bounds the once-per-head bookkeeping like
// gateWarnKeyLimit bounds the warnings.
const autoMergeOnceLimit = 4096

// autoMerger is the poller's auto-merge state.
type autoMerger struct {
	opts    AutoMergeOptions
	spacing time.Duration
	// done holds the once-per-head keys: "update:<sha>", "conflict:<sha>",
	// "check:<sha>:<name>". It is only touched under the poller's pollMu.
	done map[string]bool
	// mergedThisPoll enforces one merge per poll; reset at the top of
	// every poll.
	mergedThisPoll bool
}

func newAutoMerger(o *AutoMergeOptions) *autoMerger {
	if o == nil {
		return nil
	}
	am := &autoMerger{opts: *o, spacing: o.Spacing, done: map[string]bool{}}
	if am.spacing <= 0 {
		am.spacing = config.DefaultAutoMergeSpacing
	}
	am.opts.Checks = append([]string(nil), o.Checks...)
	return am
}

// validAutoMerge reports whether the poller has every part a safe
// auto-merge needs.
func validAutoMerge(opts GatePollerOptions) error {
	o := opts.AutoMerge
	if o == nil {
		return nil
	}
	switch {
	case opts.Scope == "", opts.Beads == nil, strings.TrimSpace(opts.Branch) == "",
		o.RunStarted.IsZero(), o.GH == nil, o.Branches == nil, o.Ledger == nil, o.Outcomes == nil:
		return ErrAutoMergeIncomplete
	}
	return nil
}

// once reports whether key has not been acted on yet, and marks it.
func (am *autoMerger) once(key string) bool {
	if am.done[key] {
		return false
	}
	if len(am.done) >= autoMergeOnceLimit {
		am.done = make(map[string]bool, autoMergeOnceLimit)
	}
	am.done[key] = true
	return true
}

// autoMerge is the D-31 step for one pending gate. It never returns an
// error: everything it cannot do is a Warning or a status, and the next
// poll looks again.
//
// It returns the bead the gate blocks when the bead is this run's, so the
// poll can tell which statuses are still current.
func (p *GatePoller) autoMerge(ctx context.Context, r beads.GateResult) domain.BeadID {
	g, verdict := p.gate(ctx, r.GateID)
	if verdict != gateBeadAct {
		return ""
	}
	issue, state, verdict := p.bead(ctx, g)
	if verdict != gateBeadAct {
		return ""
	}
	if state == domain.BeadStateDelivering {
		p.autoMergeBead(ctx, g, issue)
	}
	// Any other state means somebody else has the bead just now (a round,
	// a person): auto-merge only ever acts on a bead that is waiting on
	// its pull request.
	return g.Blocks
}

// autoMergeBead is [GatePoller.autoMerge] for a bead in delivering.
func (p *GatePoller) autoMergeBead(ctx context.Context, g beads.Gate, issue beads.Issue) {
	am := p.auto
	id := g.Blocks
	number := issue.PRNumber()
	if gn, ok := g.PRNumber(); !ok || number <= 0 || gn != number {
		p.amStatus(id, number, AutoMergeNotOwned, fmt.Sprintf("the gate waits on #%s but the bead records #%d", g.AwaitID, number), true)
		return
	}
	bead, _ := issue.SafeBead(p.statuses)
	bead.ID = id

	view, err := am.opts.GH.ViewPR(ctx, number)
	if err != nil {
		p.amFailure(id, number, "read", fmt.Sprintf("could not read pull request #%d", number), err)
		return
	}
	login, err := am.opts.GH.Login(ctx)
	if err != nil {
		p.amFailure(id, number, "login", "could not read the gh login, so no pull request can be shown to be the run's own", err)
		return
	}
	branch, exists, err := am.opts.Branches.BranchFor(ctx, bead)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("%w: %s", gitx.ErrBranchMissing, branch)
		}
		p.amFailure(id, number, "branch", fmt.Sprintf("could not find the branch of %s", id), err)
		return
	}
	p.clearWarning("automerge-fail:" + string(id))

	if owned, why := gitx.OwnedByRun(view.PullRequest, gitx.Ownership{
		Bead: id, Run: p.scope, Branch: branch, Base: p.branch, PRNumber: number,
		RunStarted: am.opts.RunStarted, Login: login,
	}); !owned {
		p.amStatus(id, number, AutoMergeNotOwned, fmt.Sprintf("pull request #%d is not this run's own: %s; it is left for a human to merge", number, why), true)
		return
	}
	head := view.HeadSHA
	switch {
	case view.State != "OPEN":
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("pull request #%d is %s", number, strings.ToLower(view.State)), false)
		return
	case view.IsDraft:
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("pull request #%d is a draft", number), false)
		return
	case head == "":
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("GitHub reported no head commit for pull request #%d", number), false)
		return
	case view.MergeStateStatus == "DIRTY" || view.Mergeable == "CONFLICTING":
		p.amConflict(ctx, issue, bead, branch, view)
		return
	}
	checks := view.Summary(am.opts.Checks)
	if len(checks.Failed) > 0 {
		p.amChecksFailed(ctx, issue, bead, view, checks.Failed)
		return
	}
	if view.MergeStateStatus == "BEHIND" {
		p.amUpdate(ctx, id, number, head)
		return
	}
	switch {
	case checks.Total == 0:
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("pull request #%d has no checks yet", number), false)
		return
	case len(checks.Pending) > 0:
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("pull request #%d: %s still running", number, checkNames(checks.Pending)), false)
		return
	case len(checks.Missing) > 0:
		p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("pull request #%d: required %s not reported as successful", number, strings.Join(checks.Missing, ", ")), false)
		return
	case view.MergeStateStatus != "CLEAN":
		reason := fmt.Sprintf("GitHub reports pull request #%d %s", number, mergeStateWords(view.MergeStateStatus))
		p.amStatus(id, number, AutoMergeBlocked, reason, view.MergeStateStatus == "BLOCKED")
		return
	}
	now := p.clock.Now()
	if last := am.opts.Ledger.LastAutoMerge(); !last.IsZero() {
		if next := last.Add(am.spacing); now.Before(next) {
			p.amStatus(id, number, AutoMergeWaiting, fmt.Sprintf("green; the next merge is allowed at %s (auto_merge_spacing %s)",
				next.UTC().Format(vitalsTimeFormat), config.NewDuration(am.spacing).String()), false)
			return
		}
	}
	if am.mergedThisPoll {
		p.amStatus(id, number, AutoMergeWaiting, "green; another pull request was merged in this poll", false)
		return
	}
	if err := am.opts.Ledger.RecordAutoMerge(now, id, number); err != nil {
		p.amFailure(id, number, "record", "could not record the merge in state.json, so it was not made", err)
		return
	}
	am.mergedThisPoll = true
	if err := am.opts.GH.MergePR(ctx, number, head); err != nil {
		p.amFailure(id, number, "merge", fmt.Sprintf("gh could not merge pull request #%d", number), err)
		return
	}
	p.mu.Lock()
	p.stats.AutoMerged++
	p.stats.LastAutoMergeAt = now
	p.mu.Unlock()
	p.amStatus(id, number, AutoMergeMerging, fmt.Sprintf("squash-merged pull request #%d at %s; the bead closes when bd sees the merge", number, shortHead(head)), false)
	p.log.Info("auto-merged the run's own pull request",
		slog.String("bead", string(id)), slog.Int("pr", number), slog.String("head", head))
}

// amUpdate asks GitHub to merge the default branch into a pull request that
// has fallen behind it, once per head commit.
func (p *GatePoller) amUpdate(ctx context.Context, id domain.BeadID, number int, head string) {
	if !p.auto.once("update:" + head) {
		p.amStatus(id, number, AutoMergeUpdating, fmt.Sprintf("pull request #%d is behind %s; waiting for the update GitHub was asked for", number, p.branch), false)
		return
	}
	if err := p.auto.opts.GH.UpdateBranch(ctx, number); err != nil {
		p.amFailure(id, number, "update", fmt.Sprintf("gh could not update pull request #%d with %s", number, p.branch), err)
		return
	}
	p.mu.Lock()
	p.stats.AutoMergeUpdates++
	p.mu.Unlock()
	p.amStatus(id, number, AutoMergeUpdating, fmt.Sprintf("pull request #%d was behind %s; GitHub is merging it in, and the checks run again", number, p.branch), false)
}

// amConflict sends a bead whose pull request conflicts with the default
// branch back to its engineer with one item: merge the default branch in.
func (p *GatePoller) amConflict(ctx context.Context, issue beads.Issue, bead domain.Bead, branch string, view *gitx.PRView) {
	number, head := view.Number, view.HeadSHA
	key := "conflict:" + head
	if p.auto.done[key] {
		p.amHandOff(ctx, issue, number, fmt.Sprintf("pull request #%d still conflicts with %s at %s after its engineer had a round to resolve it", number, p.branch, shortHead(head)))
		return
	}
	if !p.amSync(ctx, issue, bead, view) {
		return
	}
	ref, err := p.auto.opts.Branches.TrackingRef(ctx, p.branch)
	if err != nil {
		ref = "origin/" + p.branch
	}
	item := domain.FeedbackItem{
		ID:       fmt.Sprintf("pr-conflict-r%d-%s", issue.Round(), shortHead(head)),
		Severity: domain.SeverityBlocker,
		Category: "merge",
		Title:    fmt.Sprintf("the pull request conflicts with %s: merge %s into %s and resolve the conflicts", p.branch, ref, branch),
		Detail: fmt.Sprintf("GitHub cannot merge pull request #%d: it conflicts with %s. In this worktree, on %s, run "+
			"`git merge %s` (slop-cannon has fetched it), resolve every conflict, run the tests, and commit the merge. "+
			"Do not rebase, reset or amend: the branch is already on GitHub, and slop-cannon pushes your merge to the same "+
			"pull request as an ordinary fast-forward.", number, p.branch, branch, ref),
		Status: domain.ItemOpen,
	}
	if p.amSendBack(ctx, issue, number, []domain.FeedbackItem{item}, item.Title) {
		p.auto.once(key)
	}
}

// amChecksFailed sends a bead whose pull request failed a check back to its
// engineer, one qa item per failed check, at most once per head and check.
func (p *GatePoller) amChecksFailed(ctx context.Context, issue beads.Issue, bead domain.Bead, view *gitx.PRView, failed []gitx.PRCheck) {
	number, head := view.Number, view.HeadSHA
	var fresh []gitx.PRCheck
	for _, c := range failed {
		if !p.auto.done["check:"+head+":"+c.Name] {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) == 0 {
		p.amHandOff(ctx, issue, number, fmt.Sprintf("pull request #%d: %s failed again on %s, which its engineer already had a round to fix",
			number, checkNames(failed), shortHead(head)))
		return
	}
	if !p.amSync(ctx, issue, bead, view) {
		return
	}
	items := make([]domain.FeedbackItem, 0, len(fresh))
	for i, c := range fresh {
		detail := fmt.Sprintf("GitHub reports the check %q as %s on pull request #%d at %s.", c.Name, c.Verdict(), number, shortHead(head))
		if c.URL != "" {
			detail += " Details: " + c.URL
		}
		detail += " Fix the cause on the bead's branch; slop-cannon pushes it to the same pull request and the check runs again."
		items = append(items, domain.FeedbackItem{
			ID:       fmt.Sprintf("pr-check-r%d-%s-%d", issue.Round(), shortHead(head), i+1),
			Severity: domain.SeverityBlocker,
			Category: DefaultCheckItemCategory,
			Title:    fmt.Sprintf("the pull request's check %q failed", c.Name),
			Detail:   detail,
			Status:   domain.ItemOpen,
		})
	}
	if p.amSendBack(ctx, issue, number, items, items[0].Title) {
		for _, c := range fresh {
			p.auto.once("check:" + head + ":" + c.Name)
		}
	}
}

// amSync brings the bead's branch up to the pull request's head before the
// engineer gets it, and hands the bead to a person when that cannot be
// done safely. It reports whether the send-back may go ahead.
func (p *GatePoller) amSync(ctx context.Context, issue beads.Issue, bead domain.Bead, view *gitx.PRView) bool {
	err := p.auto.opts.Branches.SyncBranch(ctx, bead, view.HeadSHA)
	switch {
	case err == nil:
		return true
	case errors.Is(err, gitx.ErrBranchDiverged):
		p.amHandOff(ctx, issue, view.Number, fmt.Sprintf("pull request #%d and the bead's branch have both moved (%v); "+
			"bring them together in the bead's worktree, then retry the bead", view.Number, err))
	default:
		p.amFailure(bead.ID, view.Number, "sync", "could not bring the bead's branch up to its pull request's head", err)
	}
	return false
}

// amSendBack moves the bead along the check-failed edge and gives it to the
// feedback loop with items, the same way a failed check at delivery does.
func (p *GatePoller) amSendBack(ctx context.Context, issue beads.Issue, number int, items []domain.FeedbackItem, reason string) bool {
	id, round := issue.ID, issue.Round()
	if !p.apply(ctx, StateRequest{
		Bead: id, From: domain.BeadStateDelivering, Event: BeadEventCheckFailed, Round: round, Reason: reason,
	}) {
		return false
	}
	p.publish(events.RoundStarted{
		Meta: p.meta(), Bead: id, Round: round, RoundKind: events.RoundCheckFailed, Lenses: []domain.Lens{domain.LensQA},
	})
	out := &DeliveryOutcome{
		Bead: id, Kind: DeliveryCheckFailed, State: domain.BeadStateChangesRequested, Round: round,
		Safety: true, Items: items, Reason: reason,
	}
	if err := p.auto.opts.Outcomes.ApplyOutcome(ctx, id, out); err != nil {
		p.warn(id, fmt.Sprintf("%s was sent back to its engineer but the feedback loop could not take it", id), err)
	}
	p.mu.Lock()
	p.stats.AutoMergeSentBack++
	p.mu.Unlock()
	p.amStatus(id, number, AutoMergeSentBack, fmt.Sprintf("pull request #%d: %s; back to its engineer", number, reason), true)
	return true
}

// amHandOff parks the bead in needs_human with the reason.
func (p *GatePoller) amHandOff(ctx context.Context, issue beads.Issue, number int, reason string) {
	if !p.apply(ctx, StateRequest{
		Bead: issue.ID, From: domain.BeadStateDelivering, Event: BeadEventBlocked, Round: issue.Round(), Reason: reason,
	}) {
		return
	}
	p.mu.Lock()
	p.stats.Blocked++
	p.mu.Unlock()
	p.amStatus(issue.ID, number, AutoMergeHandedOff, reason+"; it needs a human", true)
}

// amFailure records a step that failed for a reason the next poll may not
// share (gh unreachable, a bd write): one Warning per bead and step until
// it succeeds again.
func (p *GatePoller) amFailure(id domain.BeadID, number int, step, message string, err error) {
	p.warnOnce("automerge-fail:"+string(id)+":"+step, id, "auto-merge: "+message, err)
	reason := message
	if err != nil {
		reason += ": " + err.Error()
	}
	p.amStatus(id, number, AutoMergeWaiting, reason, false)
}

// amStatus records the decision for id; warn publishes it as a Warning the
// first time this bead reaches this state with this reason.
func (p *GatePoller) amStatus(id domain.BeadID, number int, state AutoMergeDecision, reason string, warn bool) {
	now := p.clock.Now()
	p.mu.Lock()
	prev, seen := p.amStatuses[id]
	p.amStatuses[id] = AutoMergeStatus{Bead: id, PR: number, State: state, Reason: reason, At: now}
	p.mu.Unlock()
	if seen && prev.State == state && prev.Reason == reason {
		return
	}
	p.log.Info("auto-merge", slog.String("bead", string(id)), slog.Int("pr", number),
		slog.String("state", string(state)), slog.String("reason", reason))
	if warn {
		p.warnOnce("automerge:"+string(id)+":"+string(state)+":"+reason, id, "auto-merge: "+reason, nil)
	}
}

// amForget drops the statuses of beads no longer behind a pending gate: a
// merged or closed pull request, a gate resolved by hand. A bead sent back
// or handed off keeps its status while its gate stays pending, so the
// reason stays on screen until it is delivered again.
func (p *GatePoller) amForget(keep map[domain.BeadID]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.amStatuses {
		if !keep[id] {
			delete(p.amStatuses, id)
		}
	}
}

// autoMergeStatuses returns the statuses in bead order; the caller holds
// p.mu.
func (p *GatePoller) autoMergeStatusesLocked() []AutoMergeStatus {
	if len(p.amStatuses) == 0 {
		return nil
	}
	out := make([]AutoMergeStatus, 0, len(p.amStatuses))
	for _, st := range p.amStatuses {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead < out[j].Bead })
	return out
}

// checkNames lists checks by name for a reason.
func checkNames(checks []gitx.PRCheck) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, fmt.Sprintf("%q", c.Name))
	}
	return strings.Join(names, ", ")
}

// mergeStateWords explains a mergeStateStatus that is not CLEAN.
func mergeStateWords(s string) string {
	switch s {
	case "BLOCKED":
		return "BLOCKED: a requirement such as a review is not met; auto-merge never bypasses it"
	case "UNSTABLE":
		return "UNSTABLE: a check that is not required is failing"
	case "HAS_HOOKS":
		return "HAS_HOOKS: the repository runs merge hooks GitHub has not cleared"
	case "UNKNOWN", "":
		return "UNKNOWN: GitHub has not worked out whether it can merge yet"
	}
	return s
}

// shortHead abbreviates a commit for a reason.
func shortHead(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
