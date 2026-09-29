package gitx

import (
	"context"
	"fmt"
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

// dmSetup builds the fixture (optionally with a bare remote), creates the
// bead's worktree, and registers the delivery invariants every subtest must
// leave intact (see dmAssertDefaultBranchSafe).
func dmSetup(t *testing.T, fixtureOpts ...testutil.FixtureOption) *dmFixture {
	t.Helper()
	f := wtSetup(t, fixtureOpts)
	wt, err := f.m.Create(context.Background(), wtBead)
	require.NoError(t, err)
	wtRepo, err := f.m.Open(context.Background(), wtBead.ID)
	require.NoError(t, err)
	d := &dmFixture{wtFixture: f, wt: wt, wtRepo: wtRepo}
	start := d.dmMain(t)
	t.Cleanup(func() { d.dmAssertDefaultBranchSafe(t, start) })
	return d
}

// dmAssertDefaultBranchSafe checks the two things a safety-off delivery may
// never do, whatever the subtest did: put a merge commit on the default
// branch (D-10; checked with "git log --merges" locally and on the remote)
// and move the default branch backwards. It runs after every subtest, so
// the guarantee is not something an individual test has to remember.
func (f *dmFixture) dmAssertDefaultBranchSafe(t *testing.T, start string) {
	t.Helper()
	if f.dmRefExists(t, "refs/heads/main") {
		assert.Empty(t, f.r.Git(t, "log", "--merges", "--oneline", "refs/heads/main"),
			"the default branch never receives a merge commit (D-10)")
		assert.Equal(t, "0", f.r.Git(t, "rev-list", "--count", "refs/heads/main.."+start),
			"the default branch only ever moves forward")
	}
	if f.r.RemoteDir != "" && f.dmRefExists(t, "refs/heads/main", "--git-dir", f.r.RemoteDir) {
		assert.Empty(t, f.r.Git(t, "--git-dir", f.r.RemoteDir, "log", "--merges", "--oneline", "refs/heads/main"),
			"the pushed default branch never receives a merge commit (D-10)")
	}
}

// dmRefExists reports whether ref resolves, without failing the test when
// it does not. Extra git arguments (such as --git-dir) come before the
// subcommand.
func (f *dmFixture) dmRefExists(t *testing.T, ref string, gitArgs ...string) bool {
	t.Helper()
	args := append(append([]string{}, gitArgs...), "for-each-ref", "--format=%(refname)", ref)
	return f.r.Git(t, args...) != ""
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

// dmMerges lists the merge commits reachable from main; a delivery must
// never put one there.
func (f *dmFixture) dmMerges(t *testing.T) string {
	t.Helper()
	return f.r.Git(t, "log", "--merges", "--oneline", "refs/heads/main")
}

// dmBeads installs a fake bd replaying the captured samples (with
// overrides) and returns a client for the fixture repository.
func dmBeads(t *testing.T, root string, overrides map[string]string) (*beads.Client, *testutil.Shim) {
	t.Helper()
	return dmBeadsWith(t, root, overrides, nil)
}

// dmBeadsWith is dmBeads with extra sample files written into the staged
// directory (name to content), for replies the captured set cannot express
// because they carry a SHA the test only learns at run time.
func dmBeadsWith(t *testing.T, root string, overrides, extra map[string]string) (*beads.Client, *testutil.Shim) {
	t.Helper()
	dir := testutil.StageBDSamples(t, overrides)
	for name, content := range extra {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	shim := testutil.FakeBD(t, dir)
	return beads.New(root, beads.Options{Actor: "sc:test", Timeout: 20 * time.Second}), shim
}

// dmShowJSON renders a `bd show --json` reply for one bead carrying the
// sc_merge_sha metadata an earlier delivery attempt recorded.
func dmShowJSON(id domain.BeadID, mergeSHA string) string {
	return fmt.Sprintf(`{"schema_version":1,"data":[{"id":%q,"title":"Fix the mean bug","status":"delivering","priority":1,`+
		`"issue_type":"task","metadata":{"sc_run":"r1","sc_merge_sha":%q}}]}`, id, mergeSHA)
}

// dmCalled reports how many of the recorded bd calls name subcommand.
func dmCalled(t *testing.T, shim *testutil.Shim, subcommand string) int {
	t.Helper()
	n := 0
	for _, args := range shim.ArgLists(t) {
		for _, a := range args {
			if a == subcommand {
				n++
				break
			}
		}
	}
	return n
}

// dmStage returns the stage of the DeliveryError err carries, failing the
// test when it is not one.
func dmStage(t *testing.T, err error) DeliveryStage {
	t.Helper()
	var de *DeliveryError
	require.ErrorAs(t, err, &de, "every delivery failure is a *DeliveryError")
	return de.Stage
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
		{"makefile test variable is not a target", map[string]string{"Makefile": "test := $(GO) test ./...\nbuild:\n\t$(test)\n", "go.mod": "module x\n"}, "go test ./...", true},
		{"makefile test variable without spaces is not a target", map[string]string{"Makefile": "test:=1\n", "Cargo.toml": ""}, "cargo test", true},
		{"makefile posix ::= assignment is not a target", map[string]string{"Makefile": "test ::= 1\n"}, "", false},
		{"makefile :::= assignment is not a target", map[string]string{"Makefile": "test :::= 1\n"}, "", false},
		{"makefile += assignment is not a target", map[string]string{"Makefile": "test += 1\n"}, "", false},
		{"makefile ?= assignment is not a target", map[string]string{"Makefile": "test ?= 1\n"}, "", false},
		{"makefile double-colon test target counts", map[string]string{"Makefile": "test::\n\tgo test ./...\n"}, "make test", true},
		{"makefile test target with prerequisites counts", map[string]string{"Makefile": "test: build\n\tgo test ./...\n"}, "make test", true},
		{"makefile phony declaration alone is not a target", map[string]string{"Makefile": ".PHONY: test\n"}, "", false},
		{"makefile tests target is not a test target", map[string]string{"Makefile": "tests:\n\tgo test ./...\n"}, "", false},
		{"package.json with a test script", map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`}, "npm test", true},
		{"package.json without a test script falls through", map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`, "go.mod": "module x\n"}, "go test ./...", true},
		{"package.json without scripts at all", map[string]string{"package.json": "{}"}, "", false},
		{"package.json with a blank test script", map[string]string{"package.json": `{"scripts":{"test":"  "}}`}, "", false},
		{"malformed package.json", map[string]string{"package.json": "{oops"}, "", false},
		{"go.mod", map[string]string{"go.mod": "module x\n"}, "go test ./...", true},
		{"pyproject", map[string]string{"pyproject.toml": ""}, "pytest", true},
		{"pytest.ini", map[string]string{"pytest.ini": ""}, "pytest", true},
		{"requirements", map[string]string{"requirements.txt": ""}, "pytest", true},
		{"cargo", map[string]string{"Cargo.toml": ""}, "cargo test", true},
		{"make beats npm beats go", map[string]string{"Makefile": "test:\n\ttrue\n", "package.json": `{"scripts":{"test":"jest"}}`, "go.mod": "module x\n"}, "make test", true},
		{"npm beats go", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "go.mod": "module x\n"}, "npm test", true},
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

	t.Run("the candidates are the P4-06 list, in order", func(t *testing.T) {
		var got []string
		for _, c := range checkCandidates {
			got = append(got, c.command)
		}
		assert.Equal(t, []string{"make test", "npm test", "go test ./...", "pytest", "cargo test"}, got)
	})

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
	t.Run("stageErr keeps the innermost stage", func(t *testing.T) {
		inner := &DeliveryError{Stage: StageRebase, Conflict: true, Err: ErrRebaseConflict}
		err := stageErr(StageMerge, fmt.Errorf("wrapped: %w", inner), "")
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageRebase, de.Stage, "an already staged error is never restaged")
		assert.True(t, de.Conflict)
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

		// The whole CheckResult travels with the error, so a caller that
		// only has the error can still report what ran (P6-06).
		require.NotNil(t, de.Check, "the failing check reaches the caller")
		assert.Equal(t, "echo boom-out; echo boom-err >&2; exit 3", de.Check.Command)
		assert.False(t, de.Check.Detected, "it was configured, not autodetected")
		assert.Equal(t, 3, de.Check.ExitCode)
		assert.Positive(t, de.Check.Duration)
		assert.Equal(t, de.Output, de.Check.Output)

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
		assert.NotEmpty(t, de.Output, "git's own words reach the engineer")

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
		assert.NotEmpty(t, de.Output, "git's refusal reaches the caller")
		assert.Equal(t, before, f.dmMain(t))
		assert.Equal(t, "user is editing this\n", func() string {
			data, err := os.ReadFile(filepath.Join(f.r.Dir, "README.md"))
			require.NoError(t, err)
			return string(data)
		}(), "the user's edit survives")
	})

	// D-20: a bead is closed only when its code has landed. An empty sc/
	// branch is trivially an ancestor of the default branch, so ancestry
	// alone would close the bead and stamp sc_merge_sha with a commit the
	// bead never produced, unblocking its dependents (the P4-13 defect).
	t.Run("branch without commits is refused instead of closing the bead", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		assert.Nil(t, res)
		require.ErrorIs(t, err, ErrNothingToDeliver)
		assert.Equal(t, StageMerge, dmStage(t, err))
		assert.Equal(t, before, f.dmMain(t), "main is untouched")
		assert.Zero(t, dmCalled(t, bd, "close"), "the bead stays open")
		assert.Zero(t, dmCalled(t, bd, "update"), "and unstamped")
	})

	t.Run("branch without commits is refused against its recorded base", func(t *testing.T) {
		f := dmSetup(t)
		base := f.dmMain(t)
		// The default branch moves on, so the empty branch's rebase lands
		// it on a tip it did not produce.
		f.dmMainCommit(t, "from-main.txt", "m\n", "Something landed on main")
		moved := f.dmMain(t)
		client, bd := dmBeads(t, f.repo.Root, nil)

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client, BaseSHA: base})
		require.ErrorIs(t, err, ErrNothingToDeliver)
		assert.Equal(t, StageMerge, dmStage(t, err))
		assert.Equal(t, moved, f.dmMain(t))
		assert.Empty(t, bd.ArgLists(t), "the recorded base settles it without asking bd")
	})

	t.Run("branch without commits is refused with no bead client and no base", func(t *testing.T) {
		f := dmSetup(t)
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.ErrorIs(t, err, ErrNothingToDeliver)
		assert.Equal(t, StageMerge, dmStage(t, err))
	})

	t.Run("an empty branch whose bead's commit is already on main closes the bead", func(t *testing.T) {
		f := dmSetup(t)
		f.dmMainCommit(t, "pause.txt", "p\n", "sc("+string(wtBead.ID)+"): Give the pause button a bigger tap area")
		landed := f.dmMain(t)
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.True(t, res.AlreadyLanded)
		assert.True(t, res.Closed)
		assert.Equal(t, landed, f.dmMain(t), "main did not move")
		assert.Equal(t, 1, dmCalled(t, bd, "close"))
	})

	t.Run("a commit naming another bead does not count as this bead landing", func(t *testing.T) {
		f := dmSetup(t)
		f.dmMainCommit(t, "other.txt", "o\n", "sc("+string(wtBead.ID)+"x): Someone else's work")
		f.dmMainCommit(t, "mention.txt", "m\n", "Fix a test, see sc("+string(wtBead.ID)+"): in the log")
		client, bd := dmBeads(t, f.repo.Root, nil)

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.ErrorIs(t, err, ErrNothingToDeliver)
		assert.Zero(t, dmCalled(t, bd, "close"), "the bead stays open")
	})

	// The zero-commit path is what makes a delivery idempotent: the merge
	// and the push landed, only bd close failed. The recorded base tells
	// that apart from a branch that never received a commit.
	t.Run("a retry after the merge landed closes the bead without redelivering", func(t *testing.T) {
		f := dmSetup(t)
		base := f.dmMain(t)
		sha := f.dmBranchCommit(t, "stats/fix.txt", "fixed\n", "Fix the mean bug")
		client, bd := dmBeads(t, f.repo.Root, map[string]string{"close.1": "error-close-blocked"})

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client, BaseSHA: base})
		require.ErrorIs(t, err, beads.ErrBlockedOpen)
		require.Equal(t, sha, f.dmMain(t), "the merge landed before the close was refused")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client, BaseSHA: base})
		require.NoError(t, err)
		assert.Equal(t, 0, res.Commits)
		assert.True(t, res.AlreadyLanded)
		assert.True(t, res.Closed)
		assert.Equal(t, sha, res.Head)
		assert.Equal(t, sha, f.dmMain(t), "main did not move again")
		assert.Equal(t, 2, dmCalled(t, bd, "close"), "the close is the only stage retried")
		assert.Zero(t, dmCalled(t, bd, "show"), "the recorded base needs no bd call")
	})

	t.Run("a retry finishes on the sc_merge_sha the first attempt recorded", func(t *testing.T) {
		f := dmSetup(t)
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeadsWith(t, f.repo.Root,
			map[string]string{"close.1": "error-close-blocked"},
			map[string]string{"show.json": dmShowJSON(wtBead.ID, sha)})

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.ErrorIs(t, err, beads.ErrBlockedOpen)

		// No BaseSHA this time: the sc_merge_sha the first attempt stamped
		// on the bead is the proof that the code landed.
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		require.NoError(t, err)
		assert.True(t, res.AlreadyLanded)
		assert.True(t, res.Closed)
		assert.Equal(t, sha, f.dmMain(t))
		assert.Equal(t, 1, dmCalled(t, bd, "show"))
	})

	t.Run("a retry pushes a merge that landed only locally", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		base := f.dmMain(t)
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		f.r.Git(t, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "missing.git"))
		client, bd := dmBeads(t, f.repo.Root, nil)

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client, BaseSHA: base})
		require.Error(t, err)
		require.Equal(t, StagePush, dmStage(t, err))
		require.NotEqual(t, sha, f.dmRemoteMain(t))
		require.Empty(t, bd.ArgLists(t), "the bead is not touched while the push is missing")

		f.r.Git(t, "remote", "set-url", "--push", "origin", f.r.RemoteDir)
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client, BaseSHA: base})
		require.NoError(t, err)
		assert.True(t, res.AlreadyLanded)
		assert.True(t, res.Pushed, "the stage that failed is retried")
		assert.True(t, res.Closed)
		assert.Equal(t, sha, f.dmRemoteMain(t))
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
		assert.Equal(t, StagePrepare, dmStage(t, err), "even setup failures name a stage")
	})

	t.Run("empty bead id", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.DeliverMerge(ctx, domain.Bead{Title: "no id"}, MergeOptions{})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrWorktreeMissing)
		assert.Equal(t, StagePrepare, dmStage(t, err))
	})

	// P4-12: the bead was retitled after its branch was cut, so BranchName
	// names a branch that does not exist. The safety-off delivery must
	// still find the work.
	t.Run("a retitled bead delivers the branch its work is on", func(t *testing.T) {
		f := dmSetup(t)
		sha := f.dmBranchCommit(t, "stats/notes.md", "work\n", "Add notes")
		retitled := wtBead
		retitled.Title = "a much better title the PO thought of later"
		require.NotEqual(t, f.wt.Branch, BranchName(retitled))

		res, err := f.m.DeliverMerge(ctx, retitled, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Equal(t, f.wt.Branch, res.Branch)
		assert.Equal(t, sha, res.Head)
		assert.Equal(t, 1, res.Commits)
		assert.Equal(t, sha, f.dmMain(t), "the work landed on the default branch")
	})

	t.Run("worktree on the wrong branch", func(t *testing.T) {
		f := dmSetup(t)
		_, err := f.wtRepo.Git(ctx, "checkout", "-q", "-b", "somewhere-else")
		require.NoError(t, err)
		_, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		assert.ErrorIs(t, err, ErrWrongBranch)
		assert.Equal(t, StageRebase, dmStage(t, err))
		assert.Empty(t, f.dmMerges(t))
	})

	t.Run("detached worktree head is refused before anything moves", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		_, err := f.wtRepo.Git(ctx, "checkout", "-q", "--detach")
		require.NoError(t, err)
		_, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		assert.ErrorIs(t, err, ErrWrongBranch)
		assert.Equal(t, StageRebase, dmStage(t, err))
		assert.Equal(t, before, f.dmMain(t))
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
		assert.NotEmpty(t, de.Output)
		assert.Equal(t, before, f.dmMain(t))
	})

	t.Run("the check command runs with the full environment", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		t.Setenv("SC_DM_MARKER", "present")
		out := filepath.Join(t.TempDir(), "env.txt")
		command := fmt.Sprintf(`printf '%%s|%%s' "$SC_DM_MARKER" "$HOME" > "%s"`, out)
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: command})
		require.NoError(t, err)
		require.NotNil(t, res.Check)
		got, err := os.ReadFile(out)
		require.NoError(t, err)
		marker, home, ok := strings.Cut(string(got), "|")
		require.True(t, ok)
		assert.Equal(t, "present", marker, "the check inherits the process environment")
		assert.Equal(t, repoTestEnv(f.r)["HOME"], home, "the repository's explicit variables win")
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

	t.Run("a timed out check still reports what it printed", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{
			CheckCommand: "echo partial-output; sleep 30",
			CheckTimeout: 300 * time.Millisecond,
		})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageCheck, de.Stage)
		assert.ErrorIs(t, err, proc.ErrTimeout)
		assert.Contains(t, de.Output, "partial-output", "the tail of a killed check is still reported")
		require.NotNil(t, de.Check)
		assert.Equal(t, "echo partial-output; sleep 30", de.Check.Command)
		assert.Equal(t, -1, de.Check.ExitCode, "a killed check has no exit status")
	})

	t.Run("a failing autodetected check says it was autodetected", func(t *testing.T) {
		f := dmSetup(t)
		acWrite(t, f.wt.Path, "Makefile", "test:\n\t@echo detected-boom; exit 4\n")
		f.dmBranchCommit(t, "Makefile", "test:\n\t@echo detected-boom; exit 4\n", "Trim Makefile")

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageCheck, de.Stage)
		require.NotNil(t, de.Check)
		assert.Equal(t, "make test", de.Check.Command)
		assert.True(t, de.Check.Detected, "the caller can say where the command came from")
		assert.Contains(t, de.Check.Output, "detected-boom")
	})

	t.Run("a blank check command falls back to detection", func(t *testing.T) {
		f := dmSetup(t)
		acWrite(t, f.wt.Path, "Makefile", "test:\n\t@echo blank-fallback\n")
		f.dmBranchCommit(t, "Makefile", "test:\n\t@echo blank-fallback\n", "Trim Makefile")
		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "  \t\n "})
		require.NoError(t, err)
		require.NotNil(t, res.Check)
		assert.Equal(t, "make test", res.Check.Command)
		assert.True(t, res.Check.Detected)
		assert.Contains(t, res.Check.Output, "blank-fallback")
	})

	// The five stages in order: the check must see the rebased tree while
	// the default branch, local and remote, is still untouched.
	t.Run("the check runs after the rebase and before the merge and push", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		moved := f.dmMainCommit(t, "from-main.txt", "m\n", "Something landed on main")
		f.r.Git(t, "push", "-q", "origin", "main")
		out := filepath.Join(t.TempDir(), "order.txt")
		command := fmt.Sprintf(`{ git rev-parse HEAD; git -C %q rev-parse refs/heads/main; git --git-dir=%q rev-parse refs/heads/main; } > %q`,
			f.r.Dir, f.r.RemoteDir, out)
		client, bd := dmBeads(t, f.repo.Root, nil)

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: command, Beads: client})
		require.NoError(t, err)

		data, rerr := os.ReadFile(out)
		require.NoError(t, rerr)
		seen := strings.Fields(string(data))
		require.Len(t, seen, 3)
		assert.Equal(t, res.Head, seen[0], "the check ran on the rebased tip")
		assert.Equal(t, moved, seen[1], "the local default branch had not moved yet")
		assert.Equal(t, moved, seen[2], "nothing had been pushed yet")
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, res.Head, f.dmRemoteMain(t))
		assert.Len(t, bd.ArgLists(t), 2, "the bead is closed last")
	})

	t.Run("the bead is closed only after the push landed", func(t *testing.T) {
		f := dmSetup(t, testutil.WithBareRemote())
		sha := f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		client, bd := dmBeads(t, f.repo.Root, map[string]string{"close": "error-close-blocked"})

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true", Beads: client})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageClose, de.Stage)
		assert.Equal(t, sha, de.MergeSHA)
		assert.Equal(t, sha, f.dmMain(t), "the merge came first")
		assert.Equal(t, sha, f.dmRemoteMain(t), "and the push before the close")
		assert.Len(t, bd.ArgLists(t), 2)
	})

	t.Run("a default branch that moves during the check stops the delivery", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		command := fmt.Sprintf(`git -C %q commit -q --allow-empty -m "A human commit"`, f.r.Dir)
		client, bd := dmBeads(t, f.repo.Root, nil)

		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: command, Beads: client})
		var de *DeliveryError
		require.ErrorAs(t, err, &de)
		assert.Equal(t, StageMerge, de.Stage)
		assert.ErrorIs(t, err, ErrNotFastForward)
		assert.Equal(t, "A human commit", f.r.Log(t)[0], "the human's commit is still the tip")
		assert.Empty(t, bd.ArgLists(t), "the bead is not closed")
	})

	// A merge commit inside the bead's branch must not ride onto the
	// default branch on the fast-forward, not even when the repository
	// asks git to preserve merges while rebasing.
	t.Run("a merge commit in the branch never reaches the default branch", func(t *testing.T) {
		f := dmSetup(t)
		f.r.Git(t, "config", "rebase.rebaseMerges", "true")
		f.dmBranchCommit(t, "note.txt", "n\n", "Add note")
		_, err := f.wtRepo.Git(ctx, "checkout", "-q", "-b", "engineer-side")
		require.NoError(t, err)
		f.dmBranchCommit(t, "side.txt", "s\n", "Side work")
		_, err = f.wtRepo.Git(ctx, "checkout", "-q", BranchName(wtBead))
		require.NoError(t, err)
		_, err = f.wtRepo.Git(ctx, "merge", "--no-ff", "-q", "-m", "Merge side work", "engineer-side")
		require.NoError(t, err)
		require.NotEmpty(t, f.r.Git(t, "log", "--merges", "--oneline", BranchName(wtBead)),
			"the branch really does carry a merge commit")
		moved := f.dmMainCommit(t, "from-main.txt", "m\n", "Something landed on main")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Empty(t, f.dmMerges(t), "git log --merges on the default branch stays empty")
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, []string{"Side work", "Add note", "Something landed on main", "Initial import of fixture project"}, f.r.Log(t),
			"the branch is flattened onto the default branch")
		assert.Equal(t, moved, f.r.Git(t, "rev-parse", "main~2"))
		assert.FileExists(t, filepath.Join(f.r.Dir, "side.txt"))
	})

	t.Run("a rebase in progress for another branch is refused", func(t *testing.T) {
		f := dmSetup(t)
		before := f.dmMain(t)
		f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
		f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
		// The engineer left a rebase of a branch of their own mid-flight.
		_, err := f.wtRepo.Git(ctx, "checkout", "-q", "-b", "engineer-experiment")
		require.NoError(t, err)
		_, err = f.wtRepo.Git(ctx, "rebase", "main")
		require.Error(t, err)
		require.True(t, f.wtRepo.RebaseInProgress())
		assert.Equal(t, "engineer-experiment", f.wtRepo.RebasingBranch())

		_, err = f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		assert.ErrorIs(t, err, ErrWrongBranch)
		assert.Equal(t, StageRebase, dmStage(t, err))
		assert.NotEqual(t, before, f.dmMain(t))
		assert.Equal(t, f.r.Head(t), f.dmMain(t), "main is left where the test put it")
	})

	t.Run("a resolved rebase against a stale default branch is redone", func(t *testing.T) {
		f := dmSetup(t)
		f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
		f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
		_, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "true"})
		require.ErrorIs(t, err, ErrRebaseConflict)

		// The engineer resolves the conflict, and while they do, more work
		// lands on the default branch.
		acWrite(t, f.wt.Path, "README.md", "resolved version\n")
		_, err = f.wtRepo.Git(ctx, "add", "README.md")
		require.NoError(t, err)
		later := f.dmMainCommit(t, "later.txt", "l\n", "Landed while the engineer resolved")

		res, err := f.m.DeliverMerge(ctx, wtBead, MergeOptions{CheckCommand: "test -f later.txt"})
		require.NoError(t, err)
		assert.False(t, f.wtRepo.RebaseInProgress())
		assert.Equal(t, later, res.Before, "the delivery rebased onto the newest default branch")
		assert.Equal(t, res.Head, f.dmMain(t))
		assert.Equal(t, later, f.r.Git(t, "rev-parse", "main~1"))
		assert.Empty(t, f.dmMerges(t))
		assert.Equal(t, "resolved version\n", f.r.Git(t, "show", "main:README.md")+"\n")
	})
}

func TestRepo_RunCheckCannotStart(t *testing.T) {
	f := dmSetup(t)
	gone := filepath.Join(t.TempDir(), "removed")
	broken := *f.wtRepo
	broken.Root = gone // never existed, so sh cannot be started there

	check, err := broken.runCheck(context.Background(), "true", time.Second)
	var de *DeliveryError
	require.ErrorAs(t, err, &de, "a check that cannot start is still a typed delivery failure")
	assert.Equal(t, StageCheck, de.Stage)
	assert.ErrorIs(t, err, ErrCheckFailed)
	require.NotNil(t, check)
	assert.Equal(t, -1, check.ExitCode, "no exit status when the shell never ran")
}

func TestManager_AssertLinear(t *testing.T) {
	ctx := context.Background()
	f := dmSetup(t)
	base := f.dmMain(t)

	require.NoError(t, f.m.assertLinear(ctx, base, base), "an empty range is linear")

	// Build a merge commit on a branch of the fixture and check the range
	// that a fast-forward would add: this is the guard that refuses to put
	// it on the default branch, whatever the rebase left behind.
	f.r.Git(t, "checkout", "-q", "-b", "topic")
	f.dmMainCommit(t, "topic.txt", "t\n", "Topic work")
	f.r.Git(t, "checkout", "-q", "main")
	f.dmMainCommit(t, "other.txt", "o\n", "Other work")
	f.r.Git(t, "checkout", "-q", "-b", "with-merge")
	f.r.Git(t, "merge", "--no-ff", "-q", "-m", "Merge topic", "topic")
	head := f.r.Git(t, "rev-parse", "refs/heads/with-merge")
	f.r.Git(t, "checkout", "-q", "main")

	err := f.m.assertLinear(ctx, base, head)
	var de *DeliveryError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, StageRebase, de.Stage)
	assert.ErrorIs(t, err, ErrNonLinearBranch)
	assert.NotEmpty(t, de.Output)
}

func TestRepo_RebaseHelpers(t *testing.T) {
	ctx := context.Background()
	f := dmSetup(t)
	assert.False(t, f.wtRepo.RebaseInProgress())
	assert.False(t, f.repo.RebaseInProgress())
	assert.Empty(t, f.wtRepo.RebasingBranch(), "no rebase, no branch")
	files, err := f.wtRepo.ConflictedPaths(ctx)
	require.NoError(t, err)
	assert.Nil(t, files)

	f.dmBranchCommit(t, "README.md", "branch version\n", "Rewrite README on the branch")
	f.dmMainCommit(t, "README.md", "main version\n", "Rewrite README on main")
	_, err = f.wtRepo.Git(ctx, "rebase", "main")
	require.Error(t, err)
	assert.True(t, f.wtRepo.RebaseInProgress(), "the linked worktree's own git dir holds the rebase state")
	assert.Equal(t, BranchName(wtBead), f.wtRepo.RebasingBranch(), "the branch being rebased is readable")
	assert.False(t, f.repo.RebaseInProgress(), "the main working tree is not rebasing")
	files, err = f.wtRepo.ConflictedPaths(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md"}, files)

	_, err = f.wtRepo.Git(ctx, "rebase", "--abort")
	require.NoError(t, err)
	assert.False(t, f.wtRepo.RebaseInProgress())
	assert.Empty(t, f.wtRepo.RebasingBranch())
}
