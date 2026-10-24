package gitguard_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/testutil"
	"slop-cannon/internal/testutil/gitguard"
)

// env is the machine the table below judges against: "main" on "origin",
// with the main working tree at /repo.
func env(s gitguard.Safety) gitguard.Env {
	return gitguard.Env{Safety: s, Default: "main", Remote: "origin", MainDir: "/repo"}
}

// call builds a Call the way Parse would, from the directory git ran in
// and its argument list.
func call(dir string, args ...string) gitguard.Call {
	sub, rest := gitguard.Split(args)
	return gitguard.Call{Seq: 1, Dir: dir, Args: args, Sub: sub, Rest: rest}
}

func TestInspectJudgesEveryWayAGitCommandCanDamageTheRepository(t *testing.T) {
	const wt = "/run/worktrees/fx-1"
	cases := []struct {
		name   string
		safety gitguard.Safety
		dir    string
		args   []string
		want   string // a fragment of the reason, "" when the call is fine
	}{
		// Both modes: damage rather than delivery.
		{"pull is never run", gitguard.SafetyOff, wt, []string{"pull", "origin", "main"}, "merges into the checked-out branch"},
		{"checking out the default branch", gitguard.SafetyOff, wt, []string{"checkout", "main"}, "checking out main"},
		{"resetting the default branch by name", gitguard.SafetyOff, wt, []string{"switch", "-C", "main"}, "creating or resetting main"},
		{"deleting the default branch", gitguard.SafetyOff, wt, []string{"branch", "-D", "main"}, "deleting or moving main"},
		{"renaming the default branch", gitguard.SafetyOff, wt, []string{"branch", "-m", "main", "old"}, "deleting or moving main"},
		{"writing the default branch's ref", gitguard.SafetyOff, wt, []string{"update-ref", "refs/heads/main", "deadbeef"}, "writing the ref of main"},
		{"a worktree that checks out the default branch", gitguard.SafetyOff, "/repo", []string{"worktree", "add", wt, "main"}, "a worktree checking out main"},
		{"a worktree branched from the default branch is fine", gitguard.SafetyOff, "/repo", []string{"worktree", "add", wt, "-b", "sc/fx-1", "main"}, ""},
		{"committing in the main working tree", gitguard.SafetyOff, "/repo", []string{"commit", "-m", "x"}, "commit in the main working tree moves main"},
		{"committing in a bead's worktree is the job", gitguard.SafetyOff, wt, []string{"commit", "-m", "x"}, ""},
		{"cleaning the main working tree", gitguard.SafetyOff, "/repo", []string{"clean", "-fd"}, "discards the user's uncommitted work"},
		{"restoring in the main working tree", gitguard.SafetyOn, "/repo", []string{"restore", "."}, "discards the user's uncommitted work"},
		{"rebasing in the main working tree", gitguard.SafetyOff, "/repo", []string{"rebase", "origin/main"}, "rewrites main"},
		{"rebasing the default branch itself", gitguard.SafetyOff, wt, []string{"rebase", "--onto", "x", "sc/fx-1", "main"}, "rebasing main itself"},
		{"a push carrying every ref", gitguard.SafetyOff, wt, []string{"push", "--mirror", "origin"}, "carries every ref"},

		// Safety on: nothing ever writes the default branch.
		{"safety on merges nothing", gitguard.SafetyOn, wt, []string{"merge", "--ff-only", "sc/fx-1"}, "safety on merges nothing"},
		{"safety on never pushes the default branch", gitguard.SafetyOn, wt, []string{"push", "origin", "main:main"}, "pushing main"},
		{"safety on pushes a bead's branch", gitguard.SafetyOn, wt, []string{"push", "origin", "sc/fx-1:sc/fx-1"}, ""},
		{"safety on never fetches into the default branch", gitguard.SafetyOn, "/repo", []string{"fetch", "origin", "main:main"}, "updating main in place"},
		{"a plain fetch only moves tracking refs", gitguard.SafetyOn, "/repo", []string{"fetch", "origin"}, ""},

		// Safety off: fast-forward only.
		{"a merge without --ff-only", gitguard.SafetyOff, "/repo", []string{"merge", "sc/fx-1"}, "without --ff-only"},
		{"a merge that makes a commit", gitguard.SafetyOff, "/repo", []string{"merge", "--ff-only", "--no-ff", "sc/fx-1"}, "not a plain fast-forward"},
		{"a fast-forward merge is the job", gitguard.SafetyOff, "/repo", []string{"merge", "--ff-only", "sc/fx-1"}, ""},
		{"a forced push of the default branch", gitguard.SafetyOff, "/repo", []string{"push", "--force", "origin", "main"}, "not a fast-forward"},
		{"a plus-refspec push of the default branch", gitguard.SafetyOff, "/repo", []string{"push", "origin", "+main:main"}, "not a fast-forward"},
		{"a deleting push of the default branch", gitguard.SafetyOff, "/repo", []string{"push", "--delete", "origin", "main"}, "not a fast-forward"},
		{"a fast-forward push of the default branch is the job", gitguard.SafetyOff, "/repo", []string{"push", "origin", "main:main"}, ""},
		{"a refspec-less push in the main working tree pushes the default branch", gitguard.SafetyOn, "/repo", []string{"push"}, "pushing main"},
		{"a forced fetch into the default branch", gitguard.SafetyOff, "/repo", []string{"fetch", "--force", "origin", "sc/fx-1:main"}, "not a fast-forward"},
		{"a fast-forward fetch into the default branch is the job", gitguard.SafetyOff, "/repo", []string{"fetch", "origin", "sc/fx-1:main"}, ""},

		{"deleting a bead branch is allowed", gitguard.SafetyOff, wt, []string{"branch", "-D", "--quiet", "sc/fx-1"}, ""},
		{"a rebase in a bead's worktree is the job", gitguard.SafetyOff, wt, []string{"rebase", "--no-verify", "--quiet", "main"}, ""},
		{"cleaning a bead's worktree is the agent tidying up", gitguard.SafetyOn, wt, []string{"clean", "-fdx"}, ""},
		{"checking out paths in the main working tree", gitguard.SafetyOff, "/repo", []string{"checkout", "--", "."}, "discards the user's uncommitted work"},
		{"checking out paths in a bead's worktree is allowed", gitguard.SafetyOff, wt, []string{"checkout", "--", "stats/mean.go"}, ""},
		{"a fetch of the default branch's tracking ref is allowed", gitguard.SafetyOn, "/repo", []string{"fetch", "origin", "refs/heads/main:refs/remotes/origin/main"}, ""},
		{"a refspec-less push from a bead's worktree is allowed", gitguard.SafetyOn, wt, []string{"push", "--quiet", "origin"}, ""},
		{"safety off allows a refspec-less push from the main working tree", gitguard.SafetyOff, "/repo", []string{"push", "--quiet", "origin"}, ""},
		{"safety off refuses a force-with-lease push from the main working tree", gitguard.SafetyOff, "/repo", []string{"push", "--force-with-lease", "origin"}, "not a fast-forward"},

		// Reads are always fine, wherever they run.
		{"rev-parse in the main working tree", gitguard.SafetyOn, "/repo", []string{"rev-parse", "HEAD"}, ""},
		{"status in the main working tree", gitguard.SafetyOn, "/repo", []string{"status", "--porcelain"}, ""},
		{"global options are stepped over", gitguard.SafetyOff, wt, []string{"-c", "merge.ff=only", "-C", "/elsewhere", "log", "--oneline"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gitguard.Inspect(call(tc.dir, tc.args...), env(tc.safety))
			if tc.want == "" {
				assert.Empty(t, got, "this command line is allowed")
				return
			}
			require.NotEmpty(t, got, "this command line must be reported")
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}
}

func TestInspectGHAllowsOnlyWhatADeliveryDoes(t *testing.T) {
	assert.Empty(t, gitguard.InspectGH([]string{"pr", "create", "--head", "sc/fx-1"}))
	assert.Empty(t, gitguard.InspectGH([]string{"pr", "view", "3", "--json", "state"}))
	assert.Empty(t, gitguard.InspectGH([]string{"--version"}), "a bare flag keys to nothing")

	merge := gitguard.InspectGH([]string{"pr", "merge", "3"})
	require.Len(t, merge, 1)
	assert.Contains(t, merge[0], "landing work on the default branch", "D-11")

	assert.Empty(t, gitguard.InspectGH([]string{"pr", "list", "--head", "sc/fx-1", "--json", "number"}))
	assert.Empty(t, gitguard.InspectGH([]string{"pr", "review", "12", "--request-changes", "--body-file", "-"}))
	assert.Empty(t, gitguard.InspectGH([]string{"label", "create", "slop-cannon", "--force"}))

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "close", "12"}, "still be reading"},
		{[]string{"repo", "delete", "acme/widgets", "--yes"}, "deletes the repository"},
		{[]string{"repo", "edit", "--default-branch", "sc/fx-1"}, "default branch"},
	} {
		got := gitguard.InspectGH(tc.args)
		require.Len(t, got, 1, "gh %s must be reported", strings.Join(tc.args, " "))
		assert.Contains(t, got[0], tc.want)
	}

	unknown := gitguard.InspectGH([]string{"api", "-X", "DELETE", "/repos/acme/widgets"})
	require.Len(t, unknown, 1)
	assert.Contains(t, unknown[0], "not one of the commands a delivery makes")
}

// TestInspectGHAllowsAutoMergeOnlyWhenAskedAndOnlyExactly is D-31's half of
// the gh invariant: `gh pr merge` stays damage unless the scenario opted
// in, and under the opt-in only a squash that names its head commit, with
// no flag that bypasses protection, defers the merge or deletes the branch.
func TestInspectGHAllowsAutoMergeOnlyWhenAskedAndOnlyExactly(t *testing.T) {
	on := gitguard.GHOptions{AutoMerge: true}
	exact := []string{"pr", "merge", "7", "--squash", "--match-head-commit", "abc123"}
	assert.Empty(t, gitguard.InspectGHWith(exact, on))
	assert.Empty(t, gitguard.InspectGHWith([]string{"pr", "merge", "7", "--squash", "--match-head-commit=abc123"}, on))

	off := gitguard.InspectGH(exact)
	require.Len(t, off, 1, "without the opt-in even the exact merge is damage")
	assert.Contains(t, off[0], "landing work on the default branch")
	require.Len(t, gitguard.InspectGHWith(exact, gitguard.GHOptions{}), 1)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "merge", "7", "--match-head-commit", "abc123"}, "squash"},
		{[]string{"pr", "merge", "7", "--squash"}, "names the head commit"},
		{[]string{"pr", "merge", "7", "--squash", "--match-head-commit"}, "names the head commit"},
		{[]string{"pr", "merge", "7", "--squash", "--match-head-commit", "--admin"}, "names the head commit"},
		{append(append([]string{}, exact...), "--admin"), "--admin"},
		{append(append([]string{}, exact...), "--auto"), "--auto"},
		{append(append([]string{}, exact...), "--delete-branch"), "--delete-branch"},
		{append(append([]string{}, exact...), "-d"), "-d"},
		{append(append([]string{}, exact...), "--rebase"), "--rebase"},
		{append(append([]string{}, exact...), "--merge"), "--merge"},
	} {
		got := gitguard.InspectGHWith(tc.args, on)
		require.NotEmpty(t, got, "gh %s must be reported", strings.Join(tc.args, " "))
		assert.Contains(t, strings.Join(got, "\n"), tc.want)
	}

	update := []string{"pr", "update-branch", "7"}
	assert.Empty(t, gitguard.InspectGHWith(update, on))
	require.Len(t, gitguard.InspectGH(update), 1, "a branch update is only allowed under the opt-in")
	require.Len(t, gitguard.InspectGHWith([]string{"pr", "update-branch", "7", "--rebase"}, on), 1)

	// The opt-in widens nothing else.
	require.Len(t, gitguard.InspectGHWith([]string{"pr", "close", "7"}, on), 1)
	require.Len(t, gitguard.InspectGHWith([]string{"repo", "edit", "--default-branch", "x"}, on), 1)

	// The login query is the one gh api call a run makes, in that exact form.
	assert.Empty(t, gitguard.InspectGH(gitguard.GHLoginQuery))
	require.Len(t, gitguard.InspectGH([]string{"api", "user", "-X", "PATCH", "-f", "name=x"}), 1)
	require.Len(t, gitguard.InspectGH([]string{"api", "user"}), 1)
}

func TestSafetyForMapsTheConfigurationFlag(t *testing.T) {
	assert.Equal(t, gitguard.SafetyOn, gitguard.SafetyFor(true))
	assert.Equal(t, gitguard.SafetyOff, gitguard.SafetyFor(false))
}

// TestParseReadsWhatTheShimRecorded runs real commands through a shim and
// judges them, which is the whole path a caller uses.
func TestParseReadsWhatTheShimRecorded(t *testing.T) {
	git := testutil.FakeBin(t, "git", "exit 0\n")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"rev-parse", "HEAD"},
		{"clean", "-fd"},
	} {
		cmd := exec.Command(git.Path, args...) //nolint:gosec // the shim this test just installed
		cmd.Dir = dir
		require.NoError(t, cmd.Run())
	}

	calls := gitguard.Parse(t, git)
	require.Len(t, calls, 2)
	assert.Equal(t, "rev-parse", calls[0].Sub)
	assert.Equal(t, gitguard.ResolvePath(dir), calls[0].Dir, "the directory git ran in, symlinks resolved")
	assert.Equal(t, 1, gitguard.Count(calls, "clean"))
	assert.Contains(t, calls[1].String(), "git clean -fd")

	bad := gitguard.Scan(calls, gitguard.Env{
		Safety: gitguard.SafetyOn, Default: "main", Remote: "origin",
		MainDir: gitguard.ResolvePath(dir),
	})
	require.Len(t, bad, 1, "only the clean is damage")
	assert.Contains(t, bad[0], "discards the user's uncommitted work")

	assert.Empty(t, gitguard.Scan(calls, gitguard.Env{
		Safety: gitguard.SafetyOn, Default: "main", Remote: "origin",
		MainDir: filepath.Join(dir, "somewhere-else"),
	}), "the same commands in a bead's worktree are fine")
}

// TestScanGHReadsTheShimsLog is the gh half of the same path.
func TestScanGHReadsTheShimsLog(t *testing.T) {
	gh := testutil.FakeBin(t, "gh", "exit 0\n")
	for _, args := range [][]string{{"pr", "create"}, {"pr", "merge", "1"}} {
		cmd := exec.Command(gh.Path, args...) //nolint:gosec // the shim this test just installed
		require.NoError(t, cmd.Run())
	}
	bad := gitguard.ScanGH(t, gh)
	require.Len(t, bad, 1)
	assert.Contains(t, bad[0], "gh pr merge")
}
