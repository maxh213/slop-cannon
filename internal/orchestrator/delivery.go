package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// This file is the delivery handoff (P6-06, P6-16) and the orchestrator half
// of P5-12.
//
// A bead that every tester approved is [domain.BeadStateApproved]. The
// [Deliverer] takes it from there, exactly once per attempt, along the path
// the run's safety setting chooses (D-10):
//
//	safety on   push the branch, open (or find) the pull request, and leave
//	            the bead in delivering behind its gh:pr gate; the gate poll
//	            (P6-10) closes it when the human merges (D-20).
//	safety off  rebase onto the default branch, run the check command,
//	            fast-forward, push, then close the bead with sc_merge_sha
//	            (D-20).
//
// and it maps the three ways that goes wrong onto the D-07 table:
//
//	conflict      delivering → in_progress, back to the engineer in
//	              resolve-conflict mode (D-10). The round does not count
//	              against max_review_rounds; when the engineer's resolution
//	              changed no more than the D-09 re-review threshold the bead
//	              is re-delivered without a new review, otherwise it goes
//	              back to in_review for one.
//	check failed  delivering → changes_requested with a qa feedback item
//	              carrying the check command's output, which the feedback
//	              engine (P6-04) treats as an ordinary change-request round.
//	hand-off      delivering → needs_human when the delivery cannot proceed
//	              without a person (no remote, no gh, a diverged default
//	              branch).
//
// Two call-site rules gitx cannot enforce itself live here (P6-16):
//
//   - [gitx.PRDeliverer.Deliver] never writes `delivering` itself and leaves
//     a push-only or no-remote hand-off in needs_human, so a later retry
//     that does reach gh reports State delivering while the bead is still
//     needs_human. Every delivery therefore starts by putting the bead back
//     into delivering — BeadEventDeliveryStarted from approved,
//     BeadEventDeliveryRetried from needs_human.
//   - [gitx.Manager.CommitLeftovers] would stage and commit the conflict
//     markers of a worktree left mid-rebase by an earlier delivery. Every
//     delivery therefore probes the worktree first and, when a rebase is in
//     progress, commits nothing and sends the bead straight to the engineer
//     as a conflict round.
//
// Every bd write goes through the [StateMachine], so the D-07 table, the
// Warning on a refused transition and the [events.BeadStateChanged] are the
// same ones the rest of the orchestrator gets. The one thing written outside
// it is the sc_merge_sha metadata, which gitx records inside its own merge
// (before the close, so a crash between the two is recoverable); the close
// itself is intercepted and applied through the machine.

// SourceDelivery is the [events.Warning] Source the delivery handoff stamps
// on anything it had to hand to a human or skip.
const SourceDelivery = "delivery"

// DefaultMaxConflictRounds bounds how many times one bead may bounce
// between the delivery and its engineer for the same delivery. An engineer
// that keeps leaving the worktree mid-rebase would otherwise loop forever;
// after this many rounds the bead goes to needs_human.
const DefaultMaxConflictRounds = 3

// DefaultCheckItemCategory is the [domain.FeedbackItem] Category of the qa
// change request a failed check command raises.
const DefaultCheckItemCategory = "check"

// Delivery errors, matched with errors.Is.
var (
	// ErrNoDeliveryHost means [NewDeliverer] was called without a host.
	ErrNoDeliveryHost = errors.New("orchestrator: delivery handoff has no host")
	// ErrNoDeliveryPath means the run's safety setting needs a delivery
	// path the deliverer was not given (the pull request path with safety
	// on, the merge path with safety off).
	ErrNoDeliveryPath = errors.New("orchestrator: delivery handoff has no path for this safety setting")
	// ErrNotDeliverable means the bead is in a state the delivery cannot
	// take: only approved (a fresh delivery), needs_human (a retry of a
	// hand-off, P6-16) and delivering (a retry in flight) are deliverable.
	ErrNotDeliverable = errors.New("orchestrator: bead is not in a deliverable state")
	// ErrNoConflictRound means the bead has no conflict round outstanding.
	ErrNoConflictRound = errors.New("orchestrator: no conflict round is pending for this bead")
)

// DeliveryHost is the slice of [Run] the delivery handoff needs: who it is
// running for, what the run was configured with, and where events and log
// lines go. *Run satisfies it.
type DeliveryHost interface {
	// EventHost is where the delivery's events and log lines go, with the
	// run's own header on them.
	EventHost
	// ID is the run the delivery belongs to.
	ID() domain.RunID
	// Now is the run's clock; a test's is fake.
	Now() time.Time
	// Snapshot is the frozen configuration the run behaves by; the delivery
	// reads Safety off it (D-10, D-23).
	Snapshot() domain.ConfigSnapshot
}

// PRPath is the safety-on delivery: push the branch and open or find the
// pull request. *gitx.PRDeliverer satisfies it.
type PRPath interface {
	Deliver(ctx context.Context, req gitx.PRRequest) (*gitx.PRResult, error)
}

// *gitx.PRDeliverer is the production PRPath.
var _ PRPath = (*gitx.PRDeliverer)(nil)

// MergePath is the safety-off delivery: rebase, check, fast-forward, push.
// *gitx.Manager satisfies it.
type MergePath interface {
	DeliverMerge(ctx context.Context, bead domain.Bead, opts gitx.MergeOptions) (*gitx.MergeResult, error)
}

// *gitx.Manager is the production MergePath.
var _ MergePath = (*gitx.Manager)(nil)

// RebaseState is what a worktree looks like to the delivery before it
// commits anything: a worktree left mid-rebase by an earlier delivery must
// never have its conflict markers committed (P6-16).
type RebaseState struct {
	// InProgress is true when the worktree is in the middle of a rebase.
	InProgress bool
	// Branch is the branch being rebased, when git says.
	Branch string
	// Files are the unmerged paths, relative to the worktree root.
	Files []string
}

// DeliveryGit is the git the delivery needs around the two delivery paths:
// the default branch a conflict names as its target, the mid-rebase probe of
// P6-16, the leftovers commit of D-14, and the diffs the D-09 re-review rule
// compares. [NewDeliveryGit] wraps a *gitx.Manager; tests use a fake.
type DeliveryGit interface {
	// DefaultBranch is the repository's default branch, which is what a
	// bead's branch is rebased onto.
	DefaultBranch(ctx context.Context) (string, error)
	// RebaseState reports whether the bead's worktree is mid-rebase. It
	// fails with [gitx.ErrWorktreeMissing] when the bead has no worktree.
	RebaseState(ctx context.Context, id domain.BeadID) (RebaseState, error)
	// CommitLeftovers commits whatever the engineer left uncommitted
	// (D-14).
	CommitLeftovers(ctx context.Context, bead domain.Bead, round int) (*gitx.Leftovers, error)
	// Diff is the bead's branch against the default branch.
	Diff(ctx context.Context, id domain.BeadID) (*gitx.Diff, error)
	// ChangeRatio measures how far the branch moved between two diffs
	// (D-09).
	ChangeRatio(ctx context.Context, id domain.BeadID, previous, current *gitx.Diff) (gitx.ChangeRatio, error)
}

// managerGit is the *gitx.Manager implementation of [DeliveryGit].
type managerGit struct{ m *gitx.Manager }

// NewDeliveryGit adapts a worktree manager to [DeliveryGit]. A nil manager
// yields nil, which a [Deliverer] accepts: it then commits no leftovers and
// measures no diffs, which is what a test with pre-committed worktrees
// wants.
func NewDeliveryGit(m *gitx.Manager) DeliveryGit {
	if m == nil {
		return nil
	}
	return &managerGit{m: m}
}

// DefaultBranch implements [DeliveryGit].
func (g *managerGit) DefaultBranch(ctx context.Context) (string, error) {
	return g.m.Repo().DefaultBranch(ctx)
}

// RebaseState implements [DeliveryGit].
func (g *managerGit) RebaseState(ctx context.Context, id domain.BeadID) (RebaseState, error) {
	repo, err := g.m.Open(ctx, id)
	if err != nil {
		return RebaseState{}, err
	}
	if !repo.RebaseInProgress() {
		return RebaseState{}, nil
	}
	files, err := repo.ConflictedPaths(ctx)
	if err != nil {
		return RebaseState{InProgress: true, Branch: repo.RebasingBranch()}, err
	}
	return RebaseState{InProgress: true, Branch: repo.RebasingBranch(), Files: files}, nil
}

// CommitLeftovers implements [DeliveryGit].
func (g *managerGit) CommitLeftovers(ctx context.Context, bead domain.Bead, round int) (*gitx.Leftovers, error) {
	return g.m.CommitLeftovers(ctx, bead, round)
}

// Diff implements [DeliveryGit].
func (g *managerGit) Diff(ctx context.Context, id domain.BeadID) (*gitx.Diff, error) {
	return g.m.Diff(ctx, id)
}

// ChangeRatio implements [DeliveryGit].
func (g *managerGit) ChangeRatio(ctx context.Context, id domain.BeadID, previous, current *gitx.Diff) (gitx.ChangeRatio, error) {
	repo, err := g.m.Open(ctx, id)
	if err != nil {
		return gitx.ChangeRatio{}, err
	}
	return repo.ChangeRatio(ctx, previous, current)
}

// DeliveryKind classifies what one delivery attempt did.
type DeliveryKind string

// The delivery outcomes.
const (
	// DeliveryPending means the pull request is open and the bead stays in
	// delivering behind its gh:pr gate until a human merges it (D-20).
	DeliveryPending DeliveryKind = "pending_gate"
	// DeliveryMerged means the work landed on the default branch and the
	// bead is closed (safety off).
	DeliveryMerged DeliveryKind = "merged"
	// DeliveryHandedOff means the delivery needs a person: no remote, no
	// usable gh, or a repository state the cannon will not touch. The bead
	// is in needs_human and [DeliveryOutcome.Instructions] says what to do.
	DeliveryHandedOff DeliveryKind = "handed_off"
	// DeliveryConflict means the rebase stopped on conflicts and the bead
	// went back to its engineer in resolve-conflict mode (D-10). Call
	// [Deliverer.Resolved] when that engineer is done.
	DeliveryConflict DeliveryKind = "conflict"
	// DeliveryCheckFailed means the check command failed and the bead is in
	// changes_requested with [DeliveryOutcome.Items] to answer.
	DeliveryCheckFailed DeliveryKind = "check_failed"
	// DeliveryNeedsReview means a conflict resolution changed more than the
	// D-09 threshold, so the bead went back to in_review instead of being
	// re-delivered.
	DeliveryNeedsReview DeliveryKind = "needs_review"
	// DeliveryRetryable means the attempt failed for a reason that may pass
	// (the remote was unreachable, the default branch moved). The bead is
	// still in delivering and the caller may deliver it again.
	DeliveryRetryable DeliveryKind = "retryable"
)

// String returns the kind's name.
func (k DeliveryKind) String() string { return string(k) }

// DeliveryRequest is one bead to deliver.
type DeliveryRequest struct {
	// Bead is the bead, with the state the caller last saw it in: approved
	// for a fresh delivery, needs_human for a retry of a hand-off (P6-16),
	// delivering for a retry of an attempt that never finished.
	Bead domain.Bead
	// Round is the review round the bead was approved in; it names the
	// leftovers commit and the conflict round. Zero means the bead's own
	// Round.
	Round int
	// Agent is the engineer that last held the bead; a conflict round is
	// handed back to it. Empty mints one from the bead's assignee or the
	// bead id.
	Agent domain.AgentID
	// Report is the engineer's final implementation report, for the pull
	// request body.
	Report *domain.ImplReport
	// Verdicts are the testers' final verdicts, for the pull request body.
	Verdicts []domain.Verdict
	// Base overrides the branch the pull request targets (safety on);
	// empty means the repository's default branch. The safety-off path
	// always merges into the default branch.
	Base string
	// BaseSHA is the revision the bead's branch was created from (P4-13's
	// sc_base_sha metadata). It is how a safety-off retry tells "already
	// landed, only the close failed" from "this branch never received a
	// commit"; empty falls back to the bead's recorded sc_merge_sha.
	BaseSHA string
}

// round is the review round the request belongs to.
func (r DeliveryRequest) round() int {
	if r.Round > 0 {
		return r.Round
	}
	return r.Bead.Round
}

// DeliveryOutcome is what one call to [Deliverer.Deliver] did.
type DeliveryOutcome struct {
	// Bead is the delivered bead and Round the review round it was in.
	Bead  domain.BeadID
	Round int
	// Kind classifies the attempt.
	Kind DeliveryKind
	// State is the bead's workflow state afterwards.
	State domain.BeadState
	// Safety is the path that was taken: true for the pull request path.
	Safety bool
	// Branch is the bead's branch and Target the branch it is delivered to.
	Branch string
	Target string
	// PR is the pull request, when one was opened or reused, and Gate the
	// gh:pr gate bead that now blocks this bead (D-20).
	PR   *gitx.PullRequest
	Gate domain.BeadID
	// MergeSHA is the commit that landed on the default branch, and Pushed
	// says whether it reached the remote (safety off).
	MergeSHA string
	Pushed   bool
	// Check is the check command's outcome, when one ran.
	Check *gitx.CheckResult
	// Conflict describes the failed rebase handed to the engineer, for
	// DeliveryConflict.
	Conflict agents.EngineerConflict
	// Items are the feedback items the bead must answer, for
	// DeliveryCheckFailed.
	Items []domain.FeedbackItem
	// Ratio is the D-09 measurement of a conflict resolution, for
	// DeliveryNeedsReview and for a re-delivery that skipped the review.
	Ratio gitx.ChangeRatio
	// Reason explains a hand-off, a conflict or a retryable failure in one
	// sentence, and Instructions tell the human what to do about a
	// hand-off.
	Reason       string
	Instructions string
	// Warnings lists problems that did not stop the attempt.
	Warnings []string
	// Err is the underlying failure of a DeliveryRetryable outcome; it is
	// also returned from Deliver.
	Err error
}

// Summary renders the outcome as one human-readable line.
func (o *DeliveryOutcome) Summary() string {
	switch o.Kind {
	case DeliveryPending:
		if o.PR != nil {
			return fmt.Sprintf("%s: pull request #%d is open, waiting for the gate", o.Bead, o.PR.Number)
		}
		return fmt.Sprintf("%s: waiting for the pull request gate", o.Bead)
	case DeliveryMerged:
		return fmt.Sprintf("%s: merged into %s as %s", o.Bead, o.Target, deliveryShortSHA(o.MergeSHA))
	case DeliveryHandedOff:
		return fmt.Sprintf("%s: needs a human: %s", o.Bead, o.Reason)
	case DeliveryConflict:
		return fmt.Sprintf("%s: conflict round %d: %s", o.Bead, o.Round, o.Reason)
	case DeliveryCheckFailed:
		return fmt.Sprintf("%s: the check command failed, sent back as a qa change request", o.Bead)
	case DeliveryNeedsReview:
		return fmt.Sprintf("%s: the conflict resolution changed %.0f%% of the diff, re-reviewing", o.Bead, o.Ratio.Ratio*100)
	case DeliveryRetryable:
		return fmt.Sprintf("%s: delivery did not finish: %s", o.Bead, o.Reason)
	}
	return fmt.Sprintf("%s: %s", o.Bead, o.Kind)
}

// PendingConflict is a bead waiting for its engineer to resolve a delivery
// conflict (D-10). The [Deliverer] offers one [Demand] per pending conflict
// and keeps the context the re-delivery needs.
type PendingConflict struct {
	// Bead is the bead, as the delivery last saw it.
	Bead domain.Bead
	// Round is the review round the conflict interrupted; a conflict round
	// keeps its number (D-10).
	Round int
	// Attempt counts the conflict rounds this delivery has already had,
	// starting at 1.
	Attempt int
	// Agent is the engineer the round is handed to.
	Agent domain.AgentID
	// Conflict is what the engineer is told (P5-12).
	Conflict agents.EngineerConflict
	// Diff is the branch as it stood when the conflict was found; the D-09
	// ratio of the resolution is measured against it. Nil when the diff
	// could not be taken, which counts as "everything changed".
	Diff *gitx.Diff
	// StartedAt is when the round was opened.
	StartedAt time.Time
	// Request is the delivery this conflict interrupted; the re-delivery
	// reuses it.
	Request DeliveryRequest
}

// DeliveryOptions configures [NewDeliverer].
type DeliveryOptions struct {
	// Host is the run. Required.
	Host DeliveryHost
	// States applies the D-07 transitions. A nil machine is allowed and
	// makes every transition a dry run that still publishes its events.
	States *StateMachine
	// PR is the safety-on path; required when the run has safety on.
	PR PRPath
	// Merge is the safety-off path; required when the run has safety off.
	Merge MergePath
	// Git is the worktree side of a delivery; nil skips the leftovers
	// commit, the mid-rebase probe and the D-09 measurement.
	Git DeliveryGit
	// Beads lets the safety-off path record sc_merge_sha before the close
	// and read the bead back when a retry has to tell an already landed
	// delivery from a branch that never received a commit. The close itself
	// is always applied through States. *beads.Client satisfies it; nil
	// skips both.
	Beads gitx.BeadCloser
	// CheckCommand is the repository's check command; empty means gitx
	// autodetects one (D-10).
	CheckCommand string
	// CheckTimeout bounds it; zero means [gitx.DefaultCheckTimeout].
	CheckTimeout time.Duration
	// ReReviewThreshold is the D-09 fraction of changed lines above which a
	// conflict resolution is reviewed again; zero means
	// [gitx.DefaultReReviewThreshold].
	ReReviewThreshold float64
	// MaxConflictRounds bounds the conflict rounds one delivery may have;
	// zero means [DefaultMaxConflictRounds].
	MaxConflictRounds int
}

// Deliverer is the delivery handoff (P6-06): it ships approved beads, keeps
// the conflict rounds a delivery raises, and is the [ResumeHandler] a
// resumed run acts through (P6-15). It is safe for concurrent use, though
// the orchestrator delivers one bead at a time.
type Deliverer struct {
	host      DeliveryHost
	states    *StateMachine
	pr        PRPath
	merge     MergePath
	git       DeliveryGit
	beads     gitx.BeadCloser
	check     string
	timeout   time.Duration
	threshold float64
	maxRounds int
	safety    bool

	mu       sync.Mutex
	pending  map[domain.BeadID]PendingConflict
	attempts map[domain.BeadID]int
	checks   map[domain.BeadID]int
}

// The Deliverer is a work source (its conflict rounds) and the resume
// handler for the delivery half of P6-15.
var (
	_ WorkSource    = (*Deliverer)(nil)
	_ ResumeHandler = (*Deliverer)(nil)
)

// NewDeliverer returns a delivery handoff for one run. It fails with
// [ErrNoDeliveryHost] without a host and with [ErrNoDeliveryPath] when the
// path the run's safety setting needs is missing.
func NewDeliverer(opts DeliveryOptions) (*Deliverer, error) {
	if opts.Host == nil {
		return nil, ErrNoDeliveryHost
	}
	d := &Deliverer{
		host:      opts.Host,
		states:    opts.States,
		pr:        opts.PR,
		merge:     opts.Merge,
		git:       opts.Git,
		beads:     opts.Beads,
		check:     strings.TrimSpace(opts.CheckCommand),
		timeout:   opts.CheckTimeout,
		threshold: opts.ReReviewThreshold,
		maxRounds: opts.MaxConflictRounds,
		safety:    opts.Host.Snapshot().Safety,
		pending:   map[domain.BeadID]PendingConflict{},
		attempts:  map[domain.BeadID]int{},
		checks:    map[domain.BeadID]int{},
	}
	if d.threshold <= 0 {
		d.threshold = gitx.DefaultReReviewThreshold
	}
	if d.maxRounds <= 0 {
		d.maxRounds = DefaultMaxConflictRounds
	}
	if d.states == nil {
		d.states = NewStateMachine(nil, WithStatePublisher(PublisherFunc(d.host.Publish)),
			WithStateRun(d.host.ID()), WithStateClock(d.host.Now))
	}
	if d.safety && d.pr == nil {
		return nil, fmt.Errorf("%w: safety is on and there is no pull request path", ErrNoDeliveryPath)
	}
	if !d.safety && d.merge == nil {
		return nil, fmt.Errorf("%w: safety is off and there is no merge path", ErrNoDeliveryPath)
	}
	return d, nil
}

// Safety reports which path the deliverer takes (D-10).
func (d *Deliverer) Safety() bool { return d.safety }

// Deliver ships one approved bead.
//
// It first puts the bead into delivering — from approved with
// BeadEventDeliveryStarted, from needs_human with BeadEventDeliveryRetried,
// which is what makes a retry of a hand-off legal (P6-16) — then, unless the
// worktree was left mid-rebase, commits the engineer's leftovers (D-14) and
// takes the safety-appropriate path.
//
// The outcome says what happened and where the bead now is; an error is
// returned only for a failure the caller may retry (the outcome is then
// [DeliveryRetryable] and the bead is still in delivering) or for a bad
// request ([ErrNotDeliverable], [ErrNoDeliveryPath]). A conflict, a failed
// check and a hand-off are outcomes, not errors: the bead has moved and
// somebody else's work is queued.
func (d *Deliverer) Deliver(ctx context.Context, req DeliveryRequest) (*DeliveryOutcome, error) {
	bead := req.Bead
	if strings.TrimSpace(string(bead.ID)) == "" {
		return nil, fmt.Errorf("%w: the request has no bead", ErrNotDeliverable)
	}
	round := req.round()
	out := &DeliveryOutcome{Bead: bead.ID, Round: round, Safety: d.safety, Branch: gitx.BranchName(bead)}

	state, err := d.startDelivering(ctx, bead, round)
	if err != nil {
		return nil, err
	}
	bead.State = state
	req.Bead = bead
	out.State = state

	// P6-16: a worktree left mid-rebase must never have its conflict
	// markers committed. Probe before the leftovers commit, not after.
	if d.git != nil {
		rs, err := d.git.RebaseState(ctx, bead.ID)
		switch {
		case errors.Is(err, gitx.ErrWorktreeMissing):
			return d.handOff(ctx, req, out, "the bead has no worktree to deliver from", "", err)
		case err != nil:
			return d.retryable(out, "the worktree could not be inspected", err)
		case rs.InProgress:
			return d.conflictRound(ctx, req, out, d.conflictOf(ctx, rs.Files, ""),
				"the worktree was left mid-rebase")
		}
		if err := d.commitLeftovers(ctx, bead, round, out); err != nil {
			if errors.Is(err, gitx.ErrWorktreeMissing) {
				return d.handOff(ctx, req, out, "the bead has no worktree to deliver from", "", err)
			}
			return d.retryable(out, "the engineer's leftovers could not be committed", err)
		}
	}

	if d.safety {
		return d.deliverPR(ctx, req, out)
	}
	return d.deliverMerge(ctx, req, out)
}

// startDelivering moves the bead into delivering and returns the state it
// ends up in. A bead already in delivering is left alone: an attempt that
// died halfway is retried, not re-announced.
func (d *Deliverer) startDelivering(ctx context.Context, bead domain.Bead, round int) (domain.BeadState, error) {
	var ev BeadEvent
	switch bead.State {
	case domain.BeadStateDelivering:
		return domain.BeadStateDelivering, nil
	case domain.BeadStateApproved:
		ev = BeadEventDeliveryStarted
	case domain.BeadStateNeedsHuman:
		// P6-16(a): the pull request path hands off in needs_human without
		// writing delivering, so a retry has to put the bead back in flight
		// itself or the successful result describes a state bd never saw.
		ev = BeadEventDeliveryRetried
	default:
		return "", fmt.Errorf("%w: %s is %s (want %s, %s or %s)", ErrNotDeliverable, bead.ID,
			string(bead.State), string(domain.BeadStateApproved), string(domain.BeadStateNeedsHuman),
			string(domain.BeadStateDelivering))
	}
	res, err := d.states.Apply(ctx, StateRequest{
		Bead: bead.ID, From: bead.State, Event: ev, Round: round, Reason: d.startReason(ev),
	})
	if err != nil {
		return "", err
	}
	return res.To, nil
}

// startReason is the BeadStateChanged reason of the delivering transition.
func (d *Deliverer) startReason(ev BeadEvent) string {
	if ev == BeadEventDeliveryRetried {
		return "retrying the delivery"
	}
	if d.safety {
		return "delivering: opening a pull request"
	}
	return "delivering: merging into the default branch"
}

// commitLeftovers implements D-14 for the delivery: whatever the engineer
// left uncommitted is committed before anything is delivered, so the
// delivered diff is the reviewed one.
func (d *Deliverer) commitLeftovers(ctx context.Context, bead domain.Bead, round int, out *DeliveryOutcome) error {
	lo, err := d.git.CommitLeftovers(ctx, bead, round)
	if err != nil {
		return err
	}
	if lo != nil && lo.Committed {
		d.host.Log().Info("committed the engineer's leftovers before delivery",
			slog.String("bead", string(bead.ID)), slog.String("commit", lo.SHA))
		out.Warnings = append(out.Warnings, fmt.Sprintf("committed leftovers as %s", deliveryShortSHA(lo.SHA)))
	}
	return nil
}

// deliverPR is the safety-on path (D-10, D-20).
func (d *Deliverer) deliverPR(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome) (*DeliveryOutcome, error) {
	if d.pr == nil {
		return nil, fmt.Errorf("%w: safety is on and there is no pull request path", ErrNoDeliveryPath)
	}
	res, err := d.pr.Deliver(ctx, gitx.PRRequest{
		Bead:     req.Bead,
		Run:      d.host.ID(),
		Report:   req.Report,
		Verdicts: req.Verdicts,
		Rounds:   out.Round,
		Base:     req.Base,
	})
	if err != nil {
		return d.retryable(out, "the pull request could not be delivered", err)
	}
	out.Warnings = append(out.Warnings, res.Warnings...)
	for _, w := range res.Warnings {
		d.warn(req.Bead.ID, w, nil)
	}
	out.Branch, out.Target, out.PR, out.Gate = res.Branch, res.Base, res.PR, res.Gate
	out.Reason, out.Instructions = res.Reason, res.Instructions

	switch res.Status {
	case gitx.PRCreated, gitx.PRExisting:
		out.Kind, out.State = DeliveryPending, domain.BeadStateDelivering
		if res.PR != nil {
			d.host.Publish(events.PROpened{
				Meta:   d.meta(),
				Bead:   req.Bead.ID,
				URL:    res.PR.URL,
				Number: res.PR.Number,
				Branch: res.Branch,
				Base:   res.Base,
			})
		}
		d.host.Log().Info("delivery is waiting for the pull request gate",
			slog.String("bead", string(req.Bead.ID)), slog.String("gate", string(res.Gate)))
		return out, nil
	default:
		// PRPushOnly and PRNoRemote: gitx did what it could and already set
		// needs_human through its tracker, but it never announced the move.
		// Applying the edge here rewrites the same status (bd is idempotent
		// about that) and publishes the BeadStateChanged the run needs, so
		// nothing downstream thinks the bead is still delivering.
		return d.handOff(ctx, req, out, res.Reason, res.Instructions, nil)
	}
}

// deliverMerge is the safety-off path (D-10, D-20).
func (d *Deliverer) deliverMerge(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome) (*DeliveryOutcome, error) {
	if d.merge == nil {
		return nil, fmt.Errorf("%w: safety is off and there is no merge path", ErrNoDeliveryPath)
	}
	res, err := d.merge.DeliverMerge(ctx, req.Bead, gitx.MergeOptions{
		CheckCommand: d.check,
		CheckTimeout: d.timeout,
		BaseSHA:      req.BaseSHA,
		Beads:        d.mergeBeads(),
	})
	if res != nil {
		out.Target, out.MergeSHA, out.Pushed, out.Check = res.Target, res.Head, res.Pushed, res.Check
	}
	if err != nil {
		return d.mergeFailure(ctx, req, out, err)
	}
	d.host.Publish(events.Merged{
		Meta:   d.meta(),
		Bead:   req.Bead.ID,
		SHA:    res.Head,
		Branch: res.Target,
		Pushed: res.Pushed,
	})
	reason := fmt.Sprintf("merged into %s as %s", res.Target, deliveryShortSHA(res.Head))
	if _, err := d.states.Apply(ctx, StateRequest{
		Bead: req.Bead.ID, From: domain.BeadStateDelivering, Event: BeadEventDelivered,
		Round: out.Round, Reason: reason,
	}); err != nil {
		// The work is on the default branch; only the close failed (a
		// blocker still open, D-20, or bd being unavailable). A retry finds
		// the merge already landed and finishes the close.
		return d.retryable(out, "the merge landed but the bead could not be closed", err)
	}
	out.Kind, out.State, out.Reason = DeliveryMerged, domain.BeadStateClosed, reason
	d.forget(req.Bead.ID)
	d.host.Log().Info("delivered", slog.String("bead", string(req.Bead.ID)), slog.String("sha", res.Head))
	return out, nil
}

// mergeFailure maps a failed safety-off delivery onto the D-07 table.
func (d *Deliverer) mergeFailure(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome, err error) (*DeliveryOutcome, error) {
	var de *gitx.DeliveryError
	if !errors.As(err, &de) {
		return d.retryable(out, "the merge failed", err)
	}
	switch {
	case de.Conflict:
		// D-10: the worktree is left mid-rebase with the markers; the
		// engineer resolves them and the delivery is retried.
		return d.conflictRound(ctx, req, out, d.conflictOf(ctx, de.Files, de.Output),
			"the rebase onto the default branch stopped on conflicts")
	case de.Stage == gitx.StageCheck:
		return d.checkFailed(ctx, req, out, de)
	case handOffStage(de):
		return d.handOff(ctx, req, out, deliveryReason(de), "", err)
	default:
		return d.retryable(out, fmt.Sprintf("the delivery failed at %s", string(de.Stage)), err)
	}
}

// handOffStage reports whether a delivery failure needs a person rather than
// another attempt: the repository is in a state the cannon will not resolve
// on its own.
func handOffStage(de *gitx.DeliveryError) bool {
	switch {
	case errors.Is(de, gitx.ErrDefaultBranchDiverged),
		errors.Is(de, gitx.ErrNonLinearBranch),
		errors.Is(de, gitx.ErrNothingToDeliver),
		errors.Is(de, gitx.ErrWrongBranch),
		errors.Is(de, gitx.ErrWorktreeMissing):
		return true
	default:
		return false
	}
}

// deliveryReason renders a delivery failure as one sentence.
func deliveryReason(de *gitx.DeliveryError) string {
	if de.Err != nil {
		return fmt.Sprintf("%s failed: %s", string(de.Stage), de.Err.Error())
	}
	return fmt.Sprintf("%s failed", string(de.Stage))
}

// checkFailed treats a failing check command as a qa change-request round
// (P6-06): the bead goes to changes_requested with one blocker item holding
// the command's output, which the feedback engine hands to the engineer.
func (d *Deliverer) checkFailed(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome, de *gitx.DeliveryError) (*DeliveryOutcome, error) {
	command := ""
	if de.Check != nil {
		command = de.Check.Command
		out.Check = de.Check
	}
	item := d.checkItem(req.Bead.ID, out.Round, command, de.Output)
	if _, err := d.states.Apply(ctx, StateRequest{
		Bead: req.Bead.ID, From: domain.BeadStateDelivering, Event: BeadEventCheckFailed,
		Round: out.Round, Reason: item.Title,
	}); err != nil {
		return d.retryable(out, "the check failed and the bead could not be moved back", err)
	}
	out.Kind, out.State = DeliveryCheckFailed, domain.BeadStateChangesRequested
	out.Items, out.Reason = []domain.FeedbackItem{item}, item.Title
	d.host.Publish(events.RoundStarted{
		Meta:      d.meta(),
		Bead:      req.Bead.ID,
		Round:     out.Round,
		RoundKind: events.RoundCheckFailed,
		Lenses:    []domain.Lens{domain.LensQA},
	})
	d.warn(req.Bead.ID, "the check command failed at delivery; sending the bead back as a qa change request", de)
	return out, nil
}

// checkItem builds the qa feedback item a failed check raises. The id is
// unique within the bead across rounds and attempts, as feedback items must
// be (domain.FeedbackItem).
func (d *Deliverer) checkItem(id domain.BeadID, round int, command, output string) domain.FeedbackItem {
	d.mu.Lock()
	d.checks[id]++
	n := d.checks[id]
	d.mu.Unlock()
	title := "the repository's check command failed"
	if command != "" {
		title = fmt.Sprintf("the check command failed: %s", deliveryOneLine(command))
	}
	detail := strings.TrimSpace(output)
	if detail == "" {
		detail = "The check command exited non-zero and printed nothing."
	}
	return domain.FeedbackItem{
		ID:       fmt.Sprintf("check-r%d-%d", round, n),
		Severity: domain.SeverityBlocker,
		Category: DefaultCheckItemCategory,
		Title:    title,
		Detail:   detail,
		Status:   domain.ItemOpen,
	}
}

// handOff moves the bead to needs_human and records why.
func (d *Deliverer) handOff(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome, reason, instructions string, cause error) (*DeliveryOutcome, error) {
	if reason == "" {
		reason = "the delivery cannot continue without a person"
	}
	if _, err := d.states.Apply(ctx, StateRequest{
		Bead: req.Bead.ID, From: out.State, Event: BeadEventBlocked, Round: out.Round, Reason: reason,
	}); err != nil {
		return d.retryable(out, "the bead could not be handed to a human", err)
	}
	out.Kind, out.State = DeliveryHandedOff, domain.BeadStateNeedsHuman
	out.Reason = reason
	if instructions != "" {
		out.Instructions = instructions
	}
	d.forget(req.Bead.ID)
	message := reason
	if out.Instructions != "" {
		message = reason + "; " + out.Instructions
	}
	d.warn(req.Bead.ID, "delivery handed to a human: "+message, cause)
	return out, nil
}

// retryable records a failure the caller may try again, leaving the bead in
// delivering. The error is returned as well as recorded.
func (d *Deliverer) retryable(out *DeliveryOutcome, reason string, err error) (*DeliveryOutcome, error) {
	out.Kind, out.Reason, out.Err = DeliveryRetryable, reason, err
	if out.State == "" {
		out.State = domain.BeadStateDelivering
	}
	d.warn(out.Bead, reason, err)
	return out, err
}

// conflictOf builds the engineer's view of a failed rebase (P5-12): the
// target branch it was rebasing onto, the conflicted files and git's output.
func (d *Deliverer) conflictOf(ctx context.Context, files []string, output string) agents.EngineerConflict {
	return agents.NewEngineerConflict(d.defaultBranch(ctx), files, output)
}

// defaultBranch is the branch a delivery rebases onto; the empty string when
// it cannot be resolved (the conflict then names no target, which the prompt
// tolerates).
func (d *Deliverer) defaultBranch(ctx context.Context) string {
	if d.git == nil {
		return ""
	}
	branch, err := d.git.DefaultBranch(ctx)
	if err != nil {
		d.host.Log().Debug("could not resolve the default branch for a conflict round", slog.Any("err", err))
		return ""
	}
	return branch
}

// ConflictFrom maps a delivery failure onto the engineer's resolve-conflict
// input (P5-12). [agents.EngineerConflict] deliberately does not import
// internal/gitx, so this is the one place that copies Conflict, Files and
// Output across; target is the branch the work was being rebased onto.
//
// The second result is false when err is not a rebase conflict, in which
// case the conflict is the zero value and no conflict round is owed.
func ConflictFrom(err error, target string) (agents.EngineerConflict, bool) {
	var de *gitx.DeliveryError
	if !errors.As(err, &de) || !de.Conflict {
		return agents.EngineerConflict{}, false
	}
	return agents.NewEngineerConflict(target, de.Files, de.Output), true
}

// ConflictFromResume maps what a resume found in a worktree
// ([gitx.ResumeEntry]: Conflict, ConflictFiles) onto the same engineer
// input, for the conflict round a resumed run owes (P6-15, P5-12).
func ConflictFromResume(e gitx.ResumeEntry, target string) (agents.EngineerConflict, bool) {
	if !e.Conflict {
		return agents.EngineerConflict{}, false
	}
	return agents.NewEngineerConflict(target, e.ConflictFiles, e.Reason), true
}

// conflictRound sends the bead back to its engineer in resolve-conflict mode
// (D-10) and registers the pending round the work source offers. The round
// does not count against max_review_rounds, so the bead keeps its round
// number; it does count against MaxConflictRounds, and a bead that exhausts
// those is handed to a human instead.
func (d *Deliverer) conflictRound(ctx context.Context, req DeliveryRequest, out *DeliveryOutcome, conflict agents.EngineerConflict, reason string) (*DeliveryOutcome, error) {
	attempt := d.nextAttempt(req.Bead.ID)
	if attempt > d.maxRounds {
		return d.handOff(ctx, req, out,
			fmt.Sprintf("%s and %d conflict rounds did not resolve it", reason, d.maxRounds), "", nil)
	}
	agent := d.engineerFor(req, out.Round, attempt)
	if out.State != domain.BeadStateInProgress {
		if _, err := d.states.Apply(ctx, StateRequest{
			Bead: req.Bead.ID, From: out.State, Event: BeadEventConflict,
			Agent: agent, Round: out.Round, Reason: reason,
		}); err != nil {
			return d.retryable(out, "the conflict round could not be opened", err)
		}
	}
	bead := req.Bead
	bead.State = domain.BeadStateInProgress
	pending := PendingConflict{
		Bead:      bead,
		Round:     out.Round,
		Attempt:   attempt,
		Agent:     agent,
		Conflict:  conflict,
		Diff:      d.snapshotDiff(ctx, req.Bead.ID),
		StartedAt: d.host.Now(),
		Request:   req,
	}
	d.mu.Lock()
	d.pending[req.Bead.ID] = pending
	d.mu.Unlock()

	out.Kind, out.State, out.Conflict, out.Reason = DeliveryConflict, domain.BeadStateInProgress, conflict, reason
	commit := ""
	if pending.Diff != nil {
		commit = pending.Diff.Head()
	}
	d.host.Publish(events.RoundStarted{
		Meta:      d.meta(),
		Bead:      req.Bead.ID,
		Round:     out.Round,
		RoundKind: events.RoundConflict,
		Commit:    commit,
	})
	d.host.Log().Info("delivery conflict: sending the bead back to its engineer",
		slog.String("bead", string(req.Bead.ID)), slog.String("agent", string(agent)),
		slog.Int("attempt", attempt), slog.Any("files", conflict.Files))
	return out, nil
}

// snapshotDiff takes the branch diff a later D-09 measurement compares
// against. A failure is not fatal: a nil diff means "everything changed",
// which sends the bead back for a full review rather than skipping one.
func (d *Deliverer) snapshotDiff(ctx context.Context, id domain.BeadID) *gitx.Diff {
	if d.git == nil {
		return nil
	}
	diff, err := d.git.Diff(ctx, id)
	if err != nil {
		d.host.Log().Debug("could not snapshot the diff before a conflict round",
			slog.String("bead", string(id)), slog.Any("err", err))
		return nil
	}
	return diff
}

// engineerFor names the engineer a conflict round is handed to: the caller's
// choice, else the agent bd records as the bead's holder, else a minted id
// in the scheduler's shape with a conflict suffix so it does not collide
// with the review round's engineer.
func (d *Deliverer) engineerFor(req DeliveryRequest, round, attempt int) domain.AgentID {
	if req.Agent != "" {
		return req.Agent
	}
	if a := strings.TrimSpace(req.Bead.Assignee); a != "" {
		return domain.AgentID(a)
	}
	id := fmt.Sprintf("eng-%s-r%d-conflict", req.Bead.ID, round)
	if attempt > 1 {
		id = fmt.Sprintf("%s%d", id, attempt)
	}
	return domain.AgentID(id)
}

// nextAttempt counts the conflict rounds one delivery has had.
func (d *Deliverer) nextAttempt(id domain.BeadID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attempts[id]++
	return d.attempts[id]
}

// forget drops the per-bead bookkeeping of a delivery that ended.
func (d *Deliverer) forget(id domain.BeadID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pending, id)
	delete(d.attempts, id)
	delete(d.checks, id)
}

// Pending returns the conflict rounds still outstanding, ordered by bead id.
func (d *Deliverer) Pending() []PendingConflict {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]PendingConflict, 0, len(d.pending))
	for _, p := range d.pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead.ID < out[j].Bead.ID })
	return out
}

// PendingFor returns the conflict round outstanding for one bead.
func (d *Deliverer) PendingFor(id domain.BeadID) (PendingConflict, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.pending[id]
	return p, ok
}

// Demands implements [WorkSource]: one engineer firing per outstanding
// conflict round, re-offered every tick until [Deliverer.Resolved] or
// [Deliverer.AbandonConflict] settles it (the scheduler drops the demand
// while its agent is in flight). The payload is the [PendingConflict], whose
// Conflict field is what an [agents.EngineerRequest] in
// [agents.EngineerResolveConflict] mode needs (P5-12).
func (d *Deliverer) Demands(context.Context) ([]Demand, error) {
	pending := d.Pending()
	out := make([]Demand, 0, len(pending))
	for _, p := range pending {
		out = append(out, Demand{
			Kind:      DemandImplement,
			Bead:      p.Bead.ID,
			BeadTitle: p.Bead.Title,
			Round:     p.Round,
			Task:      events.TaskResolveConflict,
			Claim:     ClaimRework,
			// The deliverer already moved the bead to in_progress (D-10),
			// so the claim is a hand-over, not a transition.
			From:    domain.BeadStateInProgress,
			HeldBy:  domain.AgentID(strings.TrimSpace(p.Bead.Assignee)),
			Agent:   p.Agent,
			Payload: p,
		})
	}
	return out, nil
}

// Resolved is called when the engineer of a conflict round has finished.
//
// It applies D-09 to what that engineer did: when the resolution changed no
// more than the re-review threshold the bead is re-delivered straight away,
// with no new review (D-10) — it walks in_progress → in_review → approved so
// the D-07 table stays honest about where the bead has been — and otherwise
// it stops at in_review and lets the feedback engine run a full round. A
// bead with no diff to compare against is treated as "everything changed".
func (d *Deliverer) Resolved(ctx context.Context, id domain.BeadID) (*DeliveryOutcome, error) {
	p, ok := d.PendingFor(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoConflictRound, id)
	}
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()

	ratio, full := d.reReview(ctx, p)
	out := &DeliveryOutcome{
		Bead: id, Round: p.Round, Safety: d.safety, Branch: gitx.BranchName(p.Bead),
		State: domain.BeadStateInProgress, Ratio: ratio,
	}
	if _, err := d.states.Apply(ctx, StateRequest{
		Bead: id, From: domain.BeadStateInProgress, Event: BeadEventSubmitted,
		Round: p.Round, Reason: "conflict resolved",
	}); err != nil {
		return d.retryable(out, "the resolved conflict could not be submitted", err)
	}
	out.State = domain.BeadStateInReview
	if full {
		out.Kind = DeliveryNeedsReview
		out.Reason = fmt.Sprintf("the conflict resolution changed %d of %d lines, above the %.0f%% re-review threshold",
			ratio.Delta, ratio.Total, d.threshold*100)
		d.host.Log().Info("conflict resolution needs a full re-review (D-09)",
			slog.String("bead", string(id)), slog.Float64("ratio", ratio.Ratio))
		return out, nil
	}
	if _, err := d.states.Apply(ctx, StateRequest{
		Bead: id, From: domain.BeadStateInReview, Event: BeadEventApproved,
		Round: p.Round, Reason: "conflict resolved; no re-review needed (D-09)",
	}); err != nil {
		return d.retryable(out, "the resolved conflict could not be approved", err)
	}
	req := p.Request
	req.Bead = p.Bead
	req.Bead.State = domain.BeadStateApproved
	req.Round = p.Round
	res, err := d.Deliver(ctx, req)
	if res != nil {
		res.Ratio = ratio
	}
	return res, err
}

// reReview applies D-09 to a conflict round: it returns the measurement and
// whether every tester must review again.
func (d *Deliverer) reReview(ctx context.Context, p PendingConflict) (gitx.ChangeRatio, bool) {
	if d.git == nil {
		// Nothing to measure with: the delivery has no git of its own, so
		// the resolution is trusted and re-delivered.
		return gitx.ChangeRatio{}, false
	}
	if p.Diff == nil {
		// The diff before the conflict round could not be taken, so how
		// much moved is unknown: err on the side of a full review (D-09).
		return gitx.ChangeRatio{Ratio: 1}, true
	}
	current, err := d.git.Diff(ctx, p.Bead.ID)
	if err != nil {
		d.warn(p.Bead.ID, "the conflict resolution could not be measured; reviewing it again", err)
		return gitx.ChangeRatio{Ratio: 1}, true
	}
	ratio, err := d.git.ChangeRatio(ctx, p.Bead.ID, p.Diff, current)
	if err != nil {
		d.warn(p.Bead.ID, "the conflict resolution could not be measured; reviewing it again", err)
		return gitx.ChangeRatio{Ratio: 1}, true
	}
	return ratio, ratio.Exceeds(d.threshold)
}

// AbandonConflict gives up on a conflict round: the bead goes to needs_human
// and the round stops being offered. Use it when the engineer could not be
// fired or failed for good.
func (d *Deliverer) AbandonConflict(ctx context.Context, id domain.BeadID, reason string, cause error) error {
	p, ok := d.PendingFor(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoConflictRound, id)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "the delivery conflict was not resolved"
	}
	out := &DeliveryOutcome{Bead: id, Round: p.Round, Safety: d.safety, State: domain.BeadStateInProgress}
	req := p.Request
	req.Bead = p.Bead
	if _, err := d.handOff(ctx, req, out, reason, "", cause); err != nil {
		return err
	}
	return nil
}

// RedeliverPR implements [ResumeHandler]: a resumed run found a pull request
// for a bead, so the (idempotent) pull request delivery is run again to
// recreate its gh:pr gate (D-20). A bead the resume already settled, or a
// run with safety off, is left alone.
func (d *Deliverer) RedeliverPR(ctx context.Context, e gitx.ResumeEntry) error {
	if !d.safety {
		d.warn(e.Bead.ID, "a pull request exists but this run has safety off; leaving it to a human", nil)
		return nil
	}
	bead := e.Bead
	bead.State = e.State
	if e.PR != nil {
		bead.PRURL, bead.PRNumber = e.PR.URL, e.PR.Number
	}
	switch bead.State {
	case domain.BeadStateApproved, domain.BeadStateDelivering, domain.BeadStateNeedsHuman:
	default:
		d.host.Log().Debug("resume: nothing to redeliver",
			slog.String("bead", string(bead.ID)), slog.String("state", string(bead.State)))
		return nil
	}
	_, err := d.Deliver(ctx, DeliveryRequest{Bead: bead, Round: bead.Round})
	return err
}

// ConflictRound implements [ResumeHandler]: a resumed run found a worktree
// left mid-rebase, so the bead owes its engineer a conflict round before
// anything else can happen to it (D-10). The bead is moved to in_progress
// when it is not there already and the round is offered to the scheduler.
func (d *Deliverer) ConflictRound(ctx context.Context, e gitx.ResumeEntry) error {
	bead := e.Bead
	bead.State = e.State
	if _, ok := d.PendingFor(bead.ID); ok {
		return nil
	}
	conflict, ok := ConflictFromResume(e, d.defaultBranch(ctx))
	if !ok {
		conflict = agents.NewEngineerConflict(d.defaultBranch(ctx), e.ConflictFiles, e.Reason)
	}
	round := bead.Round
	out := &DeliveryOutcome{
		Bead: bead.ID, Round: round, Safety: d.safety,
		Branch: gitx.BranchName(bead), State: bead.State,
	}
	switch bead.State {
	case domain.BeadStateInProgress, domain.BeadStateDelivering:
	default:
		// The bead is somewhere the conflict edge cannot start from; record
		// the round without moving it, so the engineer is still fired.
		out.State = domain.BeadStateInProgress
	}
	req := DeliveryRequest{Bead: bead, Round: round, Agent: domain.AgentID(strings.TrimSpace(bead.Assignee))}
	res, err := d.conflictRound(ctx, req, out, conflict, "the worktree was left mid-rebase before the restart")
	if err != nil {
		return err
	}
	if res.Kind != DeliveryConflict {
		return fmt.Errorf("orchestrator: resume: %s: %s", bead.ID, res.Reason)
	}
	return nil
}

// warn publishes an [events.Warning] against the bead and logs it.
func (d *Deliverer) warn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: d.meta(), Source: SourceDelivery, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	d.host.Log().Warn(message, slog.String("source", SourceDelivery),
		slog.String("bead", string(bead)), slog.Any("err", err))
	d.host.Publish(w)
}

// meta stamps an event with the run and the run's clock.
func (d *Deliverer) meta() events.Meta { return d.host.Meta() }

// mergeBeads is what the safety-off delivery is allowed to do in bd: record
// sc_merge_sha (which must happen before the close, and inside the delivery,
// so a crash between the two is recoverable) and read the bead back for the
// already-landed check. The close is withheld and applied through the state
// machine instead, so it takes the D-07 edge and publishes its event like
// every other transition.
func (d *Deliverer) mergeBeads() gitx.BeadCloser {
	if d.beads == nil {
		return nil
	}
	return withheldClose{d.beads}
}

// withheldClose is a [gitx.BeadCloser] whose Close does nothing; see
// [Deliverer.mergeBeads].
type withheldClose struct{ gitx.BeadCloser }

// Close does nothing and reports success: the delivery's close is the state
// machine's (BeadEventDelivered).
func (withheldClose) Close(context.Context, domain.BeadID, string) (beads.Issue, error) {
	return beads.Issue{}, nil
}

// deliveryOneLine collapses s to a single line for a title.
func deliveryOneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// deliveryShortSHA abbreviates a commit for a message; it is the empty string for an
// empty sha.
func deliveryShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
