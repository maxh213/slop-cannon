package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/testutil"
)

// Tests for the `--review-prs` decision and the reviewer it assembles
// (P8-04, D-11). Every helper is prefixed "rpr" so it cannot collide with
// another task's file in this package. No test here reaches GitHub: `gh` is
// always a shim, and the only gh subcommand any of them provokes is `auth
// status`.

// rprGH installs a gh shim whose `auth status` exits with code and prints
// stderr.
func rprGH(t *testing.T, code int, stderr string) *testutil.Shim {
	t.Helper()
	script := "printf '%s' " + rprQuote(stderr) + " >&2\n"
	script += "exit " + rprItoa(code) + "\n"
	return testutil.FakeBin(t, "gh", script)
}

// rprQuote single-quotes s for the shim script.
func rprQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// rprItoa renders a small non-negative integer.
func rprItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// rprRepo opens a fixture repository, with a bare remote when remote is
// true, under its own git identity.
func rprRepo(t *testing.T, remote bool) *gitx.Repo {
	t.Helper()
	var opts []testutil.FixtureOption
	if remote {
		opts = append(opts, testutil.WithBareRemote())
	}
	f := testutil.NewFixtureRepo(t, opts...)
	env := map[string]string{}
	for _, kv := range f.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			env[k] = v
		}
	}
	repo, err := gitx.Open(context.Background(), f.Dir, gitx.WithEnv(env))
	if err != nil {
		t.Fatalf("gitx.Open: %v", err)
	}
	return repo
}

// rprGitOnlyPath returns a directory holding git and nothing else, for a
// test that needs git to work while gh does not exist.
func rprGitOnlyPath(t *testing.T) string {
	t.Helper()
	git, err := testutil.RealBinary("git")
	if err != nil {
		t.Skipf("no git: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatalf("linking git: %v", err)
	}
	return dir
}

// TestDecideReviewPRs is D-11's "when gh and a remote exist": the flag is
// on by default, and every reason it can still be off names itself.
func TestDecideReviewPRs(t *testing.T) {
	if testing.Short() {
		t.Skip("the gh shim spawns subprocesses")
	}
	ctx := context.Background()

	t.Run("gh and a remote", func(t *testing.T) {
		repo := rprRepo(t, true)
		rprGH(t, 0, "")
		got := decideReviewPRs(ctx, true, repo, gitx.NewGH(repo))
		if !got.Enabled {
			t.Fatalf("decision = %+v, want external reviews on", got)
		}
	})
	t.Run("the flag is off", func(t *testing.T) {
		repo := rprRepo(t, true)
		rprGH(t, 0, "")
		got := decideReviewPRs(ctx, false, repo, gitx.NewGH(repo))
		if got.Enabled || got.Reason != prReviewOff {
			t.Fatalf("decision = %+v, want off because of the flag", got)
		}
	})
	t.Run("no remote", func(t *testing.T) {
		repo := rprRepo(t, false)
		rprGH(t, 0, "")
		got := decideReviewPRs(ctx, true, repo, gitx.NewGH(repo))
		if got.Enabled || !strings.Contains(got.Reason, "no git remote") {
			t.Fatalf("decision = %+v, want off because there is no remote", got)
		}
	})
	t.Run("gh is not authenticated", func(t *testing.T) {
		repo := rprRepo(t, true)
		rprGH(t, 1, "gh: To get started with GitHub CLI, please run: gh auth login\n")
		got := decideReviewPRs(ctx, true, repo, gitx.NewGH(repo))
		if got.Enabled || !strings.Contains(got.Reason, "not authenticated") {
			t.Fatalf("decision = %+v, want off because gh is not logged in", got)
		}
	})
	t.Run("no gh", func(t *testing.T) {
		repo := rprRepo(t, true)
		// A PATH with git on it and no gh at all: the remote check must
		// still work, so the only thing missing is the GitHub CLI.
		t.Setenv("PATH", rprGitOnlyPath(t))
		got := decideReviewPRs(ctx, true, repo, gitx.NewGH(repo))
		if got.Enabled || !strings.Contains(got.Reason, "gh is not installed") {
			t.Fatalf("decision = %+v, want off because gh is missing", got)
		}
	})
}

// TestNewPRReviewer builds the run's reviewer, or does not, from the same
// decision — and never fails the run for want of gh.
func TestNewPRReviewer(t *testing.T) {
	if testing.Short() {
		t.Skip("the gh shim spawns subprocesses")
	}
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)

	setup := func(t *testing.T, want bool, remote bool) prReviewSetup {
		t.Helper()
		repo := rprRepo(t, remote)
		rc := &config.RunConfig{}
		rc.Run.ID = domain.RunID("run-rpr")
		rc.Run.RepoPath = repo.Root
		rc.Run.Config.ReviewPRs = want
		m, err := gitx.NewManager(repo, t.TempDir())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return prReviewSetup{
			RC:        rc,
			Repo:      repo,
			Worktrees: m,
			Beads:     beads.New(repo.Root, beads.Options{Logger: logger}),
			Statuses:  beads.NewStatusMap(beads.StatusList{}),
			Logger:    logger,
		}
	}

	t.Run("on", func(t *testing.T) {
		s := setup(t, true, true)
		rprGH(t, 0, "")
		reviewer, decision, err := newPRReviewer(ctx, s)
		if err != nil {
			t.Fatalf("newPRReviewer: %v", err)
		}
		if !decision.Enabled || reviewer == nil {
			t.Fatalf("decision = %+v, reviewer = %v, want a reviewer", decision, reviewer)
		}
		if reviewer.Interval() != gitx.DefaultPRPollInterval {
			t.Errorf("interval = %s, want the D-11 default %s", reviewer.Interval(), gitx.DefaultPRPollInterval)
		}
		if got := len(reviewer.Lenses()); got != len(domain.Lenses()) {
			t.Errorf("lenses = %d, want %d", got, len(domain.Lenses()))
		}
		if reviewer.StalePolicy() != orchestrator.StaleReviewClose {
			t.Errorf("stale policy = %q, want %q", reviewer.StalePolicy(), orchestrator.StaleReviewClose)
		}
	})

	t.Run("off without a remote", func(t *testing.T) {
		s := setup(t, true, false)
		rprGH(t, 0, "")
		reviewer, decision, err := newPRReviewer(ctx, s)
		if err != nil {
			t.Fatalf("newPRReviewer: %v", err)
		}
		if decision.Enabled || reviewer != nil {
			t.Fatalf("decision = %+v, reviewer = %v, want no reviewer", decision, reviewer)
		}
	})

	t.Run("off by the flag", func(t *testing.T) {
		s := setup(t, false, true)
		reviewer, decision, err := newPRReviewer(ctx, s)
		if err != nil {
			t.Fatalf("newPRReviewer: %v", err)
		}
		if decision.Enabled || reviewer != nil {
			t.Fatalf("decision = %+v, reviewer = %v, want no reviewer", decision, reviewer)
		}
	})
}

// TestPRReviewPlannerRoutesByPayload: the supervisor plans every firing
// through one planner, so the external reviewer and the feedback loop have
// to sort their own demands out between them.
func TestPRReviewPlannerRoutesByPayload(t *testing.T) {
	t.Parallel()
	var inner int
	fallback := orchestrator.FiringPlannerFunc(func(context.Context, orchestrator.Firing) (orchestrator.AgentFiring, error) {
		inner++
		return orchestrator.AgentFiring{}, nil
	})
	if got := prReviewPlanner(nil, fallback); got == nil {
		t.Fatal("prReviewPlanner(nil, inner) = nil, want inner unchanged")
	}
	if _, err := prReviewPlanner(nil, fallback).PlanFiring(context.Background(), orchestrator.Firing{}); err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	if inner != 1 {
		t.Errorf("inner planner called %d times, want 1", inner)
	}

	// A reviewer that owns no bead routes everything to the inner planner
	// too, so wiring it in never steals the run's own work.
	reviewer, err := orchestrator.NewPRReview(orchestrator.PRReviewOptions{Watcher: rprWatcher{}})
	if err != nil {
		t.Fatalf("NewPRReview: %v", err)
	}
	if _, err := prReviewPlanner(reviewer, fallback).PlanFiring(context.Background(), orchestrator.Firing{
		Demand: orchestrator.Demand{Kind: orchestrator.DemandImplement, Bead: "sc-1a2b"},
	}); err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	if inner != 2 {
		t.Errorf("inner planner called %d times, want 2", inner)
	}
	if ids := prReviewBeadIDs(reviewer); len(ids) != 0 {
		t.Errorf("review beads = %v, want none", ids)
	}
	if ids := prReviewBeadIDs(nil); ids != nil {
		t.Errorf("review beads of no reviewer = %v, want nil", ids)
	}
	src := prReviewSources(reviewer, orchestrator.WorkSourceFunc(func(context.Context) ([]orchestrator.Demand, error) {
		return []orchestrator.Demand{{Kind: orchestrator.DemandPlan}}, nil
	}))
	ds, err := src.Demands(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("Demands = %+v, %v, want the inner source's one demand", ds, err)
	}
}

// rprWatcher is a poller that finds nothing.
type rprWatcher struct{}

func (rprWatcher) Poll(context.Context) (*gitx.PollResult, error) { return &gitx.PollResult{}, nil }
func (rprWatcher) Interval() time.Duration                        { return time.Minute }
