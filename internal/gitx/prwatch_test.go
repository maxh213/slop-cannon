package gitx

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// pwUpdateGolden rewrites the review-body golden files under testdata/gh
// from the current output: go test ./internal/gitx -run TestBuildReview -update-gh-golden
var pwUpdateGolden = flag.Bool("update-gh-golden", false, "rewrite the gh review-body golden files")

// pwTestdata returns the absolute path of a file under testdata/gh.
func pwTestdata(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "gh", name))
	require.NoError(t, err)
	return abs
}

// pwRepo opens a fixture with a bare remote and the isolated environment.
func pwRepo(t *testing.T) (*testutil.FixtureRepo, *Repo) {
	t.Helper()
	r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
	return r, repoOpen(t, r)
}

// pwShimConfig describes how the gh shim answers each subcommand.
type pwShimConfig struct {
	// listFile is the JSON file `gh pr list` prints; empty prints "[]".
	listFile string
	// listExit and listStderr make `gh pr list` fail.
	listExit   int
	listStderr string
	// reviewExit and reviewStderr make `gh pr review` fail.
	reviewExit   int
	reviewStderr string
	// authExit and authStderr shape `gh auth status`.
	authExit   int
	authStderr string
}

// pwGH installs a gh shim that replays the configured answers and records
// every invocation (stdin captured, so review bodies can be checked). It
// also dumps a few environment variables of every call into env.<seq>.
func pwGH(t *testing.T, cfg pwShimConfig) *testutil.Shim {
	t.Helper()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	var b strings.Builder
	b.WriteString("printf 'GH_PROMPT_DISABLED=%s\\nHOME=%s\\nGH_TOKEN=%s\\nSECRET=%s\\n' \"$GH_PROMPT_DISABLED\" \"$HOME\" \"$GH_TOKEN\" \"$SECRET\" > \"$SC_FAKEBIN_DIR/env.$SC_FAKEBIN_SEQ\"\n")
	b.WriteString("case \"$1 $2\" in\n")
	b.WriteString("  'pr list')\n")
	if cfg.listStderr != "" {
		b.WriteString("    printf '%s\\n' " + q(cfg.listStderr) + " >&2\n")
	}
	if cfg.listExit != 0 {
		fmt.Fprintf(&b, "    exit %d ;;\n", cfg.listExit)
	} else if cfg.listFile != "" {
		b.WriteString("    cat " + q(cfg.listFile) + " ;;\n")
	} else {
		b.WriteString("    echo '[]' ;;\n")
	}
	b.WriteString("  'pr review')\n")
	if cfg.reviewStderr != "" {
		b.WriteString("    printf '%s\\n' " + q(cfg.reviewStderr) + " >&2\n")
	}
	fmt.Fprintf(&b, "    exit %d ;;\n", cfg.reviewExit)
	b.WriteString("  'auth status')\n")
	if cfg.authStderr != "" {
		b.WriteString("    printf '%s\\n' " + q(cfg.authStderr) + " >&2\n")
	}
	fmt.Fprintf(&b, "    exit %d ;;\n", cfg.authExit)
	b.WriteString("  *) echo \"gh shim: unexpected command: $*\" >&2; exit 2 ;;\n")
	b.WriteString("esac\n")
	return testutil.FakeBin(t, "gh", b.String(), testutil.WithStdinCapture())
}

// pwStore is an in-memory BeadStore that mints ids and can fail on demand.
type pwStore struct {
	mu      sync.Mutex
	issues  []beads.Issue
	created []beads.BeadInput
	failFor map[int]error // PR number -> error for Create
	listErr error
	next    int
}

func (s *pwStore) Create(_ context.Context, in beads.BeadInput) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := beads.Metadata(in.Metadata).Int(beads.MetaPRNumber); ok {
		if err := s.failFor[n]; err != nil {
			return beads.Issue{}, err
		}
	}
	s.next++
	is := beads.Issue{
		ID:          domain.BeadID(fmt.Sprintf("fx-pr%d", s.next)),
		Title:       in.Title,
		Description: in.Description,
		Acceptance:  in.Acceptance,
		Status:      string(domain.BeadStateOpen),
		Type:        string(in.Type),
		Priority:    in.Priority,
		Labels:      append([]string(nil), in.Labels...),
		Metadata:    beads.Metadata(in.Metadata),
	}
	s.issues = append(s.issues, is)
	s.created = append(s.created, in)
	return is, nil
}

func (s *pwStore) List(_ context.Context, f beads.ListFilter) ([]beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []beads.Issue
	for _, is := range s.issues {
		if !f.All && is.Status == string(domain.BeadStateClosed) {
			continue
		}
		ok := true
		for _, l := range f.Labels {
			if !is.HasLabel(l) {
				ok = false
			}
		}
		if ok {
			out = append(out, is)
		}
	}
	return out, nil
}

// pwSeed adds a review bead for a pull request number to the store.
func (s *pwStore) pwSeed(id domain.BeadID, status string, meta map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = append(s.issues, beads.Issue{
		ID: id, Title: "seeded " + string(id), Status: status,
		Labels: []string{beads.LabelExternalPR}, Metadata: beads.Metadata(meta),
	})
}

// pwVerdicts returns the mixed verdicts used by the golden test: qa
// requests changes, ux and test approve. Given out of lens order so the
// sort is exercised.
func pwVerdicts() []domain.Verdict {
	return []domain.Verdict{
		{Lens: domain.LensTest, Round: 1, Outcome: domain.VerdictApprove, Summary: "Coverage is fine; TestMean now covers the empty slice."},
		{Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "Two problems block this.", Items: []domain.FeedbackItem{
			{ID: "qa-r1-1", Severity: domain.SeverityBlocker, Category: "correctness", Title: "Mean of an empty slice still divides by zero", File: "stats/mean.go", Line: 14, Detail: "The guard checks len(xs) < 0, which is never true.\nUse len(xs) == 0.", Suggestion: "if len(xs) == 0 {\n  return 0, ErrEmpty\n}", Status: domain.ItemOpen},
			{ID: "qa-r1-2", Severity: domain.SeverityMinor, Title: "Error message says 'slice' but callers pass arrays", File: "stats/mean.go", Status: domain.ItemOpen},
			{ID: "qa-r1-3", Severity: domain.SeverityNit, Category: "style", Title: "Stray blank line", Status: domain.ItemAccepted},
		}},
		{Lens: domain.LensUX, Round: 1, Outcome: domain.VerdictApprove, Summary: "CLI output unchanged."},
	}
}

// pwPR returns the external pull request #12 of the sample list.
func pwPR() PullRequest {
	return PullRequest{
		Number: 12, Title: "Fix Mean for empty slices", URL: "https://github.com/example/fixture/pull/12",
		Body: "Mean returned NaN for an empty slice.\n\nFixes #7.", Author: "alice",
		HeadRef: "alice/fix-mean", BaseRef: "main", HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		Labels: []string{"bug"},
	}
}

// pwGolden compares got with testdata/gh/<name>, rewriting it under
// -update-gh-golden.
func pwGolden(t *testing.T, name, got string) {
	t.Helper()
	path := pwTestdata(t, name)
	if *pwUpdateGolden {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "golden file missing; run with -update-gh-golden")
	assert.Equal(t, string(want), got, "review body differs from %s", path)
}

func TestParsePRList(t *testing.T) {
	data, err := os.ReadFile(pwTestdata(t, "pr-list.json"))
	require.NoError(t, err)
	prs, err := ParsePRList(data)
	require.NoError(t, err)
	require.Len(t, prs, 4)

	assert.Equal(t, pwPR().Number, prs[0].Number)
	assert.Equal(t, pwPR().Title, prs[0].Title)
	assert.Equal(t, pwPR().URL, prs[0].URL)
	assert.Equal(t, "alice", prs[0].Author)
	assert.Equal(t, "alice/fix-mean", prs[0].HeadRef)
	assert.Equal(t, "main", prs[0].BaseRef)
	assert.Equal(t, pwPR().HeadSHA, prs[0].HeadSHA)
	assert.Equal(t, []string{"bug"}, prs[0].Labels)
	assert.False(t, prs[0].IsDraft)
	assert.Equal(t, time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC), prs[0].UpdatedAt)
	assert.Equal(t, "gh-12", prs[0].ExternalRef())

	assert.True(t, prs[1].HasLabel("slop-cannon"))
	assert.True(t, prs[1].HasLabel("SLOP-CANNON"), "label match is case-insensitive")
	assert.Nil(t, prs[2].Labels)
	assert.True(t, prs[3].IsDraft)

	t.Run("empty and invalid", func(t *testing.T) {
		prs, err := ParsePRList([]byte("  \n"))
		require.NoError(t, err)
		assert.Nil(t, prs)
		prs, err = ParsePRList([]byte("[]"))
		require.NoError(t, err)
		assert.Empty(t, prs)
		_, err = ParsePRList([]byte("{not json"))
		require.Error(t, err)
	})
}

func TestIsCannonPR(t *testing.T) {
	assert.False(t, IsCannonPR(pwPR()))
	assert.True(t, IsCannonPR(PullRequest{Number: 1, HeadRef: "feature/x", Labels: []string{"bug", CannonPRLabel}}), "labelled")
	assert.True(t, IsCannonPR(PullRequest{Number: 2, HeadRef: BranchPrefix + "fx-1-slug"}), "sc/ branch")
	assert.False(t, IsCannonPR(PullRequest{Number: 3, HeadRef: "scratch/x"}), "prefix must be a path segment")
}

func TestGH_ListOpenPRs(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)
	t.Setenv("GH_TOKEN", "ghp_0123456789abcdefTESTTOKEN")
	t.Setenv("SECRET", "should-not-leak")
	shim := pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})

	prs, err := NewGH(repo).ListOpenPRs(ctx)
	require.NoError(t, err)
	require.Len(t, prs, 4)
	assert.Equal(t, 12, prs[0].Number)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"pr", "list", "--state", "open", "--limit", "200", "--json", strings.Join(ghPRFields, ",")}, calls[0].Args)
	repoSamePath(t, repo.Root, calls[0].Dir)

	env, err := os.ReadFile(filepath.Join(shim.Dir, "env.1"))
	require.NoError(t, err)
	assert.Contains(t, string(env), "GH_PROMPT_DISABLED=1\n")
	assert.Contains(t, string(env), "GH_TOKEN=ghp_0123456789abcdefTESTTOKEN\n", "gh needs its token")
	assert.Contains(t, string(env), "SECRET=\n", "unrelated variables are withheld")
	assert.NotContains(t, string(env), "HOME=\n", "HOME is inherited")
}

func TestGH_Errors(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)

	t.Run("gh missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := NewGH(repo).ListOpenPRs(ctx)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrGHNotFound), err)
		assert.True(t, proc.IsNotFound(err), err)
		assert.ErrorIs(t, NewGH(repo).Check(ctx), ErrGHNotFound)
	})

	t.Run("unauthenticated", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 4, listStderr: "To get started with GitHub CLI, please run:  gh auth login\nAlternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token."})
		_, err := NewGH(repo).ListOpenPRs(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrGHUnauthenticated)
		var ghe *GHError
		require.True(t, errors.As(err, &ghe))
		assert.Equal(t, 4, ghe.ExitCode)
		assert.Equal(t, []string{"pr", "list", "--state", "open", "--limit", "200", "--json", strings.Join(ghPRFields, ",")}, ghe.Args)
		assert.Contains(t, err.Error(), "gitx: gh pr list")
		assert.Contains(t, err.Error(), "not authenticated")
		var ee *proc.ExitError
		assert.True(t, errors.As(err, &ee), "the runner's ExitError stays reachable")
	})

	t.Run("no github remote", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 1, listStderr: "no git remotes found"})
		_, err := NewGH(repo).ListOpenPRs(ctx)
		assert.ErrorIs(t, err, ErrNoGitHubRemote)
	})

	t.Run("other failure", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 1, listStderr: "HTTP 502: bad gateway"})
		_, err := NewGH(repo).ListOpenPRs(ctx)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrGHUnauthenticated))
		assert.False(t, errors.Is(err, ErrNoGitHubRemote))
		assert.False(t, errors.Is(err, ErrGHNotFound))
		assert.Equal(t, "gitx: gh pr list --state open --limit 200 --json "+strings.Join(ghPRFields, ",")+": exit status 1: HTTP 502: bad gateway", err.Error())
	})

	t.Run("malformed output", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(bad, []byte("not json"), 0o644))
		pwGH(t, pwShimConfig{listFile: bad})
		_, err := NewGH(repo).ListOpenPRs(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decoding output")
	})
}

func TestGH_Check(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)

	t.Run("logged in", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{})
		require.NoError(t, NewGH(repo).Check(ctx))
		assert.Equal(t, [][]string{{"auth", "status"}}, shim.ArgLists(t))
	})
	t.Run("not logged in, known wording", func(t *testing.T) {
		pwGH(t, pwShimConfig{authExit: 1, authStderr: "You are not logged into any GitHub hosts. To log in, run: gh auth login"})
		assert.ErrorIs(t, NewGH(repo).Check(ctx), ErrGHUnauthenticated)
	})
	t.Run("not logged in, unknown wording", func(t *testing.T) {
		pwGH(t, pwShimConfig{authExit: 1, authStderr: "something else entirely"})
		assert.ErrorIs(t, NewGH(repo).Check(ctx), ErrGHUnauthenticated)
	})
}

func TestBuildReview(t *testing.T) {
	pr := pwPR()

	t.Run("changes requested golden", func(t *testing.T) {
		review, err := BuildReview(pr, pwVerdicts())
		require.NoError(t, err)
		assert.Equal(t, ReviewRequestChanges, review.Event)
		assert.Equal(t, "--request-changes", review.Event.Flag())
		require.Len(t, review.Verdicts, 3)
		assert.Equal(t, []domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest},
			[]domain.Lens{review.Verdicts[0].Lens, review.Verdicts[1].Lens, review.Verdicts[2].Lens}, "ordered by lens")
		assert.Equal(t, pr, review.PR)
		pwGolden(t, "review-body-changes.golden.md", review.Body)
	})

	t.Run("approved golden", func(t *testing.T) {
		verdicts := []domain.Verdict{
			{Lens: domain.LensUX, Outcome: domain.VerdictApprove, Summary: "Looks good."},
			{Lens: domain.LensQA, Outcome: domain.VerdictApprove},
			{Lens: domain.LensTest, Outcome: domain.VerdictApprove, Summary: "Tests cover the fix.", Items: []domain.FeedbackItem{
				{ID: "test-r1-1", Severity: domain.SeverityNit, Title: "Table test would read better", File: "stats/mean_test.go", Line: 30, Status: domain.ItemOpen},
			}},
		}
		review, err := BuildReview(pr, verdicts)
		require.NoError(t, err)
		assert.Equal(t, ReviewApprove, review.Event)
		pwGolden(t, "review-body-approved.golden.md", review.Body)
	})

	t.Run("single verdict without lens", func(t *testing.T) {
		review, err := BuildReview(pr, []domain.Verdict{{Outcome: domain.VerdictChanges, Summary: "Nope."}})
		require.NoError(t, err)
		assert.Equal(t, ReviewRequestChanges, review.Event)
		assert.Contains(t, review.Body, "**Verdict: changes requested** by tester.\n")
		assert.Contains(t, review.Body, "### tester: changes requested\n")
		assert.Contains(t, review.Body, "from 1 tester verdict.")
	})

	t.Run("no verdicts", func(t *testing.T) {
		_, err := BuildReview(pr, nil)
		assert.ErrorIs(t, err, ErrNoVerdicts)
	})

	t.Run("secrets are redacted", func(t *testing.T) {
		review, err := BuildReview(pr, []domain.Verdict{{Lens: domain.LensQA, Outcome: domain.VerdictChanges,
			Summary: "The test hardcodes ghp_0123456789abcdefLEAKED as a token."}})
		require.NoError(t, err)
		assert.NotContains(t, review.Body, "ghp_0123456789abcdefLEAKED")
		assert.Contains(t, review.Body, "ghp_")
	})

	t.Run("input is not mutated", func(t *testing.T) {
		verdicts := pwVerdicts()
		_, err := BuildReview(pr, verdicts)
		require.NoError(t, err)
		assert.Equal(t, domain.LensTest, verdicts[0].Lens)
	})
}

func TestGH_PostReview(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)
	pr := pwPR()

	t.Run("request changes", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{})
		review, err := NewGH(repo).PostReview(ctx, pr, pwVerdicts())
		require.NoError(t, err)
		calls := shim.Invocations(t)
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"pr", "review", "12", "--request-changes", "--body-file", "-"}, calls[0].Args)
		assert.Equal(t, review.Body, calls[0].Stdin, "the body is streamed on stdin")
		repoSamePath(t, repo.Root, calls[0].Dir)
	})

	t.Run("approve", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{})
		review, err := NewGH(repo).PostReview(ctx, pr, []domain.Verdict{
			{Lens: domain.LensUX, Outcome: domain.VerdictApprove},
			{Lens: domain.LensQA, Outcome: domain.VerdictApprove},
			{Lens: domain.LensTest, Outcome: domain.VerdictApprove},
		})
		require.NoError(t, err)
		assert.Equal(t, ReviewApprove, review.Event)
		assert.Equal(t, [][]string{{"pr", "review", "12", "--approve", "--body-file", "-"}}, shim.ArgLists(t))
	})

	t.Run("gh fails", func(t *testing.T) {
		pwGH(t, pwShimConfig{reviewExit: 1, reviewStderr: "GraphQL: Review Can not approve your own pull request (addPullRequestReview)"})
		_, err := NewGH(repo).PostReview(ctx, pr, pwVerdicts())
		require.Error(t, err)
		var ghe *GHError
		require.True(t, errors.As(err, &ghe))
		assert.Equal(t, 1, ghe.ExitCode)
		assert.Contains(t, ghe.Stderr, "Can not approve")
	})

	t.Run("nothing to post", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{})
		_, err := NewGH(repo).PostReview(ctx, pr, nil)
		assert.ErrorIs(t, err, ErrNoVerdicts)
		_, err = NewGH(repo).PostReview(ctx, PullRequest{}, pwVerdicts())
		require.Error(t, err)
		assert.Empty(t, shim.ArgLists(t), "gh is not run without a review to post")
	})

	t.Run("never merges", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		gh := NewGH(repo)
		_, err := gh.ListOpenPRs(ctx)
		require.NoError(t, err)
		_, err = gh.PostReview(ctx, pr, pwVerdicts())
		require.NoError(t, err)
		require.NoError(t, gh.Check(ctx))
		for _, args := range shim.ArgLists(t) {
			assert.NotContains(t, args, "merge")
		}
	})
}

func TestReviewBeadInput(t *testing.T) {
	pr := pwPR()
	pr.IsDraft = true

	in := ReviewBeadInput(pr, "")
	assert.Equal(t, "Review PR #12: Fix Mean for empty slices", in.Title)
	assert.Equal(t, domain.BeadTypeTask, in.Type)
	assert.Equal(t, domain.PriorityDefault, in.Priority)
	assert.Equal(t, []string{beads.LabelExternalPR}, in.Labels)
	assert.Equal(t, "https://github.com/example/fixture/pull/12", in.Metadata[beads.MetaPRURL])
	assert.Equal(t, 12, in.Metadata[beads.MetaPRNumber])
	assert.Equal(t, KindExternalPR, in.Metadata[beads.MetaKind])
	assert.Equal(t, "gh-12", in.Metadata[MetaExternalRef])
	assert.Equal(t, "gh-12", in.ExternalRef, "bd's own --external-ref flag carries it too (P3-19)")
	assert.Equal(t, pr.HeadSHA, in.Metadata[MetaPRHead])
	assert.NotContains(t, in.Metadata, beads.MetaRun)
	assert.Equal(t, "External pull request #12 by alice: https://github.com/example/fixture/pull/12\n"+
		"Branch: alice/fix-mean -> main (head 0123456789ab)\n"+
		"Status: draft\n\n"+
		"Mean returned NaN for an empty slice.\n\nFixes #7.\n", in.Description)
	assert.Contains(t, in.Acceptance, "gh pr review")
	assert.Contains(t, in.Acceptance, "never merged")

	t.Run("with run", func(t *testing.T) {
		in := ReviewBeadInput(PullRequest{Number: 3, Title: "  T  ", URL: "u", HeadRef: "h", BaseRef: "b"}, "r1")
		assert.Equal(t, "Review PR #3: T", in.Title)
		assert.Equal(t, []string{beads.LabelExternalPR, beads.RunLabel("r1")}, in.Labels)
		assert.Equal(t, "r1", in.Metadata[beads.MetaRun])
		assert.NotContains(t, in.Metadata, MetaPRHead)
		assert.Equal(t, "External pull request #3: u\nBranch: h -> b\n", in.Description)
	})

	t.Run("round-trips through beads accessors", func(t *testing.T) {
		is := beads.Issue{Labels: in.Labels, Metadata: beads.Metadata(in.Metadata)}
		assert.True(t, is.IsExternalPR())
		assert.Equal(t, 12, is.PRNumber())
		assert.Equal(t, pr.URL, is.PRURL())
		assert.Equal(t, KindExternalPR, is.Kind())
	})
}

func TestPRWatcher_Poll(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)

	t.Run("creates one bead per external PR, then none", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{}
		w := NewPRWatcher(NewGH(repo), store, WithRun("r1"))
		assert.Equal(t, DefaultPRPollInterval, w.Interval())

		res, err := w.Poll(ctx)
		require.NoError(t, err)
		require.Len(t, res.External, 2)
		assert.Equal(t, []int{12, 15}, []int{res.External[0].Number, res.External[1].Number})
		require.Len(t, res.Skipped, 2)
		assert.Equal(t, []int{13, 14}, []int{res.Skipped[0].Number, res.Skipped[1].Number})
		require.Len(t, res.New, 2)
		assert.Equal(t, domain.BeadID("fx-pr1"), res.New[0].Issue.ID)
		assert.Equal(t, 12, res.New[0].PR.Number)
		assert.Equal(t, 15, res.New[1].PR.Number)
		assert.Empty(t, res.Existing)
		assert.Empty(t, res.Stale)
		require.Len(t, store.created, 2)
		assert.Equal(t, []string{beads.LabelExternalPR, "sc:run:r1"}, store.created[0].Labels)
		assert.Equal(t, "gh-12", store.created[0].Metadata[MetaExternalRef])
		assert.Equal(t, "gh-12", store.created[0].ExternalRef)

		res, err = w.Poll(ctx)
		require.NoError(t, err)
		assert.Empty(t, res.New, "second poll is idempotent")
		require.Len(t, res.Existing, 2)
		assert.Equal(t, domain.BeadID("fx-pr1"), res.Existing[0].Issue.ID)
		assert.Equal(t, domain.BeadID("fx-pr2"), res.Existing[1].Issue.ID)
		assert.Len(t, store.created, 2)
	})

	// D-31: a pull request that was already open when the run started is
	// never reviewed, even with --review-prs on: no review bead, no tester,
	// no gh pr review. A review bead an earlier run left for it is stale
	// with a reason that says so, not "no longer open".
	t.Run("never reviews a pull request created before the run started", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{}
		store.pwSeed("fx-earlier", "in_review", map[string]any{beads.MetaPRNumber: 12, beads.MetaRun: "r0"})
		started := time.Date(2026, 9, 2, 7, 45, 0, 0, time.UTC) // after #12, before #15
		w := NewPRWatcher(NewGH(repo), store, WithRun("r1"), WithRunStarted(started))

		res, err := w.Poll(ctx)
		require.NoError(t, err)
		require.Len(t, res.PreRun, 1)
		assert.Equal(t, 12, res.PreRun[0].Number)
		require.Len(t, res.External, 1)
		assert.Equal(t, 15, res.External[0].Number)
		require.Len(t, res.New, 1, "only the pull request opened during the run gets a review bead")
		assert.Equal(t, 15, res.New[0].PR.Number)
		assert.Empty(t, res.Existing, "the earlier run's bead for #12 is not adopted")
		require.Len(t, store.created, 1)
		require.Len(t, res.Stale, 1)
		assert.Equal(t, StalePreRun, res.Stale[0].Reason)
		assert.True(t, res.Stale[0].ForeignRun)
		assert.Equal(t, "pull request #12 was opened before this run started, so this run does not review it (its review bead was raised by an earlier run)", res.Stale[0].Explain())

		// A pull request gh reports no creation time for counts as older.
		assert.True(t, CreatedBefore(PullRequest{}, started))
		assert.False(t, CreatedBefore(PullRequest{CreatedAt: started}, started), "one created at the start itself is the run's to review")
	})

	t.Run("matches existing beads by number, url, and after restart", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{}
		// #12 known only by URL (an older bead without sc_pr_number); #15 by
		// number; both closed already, which still counts.
		store.pwSeed("fx-old12", "closed", map[string]any{beads.MetaPRURL: "https://github.com/example/fixture/pull/12"})
		store.pwSeed("fx-old15", "closed", map[string]any{beads.MetaPRNumber: "15"})
		// A stale open bead for a PR that is no longer open, and a closed one
		// that is not reported.
		store.pwSeed("fx-stale", "in_review", map[string]any{beads.MetaPRNumber: 99, beads.MetaPRURL: "https://github.com/example/fixture/pull/99"})
		store.pwSeed("fx-done", "closed", map[string]any{beads.MetaPRNumber: 98})

		res, err := NewPRWatcher(NewGH(repo), store).Poll(ctx)
		require.NoError(t, err)
		assert.Empty(t, res.New)
		require.Len(t, res.Existing, 2)
		assert.Equal(t, domain.BeadID("fx-old12"), res.Existing[0].Issue.ID)
		assert.Equal(t, domain.BeadID("fx-old15"), res.Existing[1].Issue.ID)
		require.Len(t, res.Stale, 1)
		assert.Equal(t, domain.BeadID("fx-stale"), res.Stale[0].Issue.ID)
		assert.Equal(t, StaleClosed, res.Stale[0].Reason)
		assert.Equal(t, "pull request #99 is no longer open on GitHub", res.Stale[0].Explain())
		assert.Empty(t, store.created)
	})

	// A stale review bead is not one condition, and the reason the run
	// writes into bd has to be the true one (P8-04 review gap).
	t.Run("says why each unmatched review bead is stale", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{}
		// #13 is still open, but it now carries the slop-cannon label, so
		// the poll skips it rather than reviewing it.
		store.pwSeed("fx-cannon", "in_review", map[string]any{
			beads.MetaPRNumber: 13,
			beads.MetaPRURL:    "https://github.com/example/fixture/pull/13",
			beads.MetaRun:      "r1",
		})
		// #99 really has gone, and its bead belongs to an earlier run.
		store.pwSeed("fx-old", "in_review", map[string]any{beads.MetaPRNumber: 99, beads.MetaRun: "r0"})
		// A bead with no pull request at all: no listing can ever match it.
		store.pwSeed("fx-nameless", "in_review", nil)

		res, err := NewPRWatcher(NewGH(repo), store, WithRun("r1")).Poll(ctx)
		require.NoError(t, err)
		byID := map[domain.BeadID]StaleReview{}
		for _, st := range res.Stale {
			byID[st.Issue.ID] = st
		}
		require.Len(t, byID, 3)

		cannon := byID["fx-cannon"]
		assert.Equal(t, StaleCannonPR, cannon.Reason)
		assert.Equal(t, 13, cannon.PR.Number)
		assert.False(t, cannon.ForeignRun)
		assert.Equal(t, "pull request #13 is still open but is now one of slop-cannon's own, "+
			"so it is not reviewed as an external pull request", cannon.Explain())
		assert.NotContains(t, cannon.Explain(), "no longer open")

		old := byID["fx-old"]
		assert.Equal(t, StaleClosed, old.Reason)
		assert.True(t, old.ForeignRun, "a bead of an earlier run must be marked as one")
		assert.Equal(t, "pull request #99 is no longer open on GitHub "+
			"(its review bead was raised by an earlier run)", old.Explain())

		nameless := byID["fx-nameless"]
		assert.Equal(t, StaleUnknownPR, nameless.Reason)
		assert.Equal(t, 0, nameless.Number())
		assert.Equal(t, "the review bead records no pull request number or URL, "+
			"so no pull request could be matched to it", nameless.Explain())
	})

	t.Run("one failed create does not stop the others", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{failFor: map[int]error{12: beads.ErrLocked}}
		res, err := NewPRWatcher(NewGH(repo), store).Poll(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, beads.ErrLocked)
		assert.Contains(t, err.Error(), "PR #12")
		require.NotNil(t, res)
		require.Len(t, res.New, 1)
		assert.Equal(t, 15, res.New[0].PR.Number)
	})

	t.Run("gh failure", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 4, listStderr: "gh auth login"})
		store := &pwStore{}
		res, err := NewPRWatcher(NewGH(repo), store).Poll(ctx)
		assert.ErrorIs(t, err, ErrGHUnauthenticated)
		assert.Nil(t, res)
		assert.Empty(t, store.created)
	})

	t.Run("bead listing failure creates nothing", func(t *testing.T) {
		pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{listErr: beads.ErrNoProject}
		res, err := NewPRWatcher(NewGH(repo), store).Poll(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, beads.ErrNoProject)
		require.NotNil(t, res)
		assert.Len(t, res.External, 2)
		assert.Empty(t, store.created)
	})

	t.Run("no open pull requests", func(t *testing.T) {
		pwGH(t, pwShimConfig{})
		res, err := NewPRWatcher(NewGH(repo), &pwStore{}).Poll(ctx)
		require.NoError(t, err)
		assert.Empty(t, res.External)
		assert.Empty(t, res.New)
	})
}

func TestPRWatcher_Run(t *testing.T) {
	_, repo := pwRepo(t)

	t.Run("polls on the interval until cancelled", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		store := &pwStore{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		polls := 0
		w := NewPRWatcher(NewGH(repo), store, WithPollInterval(5*time.Millisecond))
		assert.Equal(t, 5*time.Millisecond, w.Interval())
		err := w.Run(ctx, func(res *PollResult) {
			polls++
			if polls == 1 {
				assert.Len(t, res.New, 2)
			} else {
				assert.Empty(t, res.New)
				assert.Len(t, res.Existing, 2)
			}
			if polls == 3 {
				cancel()
			}
		})
		require.NoError(t, err, "a cancelled watcher stops cleanly")
		assert.Equal(t, 3, polls)
		assert.Len(t, store.created, 2, "beads are created once across polls")
		assert.GreaterOrEqual(t, len(shim.ArgLists(t)), 3)
	})

	t.Run("keeps going through transient failures", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 1, listStderr: "HTTP 502: bad gateway"})
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		polls := 0
		err := NewPRWatcher(NewGH(repo), &pwStore{}, WithPollInterval(5*time.Millisecond)).Run(ctx, func(*PollResult) { polls++ })
		require.NoError(t, err)
		assert.Zero(t, polls, "failed polls yield no result")
	})

	t.Run("stops when gh cannot work", func(t *testing.T) {
		pwGH(t, pwShimConfig{listExit: 4, listStderr: "To get started with GitHub CLI, please run:  gh auth login"})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := NewPRWatcher(NewGH(repo), &pwStore{}, WithPollInterval(time.Millisecond)).Run(ctx, nil)
		assert.ErrorIs(t, err, ErrGHUnauthenticated)
		assert.NoError(t, ctx.Err(), "returned before the deadline")
	})

	t.Run("already cancelled", func(t *testing.T) {
		shim := pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := NewPRWatcher(NewGH(repo), &pwStore{}, WithPollInterval(time.Millisecond)).Run(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, shim.ArgLists(t), "no gh call after cancellation")
	})
}

// TestPRWatcher_PollWithBD drives a real beads.Client against the FakeBD
// shim to pin the bd command shape of a review bead (D-06, P4-07): task
// type, sc:external-pr label, metadata through @file, -n 0 --flat listing.
func TestPRWatcher_PollWithBD(t *testing.T) {
	ctx := context.Background()
	_, repo := pwRepo(t)
	pwGH(t, pwShimConfig{listFile: pwTestdata(t, "pr-list.json")})

	samples := testutil.StageBDSamples(t, nil)
	require.NoError(t, os.WriteFile(filepath.Join(samples, "list.json"), []byte(`{"schema_version":1,"data":[]}`), 0o644))
	bd := testutil.FakeBD(t, samples)
	client := beads.New(repo.Root, beads.Options{})

	res, err := NewPRWatcher(NewGH(repo), client, WithRun("r1")).Poll(ctx)
	require.NoError(t, err)
	assert.Empty(t, testutil.MissingSamples(t, bd))
	require.Len(t, res.New, 2)
	assert.Equal(t, domain.BeadID("fx-a0u.1"), res.New[0].Issue.ID, "the create sample's id")

	calls := bd.ArgLists(t)
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"-C", repo.Root, "list", "--json", "-n", "0", "--flat", "--all", "-l", beads.LabelExternalPR}, calls[0])
	create := calls[1]
	assert.Equal(t, []string{"-C", repo.Root, "create", "Review PR #12: Fix Mean for empty slices", "-t", "task", "-p", "2"}, create[:8])
	assert.Contains(t, create, "--description")
	assert.Contains(t, create, "--acceptance")
	assert.Contains(t, create, "-l")
	assert.Contains(t, create, beads.LabelExternalPR+",sc:run:r1")
	assert.Contains(t, create, "--metadata")
	assert.Equal(t, "--json", create[len(create)-1])
	// P3-19: bd's own field for the pull request this bead stands for.
	extRef := slices.Index(create, "--external-ref")
	require.NotEqual(t, -1, extRef, "the create carries --external-ref: %v", create)
	assert.Equal(t, "gh-12", create[extRef+1])
	for i, a := range create {
		if a == "--metadata" {
			assert.True(t, strings.HasPrefix(create[i+1], "@"), "metadata goes through @file: %q", create[i+1])
		}
	}
	assert.Equal(t, "Review PR #15: WIP: Sum overflow guard", calls[2][3])
}

// TestClassifyGH is P4-09's table over the messages gh really prints. The
// row that matters most is the non-GitHub remote: its message ends with a
// `gh auth login` hint, which an auth-first classifier reads as "not
// logged in" and reports to the user as the cause of a push-only
// delivery.
func TestClassifyGH(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		cause  error
		reason string
	}{
		{
			name:   "no known GitHub host, with gh's auth hint",
			stderr: "none of the git remotes configured for this repository point to a known GitHub host. To tell gh about a new GitHub host, please use `gh auth login`",
			cause:  ErrNoGitHubRemote,
			reason: "the remote is not on a GitHub host gh knows",
		},
		{
			name:   "no remotes at all",
			stderr: "no git remotes found",
			cause:  ErrNoGitHubRemote,
			reason: "the remote is not on a GitHub host gh knows",
		},
		{
			name:   "base repository cannot be determined",
			stderr: "could not determine base repository: no git remotes found",
			cause:  ErrNoGitHubRemote,
			reason: "the remote is not on a GitHub host gh knows",
		},
		{
			name:   "run outside a repository",
			stderr: "fatal: not a git repository (or any of the parent directories): .git",
			cause:  ErrNoGitHubRemote,
			reason: "the remote is not on a GitHub host gh knows",
		},
		{
			name:   "never logged in",
			stderr: "To get started with GitHub CLI, please run:  gh auth login\nAlternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token.",
			cause:  ErrGHUnauthenticated,
			reason: "gh is not authenticated (run `gh auth login`)",
		},
		{
			name:   "logged out",
			stderr: "You are not logged into any GitHub hosts. To log in, run: gh auth login",
			cause:  ErrGHUnauthenticated,
			reason: "gh is not authenticated (run `gh auth login`)",
		},
		{
			name:   "token rejected",
			stderr: "HTTP 401: Bad credentials (https://api.github.com/graphql)",
			cause:  ErrGHUnauthenticated,
			reason: "gh is not authenticated (run `gh auth login`)",
		},
		{name: "network", stderr: "HTTP 502: bad gateway"},
		{name: "rate limited", stderr: "HTTP 403: API rate limit exceeded"},
		{name: "no such pull request", stderr: `no pull requests found for branch "sc/fx-1"`},
		{name: "empty", stderr: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyGH(c.stderr)
			assert.Equal(t, c.cause, got, "classifyGH(%q)", c.stderr)

			// The same table drives the reason a push-only delivery
			// reports, through the GHError the runner builds.
			err := error(&GHError{Args: []string{"pr", "list"}, ExitCode: 1, Stderr: c.stderr, Cause: got})
			reason, unavailable := ghUnavailable(err)
			assert.Equal(t, c.reason != "", unavailable)
			assert.Equal(t, c.reason, reason)
		})
	}

	t.Run("a missing binary needs no stderr", func(t *testing.T) {
		reason, ok := ghUnavailable(&GHError{Args: []string{"pr", "list"}, ExitCode: -1, Cause: ErrGHNotFound})
		assert.True(t, ok)
		assert.Equal(t, "gh is not installed", reason)
	})

	t.Run("no error is not an unavailability", func(t *testing.T) {
		reason, ok := ghUnavailable(nil)
		assert.False(t, ok)
		assert.Empty(t, reason)
	})
}
