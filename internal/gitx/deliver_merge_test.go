package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// dmFixture is a wtFixture with a worktree created for wtBead and a Repo
// opened inside it.
type dmFixture struct {
	*wtFixture
	wt     *Worktree
	wtRepo *Repo
}

// dmSetup builds the fixture (optionally with a bare remote) and creates
// the bead's worktree.
func dmSetup(t *testing.T, fixtureOpts ...testutil.FixtureOption) *dmFixture {
	t.Helper()
	f := wtSetup(t, fixtureOpts)
	wt, err := f.m.Create(context.Background(), wtBead)
	require.NoError(t, err)
	wtRepo, err := f.m.Open(context.Background(), wtBead.ID)
	require.NoError(t, err)
	return &dmFixture{wtFixture: f, wt: wt, wtRepo: wtRepo}
}

// dmBranchCommit writes rel in the worktree and commits everything there.
func (f *dmFixture) dmBranchCommit(t *testing.T, rel, content, message string) string {
	t.Helper()
	acWrite(t, f.wt.Path, rel, content)
	c, err := f.wtRepo.CommitAll(context.Background(), message)
	require.NoError(t, err)
	require.True(t, c.Committed)
	return c.SHA
}

// dmMainCommit writes rel in the main repository and commits it on the
// branch checked out there (main unless a test switched).
func (f *dmFixture) dmMainCommit(t *testing.T, rel, content, message string) string {
	t.Helper()
	f.r.WriteFile(t, rel, content)
	return f.r.Commit(t, message)
}

// dmMain returns the SHA of the local main branch.
func (f *dmFixture) dmMain(t *testing.T) string {
	t.Helper()
	return f.r.Git(t, "rev-parse", "refs/heads/main")
}

// dmRemoteMain returns the SHA of main in the bare remote.
func (f *dmFixture) dmRemoteMain(t *testing.T) string {
	t.Helper()
	return f.r.Git(t, "--git-dir", f.r.RemoteDir, "rev-parse", "refs/heads/main")
}

// dmMerges lists merge commits reachable from main; a delivery must never
// create one.
func (f *dmFixture) dmMerges(t *testing.T) string {
	t.Helper()
	return f.r.Git(t, "rev-list", "--merges", "refs/heads/main")
}

// dmBeads installs a fake bd replaying the captured samples (with
// overrides) and returns a client for the fixture repository.
func dmBeads(t *testing.T, root string, overrides map[string]string) (*beads.Client, *testutil.Shim) {
	t.Helper()
	shim := testutil.FakeBD(t, testutil.StageBDSamples(t, overrides))
	return beads.New(root, beads.Options{Actor: "sc:test", Timeout: 20 * time.Second}), shim
}

// dmPwdCheck returns a check command that records its working directory
// in a file outside the worktree, and the file's path.
func dmPwdCheck(t *testing.T) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pwd.txt")
	return `printf '%s' "$PWD" > "` + out + `"`, out
}

func TestDetectCheckCommand(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
		ok    bool
	}{
		{"nothing", nil, "", false},
		{"makefile with test target", map[string]string{"Makefile": "build:\n\tgo build\n\ntest:\n\tgo test ./...\n"}, "make test", true},
		{"makefile with spaced test target", map[string]string{"GNUmakefile": "test :\n\tpytest\n"}, "make test", true},
		{"makefile without test target falls through", map[string]string{"Makefile": "build:\n\tgo build\n", "go.mod": "module x\n"}, "go test ./...", true},
		{"makefile whose test target is only a prerequisite", map[string]string{"Makefile": "all: test\n", "Cargo.toml": ""}, "cargo test", true},
		{"package.json", map[string]string{"package.json": "{}"}, "npm test", true},
		{"go.mod", map[string]string{"go.mod": "module x\n"}, "go test ./...", true},
		{"pyproject", map[string]string{"pyproject.toml": ""}, "pytest", true},
		{"pytest.ini", map[string]string{"pytest.ini": ""}, "pytest", true},
		{"requirements", map[string]string{"requirements.txt": ""}, "pytest", true},
		{"cargo", map[string]string{"Cargo.toml": ""}, "cargo test", true},
		{"make beats npm beats go", map[string]string{"Makefile": "test:\n\ttrue\n", "package.json": "{}", "go.mod": "module x\n"}, "make test", true},
		{"npm beats go", map[string]string{"package.json": "{}", "go.mod": "module x\n"}, "npm test", true},
		{"go beats pytest and cargo", map[string]string{"go.mod": "module x\n", "setup.py": "", "Cargo.toml": ""}, "go test ./...", true},
		{"directories do not count", map[string]string{"go.mod/": ""}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				if strings.HasSuffix(name, "/") {
					require.NoError(t, os.Mkdir(filepath.Join(dir, strings.TrimSuffix(name, "/")), 0o755))
					continue
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
			}
			got, ok := DetectCheckCommand(dir)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("fixture repo uses make test", func(t *testing.T) {
		got, ok := DetectCheckCommand(testutil.FixtureSourceDir(t))
		assert.True(t, ok)
		assert.Equal(t, "make test", got)
	})
}

func TestDeliveryError(t *testing.T) {
	t.Run("renders the stage and cause", func(t *testing.T) {
		err := &DeliveryError{Stage: StageCheck, Err: ErrCheckFailed}
		assert.Equal(t, "gitx: delivery failed at check: gitx: check command failed", err.Error())
		assert.ErrorIs(t, err, ErrCheckFailed)
	})
	t.Run("renders conflicts with files", func(t *testing.T) {
		err := &DeliveryError{Stage: StageRebase, Conflict: true, Files: []string{"a.go", "b.go"}, Err: ErrRebaseConflict}
		assert.Equal(t, "gitx: delivery failed at rebase: conflicts in a.go, b.go", err.Error())
		assert.ErrorIs(t, err, ErrRebaseConflict)
	})
	t.Run("stageErr keeps git stderr and unwraps the sentinel", func(t *testing.T) {
		ge := &GitError{Args: []string{"push"}, ExitCode: 1, Stderr: "fatal: no\n", Cause: ErrNoRemote}
		err := stageErr(StagePush, ge, "abc")
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StagePush, de.Stage)
		assert.Equal(t, "fatal: no\n", de.Output)
		assert.Equal(t, "abc", de.MergeSHA)
		assert.ErrorIs(t, err, ErrNoRemote)
		var got *GitError
		assert.ErrorAs(t, err, &got)
	})
	t.Run("stageErr keeps the bd message", func(t *testing.T) {
		be := &beads.Error{Command: "close", Kind: beads.ErrBlockedOpen, Message: "blocked by fx-1"}
		err := stageErr(StageClose, be, "abc")
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, "blocked by fx-1", de.Output)
		assert.ErrorIs(t, err, beads.ErrBlockedOpen)
	})
}

func TestTailOf(t *testing.T) {
	assert.Equal(t, "short", tailOf("short", 10))
	assert.Equal(t, "abcdefghij", tailOf("abcdefghij", 10))
	got := tailOf("line1\nline2\nline3\n", 9)
	assert.Equal(t, "[... output truncated ...]\nline3\n", got, "cut at a line boundary")
	got = tailOf(strings.Repeat("x", 100), 10)
	assert.Equal(t, "[... output truncated ...]\n"+strings.Repeat("x", 10), got)
	assert.Equal(t, "", tailOf("", 5))
}

func TestManager_DeliverMerge(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path without a remote fast-forwards main and closes the bead", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		sha := f.dmBranchCommit(t, "stats/fix.txt", "fixed\n", "Fix the mean bug")
		client, bd := dmBeads(t, f.repo.Root, nil)
		check, pwdFile := dmPwdCheck(t)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: check, Beads: client})
		require.NoError(t, err)

		assert.Equal(t, wtBead.ID, res.BeadID)
		assert.Equal(t, "sc/fx-a0u-fix-the-mean-bug", res.Branch)
		assert.Equal(t, "main", res.Target)
		assert.Empty(t, res.Remote)
		assert.Equal(t, before, res.Before)
		assert.Equal(t, sha, res.Head, "the branch tip lands unchanged")
		assert.Equal(t, 1, res.Commits)
		assert.False(t, res.Rebased)
		assert.False(t, res.Pushed)
		assert.True(t, res.Closed)

		// The default branch is fast-forwarded, never merged.
		assert.Equal(t, sha, f.dmMain(t))
		assert.Empty(t, f.dmMerges(t), "no merge commit on main")
		assert.Equal(t, []string{"Fix the mean bug", "Initial import of fixture project"}, f.r.Log(t))
		assert.FileExists(t, filepath.Join(f.r.Dir, "stats", "fix.txt"), "the checked-out main working tree follows")
		assert.Empty(t, f.r.Status(t), "the main working tree stays clean")
		assert.Equal(t, "main", f.r.Git(t, "symbolic-ref", "--short", "HEAD"))

		// The check ran in the worktree.
		require.NotNil(t, res.Check)
		assert.Equal(t, check, res.Check.Command)
		assert.False(t, res.Check.Detected)
		assert.Equal(t, 0, res.Check.ExitCode)
		pwd, err := os.ReadFile(pwdFile)
		require.NoError(t, err)
		repoSamePath(t, f.wt.Path, string(pwd))

		// bd recorded the merge and closed the bead (D-20).
		calls := bd.ArgLists(t)
		require.Len(t, calls, 2)
		assert.Equal(t, []string{"-C", f.repo.Root}, calls[0][:2], "every bd call targets the repository")
		assert.Contains(t, calls[0], "update")
		assert.Contains(t, calls[0], "fx-a0u")
		assert.Contains(t, calls[0], "--metadata")
		assert.Contains(t, calls[1], "close")
		assert.Contains(t, calls[1], "fx-a0u")
		assert.Contains(t, calls[1], "--reason")
		assert.Contains(t, calls[1], "merged into main as "+sha[:12])
		assert.Empty(t, testutil.MissingSamples(t, bd))
	})

	t.Run("custom close reason and detected check command", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeads(t, f.repo.Root, nil)
		// The fixture has a Makefile with a test target; make it cheap.
		acWrite(t, f.wt.Path, "Makefile", "test:\n\t@echo detected-check-ran\n")
		f.dmBranchCommit(t, "note2.txt", "n\n", "Trim Makefile")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{Beads: client, CloseReason: "done in round 2"})
		require.NoError(t, err)
		require.NotNil(t, res.Check)
		assert.Equal(t, "make test", res.Check.Command)
		assert.True(t, res.Check.Detected)
		assert.Contains(t, res.Check.Output, "detected-check-ran")
		assert.Equal(t, 2, res.Commits)
		calls := bd.ArgLists(t)
		require.Len(t, calls, 2)
		assert.Contains(t, calls[1], "done in round 2")
	})

	t.Run("no beads client skips the close stage", func(t *testing.T) {
		f := dmSetup(t)
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.False(t, res.Closed)
		assert.Equal(t, sha, f.dmMain(t))
	})

	t.Run("happy path with a remote pushes main", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, _ := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.Equal(t, "origin", res.Remote)
		assert.True(t, res.Pushed)
		assert.True(t, res.Closed)
		assert.Equal(t, sha, f.dmMain(t))
		assert.Equal(t, sha, f.dmRemoteMain(t))
		assert.Empty(t, f.dmMerges(t))
	})

	t.Run("rebases onto a default branch that moved and checks the rebased tree", func(t *testing.T) {
		f := dmSetup(t)
		tip := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		moved := f.dmMainCommit(t, "from-main.txt", "m\n", "Something landed on main")

		// The check only passes on the rebased tree, which has both files.
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "test -f from-main.txt && test -f note.txt"})
		require.NoError(t, err)
		assert.True(t, res.Rebased)
		assert.NotEqual(t, tip, res.Head)
		assert.Equal(t, moved, res.Before)
		assert.Equal(t, 1, res.Commits)
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, moved, f.r.Git(t, "rev-parse", "main~1"), "the rebased commit sits on top of main's previous tip")
		assert.Empty(t, f.dmMerges(t))
		assert.Equal(t, []string{"Add note", "Something landed on main", "Initial import of fixture project"}, f.r.Log(t))
		assert.Equal(t, res.Head, f.r.Git(t, "rev-parse", res.Branch), "the branch itself was rebased")
	})

	t.Run("failing check stops before the merge", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "echo boom-out; echo boom-err >&2; exit 3", Beads: client})
		require.Error(t, err)
		assert.Nil(t, res)
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageCheck, de.Stage)
		assert.False(t, de.Conflict)
		assert.Empty(t, de.MergeSHA)
		assert.Contains(t, de.Output, "boom-out")
		assert.Contains(t, de.Output, "boom-err")
		assert.ErrorIs(t, err, ErrCheckFailed)
		var ee *proc.ExitError
		require.ErrorAs(t, err, &ee)
		assert.Equal(t, 3, ee.Result.ExitCode)

		assert.Equal(t, before, f.dmMain(t), "main is untouched")
		assert.Empty(t, bd.ArgLists(t), "the bead is not closed")
	})

	t.Run("check timeout", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		start := time.Now()
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "sleep 30", CheckTimeout: 200 * time.Millisecond})
		require.Error(t, err)
		assert.Less(t, time.Since(start), 10*time.Second)
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageCheck, de.Stage)
		assert.ErrorIs(t, err, ErrCheckFailed)
		assert.ErrorIs(t, err, proc.ErrTimeout)
	})

	t.Run("conflict leaves the worktree mid-rebase for the engineer", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
		moved := f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.Error(t, err)
		assert.Nil(t, res)
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageRebase, de.Stage)
		assert.True(t, de.Conflict)
		assert.Equal(t, []string{"README.md"}, de.Files)
		assert.ErrorIs(t, err, ErrRebaseConflict)
		assert.Contains(t, err.Error(), "conflicts in README.md")

		// The conflict state is left for the engineer's conflict round.
		assert.True(t, f.wtRepo.RebaseInProgress())
		files, ferr := f.wtRepo.ConflictedPaths(ctx)
		require.NoError(t, ferr)
		assert.Equal(t, []string{"README.md"}, files)
		data, rerr := os.ReadFile(filepath.Join(f.wt.Path, "README.md"))
		require.NoError(t, rerr)
		assert.Contains(t, string(data), "<<<<<<<")

		// Nothing happened to main or to the bead.
		assert.Equal(t, moved, f.dmMain(t))
		assert.Empty(t, f.dmMerges(t))
		assert.Empty(t, bd.ArgLists(t))

		// A retry without resolution reports the same conflict again.
		_, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.ErrorAs(t, err, &de)
		assert.True(t, de.Conflict)
		assert.Equal(t, []string{"README.md"}, de.Files)
		assert.True(t, f.wtRepo.RebaseInProgress())

		// The engineer resolves and stages; the next delivery finishes the
		// rebase itself and lands the work.
		acWrite(t, f.wt.Path, "README.md", "resolved version\n")
		_, gerr := f.wtRepo.Git(ctx, "add", "README.md")
		require.NoError(t, gerr)
		res, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.False(t, f.wtRepo.RebaseInProgress())
		assert.True(t, res.Rebased)
		assert.True(t, res.Closed)
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, moved, f.r.Git(t, "rev-parse", "main~1"))
		assert.Empty(t, f.dmMerges(t))
		assert.Equal(t, "resolved version\n", f.r.Git(t, "show", "main:README.md")+"\n")
		assert.Len(t, bd.ArgLists(t), 2)
	})

	t.Run("conflict resolved and continued by the engineer", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
		f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.ErrorIs(t, err, ErrRebaseConflict)

		acWrite(t, f.wt.Path, "README.md", "resolved version\n")
		_, err = f.wtRepo.Git(ctx, "add", "README.md")
		require.NoError(t, err)
		_, err = f.wtRepo.Git(ctx, "rebase", "--continue")
		require.NoError(t, err)
		require.False(t, f.wtRepo.RebaseInProgress())

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.False(t, res.Rebased, "the engineer already rebased")
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Empty(t, f.dmMerges(t))
	})

	t.Run("default branch not checked out in the main repository", func(t *testing.T) {
		f := dmSetup(t)
		f.r.Git(t, "checkout", "-q", "-b", "users-own-work")
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Equal(t, sha, res.Head)
		assert.Equal(t, sha, f.dmMain(t), "main moved without being checked out")
		assert.Equal(t, "users-own-work", f.r.Git(t, "symbolic-ref", "--short", "HEAD"), "the user's checkout is untouched")
		assert.NoFileExists(t, filepath.Join(f.r.Dir, "note.txt"))
		assert.Empty(t, f.dmMerges(t))
	})

	t.Run("refuses when the default branch is checked out in another worktree", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.r.Git(t, "checkout", "-q", "-b", "users-own-work")
		dir := filepath.Join(t.TempDir(), "main-elsewhere")
		f.r.Git(t, "worktree", "add", "-q", dir, "main")
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageMerge, de.Stage)
		assert.Contains(t, de.Output, "checked out")
		assert.Equal(t, before, f.dmMain(t))
	})

	t.Run("syncs a default branch that is behind its remote", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		// Someone else pushed to main: simulate by pushing then rewinding.
		remoteTip := f.dmMainCommit(t, "elsewhere.txt", "e\n", "Pushed by a colleague")
		f.r.Git(t, "push", "-q", "origin", "main")
		f.r.Git(t, "reset", "-q", "--hard", "HEAD~1")
		require.NotEqual(t, remoteTip, f.dmMain(t))

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "test -f elsewhere.txt"})
		require.NoError(t, err)
		assert.Equal(t, remoteTip, res.Before, "main was fast-forwarded to the remote first")
		assert.True(t, res.Rebased)
		assert.True(t, res.Pushed)
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, res.Head, f.dmRemoteMain(t))
		assert.Equal(t, remoteTip, f.r.Git(t, "rev-parse", "main~1"))
		assert.Empty(t, f.dmMerges(t))
		assert.FileExists(t, filepath.Join(f.r.Dir, "elsewhere.txt"), "the checked-out main follows the sync")
	})

	t.Run("default branch that exists only on the remote is pushed directly", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		f.r.Git(t, "remote", "set-head", "origin", "main")
		f.r.Git(t, "checkout", "-q", "-b", "users-own-work")
		f.r.Git(t, "branch", "-q", "-D", "main")
		remoteTip := f.dmRemoteMain(t)
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.Equal(t, "main", res.Target)
		assert.Equal(t, remoteTip, res.Before)
		assert.Equal(t, sha, res.Head)
		assert.True(t, res.Pushed)
		assert.True(t, res.Closed)
		assert.Equal(t, sha, f.dmRemoteMain(t))
		assert.Equal(t, sha, f.r.Git(t, "rev-parse", "refs/remotes/origin/main"), "the tracking ref follows the push")
		exists, err := f.repo.BranchExists(ctx, "main")
		require.NoError(t, err)
		assert.False(t, exists, "no local main is created")
		assert.Len(t, bd.ArgLists(t), 2)
	})

	t.Run("local default branch ahead of the remote is pushed along", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		local := f.dmMainCommit(t, "local.txt", "l\n", "Unpushed local commit")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Equal(t, local, res.Before)
		assert.Equal(t, res.Head, f.dmRemoteMain(t))
	})

	t.Run("diverged default branch fails at the fetch stage", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		f.dmMainCommit(t, "elsewhere.txt", "e\n", "Pushed by a colleague")
		f.r.Git(t, "push", "-q", "origin", "main")
		f.r.Git(t, "reset", "-q", "--hard", "HEAD~1")
		local := f.dmMainCommit(t, "local.txt", "l\n", "Unpushed local commit")

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageFetch, de.Stage)
		assert.ErrorIs(t, err, ErrDefaultBranchDiverged)
		assert.Equal(t, local, f.dmMain(t), "main is left alone")
	})

	t.Run("unreachable remote fails at the fetch stage", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.r.Git(t, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageFetch, de.Stage)
		var ge *GitError
		assert.ErrorAs(t, err, &ge)
		assert.NotEmpty(t, de.Output)
		assert.Equal(t, before, f.dmMain(t))
	})

	t.Run("push failure reports the local merge", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		f.r.Git(t, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "missing.git"))
		client, bd := dmBeads(t, f.repo.Root, nil)

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StagePush, de.Stage)
		assert.Equal(t, sha, de.MergeSHA)
		assert.Equal(t, sha, f.dmMain(t), "the local merge happened")
		assert.NotEqual(t, sha, f.dmRemoteMain(t))
		assert.Empty(t, bd.ArgLists(t), "the bead is not closed until the push lands")
	})

	t.Run("close refusal surfaces the bd error after the merge", func(t *testing.T) {
		f := dmSetup(t)
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeads(t, f.repo.Root, map[string]string{"close": "error-close-blocked"})

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		assert.Nil(t, res)
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageClose, de.Stage)
		assert.Equal(t, sha, de.MergeSHA)
		assert.ErrorIs(t, err, beads.ErrBlockedOpen)
		assert.NotEmpty(t, de.Output)
		assert.Equal(t, sha, f.dmMain(t))
		assert.Len(t, bd.ArgLists(t), 2)
	})

	t.Run("merge refused by conflicting uncommitted changes in the main checkout", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README")
		f.r.WriteFile(t, "README.md", "user is editing this\n")

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageMerge, de.Stage)
		assert.Equal(t, before, f.dmMain(t))
		assert.Equal(t, "user is editing this\n", func() string {
			data, err := os.ReadFile(filepath.Join(f.r.Dir, "README.md"))
			require.NoError(t, err)
			return string(data)
		}(), "the user's edit survives")
	})

	t.Run("branch without commits closes without touching main", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		client, bd := dmBeads(t, f.repo.Root, nil)
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.Equal(t, 0, res.Commits)
		assert.Equal(t, before, res.Head)
		assert.Equal(t, before, f.dmMain(t))
		assert.True(t, res.Closed)
		assert.Len(t, bd.ArgLists(t), 2)
	})

	t.Run("no check command runs when nothing is configured or detected", func(t *testing.T) {
		f := dmSetup(t)
		_, err := f.wtRepo.Git(ctx, "rm", "-q", "Makefile", "go.mod")
		require.NoError(t, err)
		_, err = f.wtRepo.CommitAll(ctx, "Drop build files")
		require.NoError(t, err)
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{})
		require.NoError(t, err)
		assert.Nil(t, res.Check)
		assert.Equal(t, res.Head, f.dmMain(t))
	})

	t.Run("missing worktree", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{})
		assert.ErrorIs(t, err, ErrWorktreeMissing)
	})

	t.Run("empty bead id", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.DeliverMerge(ctx, domain.Bead{Title: "no id"}, MergeOptions{})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrWorktreeMissing)
	})

	t.Run("worktree on the wrong branch", func(t *testing.T) {
		f := dmSetup(t)
		_, err := f.wtRepo.Git(ctx, "checkout", "-q", "-b", "somewhere-else")
		require.NoError(t, err)
		_, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		assert.ErrorIs(t, err, ErrWrongBranch)
	})

	t.Run("dirty worktree is refused by the rebase", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		acWrite(t, f.wt.Path, "note.txt", "uncommitted edit\n")
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageRebase, de.Stage)
		assert.False(t, de.Conflict)
		assert.Equal(t, before, f.dmMain(t))
	})

	t.Run("check output is capped", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "yes | head -c 100000"})
		require.NoError(t, err)
		require.NotNil(t, res.Check)
		assert.LessOrEqual(t, len(res.Check.Output), MaxCheckOutput+len("[... output truncated ...]\n"))
		assert.True(t, strings.HasPrefix(res.Check.Output, "[... output truncated ...]\n"))
	})
}

func TestRepo_RebaseHelpers(t *testing.T) {
	ctx := context.Background()
	f := dmSetup(t)
	assert.False(t, f.wtRepo.RebaseInProgress())
	assert.False(t, f.repo.RebaseInProgress())
	files, err := f.wtRepo.ConflictedPaths(ctx)
	require.NoError(t, err)
	assert.Nil(t, files)

	f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
	f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
	_, err = f.wtRepo.Git(ctx, "rebase", "main")
	require.Error(t, err)
	assert.True(t, f.wtRepo.RebaseInProgress(), "the linked worktree's own git dir holds the rebase state")
	assert.False(t, f.repo.RebaseInProgress(), "the main working tree is not rebasing")
	files, err = f.wtRepo.ConflictedPaths(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md"}, files)

	_, err = f.wtRepo.Git(ctx, "rebase", "--abort")
	require.NoError(t, err)
	assert.False(t, f.wtRepo.RebaseInProgress())
}
