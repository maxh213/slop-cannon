package gitx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// Cleanup and resume (P4-08). When a run finishes, the worktrees of closed
// beads are removed (their work has landed on the default branch or is on
// the remote behind a pull request) while the worktrees of every other
// bead are kept and their paths reported, so a human can look at what a
// failed, escalated or stopped bead left behind. On --resume, the
// worktrees of the run's beads are reattached (recreated from their
// branches when the directory vanished) and the git state of each branch
// is reconciled into the bead's workflow state: a delivery that landed
// before the crash closes the bead, a pull request that merged does the
// same through its gate, and a pull request closed without merging hands
// the bead to a human (D-20).

// CleanupAction says what [Manager.Cleanup] did with one bead's worktree.
type CleanupAction string

const (
	// CleanupRemoved means the worktree directory and registration are
	// gone.
	CleanupRemoved CleanupAction = "removed"
	// CleanupKept means the worktree stays for a human to inspect; the
	// entry's Reason says why and Path where.
	CleanupKept CleanupAction = "kept"
	// CleanupPruned means the worktree directory had already vanished and
	// only its stale registration was dropped.
	CleanupPruned CleanupAction = "pruned"
	// CleanupNone means the bead had no worktree; the entry exists because
	// its branch was looked at.
	CleanupNone CleanupAction = "none"
)

// CleanupOptions tunes [Manager.Cleanup].
type CleanupOptions struct {
	// KeepBranches leaves the branches of closed beads in place even when
	// they are fully merged into the default branch. By default a merged
	// branch of a closed bead is deleted; an unmerged one (a pull request
	// that was squash-merged on GitHub, or a bead closed by hand) is
	// always kept.
	KeepBranches bool
	// Logger receives one line per action; nil means slog.Default().
	Logger *slog.Logger
}

// CleanupEntry is the outcome for one bead or worktree.
type CleanupEntry struct {
	// BeadID is the bead the worktree belongs to.
	BeadID domain.BeadID
	// Path is the worktree path (PathFor), whether or not it still exists.
	Path string
	// Branch is the bead's branch; empty when neither a worktree nor a
	// branch was found.
	Branch string
	// State is the bead's workflow state as the caller reported it; empty
	// when the worktree belongs to a bead the caller did not list.
	State domain.BeadState
	// Action is what happened to the worktree.
	Action CleanupAction
	// Reason explains Action in one sentence.
	Reason string
	// BranchDeleted is true when the branch was deleted from the
	// repository.
	BranchDeleted bool
	// Err is the failure that stopped the intended action, or nil.
	Err error
}

// CleanupResult is what [Manager.Cleanup] did, one entry per bead or
// worktree, sorted by bead id.
type CleanupResult struct {
	// Default is the default branch merged branches were checked against.
	Default string
	// Entries lists every bead or worktree looked at.
	Entries []CleanupEntry
}

// Kept returns the entries whose worktree was kept.
func (r *CleanupResult) Kept() []CleanupEntry { return r.byAction(CleanupKept) }

// Removed returns the entries whose worktree was removed.
func (r *CleanupResult) Removed() []CleanupEntry { return r.byAction(CleanupRemoved) }

// KeptPaths returns the paths of the worktrees kept for a human, in bead
// id order.
func (r *CleanupResult) KeptPaths() []string {
	var out []string
	for _, e := range r.Kept() {
		out = append(out, e.Path)
	}
	return out
}

// Failed returns the entries whose action failed.
func (r *CleanupResult) Failed() []CleanupEntry {
	var out []CleanupEntry
	for _, e := range r.Entries {
		if e.Err != nil {
			out = append(out, e)
		}
	}
	return out
}

// Err joins the errors of every failed entry, or returns nil.
func (r *CleanupResult) Err() error {
	var errs []error
	for _, e := range r.Failed() {
		errs = append(errs, fmt.Errorf("%s: %w", e.BeadID, e.Err))
	}
	return errors.Join(errs...)
}

// Summary renders the outcome for the run's final report: a count line,
// then one line per kept worktree ("<id> (<state>): <path>") and one per
// failure.
func (r *CleanupResult) Summary() string {
	var b strings.Builder
	removed, kept, failed := len(r.Removed()), r.Kept(), r.Failed()
	fmt.Fprintf(&b, "removed %d %s", removed, plural(removed, "worktree", "worktrees"))
	if len(kept) > 0 {
		fmt.Fprintf(&b, ", kept %d:", len(kept))
		for _, e := range kept {
			state := string(e.State)
			if state == "" {
				state = "unknown"
			}
			fmt.Fprintf(&b, "\n  %s (%s): %s", e.BeadID, state, e.Path)
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n%d %s:", len(failed), plural(len(failed), "failure", "failures"))
		for _, e := range failed {
			fmt.Fprintf(&b, "\n  %s: %v", e.BeadID, e.Err)
		}
	}
	return b.String()
}

func (r *CleanupResult) byAction(a CleanupAction) []CleanupEntry {
	var out []CleanupEntry
	for _, e := range r.Entries {
		if e.Action == a {
			out = append(out, e)
		}
	}
	return out
}

// Cleanup is the run-finish pass over the manager's worktrees. beads are
// the run's beads with their final states. The worktree of every closed
// bead is removed (uncommitted leftovers included: the work has landed)
// and its branch deleted when it is fully merged into the default branch,
// unless [CleanupOptions.KeepBranches] is set. Every other worktree is
// kept, whether its bead failed, waits for a human, is escalated, or was
// still in flight when the run was stopped, so a resumed run can reattach
// it; a worktree of a bead the caller did not list is kept as well.
// Registrations whose directory vanished and empty leftover directories
// are pruned. The result names every kept path; see
// [CleanupResult.Summary]. Only a failure to inspect the repository is
// returned as an error; per-bead failures are in the entries and
// [CleanupResult.Err].
func (m *Manager) Cleanup(ctx context.Context, beadList []domain.Bead, opts CleanupOptions) (*CleanupResult, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	states := make(map[domain.BeadID]domain.Bead, len(beadList))
	for _, b := range beadList {
		if b.ID != "" {
			states[b.ID] = b
		}
	}
	worktrees, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	res := &CleanupResult{}
	if res.Default, err = m.repo.DefaultBranch(ctx); err != nil && !errors.Is(err, ErrNoDefaultBranch) {
		return nil, err
	}

	seen := map[domain.BeadID]bool{}
	for _, wt := range worktrees {
		seen[wt.BeadID] = true
		bead, known := states[wt.BeadID]
		e := CleanupEntry{BeadID: wt.BeadID, Path: wt.Path, Branch: wt.Branch, State: bead.State}
		switch {
		case wt.Prunable:
			e.Action = CleanupPruned
			e.Reason = "the worktree directory had vanished; its registration was dropped"
		case !known:
			e.Action = CleanupKept
			e.Reason = "the bead is not part of this run's final state"
		case bead.State != domain.BeadStateClosed:
			e.Action = CleanupKept
			e.Reason = fmt.Sprintf("the bead is %s", bead.State)
		default:
			if err := m.Remove(ctx, wt.BeadID, true); err != nil {
				e.Action = CleanupKept
				e.Reason = "the worktree could not be removed"
				e.Err = err
				break
			}
			e.Action = CleanupRemoved
			e.Reason = "the bead is closed"
			m.cleanupBranch(ctx, &e, bead, res.Default, opts.KeepBranches)
		}
		log.InfoContext(ctx, "cleanup: "+string(e.Action)+" worktree", "bead", string(e.BeadID), "path", e.Path, "state", string(e.State), "reason", e.Reason, "err", e.Err)
		res.Entries = append(res.Entries, e)
	}

	// Closed beads whose worktree is already gone may still own a merged
	// branch (Remove keeps branches).
	for id, bead := range states {
		if seen[id] || bead.State != domain.BeadStateClosed {
			continue
		}
		branch := BranchName(bead)
		exists, err := m.repo.BranchExists(ctx, branch)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		e := CleanupEntry{BeadID: id, Path: m.PathFor(id), Branch: branch, State: bead.State, Action: CleanupNone, Reason: "the bead is closed and has no worktree"}
		m.cleanupBranch(ctx, &e, bead, res.Default, opts.KeepBranches)
		log.InfoContext(ctx, "cleanup: closed bead without worktree", "bead", string(id), "branch", branch, "deleted", e.BranchDeleted, "err", e.Err)
		res.Entries = append(res.Entries, e)
	}

	if err := m.Prune(ctx); err != nil {
		return nil, err
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].BeadID < res.Entries[j].BeadID })
	return res, nil
}

// cleanupBranch deletes the closed bead's branch when it is fully merged
// into the default branch and keep is false, recording the outcome on e.
func (m *Manager) cleanupBranch(ctx context.Context, e *CleanupEntry, bead domain.Bead, def string, keep bool) {
	branch := BranchName(bead)
	if e.Branch == "" {
		e.Branch = branch
	}
	if keep {
		e.Reason += "; branch kept as requested"
		return
	}
	if def == "" {
		e.Reason += "; branch kept (no default branch to check it against)"
		return
	}
	exists, err := m.repo.BranchExists(ctx, branch)
	if err != nil {
		e.Err = err
		return
	}
	if !exists {
		return
	}
	merged, err := m.repo.isAncestor(ctx, "refs/heads/"+branch, def)
	if err != nil {
		e.Err = err
		return
	}
	if !merged {
		e.Reason += fmt.Sprintf("; branch %s kept (not merged into %s)", branch, def)
		return
	}
	if err := m.DeleteBranch(ctx, bead); err != nil {
		e.Err = err
		return
	}
	e.BranchDeleted = true
	e.Reason += fmt.Sprintf("; branch %s deleted (merged into %s)", branch, def)
}

// WorktreeStatus says whether a bead's worktree could be reattached on
// resume.
type WorktreeStatus string

const (
	// WorktreeAttached means the worktree exists and was reattached.
	WorktreeAttached WorktreeStatus = "attached"
	// WorktreeRecreated means the worktree was missing and has been created
	// again from the bead's branch (or from the default branch when the
	// branch was missing too).
	WorktreeRecreated WorktreeStatus = "recreated"
	// WorktreeMissing means the worktree is registered but its directory
	// is gone, or it was never created, and it was not recreated.
	WorktreeMissing WorktreeStatus = "missing"
)

// ResumeTracker is the part of *beads.Client the resume needs to write
// the reconciled state back (D-06, D-20). *beads.Client satisfies it.
type ResumeTracker interface {
	// SetStatus moves the bead to a workflow status.
	SetStatus(ctx context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error)
	// Update applies a patch (here: the sc_merge_sha metadata).
	Update(ctx context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error)
	// Close closes the bead with a reason.
	Close(ctx context.Context, id domain.BeadID, reason string) (beads.Issue, error)
}

var _ ResumeTracker = (*beads.Client)(nil)

// ResumeEntry is the reconciled state of one bead after a resume.
type ResumeEntry struct {
	// Bead is the bead as the caller passed it; Bead.State is the state
	// before reconciliation.
	Bead domain.Bead
	// Branch is the bead's branch name; BranchExists says whether the
	// repository still has it.
	Branch       string
	BranchExists bool
	// Head is the branch tip, empty when the branch is missing.
	Head string
	// Ahead is the number of commits on the branch that are not on the
	// default branch.
	Ahead int
	// Landed is true when the branch tip is an ancestor of the default
	// branch (local, or its remote-tracking branch after a fetch): the
	// work is on the default branch.
	Landed bool
	// Worktree is the reattached or recreated worktree; nil when missing.
	Worktree *Worktree
	// WorktreeStatus says how Worktree was obtained.
	WorktreeStatus WorktreeStatus
	// Dirty is true when the worktree has uncommitted or untracked
	// changes (the engineer was mid-edit).
	Dirty bool
	// Conflict is true when the worktree is mid-rebase with unmerged
	// paths, listed in ConflictFiles: the bead needs a conflict round.
	Conflict      bool
	ConflictFiles []string
	// PR is the pull request found for the branch (open first, then
	// merged), with PRState "OPEN" or "MERGED"; nil when none or when gh
	// was not consulted.
	PR      *PullRequest
	PRState string
	// State is the reconciled workflow state; it equals Bead.State when
	// nothing changed.
	State domain.BeadState
	// MergeSHA is the commit recorded as sc_merge_sha when the bead was
	// closed because its work landed.
	MergeSHA string
	// Reason explains the reconciliation in one sentence.
	Reason string
	// Applied is true when the state change was written to the tracker.
	Applied bool
	// Err is the failure that stopped reattaching or applying, or nil.
	Err error

	// prLookedUp is true when gh was asked about the branch and answered.
	prLookedUp bool
}

// Changed reports whether reconciliation moved the bead to another state.
func (e ResumeEntry) Changed() bool { return e.State != e.Bead.State }

// ResumeResult is the outcome of [Resumer.Resume].
type ResumeResult struct {
	// Default is the default branch, and Base the revision the "landed"
	// check used (the remote-tracking branch when there is a remote).
	Default string
	Base    string
	// Fetched is true when the remote was fetched before the checks.
	Fetched bool
	// Entries holds one entry per bead, in the caller's order.
	Entries []ResumeEntry
	// Orphans are worktrees under the manager's directory that belong to
	// no listed bead; they are left alone.
	Orphans []Worktree
	// Warnings lists problems that did not stop the resume (the fetch
	// failed, gh could not be used).
	Warnings []string
}

// Changed returns the entries whose state was reconciled to another one.
func (r *ResumeResult) Changed() []ResumeEntry {
	var out []ResumeEntry
	for _, e := range r.Entries {
		if e.Changed() {
			out = append(out, e)
		}
	}
	return out
}

// Failed returns the entries with an error.
func (r *ResumeResult) Failed() []ResumeEntry {
	var out []ResumeEntry
	for _, e := range r.Entries {
		if e.Err != nil {
			out = append(out, e)
		}
	}
	return out
}

// Err joins the errors of every failed entry, or returns nil.
func (r *ResumeResult) Err() error {
	var errs []error
	for _, e := range r.Failed() {
		errs = append(errs, fmt.Errorf("%s: %w", e.Bead.ID, e.Err))
	}
	return errors.Join(errs...)
}

// Summary renders one line per bead: "<id>: <state>" with "-> <state>"
// for a change, the worktree status and the reason.
func (r *ResumeResult) Summary() string {
	var b strings.Builder
	for i, e := range r.Entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s", e.Bead.ID, e.Bead.State)
		if e.Changed() {
			fmt.Fprintf(&b, " -> %s", e.State)
			if !e.Applied {
				b.WriteString(" (not applied)")
			}
		}
		fmt.Fprintf(&b, " [worktree %s", e.WorktreeStatus)
		if e.Conflict {
			b.WriteString(", conflict")
		} else if e.Dirty {
			b.WriteString(", dirty")
		}
		b.WriteString("]")
		if e.Reason != "" {
			fmt.Fprintf(&b, ": %s", e.Reason)
		}
		if e.Err != nil {
			fmt.Fprintf(&b, " (error: %v)", e.Err)
		}
	}
	for _, w := range r.Warnings {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "warning: %s", w)
	}
	return b.String()
}

// Resumer reattaches a run's worktrees and reconciles branch state into
// bead state after a crash or stop (--resume).
type Resumer struct {
	m       *Manager
	tracker ResumeTracker
	gh      *GH
	logger  *slog.Logger
	noGH    bool
}

// ResumeOption customises [NewResumer].
type ResumeOption func(*Resumer)

// WithResumeLogger sets the logger; the default is slog.Default().
func WithResumeLogger(l *slog.Logger) ResumeOption {
	return func(r *Resumer) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithResumeGH sets the gh runner used to look up pull requests; the
// default is NewGH(m.Repo()).
func WithResumeGH(gh *GH) ResumeOption {
	return func(r *Resumer) {
		if gh != nil {
			r.gh = gh
		}
	}
}

// WithoutGH skips pull request lookups entirely (a run with safety off
// never opens pull requests).
func WithoutGH() ResumeOption {
	return func(r *Resumer) { r.noGH = true }
}

// NewResumer returns a Resumer for the manager's run. tracker receives the
// reconciled states (normally the run's *beads.Client); nil makes
// [Resumer.Resume] a dry run that reports what it would change without
// writing anything.
func NewResumer(m *Manager, tracker ResumeTracker, opts ...ResumeOption) (*Resumer, error) {
	if m == nil {
		return nil, errors.New("gitx: NewResumer: nil manager")
	}
	r := &Resumer{m: m, tracker: tracker, logger: slog.Default()}
	for _, o := range opts {
		o(r)
	}
	if r.gh == nil {
		r.gh = NewGH(m.Repo())
	}
	return r, nil
}

// activeOnResume reports whether a bead in state still needs its worktree:
// an engineer, reviewer, or delivery will pick it up again.
func activeOnResume(state domain.BeadState) bool {
	switch state {
	case domain.BeadStateInProgress, domain.BeadStateInReview, domain.BeadStateChangesRequested,
		domain.BeadStateApproved, domain.BeadStateDelivering, domain.BeadStateEscalated:
		return true
	}
	return false
}

// Resume reattaches the worktree of every bead in beadList and reconciles
// its git state into its workflow state:
//
//   - A delivering bead whose branch tip has landed on the default branch
//     (locally or on the remote) is closed with sc_merge_sha set: the
//     crash hit after the fast-forward (D-20).
//   - A bead with a pull request, open or merged, stays delivering: the
//     caller re-runs the PR delivery (idempotent) so the gh:pr gate exists,
//     and the gate poller closes the bead once the merge is seen. bd
//     refuses to close a bead its gate still blocks, so a landed branch
//     behind a gate is left to the poller as well.
//   - A bead that recorded a pull request which is no longer open or
//     merged moves to needs_human: the PR was closed without merging.
//   - A delivering bead whose branch is gone moves to needs_human.
//   - Every other state is left alone; the entry reports the worktree
//     (dirty, mid-rebase with conflicts) for the orchestrator to act on.
//
// Worktrees of beads that still need one (in_progress through delivering,
// escalated) are recreated from their branch when the directory vanished.
// Changes are written through the tracker unless it is nil. Worktrees of
// beads not in beadList are reported as orphans and left alone. Only a
// failure to inspect the repository is an error; per-bead failures are in
// the entries and [ResumeResult.Err].
func (r *Resumer) Resume(ctx context.Context, beadList []domain.Bead) (*ResumeResult, error) {
	repo := r.m.Repo()
	res := &ResumeResult{}

	def, err := repo.DefaultBranch(ctx)
	if err != nil {
		return nil, err
	}
	res.Default = def
	remote, err := repo.DefaultRemote(ctx)
	if err != nil && !errors.Is(err, ErrNoRemote) {
		return nil, err
	}
	if remote != "" {
		if err := repo.Fetch(ctx); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("fetch from %s failed, checking against local state only: %v", remote, err))
		} else {
			res.Fetched = true
		}
	}
	// The "landed" check looks at both the local default branch and its
	// remote-tracking branch, whichever exist; Base names the preferred.
	var bases []string
	if ok, err := repo.BranchExists(ctx, def); err != nil {
		return nil, err
	} else if ok {
		bases = append(bases, "refs/heads/"+def)
	}
	if remote != "" {
		tracking := "refs/remotes/" + remote + "/" + def
		if _, err := repo.RevParse(ctx, tracking); err == nil {
			bases = append([]string{tracking}, bases...)
		} else if !errors.Is(err, ErrUnknownRevision) {
			return nil, err
		}
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("%w: %s has no commits", ErrNoDefaultBranch, def)
	}
	res.Base = strings.TrimPrefix(strings.TrimPrefix(bases[0], "refs/heads/"), "refs/remotes/")

	worktrees, err := r.m.List(ctx)
	if err != nil {
		return nil, err
	}
	listed := map[domain.BeadID]bool{}
	for _, b := range beadList {
		listed[b.ID] = true
	}
	for _, wt := range worktrees {
		if !listed[wt.BeadID] {
			res.Orphans = append(res.Orphans, wt)
		}
	}

	ghUsable := !r.noGH && remote != ""
	for _, bead := range beadList {
		e := r.inspect(ctx, bead, bases, &ghUsable, res)
		r.reconcile(&e, def)
		r.apply(ctx, &e, def)
		r.logger.InfoContext(ctx, "resume: reconciled bead",
			"bead", string(bead.ID), "before", string(bead.State), "after", string(e.State),
			"worktree", string(e.WorktreeStatus), "landed", e.Landed, "conflict", e.Conflict, "reason", e.Reason, "err", e.Err)
		res.Entries = append(res.Entries, e)
	}
	return res, nil
}

// inspect gathers the git facts of one bead: branch, tip, landed, the
// worktree (reattached or recreated), and the pull request when gh can
// be used.
func (r *Resumer) inspect(ctx context.Context, bead domain.Bead, bases []string, ghUsable *bool, res *ResumeResult) ResumeEntry {
	repo := r.m.Repo()
	e := ResumeEntry{Bead: bead, State: bead.State, Branch: BranchName(bead), WorktreeStatus: WorktreeMissing}
	if bead.ID == "" {
		e.Err = errors.New("gitx: Resume: bead has no id")
		return e
	}

	// Worktree: reattach, or recreate for a bead that still needs one.
	wt, err := r.m.Reattach(ctx, bead.ID)
	switch {
	case err == nil:
		e.Worktree, e.WorktreeStatus = wt, WorktreeAttached
	case !errors.Is(err, ErrWorktreeMissing):
		e.Err = err
		return e
	}

	// Branch facts.
	exists, err := repo.BranchExists(ctx, e.Branch)
	if err != nil {
		e.Err = err
		return e
	}
	e.BranchExists = exists
	if exists {
		if e.Head, err = repo.RevParse(ctx, "refs/heads/"+e.Branch); err != nil {
			e.Err = err
			return e
		}
		for _, base := range bases {
			landed, err := repo.isAncestor(ctx, e.Head, base)
			if err != nil {
				e.Err = err
				return e
			}
			if landed {
				e.Landed = true
				break
			}
		}
		out, err := repo.Git(ctx, "rev-list", "--count", e.Head, "^"+bases[0])
		if err != nil {
			e.Err = err
			return e
		}
		if e.Ahead, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
			e.Err = fmt.Errorf("gitx: unexpected rev-list output %q", out)
			return e
		}
	}

	// Pull request, for beads that went through the PR path.
	if *ghUsable && (bead.State == domain.BeadStateDelivering || bead.PRURL != "" || bead.PRNumber != 0) && exists {
		pr, state, err := r.gh.FindPR(ctx, e.Branch)
		if reason, ok := ghUnavailable(err); ok {
			*ghUsable = false
			res.Warnings = append(res.Warnings, "pull requests were not checked: "+reason)
		} else if err != nil {
			e.Err = err
			return e
		} else {
			e.prLookedUp = true
			if pr != nil {
				e.PR, e.PRState = pr, state
			}
		}
	}

	// Recreate a vanished worktree when the bead will be worked on again.
	landedForGood := e.Landed && bead.State == domain.BeadStateDelivering
	if e.WorktreeStatus == WorktreeMissing && activeOnResume(bead.State) && !landedForGood && (exists || bead.State == domain.BeadStateInProgress) {
		wt, err := r.m.Create(ctx, bead)
		if err != nil {
			e.Err = fmt.Errorf("recreating the worktree: %w", err)
		} else {
			e.Worktree, e.WorktreeStatus = wt, WorktreeRecreated
			if !exists {
				e.BranchExists = true
				e.Head = wt.Head
			}
		}
	}

	// Worktree condition.
	if e.Worktree != nil {
		inner, err := Open(ctx, e.Worktree.Path, WithEnv(repo.env), WithTimeout(repo.timeout))
		if err != nil {
			e.Err = err
			return e
		}
		if inner.RebaseInProgress() {
			files, err := inner.ConflictedPaths(ctx)
			if err != nil {
				e.Err = err
				return e
			}
			e.Conflict, e.ConflictFiles = true, files
		}
		clean, err := inner.IsClean(ctx)
		if err != nil {
			e.Err = err
			return e
		}
		e.Dirty = !clean
	}
	return e
}

// reconcile decides the bead's state from the facts inspect gathered.
func (r *Resumer) reconcile(e *ResumeEntry, def string) {
	bead := e.Bead
	if e.Err != nil && e.Head == "" {
		return
	}
	switch {
	case bead.State == domain.BeadStateClosed:
		e.Reason = "already closed"
	case e.PRState == "MERGED":
		e.State = domain.BeadStateDelivering
		e.Reason = fmt.Sprintf("PR #%d merged; re-run the PR delivery so its gate resolves and closes the bead", e.PR.Number)
	case e.PRState == "OPEN":
		e.State = domain.BeadStateDelivering
		e.Reason = fmt.Sprintf("PR #%d is open; re-run the PR delivery to confirm its gate", e.PR.Number)
	case bead.State == domain.BeadStateDelivering && e.Landed:
		e.State, e.MergeSHA = domain.BeadStateClosed, e.Head
		e.Reason = fmt.Sprintf("the branch landed on %s before the crash", def)
	case (bead.PRURL != "" || bead.PRNumber != 0) && e.PR == nil && e.prLookedUp:
		e.State = domain.BeadStateNeedsHuman
		e.Reason = fmt.Sprintf("the recorded pull request %s is no longer open or merged", bead.PRURL)
	case bead.State == domain.BeadStateDelivering && !e.BranchExists:
		e.State = domain.BeadStateNeedsHuman
		e.Reason = fmt.Sprintf("branch %s is gone and nothing landed on %s", e.Branch, def)
	case bead.State == domain.BeadStateDelivering:
		e.Reason = "the delivery did not complete; deliver again"
	case e.Conflict:
		e.Reason = fmt.Sprintf("the worktree is mid-rebase with conflicts in %s", strings.Join(e.ConflictFiles, ", "))
	case e.WorktreeStatus == WorktreeRecreated:
		e.Reason = "the worktree was recreated from its branch"
	case e.WorktreeStatus == WorktreeMissing && activeOnResume(bead.State):
		e.Reason = "the worktree is missing"
	default:
		e.Reason = "unchanged"
	}
}

// apply writes the reconciled state through the tracker.
func (r *Resumer) apply(ctx context.Context, e *ResumeEntry, def string) {
	if !e.Changed() || r.tracker == nil || e.Err != nil {
		return
	}
	id := e.Bead.ID
	var err error
	switch e.State {
	case domain.BeadStateClosed:
		if _, err = r.tracker.Update(ctx, id, beads.Patch{Metadata: beads.MergeMetadata(e.MergeSHA)}); err == nil {
			_, err = r.tracker.Close(ctx, id, fmt.Sprintf("merged into %s as %s (found on resume)", def, shortSHA(e.MergeSHA)))
		}
	default:
		_, err = r.tracker.SetStatus(ctx, id, e.State)
	}
	if errors.Is(err, beads.ErrBlockedOpen) && e.State == domain.BeadStateClosed {
		// A gh:pr gate still blocks the bead (safety on, merged by hand):
		// the gate poller closes it once bd sees the merge (D-20).
		e.State = e.Bead.State
		e.MergeSHA = ""
		e.Reason += "; a gate still blocks it, so the gate poller closes it"
		return
	}
	if err != nil {
		e.Err = fmt.Errorf("applying %s: %w", e.State, err)
		return
	}
	e.Applied = true
}
