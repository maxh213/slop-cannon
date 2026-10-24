package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

// External pull request review (D-11, P8-04 and P8-11).
//
// [gitx.PRWatcher] is half of D-11: it polls GitHub for open pull requests
// the cannon did not open and records one review bead per pull request
// (P4-07). [gitx.GH.PostReview] is the other half: it posts one
// consolidated review. Nothing joined the two, so no tester was ever fired
// at an external pull request and [gitx.PollResult.Stale] — a review bead
// whose pull request has been merged or closed on GitHub — was left where
// it was.
//
// [PRReview] is the join. It owns the review beads of one run:
//
//   - it polls through the watcher ([PRReview.Sync], [PRReview.Run]) and
//     publishes what the run's readers need to see a pull request at all
//     ([events.BeadCreated], [events.PROpened] with External set, and the
//     [events.RoundStarted] that tells the PRs view how many verdicts the
//     round is waiting for);
//   - it offers one [DemandReview] per lens per review bead
//     ([PRReview.Demands]), so the ordinary scheduler fires the ordinary
//     testers at it under the ordinary D-05 allocation;
//   - it plans those firings ([PRReview.PlanFiring]) against a throwaway
//     worktree holding the pull request's head, so the tester reads the
//     author's code rather than the cannon's;
//   - when every lens has answered it posts exactly one consolidated
//     review through gh and closes the review bead. It never merges
//     anything: D-11 forbids merging a pull request the cannon did not
//     open, whatever safety says, and there is no code path here that
//     could.
//
// P8-11 is the last clause: a review bead whose pull request has vanished
// is settled by [StaleReviewPolicy] — closed by default — instead of
// staying open forever with testers being offered work on a pull request
// nobody can review.
//
// # Manual verification against a throwaway repository
//
// Everything here is tested against a `gh` shim and a real git fixture, and
// no test ever touches a real pull request. To check it against GitHub for
// yourself, on a repository you are happy to throw away:
//
//  1. `gh repo create sc-prreview-demo --private --clone`, commit something
//     on the default branch and push it.
//  2. From a second branch, open a pull request whose head branch does NOT
//     start with "sc/" and which carries no "slop-cannon" label — those two
//     are how the watcher recognises the cannon's own work (D-11), and a
//     pull request carrying either is skipped rather than reviewed.
//  3. Fire the cannon at the clone with `--review-prs` (its default) and a
//     brief of your own. Within one poll interval — the `pr_poll_interval`
//     configuration key, 60 s by default, and worth turning down to a few
//     seconds for this — `sc status` shows a new review bead, the TUI's PRs
//     tab shows the pull request as "external … reviewing", and one tester
//     per lens is fired at a worktree holding the pull request's head.
//  4. When the last lens answers, exactly one review appears on the pull
//     request on GitHub — approved, or requesting changes with the items —
//     and the review bead closes. Check `gh pr view --json state`: the pull
//     request is still open. The cannon never merges it. Two more things to
//     look at while you are there: `gh pr view --json reviews` must hold
//     exactly one review however slow GitHub was (a post that outlives the
//     poll interval must not be started twice), and `git branch --list
//     'sc/*'` in your clone must show nothing for the review bead — the
//     checkout detaches and deletes the branch it borrowed (D-23).
//  5. Merge or close the pull request yourself while a review is still in
//     flight and watch P8-11: the next poll settles the review bead
//     (closed, or needs_human under [StaleReviewNeedsHuman]) and no review
//     is posted on a pull request that is no longer open. The reason bd
//     records is the watcher's own ([gitx.StaleReview.Explain]), so add the
//     "slop-cannon" label to the pull request instead of closing it and the
//     bead is settled saying the pull request is now one of the cannon's
//     own rather than claiming it has gone.

// SourcePRReview is the [events.Warning] Source the external pull request
// reviewer stamps on its own notices, following host.go's
// Source<Component> convention.
const SourcePRReview = "pr-review"

// PRReviewRound is the review round every external pull request review
// belongs to. An external review is a single pass: the cannon has no
// engineer to send an item back to, so there is no second round (D-11).
const PRReviewRound = 1

// DefaultPRReviewPostAttempts is how many times a consolidated review is
// posted before the review bead is handed to a human. Posting is one `gh
// pr review` call, so a failure is usually GitHub being unreachable, which
// the next poll may well fix.
const DefaultPRReviewPostAttempts = 3

// DefaultPRReviewPostTimeout bounds one consolidated review post. The last
// verdict of a round posts on the firing's goroutine with the firing's
// cancellation removed — the tester's session is over and the review must
// still go out — so the deadline is what keeps a stuck `gh pr review` from
// holding a stopping run open. It is deliberately shorter than
// [gitx.DefaultTimeout], the git/gh subprocess timeout underneath: a post
// that has not finished in a minute is not going to be finished by the
// caller waiting, and the next poll retries it ([PRReview.Sync]).
const DefaultPRReviewPostTimeout = time.Minute

// Errors returned by the external pull request reviewer.
var (
	// ErrNoPRWatcher is returned by [NewPRReview] without a watcher.
	ErrNoPRWatcher = errors.New("orchestrator: pr review: no watcher")
	// ErrNoPRReviewBead is returned when a demand names a review bead the
	// reviewer is not tracking (it was settled while the firing waited).
	ErrNoPRReviewBead = errors.New("orchestrator: pr review: no such review bead")
	// ErrNoPRCheckout is returned by [PRReview.PlanFiring] when no
	// [PRCheckout] was configured: a tester cannot review a pull request
	// whose code nobody fetched.
	ErrNoPRCheckout = errors.New("orchestrator: pr review: no checkout for the pull request")
)

// PRPoller is the slice of [gitx.PRWatcher] the reviewer drives: one poll
// and the interval the watcher was configured with.
type PRPoller interface {
	// Poll lists the open pull requests once and records a review bead for
	// every external one that has none.
	Poll(ctx context.Context) (*gitx.PollResult, error)
	// Interval is how often the watcher expects to be polled.
	Interval() time.Duration
}

// *gitx.PRWatcher is the production poller.
var _ PRPoller = (*gitx.PRWatcher)(nil)

// PRReviewPoster posts the consolidated review of one pull request.
// *gitx.GH satisfies it, and it has no merge method for the same reason
// [gitx.GH] has none: D-11 forbids merging a pull request the cannon did
// not open.
type PRReviewPoster interface {
	PostReview(ctx context.Context, pr gitx.PullRequest, verdicts []domain.Verdict) (gitx.Review, error)
}

// *gitx.GH is the production poster.
var _ PRReviewPoster = (*gitx.GH)(nil)

// PRReviewStore is the slice of bd the reviewer writes: closing a review
// bead whose review has gone out, and parking one whose pull request
// vanished when no state machine was given. *beads.Client satisfies it.
type PRReviewStore interface {
	// Close closes the bead with a reason (`bd close --reason`).
	Close(ctx context.Context, id domain.BeadID, reason string) (beads.Issue, error)
	// SetStatus moves the bead to a workflow status other than closed.
	SetStatus(ctx context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error)
}

// *beads.Client is the production store.
var _ PRReviewStore = (*beads.Client)(nil)

// StaleReviewPolicy says what happens to a review bead whose pull request
// is no longer open on GitHub — merged or closed by a human while the
// cannon was reviewing it ([gitx.PollResult.Stale], P8-11).
type StaleReviewPolicy string

// The stale review policies.
const (
	// StaleReviewClose closes the review bead with a reason. It is the
	// default: the pull request is gone, so the review is moot and a bead
	// nobody can act on should not sit in the run's inventory. Nothing was
	// merged by the cannon either way.
	StaleReviewClose StaleReviewPolicy = "close"
	// StaleReviewNeedsHuman parks the review bead in needs_human, for a
	// user who would rather look at every pull request the cannon was
	// reviewing when it disappeared.
	StaleReviewNeedsHuman StaleReviewPolicy = "needs_human"
	// StaleReviewIgnore leaves the bead exactly where it is and only warns.
	// The reviewer still stops offering work on it.
	StaleReviewIgnore StaleReviewPolicy = "ignore"
)

// Valid reports whether p is one of the declared policies.
func (p StaleReviewPolicy) Valid() bool {
	switch p {
	case StaleReviewClose, StaleReviewNeedsHuman, StaleReviewIgnore:
		return true
	default:
		return false
	}
}

// String returns the policy name.
func (p StaleReviewPolicy) String() string { return string(p) }

// PRReviewWork is the [Demand.Payload] of an external pull request review:
// which bead, which pull request and which lens. It is how
// [PRReview.Handles] recognises its own demands, exactly as
// [FeedbackWork] is for the feedback loop.
type PRReviewWork struct {
	// Bead is the review bead.
	Bead domain.BeadID
	// PR is the pull request under review.
	PR gitx.PullRequest
	// Lens is the tester's perspective.
	Lens domain.Lens
	// Round is always [PRReviewRound]; it is here so the payload reads
	// like the feedback loop's.
	Round int
}

// PRWorkspace is a checked-out external pull request: where a tester runs
// and what it reviews.
type PRWorkspace struct {
	// Worktree is the absolute path of the throwaway checkout holding the
	// pull request's head.
	Worktree string
	// Branch is the pull request's head branch name, for the events.
	Branch string
	// Base is the branch the pull request targets.
	Base string
	// HeadSHA is the commit the checkout holds.
	HeadSHA string
	// Diff is the pull request's change set, measured against its base.
	Diff *gitx.Diff
}

// PRCheckout prepares the workspace a tester reviews an external pull
// request in. It is a seam because there is more than one way to get a
// pull request's code (a worktree at its head, a shallow clone, a patch
// applied to a temporary tree); [PRWorktreeCheckout] is the one the run
// uses.
type PRCheckout interface {
	// Checkout makes the pull request's code available for bead, which is
	// the review bead tracking it.
	Checkout(ctx context.Context, bead domain.Bead, pr gitx.PullRequest) (PRWorkspace, error)
}

// PRWorktrees is the slice of [gitx.Manager] [PRWorktreeCheckout] needs.
// *gitx.Manager satisfies it.
type PRWorktrees interface {
	// PathFor is where the bead's worktree lives (D-23).
	PathFor(id domain.BeadID) string
	// Create makes it if it is not there.
	Create(ctx context.Context, bead domain.Bead) (*gitx.Worktree, error)
	// Open opens the bead's worktree as a repository of its own.
	Open(ctx context.Context, id domain.BeadID) (*gitx.Repo, error)
}

// *gitx.Manager is the production worktree manager.
var _ PRWorktrees = (*gitx.Manager)(nil)

// PRWorktreeCheckout puts an external pull request in the review bead's own
// worktree (D-23) and hands the tester the diff against the pull request's
// base.
//
// It only ever fetches and resets inside that worktree: the pull request's
// head is fetched by its GitHub `pull/<n>/head` reference (falling back to
// the head branch, which exists on the remote when the pull request comes
// from the same repository), the worktree is moved onto it, and the base is
// fetched so the diff is the pull request's, not the difference from
// whatever the local default branch happens to be. Nothing is ever pushed
// and the repository's own working tree is never touched, so the safety
// guarantees of D-10 are untouched by reviewing somebody else's code.
//
// The checkout is then detached from the `sc/<bead>` branch the worktree
// manager minted for it and that branch is deleted, so reviewing somebody
// else's pull request leaves no branch of ours in the user's repository
// (D-23); see [PRWorktreeCheckout.detach].
type PRWorktreeCheckout struct {
	wt     PRWorktrees
	remote string
	log    *slog.Logger
}

// PRCheckoutOption customises [NewPRWorktreeCheckout].
type PRCheckoutOption func(*PRWorktreeCheckout)

// WithPRRemote names the git remote the pull request is fetched from;
// empty keeps the repository's default remote.
func WithPRRemote(name string) PRCheckoutOption {
	return func(c *PRWorktreeCheckout) {
		if strings.TrimSpace(name) != "" {
			c.remote = name
		}
	}
}

// WithPRCheckoutLogger sets the logger; nil keeps slog.Default().
func WithPRCheckoutLogger(l *slog.Logger) PRCheckoutOption {
	return func(c *PRWorktreeCheckout) {
		if l != nil {
			c.log = l
		}
	}
}

// NewPRWorktreeCheckout returns a checkout backed by the run's worktree
// manager.
func NewPRWorktreeCheckout(wt PRWorktrees, opts ...PRCheckoutOption) (*PRWorktreeCheckout, error) {
	if wt == nil {
		return nil, errors.New("orchestrator: NewPRWorktreeCheckout: no worktree manager")
	}
	c := &PRWorktreeCheckout{wt: wt, log: slog.Default()}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Checkout implements [PRCheckout].
func (c *PRWorktreeCheckout) Checkout(ctx context.Context, bead domain.Bead, pr gitx.PullRequest) (PRWorkspace, error) {
	if bead.ID == "" {
		return PRWorkspace{}, errors.New("orchestrator: pr checkout: the review bead has no id")
	}
	if pr.Number <= 0 {
		return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: invalid pull request number %d", pr.Number)
	}
	path := c.wt.PathFor(bead.ID)
	if _, err := os.Stat(path); err != nil {
		if _, err := c.wt.Create(ctx, bead); err != nil && !errors.Is(err, gitx.ErrWorktreeExists) {
			return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: worktree for %s: %w", bead.ID, err)
		}
	}
	repo, err := c.wt.Open(ctx, bead.ID)
	if err != nil {
		return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: opening %s: %w", path, err)
	}

	remote := c.remote
	if remote == "" {
		if remote, err = repo.DefaultRemote(ctx); err != nil {
			return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: no remote to fetch #%d from: %w", pr.Number, err)
		}
	}

	// The base first: fetching it second would overwrite the FETCH_HEAD the
	// head is read from.
	base := c.fetchBase(ctx, repo, remote, pr)
	head, err := c.fetchHead(ctx, repo, remote, pr)
	if err != nil {
		return PRWorkspace{}, err
	}
	if _, err := repo.Git(ctx, "reset", "--hard", head); err != nil {
		return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: moving %s onto #%d: %w", path, pr.Number, err)
	}
	c.detach(ctx, repo, bead, pr)
	ws := PRWorkspace{Worktree: repo.Root, Branch: pr.HeadRef, Base: pr.BaseRef, HeadSHA: head}
	if ws.Worktree == "" {
		ws.Worktree = path
	}
	if base == "" {
		return ws, nil
	}
	diff, err := repo.Diff(ctx, base)
	if err != nil {
		return PRWorkspace{}, fmt.Errorf("orchestrator: pr checkout: diffing #%d against %s: %w", pr.Number, base, err)
	}
	ws.Diff = diff
	return ws, nil
}

// detach takes the review worktree off the branch [gitx.Manager.Create]
// minted for it and deletes that branch, leaving the checkout on a detached
// HEAD at the pull request's commit.
//
// This is D-23 at its narrowest: reviewing somebody else's pull request
// must leave nothing of ours in the user's repository, and an `sc/<bead>`
// branch pointing at a contributor's commit is something of ours — it
// outlives the run (cleanup keeps a branch that is not merged) and shows up
// in every `git branch` the user types afterwards. The pull request's code
// is still there to read: a detached HEAD is a checkout like any other.
//
// It never fails the checkout. A branch that could not be deleted is a
// cosmetic leftover, not a reason to refuse to review the pull request, so
// it is logged and the review goes on.
func (c *PRWorktreeCheckout) detach(ctx context.Context, repo *gitx.Repo, bead domain.Bead, pr gitx.PullRequest) {
	branch, err := repo.Git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	name := strings.TrimSpace(branch)
	if err != nil || name == "" || name == "HEAD" {
		// Already detached (a second checkout of the same pull request), or
		// git would not say: nothing to take off.
		return
	}
	if name != gitx.BranchName(bead) {
		// Not the branch we minted: somebody put this worktree somewhere
		// else, and moving their branch is not ours to do.
		c.log.WarnContext(ctx, "the review worktree is on a branch slop-cannon did not create; leaving it alone",
			slog.Int("pr", pr.Number), slog.String("branch", name))
		return
	}
	if _, err := repo.Git(ctx, "checkout", "--quiet", "--detach"); err != nil {
		c.log.WarnContext(ctx, "could not detach the review worktree; its branch is left behind",
			slog.Int("pr", pr.Number), slog.String("branch", name), slog.Any("err", err))
		return
	}
	if _, err := repo.Git(ctx, "branch", "--quiet", "-D", name); err != nil {
		c.log.WarnContext(ctx, "could not delete the review worktree's branch",
			slog.Int("pr", pr.Number), slog.String("branch", name), slog.Any("err", err))
	}
}

// fetchHead fetches the pull request's head and returns the revision the
// worktree must be moved onto.
func (c *PRWorktreeCheckout) fetchHead(ctx context.Context, repo *gitx.Repo, remote string, pr gitx.PullRequest) (string, error) {
	ref := fmt.Sprintf("pull/%d/head", pr.Number)
	_, err := repo.Git(ctx, "fetch", "--no-tags", "--quiet", remote, ref)
	if err != nil && strings.TrimSpace(pr.HeadRef) != "" {
		// A pull request from the same repository has its head branch on
		// the remote; a fork's only reachable through the pull reference,
		// which is what the first attempt asked for.
		c.log.DebugContext(ctx, "fetching the pull request reference failed; trying the head branch",
			slog.Int("pr", pr.Number), slog.String("ref", ref), slog.Any("err", err))
		_, err = repo.Git(ctx, "fetch", "--no-tags", "--quiet", remote, pr.HeadRef)
	}
	if err != nil {
		return "", fmt.Errorf("orchestrator: pr checkout: fetching #%d from %s: %w", pr.Number, remote, err)
	}
	sha, err := repo.RevParse(ctx, "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("orchestrator: pr checkout: resolving the head of #%d: %w", pr.Number, err)
	}
	return sha, nil
}

// fetchBase resolves the revision the pull request is diffed against: its
// base branch on the remote, fetched when the local clone has never seen
// it. It never fails the checkout — a diff against the local default
// branch is still a review, and saying so is the logger's job.
func (c *PRWorktreeCheckout) fetchBase(ctx context.Context, repo *gitx.Repo, remote string, pr gitx.PullRequest) string {
	name := strings.TrimSpace(pr.BaseRef)
	if name == "" {
		branch, err := repo.DefaultBranch(ctx)
		if err != nil {
			c.log.WarnContext(ctx, "no base branch for the pull request", slog.Int("pr", pr.Number), slog.Any("err", err))
			return ""
		}
		name = branch
	}
	if _, err := repo.Git(ctx, "fetch", "--no-tags", "--quiet", remote, name); err != nil {
		c.log.WarnContext(ctx, "fetching the pull request's base failed; diffing against what is here",
			slog.Int("pr", pr.Number), slog.String("base", name), slog.Any("err", err))
		if sha, err := repo.RevParse(ctx, name); err == nil {
			return sha
		}
		return ""
	}
	sha, err := repo.RevParse(ctx, "FETCH_HEAD")
	if err != nil {
		c.log.WarnContext(ctx, "resolving the pull request's base failed", slog.Int("pr", pr.Number), slog.Any("err", err))
		return ""
	}
	return sha
}

// PRReviewOptions configures [NewPRReview]. Only Watcher is required; every
// other field turns one part of the job on, so a test can exercise the
// polling half without a cartridge and the review half without GitHub.
type PRReviewOptions struct {
	// Watcher polls GitHub and records the review beads. Required.
	Watcher PRPoller
	// Poster posts the consolidated review. Nil builds the review and
	// reports it as a warning without posting anything, which is what a
	// dry run wants; the bead is then left alone.
	Poster PRReviewPoster
	// Beads is bd. Nil makes every bead write a no-op, so the reviewer
	// still drives the firings and the events.
	Beads PRReviewStore
	// States is the run's state machine, used for the one transition D-07
	// has an edge for: parking a bead in needs_human. Nil falls back to
	// Beads.SetStatus.
	States BeadParker
	// Messages records each verdict as a bead comment (D-06); nil records
	// nothing.
	Messages FeedbackMessageStore
	// Checkout prepares the pull request's code. Required to plan a firing;
	// without it [PRReview.PlanFiring] fails with [ErrNoPRCheckout].
	Checkout PRCheckout
	// Context describes the bead for the prompt builders, exactly as the
	// feedback loop's does — pass [Conductor.BeadContext]. Nil builds a
	// prompt from the review bead alone, with no repository context.
	Context FeedbackContextFunc

	// Host is the run: it stamps and receives every event and lends its
	// logger. Nil falls back to Sink, Run and Logger.
	Host EventHost
	// Sink receives the events when there is no Host; nil publishes none.
	Sink Publisher
	// Run stamps the events when there is no Host.
	Run domain.RunID
	// Statuses maps bd's registered statuses onto the D-07 states, so a
	// review bead in a status the cannon never registered is left alone
	// (P6-12).
	Statuses beads.StatusMap

	// Lenses are the tester lenses that review each pull request; nil means
	// [domain.Lenses].
	Lenses []domain.Lens
	// Stale says what to do with a review bead whose pull request has gone
	// (P8-11); the zero value is [StaleReviewClose].
	Stale StaleReviewPolicy
	// Interval is how often [PRReview.Run] polls; zero means the watcher's
	// own interval.
	Interval time.Duration
	// PostAttempts bounds how often a failing `gh pr review` is retried
	// before the bead is parked; zero means [DefaultPRReviewPostAttempts].
	PostAttempts int
	// PostTimeout bounds one post; zero means
	// [DefaultPRReviewPostTimeout]. Negative leaves the post unbounded,
	// which only a test that drives the poster itself should ask for.
	PostTimeout time.Duration
	// Clock is the time source; nil means the host's clock, else time.Now.
	Clock func() time.Time
	// Logger overrides the host's logger.
	Logger *slog.Logger
}

// PRReviewStats is a point-in-time snapshot of what the reviewer has done.
type PRReviewStats struct {
	// Polls is how many polls have completed, Failures how many failed.
	Polls    int `json:"polls"`
	Failures int `json:"failures"`
	// Tracked is how many review beads are being reviewed right now.
	Tracked int `json:"tracked"`
	// Created is how many review beads the watcher created for this run.
	Created int `json:"created"`
	// Verdicts is how many tester verdicts have come back.
	Verdicts int `json:"verdicts"`
	// Posted is how many consolidated reviews went out.
	Posted int `json:"posted"`
	// Closed is how many review beads were closed, Parked how many were
	// handed to a human.
	Closed int `json:"closed"`
	Parked int `json:"parked"`
	// Stale is how many review beads were settled because their pull
	// request had gone (P8-11).
	Stale int `json:"stale"`
	// LastAt is when the last poll finished.
	LastAt time.Time `json:"last_at,omitempty"`
}

// PRReviewStatus is one review bead as the reviewer sees it, for `sc
// status` and the TUI.
type PRReviewStatus struct {
	// Bead is the review bead.
	Bead domain.BeadID `json:"bead"`
	// Number, URL and Title identify the pull request.
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	// Lenses are the lenses reviewing it; Verdicts how many have answered.
	Lenses   []domain.Lens `json:"lenses,omitempty"`
	Verdicts int           `json:"verdicts"`
	// Posted says whether the consolidated review has gone out, and
	// Outcome what it said.
	Posted  bool                  `json:"posted,omitempty"`
	Outcome domain.VerdictOutcome `json:"outcome,omitempty"`
	// Settled says the reviewer is done with this bead: reviewed, parked,
	// or its pull request vanished.
	Settled bool `json:"settled,omitempty"`
}

// prReviewEntry is one tracked review bead.
type prReviewEntry struct {
	bead     domain.Bead
	pr       gitx.PullRequest
	lenses   []domain.Lens
	verdicts map[domain.Lens]domain.Verdict
	ws       *PRWorkspace
	order    int
	posted   bool
	settled  bool
	outcome  domain.VerdictOutcome
	attempts int
	// posting is set while a `gh pr review` for this bead is in flight, so
	// a poll that lands mid-call cannot start a second review of one pull
	// request (D-11: exactly one consolidated review). It is cleared when
	// the call returns, whether it posted or failed.
	posting bool
}

// PRReview is D-11's external pull request review, joined up: the watcher's
// review beads become tester demands, the testers' verdicts become one
// consolidated review, and the review bead is closed when it has gone out.
//
// All methods are safe for concurrent use: [PRReview.Run] polls on its own
// goroutine, [PRReview.Demands] is called from the scheduler's loop and
// [PRReview.PlanFiring] from each firing's goroutine.
type PRReview struct {
	opts     PRReviewOptions
	lenses   []domain.Lens
	interval time.Duration
	stale    StaleReviewPolicy
	attempts int
	postWait time.Duration
	now      func() time.Time
	log      *slog.Logger

	// prep serialises the checkouts, so the three testers of one pull
	// request fetch it once between them.
	prep sync.Mutex

	mu      sync.Mutex
	tracked map[domain.BeadID]*prReviewEntry
	seen    map[domain.BeadID]bool
	// preRun holds the review beads already reported as left alone
	// because their pull request predates the run (D-31).
	preRun map[domain.BeadID]bool
	stats  PRReviewStats
	next   int
}

// preRunSaidLimit bounds [PRReview.preRun]; past it a pre-run review bead
// is reported on every poll rather than remembered.
const preRunSaidLimit = 4096

// NewPRReview returns a reviewer configured by opts. It fails only without
// a watcher: every other part of the job is optional, and a reviewer
// missing one reports what it could not do rather than refusing to start.
func NewPRReview(opts PRReviewOptions) (*PRReview, error) {
	if opts.Watcher == nil {
		return nil, ErrNoPRWatcher
	}
	r := &PRReview{
		opts:     opts,
		lenses:   append([]domain.Lens(nil), opts.Lenses...),
		interval: opts.Interval,
		stale:    opts.Stale,
		attempts: opts.PostAttempts,
		postWait: opts.PostTimeout,
		now:      opts.Clock,
		log:      opts.Logger,
		tracked:  map[domain.BeadID]*prReviewEntry{},
		seen:     map[domain.BeadID]bool{},
		preRun:   map[domain.BeadID]bool{},
	}
	if len(r.lenses) == 0 {
		r.lenses = domain.Lenses()
	}
	if r.interval <= 0 {
		r.interval = opts.Watcher.Interval()
	}
	if r.interval <= 0 {
		r.interval = gitx.DefaultPRPollInterval
	}
	if !r.stale.Valid() {
		r.stale = StaleReviewClose
	}
	if r.attempts <= 0 {
		r.attempts = DefaultPRReviewPostAttempts
	}
	if r.postWait == 0 {
		r.postWait = DefaultPRReviewPostTimeout
	}
	if r.now == nil {
		if opts.Host != nil {
			r.now = func() time.Time { return opts.Host.Meta().Time }
		} else {
			r.now = time.Now
		}
	}
	if r.log == nil {
		if opts.Host != nil {
			r.log = opts.Host.Log()
		} else {
			r.log = slog.Default()
		}
	}
	return r, nil
}

// Interval is how often [PRReview.Run] polls.
func (r *PRReview) Interval() time.Duration { return r.interval }

// Lenses are the tester lenses every external pull request is reviewed
// through.
func (r *PRReview) Lenses() []domain.Lens { return append([]domain.Lens(nil), r.lenses...) }

// StalePolicy is what the reviewer does with a review bead whose pull
// request has gone (P8-11).
func (r *PRReview) StalePolicy() StaleReviewPolicy { return r.stale }

// Stats returns a snapshot of the counters.
func (r *PRReview) Stats() PRReviewStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Tracked = 0
	for _, e := range r.tracked {
		if !e.settled {
			s.Tracked++
		}
	}
	return s
}

// Reviews returns one status per review bead the reviewer has adopted, in
// the order the pull requests were adopted.
func (r *PRReview) Reviews() []PRReviewStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PRReviewStatus, 0, len(r.tracked))
	for _, e := range r.tracked {
		out = append(out, PRReviewStatus{
			Bead:     e.bead.ID,
			Number:   e.pr.Number,
			URL:      e.pr.URL,
			Title:    e.pr.Title,
			Lenses:   append([]domain.Lens(nil), e.lenses...),
			Verdicts: len(e.verdicts),
			Posted:   e.posted,
			Outcome:  e.outcome,
			Settled:  e.settled,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// Run polls immediately and then every [PRReview.Interval] until ctx is
// done. A poll that fails because gh is missing, not authenticated, or the
// repository has no GitHub remote ends the loop with that error, exactly as
// [gitx.PRWatcher.Run] does: polling again cannot help. Any other failure
// is a warning and the next tick tries again. When ctx ends, Run returns
// nil.
func (r *PRReview) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		switch err := r.Sync(ctx); {
		case err == nil:
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, gitx.ErrGHNotFound),
			errors.Is(err, gitx.ErrGHUnauthenticated),
			errors.Is(err, gitx.ErrNoGitHubRemote):
			r.warn("", "external pull request review stopped", err)
			return err
		default:
			r.warn("", "polling for external pull requests failed; retrying", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Sync is one pass: poll GitHub, adopt the review beads the watcher found,
// settle the ones whose pull request has gone (P8-11), and retry any
// consolidated review that could not be posted last time.
func (r *PRReview) Sync(ctx context.Context) error {
	res, err := r.opts.Watcher.Poll(ctx)
	r.mu.Lock()
	if err != nil {
		r.stats.Failures++
	}
	r.stats.Polls++
	r.stats.LastAt = r.now()
	r.mu.Unlock()
	if res != nil {
		for _, rb := range res.New {
			r.adopt(rb, true)
		}
		for _, rb := range res.Existing {
			r.adopt(rb, false)
		}
		r.settleStale(ctx, res.Stale)
	}
	r.retryPosts(ctx)
	return err
}

// adopt starts tracking one review bead. A bead already settled in bd —
// reviewed by an earlier run, or closed by a human — is not reviewed
// again, and neither is one in a status the cannon never registered
// (P6-12).
//
// A bead already being tracked is re-read rather than skipped: its pull
// request is refreshed, and a bead something else has settled since the
// last poll (a human, or the scheduler giving up on its firings) stops
// being offered to testers here, which is how the reviewer lets go of work
// it can no longer do.
func (r *PRReview) adopt(rb gitx.ReviewBead, created bool) {
	bead, _ := rb.Issue.SafeBead(r.opts.Statuses)
	if bead.ID == "" {
		return
	}
	r.mu.Lock()
	if r.seen[bead.ID] {
		gone := false
		if e, ok := r.tracked[bead.ID]; ok {
			e.pr = rb.PR
			// bd is the truth about the bead. Somebody else may have
			// settled it since the last poll — a human closing it, or the
			// scheduler taking it down the failed edge after giving up on
			// its firings (P6-31) — and a settled bead is not reviewed any
			// further.
			if !e.settled && BeadStateSettled(bead.State) {
				e.bead.State, e.settled, gone = bead.State, true, true
			}
		}
		r.mu.Unlock()
		if gone {
			r.warn(bead.ID, fmt.Sprintf("the review bead of pull request #%d is %s; no review will be posted",
				rb.PR.Number, bead.State), nil)
		}
		return
	}
	r.seen[bead.ID] = true
	if created {
		r.stats.Created++
	}
	if BeadStateSettled(bead.State) {
		r.mu.Unlock()
		r.log.Info("external pull request already reviewed; leaving it alone",
			slog.String("bead", bead.ID.String()), slog.Int("pr", rb.PR.Number), slog.String("state", string(bead.State)))
		return
	}
	e := &prReviewEntry{
		bead:     bead,
		pr:       rb.PR,
		lenses:   append([]domain.Lens(nil), r.lenses...),
		verdicts: map[domain.Lens]domain.Verdict{},
		order:    r.next,
	}
	r.next++
	r.tracked[bead.ID] = e
	r.mu.Unlock()

	if created {
		r.publish(events.BeadCreated{Meta: r.meta(), Bead: bead})
	}
	r.publish(events.PROpened{
		Meta:     r.meta(),
		Bead:     bead.ID,
		URL:      rb.PR.URL,
		Number:   rb.PR.Number,
		Branch:   rb.PR.HeadRef,
		Base:     rb.PR.BaseRef,
		External: true,
	})
	// The PRs view reads the round to say how many verdicts it is waiting
	// for, so the lenses are announced before any tester is fired (P7-05).
	r.publish(events.RoundStarted{
		Meta:      r.meta(),
		Bead:      bead.ID,
		Round:     PRReviewRound,
		RoundKind: events.RoundReview,
		Lenses:    append([]domain.Lens(nil), e.lenses...),
		Commit:    rb.PR.HeadSHA,
	})
	r.log.Info("reviewing an external pull request",
		slog.String("bead", bead.ID.String()), slog.Int("pr", rb.PR.Number), slog.String("url", rb.PR.URL))
}

// Demands implements [WorkSource]: one review demand per lens that has not
// answered yet, for every pull request still being reviewed. The scheduler
// drops the ones already in flight, so this stays stateless.
//
// Each demand carries the review bead's state in [Demand.From] even though
// it claims nothing, so the scheduler can settle the bead if it gives up on
// the work (P6-31): a checkout that keeps failing must leave a failed bead
// behind, not an open one nobody will ever finish.
func (r *PRReview) Demands(_ context.Context) ([]Demand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Demand
	for _, e := range r.tracked {
		if e.settled || e.posted {
			continue
		}
		for _, l := range e.lenses {
			if _, done := e.verdicts[l]; done {
				continue
			}
			out = append(out, Demand{
				Kind:      DemandReview,
				Bead:      e.bead.ID,
				BeadTitle: e.bead.Title,
				Lens:      l,
				Round:     PRReviewRound,
				Task:      events.TaskReview,
				Claim:     ClaimNone,
				// A review demand claims nothing — the tester never holds
				// the review bead's lease — but the scheduler still has to
				// know where the bead is to take it down the D-07 failed
				// edge when it gives up on the demand (P6-31,
				// Scheduler.parkFrom). Without From the give-up path
				// cannot park the bead, and a run whose stop condition
				// waits for every bead of the run to settle waits for ever.
				From:    e.bead.State,
				Order:   e.order,
				Payload: PRReviewWork{Bead: e.bead.ID, PR: e.pr, Lens: l, Round: PRReviewRound},
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Lens < out[j].Lens
	})
	return out, nil
}

// Handles reports whether the demand is one of this reviewer's.
func (r *PRReview) Handles(d Demand) bool {
	w, ok := d.Payload.(PRReviewWork)
	if !ok {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, tracked := r.tracked[w.Bead]
	return tracked && !e.settled
}

// PlanFiring implements [FiringPlanner]: it turns one review demand into
// the tester session that serves it, reading the pull request's own code
// out of the workspace [PRCheckout] prepared.
func (r *PRReview) PlanFiring(ctx context.Context, fi Firing) (AgentFiring, error) {
	w, ok := fi.Demand.Payload.(PRReviewWork)
	if !ok {
		return AgentFiring{}, fmt.Errorf("%w: %s wants %s", ErrUnknownFeedbackWork, fi.Agent.ID, fi.Demand.Kind)
	}
	r.mu.Lock()
	e, tracked := r.tracked[w.Bead]
	var bead domain.Bead
	if tracked {
		bead = e.bead
	}
	r.mu.Unlock()
	if !tracked {
		return AgentFiring{}, fmt.Errorf("%w: %s", ErrNoPRReviewBead, w.Bead)
	}

	ws, err := r.workspace(ctx, bead, w.PR)
	if err != nil {
		return AgentFiring{}, err
	}

	bc := FeedbackBeadContext{}
	if r.opts.Context != nil {
		if bc, err = r.opts.Context(ctx, w.Bead); err != nil {
			return AgentFiring{}, fmt.Errorf("orchestrator: pr review context for %s: %w", w.Bead, err)
		}
	}
	if bc.Bead.Bead.ID == "" {
		bc.Bead.Bead = bead
	}
	// The workspace wins over whatever the shared context said: the tester
	// reviews the pull request's head, not the branch a bead of the run's
	// own work would have.
	bc.Bead.Worktree = ws.Worktree
	bc.Bead.Branch = ws.Branch
	bc.Bead.Round = PRReviewRound
	bc.Bead.Notes = append(bc.Bead.Notes, prReviewNotes(w.PR)...)

	req := agents.TesterRequest{
		Mode:       agents.TesterModeReview,
		Lens:       w.Lens,
		RunID:      r.runID(),
		AgentID:    fi.Agent.ID,
		Workdir:    ws.Worktree,
		Round:      PRReviewRound,
		Bead:       bc.Bead,
		Repo:       bc.Repo,
		Brief:      bc.Brief,
		Rules:      bc.Rules,
		Vibes:      bc.Vibes,
		Diff:       ws.Diff,
		Notes:      bc.Notes,
		Budget:     bc.Budget,
		Model:      bc.Model,
		MaxTurns:   bc.MaxTurns,
		Timeout:    bc.Timeout,
		InheritEnv: bc.InheritEnv,
	}
	if err := req.Validate(); err != nil {
		return AgentFiring{}, err
	}
	env := bc.Env
	return AgentFiring{
		Agent:     req.Agent(),
		Task:      fi.Demand.FiringTask(),
		Round:     PRReviewRound,
		BeadTitle: bead.Title,
		Worktree:  ws.Worktree,
		// A tester never holds the review bead's lease: the bead belongs to
		// the run's bookkeeping, not to the agent reading somebody else's
		// pull request.
		NoLease: true,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			q := req
			q.Env = mergeEnv(env, fc.Env)
			q.Timeout = fc.Timeout
			spec, _, err := agents.TesterRunSpec(q)
			if err != nil && !errors.Is(err, agents.ErrPromptOverBudget) {
				return agents.AgentRun{}, err
			}
			if err != nil {
				r.warn(w.Bead, fmt.Sprintf("the %s review of pull request #%d is over its token budget", q.Lens, w.PR.Number), err)
			}
			return agents.AgentRun{
				Spec: spec, Message: domain.MessageVerdict,
				Check: agents.TesterVerdictCheck(q), Now: r.now,
			}, nil
		},
		Result: func(payload any, _ agents.Transcript) error {
			return r.verdictReceived(context.WithoutCancel(ctx), w, fi.Agent.ID, payload)
		},
	}, nil
}

// prReviewNotes are the lines every external review prompt carries: what
// the agent is looking at, and the two things it must not do.
func prReviewNotes(pr gitx.PullRequest) []string {
	notes := []string{
		fmt.Sprintf("this is external pull request #%d, opened by somebody else (%s)", pr.Number, prAuthor(pr)),
		"the worktree holds the pull request's head: read it, never change it",
		"your verdict is consolidated with the other lenses' into one review posted with gh; " +
			"slop-cannon never merges a pull request it did not open",
	}
	if pr.URL != "" {
		notes = append(notes, "pull request: "+pr.URL)
	}
	if pr.IsDraft {
		notes = append(notes, "the pull request is still a draft")
	}
	return notes
}

// prAuthor names the pull request's author, "an unknown author" when gh
// reported none.
func prAuthor(pr gitx.PullRequest) string {
	if strings.TrimSpace(pr.Author) == "" {
		return "an unknown author"
	}
	return pr.Author
}

// workspace returns the checkout of the pull request, preparing it once for
// however many testers review it.
func (r *PRReview) workspace(ctx context.Context, bead domain.Bead, pr gitx.PullRequest) (PRWorkspace, error) {
	r.mu.Lock()
	e, tracked := r.tracked[bead.ID]
	if tracked && e.ws != nil {
		ws := *e.ws
		r.mu.Unlock()
		return ws, nil
	}
	r.mu.Unlock()
	if r.opts.Checkout == nil {
		return PRWorkspace{}, fmt.Errorf("%w: #%d", ErrNoPRCheckout, pr.Number)
	}

	r.prep.Lock()
	defer r.prep.Unlock()
	r.mu.Lock()
	if e, tracked := r.tracked[bead.ID]; tracked && e.ws != nil {
		ws := *e.ws
		r.mu.Unlock()
		return ws, nil
	}
	r.mu.Unlock()

	ws, err := r.opts.Checkout.Checkout(ctx, bead, pr)
	if err != nil {
		return PRWorkspace{}, err
	}
	r.mu.Lock()
	if e, tracked := r.tracked[bead.ID]; tracked {
		copied := ws
		e.ws = &copied
	}
	r.mu.Unlock()
	return ws, nil
}

// verdictReceived folds one tester's verdict in and, when the round is
// complete, posts the consolidated review.
func (r *PRReview) verdictReceived(ctx context.Context, w PRReviewWork, agent domain.AgentID, payload any) error {
	v, ok := payload.(domain.Verdict)
	if !ok {
		return fmt.Errorf("orchestrator: tester %s produced %T, not a verdict", agent, payload)
	}
	if v.Lens == "" {
		v.Lens = w.Lens
	}
	if v.Round == 0 {
		v.Round = PRReviewRound
	}
	r.mu.Lock()
	e, tracked := r.tracked[w.Bead]
	if !tracked {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoPRReviewBead, w.Bead)
	}
	if _, dup := e.verdicts[w.Lens]; !dup {
		r.stats.Verdicts++
	}
	e.verdicts[w.Lens] = v
	complete := !e.settled && !e.posted && len(e.verdicts) >= len(e.lenses)
	r.mu.Unlock()

	r.record(ctx, w.Bead, agent, v)
	r.publish(events.VerdictReceived{
		Meta: r.meta(), Bead: w.Bead, Round: v.Round, Agent: agent, Verdict: v,
	})
	if !complete {
		return nil
	}
	// A failure to post is not the tester's failure: the verdict is in and
	// the firing succeeded, so it is reported and retried by the next poll
	// rather than raised here (a Result error would fail the agent, D-06).
	pctx, cancel := r.postContext(ctx)
	defer cancel()
	r.post(pctx, w.Bead)
	return nil
}

// postContext bounds the detached context the last verdict posts under.
// The caller has already removed the firing's cancellation ([PRReview.
// PlanFiring] hands the result callback a context.WithoutCancel, because a
// review must still go out when the tester's session ends), which without a
// deadline would let one unreachable GitHub hold a stopping run open for
// the whole subprocess timeout.
func (r *PRReview) postContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.postWait <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.postWait)
}

// post builds and posts the one consolidated review of a bead whose lenses
// have all answered, then closes the review bead.
//
// It is safe to call more than once and safe to call concurrently, which is
// what makes D-11's "exactly one consolidated review" true rather than
// likely: a bead whose review has gone out is never posted again, and a
// bead whose review is in flight is skipped instead of posted beside
// itself. Posting can outlive a poll interval — `gh pr review` is a network
// call — so without the in-flight flag the next [PRReview.Sync] would see a
// bead with every verdict in and no review posted and start a second one
// while the first was still talking to GitHub.
func (r *PRReview) post(ctx context.Context, id domain.BeadID) {
	r.mu.Lock()
	e, tracked := r.tracked[id]
	if !tracked || e.settled || e.posted || e.posting || len(e.verdicts) < len(e.lenses) {
		r.mu.Unlock()
		return
	}
	pr, bead := e.pr, e.bead
	verdicts := make([]domain.Verdict, 0, len(e.verdicts))
	for _, l := range e.lenses {
		if v, ok := e.verdicts[l]; ok {
			verdicts = append(verdicts, v)
		}
	}
	e.attempts++
	attempt := e.attempts
	e.posting = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		e.posting = false
		r.mu.Unlock()
	}()

	if r.opts.Poster == nil {
		review, err := gitx.BuildReview(pr, verdicts)
		if err != nil {
			r.warn(id, fmt.Sprintf("could not build the review of pull request #%d", pr.Number), err)
			return
		}
		r.mu.Lock()
		e.outcome, e.settled = prOutcome(review.Event), true
		r.mu.Unlock()
		r.warn(id, fmt.Sprintf("no gh: the review of pull request #%d (%s) was not posted", pr.Number, review.Event), nil)
		return
	}

	review, err := r.opts.Poster.PostReview(ctx, pr, verdicts)
	if err != nil {
		r.warn(id, fmt.Sprintf("posting the review of pull request #%d failed (attempt %d of %d)", pr.Number, attempt, r.attempts), err)
		if attempt >= r.attempts {
			r.park(ctx, id, fmt.Sprintf("the review of pull request #%d could not be posted after %d attempts", pr.Number, attempt))
		}
		return
	}
	r.mu.Lock()
	e.posted, e.outcome = true, prOutcome(review.Event)
	r.stats.Posted++
	r.mu.Unlock()
	r.log.Info("posted a review on an external pull request",
		slog.String("bead", id.String()), slog.Int("pr", pr.Number), slog.String("event", string(review.Event)))

	reason := fmt.Sprintf("reviewed pull request #%d: %s (slop-cannon never merges a pull request it did not open)",
		pr.Number, review.Event)
	r.close(ctx, bead, reason)
}

// prOutcome maps a posted review's event onto the verdict outcome the run's
// readers speak.
func prOutcome(ev gitx.ReviewEvent) domain.VerdictOutcome {
	if ev == gitx.ReviewApprove {
		return domain.VerdictApprove
	}
	return domain.VerdictChanges
}

// retryPosts re-attempts every consolidated review that has all its
// verdicts but never went out, so a poll can finish what a failed `gh pr
// review` started. A review that is still in flight is left alone: the
// attempt that is running is the one attempt this pull request gets.
func (r *PRReview) retryPosts(ctx context.Context) {
	r.mu.Lock()
	var ids []domain.BeadID
	for id, e := range r.tracked {
		if !e.settled && !e.posted && !e.posting && e.attempts > 0 && len(e.verdicts) >= len(e.lenses) {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		func() {
			pctx, cancel := r.postContext(ctx)
			defer cancel()
			r.post(pctx, id)
		}()
	}
}

// settleStale is P8-11: a review bead the poll could not match to an open
// external pull request is settled by the configured [StaleReviewPolicy]
// and never offered to a tester again. The watcher only reports such beads;
// deciding what happens to them is this component's job.
//
// The reason written into bd and into the run log is the watcher's own
// ([gitx.StaleReview.Explain]) rather than a fixed sentence, because
// "stale" is three different conditions: the pull request was merged or
// closed, it is still open but has become one of the cannon's own (D-11),
// or the bead names no pull request at all. A bead an earlier run left open
// is settled too — the dedupe listing spans every run — and says so.
func (r *PRReview) settleStale(ctx context.Context, stale []gitx.StaleReview) {
	for _, st := range stale {
		is := st.Issue
		bead, _ := is.SafeBead(r.opts.Statuses)
		if bead.ID == "" || BeadStateSettled(bead.State) {
			r.forget(bead.ID)
			continue
		}
		if st.Reason == gitx.StalePreRun {
			// D-31: the pull request is still open and its review is still
			// owed; it is only not this run's to give. The bead — an earlier
			// run's, in flight — is not this run's to settle either, so
			// nothing is written to it whatever the policy, and it is said
			// once. When the pull request goes it is settled like any other.
			r.mu.Lock()
			said := r.preRun[bead.ID]
			if !said && len(r.preRun) < preRunSaidLimit {
				r.preRun[bead.ID] = true
			}
			r.mu.Unlock()
			if !said {
				r.warn(bead.ID, gitx.StaleSentence(st.Reason, prNumberOf(st, bead), st.ForeignRun)+"; leaving its review bead where it is", nil)
			}
			continue
		}
		r.mu.Lock()
		if e, tracked := r.tracked[bead.ID]; tracked {
			if e.settled {
				r.mu.Unlock()
				continue
			}
			// The bead as the poll just read it wins: the reviewer's copy
			// was made when the pull request was still open.
			e.bead.State = bead.State
			bead = e.bead
			e.settled = true
		}
		r.stats.Stale++
		r.mu.Unlock()

		// The number comes from bd's metadata, from the still-open pull
		// request the bead was matched to, or from what the reviewer
		// remembers adopting — in that order — so a bead whose metadata
		// has lost it is still named in the reason.
		msg := gitx.StaleSentence(st.Reason, prNumberOf(st, bead), st.ForeignRun)
		switch r.stale {
		case StaleReviewIgnore:
			r.warn(bead.ID, msg+"; leaving its review bead where it is", nil)
		case StaleReviewNeedsHuman:
			r.warn(bead.ID, msg+"; handing its review bead to a human", nil)
			r.park(ctx, bead.ID, msg)
		default:
			r.warn(bead.ID, msg+"; closing its review bead", nil)
			r.close(ctx, bead, msg+"; nothing left to review")
		}
	}
}

// prNumberOf is the pull request number of a stale review bead: what bd
// and the poll know, falling back to what the reviewer remembers adopting.
func prNumberOf(st gitx.StaleReview, bead domain.Bead) int {
	if n := st.Number(); n > 0 {
		return n
	}
	return bead.PRNumber
}

// forget drops a bead the reviewer has nothing more to do with.
func (r *PRReview) forget(id domain.BeadID) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.tracked[id]; ok {
		e.settled = true
	}
}

// close closes a review bead and publishes the state change.
//
// It does not go through the [StateMachine]: D-07's table has no edge from
// open to closed, because every bead of the run's own work reaches closed
// through delivery ([BeadEventDelivered]), and an external review bead is
// never delivered — the cannon does not merge somebody else's pull request,
// so walking it through approved and delivering to reach closed would
// record five transitions that did not happen. The write is the same `bd
// close --reason` the state machine would make (through the same
// [StateStore] method), and the transition is published here so `sc status`
// and the TUI see it. Parking a review bead does go through the machine:
// [BeadEventBlocked] is valid from every non-terminal state.
func (r *PRReview) close(ctx context.Context, bead domain.Bead, reason string) {
	r.mu.Lock()
	if e, ok := r.tracked[bead.ID]; ok {
		e.settled = true
	}
	r.mu.Unlock()

	if r.opts.Beads != nil {
		if _, err := r.opts.Beads.Close(ctx, bead.ID, reason); err != nil {
			r.warn(bead.ID, "could not close the review bead", err)
			return
		}
	}
	r.mu.Lock()
	r.stats.Closed++
	r.mu.Unlock()
	r.publish(events.BeadStateChanged{
		Meta:   r.meta(),
		Bead:   bead.ID,
		From:   bead.State,
		To:     domain.BeadStateClosed,
		Round:  PRReviewRound,
		Reason: reason,
	})
}

// park hands a review bead to a human (D-07's blocked edge, valid from
// every non-terminal state).
func (r *PRReview) park(ctx context.Context, id domain.BeadID, reason string) {
	r.mu.Lock()
	e, tracked := r.tracked[id]
	var from domain.BeadState
	if tracked {
		e.settled = true
		from = e.bead.State
	}
	r.mu.Unlock()

	switch {
	case r.opts.States != nil:
		if _, err := r.opts.States.Apply(ctx, StateRequest{
			Bead: id, From: from, Event: BeadEventBlocked, Reason: reason, Round: PRReviewRound,
		}); err != nil {
			r.warn(id, "could not hand the review bead to a human", err)
			return
		}
		r.mu.Lock()
		r.stats.Parked++
		r.mu.Unlock()
		return
	case r.opts.Beads != nil:
		if _, err := r.opts.Beads.SetStatus(ctx, id, domain.BeadStateNeedsHuman); err != nil {
			r.warn(id, "could not hand the review bead to a human", err)
			return
		}
	}
	r.mu.Lock()
	r.stats.Parked++
	r.mu.Unlock()
	r.publish(events.BeadStateChanged{
		Meta: r.meta(), Bead: id, From: from, To: domain.BeadStateNeedsHuman,
		Round: PRReviewRound, Reason: reason,
	})
}

// record posts a verdict on the review bead as a structured comment (D-06).
func (r *PRReview) record(ctx context.Context, id domain.BeadID, agent domain.AgentID, msg any) {
	if r.opts.Messages == nil {
		return
	}
	actor := "sc:" + agent.String()
	if agent == "" {
		actor = "sc:" + SourcePRReview
	}
	if _, err := r.opts.Messages.PostMessage(ctx, id, actor, msg); err != nil {
		r.warn(id, "could not record the verdict on the review bead", err)
	}
}

// runID is the run the events are stamped with.
func (r *PRReview) runID() domain.RunID {
	if r.opts.Host != nil {
		return r.opts.Host.Meta().Run
	}
	return r.opts.Run
}

// meta stamps an event with the run and the reviewer's clock.
func (r *PRReview) meta() events.Meta {
	if r.opts.Host != nil {
		return r.opts.Host.Meta()
	}
	return events.MetaAt(r.opts.Run, r.now())
}

// publish sends an event to the host, or to the bare sink.
func (r *PRReview) publish(ev events.Event) {
	switch {
	case r.opts.Host != nil:
		r.opts.Host.Publish(ev)
	case r.opts.Sink != nil:
		r.opts.Sink.Publish(ev)
	}
}

// warn publishes an events.Warning and logs it.
func (r *PRReview) warn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: r.meta(), Source: SourcePRReview, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	r.publish(w)
	r.log.Warn("external pull request review: "+message,
		slog.String("bead", bead.String()), slog.Any("err", err))
}

// Compile-time proof that the reviewer is an ordinary work source and an
// ordinary firing planner: the scheduler and the supervisor need no
// special case for external pull requests.
var (
	_ WorkSource    = (*PRReview)(nil)
	_ FiringPlanner = (*PRReview)(nil)
)
