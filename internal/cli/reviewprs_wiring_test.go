package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
)

// The external pull request reviewer, driven through an assembled run
// (P8-04).
//
// internal/orchestrator's tests drive orchestrator.PRReview by hand and
// internal/gitx's drive the watcher and gh against a shim, so both halves
// were covered — but nothing fired the whole of `sc run` with the reviewer
// switched on. Every other whole-run rig answers `gh pr list` with "[]" and
// nothing at all to `gh auth status`, so cli.decideReviewPRs said no and
// the three lines that wire the reviewer into the run — the `c.prs` poll
// goroutine, prReviewSources and prReviewPlanner — could all have been
// deleted with the suite still green.
//
// The test below is that missing one: the same machine
// [runFireSetup] builds, plus a `gh` stand-in that is authenticated and
// lists a real external pull request whose head branch is on the fixture's
// bare origin. It asserts what D-11 promises about a whole run — one review
// bead, one tester per lens fired through the real scheduler, exactly one
// `gh pr review`, no merge ever, and the review bead closed — rather than
// re-testing the component.

// prwPRNumber is the external pull request the gh stand-in lists.
const prwPRNumber = 7

// prwBranch is the contributor's head branch, pushed to the fixture's bare
// origin so the review worktree has something real to check out. It must
// not start with gitx.BranchPrefix, or the watcher would take it for one of
// the cannon's own (D-11).
const prwBranch = "contrib/typo-fix"

// prwInterval is the `pr_poll_interval` this run is configured with. It is
// short so the reviewer polls repeatedly during a run that lasts seconds,
// which is also how the key is proved to reach the watcher: the default is
// a minute, and a run this short would poll exactly once.
const prwInterval = 250 * time.Millisecond

// prwGHScript is a `gh` that is logged in, lists one external pull request,
// records every review posted, and fails loudly if anything ever asks it to
// merge. It answers the cannon's own delivery calls too, so the run reaches
// its finish the way TestRunHeadlessWithTheMockCartridge's does.
const prwGHScript = `
case "$1 $2" in
  "auth status") %s ;;
  "pr list")
    # The watcher asks for every open pull request; delivery asks whether
    # one branch already has one (--head). Only the first is the reviewer's.
    case "$*" in
      *--head*) echo '[]' ;;
      *) cat "$SC_FAKEBIN_DIR/prlist.json" ;;
    esac
    ;;
  "pr review")
    cat > "$SC_FAKEBIN_DIR/review.$SC_FAKEBIN_SEQ"
    ;;
  "pr merge")
    echo "gh shim: slop-cannon must never merge a pull request" >&2
    touch "$SC_FAKEBIN_DIR/merged"
    exit 1
    ;;
  "label create") ;;
  "pr create")
    cat > "$SC_FAKEBIN_DIR/body.$SC_FAKEBIN_SEQ"
    n=$(cat "$SC_FAKEBIN_DIR/prcount" 2>/dev/null || echo 100)
    n=$((n + 1))
    echo "$n" > "$SC_FAKEBIN_DIR/prcount"
    echo "https://github.com/acme/widgets/pull/$n"
    ;;
  "pr view") echo '{"state":"MERGED"}' ;;
  *) echo "gh shim: unexpected command: $*" >&2; exit 1 ;;
esac
`

// prwContributorBranch commits a change on a branch nobody in the run owns,
// pushes it to the bare origin and leaves the working tree back on the
// default branch with the branch deleted locally — which is exactly what a
// contributor's pull request looks like from the cannon's clone.
func prwContributorBranch(t *testing.T, env *runFireEnv) string {
	t.Helper()
	env.Repo.Git(t, "checkout", "-q", "-b", prwBranch)
	env.Repo.WriteFile(t, "CONTRIBUTING.md", "Thanks for helping out.\n")
	head := env.Repo.Commit(t, "docs: add a contributing note")
	env.Repo.Git(t, "push", "-q", "origin", prwBranch)
	env.Repo.Git(t, "checkout", "-q", env.Repo.DefaultBranch)
	env.Repo.Git(t, "branch", "-q", "-D", prwBranch)
	return head
}

// prwAuthOK and prwAuthMissing are the two answers `gh auth status` can
// give, which is what [decideReviewPRs] turns into a yes or a no (D-11).
const (
	prwAuthOK      = `echo "Logged in to github.com as tester"`
	prwAuthMissing = `echo "gh auth login" >&2; exit 1`
)

// prwGH installs the stand-in above, after [runFireSetup]'s own gh, so it
// is the one PATH resolves. head is the commit the pull request points at
// and auth is how `gh auth status` answers.
func prwGH(t *testing.T, head, auth string) *testutil.Shim {
	t.Helper()
	sh := testutil.FakeBin(t, "gh", fmt.Sprintf(prwGHScript, auth))
	list, err := json.Marshal([]map[string]any{{
		"number":      prwPRNumber,
		"title":       "Add a contributing note",
		"url":         fmt.Sprintf("https://github.com/acme/widgets/pull/%d", prwPRNumber),
		"body":        "A small documentation fix.",
		"author":      map[string]any{"login": "outsider"},
		"headRefName": prwBranch,
		"baseRefName": "main",
		"headRefOid":  head,
		"labels":      []any{},
		"isDraft":     false,
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(sh.Dir, "prlist.json"), list, 0o644))
	return sh
}

// prwSetPollInterval adds `pr_poll_interval` to the run's global
// configuration. It goes in the global file rather than a repository one so
// the target's working tree stays clean: an untracked .slop-cannon
// directory would make gitx.Manager refuse every worktree (ErrRepoDirty).
func prwSetPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	path := config.GlobalPath()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	const anchor = "gate_poll_interval = \"20ms\"\n"
	require.Contains(t, string(data), anchor, "runFireSetup's global config changed shape")
	configWriteFile(t, path, strings.Replace(string(data), anchor,
		anchor+fmt.Sprintf("pr_poll_interval = %q\n", d.String()), 1))
}

// prwEvents reads the run's event log.
func prwEvents(t *testing.T, repo string) []events.Event {
	t.Helper()
	dir := config.RunDir(repo, string(runLatestID(t, repo)))
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	evs, err := events.ReadAll(f)
	require.NoError(t, err)
	return evs
}

// TestRunReviewsAnExternalPullRequestEndToEnd is P8-04's wiring, fired for
// real: `sc <repo> -m … --headless` with a gh that lists somebody else's
// open pull request must raise a review bead, fire one tester per lens at a
// worktree holding the contributor's commit, post exactly one consolidated
// review, close the bead — and never merge anything (D-11).
func TestRunReviewsAnExternalPullRequestEndToEnd(t *testing.T) {
	env := runFireSetup(t, true)
	head := prwContributorBranch(t, env)
	gh := prwGH(t, head, prwAuthOK)
	// P8-04: the poll interval is a configuration key, so the run can be
	// asked to look more often than once a minute.
	prwSetPollInterval(t, prwInterval)

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock", "--verbose")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stdout, "run finished: completed")

	// The decision was taken and the interval came from the repository's
	// configuration rather than from gitx.DefaultPRPollInterval.
	require.NotContains(t, prwRunLog(t, env.Repo.Dir), "not reviewing external pull requests")
	rec := prwLogRecord(t, env.Repo.Dir, "reviewing external pull requests")
	require.Equal(t, "gh is installed and a remote exists", rec["reason"])
	require.EqualValues(t, prwInterval.Nanoseconds(), rec["interval"],
		"pr_poll_interval must reach the watcher and the reviewer")

	// One review bead for the pull request, and the PRs view's row for it.
	var bead domain.BeadID
	var opened int
	lenses := map[domain.Lens]bool{}
	closed := map[domain.BeadID]string{}
	for _, ev := range prwEvents(t, env.Repo.Dir) {
		switch e := ev.(type) {
		case events.PROpened:
			if e.External {
				opened++
				bead = e.Bead
				require.Equal(t, prwPRNumber, e.Number)
				require.Equal(t, prwBranch, e.Branch)
			}
		case events.AgentSpawned:
			if e.Task == events.TaskReview && e.Agent.Bead != "" && e.Agent.Bead == bead {
				lenses[e.Agent.Lens] = true
			}
		case events.BeadStateChanged:
			if e.To == domain.BeadStateClosed {
				closed[e.Bead] = e.Reason
			}
		}
	}
	require.Equal(t, 1, opened, "exactly one external pull request must be announced")
	require.NotEmpty(t, bead, "the watcher raised no review bead")
	require.Len(t, lenses, len(domain.Lenses()),
		"every lens must be fired at the pull request through the real scheduler, got %v", lenses)
	require.Contains(t, closed, bead, "the review bead was left open")

	// Exactly one consolidated review, posted with gh pr review, approving,
	// and no merge attempted by anything.
	var reviews []testutil.Invocation
	for _, in := range gh.Invocations(t) {
		require.False(t, prwArg(in.Args, 0) == "pr" && prwArg(in.Args, 1) == "merge",
			"slop-cannon must never merge a pull request it did not open (D-11)")
		if prwArg(in.Args, 0) == "pr" && prwArg(in.Args, 1) == "review" {
			reviews = append(reviews, in)
		}
	}
	require.Len(t, reviews, 1, "exactly one review must be posted for one pull request")
	var polls int
	for _, in := range gh.Invocations(t) {
		if prwIsPoll(in.Args) {
			polls++
		}
	}
	require.GreaterOrEqual(t, polls, 2,
		"pr_poll_interval of %s must make the run poll more than once", prwInterval)
	require.Equal(t,
		[]string{"pr", "review", fmt.Sprint(prwPRNumber), "--approve", "--body-file", "-"},
		reviews[0].Args)
	require.NoFileExists(t, filepath.Join(gh.Dir, "merged"))

	body := prwReviewBody(t, gh)
	require.Contains(t, body, fmt.Sprintf("## slop-cannon review of #%d", prwPRNumber))
	for _, l := range domain.Lenses() {
		require.Contains(t, body, "### "+string(l)+":")
	}
	require.Contains(t, body, "never merges pull requests it did not open")

	// D-23: reviewing somebody else's code leaves no branch of ours behind.
	branches := env.Repo.Git(t, "branch", "--list", gitx.BranchPrefix+"*")
	require.NotContains(t, branches, string(bead), "the review worktree's branch was left in the repository")
}

// prwRunLog is the run's sc.log.
func prwRunLog(t *testing.T, repo string) string {
	t.Helper()
	dir := config.RunDir(repo, string(runLatestID(t, repo)))
	data, err := os.ReadFile(filepath.Join(dir, "sc.log"))
	require.NoError(t, err)
	return string(data)
}

// prwReviewBody is the markdown the shim captured from `gh pr review
// --body-file -`.
func prwReviewBody(t *testing.T, gh *testutil.Shim) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(gh.Dir, "review.*"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "exactly one review body must have been written")
	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	return string(data)
}

// TestRunDoesNotReviewPullRequestsWhenGHIsNotAuthenticated is the other
// side of the same wiring: the same machine with a gh that is not logged in
// fires no review, posts nothing, and says why in the run log — the run
// itself still completes.
func TestRunDoesNotReviewPullRequestsWhenGHIsNotAuthenticated(t *testing.T) {
	env := runFireSetup(t, true)
	head := prwContributorBranch(t, env)
	gh := prwGH(t, head, prwAuthMissing)

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	runLog := prwRunLog(t, env.Repo.Dir)
	require.Contains(t, runLog, "not reviewing external pull requests")
	require.Contains(t, runLog, "gh is not authenticated")
	for _, in := range gh.Invocations(t) {
		require.False(t, prwIsPoll(in.Args), "nothing may poll for external pull requests: gh %v", in.Args)
		require.False(t, prwArg(in.Args, 0) == "pr" && prwArg(in.Args, 1) == "review",
			"nothing may post a review: gh %v", in.Args)
	}
	for _, ev := range prwEvents(t, env.Repo.Dir) {
		if e, ok := ev.(events.PROpened); ok {
			require.False(t, e.External, "no external pull request may be announced")
		}
	}
}

// prwLogRecord finds the one sc.log record with that message and returns
// its fields.
func prwLogRecord(t *testing.T, repo, msg string) map[string]any {
	t.Helper()
	var out map[string]any
	for _, line := range strings.Split(prwRunLog(t, repo), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != msg {
			continue
		}
		require.Nil(t, out, "the run log says %q more than once", msg)
		out = rec
	}
	require.NotNil(t, out, "the run log never says %q", msg)
	return out
}

// prwIsPoll reports whether the call is the watcher's own listing. The
// deliverer also runs `gh pr list`, but always for one branch (--head), so
// the absence of that flag is what separates the two (P4-05, P4-07).
func prwIsPoll(args []string) bool {
	if prwArg(args, 0) != "pr" || prwArg(args, 1) != "list" {
		return false
	}
	for _, a := range args {
		if a == "--head" {
			return false
		}
	}
	return true
}

// prwArg returns args[i], or "" when the call was shorter.
func prwArg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
