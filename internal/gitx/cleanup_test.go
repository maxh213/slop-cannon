package gitx

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// cuTracker is an in-memory ResumeTracker recording what Resume wrote.
type cuTracker struct {
	mu       sync.Mutex
	statuses map[domain.BeadID]domain.BeadState
	meta     map[domain.BeadID]map[string]any
	closed   map[domain.BeadID]string
	calls    []string
	err      error
	closeErr error
}

func (tr *cuTracker) record(call string) {
	tr.calls = append(tr.calls, call)
}

func (tr *cuTracker) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("status " + string(id) + " " + string(state))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.statuses == nil {
		tr.statuses = map[domain.BeadID]domain.BeadState{}
	}
	tr.statuses[id] = state
	return beads.Issue{ID: id, Status: string(state)}, nil
}

func (tr *cuTracker) Update(_ context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("update " + string(id))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.meta == nil {
		tr.meta = map[domain.BeadID]map[string]any{}
	}
	if tr.meta[id] == nil {
		tr.meta[id] = map[string]any{}
	}
	for k, v := range p.Metadata {
		tr.meta[id][k] = v
	}
	return beads.Issue{ID: id}, nil
}

func (tr *cuTracker) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("close " + string(id))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.closeErr != nil {
		return beads.Issue{}, tr.closeErr
	}
	if tr.closed == nil {
		tr.closed = map[domain.BeadID]string{}
	}
	tr.closed[id] = reason
	return beads.Issue{ID: id, Status: "closed", CloseReason: reason}, nil
}

// cuBead builds a bead in a state.
func cuBead(id, title string, state domain.BeadState) domain.Bead {
	return domain.Bead{ID: domain.BeadID(id), Title: title, State: state}
}

// cuWork creates the bead's worktree and commits one file in it,
// returning the commit.
func cuWork(t *testing.T, f *wtFixture, bead domain.Bead) string {
	t.Helper()
	ctx := context.Background()
	_, err := f.m.Create(ctx, bead)
	require.NoError(t, err)
	wtRepo, err := f.m.Open(ctx, bead.ID)
	require.NoError(t, err)
	acWrite(t, wtRepo.Root, "work/"+string(bead.ID)+".txt", "done by "+string(bead.ID)+"\n")
	c, err := wtRepo.CommitAll(ctx, "work for "+string(bead.ID))
	require.NoError(t, err)
	require.True(t, c.Committed)
	return c.SHA
}

// cuLand fast-forwards main to the bead's branch tip in the main
// repository, the way a delivery that crashed right after its merge stage
// leaves things.
func cuLand(t *testing.T, f *wtFixture, bead domain.Bead) string {
	t.Helper()
	f.r.Git(t, "merge", "--ff-only", "-q", BranchName(bead))
	return f.r.Head(t)
}

// cuConflict leaves the bead's worktree mid-rebase with a conflict in
// README.md.
func cuConflict(t *testing.T, f *wtFixture, bead domain.Bead) {
	t.Helper()
	ctx := context.Background()
	wtRepo, err := f.m.Open(ctx, bead.ID)
	require.NoError(t, err)
	acWrite(t, wtRepo.Root, "README.md", "branch version\n")
	_, err = wtRepo.CommitAll(ctx, "branch README")
	require.NoError(t, err)
	f.r.WriteFile(t, "README.md", "main version\n")
	f.r.Commit(t, "main README")
	_, err = wtRepo.Git(ctx, "-c", "commit.gpgsign=false", "rebase", "--quiet", "main")
	require.Error(t, err)
	require.True(t, wtRepo.RebaseInProgress())
}

// cuEntry finds the cleanup entry of a bead.
func cuEntry(t *testing.T, res *CleanupResult, id domain.BeadID) CleanupEntry {
	t.Helper()
	for _, e := range res.Entries {
		if e.BeadID == id {
			return e
		}
	}
	t.Fatalf("no cleanup entry for %s in %+v", id, res.Entries)
	return CleanupEntry{}
}

// cuResumeEntry finds the resume entry of a bead.
func cuResumeEntry(t *testing.T, res *ResumeResult, id domain.BeadID) ResumeEntry {
	t.Helper()
	for _, e := range res.Entries {
		if e.Bead.ID == id {
			return e
		}
	}
	t.Fatalf("no resume entry for %s", id)
	return ResumeEntry{}
}

// cuResumer builds a resumer for a fresh manager (simulated restart) with
// a quiet logger.
func cuResumer(t *testing.T, m *Manager, tracker ResumeTracker, opts ...ResumeOption) *Resumer {
	t.Helper()
	opts = append([]ResumeOption{WithResumeLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	r, err := NewResumer(m, tracker, opts...)
	require.NoError(t, err)
	return r
}

func TestManager_Cleanup(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("finish with mixed states", func(t *testing.T) {
		f := wtSetup(t, nil)
		closedMerged := cuBead("fx-c1", "closed and merged", domain.BeadStateClosed)
		closedUnmerged := cuBead("fx-c2", "closed but never merged", domain.BeadStateClosed)
		closedDirty := cuBead("fx-c3", "closed with leftovers", domain.BeadStateClosed)
		closedNoWT := cuBead("fx-c4", "closed, worktree already gone", domain.BeadStateClosed)
		closedWiped := cuBead("fx-c5", "closed, worktree directory wiped", domain.BeadStateClosed)
		closedRetitled := cuBead("fx-c6", "the title it was created with", domain.BeadStateClosed)
		failed := cuBead("fx-f1", "failed", domain.BeadStateFailed)
		human := cuBead("fx-h1", "needs a human", domain.BeadStateNeedsHuman)
		inFlight := cuBead("fx-p1", "still in progress", domain.BeadStateInProgress)
		blocked := cuBead("fx-b1", "blocked by bd", domain.BeadStateBlocked)
		unknown := cuBead("fx-u1", "not in the final state", domain.BeadStateInReview)
		vanished := cuBead("fx-v1", "directory wiped", domain.BeadStateFailed)

		cuWork(t, f, closedMerged)
		cuLand(t, f, closedMerged)
		cuWork(t, f, closedUnmerged)
		cuWork(t, f, closedDirty)
		cuLand(t, f, closedDirty)
		require.NoError(t, os.WriteFile(filepath.Join(f.m.PathFor(closedDirty.ID), "scratch.txt"), []byte("wip\n"), 0o644))
		cuWork(t, f, closedNoWT)
		cuLand(t, f, closedNoWT)
		require.NoError(t, f.m.Remove(ctx, closedNoWT.ID, false))
		cuWork(t, f, closedWiped)
		cuLand(t, f, closedWiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(closedWiped.ID)))
		cuWork(t, f, closedRetitled)
		cuLand(t, f, closedRetitled)
		closedRetitledBranch := BranchName(closedRetitled)
		// The PO renamed the bead after its worktree was created.
		closedRetitled.Title = "renamed after the fact"
		require.NotEqual(t, closedRetitledBranch, BranchName(closedRetitled))
		for _, b := range []domain.Bead{failed, human, inFlight, blocked, unknown, vanished} {
			cuWork(t, f, b)
		}
		require.NoError(t, os.RemoveAll(f.m.PathFor(vanished.ID)))
		require.NoError(t, os.MkdirAll(filepath.Join(f.m.Dir(), "fx-husk"), 0o755))
		mainBefore := f.r.Head(t)

		var printed strings.Builder
		res, err := f.m.Cleanup(ctx, []domain.Bead{closedMerged, closedUnmerged, closedDirty, closedNoWT, closedWiped, closedRetitled, failed, human, inFlight, blocked, vanished}, CleanupOptions{Logger: quiet, Out: &printed})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, []string{"main"}, res.Bases, "no remote: only the local default branch")

		e := cuEntry(t, res, closedMerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.True(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "deleted (merged into main)")
		assert.NoDirExists(t, e.Path)

		e = cuEntry(t, res, closedUnmerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "kept (not merged into main)")
		ok, err := f.repo.BranchExists(ctx, BranchName(closedUnmerged))
		require.NoError(t, err)
		assert.True(t, ok, "an unmerged branch is never deleted")

		e = cuEntry(t, res, closedDirty.ID)
		assert.Equal(t, CleanupRemoved, e.Action, "leftovers of a closed bead do not stop the removal")
		assert.True(t, e.BranchDeleted)

		e = cuEntry(t, res, closedNoWT.ID)
		assert.Equal(t, CleanupNone, e.Action)
		assert.True(t, e.BranchDeleted, "a merged branch is deleted even without a worktree")

		e = cuEntry(t, res, closedWiped.ID)
		assert.Equal(t, CleanupPruned, e.Action)
		assert.True(t, e.BranchDeleted, "a done bead's merged branch goes even when its directory was wiped")
		assert.Contains(t, e.Reason, "deleted (merged into main)")
		ok, err = f.repo.BranchExists(ctx, BranchName(closedWiped))
		require.NoError(t, err)
		assert.False(t, ok)

		e = cuEntry(t, res, closedRetitled.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.Equal(t, closedRetitledBranch, e.Branch)
		assert.True(t, e.BranchDeleted, "the branch the worktree held is deleted, not the one the new title names")
		ok, err = f.repo.BranchExists(ctx, closedRetitledBranch)
		require.NoError(t, err)
		assert.False(t, ok)

		for _, b := range []domain.Bead{failed, human, inFlight, blocked} {
			e = cuEntry(t, res, b.ID)
			assert.Equal(t, CleanupKept, e.Action, b.ID)
			assert.Equal(t, b.State, e.State)
			assert.Equal(t, "the bead is "+string(b.State), e.Reason)
			assert.Equal(t, f.m.PathFor(b.ID), e.Path)
			assert.DirExists(t, e.Path)
			assert.False(t, e.BranchDeleted)
		}
		e = cuEntry(t, res, unknown.ID)
		assert.Equal(t, CleanupKept, e.Action)
		assert.Empty(t, e.State)
		assert.Contains(t, e.Reason, "not part of this run")
		e = cuEntry(t, res, vanished.ID)
		assert.Equal(t, CleanupPruned, e.Action)
		assert.False(t, e.BranchDeleted, "a failed bead keeps its branch even when its directory is gone")
		ok, err = f.repo.BranchExists(ctx, BranchName(vanished))
		require.NoError(t, err)
		assert.True(t, ok)

		assert.Equal(t, []string{
			f.m.PathFor(blocked.ID), f.m.PathFor(failed.ID), f.m.PathFor(human.ID), f.m.PathFor(inFlight.ID), f.m.PathFor(unknown.ID),
		}, res.KeptPaths(), "sorted by bead id")
		assert.Len(t, res.Removed(), 4)
		assert.Empty(t, res.Failed())

		summary := res.Summary()
		assert.True(t, strings.HasPrefix(summary, "removed 4 worktrees, pruned 2 stale registrations, kept 5:"), summary)
		assert.Contains(t, summary, "\n  fx-b1 (blocked): "+f.m.PathFor(blocked.ID))
		assert.Contains(t, summary, "\n  fx-f1 (failed): "+f.m.PathFor(failed.ID))
		assert.Contains(t, summary, "\n  fx-h1 (needs_human): "+f.m.PathFor(human.ID))
		assert.Contains(t, summary, "\n  fx-u1 (unknown): "+f.m.PathFor(unknown.ID))
		assert.NotContains(t, summary, "failure")
		assert.Equal(t, summary+"\n", printed.String(), "the kept paths are printed for the run's final report")

		// Housekeeping: the vanished registration and the husk are gone,
		// the kept worktrees are still registered, main is untouched.
		list, err := f.m.List(ctx)
		require.NoError(t, err)
		require.Len(t, list, 5)
		for _, wt := range list {
			assert.False(t, wt.Prunable, wt.BeadID)
		}
		assert.NoDirExists(t, filepath.Join(f.m.Dir(), "fx-husk"))
		assert.Equal(t, mainBefore, f.r.Head(t))
		assert.Empty(t, f.r.Status(t))

		// A second pass is a no-op apart from the kept worktrees.
		again, err := f.m.Cleanup(ctx, []domain.Bead{closedMerged, failed}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, again.Removed())
		assert.Len(t, again.Kept(), 5)
	})

	t.Run("KeepBranches leaves merged branches alone", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		cuLand(t, f, bead)
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{KeepBranches: true, Logger: quiet})
		require.NoError(t, err)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "branch kept as requested")
		ok, err := f.repo.BranchExists(ctx, BranchName(bead))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("no default branch keeps branches", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		f.r.Git(t, "checkout", "-q", "-b", "trunk")
		f.r.Git(t, "branch", "-D", "-q", "main")
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, res.Default)
		assert.Empty(t, res.Bases)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "no default branch")
	})

	t.Run("nothing to do", func(t *testing.T) {
		f := wtSetup(t, nil)
		res, err := f.m.Cleanup(ctx, []domain.Bead{cuBead("fx-1", "never started", domain.BeadStateOpen)}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, res.Entries)
		assert.Equal(t, "removed 0 worktrees", res.Summary())
		assert.NoError(t, res.Err())
	})

	t.Run("a branch merged only on the remote is deleted", func(t *testing.T) {
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		landed := cuBead("fx-c1", "delivered with safety on", domain.BeadStateClosed)
		unmerged := cuBead("fx-c2", "closed with the PR still open", domain.BeadStateClosed)
		cuWork(t, f, landed)
		cuWork(t, f, unmerged)
		// The pull request merged on the forge: the remote default branch
		// has the work, the local one does not.
		f.r.Git(t, "push", "-q", "origin", BranchName(landed)+":main")
		f.r.Git(t, "fetch", "-q", "origin")
		localMain := f.r.Head(t)

		res, err := f.m.Cleanup(ctx, []domain.Bead{landed, unmerged}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, []string{"origin/main", "main"}, res.Bases, "the remote-tracking branch is checked first")

		e := cuEntry(t, res, landed.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.True(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "deleted (merged into origin/main)")
		ok, err := f.repo.BranchExists(ctx, BranchName(landed))
		require.NoError(t, err)
		assert.False(t, ok)

		e = cuEntry(t, res, unmerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "kept (not merged into origin/main or main)")
		ok, err = f.repo.BranchExists(ctx, BranchName(unmerged))
		require.NoError(t, err)
		assert.True(t, ok)

		assert.Equal(t, localMain, f.r.Head(t), "cleanup never moves the default branch")
		assert.Empty(t, f.r.Status(t))
	})

	t.Run("a failed removal is reported and the worktree kept", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		if os.Geteuid() == 0 {
			t.Skip("root can delete anything")
		}
		cuWork(t, f, bead)
		// An unwritable directory inside the worktree makes the removal
		// fail half-way.
		locked := filepath.Join(f.m.PathFor(bead.ID), "locked")
		require.NoError(t, os.MkdirAll(locked, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(locked, "pin"), []byte("x"), 0o644))
		require.NoError(t, os.Chmod(locked, 0o500))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupKept, e.Action)
		assert.Error(t, e.Err)
		assert.ErrorContains(t, res.Err(), "fx-c1")
		assert.Contains(t, res.Summary(), "1 failure:")
		assert.DirExists(t, e.Path)
	})
}

func TestNewResumer(t *testing.T) {
	f := wtSetup(t, nil)
	_, err := NewResumer(nil, nil)
	require.Error(t, err)
	r, err := NewResumer(f.m, nil)
	require.NoError(t, err)
	assert.Same(t, f.repo, r.gh.Repo())
	gh := NewGH(f.repo)
	r, err = NewResumer(f.m, nil, WithResumeGH(gh), WithResumeGH(nil), WithResumeLogger(nil))
	require.NoError(t, err)
	assert.Same(t, gh, r.gh)
	assert.NotNil(t, r.logger)
}

func TestResumer_Resume(t *testing.T) {
	ctx := context.Background()

	t.Run("after a simulated crash without a remote", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered before the crash", domain.BeadStateDelivering)
		undelivered := cuBead("fx-d2", "delivery never ran", domain.BeadStateDelivering)
		branchGone := cuBead("fx-d3", "branch deleted by hand", domain.BeadStateDelivering)
		retitled := cuBead("fx-d4", "the title it was created with", domain.BeadStateDelivering)
		wiped := cuBead("fx-p1", "worktree wiped mid-flight", domain.BeadStateInProgress)
		fresh := cuBead("fx-p2", "claimed, nothing created yet", domain.BeadStateInProgress)
		conflicted := cuBead("fx-r1", "mid-rebase", domain.BeadStateInReview)
		dirty := cuBead("fx-r2", "uncommitted edits", domain.BeadStateChangesRequested)
		done := cuBead("fx-x1", "closed, worktree not yet cleaned", domain.BeadStateClosed)
		failed := cuBead("fx-x2", "failed without a worktree", domain.BeadStateFailed)
		orphan := cuBead("fx-o1", "from another list", domain.BeadStateInProgress)

		landedSHA := cuWork(t, f, landed)
		cuLand(t, f, landed)
		undeliveredSHA := cuWork(t, f, undelivered)
		cuWork(t, f, branchGone)
		require.NoError(t, f.m.Remove(ctx, branchGone.ID, false))
		require.NoError(t, f.m.DeleteBranch(ctx, branchGone))
		retitledSHA := cuWork(t, f, retitled)
		retitledBranch := BranchName(retitled)
		// The PO rewrote the bead's title after the worktree was created, so
		// BranchName(bead) no longer names the branch the work is on.
		retitled.Title = "a much better title"
		require.NotEqual(t, retitledBranch, BranchName(retitled))
		wipedSHA := cuWork(t, f, wiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))
		cuWork(t, f, conflicted)
		cuConflict(t, f, conflicted)
		cuWork(t, f, dirty)
		require.NoError(t, os.WriteFile(filepath.Join(f.m.PathFor(dirty.ID), "edit.txt"), []byte("wip\n"), 0o644))
		cuWork(t, f, done)
		cuWork(t, f, orphan)
		mainHead := f.r.Head(t)

		// Restart: a fresh manager built only from the paths.
		m2 := f.wtReopen(t)
		tracker := &cuTracker{}
		r := cuResumer(t, m2, tracker)
		beadList := []domain.Bead{landed, undelivered, branchGone, retitled, wiped, fresh, conflicted, dirty, done, failed}
		res, err := r.Resume(ctx, beadList)
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, "main", res.Base)
		assert.False(t, res.Fetched)
		assert.Empty(t, res.Warnings)
		require.Len(t, res.Entries, len(beadList))
		for i, b := range beadList {
			assert.Equal(t, b.ID, res.Entries[i].Bead.ID, "caller's order")
		}

		// Delivered before the crash: closed with sc_merge_sha.
		e := cuResumeEntry(t, res, landed.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, 0, e.Ahead)
		assert.Equal(t, landedSHA, e.Head)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Changed())
		assert.True(t, e.Applied)
		assert.Equal(t, landedSHA, e.MergeSHA)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Contains(t, e.Reason, "landed on main")
		assert.Equal(t, map[string]any{beads.MetaMergeSHA: landedSHA}, tracker.meta[landed.ID])
		assert.Contains(t, tracker.closed[landed.ID], "merged into main as "+landedSHA[:12])

		// Delivery never ran: still delivering, worktree reattached.
		e = cuResumeEntry(t, res, undelivered.ID)
		assert.False(t, e.Landed)
		assert.Equal(t, 1, e.Ahead)
		assert.Equal(t, undeliveredSHA, e.Head)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, m2.PathFor(undelivered.ID), e.Worktree.Path)
		assert.Contains(t, e.Reason, "deliver again")

		// Branch gone with nothing landed: a human must look.
		e = cuResumeEntry(t, res, branchGone.ID)
		assert.False(t, e.BranchExists)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.Equal(t, domain.BeadStateNeedsHuman, tracker.statuses[branchGone.ID])

		// Retitled since the worktree was created: the branch the worktree
		// holds is reconciled, not the one the new title would name.
		e = cuResumeEntry(t, res, retitled.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Equal(t, retitledBranch, e.Branch)
		assert.True(t, e.BranchExists, "a retitled bead's work is not reported as a vanished branch")
		assert.Equal(t, retitledSHA, e.Head)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "a retitled bead is never handed to a human for a branch that is right there")
		assert.Contains(t, e.Reason, "deliver again")

		// Wiped worktree of an in-flight bead: recreated from its branch.
		e = cuResumeEntry(t, res, wiped.ID)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, wipedSHA, e.Worktree.Head, "the committed work survived on the branch")
		assert.FileExists(t, filepath.Join(e.Worktree.Path, "work", "fx-p1.txt"))
		assert.Equal(t, domain.BeadStateInProgress, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "recreated")

		// Claimed but never created: a worktree is created from main.
		e = cuResumeEntry(t, res, fresh.ID)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		assert.True(t, e.BranchExists)
		assert.Equal(t, mainHead, e.Head)
		assert.False(t, e.Changed())

		// Mid-rebase: reported, not touched.
		e = cuResumeEntry(t, res, conflicted.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.True(t, e.Conflict)
		assert.Equal(t, []string{"README.md"}, e.ConflictFiles)
		assert.True(t, e.Dirty)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "conflicts in README.md")

		// Uncommitted edits: reported, kept.
		e = cuResumeEntry(t, res, dirty.ID)
		assert.True(t, e.Dirty)
		assert.False(t, e.Conflict)
		assert.False(t, e.Changed())
		assert.Equal(t, "unchanged", e.Reason)
		assert.FileExists(t, filepath.Join(m2.PathFor(dirty.ID), "edit.txt"))

		// Closed: nothing to do, the worktree waits for Cleanup.
		e = cuResumeEntry(t, res, done.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.Equal(t, "already closed", e.Reason)

		// Failed without a worktree: not recreated.
		e = cuResumeEntry(t, res, failed.ID)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.False(t, e.BranchExists)
		assert.False(t, e.Changed())
		assert.NoDirExists(t, m2.PathFor(failed.ID))

		// The orphan is reported and left alone.
		require.Len(t, res.Orphans, 1)
		assert.Equal(t, orphan.ID, res.Orphans[0].BeadID)
		assert.DirExists(t, m2.PathFor(orphan.ID))

		changed := res.Changed()
		require.Len(t, changed, 2)
		assert.Equal(t, landed.ID, changed[0].Bead.ID)
		assert.Equal(t, branchGone.ID, changed[1].Bead.ID)
		assert.Equal(t, []string{"update fx-d1", "close fx-d1", "status fx-d3 needs_human"}, tracker.calls)

		summary := res.Summary()
		assert.Contains(t, summary, "fx-d1: delivering -> closed [worktree attached]")
		assert.Contains(t, summary, "fx-d3: delivering -> needs_human [worktree missing]")
		assert.Contains(t, summary, "fx-r1: in_review [worktree attached, conflict]")
		assert.Contains(t, summary, "fx-r2: changes_requested [worktree attached, dirty]")
		assert.Contains(t, summary, "fx-p1: in_progress [worktree recreated]")

		// Main is untouched by the resume and the repository stays clean.
		assert.NotEqual(t, mainHead, landedSHA)
		assert.Equal(t, "main", f.r.Git(t, "symbolic-ref", "--short", "HEAD"))
		assert.Empty(t, f.r.Status(t))

		// Resuming again finds everything attached and nothing to change.
		tracker2 := &cuTracker{}
		res2, err := cuResumer(t, f.wtReopen(t), tracker2).Resume(ctx, []domain.Bead{undelivered, wiped, fresh, conflicted})
		require.NoError(t, err)
		assert.Empty(t, res2.Changed())
		assert.Empty(t, tracker2.calls)
		for _, e := range res2.Entries {
			assert.Equal(t, WorktreeAttached, e.WorktreeStatus, e.Bead.ID)
		}
	})

	t.Run("without a tracker it is a dry run", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		res, err := cuResumer(t, f.wtReopen(t), nil).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Contains(t, res.Summary(), "-> closed (not applied)")
	})

	t.Run("a gate still blocking the close leaves the bead to the gate poller", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "merged by hand with a PR gate", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		tracker := &cuTracker{closeErr: fmt.Errorf("beads: close: %w", beads.ErrBlockedOpen)}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Empty(t, e.MergeSHA)
		assert.Contains(t, e.Reason, "gate poller")
	})

	t.Run("tracker failures are reported per bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		tracker := &cuTracker{err: errors.New("bd is down")}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.False(t, e.Applied)
		assert.ErrorContains(t, e.Err, "bd is down")
		assert.ErrorContains(t, res.Err(), "fx-d1")
		assert.Contains(t, res.Summary(), "(error: applying closed: bd is down)")
	})

	t.Run("recreation failures are reported per bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		wiped := cuBead("fx-p1", "wiped", domain.BeadStateInProgress)
		cuWork(t, f, wiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))
		f.r.WriteFile(t, "notes.txt", "the user is editing\n")
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{wiped})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, wiped.ID)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.ErrorIs(t, e.Err, ErrRepoDirty)
		assert.False(t, e.Changed())
	})

	t.Run("a bead without an id is an entry error", func(t *testing.T) {
		f := wtSetup(t, nil)
		res, err := cuResumer(t, f.m, &cuTracker{}).Resume(ctx, []domain.Bead{{Title: "no id"}})
		require.NoError(t, err)
		require.Len(t, res.Entries, 1)
		assert.Error(t, res.Entries[0].Err)
		assert.Error(t, res.Err())
	})

	t.Run("no default branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		f.r.Git(t, "checkout", "-q", "-b", "trunk")
		f.r.Git(t, "branch", "-D", "-q", "main")
		_, err := cuResumer(t, f.m, nil).Resume(ctx, nil)
		require.ErrorIs(t, err, ErrNoDefaultBranch)
	})
}

func TestResumer_Resume_WithRemote(t *testing.T) {
	ctx := context.Background()

	// cuRemoteSetup builds a fixture with a bare remote, a gh shim, and one
	// delivering bead with a commit on its branch.
	type remoteFixture struct {
		*wtFixture
		gh   *testutil.Shim
		bead domain.Bead
		sha  string
	}
	setup := func(t *testing.T, state domain.BeadState) *remoteFixture {
		t.Helper()
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		bead := cuBead("fx-d1", "delivered with safety on", state)
		sha := cuWork(t, f, bead)
		return &remoteFixture{wtFixture: f, gh: dpGH(t), bead: bead, sha: sha}
	}

	t.Run("landed on the remote default branch only", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		// The push stage reached the remote, the local main was not moved
		// (the delivery ran from the remote-tracking branch) and then the
		// process died before the close.
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead)+":main")
		localMain := f.r.Head(t)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker, WithoutGH()).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.True(t, res.Fetched)
		assert.Equal(t, "origin/main", res.Base)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, f.sha, e.MergeSHA)
		assert.Equal(t, localMain, f.r.Head(t), "resume never moves the local default branch")
		assert.Empty(t, f.gh.Invocations(t), "WithoutGH skips gh")
	})

	t.Run("open pull request stays delivering", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "prs.json", `[{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"OPEN","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		require.NotNil(t, e.PR)
		assert.Equal(t, 5, e.PR.Number)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "PR #5 is open")
		assert.Empty(t, tracker.calls)
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"pr", "list", "--head", BranchName(f.bead), "--state", "all", "--limit", "50", "--json", "number,url,state,title"}, calls[0].Args)
	})

	t.Run("merged pull request waits for its gate", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "prs.json", `[{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"MERGED","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "MERGED", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "PR #5 merged")
		assert.Empty(t, tracker.calls, "bd closes the bead through the gate, never directly")
	})

	t.Run("an open pull request outlives the local branch", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		// The branch was pushed and its pull request opened; then the local
		// worktree and branch were cleaned away (a partial cleanup, or a
		// human tidying up) before the crash. The work is on the remote.
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead))
		require.NoError(t, f.m.Remove(ctx, f.bead.ID, true))
		require.NoError(t, f.m.DeleteBranch(ctx, f.bead))
		dpSet(t, f.gh, "prs.json", `[{"number":7,"url":"https://github.com/acme/widgets/pull/7","state":"OPEN","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.False(t, e.BranchExists)
		assert.Empty(t, e.Head)
		require.NotNil(t, e.PR)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "an open pull request is not a case for a human")
		assert.Contains(t, e.Reason, "PR #7 is open")
		assert.Empty(t, tracker.calls)
		assert.Len(t, f.gh.Invocations(t), 1, "gh is asked even without a local branch")

		// With no pull request left, the same bead does need a human.
		dpSet(t, f.gh, "prs.json", `[]`)
		res, err = cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e = cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.Contains(t, e.Reason, "is gone and nothing landed on main")
	})

	t.Run("recorded pull request that is gone hands the bead to a human", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		f.bead.PRURL, f.bead.PRNumber = "https://github.com/acme/widgets/pull/5", 5
		dpSet(t, f.gh, "prs.json", `[{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"CLOSED","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.True(t, e.Applied)
		assert.Contains(t, e.Reason, "no longer open or merged")
		assert.Equal(t, domain.BeadStateNeedsHuman, tracker.statuses[f.bead.ID])

		// The same for a bead that was still under review when its PR
		// (opened by a retry) was closed.
		f.bead.State = domain.BeadStateInReview
		res, err = cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.Equal(t, domain.BeadStateNeedsHuman, cuResumeEntry(t, res, f.bead.ID).State)
	})

	t.Run("beads without a pull request never ask gh", func(t *testing.T) {
		f := setup(t, domain.BeadStateInReview)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.False(t, cuResumeEntry(t, res, f.bead.ID).Changed())
		assert.Empty(t, f.gh.Invocations(t))
	})

	t.Run("gh missing is a warning, not an error", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpHideGH(t)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "gh is not installed")
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.Contains(t, e.Reason, "deliver again")
		assert.Contains(t, res.Summary(), "warning: pull requests were not checked: gh is not installed")
	})

	t.Run("unauthenticated gh is a warning too", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "mode", "noauth")
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "not authenticated")
	})

	t.Run("a fetch failure is a warning and local state is used", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		cuLand(t, f.wtFixture, f.bead)
		require.NoError(t, os.RemoveAll(f.r.RemoteDir))
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}, WithoutGH()).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.False(t, res.Fetched)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "fetch from origin failed")
		assert.Equal(t, "origin/main", res.Base, "the stale remote-tracking branch is still checked")
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.True(t, e.Landed, "landed on the local main")
		assert.Equal(t, domain.BeadStateClosed, e.State)
	})
}
