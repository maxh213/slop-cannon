package gitx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
)

// DefaultCheckTimeout bounds the repository's check command when
// [MergeOptions.CheckTimeout] is not set.
const DefaultCheckTimeout = 30 * time.Minute

// MaxCheckOutput is how much of the check command's output (the tail) a
// [CheckResult] and a [DeliveryError] keep.
const MaxCheckOutput = 32 << 10

// rebaseConfig are the "-c" overrides every delivery rebase runs with, so
// that the user's own git configuration cannot change what a delivery does:
//   - commit.gpgsign=false: an agent's commits are never signed (P4-03) and
//     a signing prompt would hang the run.
//   - rebase.rebaseMerges=false: merge commits inside the bead's branch are
//     flattened, so the fast-forward can never put a merge commit on the
//     default branch (D-10). git preserves them when the user set
//     rebase.rebaseMerges, which is exactly the case this override closes.
//   - rebase.autoStash=false: uncommitted work in the engineer's worktree
//     stops the delivery instead of being stashed behind their back.
//   - rebase.updateRefs=false: only the bead's branch is rewritten, never
//     other branches of the user that happen to point into the range.
var rebaseConfig = []string{
	"-c", "commit.gpgsign=false",
	"-c", "rebase.rebaseMerges=false",
	"-c", "rebase.autoStash=false",
	"-c", "rebase.updateRefs=false",
}

// mergeConfig are the "-c" overrides the fast-forward runs with:
// merge.autoStash=false leaves a human's uncommitted changes in the main
// working tree alone (the merge is refused instead), and merge.ff=only
// together with the --ff-only flag keeps a merge commit impossible whatever
// the repository configures.
var mergeConfig = []string{
	"-c", "commit.gpgsign=false",
	"-c", "merge.autoStash=false",
	"-c", "merge.ff=only",
}

// gitArgs prepends the "-c" overrides in config to args.
func gitArgs(config []string, args ...string) []string {
	full := make([]string, 0, len(config)+len(args))
	full = append(full, config...)
	return append(full, args...)
}

// DeliveryStage names the step of a delivery that failed.
type DeliveryStage string

// Stages of a safety-off delivery, in the order they run.
const (
	// StagePrepare resolves the bead's worktree, branch, default branch
	// and remote before anything is touched.
	StagePrepare DeliveryStage = "prepare"
	// StageFetch syncs the local default branch with its remote and
	// resolves the base the branch is rebased onto.
	StageFetch DeliveryStage = "fetch"
	// StageRebase rebases the bead's branch onto the default branch.
	StageRebase DeliveryStage = "rebase"
	// StageCheck runs the repository's check command in the worktree.
	StageCheck DeliveryStage = "check"
	// StageMerge fast-forwards the default branch to the rebased branch.
	StageMerge DeliveryStage = "merge"
	// StagePush pushes the default branch to the remote.
	StagePush DeliveryStage = "push"
	// StageClose records the merge on the bead and closes it (D-20).
	StageClose DeliveryStage = "close"
)

// Delivery sentinel errors. They match with errors.Is through a
// [*DeliveryError].
var (
	// ErrRebaseConflict means the rebase stopped on conflicts; the worktree
	// is left in that state for the engineer (D-10 conflict round).
	ErrRebaseConflict = errors.New("gitx: rebase stopped on conflicts")
	// ErrCheckFailed means the repository's check command exited non-zero.
	ErrCheckFailed = errors.New("gitx: check command failed")
	// ErrDefaultBranchDiverged means the local default branch and its
	// remote-tracking branch each have commits the other lacks, so neither
	// a fast-forward nor a push can succeed until a human reconciles them.
	ErrDefaultBranchDiverged = errors.New("gitx: local default branch has diverged from its remote")
	// ErrNotFastForward means the default branch could not be fast-forwarded
	// to the bead's branch (it moved while the delivery ran).
	ErrNotFastForward = errors.New("gitx: default branch cannot be fast-forwarded")
	// ErrWrongBranch means the bead's worktree is not on the bead's branch.
	ErrWrongBranch = errors.New("gitx: worktree is not on the bead's branch")
	// ErrNonLinearBranch means the rebased branch still carries a merge
	// commit, so fast-forwarding the default branch onto it would put one
	// there. The delivery stops instead: the default branch never receives
	// a merge commit (D-10).
	ErrNonLinearBranch = errors.New("gitx: rebased branch still contains a merge commit")
)

// DeliveryError reports which stage of a delivery failed and what the
// failing command printed. Unwrap exposes the underlying error, so
// errors.Is works for the sentinels above, for [*GitError] causes, for
// proc.ErrTimeout, and for beads errors such as beads.ErrBlockedOpen.
type DeliveryError struct {
	// Stage is the step that failed.
	Stage DeliveryStage
	// Output is what the failing command printed: git's stderr, the tail
	// of the check command's combined output, or bd's message.
	Output string
	// Conflict is set when the rebase stopped on conflicts. The worktree is
	// left mid-rebase with conflict markers so the engineer can resolve
	// them; Files names the conflicted paths.
	Conflict bool
	// Files lists the conflicted paths relative to the worktree root.
	Files []string
	// MergeSHA is set when the default branch was already fast-forwarded
	// before the failure (push and close stages): the work has landed
	// locally even though the delivery did not complete.
	MergeSHA string
	// Err is the underlying error.
	Err error
}

// Error renders "gitx: delivery failed at <stage>: <cause>", naming the
// conflicted files for a conflict.
func (e *DeliveryError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gitx: delivery failed at %s", e.Stage)
	if e.Conflict {
		b.WriteString(": conflicts")
		if len(e.Files) > 0 {
			fmt.Fprintf(&b, " in %s", strings.Join(e.Files, ", "))
		}
		return b.String()
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap returns the underlying error.
func (e *DeliveryError) Unwrap() error { return e.Err }

// BeadCloser is the part of *beads.Client a delivery needs to record the
// merge and close the bead (D-20). *beads.Client satisfies it.
type BeadCloser interface {
	// Update applies a patch to the bead (here: the sc_merge_sha metadata).
	Update(ctx context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error)
	// Close closes the bead with a reason.
	Close(ctx context.Context, id domain.BeadID, reason string) (beads.Issue, error)
}

// MergeOptions configures [Manager.DeliverMerge].
type MergeOptions struct {
	// CheckCommand is the repository's check command (config check_command),
	// run through "sh -c" in the worktree after the rebase. Empty or blank
	// means [DetectCheckCommand]; when nothing is detected no check runs.
	CheckCommand string
	// CheckTimeout bounds the check command; zero or negative means
	// [DefaultCheckTimeout].
	CheckTimeout time.Duration
	// Beads, when set, receives the sc_merge_sha metadata and the close
	// after a successful merge (and push, when there is a remote). Nil
	// skips the close stage; the caller then owns the bead's state.
	Beads BeadCloser
	// CloseReason is the bd close reason; empty means
	// "merged into <default> as <short sha>".
	CloseReason string
}

// CheckResult is the outcome of the check command stage.
type CheckResult struct {
	// Command is the shell command that ran.
	Command string
	// Detected is true when the command came from [DetectCheckCommand]
	// rather than configuration.
	Detected bool
	// ExitCode is the command's exit status (0 on success, -1 when killed).
	ExitCode int
	// Output is the tail of the command's combined standard output and
	// error, at most [MaxCheckOutput] bytes.
	Output string
	// Duration is how long the command ran.
	Duration time.Duration
}

// MergeResult describes a completed safety-off delivery.
type MergeResult struct {
	// BeadID is the delivered bead.
	BeadID domain.BeadID
	// Branch is the bead's branch ("sc/<bead-id>-<slug>").
	Branch string
	// Target is the default branch the work landed on.
	Target string
	// Remote is the remote the default branch was pushed to; empty when
	// the repository has none.
	Remote string
	// Before is the default branch tip before the merge.
	Before string
	// Head is the default branch tip after the merge: the rebased branch
	// tip, recorded on the bead as sc_merge_sha.
	Head string
	// Commits is the number of commits that landed.
	Commits int
	// Rebased is true when the rebase rewrote the branch (its tip changed).
	Rebased bool
	// Check is the check command's outcome; nil when no command was
	// configured or detected.
	Check *CheckResult
	// Pushed is true when the default branch was pushed to Remote.
	Pushed bool
	// Closed is true when the bead was closed through [MergeOptions.Beads].
	Closed bool
}

// DeliverMerge implements the safety-off path of D-10 for one bead:
// sync the default branch with its remote (when there is one), rebase the
// bead's branch onto it in the bead's worktree, run the check command,
// fast-forward the default branch in the main repository (never a merge
// commit), push it when a remote exists, then record sc_merge_sha on the
// bead and close it (D-20). The caller serialises deliveries: only one
// runs at a time per repository.
//
// Every failure is a [*DeliveryError] naming the stage it happened in and
// carrying the failing command's output; the sentinels below and the
// underlying [*GitError], proc and beads errors all stay matchable with
// errors.Is through it. A rebase that stops on conflicts is reported with
// Conflict set and [ErrRebaseConflict]; the worktree is left mid-rebase
// with the conflict markers so the engineer's conflict round can resolve
// them. On the next attempt a resolved rebase (conflicts fixed and staged,
// or already continued) is finished automatically and redone onto the
// current default branch when that moved meanwhile; an unresolved one is
// reported again. It fails at [StagePrepare] with [ErrWorktreeMissing] when
// the bead has no worktree, and at [StageRebase] with [ErrWrongBranch] when
// the worktree is not on the bead's branch (or is mid-rebase of another
// one).
//
// The default branch never receives a merge commit: the rebase flattens any
// merge in the bead's branch whatever the repository configures (see
// rebaseConfig), the result is checked for merge commits
// ([ErrNonLinearBranch]), and the branch only ever reaches the default
// branch through a fast-forward.
func (m *Manager) DeliverMerge(ctx context.Context, bead domain.Bead, opts MergeOptions) (*MergeResult, error) {
	if bead.ID == "" {
		return nil, stageErr(StagePrepare, errors.New("gitx: DeliverMerge: bead has no id"), "")
	}
	branch := BranchName(bead)
	wt, err := m.Reattach(ctx, bead.ID)
	if err != nil {
		return nil, stageErr(StagePrepare, err, "")
	}
	wtRepo, err := Open(ctx, wt.Path, WithEnv(m.repo.env), WithTimeout(m.repo.timeout))
	if err != nil {
		return nil, stageErr(StagePrepare, err, "")
	}

	target, err := m.repo.DefaultBranch(ctx)
	if err != nil {
		return nil, stageErr(StagePrepare, err, "")
	}
	res := &MergeResult{BeadID: bead.ID, Branch: branch, Target: target}

	remote, err := m.repo.DefaultRemote(ctx)
	if err != nil && !errors.Is(err, ErrNoRemote) {
		return nil, stageErr(StagePrepare, err, "")
	}
	res.Remote = remote

	// Stage fetch: bring the local default branch level with the remote,
	// then resolve the base the branch is rebased onto.
	if remote != "" {
		if err := m.syncDefault(ctx, remote, target); err != nil {
			return nil, err
		}
	}
	local, err := m.repo.BranchExists(ctx, target)
	if err != nil {
		return nil, stageErr(StageFetch, err, "")
	}
	base := target
	if !local {
		base = remote + "/" + target
	}
	if res.Before, err = m.repo.RevParse(ctx, base); err != nil {
		return nil, stageErr(StageFetch, err, "")
	}

	// Stage rebase.
	tipBefore, err := wtRepo.HeadSHA(ctx)
	if err != nil {
		return nil, stageErr(StageRebase, err, "")
	}
	if err := wtRepo.rebaseOnto(ctx, branch, base); err != nil {
		return nil, err
	}
	head, err := wtRepo.HeadSHA(ctx)
	if err != nil {
		return nil, stageErr(StageRebase, err, "")
	}
	res.Head = head
	res.Rebased = head != tipBefore
	cur, err := wtRepo.CurrentBranch(ctx)
	if err != nil && !errors.Is(err, ErrDetachedHead) {
		return nil, stageErr(StageRebase, err, "")
	}
	if cur != branch {
		return nil, stageErr(StageRebase, fmt.Errorf("%w: %s is on %q, want %q", ErrWrongBranch, wt.Path, cur, branch), "")
	}
	count, err := m.repo.Git(ctx, "rev-list", "--count", res.Before+".."+head)
	if err != nil {
		return nil, stageErr(StageRebase, err, "")
	}
	if res.Commits, err = strconv.Atoi(strings.TrimSpace(count)); err != nil {
		return nil, stageErr(StageRebase, fmt.Errorf("gitx: unexpected rev-list output %q", count), "")
	}
	if err := m.assertLinear(ctx, res.Before, head); err != nil {
		return nil, err
	}

	// Stage check.
	command, detected := strings.TrimSpace(opts.CheckCommand), false
	if command == "" {
		command, detected = DetectCheckCommand(wt.Path)
	}
	if command != "" {
		check, err := wtRepo.runCheck(ctx, command, opts.CheckTimeout)
		if check != nil {
			check.Detected = detected
			res.Check = check
		}
		if err != nil {
			return nil, err
		}
	}

	// Stage merge: fast-forward only, never a merge commit.
	if res.Commits > 0 {
		if local {
			if err := m.fastForward(ctx, target, head, branch); err != nil {
				return nil, err
			}
		}
		// Stage push.
		if remote != "" {
			src := "refs/heads/" + target
			if !local {
				src = "refs/heads/" + branch
			}
			if _, err := m.repo.Git(ctx, "push", "--quiet", remote, src+":refs/heads/"+target); err != nil {
				return nil, stageErr(StagePush, err, head)
			}
			res.Pushed = true
		}
	}

	// Stage close (D-20).
	if opts.Beads != nil {
		reason := opts.CloseReason
		if reason == "" {
			reason = fmt.Sprintf("merged into %s as %s", target, shortSHA(head))
		}
		if _, err := opts.Beads.Update(ctx, bead.ID, beads.Patch{Metadata: beads.MergeMetadata(head)}); err != nil {
			return nil, stageErr(StageClose, err, head)
		}
		if _, err := opts.Beads.Close(ctx, bead.ID, reason); err != nil {
			return nil, stageErr(StageClose, err, head)
		}
		res.Closed = true
	}
	return res, nil
}

// assertLinear checks that the commits the fast-forward would add to the
// default branch (base..head) contain no merge commit. The rebase flattens
// them (see rebaseConfig), so this is the last guard that keeps D-10's
// promise that the default branch never receives a merge commit; it fails
// at [StageRebase] with [ErrNonLinearBranch] rather than merging.
func (m *Manager) assertLinear(ctx context.Context, base, head string) error {
	out, err := m.repo.Git(ctx, "rev-list", "--merges", base+".."+head)
	if err != nil {
		return stageErr(StageRebase, err, "")
	}
	if merges := strings.Fields(out); len(merges) > 0 {
		return &DeliveryError{
			Stage:  StageRebase,
			Output: out,
			Err:    fmt.Errorf("%w: %s", ErrNonLinearBranch, shortSHA(merges[0])),
		}
	}
	return nil
}

// syncDefault fetches remote and fast-forwards the local default branch to
// its remote-tracking branch when it is behind. A local branch that is
// ahead is left alone (the push will carry its commits); one that has
// diverged fails with ErrDefaultBranchDiverged.
func (m *Manager) syncDefault(ctx context.Context, remote, target string) error {
	if _, err := m.repo.Git(ctx, "fetch", "--quiet", "--prune", remote); err != nil {
		return stageErr(StageFetch, err, "")
	}
	local, err := m.repo.BranchExists(ctx, target)
	if err != nil {
		return stageErr(StageFetch, err, "")
	}
	tracking, err := m.repo.RevParse(ctx, remote+"/"+target)
	if errors.Is(err, ErrUnknownRevision) {
		return nil // never pushed; the push stage creates it
	}
	if err != nil {
		return stageErr(StageFetch, err, "")
	}
	if !local {
		return nil
	}
	localSHA, err := m.repo.RevParse(ctx, target)
	if err != nil {
		return stageErr(StageFetch, err, "")
	}
	if localSHA == tracking {
		return nil
	}
	behind, err := m.repo.isAncestor(ctx, localSHA, tracking)
	if err != nil {
		return stageErr(StageFetch, err, "")
	}
	if behind {
		if err := m.fastForward(ctx, target, tracking, "refs/remotes/"+remote+"/"+target); err != nil {
			var de *DeliveryError
			if errors.As(err, &de) {
				de.Stage = StageFetch
			}
			return err
		}
		return nil
	}
	ahead, err := m.repo.isAncestor(ctx, tracking, localSHA)
	if err != nil {
		return stageErr(StageFetch, err, "")
	}
	if ahead {
		return nil
	}
	return &DeliveryError{
		Stage: StageFetch,
		Err:   fmt.Errorf("%w: %s is at %s, %s/%s at %s", ErrDefaultBranchDiverged, target, shortSHA(localSHA), remote, target, shortSHA(tracking)),
	}
}

// fastForward moves the local branch target to sha (which must descend
// from it) in the main repository. When target is checked out there,
// "git merge --ff-only" updates the working tree too; otherwise the ref is
// updated in place with "git fetch . <ref>:<target>", which git refuses
// when target is checked out in some other worktree. ref names the
// revision for git (a branch or full ref resolving to sha).
func (m *Manager) fastForward(ctx context.Context, target, sha, ref string) error {
	ok, err := m.repo.isAncestor(ctx, target, sha)
	if err != nil {
		return stageErr(StageMerge, err, "")
	}
	if !ok {
		return &DeliveryError{Stage: StageMerge, Err: fmt.Errorf("%w: %s is not an ancestor of %s", ErrNotFastForward, target, shortSHA(sha))}
	}
	current, err := m.repo.CurrentBranch(ctx)
	if err != nil && !errors.Is(err, ErrDetachedHead) {
		return stageErr(StageMerge, err, "")
	}
	if current == target {
		_, err = m.repo.Git(ctx, gitArgs(mergeConfig, "merge", "--ff-only", "--quiet", "--no-verify", ref)...)
	} else {
		// "git fetch ." refuses a non-fast-forward update without a
		// leading "+", and never creates a commit of its own.
		_, err = m.repo.Git(ctx, "fetch", "--quiet", ".", ref+":refs/heads/"+target)
	}
	if err != nil {
		return stageErr(StageMerge, err, "")
	}
	got, err := m.repo.RevParse(ctx, target)
	if err != nil {
		return stageErr(StageMerge, err, "")
	}
	if got != sha {
		return &DeliveryError{Stage: StageMerge, Err: fmt.Errorf("%w: %s is at %s after the fast-forward, want %s", ErrNotFastForward, target, shortSHA(got), shortSHA(sha))}
	}
	return nil
}

// rebaseOnto rebases the worktree's branch onto base and leaves the branch
// checked out on top of it.
//
// A rebase left mid-way by an earlier attempt is continued when its
// conflicts are resolved (no unmerged paths) and reported again otherwise;
// one that is rebasing some other branch is refused with [ErrWrongBranch].
// A continued rebase may have been started against an older base (the
// default branch moved, or the engineer continued it themselves), so the
// branch is rebased onto base again unless base is already an ancestor of
// it. Conflicts are returned as a DeliveryError with Conflict set; the
// worktree keeps the conflict state.
func (r *Repo) rebaseOnto(ctx context.Context, branch, base string) error {
	committer, err := r.withIdentity(ctx)
	if err != nil {
		return stageErr(StageRebase, err, "")
	}
	if r.RebaseInProgress() {
		if got := r.RebasingBranch(); got != "" && got != branch {
			return stageErr(StageRebase, fmt.Errorf("%w: %s is mid-rebase of %q, want %q", ErrWrongBranch, r.Root, got, branch), "")
		}
		files, err := r.ConflictedPaths(ctx)
		if err != nil {
			return stageErr(StageRebase, err, "")
		}
		if len(files) > 0 {
			return &DeliveryError{Stage: StageRebase, Conflict: true, Files: files, Err: ErrRebaseConflict}
		}
		out, rebaseErr := committer.Git(ctx, gitArgs(rebaseConfig, "rebase", "--continue")...)
		if rebaseErr != nil {
			return r.rebaseFailure(ctx, out, rebaseErr)
		}
		onBase, err := r.isAncestor(ctx, base, branch)
		if err != nil {
			return stageErr(StageRebase, err, "")
		}
		if onBase {
			return nil
		}
		// The continued rebase landed on an older base; redo it.
	}
	cur, err := r.CurrentBranch(ctx)
	if err != nil && !errors.Is(err, ErrDetachedHead) {
		return stageErr(StageRebase, err, "")
	}
	if cur != branch {
		return stageErr(StageRebase, fmt.Errorf("%w: %s is on %q, want %q", ErrWrongBranch, r.Root, cur, branch), "")
	}
	out, rebaseErr := committer.Git(ctx, gitArgs(rebaseConfig, "rebase", "--no-verify", "--quiet", base)...)
	if rebaseErr != nil {
		return r.rebaseFailure(ctx, out, rebaseErr)
	}
	return nil
}

// rebaseFailure turns a failed "git rebase" into a DeliveryError: a
// conflict (the worktree is left mid-rebase for the engineer) when the
// rebase is still in progress, a plain stage failure otherwise.
func (r *Repo) rebaseFailure(ctx context.Context, out string, err error) error {
	var ge *GitError
	output := out
	if errors.As(err, &ge) {
		output = ge.Stderr
	}
	if !r.RebaseInProgress() {
		return stageErr(StageRebase, err, "")
	}
	files, ferr := r.ConflictedPaths(ctx)
	if ferr != nil {
		return stageErr(StageRebase, ferr, "")
	}
	return &DeliveryError{Stage: StageRebase, Output: output, Conflict: true, Files: files, Err: fmt.Errorf("%w: %w", ErrRebaseConflict, err)}
}

// RebaseInProgress reports whether a rebase was started in the working
// tree and not yet finished or aborted.
func (r *Repo) RebaseInProgress() bool {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if info, err := os.Stat(filepath.Join(r.GitDir, d)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// RebasingBranch returns the branch an in-progress rebase in the working
// tree is rebasing, without the "refs/heads/" prefix. It returns "" when no
// rebase is in progress, and "detached HEAD" (git's own wording) when the
// rebase was started from a detached HEAD.
func (r *Repo) RebasingBranch() string {
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		data, err := os.ReadFile(filepath.Join(r.GitDir, d, "head-name"))
		if err != nil {
			continue
		}
		return strings.TrimPrefix(strings.TrimSpace(string(data)), "refs/heads/")
	}
	return ""
}

// ConflictedPaths lists the unmerged paths of the working tree relative
// to its root; nil when there are none.
func (r *Repo) ConflictedPaths(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// isAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.Git(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if isExit(err, 1) {
		return false, nil
	}
	return false, err
}

// runCheck runs command with "sh -c" in the working tree, with the
// process's full environment (plus the Repo's explicit variables) so
// toolchains find their caches and settings. The result is returned even
// on failure; a non-zero exit is a DeliveryError at StageCheck wrapping
// ErrCheckFailed, and a timeout also matches proc.ErrTimeout.
func (r *Repo) runCheck(ctx context.Context, command string, timeout time.Duration) (*CheckResult, error) {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	env := map[string]string{}
	for k, v := range r.env {
		env[k] = v
	}
	var inherit []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		inherit = append(inherit, name)
	}
	spec := proc.Spec{
		Command:    "sh",
		Args:       []string{"-c", command},
		Dir:        r.Root,
		Env:        env,
		InheritEnv: inherit,
		Timeout:    timeout,
	}
	result, err := proc.Run(ctx, spec)
	check := &CheckResult{
		Command:  command,
		ExitCode: result.ExitCode,
		Output:   tailOf(result.Stdout+result.Stderr, MaxCheckOutput),
		Duration: result.Duration,
	}
	if err != nil {
		var ee *proc.ExitError
		if !errors.As(err, &ee) {
			// sh never started, so there is no exit status.
			check.ExitCode = -1
		}
		// Every failure — a non-zero exit, a timeout, or sh not starting
		// at all — carries whatever the command managed to print.
		return check, &DeliveryError{Stage: StageCheck, Output: check.Output, Err: fmt.Errorf("%w: %w", ErrCheckFailed, err)}
	}
	return check, nil
}

// checkCandidates lists the autodetected check commands in the order the
// task text gives them: make test, npm test, go test, pytest, cargo test.
var checkCandidates = []struct {
	command string
	present func(dir string) bool
}{
	{"make test", hasMakeTestTarget},
	{"npm test", hasNPMTestScript},
	{"go test ./...", hasFile("go.mod")},
	{"pytest", hasFile("pyproject.toml", "setup.py", "setup.cfg", "pytest.ini", "tox.ini", "requirements.txt")},
	{"cargo test", hasFile("Cargo.toml")},
}

// DetectCheckCommand picks the check command for a project by the files in
// dir, in the order P4-06 gives: "make test" when a Makefile has a test
// target, "npm test" when package.json declares a test script, "go test
// ./..." for go.mod, "pytest" for a Python project file (pyproject.toml,
// setup.py, setup.cfg, pytest.ini, tox.ini, requirements.txt), "cargo test"
// for Cargo.toml. It returns "" and false when nothing matches, which means
// no check runs (D-10).
//
// A candidate whose command could not work is skipped rather than
// detected — a Makefile without a test target, a package.json without a
// test script — because a check that fails for want of a target would send
// working code back to the engineer.
func DetectCheckCommand(dir string) (string, bool) {
	for _, c := range checkCandidates {
		if c.present(dir) {
			return c.command, true
		}
	}
	return "", false
}

func hasFile(names ...string) func(string) bool {
	return func(dir string) bool {
		for _, n := range names {
			if info, err := os.Stat(filepath.Join(dir, n)); err == nil && !info.IsDir() {
				return true
			}
		}
		return false
	}
}

// maxManifestBytes bounds how much of a project manifest the detector
// reads.
const maxManifestBytes = 1 << 20

// hasNPMTestScript reports whether dir holds a package.json that declares a
// non-empty "test" script. Without one "npm test" fails with "Missing
// script: test", so it is not a usable check command; an unreadable or
// malformed manifest counts the same way.
func hasNPMTestScript(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxManifestBytes)).Decode(&manifest); err != nil {
		return false
	}
	return strings.TrimSpace(manifest.Scripts["test"]) != ""
}

var makeTestTarget = regexp.MustCompile(`^test\s*:`)

// hasMakeTestTarget reports whether a Makefile in dir defines a "test"
// target.
func hasMakeTestTarget(dir string) bool {
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		found := false
		for sc.Scan() {
			if makeTestTarget.MatchString(sc.Text()) {
				found = true
				break
			}
		}
		_ = f.Close()
		if found {
			return true
		}
	}
	return false
}

// stageErr wraps err as a DeliveryError at stage, carrying git's stderr as
// the output and mergeSHA when the merge already happened.
func stageErr(stage DeliveryStage, err error, mergeSHA string) error {
	var already *DeliveryError
	if errors.As(err, &already) {
		return err // already staged deeper down; keep the precise stage
	}
	de := &DeliveryError{Stage: stage, Err: err, MergeSHA: mergeSHA}
	var ge *GitError
	if errors.As(err, &ge) {
		de.Output = ge.Stderr
	}
	var be *beads.Error
	if errors.As(err, &be) {
		de.Output = be.Message
	}
	return de
}

// tailOf returns the last n bytes of s, cut at a line boundary and
// prefixed with a marker when s was longer.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "[... output truncated ...]\n" + cut
}
