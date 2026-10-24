package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
)

// External pull request review, from the command line's side (P8-04, D-11).
//
// `--review-prs` is on by default, but D-11 only asks for external reviews
// "when gh and a remote exist": a repository with no GitHub remote, or a
// machine with no gh, must fire the cannon exactly as before rather than
// fail or warn on every poll. So the flag is a permission, not an order —
// [decideReviewPRs] turns it into the run's actual answer, with a reason a
// user can read — and [newPRReviewer] assembles the part of the run that
// does the work ([orchestrator.PRReview]) only when the answer is yes.
//
// Nothing here talks to GitHub itself: gh is reached through [gitx.GH],
// which never merges anything (D-11).

// prReviewOff is the reason recorded when the user turned the flag off.
const prReviewOff = "--" + FlagRunReviewPRs + "=false"

// prReviewSetup is what [newPRReviewer] needs from the run assembly. Every
// field is what `sc run` already built for the rest of the run: the
// reviewer adds gh and a watcher, and shares everything else.
type prReviewSetup struct {
	// RC is the resolved run configuration; RC.Run.Config.ReviewPRs is the
	// flag and RC.Run.ID stamps the review beads.
	RC *config.RunConfig
	// Repo is the target repository, for the remote check.
	Repo *gitx.Repo
	// Worktrees is where the pull request is checked out (D-23).
	Worktrees *gitx.Manager
	// Beads is bd: the watcher creates review beads through it and the
	// reviewer closes them through it.
	Beads *beads.Client
	// States is the run's state machine, for the one bead transition D-07
	// has an edge for (needs_human).
	States *orchestrator.StateMachine
	// Host is the run, so every event the reviewer publishes reaches the
	// run log and state.json.
	Host orchestrator.EventHost
	// Statuses maps bd's registered statuses onto the D-07 states.
	Statuses beads.StatusMap
	// Context describes a bead for the prompt builders
	// ([orchestrator.Conductor.BeadContext]).
	Context orchestrator.FeedbackContextFunc
	// Interval is how often GitHub is asked for open pull requests: the
	// `pr_poll_interval` configuration key (P8-04). It is the watcher's
	// interval and the reviewer's, so one key moves both. Zero or negative
	// keeps [gitx.DefaultPRPollInterval], which is that key's default.
	Interval time.Duration
	// Filter is the run's P9-05 secrets filter. The pull request's diff is
	// the one change set that does not come from the conductor, so it is
	// filtered here instead ([filterPRDiffs]); nil still applies the
	// built-in patterns.
	Filter *agents.SecretFilter
	// Logger is the run's logger.
	Logger *slog.Logger
}

// prReviewDecision is whether this run reviews external pull requests, and
// why.
type prReviewDecision struct {
	// Enabled says whether the run polls GitHub for external pull requests.
	Enabled bool
	// Reason is one clause naming what decided it, for the log and for
	// `sc doctor`-style output ("gh is not installed", "no git remote").
	Reason string
}

// decideReviewPRs answers D-11's "when gh and a remote exist" for this run.
// want is the `--review-prs` flag: false is final, and true is a permission
// that gh and the repository still have to earn. Nothing here fails the
// run — a repository with no remote is a perfectly good target for the
// cannon, it just has no pull requests to review.
func decideReviewPRs(ctx context.Context, want bool, repo *gitx.Repo, gh *gitx.GH) prReviewDecision {
	if !want {
		return prReviewDecision{Reason: prReviewOff}
	}
	if repo == nil {
		return prReviewDecision{Reason: "no repository"}
	}
	switch remote, err := repo.HasRemote(ctx); {
	case err != nil:
		return prReviewDecision{Reason: "could not read the repository's remotes: " + err.Error()}
	case !remote:
		return prReviewDecision{Reason: "the repository has no git remote"}
	}
	if gh == nil {
		return prReviewDecision{Reason: "no gh"}
	}
	switch err := gh.Check(ctx); {
	case err == nil:
		return prReviewDecision{Enabled: true, Reason: "gh is installed and a remote exists"}
	case errors.Is(err, gitx.ErrGHNotFound):
		return prReviewDecision{Reason: "gh is not installed"}
	case errors.Is(err, gitx.ErrGHUnauthenticated):
		return prReviewDecision{Reason: "gh is not authenticated (gh auth login, or set GH_TOKEN)"}
	case errors.Is(err, gitx.ErrNoGitHubRemote):
		return prReviewDecision{Reason: "no git remote points at a GitHub repository"}
	default:
		return prReviewDecision{Reason: "gh is not usable: " + err.Error()}
	}
}

// newPRReviewer builds the external pull request reviewer for the run, or
// returns nil when this run does not review external pull requests. The
// decision is returned either way, so the caller can log one line about it
// (and `sc run --json`'s resolution could show it) instead of guessing.
//
// The reviewer it returns is an ordinary [orchestrator.WorkSource] and an
// ordinary [orchestrator.FiringPlanner]; see the package notes on
// [prReviewPlanner] for the three lines the assembly owes it.
func newPRReviewer(ctx context.Context, s prReviewSetup) (*orchestrator.PRReview, prReviewDecision, error) {
	if s.RC == nil {
		return nil, prReviewDecision{Reason: "no run configuration"}, errors.New("cli: newPRReviewer: no run configuration")
	}
	var gh *gitx.GH
	if s.Repo != nil {
		gh = gitx.NewGH(s.Repo)
	}
	decision := decideReviewPRs(ctx, s.RC.Run.Config.ReviewPRs, s.Repo, gh)
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if !decision.Enabled {
		logger.Info("not reviewing external pull requests", slog.String("reason", decision.Reason))
		return nil, decision, nil
	}

	watcher := gitx.NewPRWatcher(gh, s.Beads,
		gitx.WithRun(s.RC.Run.ID),
		// D-31: a pull request that was open before the run first started
		// (a resume keeps the original start) is never reviewed.
		gitx.WithRunStarted(s.RC.Run.StartedAt),
		gitx.WithPollInterval(s.Interval),
		gitx.WithWatcherLogger(logger))
	var checkout orchestrator.PRCheckout
	if s.Worktrees != nil {
		c, err := orchestrator.NewPRWorktreeCheckout(s.Worktrees, orchestrator.WithPRCheckoutLogger(logger))
		if err != nil {
			return nil, decision, err
		}
		// P9-05: a contributor's branch may carry a .env or a key, and the
		// tester's prompt quotes this diff.
		checkout = filterPRDiffs(c, s.Filter)
	}
	reviewer, err := orchestrator.NewPRReview(orchestrator.PRReviewOptions{
		Watcher:  watcher,
		Poster:   gh,
		Beads:    s.Beads,
		States:   s.States,
		Messages: s.Beads,
		Checkout: checkout,
		Context:  s.Context,
		Host:     s.Host,
		Run:      s.RC.Run.ID,
		Statuses: s.Statuses,
		Interval: s.Interval,
		Logger:   logger,
	})
	if err != nil {
		return nil, decision, err
	}
	logger.Info("reviewing external pull requests",
		slog.String("reason", decision.Reason),
		slog.Duration("interval", reviewer.Interval()))
	return reviewer, decision, nil
}

// prReviewPlanner routes a firing to the external pull request reviewer
// when the demand is one of its own, and to inner otherwise. It is how the
// supervisor's single [orchestrator.FiringPlanner] serves both loops: the
// reviewer's demands carry an [orchestrator.PRReviewWork] payload the
// feedback loop would refuse, and the feedback loop's carry one the
// reviewer would.
//
// A nil reviewer returns inner unchanged, so the wrap site is
// unconditional.
func prReviewPlanner(r *orchestrator.PRReview, inner orchestrator.FiringPlanner) orchestrator.FiringPlanner {
	if r == nil {
		return inner
	}
	return orchestrator.FiringPlannerFunc(func(ctx context.Context, fi orchestrator.Firing) (orchestrator.AgentFiring, error) {
		if r.Handles(fi.Demand) {
			return r.PlanFiring(ctx, fi)
		}
		if inner == nil {
			return orchestrator.AgentFiring{}, fmt.Errorf("%w: %s", orchestrator.ErrUnknownFeedbackWork, fi.Demand.Kind)
		}
		return inner.PlanFiring(ctx, fi)
	})
}

// prReviewSources adds the reviewer to the run's work sources when there is
// one, so the scheduler offers external reviews beside the run's own work
// under the same D-05 allocation. A nil reviewer changes nothing.
func prReviewSources(r *orchestrator.PRReview, sources ...orchestrator.WorkSource) orchestrator.WorkSource {
	if r != nil {
		sources = append(sources, r)
	}
	return orchestrator.WorkSources(sources...)
}
