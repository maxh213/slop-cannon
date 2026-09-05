package gitx

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/testutil"
)

// dfFile finds the DiffFile for path or fails the test.
func dfFile(t *testing.T, d *Diff, path string) DiffFile {
	t.Helper()
	for _, f := range d.Files {
		if f.Path == path {
			return f
		}
	}
	require.Failf(t, "file missing from diff", "%s not in %v", path, d.Files)
	return DiffFile{}
}

// dfCommit writes files into repo's working tree (nil content deletes),
// stages everything, commits, and returns the new head.
func dfCommit(t *testing.T, repo *Repo, message string, files map[string]*string) string {
	t.Helper()
	ctx := context.Background()
	for rel, content := range files {
		path := filepath.Join(repo.Root, filepath.FromSlash(rel))
		if content == nil {
			require.NoError(t, os.Remove(path))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(*content), 0o644))
	}
	_, err := repo.Git(ctx, "add", "-A", "--", ".")
	require.NoError(t, err)
	_, err = repo.Git(ctx, "commit", "--quiet", "--no-verify", "-m", message)
	require.NoError(t, err)
	head, err := repo.HeadSHA(ctx)
	require.NoError(t, err)
	return head
}

func dfStr(s string) *string { return &s }

// dfLines returns n numbered lines with the given prefix.
func dfLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

// dfBranch opens the fixture and checks out a new branch "feature" from
// main, so the diff has a base to be measured against.
func dfBranch(t *testing.T) (*testutil.FixtureRepo, *Repo) {
	t.Helper()
	r := testutil.NewFixtureRepo(t)
	r.Git(t, "checkout", "-q", "-b", "feature")
	return r, repoOpen(t, r)
}

func TestRepo_Diff(t *testing.T) {
	ctx := context.Background()

	t.Run("modified, added, deleted, renamed, and binary files", func(t *testing.T) {
		r, repo := dfBranch(t)
		base := r.Git(t, "rev-parse", "main")
		statsSrc, err := os.ReadFile(filepath.Join(r.Dir, "stats", "stats.go"))
		require.NoError(t, err)
		moved := strings.Replace(string(statsSrc), "package stats", "// moved\npackage stats", 1)
		bin := "\x00\x01\x02\xff"
		head := dfCommit(t, repo, "feature work", map[string]*string{
			"main.go":             dfStr("package main\n\nfunc main() {}\n"),
			"NEW.md":              dfStr("hello\nworld\n"),
			"README.md":           nil,
			"stats/stats.go":      nil,
			"stats/statistics.go": &moved,
			"assets/logo.bin":     &bin,
			"fixture/ignored.txt": dfStr("ignored by .gitignore\n"),
		})

		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, base, d.Base())
		assert.Equal(t, head, d.Head())
		assert.False(t, d.Empty())
		assert.False(t, d.Truncated)
		assert.Equal(t, DefaultMaxDiffBytes, d.MaxBytes)

		paths := make([]string, 0, len(d.Files))
		for _, f := range d.Files {
			paths = append(paths, f.Path)
		}
		assert.ElementsMatch(t, []string{"main.go", "NEW.md", "README.md", "stats/statistics.go", "assets/logo.bin"}, paths,
			"ignored files never appear")
		assert.Len(t, d.Stats.Changed, len(d.Files), "Files mirrors Stats.Changed")
		for i, f := range d.Files {
			assert.Equal(t, d.Stats.Changed[i], f.FileStat, "same order as Stats.Changed")
		}

		assert.Equal(t, DiffModified, dfFile(t, d, "main.go").Status)
		added := dfFile(t, d, "NEW.md")
		assert.Equal(t, DiffAdded, added.Status)
		assert.Equal(t, 2, added.Insertions)
		assert.Equal(t, 0, added.Deletions)
		deleted := dfFile(t, d, "README.md")
		assert.Equal(t, DiffDeleted, deleted.Status)
		assert.Greater(t, deleted.Deletions, 0)
		renamed := dfFile(t, d, "stats/statistics.go")
		assert.Equal(t, DiffRenamed, renamed.Status)
		assert.Equal(t, "stats/stats.go", renamed.OldPath)
		assert.Greater(t, renamed.Similarity, 90)
		assert.Equal(t, 1, renamed.Insertions)
		assert.Equal(t, 0, renamed.Deletions)
		binary := dfFile(t, d, "assets/logo.bin")
		assert.Equal(t, DiffAdded, binary.Status)
		assert.True(t, binary.Binary)

		var ins, del int
		for _, f := range d.Files {
			ins += f.Insertions
			del += f.Deletions
		}
		assert.Equal(t, ins, d.Stats.Insertions)
		assert.Equal(t, del, d.Stats.Deletions)
		assert.Equal(t, ins+del, d.ChangedLines())

		assert.Equal(t, len(d.Patch), d.Bytes)
		assert.True(t, strings.HasSuffix(d.Patch, "\n"), "the patch keeps its final newline")
		assert.Contains(t, d.Patch, "rename from stats/stats.go\n")
		assert.Contains(t, d.Patch, "rename to stats/statistics.go\n")
		assert.Contains(t, d.Patch, "diff --git a/NEW.md b/NEW.md\n")
		assert.Contains(t, d.Patch, "+hello\n+world\n")
		assert.Contains(t, d.Patch, "Binary files /dev/null and b/assets/logo.bin differ")
		assert.NotContains(t, d.Patch, "\x1b[", "no colour codes")
		assert.NotContains(t, d.Patch, "ignored.txt")

		stat := d.Stat()
		assert.Contains(t, stat, "R stats/stats.go -> stats/statistics.go  +1 -0\n")
		assert.Contains(t, stat, "A NEW.md  +2 -0\n")
		assert.Contains(t, stat, "A assets/logo.bin  binary\n")
		assert.True(t, strings.HasSuffix(stat, d.Stats.String()), stat)
	})

	t.Run("nothing committed yet", func(t *testing.T) {
		r, repo := dfBranch(t)
		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.True(t, d.Empty())
		assert.Equal(t, r.Head(t), d.Base())
		assert.Equal(t, r.Head(t), d.Head())
		assert.Empty(t, d.Files)
		assert.Empty(t, d.Patch)
		assert.Equal(t, 0, d.Bytes)
		assert.False(t, d.Truncated)
		assert.Equal(t, 0, d.ChangedLines())
		assert.Equal(t, "no changes", d.Stat())
	})

	t.Run("uncommitted changes are not part of the diff", func(t *testing.T) {
		r, repo := dfBranch(t)
		r.WriteFile(t, "main.go", "package main\n")
		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.True(t, d.Empty(), "reviewers only see committed work (D-14)")
	})

	t.Run("measured from the merge-base, not the base tip", func(t *testing.T) {
		r, repo := dfBranch(t)
		base := r.Head(t)
		dfCommit(t, repo, "feature", map[string]*string{"NEW.md": dfStr("feature\n")})
		// main moves on independently; its change must not show up.
		r.Git(t, "checkout", "-q", "main")
		r.WriteFile(t, "UPSTREAM.md", "upstream\n")
		r.Commit(t, "upstream")
		r.Git(t, "checkout", "-q", "feature")

		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, base, d.Base())
		require.Len(t, d.Files, 1)
		assert.Equal(t, "NEW.md", d.Files[0].Path)
		assert.NotContains(t, d.Patch, "UPSTREAM.md")
	})

	t.Run("size cap returns the file list without the patch", func(t *testing.T) {
		_, repo := dfBranch(t)
		big := dfLines("line", 500)
		dfCommit(t, repo, "big", map[string]*string{"big.txt": &big, "small.txt": dfStr("x\n")})

		full, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		require.False(t, full.Truncated)
		require.Greater(t, full.Bytes, 1024)

		capped, err := repo.Diff(ctx, "main", WithMaxDiffBytes(1024))
		require.NoError(t, err)
		assert.True(t, capped.Truncated)
		assert.Empty(t, capped.Patch)
		assert.Equal(t, full.Bytes, capped.Bytes, "the real size is still reported")
		assert.Equal(t, 1024, capped.MaxBytes)
		assert.Equal(t, full.Files, capped.Files, "file list and per-file stats survive the cap")
		assert.Equal(t, full.Stats, capped.Stats)
		assert.Equal(t, 500, dfFile(t, capped, "big.txt").Insertions)
		assert.Contains(t, capped.Stat(), "A big.txt  +500 -0\n")
		assert.Contains(t, capped.Stat(), "A small.txt  +1 -0\n")

		exact, err := repo.Diff(ctx, "main", WithMaxDiffBytes(full.Bytes))
		require.NoError(t, err)
		assert.False(t, exact.Truncated, "a diff exactly at the cap is delivered in full")
		assert.Equal(t, full.Patch, exact.Patch)

		def, err := repo.Diff(ctx, "main", WithMaxDiffBytes(0))
		require.NoError(t, err)
		assert.Equal(t, DefaultMaxDiffBytes, def.MaxBytes, "zero keeps the default")
	})

	t.Run("the patch is the bytes git wrote, CRLF and all", func(t *testing.T) {
		_, repo := dfBranch(t)
		dfCommit(t, repo, "windows line endings", map[string]*string{"crlf.txt": dfStr("x\r\ny\r\n")})

		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.True(t, strings.HasSuffix(d.Patch, "+x\r\n+y\r\n"),
			"the final line keeps its carriage return: %q", d.Patch)
		assert.Equal(t, len(d.Patch), d.Bytes, "the size is the patch's own")
	})

	t.Run("a change with no changed lines is still a diff", func(t *testing.T) {
		_, repo := dfBranch(t)
		require.NoError(t, os.Chmod(filepath.Join(repo.Root, "main.go"), 0o755))
		dfCommit(t, repo, "make main.go executable", nil)

		d, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		assert.False(t, d.Empty(), "a mode-only change must reach the reviewers")
		assert.Equal(t, 0, d.ChangedLines())
		require.Len(t, d.Files, 1)
		assert.Equal(t, DiffModified, d.Files[0].Status)
		assert.Equal(t, "main.go", d.Files[0].Path)
		assert.Contains(t, d.Patch, "new mode 100755\n")
		assert.Contains(t, d.Stat(), "M main.go  +0 -0\n")
	})

	t.Run("no temporary files are left behind", func(t *testing.T) {
		_, repo := dfBranch(t)
		big := dfLines("line", 500)
		dfCommit(t, repo, "big", map[string]*string{"big.txt": &big})
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)

		full, err := repo.Diff(ctx, "main")
		require.NoError(t, err)
		require.NotEmpty(t, full.Patch)
		capped, err := repo.Diff(ctx, "main", WithMaxDiffBytes(64))
		require.NoError(t, err)
		require.True(t, capped.Truncated)

		entries, err := os.ReadDir(tmp)
		require.NoError(t, err)
		assert.Empty(t, entries, "the patch scratch directory is removed")
		status, err := repo.Status(ctx)
		require.NoError(t, err)
		assert.Empty(t, status, "the repository is never written to")
	})

	t.Run("concurrent readers get the same diff", func(t *testing.T) {
		_, repo := dfBranch(t)
		dfCommit(t, repo, "work", map[string]*string{"NEW.md": dfStr(dfLines("l", 20))})
		want, err := repo.Diff(ctx, "main")
		require.NoError(t, err)

		// Three testers review the same worktree at once (D-05).
		const readers = 3
		var wg sync.WaitGroup
		got := make([]*Diff, readers)
		errs := make([]error, readers)
		wg.Add(readers)
		for i := range readers {
			go func() {
				defer wg.Done()
				got[i], errs[i] = repo.Diff(ctx, "main")
			}()
		}
		wg.Wait()
		for i := range readers {
			require.NoError(t, errs[i])
			assert.Equal(t, want, got[i])
		}
	})

	t.Run("errors", func(t *testing.T) {
		r, repo := dfBranch(t)
		_, err := repo.Diff(ctx, "no-such-branch")
		require.ErrorIs(t, err, ErrUnknownRevision)

		empty, err := Open(ctx, repoInitEmpty(t, r), WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		_, err = empty.Diff(ctx, "main")
		require.ErrorIs(t, err, ErrNoCommits)
	})
}

func TestManager_Diff(t *testing.T) {
	ctx := context.Background()

	t.Run("diff of a bead's worktree against the default branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		wt, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)

		before, err := f.m.Diff(ctx, wtBead.ID)
		require.NoError(t, err)
		assert.True(t, before.Empty())

		head := dfCommit(t, wt, "fix mean", map[string]*string{"stats/FIX.md": dfStr("fixed\n")})
		d, err := f.m.Diff(ctx, wtBead.ID, WithMaxDiffBytes(4096))
		require.NoError(t, err)
		assert.Equal(t, f.r.Head(t), d.Base(), "measured from the default branch")
		assert.Equal(t, head, d.Head())
		require.Len(t, d.Files, 1)
		assert.Equal(t, DiffAdded, d.Files[0].Status)
		assert.Equal(t, "stats/FIX.md", d.Files[0].Path)
		assert.Contains(t, d.Patch, "+fixed\n")
		assert.Equal(t, 4096, d.MaxBytes)
		assert.Empty(t, f.r.Status(t), "the target repository stays clean")
	})

	t.Run("missing worktree", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.Diff(ctx, "fx-nope")
		require.ErrorIs(t, err, ErrWorktreeMissing)
	})
}

func TestRepo_ChangeRatio(t *testing.T) {
	ctx := context.Background()
	_, repo := dfBranch(t)

	// Round 1: ten new lines.
	dfCommit(t, repo, "round 1", map[string]*string{"a.txt": dfStr(dfLines("a", 10))})
	round1, err := repo.Diff(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, 10, round1.ChangedLines())

	t.Run("same head means nothing moved", func(t *testing.T) {
		c, err := repo.ChangeRatio(ctx, round1, round1)
		require.NoError(t, err)
		assert.Equal(t, ChangeRatio{Previous: round1.Head(), Current: round1.Head(), Total: 10}, c)
		assert.False(t, c.Exceeds(DefaultReReviewThreshold))
	})

	// Round 2: one line edited (1 insertion + 1 deletion = 2 of 10).
	dfCommit(t, repo, "round 2", map[string]*string{
		"a.txt": dfStr(strings.Replace(dfLines("a", 10), "a 3\n", "a three\n", 1)),
	})
	round2, err := repo.Diff(ctx, "main")
	require.NoError(t, err)

	t.Run("a small fix keeps earlier approvals", func(t *testing.T) {
		c, err := repo.ChangeRatio(ctx, round1, round2)
		require.NoError(t, err)
		assert.Equal(t, round1.Head(), c.Previous)
		assert.Equal(t, round2.Head(), c.Current)
		assert.Equal(t, 2, c.Delta)
		assert.Equal(t, 10, c.Total)
		assert.InDelta(t, 0.2, c.Ratio, 1e-9)
		assert.False(t, c.Exceeds(0.4))
		assert.False(t, c.Exceeds(0), "zero means the default threshold")
		assert.True(t, c.Exceeds(0.1))
	})

	// Round 3: half the file rewritten (5 insertions + 5 deletions = 10 of 10).
	dfCommit(t, repo, "round 3", map[string]*string{
		"a.txt": dfStr(dfLines("b", 5) + strings.Join(strings.SplitAfter(dfLines("a", 10), "\n")[5:], "")),
	})
	round3, err := repo.Diff(ctx, "main")
	require.NoError(t, err)

	t.Run("a rewrite sends everyone back to review", func(t *testing.T) {
		c, err := repo.ChangeRatio(ctx, round2, round3)
		require.NoError(t, err)
		assert.Equal(t, 10, c.Delta)
		assert.Equal(t, 10, c.Total)
		assert.InDelta(t, 1.0, c.Ratio, 1e-9)
		assert.True(t, c.Exceeds(DefaultReReviewThreshold))
	})

	// Round 4: everything reverted, the change set is empty.
	dfCommit(t, repo, "round 4", map[string]*string{"a.txt": nil})
	round4, err := repo.Diff(ctx, "main")
	require.NoError(t, err)
	require.True(t, round4.Empty())

	t.Run("an emptied change set counts as fully changed", func(t *testing.T) {
		c, err := repo.ChangeRatio(ctx, round3, round4)
		require.NoError(t, err)
		assert.Equal(t, 10, c.Delta)
		assert.Equal(t, 0, c.Total)
		assert.Equal(t, 1.0, c.Ratio)
		assert.True(t, c.Exceeds(DefaultReReviewThreshold))
	})

	t.Run("nil diffs are errors", func(t *testing.T) {
		_, err := repo.ChangeRatio(ctx, nil, round1)
		require.Error(t, err)
		_, err = repo.ChangeRatio(ctx, round1, nil)
		require.Error(t, err)
	})

	t.Run("a head that is gone is an unknown revision", func(t *testing.T) {
		// The previous round was rebased away and its commit pruned; the
		// caller re-reviews rather than failing the round.
		bogus := &Diff{Stats: DiffStats{Head: strings.Repeat("0", 40)}}
		_, err := repo.ChangeRatio(ctx, bogus, round1)
		require.ErrorIs(t, err, ErrUnknownRevision)
		var ge *GitError
		require.ErrorAs(t, err, &ge)
		assert.Equal(t, 128, ge.ExitCode)
	})

	t.Run("the rebase-safe measurement needs no surviving commit", func(t *testing.T) {
		// P4-14: the patches are the evidence, so a previous round that was
		// rebased away and pruned no longer costs a full re-review.
		gone := &Diff{Stats: round1.Stats, Patch: round1.Patch}
		gone.Stats.Head = strings.Repeat("0", 40)
		c, err := repo.RebaseSafeChangeRatio(ctx, gone, round1)
		require.NoError(t, err)
		assert.Zero(t, c.Delta, "the same change set, whatever commit carries it")
		assert.False(t, c.Exceeds(DefaultReReviewThreshold))

		// Without a patch there is nothing to compare, so it falls back to
		// the commits — and a commit that is gone is an unknown revision.
		truncated := &Diff{Stats: DiffStats{Head: strings.Repeat("0", 40), Files: 1, Insertions: 1}, Truncated: true}
		_, err = repo.RebaseSafeChangeRatio(ctx, truncated, round1)
		require.ErrorIs(t, err, ErrUnknownRevision)

		_, err = repo.RebaseSafeChangeRatio(ctx, nil, round1)
		require.Error(t, err)
	})
}

// TestRepo_ChangeRatio_AcrossARebase is P4-14: the D-09 measurement has to
// survive a rebase. Reading the two branch tips counts every upstream
// commit the branch was rebased onto as the engineer's work, so on a busy
// default branch even a one-line fix scores above the threshold and buys a
// three-lens re-review it never earned.
func TestRepo_ChangeRatio_AcrossARebase(t *testing.T) {
	ctx := context.Background()
	r, repo := dfBranch(t)

	dfCommit(t, repo, "round 1", map[string]*string{"a.txt": dfStr(dfLines("a", 10))})
	round1, err := repo.Diff(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, 10, round1.ChangedLines())

	// The default branch moves a long way underneath the branch.
	r.Git(t, "checkout", "-q", "main")
	r.WriteFile(t, "upstream.txt", dfLines("upstream", 200))
	r.Commit(t, "a busy default branch")
	r.Git(t, "checkout", "-q", "feature")
	_, err = repo.Git(ctx, "-c", "commit.gpgsign=false", "rebase", "--quiet", "main")
	require.NoError(t, err)

	// Round 2 is one line of the engineer's own work.
	dfCommit(t, repo, "round 2", map[string]*string{
		"a.txt": dfStr(strings.Replace(dfLines("a", 10), "a 3\n", "a three\n", 1)),
	})
	round2, err := repo.Diff(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, 10, round2.ChangedLines(), "the change set is still the same ten lines")

	c, err := repo.RebaseSafeChangeRatio(ctx, round1, round2)
	require.NoError(t, err)
	assert.Equal(t, 2, c.Delta, "one line edited: +1 -1, and none of upstream's 200")
	assert.Equal(t, 10, c.Total)
	assert.InDelta(t, 0.2, c.Ratio, 1e-9)
	assert.False(t, c.Exceeds(DefaultReReviewThreshold), "earlier approvals stand (D-09)")

	// Non-vacuity: the tip-to-tip measurement really would have counted the
	// whole default branch as the round's work.
	tips, err := repo.ChangeRatio(ctx, round1, round2)
	require.NoError(t, err)
	assert.Greater(t, tips.Delta, 200, "the tips are 200 upstream lines apart")
	assert.True(t, tips.Exceeds(DefaultReReviewThreshold),
		"which would have sent every tester back to review")

	// The pure comparison is available on its own, for callers that
	// already hold both diffs.
	pure, ok := ChangeSetRatio(round1, round2)
	require.True(t, ok)
	assert.Equal(t, c, pure)

	// A resolution that changed nothing at all scores zero across the same
	// rebase.
	same, ok := ChangeSetRatio(round2, round2)
	require.True(t, ok)
	assert.Zero(t, same.Delta)
	_, ok = ChangeSetRatio(&Diff{Truncated: true, Stats: DiffStats{Files: 1}}, round2)
	assert.False(t, ok, "a truncated diff cannot be compared this way")
	_, ok = ChangeSetRatio(nil, round2)
	assert.False(t, ok)
}

func TestRatioOf(t *testing.T) {
	cases := []struct {
		delta, total int
		want         float64
	}{
		{0, 0, 0},
		{0, 10, 0},
		{-1, 10, 0},
		{4, 0, 1},
		{4, 10, 0.4},
		{3, 10, 0.3},
		{15, 10, 1.5},
	}
	for _, c := range cases {
		assert.InDelta(t, c.want, RatioOf(c.delta, c.total), 1e-9, "RatioOf(%d, %d)", c.delta, c.total)
	}
	assert.False(t, ChangeRatio{Ratio: 0.4}.Exceeds(0.4), "exactly the threshold does not exceed it")
	assert.True(t, ChangeRatio{Ratio: 0.41}.Exceeds(0.4))
	assert.True(t, ChangeRatio{Ratio: 0.41}.Exceeds(-1), "negative means the default threshold")
}

func TestClampToInt(t *testing.T) {
	assert.Equal(t, 0, clampToInt(0))
	assert.Equal(t, 1234, clampToInt(1234))
	assert.Equal(t, math.MaxInt, clampToInt(math.MaxInt64), "a size past int never wraps negative")
}

func TestParseNameStatus(t *testing.T) {
	t.Run("all forms", func(t *testing.T) {
		out := "M\x00bin\x00R075\x00f1\x00dir_f1\x00D\x00f2\x00A\x00f3\x00C100\x00src\x00copy\x00T\x00link\x00"
		got, err := parseNameStatus(out)
		require.NoError(t, err)
		assert.Equal(t, map[string]fileStatus{
			"bin":    {Status: DiffModified},
			"dir_f1": {Status: DiffRenamed, Similarity: 75},
			"f2":     {Status: DiffDeleted},
			"f3":     {Status: DiffAdded},
			"copy":   {Status: DiffCopied, Similarity: 100},
			"link":   {Status: DiffTypeChanged},
		}, got)
	})

	t.Run("empty", func(t *testing.T) {
		got, err := parseNameStatus("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("malformed", func(t *testing.T) {
		for _, bad := range []string{
			"R075\x00only-old\x00", // rename missing its new path
			"M\x00",                // status without a path
			"Rxx\x00a\x00b\x00",    // non-numeric score
			"\x00path\x00",         // empty status token
		} {
			_, err := parseNameStatus(bad)
			assert.Error(t, err, "%q", bad)
		}
	})
}

func TestDiff_Stat(t *testing.T) {
	d := &Diff{
		Stats: DiffStats{Files: 3, Insertions: 12, Deletions: 3},
		Files: []DiffFile{
			{FileStat: FileStat{Path: "b/new.go", Insertions: 10}, Status: DiffAdded},
			{FileStat: FileStat{Path: "moved.go", OldPath: "old.go", Insertions: 2, Deletions: 3}, Status: DiffRenamed, Similarity: 80},
			{FileStat: FileStat{Path: "img.png", Binary: true}, Status: DiffModified},
		},
	}
	want := "A b/new.go  +10 -0\n" +
		"R old.go -> moved.go  +2 -3\n" +
		"M img.png  binary\n" +
		"3 files changed, 12 insertions(+), 3 deletions(-)"
	assert.Equal(t, want, d.Stat())
}
