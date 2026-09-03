package gitx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
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
//
// Both passes address the branch a bead's work is actually on rather than
// the one [BranchName] derives from its (mutable) title: git's own
// worktree registration names it, and it outlives the directory. See
// resolveBranch; P4-12 records the branch in the bead's metadata and makes
// the search unnecessary.

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
	// Out receives [CleanupResult.Summary] followed by a newline once the
	// pass is over, so the run's final report names the worktrees that were
	// kept for a human and where they are. nil prints nothing; the result is
	// returned either way.
	Out io.Writer
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
	// Default is the default branch's short name; empty when the
	// repository has none.
	Default string
	// Bases are the revisions a closed bead's branch was checked against,
	// as short names: it counts as merged when it is an ancestor of any of
	// them. The remote-tracking branch of the default remote comes first
	// (a delivery pushes there, and a merged pull request shows up there
	// once fetched), then the local Default. Empty when neither ref
	// exists, in which case no branch is deleted.
	Bases []string
	// Entries lists every bead or worktree looked at.
	Entries []CleanupEntry
}

// Kept returns the entries whose worktree was kept.
func (r *CleanupResult) Kept() []CleanupEntry { return r.byAction(CleanupKept) }

// Removed returns the entries whose worktree was removed.
func (r *CleanupResult) Removed() []CleanupEntry { return r.byAction(CleanupRemoved) }

// Pruned returns the entries whose worktree directory had already vanished
// and whose stale registration was dropped.
func (r *CleanupResult) Pruned() []CleanupEntry { return r.byAction(CleanupPruned) }

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
	removed, pruned, kept, failed := len(r.Removed()), len(r.Pruned()), r.Kept(), r.Failed()
	fmt.Fprintf(&b, "removed %d %s", removed, plural(removed, "worktree", "worktrees"))
	if pruned > 0 {
		fmt.Fprintf(&b, ", pruned %d stale %s", pruned, plural(pruned, "registration", "registrations"))
	}
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
// are pruned, the run's own only (D-23: worktrees the user keeps outside
// the run directory keep their registrations); a closed bead among them
// still has its merged branch deleted. The result names every kept path,
// and is printed to [CleanupOptions.Out] when one is set; see
// [CleanupResult.Summary]. Only
// a failure to inspect the repository as a whole (listing or pruning its
// worktrees, reading the default branch) is returned as an error; per-bead
// failures are in the entries and [CleanupResult.Err].
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
	// Drop stale registrations before any branch is looked at: git refuses
	// to delete the branch of a worktree it still has on file, so a closed
	// bead whose directory vanished would otherwise keep its merged branch
	// forever. The listing above still remembers which ones were prunable.
	if err := m.pruneRun(ctx); err != nil {
		return nil, err
	}
	res := &CleanupResult{}
	if res.Default, err = m.repo.DefaultBranch(ctx); err != nil && !errors.Is(err, ErrNoDefaultBranch) {
		return nil, err
	}
	baseRefs, baseNames, err := m.repo.defaultBases(ctx, res.Default)
	if err != nil {
		return nil, err
	}
	res.Bases = baseNames

	seen := map[domain.BeadID]bool{}
	for _, wt := range worktrees {
		seen[wt.BeadID] = true
		bead, known := states[wt.BeadID]
		e := CleanupEntry{BeadID: wt.BeadID, Path: wt.Path, Branch: wt.Branch, State: bead.State}
		switch {
		case wt.Prunable:
			e.Action = CleanupPruned
			e.Reason = "the worktree directory had vanished; its registration was dropped"
			if known && bead.State == domain.BeadStateClosed {
				m.cleanupBranch(ctx, &e, bead, baseRefs, baseNames, opts.KeepBranches)
			}
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
			m.cleanupBranch(ctx, &e, bead, baseRefs, baseNames, opts.KeepBranches)
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
		branch, exists, err := m.resolveBranch(ctx, bead, "")
		if err != nil {
			// One unreadable ref must not abandon the rest of the pass.
			res.Entries = append(res.Entries, CleanupEntry{
				BeadID: id, Path: m.PathFor(id), Branch: branch, State: bead.State,
				Action: CleanupNone, Reason: "the bead's branch could not be inspected", Err: err,
			})
			continue
		}
		if !exists {
			continue
		}
		e := CleanupEntry{BeadID: id, Path: m.PathFor(id), Branch: branch, State: bead.State, Action: CleanupNone, Reason: "the bead is closed and has no worktree"}
		m.cleanupBranch(ctx, &e, bead, baseRefs, baseNames, opts.KeepBranches)
		log.InfoContext(ctx, "cleanup: closed bead without worktree", "bead", string(id), "branch", branch, "deleted", e.BranchDeleted, "err", e.Err)
		res.Entries = append(res.Entries, e)
	}

	if err := m.pruneRun(ctx); err != nil {
		return nil, err
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].BeadID < res.Entries[j].BeadID })
	if opts.Out != nil {
		_, _ = fmt.Fprintln(opts.Out, res.Summary())
	}
	return res, nil
}

// cleanupBranch deletes the closed bead's branch when it is fully merged
// into one of bases and keep is false, recording the outcome on e. names
// are the short names of bases, used in the reason. The branch is the one
// the worktree actually held when e.Branch says so, and resolveBranch's
// answer otherwise: a bead retitled mid-run no longer names its own
// branch.
func (m *Manager) cleanupBranch(ctx context.Context, e *CleanupEntry, bead domain.Bead, bases, names []string, keep bool) {
	if e.Branch == "" {
		branch, _, err := m.resolveBranch(ctx, bead, "")
		if err != nil {
			e.Err = err
			return
		}
		e.Branch = branch
	}
	branch := e.Branch
	if keep {
		e.Reason += "; branch kept as requested"
		return
	}
	if len(bases) == 0 {
		e.Reason += "; branch kept (no default branch to check it against)"
		return
	}
	exists, err := m.repo.BranchExists(ctx, branch)
	if err != nil {
		e.Err = err
		return
	}
	if !exists {
		e.Reason += fmt.Sprintf("; branch %s is already gone", branch)
		return
	}
	mergedInto := ""
	for i, base := range bases {
		merged, err := m.repo.isAncestor(ctx, "refs/heads/"+branch, base)
		if err != nil {
			e.Err = err
			return
		}
		if merged {
			mergedInto = names[i]
			break
		}
	}
	if mergedInto == "" {
		e.Reason += fmt.Sprintf("; branch %s kept (not merged into %s)", branch, strings.Join(names, " or "))
		return
	}
	if _, err := m.repo.Git(ctx, "branch", "-D", "--quiet", branch); err != nil {
		e.Err = err
		return
	}
	e.BranchDeleted = true
	e.Reason += fmt.Sprintf("; branch %s deleted (merged into %s)", branch, mergedInto)
}

// resolveBranch finds the branch a bead's work is on and whether the
// repository still has it. BranchName(bead) slugs the bead's title, which
// the product owner can change mid-run, so on its own it is a guess
// (P4-12): the branch git records for the bead's worktree is the truth,
// even when only a stale registration of a vanished directory is left, and
// wtBranch carries it when there is one. Without it the "sc/<bead-id>"
// namespace is searched for the single branch the bead can own. The guess
// is returned with exists false when nothing is found or the namespace
// holds several candidates, so callers always have a name to report.
func (m *Manager) resolveBranch(ctx context.Context, bead domain.Bead, wtBranch string) (branch string, exists bool, err error) {
	if wtBranch != "" {
		exists, err = m.repo.BranchExists(ctx, wtBranch)
		return wtBranch, exists, err
	}
	guess := BranchName(bead)
	exists, err = m.repo.BranchExists(ctx, guess)
	if err != nil || exists {
		return guess, exists, err
	}
	found, err := m.beadBranches(ctx, bead.ID)
	if err != nil {
		return guess, false, err
	}
	if len(found) == 1 {
		return found[0], true, nil
	}
	return guess, false, nil
}

// beadBranches lists the existing branches [BranchName] can produce for a
// bead id: "sc/<id>" and "sc/<id>-<slug>" for any title the bead ever
// carried. Another bead's branch never matches, because the id is followed
// by a dash or by nothing at all.
func (m *Manager) beadBranches(ctx context.Context, id domain.BeadID) ([]string, error) {
	stem := "refs/heads/" + BranchPrefix + logging.SafeFileName(string(id))
	out, err := m.repo.Git(ctx, "for-each-ref", "--format=%(refname:short)", stem, stem+"-*")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// beadOnBranch returns a copy of bead for which [BranchName] reports
// branch, so [Manager.Create] checks out the branch the work is actually
// on instead of cutting a new one from the default branch. It reports
// false when no title yields branch; the caller must then refuse rather
// than orphan committed work. P4-12 removes the need for it by recording
// the branch in the bead's metadata when the worktree is created.
func beadOnBranch(bead domain.Bead, branch string) (domain.Bead, bool) {
	suffix, ok := strings.CutPrefix(branch, BranchPrefix+logging.SafeFileName(string(bead.ID)))
	if !ok {
		return bead, false
	}
	out := bead
	out.Title = strings.TrimPrefix(suffix, "-")
	if BranchName(out) != branch {
		return bead, false
	}
	return out, true
}

// pruneRun drops the stale registrations of the run's own worktrees and
// deletes the empty husks under the manager's directory. Unlike
// [Manager.Prune] it never runs a repository-wide "git worktree prune":
// "git worktree remove" addresses one path, so the registrations of
// worktrees the user keeps outside the run directory are left alone (D-23:
// a run mutates nothing of the user's beyond its own).
func (m *Manager) pruneRun(ctx context.Context) error {
	all, err := m.listAll(ctx)
	if err != nil {
		return err
	}
	for _, wt := range all {
		if !wt.Prunable || samePath(m.dir, wt.Path) || !isWithin(m.dir, wt.Path) {
			continue
		}
		// Twice: once for a dirty tree, once for a locked one.
		if _, err := m.repo.Git(ctx, "worktree", "remove", "--force", "--force", wt.Path); err != nil {
			return err
		}
	}
	return m.removeHusks()
}

// defaultBases returns the revisions a branch counts as landed on when it
// is an ancestor of any of them: the remote-tracking branch of def on the
// default remote first (a delivery pushes there, and a pull request
// merged on the forge shows up there once fetched), then the local def.
// The first result holds full refs for git, the second their short names
// for reporting; both are empty when def is empty or neither ref exists.
func (r *Repo) defaultBases(ctx context.Context, def string) (refs, names []string, err error) {
	if def == "" {
		return nil, nil, nil
	}
	remote, err := r.DefaultRemote(ctx)
	if err != nil && !errors.Is(err, ErrNoRemote) {
		return nil, nil, err
	}
	if remote != "" {
		tracking := "refs/remotes/" + remote + "/" + def
		if _, err := r.RevParse(ctx, tracking); err == nil {
			refs = append(refs, tracking)
			names = append(names, remote+"/"+def)
		} else if !errors.Is(err, ErrUnknownRevision) {
			return nil, nil, err
		}
	}
	switch ok, err := r.BranchExists(ctx, def); {
	case err != nil:
		return nil, nil, err
	case ok:
		refs = append(refs, "refs/heads/"+def)
		names = append(names, def)
	}
	return refs, names, nil
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

	// prLookedUp is true when gh was asked about the pull request and
	// answered.
	prLookedUp bool
	// recreatedFresh is true when the recreated worktree was cut from the
	// default branch because the bead had no branch yet, rather than
	// checked out from one that already held work.
	recreatedFresh bool
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
//   - A delivering bead whose branch is gone and that has no pull request
//     left to speak for it moves to needs_human.
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
	bases, baseNames, err := repo.defaultBases(ctx, def)
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("%w: %s has no commits", ErrNoDefaultBranch, def)
	}
	res.Base = baseNames[0]

	worktrees, err := r.m.List(ctx)
	if err != nil {
		return nil, err
	}
	listed := map[domain.BeadID]bool{}
	for _, b := range beadList {
		listed[b.ID] = true
	}
	// The registrations git still holds, prunable ones included: they name
	// the branch a bead's worktree was created on even when its directory
	// is gone, which the bead's (mutable) title no longer does.
	regs := make(map[domain.BeadID]Worktree, len(worktrees))
	for _, wt := range worktrees {
		regs[wt.BeadID] = wt
		if !listed[wt.BeadID] {
			res.Orphans = append(res.Orphans, wt)
		}
	}

	ghUsable := !r.noGH && remote != ""
	for _, bead := range beadList {
		e := r.inspect(ctx, bead, bases, regs, &ghUsable, res)
		r.reconcile(&e, def)
		r.apply(ctx, &e, def)
		r.logger.InfoContext(ctx, "resume: reconciled bead",
			"bead", string(bead.ID), "before", string(bead.State), "after", string(e.State),
			"worktree", string(e.WorktreeStatus), "landed", e.Landed, "conflict", e.Conflict, "reason", e.Reason, "err", e.Err)
		res.Entries = append(res.Entries, e)
	}
	return res, nil
}

// findPR asks gh about the bead's pull request. The number the bead
// already records is used when there is one: a head branch is derived from
// the bead's title and stops naming the pull request the moment the title
// changes (P4-12), while a number never moves. Otherwise the branch is
// queried. state is "OPEN" or "MERGED", and the pull request is nil when
// there is none in either state.
func (r *Resumer) findPR(ctx context.Context, bead domain.Bead, branch string) (*PullRequest, string, error) {
	number := bead.PRNumber
	if number <= 0 {
		// beads.PRMetadata leaves a zero number out, so an older bead can
		// carry the url alone.
		number = prNumberFromURL(bead.PRURL)
	}
	if number > 0 {
		return r.gh.prByNumber(ctx, number)
	}
	return r.gh.FindPR(ctx, branch)
}

// prNumberFromURL reads the number out of a pull request URL
// (".../pull/<n>", with any fragment or query ignored). It returns 0 when
// the URL is empty or shaped differently.
func prNumberFromURL(url string) int {
	_, tail, ok := strings.Cut(url, "/pull/")
	if !ok {
		return 0
	}
	tail, _, _ = strings.Cut(tail, "/")
	tail, _, _ = strings.Cut(tail, "#")
	tail, _, _ = strings.Cut(tail, "?")
	n, err := strconv.Atoi(tail)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// prByNumber looks one pull request up by number with
// "gh pr view <n> --json ...", the query that does not depend on a branch
// name. state is "OPEN" or "MERGED"; a pull request closed without merging
// is reported as nil, like [GH.FindPR], so a retry opens a fresh one.
func (g *GH) prByNumber(ctx context.Context, number int) (*PullRequest, string, error) {
	if number <= 0 {
		return nil, "", fmt.Errorf("gitx: prByNumber: %d is not a pull request number", number)
	}
	out, err := g.run(ctx, "", "pr", "view", strconv.Itoa(number), "--json", "number,url,state,title,headRefName")
	if err != nil {
		return nil, "", err
	}
	var ref struct {
		ghPRRef
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ref); err != nil {
		return nil, "", fmt.Errorf("gitx: gh pr view %d: unexpected output %q: %w", number, out, err)
	}
	state := strings.ToUpper(strings.TrimSpace(ref.State))
	if state != "OPEN" && state != "MERGED" {
		return nil, "", nil
	}
	return &PullRequest{Number: ref.Number, URL: ref.URL, Title: ref.Title, HeadRef: ref.HeadRefName}, state, nil
}

// inspect gathers the git facts of one bead: branch, tip, landed, the
// worktree (reattached or recreated), and the pull request when gh can
// be used.
func (r *Resumer) inspect(ctx context.Context, bead domain.Bead, bases []string, regs map[domain.BeadID]Worktree, ghUsable *bool, res *ResumeResult) ResumeEntry {
	repo := r.m.Repo()
	e := ResumeEntry{Bead: bead, State: bead.State, Branch: BranchName(bead), WorktreeStatus: WorktreeMissing}
	if bead.ID == "" {
		e.Err = errors.New("gitx: Resume: bead has no id")
		return e
	}

	// Worktree: reattach, or recreate for a bead that still needs one.
	// git's registration is the authority on which branch the work is on,
	// and it survives the directory: a bead retitled mid-run must not be
	// reconciled against the branch its new title would name (P4-12).
	wtBranch := regs[bead.ID].Branch
	wt, err := r.m.Reattach(ctx, bead.ID)
	switch {
	case err == nil:
		e.Worktree, e.WorktreeStatus = wt, WorktreeAttached
		if wt.Branch != "" {
			wtBranch = wt.Branch
		}
	case !errors.Is(err, ErrWorktreeMissing):
		e.Err = err
		return e
	}

	// Branch facts.
	branch, exists, err := r.m.resolveBranch(ctx, bead, wtBranch)
	if err != nil {
		e.Err = err
		return e
	}
	e.Branch, e.BranchExists = branch, exists
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

	// Pull request, for beads that went through the PR path. The local
	// branch may well be gone (deleted after the merge, or by hand) while
	// the pull request it was pushed to is still open or already merged, so
	// gh is asked either way.
	if *ghUsable && (bead.State == domain.BeadStateDelivering || bead.PRURL != "" || bead.PRNumber != 0) {
		pr, state, err := r.findPR(ctx, bead, e.Branch)
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

	// Recreate a vanished worktree when the bead will be worked on again,
	// on the branch that holds its work: Manager.Create derives the branch
	// from the bead's title, so a retitled bead would otherwise get a fresh
	// branch cut from the default branch and its commits would be orphaned
	// (P4-12). When no title can name that branch the worktree is left
	// missing and the entry says why.
	landedForGood := e.Landed && bead.State == domain.BeadStateDelivering
	if e.WorktreeStatus == WorktreeMissing && activeOnResume(bead.State) && !landedForGood && (exists || bead.State == domain.BeadStateInProgress) {
		target, ok := beadOnBranch(bead, e.Branch)
		switch {
		case !ok:
			e.Err = fmt.Errorf("recreating the worktree: the work is on branch %s, which the bead's title no longer names (P4-12)", e.Branch)
		default:
			wt, err := r.m.Create(ctx, target)
			if err != nil {
				e.Err = fmt.Errorf("recreating the worktree: %w", err)
			} else {
				e.Worktree, e.WorktreeStatus = wt, WorktreeRecreated
				e.recreatedFresh = !exists
				if !exists {
					e.BranchExists = true
					e.Head = wt.Head
				}
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
	if e.Err != nil {
		// Half the facts are missing, so any state this could name would be
		// a guess. The entry carries the error for the caller.
		e.Reason = "the bead could not be inspected, so its state is left alone"
		return
	}
	switch {
	case bead.State == domain.BeadStateClosed:
		e.Reason = "already closed"
	case bead.State == domain.BeadStateDelivering && e.PRState == "MERGED":
		e.Reason = fmt.Sprintf("PR #%d merged; re-run the PR delivery so its gate resolves and closes the bead", e.PR.Number)
	case bead.State == domain.BeadStateDelivering && e.PRState == "OPEN":
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
	case e.WorktreeStatus == WorktreeRecreated && e.recreatedFresh:
		e.Reason = fmt.Sprintf("the bead had no branch yet, so the worktree was created from %s", def)
	case e.WorktreeStatus == WorktreeRecreated:
		e.Reason = fmt.Sprintf("the worktree was recreated from branch %s", e.Branch)
	case e.WorktreeStatus == WorktreeMissing && activeOnResume(bead.State):
		e.Reason = "the worktree is missing"
	case e.PRState != "":
		// A pull request recorded on a bead that is not delivering (a
		// review round after a delivery attempt) is reported, never acted
		// on: the bead stays in the loop it is in.
		e.Reason = fmt.Sprintf("PR #%d is %s; the bead stays %s", e.PR.Number, strings.ToLower(e.PRState), bead.State)
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
