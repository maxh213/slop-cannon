package gitx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// dpPRURL is the pull request URL the gh shim prints for a created PR.
const dpPRURL = "https://github.com/acme/widgets/pull/7"

// dpLogin is the login `gh api user` answers with, and dpStarted the start
// of the run dpRequest delivers for (D-31).
const dpLogin = "sc-bot"

var dpStarted = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// dpPR renders one `gh pr list` entry for the fixture bead's branch that
// passes every clause of [OwnedByRun] for dpRequest; edit tweaks the raw
// fields first, so a test can break exactly one clause.
func dpPR(t *testing.T, n int, state, branch string, edit func(map[string]any)) string {
	t.Helper()
	pr := map[string]any{
		"number":      n,
		"url":         fmt.Sprintf("https://github.com/acme/widgets/pull/%d", n),
		"state":       state,
		"title":       "Fix the mean() bug!",
		"headRefName": branch,
		"baseRefName": "main",
		"labels":      []map[string]string{{"name": CannonPRLabel}},
		"body":        fmt.Sprintf("<!-- slop-cannon bead=%s run=run-1 branch=%s -->\n## Bead\n", wtBead.ID, branch),
		"author":      map[string]string{"login": dpLogin},
		"createdAt":   dpStarted.Add(time.Hour).Format(time.RFC3339),
	}
	if edit != nil {
		edit(pr)
	}
	out, err := json.Marshal(pr)
	require.NoError(t, err)
	return string(out)
}

// dpPRs renders a `gh pr list` answer.
func dpPRs(entries ...string) string { return "[" + strings.Join(entries, ",") + "]" }

// dpTracker is an in-memory PRTracker recording what Deliver asked of the
// beads client.
type dpTracker struct {
	mu       sync.Mutex
	meta     map[domain.BeadID]map[string]any
	statuses []domain.BeadState
	gates    []beads.Gate
	created  []int
	resolved []string // "<gate>: <reason>"

	err        error
	resolveErr error
}

func (tr *dpTracker) SetMetadata(_ context.Context, id domain.BeadID, meta map[string]any) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.meta == nil {
		tr.meta = map[domain.BeadID]map[string]any{}
	}
	if tr.meta[id] == nil {
		tr.meta[id] = map[string]any{}
	}
	for k, v := range meta {
		tr.meta[id][k] = v
	}
	return beads.Issue{ID: id}, nil
}

func (tr *dpTracker) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	tr.statuses = append(tr.statuses, state)
	return beads.Issue{ID: id, Status: string(state)}, nil
}

func (tr *dpTracker) CreatePRGate(_ context.Context, bead domain.BeadID, prNumber int, reason string) (beads.Gate, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.err != nil {
		return beads.Gate{}, tr.err
	}
	g := beads.Gate{
		ID:          domain.BeadID(fmt.Sprintf("gate-%d", len(tr.gates)+1)),
		Status:      "open",
		AwaitType:   beads.GateTypePR,
		AwaitID:     fmt.Sprint(prNumber),
		Blocks:      bead,
		Description: "Ad-hoc gate blocking " + string(bead) + "\n\nReason: " + reason,
	}
	tr.gates = append(tr.gates, g)
	tr.created = append(tr.created, prNumber)
	return g, nil
}

func (tr *dpTracker) GateFor(_ context.Context, bead domain.BeadID) (beads.Gate, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.err != nil {
		return beads.Gate{}, tr.err
	}
	for i := len(tr.gates) - 1; i >= 0; i-- {
		if tr.gates[i].Blocks == bead && tr.gates[i].IsOpen() {
			return tr.gates[i], nil
		}
	}
	return beads.Gate{}, fmt.Errorf("beads: %w: %s", beads.ErrNoGate, bead)
}

func (tr *dpTracker) ResolveGate(_ context.Context, id domain.BeadID, reason string) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.resolveErr != nil {
		return tr.resolveErr
	}
	for i, g := range tr.gates {
		if g.ID != id {
			continue
		}
		g.Status, g.CloseReason = "closed", reason
		tr.gates[i] = g
		tr.resolved = append(tr.resolved, fmt.Sprintf("%s: %s", id, reason))
		return nil
	}
	return fmt.Errorf("beads: %w: gate %s", beads.ErrNotFound, id)
}

// dpSeedGate registers an open gh:pr gate for bead and PR number n, the
// way a previous, interrupted delivery would have left it.
func (tr *dpTracker) dpSeedGate(bead domain.BeadID, n int) beads.Gate {
	g := beads.Gate{ID: domain.BeadID(fmt.Sprintf("seed-%d", n)), Status: "open", AwaitType: beads.GateTypePR, AwaitID: fmt.Sprint(n), Blocks: bead}
	tr.gates = append(tr.gates, g)
	return g
}

// dpGH installs a gh shim driven by files in its directory: "mode" (ok,
// noauth, nohost, nolabel) and "prs.json" (what `gh pr list` prints). A
// created pull request is always dpPRURL; the body arrives on stdin and
// is recorded with the invocation.
func dpGH(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "gh", `
mode=$(cat "$SC_FAKEBIN_DIR/mode" 2>/dev/null || echo ok)
case "$mode" in
  noauth) echo "You are not logged into any GitHub hosts. To log in, run:  gh auth login" >&2; exit 4 ;;
  nohost) echo "none of the git remotes configured for this repository point to a known GitHub host. To tell gh about a new GitHub host, please use 'gh auth login'" >&2; exit 1 ;;
esac
case "$1 $2" in
  "pr list") cat "$SC_FAKEBIN_DIR/prs.json" 2>/dev/null || echo '[]' ;;
  "label create") if [ "$mode" = nolabel ]; then echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; fi ;;
  "pr create") echo "Creating pull request for $4 into $6 in acme/widgets"; echo '`+dpPRURL+`' ;;
  "pr close") echo "Closed pull request acme/widgets#$3" ;;
  "api user")
    if [ -s "$SC_FAKEBIN_DIR/login-fails" ]; then cat "$SC_FAKEBIN_DIR/login-fails" >&2; exit 1; fi
    if [ -f "$SC_FAKEBIN_DIR/login-fails" ]; then echo "HTTP 502: Bad Gateway" >&2; exit 1; fi
    cat "$SC_FAKEBIN_DIR/login" 2>/dev/null || echo `+dpLogin+` ;;
  "pr view") cat "$SC_FAKEBIN_DIR/view.json" ;;
  "pr merge")
    if [ -f "$SC_FAKEBIN_DIR/merge-fails" ]; then cat "$SC_FAKEBIN_DIR/merge-fails" >&2; exit 1; fi
    echo "Squashed and merged pull request acme/widgets#$3" ;;
  "pr update-branch") echo "Updated pull request acme/widgets#$3" ;;
  *) echo "gh shim: unexpected command: $*" >&2; exit 1 ;;
esac
`, testutil.WithStdinCapture())
}

// dpSet writes a control file of the gh shim.
func dpSet(t *testing.T, gh *testutil.Shim, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(gh.Dir, name), []byte(content), 0o644))
}

// dpFixture is a fixture repo with a worktree holding one commit of work
// for wtBead, a gh shim, a recording tracker, and a deliverer.
type dpFixture struct {
	*wtFixture
	gh      *testutil.Shim
	tracker *dpTracker
	d       *PRDeliverer
	wt      *Worktree
	head    string
}

// dpSetup builds the fixture; with remote false the repository has no
// remote at all.
func dpSetup(t *testing.T, remote bool) *dpFixture {
	t.Helper()
	var opts []testutil.FixtureOption
	if remote {
		opts = append(opts, testutil.WithBareRemote())
	}
	f := wtSetup(t, opts)
	ctx := context.Background()
	wt, err := f.m.Create(ctx, wtBead)
	require.NoError(t, err)
	wtRepo, err := f.m.Open(ctx, wtBead.ID)
	require.NoError(t, err)
	acWrite(t, wt.Path, "stats/NOTES.md", "mean starts at index 0 now\n")
	c, err := wtRepo.CommitAll(ctx, "fix mean")
	require.NoError(t, err)
	require.True(t, c.Committed)

	gh := dpGH(t)
	tracker := &dpTracker{}
	d, err := NewPRDeliverer(f.m, tracker, WithPRLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	require.NoError(t, err)
	df := &dpFixture{wtFixture: f, gh: gh, tracker: tracker, d: d, wt: wt, head: c.SHA}

	// Safety on never touches the default branch: whatever a subtest does,
	// the local and remote main tips must be where they started.
	mainBefore := f.r.Head(t)
	var remoteBefore string
	if remote {
		remoteBefore = dpRemoteHeads(t, df)["main"]
	}
	t.Cleanup(func() {
		assert.Equal(t, mainBefore, f.r.Head(t), "the local default branch must not move (safety on)")
		if remote {
			assert.Equal(t, remoteBefore, dpRemoteHeads(t, df)["main"], "the remote default branch must not move (safety on)")
		}
	})
	return df
}

// dpRequest is the delivery request for wtBead with a report and verdicts.
func dpRequest() PRRequest {
	return PRRequest{
		Bead:       domain.Bead{ID: wtBead.ID, Title: wtBead.Title, Type: domain.BeadTypeBug, Priority: 1, Acceptance: "Mean returns the average of all values."},
		Run:        "run-1",
		RunStarted: dpStarted,
		Report: &domain.ImplReport{
			Summary:      "Start the loop at index 0.",
			FilesChanged: []string{"stats/mean.go", "stats/mean_test.go"},
			HowToVerify:  "go test ./...",
			Risks:        []string{"None known"},
		},
		Verdicts: []domain.Verdict{
			{Lens: domain.LensUX, Round: 1, Outcome: domain.VerdictApprove, Summary: "Error text | reads well"},
			{Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictApprove, Summary: "Edge cases covered"},
			{Lens: domain.LensTest, Round: 2, Outcome: domain.VerdictApprove, Summary: "Regression test added"},
		},
		Rounds: 2,
	}
}

// dpGitHubRemote makes origin look like a GitHub repository to gh while
// pushes still land in the local bare remote.
func dpGitHubRemote(t *testing.T, f *dpFixture) {
	t.Helper()
	f.r.Git(t, "remote", "set-url", "--push", "origin", f.r.RemoteDir)
	f.r.Git(t, "remote", "set-url", "origin", "https://github.com/acme/widgets.git")
}

// dpRemoteHeads returns the remote's branch tips by name.
func dpRemoteHeads(t *testing.T, f *dpFixture) map[string]string {
	t.Helper()
	heads := map[string]string{}
	out := f.r.Git(t, "ls-remote", "--heads", f.r.RemoteDir)
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok {
			heads[strings.TrimPrefix(ref, "refs/heads/")] = sha
		}
	}
	return heads
}

// dpHideGH puts a PATH in place that has git (and the POSIX tools the git
// shims need) but no gh.
func dpHideGH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"git", "sh"} {
		real, err := testutil.RealBinary(name)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(real, filepath.Join(dir, name)))
	}
	t.Setenv("PATH", dir)
}

// dpGolden compares got with testdata/gh/<name>; UPDATE_GOLDEN=1 rewrites it.
func dpGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "gh", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// dpCalls returns the gh invocations as "sub command" keys in order.
func dpCalls(t *testing.T, gh *testutil.Shim) []string {
	t.Helper()
	var out []string
	for _, inv := range gh.Invocations(t) {
		if len(inv.Args) >= 2 {
			out = append(out, inv.Args[0]+" "+inv.Args[1])
		} else {
			out = append(out, strings.Join(inv.Args, " "))
		}
	}
	return out
}

func TestPRTitle(t *testing.T) {
	assert.Equal(t, "Fix the mean() bug!", PRTitle(wtBead))
	assert.Equal(t, "Bead fx-1", PRTitle(domain.Bead{ID: "fx-1", Title: "  \n "}))
	assert.Equal(t, "a b c", PRTitle(domain.Bead{ID: "fx-1", Title: " a\tb\n c "}), "whitespace is collapsed")
	long := PRTitle(domain.Bead{ID: "fx-1", Title: strings.Repeat("x", 500)})
	assert.LessOrEqual(t, len(long), maxPRTitleLen)
	assert.True(t, strings.HasSuffix(long, "…"))
	assert.True(t, utf8.ValidString(long))

	// Truncation lands on a rune boundary whatever the byte offset of the
	// cut falls in the middle of; GitHub rejects invalid UTF-8 titles.
	for _, pad := range []int{0, 1, 2} {
		title := PRTitle(domain.Bead{ID: "fx-1", Title: strings.Repeat("a", pad) + strings.Repeat("é", 400)})
		assert.LessOrEqual(t, len(title), maxPRTitleLen, "pad %d", pad)
		assert.True(t, utf8.ValidString(title), "pad %d: %q", pad, title)
		assert.True(t, strings.HasSuffix(title, "…"), "pad %d", pad)
	}
}

func TestPRBody(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		body := PRBody(dpRequest(), "sc/fx-a0u-fix-the-mean-bug", "main")
		dpGolden(t, "pr-body.golden.md", body)
		assert.True(t, strings.HasSuffix(body, "\n---\n"+PRMarker+"\n"))
		assert.Contains(t, body, "<!-- slop-cannon bead=fx-a0u run=run-1 branch=sc/fx-a0u-fix-the-mean-bug -->")
		assert.Contains(t, body, `Error text \| reads well`, "pipes are escaped in table cells")
	})

	t.Run("minimal", func(t *testing.T) {
		body := PRBody(PRRequest{Bead: domain.Bead{ID: "fx-1", Title: "Tiny"}}, "sc/fx-1-tiny", "master")
		assert.Contains(t, body, "_No implementation report was recorded._")
		assert.Contains(t, body, "_No tester verdicts were recorded._")
		assert.Contains(t, body, "0 review rounds.")
		assert.Contains(t, body, "`sc/fx-1-tiny` into `master`")
		assert.NotContains(t, body, "**Run**")
		assert.NotContains(t, body, "**Type**")
		assert.True(t, strings.HasSuffix(body, PRMarker+"\n"))
	})

	t.Run("one round and a lens-less verdict", func(t *testing.T) {
		body := PRBody(PRRequest{Bead: domain.Bead{ID: "fx-1"}, Rounds: 1, Verdicts: []domain.Verdict{{Outcome: domain.VerdictChanges}}}, "b", "main")
		assert.Contains(t, body, "1 review round.")
		assert.Contains(t, body, "| - | 0 | changes |  |")
	})
}

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		in                string
		host, owner, name string
		ok                bool
	}{
		{"https://github.com/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"https://github.com/acme/widgets", "github.com", "acme", "widgets", true},
		{"http://GitHub.com/acme/widgets/", "github.com", "acme", "widgets", true},
		{"git@github.com:acme/widgets.git", "github.com", "acme", "widgets", true},
		{"ssh://git@github.com/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"git://github.com/acme/widgets.git", "github.com", "acme", "widgets", true},
		{"https://github.corp.example.com/team/repo.git", "github.corp.example.com", "team", "repo", true},
		{"https://gitlab.com/acme/widgets.git", "", "", "", false},
		{"git@gitlab.com:acme/widgets.git", "", "", "", false},
		{"https://github.com/acme", "", "", "", false},
		{"https://github.com/acme/widgets/extra", "", "", "", false},
		{"/srv/git/remote.git", "", "", "", false},
		{"", "", "", "", false},
		{"github.com:acme/widgets", "github.com", "acme", "widgets", true},
	}
	for _, c := range cases {
		host, owner, name, ok := ParseGitHubRemote(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		assert.Equal(t, c.host, host, c.in)
		assert.Equal(t, c.owner, owner, c.in)
		assert.Equal(t, c.name, name, c.in)
	}
}

func TestCompareURL(t *testing.T) {
	assert.Equal(t, "https://github.com/acme/widgets/compare/main...sc/fx-a0u-fix?expand=1", CompareURL("git@github.com:acme/widgets.git", "main", "sc/fx-a0u-fix"))
	assert.Equal(t, "https://github.com/acme/widgets/compare/release/1.0...sc/x%20y?expand=1", CompareURL("https://github.com/acme/widgets", "release/1.0", "sc/x y"), "slashes inside branch names survive as separators; other reserved characters are escaped")
	assert.Equal(t, "", CompareURL("/srv/git/remote.git", "main", "sc/x"))
	assert.Equal(t, "", CompareURL("https://github.com/acme/widgets", "", "sc/x"))
}

func TestNewPRDeliverer(t *testing.T) {
	f := wtSetup(t, nil)
	_, err := NewPRDeliverer(nil, &dpTracker{})
	require.Error(t, err)
	_, err = NewPRDeliverer(f.m, nil)
	require.Error(t, err)
	d, err := NewPRDeliverer(f.m, &dpTracker{})
	require.NoError(t, err)
	assert.Same(t, f.repo, d.gh.Repo())
}

func TestPRDeliverer_Deliver(t *testing.T) {
	ctx := context.Background()

	t.Run("pushes the branch and opens a labelled pull request", func(t *testing.T) {
		f := dpSetup(t, true)
		mainBefore := f.r.Head(t)
		remoteBefore := dpRemoteHeads(t, f)

		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, wtBead.ID, res.Bead)
		assert.Equal(t, f.wt.Branch, res.Branch)
		assert.Equal(t, f.head, res.Head)
		assert.Equal(t, "main", res.Base)
		assert.Equal(t, "origin", res.Remote)
		repoSamePath(t, f.r.RemoteDir, res.RemoteURL)
		assert.True(t, res.Pushed)
		require.NotNil(t, res.PR)
		assert.Equal(t, 7, res.PR.Number)
		assert.Equal(t, dpPRURL, res.PR.URL)
		assert.Equal(t, "Fix the mean() bug!", res.PR.Title)
		assert.Equal(t, f.wt.Branch, res.PR.HeadRef)
		assert.Equal(t, "main", res.PR.BaseRef)
		assert.Equal(t, []string{CannonPRLabel}, res.PR.Labels)
		assert.True(t, IsCannonPR(*res.PR), "the watcher must skip it (D-11)")
		assert.Equal(t, "OPEN", res.PRState)
		assert.Equal(t, domain.BeadID("gate-1"), res.Gate)
		assert.Equal(t, domain.BeadStateDelivering, res.State)
		assert.Empty(t, res.Warnings)
		assert.Contains(t, res.Summary(), "opened PR #7")

		// The branch reached the remote; the default branch did not move
		// anywhere (safety on never touches it).
		remoteAfter := dpRemoteHeads(t, f)
		assert.Equal(t, f.head, remoteAfter[f.wt.Branch])
		assert.Equal(t, remoteBefore["main"], remoteAfter["main"])
		assert.Equal(t, mainBefore, f.r.Head(t))
		assert.Equal(t, "main", f.r.Git(t, "symbolic-ref", "--short", "HEAD"))
		assert.Empty(t, f.r.Status(t))
		assert.NotContains(t, f.r.Git(t, "config", "--list", "--local"), "branch."+f.wt.Branch+".", "no upstream is written into the repository config")

		// gh was driven as specified.
		assert.Equal(t, []string{"pr list", "label create", "pr create"}, dpCalls(t, f.gh))
		calls := f.gh.Invocations(t)
		assert.Equal(t, []string{"pr", "list", "--head", f.wt.Branch, "--state", "all", "--limit", "50", "--json", "number,url,state,title,headRefName,baseRefName,labels,body,author,createdAt"}, calls[0].Args)
		assert.Equal(t, []string{"label", "create", CannonPRLabel, "--color", prLabelColor, "--description", prLabelDescription, "--force"}, calls[1].Args)
		assert.Equal(t, []string{"pr", "create", "--head", f.wt.Branch, "--base", "main", "--title", "Fix the mean() bug!", "--body-file", "-", "--label", CannonPRLabel}, calls[2].Args)
		assert.Equal(t, PRBody(dpRequest(), f.wt.Branch, "main"), calls[2].Stdin)
		for _, c := range calls {
			repoSamePath(t, f.repo.Root, c.Dir)
		}

		// Bead bookkeeping: metadata, gate, and no status change.
		assert.Equal(t, map[string]any{beads.MetaPRURL: dpPRURL, beads.MetaPRNumber: 7, beads.MetaPRHead: f.head}, f.tracker.meta[wtBead.ID], "sc_pr_head is the tip the testers approved")
		assert.Equal(t, []int{7}, f.tracker.created)
		assert.Equal(t, wtBead.ID, f.tracker.gates[0].Blocks)
		assert.Contains(t, f.tracker.gates[0].Description, "Reason: PR #7: Fix the mean() bug!")
		assert.Empty(t, f.tracker.statuses)
	})

	t.Run("is idempotent: a retry reuses the open pull request and its gate", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "prs.json", dpPRs(dpPR(t, 3, "MERGED", f.wt.Branch, nil), dpPR(t, 5, "OPEN", f.wt.Branch, nil)))
		seed := f.tracker.dpSeedGate(wtBead.ID, 5)

		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRExisting, res.Status)
		require.NotNil(t, res.PR)
		assert.Equal(t, 5, res.PR.Number)
		assert.Equal(t, "OPEN", res.PRState)
		assert.Equal(t, seed.ID, res.Gate, "the existing gate is reused")
		assert.Equal(t, domain.BeadStateDelivering, res.State)
		assert.Empty(t, res.Warnings)
		assert.Equal(t, []string{"pr list", "api user"}, dpCalls(t, f.gh), "nothing is created; the login is read to judge the pull request")
		assert.Equal(t, map[string]any{beads.MetaPRURL: "https://github.com/acme/widgets/pull/5", beads.MetaPRNumber: 5, beads.MetaPRHead: f.head}, f.tracker.meta[wtBead.ID])
		assert.Empty(t, f.tracker.created)
		assert.Contains(t, res.Summary(), "reusing PR #5")

		// Delivering again is a no-op apart from the push and the lookup.
		f.gh.Reset(t)
		res2, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRExisting, res2.Status)
		assert.Equal(t, seed.ID, res2.Gate)
		assert.Equal(t, []string{"pr list"}, dpCalls(t, f.gh), "the login is cached")
	})

	t.Run("a merged pull request is reused over a closed one; closed alone means a new one", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "prs.json", dpPRs(dpPR(t, 4, "CLOSED", f.wt.Branch, nil), dpPR(t, 2, "MERGED", f.wt.Branch, nil)))
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRExisting, res.Status)
		assert.Equal(t, 2, res.PR.Number)
		assert.Equal(t, "MERGED", res.PRState)
		assert.Equal(t, []int{2}, f.tracker.created, "a gate is created so the poller resolves it at once")

		f.gh.Reset(t)
		dpSet(t, f.gh, "prs.json", `[{"number":4,"url":"https://github.com/acme/widgets/pull/4","state":"CLOSED","title":"abandoned"}]`)
		res, err = f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, 7, res.PR.Number)
		assert.Equal(t, []string{"pr list", "label create", "pr create"}, dpCalls(t, f.gh))
	})

	// A resume re-runs the delivery of a bead whose pull request a human
	// already merged, possibly after pushing to it: the local tip is then
	// behind the remote branch and a push could only fail. Nothing is
	// pushed; the gate is still made, so the poller closes the bead.
	t.Run("does not push the branch of a pull request that has already merged", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "prs.json", dpPRs(dpPR(t, 2, "MERGED", f.wt.Branch, nil)))
		remoteBefore := dpRemoteHeads(t, f)

		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRExisting, res.Status)
		assert.Equal(t, "MERGED", res.PRState)
		assert.False(t, res.Pushed)
		assert.Equal(t, remoteBefore, dpRemoteHeads(t, f), "the remote is untouched")
		assert.NotContains(t, dpRemoteHeads(t, f), f.wt.Branch)
		assert.Equal(t, []int{2}, f.tracker.created)
		assert.Equal(t, []string{"pr list", "api user"}, dpCalls(t, f.gh))
	})

	// P6-14/P6-22, the retry half: a gh:pr gate left behind by a pull
	// request that was abandoned is resolved before the gate for this one
	// is created. Under D-20 a bead cannot close while any gate blocks it,
	// so a second gate beside the first would leave the bead unable to
	// close even after this pull request merges.
	t.Run("clears a gate for an abandoned pull request before creating the right one", func(t *testing.T) {
		f := dpSetup(t, true)
		stale := f.tracker.dpSeedGate(wtBead.ID, 9)
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, domain.BeadID("gate-2"), res.Gate)
		assert.Equal(t, []int{7}, f.tracker.created)
		assert.Equal(t, []string{"seed-9: superseded by pull request #7"}, f.tracker.resolved)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], string(stale.ID))
		assert.Contains(t, res.Warnings[0], "not for PR #7")
		assert.Contains(t, res.Warnings[0], "it was resolved")

		// Exactly one gate blocks the bead, and it is this delivery's.
		open, err := f.tracker.GateFor(ctx, wtBead.ID)
		require.NoError(t, err)
		assert.Equal(t, res.Gate, open.ID)
		n, ok := open.PRNumber()
		require.True(t, ok)
		assert.Equal(t, 7, n)
	})

	// If the stale gate cannot be cleared, no second gate is created: the
	// delivery fails and says why, so a retry can try again rather than
	// leaving the bead permanently blocked.
	t.Run("refuses to add a second gate when the stale one will not clear", func(t *testing.T) {
		f := dpSetup(t, true)
		f.tracker.dpSeedGate(wtBead.ID, 9)
		f.tracker.resolveErr = errors.New("bd: exit status 1")

		_, err := f.d.Deliver(ctx, dpRequest())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clearing the stale gh:pr gate seed-9")
		assert.Empty(t, f.tracker.created, "no second gate is created")
	})

	// A gate of another type is not delivery's to clear: it is reported and
	// left for whoever created it.
	t.Run("warns about a gate of another type and leaves it alone", func(t *testing.T) {
		f := dpSetup(t, true)
		f.tracker.gates = append(f.tracker.gates, beads.Gate{
			ID: "gate-human", Status: "open", AwaitType: "human", AwaitID: "review", Blocks: wtBead.ID,
		})
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, []int{7}, f.tracker.created)
		assert.Empty(t, f.tracker.resolved)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "gate-human")
		assert.Contains(t, res.Warnings[0], "is already blocked by")
	})

	t.Run("opens the pull request without a label when the label cannot be created", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "mode", "nolabel")
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Empty(t, res.PR.Labels)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], `could not create label "slop-cannon"`)
		assert.Contains(t, res.Warnings[0], "HTTP 403")
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 3)
		assert.NotContains(t, calls[2].Args, "--label")
		assert.Equal(t, domain.BeadStateDelivering, res.State)
	})

	t.Run("without gh it pushes, prints the compare URL, and hands the bead to a human", func(t *testing.T) {
		f := dpSetup(t, true)
		dpGitHubRemote(t, f)
		dpHideGH(t)

		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRPushOnly, res.Status)
		assert.True(t, res.Pushed)
		assert.Equal(t, f.head, dpRemoteHeads(t, f)[f.wt.Branch], "the branch was pushed anyway")
		assert.Equal(t, "https://github.com/acme/widgets.git", res.RemoteURL)
		assert.Equal(t, "gh is not installed", res.Reason)
		assert.Equal(t, "open the pull request yourself: https://github.com/acme/widgets/compare/main..."+f.wt.Branch+"?expand=1", res.Instructions)
		assert.Nil(t, res.PR)
		assert.Empty(t, res.Gate)
		assert.Equal(t, domain.BeadStateNeedsHuman, res.State)
		assert.Equal(t, []domain.BeadState{domain.BeadStateNeedsHuman}, f.tracker.statuses)
		assert.Empty(t, f.tracker.meta, "no PR to record")
		assert.Empty(t, f.tracker.created, "no gate can be checked without gh")
		assert.Empty(t, f.gh.Invocations(t))
		assert.Contains(t, res.Summary(), "gh is not installed")
	})

	t.Run("unauthenticated gh is push only", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "mode", "noauth")
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRPushOnly, res.Status)
		assert.True(t, res.Pushed)
		assert.Equal(t, "gh is not authenticated (run `gh auth login`)", res.Reason)
		assert.Equal(t, "open a pull request from "+f.wt.Branch+" to main on origin ("+res.RemoteURL+") yourself", res.Instructions, "no compare URL for a non-GitHub remote")
		assert.Equal(t, domain.BeadStateNeedsHuman, res.State)
		assert.Equal(t, []domain.BeadState{domain.BeadStateNeedsHuman}, f.tracker.statuses)
		assert.Equal(t, []string{"pr list"}, dpCalls(t, f.gh), "gh gives up at the first call")
	})

	t.Run("a remote gh does not know is push only", func(t *testing.T) {
		f := dpSetup(t, true)
		dpSet(t, f.gh, "mode", "nohost")
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRPushOnly, res.Status)
		assert.Equal(t, "the remote is not on a GitHub host gh knows", res.Reason)
		assert.Equal(t, domain.BeadStateNeedsHuman, res.State)
	})

	// P4-12: the product owner retitled the bead after its branch was cut,
	// so BranchName no longer names it. The delivery must still push the
	// branch the work is on rather than fail with ErrBranchMissing.
	t.Run("a retitled bead delivers the branch its work is on", func(t *testing.T) {
		f := dpSetup(t, true)
		dpGitHubRemote(t, f)
		req := dpRequest()
		req.Bead.Title = "a much better title the PO thought of later"
		require.NotEqual(t, f.wt.Branch, BranchName(req.Bead))

		res, err := f.d.Deliver(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, f.wt.Branch, res.Branch, "the work's branch, not the new title's")
		assert.Equal(t, f.head, dpRemoteHeads(t, f)[f.wt.Branch])
		assert.Equal(t, domain.BeadStateDelivering, res.State)
	})

	t.Run("no remote: the branch stays local and the bead goes to a human", func(t *testing.T) {
		f := dpSetup(t, false)
		res, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		assert.Equal(t, PRNoRemote, res.Status)
		assert.False(t, res.Pushed)
		assert.Empty(t, res.Remote)
		assert.Equal(t, "the repository has no remote", res.Reason)
		assert.Equal(t, "branch "+f.wt.Branch+" is left in "+f.repo.Root+"; merge it into main yourself", res.Instructions)
		assert.Equal(t, domain.BeadStateNeedsHuman, res.State)
		assert.Equal(t, []domain.BeadState{domain.BeadStateNeedsHuman}, f.tracker.statuses)
		assert.Empty(t, f.gh.Invocations(t), "gh is never called")
		assert.Equal(t, f.head, f.r.Git(t, "rev-parse", "refs/heads/"+f.wt.Branch), "the branch is untouched")
		assert.Contains(t, res.Summary(), "no remote")
	})

	t.Run("refuses a bead without a branch or an id", func(t *testing.T) {
		f := dpSetup(t, true)
		_, err := f.d.Deliver(ctx, PRRequest{Bead: domain.Bead{ID: "fx-none", Title: "never implemented"}})
		require.ErrorIs(t, err, ErrBranchMissing)
		_, err = f.d.Deliver(ctx, PRRequest{})
		require.Error(t, err)
		assert.Empty(t, f.gh.Invocations(t))
		assert.Empty(t, f.tracker.statuses)
	})

	t.Run("a rejected push is an error", func(t *testing.T) {
		f := dpSetup(t, true)
		_, err := f.d.Deliver(ctx, dpRequest())
		require.NoError(t, err)
		// Rewrite the branch so the remote's copy is no longer an ancestor.
		wtRepo, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)
		_, err = wtRepo.Git(ctx, "commit", "--amend", "--quiet", "--no-verify", "-m", "rewritten")
		require.NoError(t, err)

		_, err = f.d.Deliver(ctx, dpRequest())
		require.ErrorIs(t, err, ErrPushRejected)
		var ge *GitError
		require.ErrorAs(t, err, &ge)
		assert.Contains(t, ge.Stderr, "rejected")
	})

	t.Run("tracker failures surface", func(t *testing.T) {
		f := dpSetup(t, true)
		f.tracker.err = errors.New("bd is down")
		_, err := f.d.Deliver(ctx, dpRequest())
		require.ErrorContains(t, err, "bd is down")
		assert.Contains(t, err.Error(), "recording PR on fx-a0u")

		f = dpSetup(t, false)
		f.tracker.err = errors.New("bd is down")
		_, err = f.d.Deliver(ctx, dpRequest())
		require.ErrorContains(t, err, "moving fx-a0u to needs_human")
	})

	t.Run("a custom base branch is used for the pull request", func(t *testing.T) {
		f := dpSetup(t, true)
		f.r.Git(t, "branch", "release", "main")
		res, err := f.d.Deliver(ctx, PRRequest{Bead: wtBead, Base: "release"})
		require.NoError(t, err)
		assert.Equal(t, "release", res.Base)
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 3)
		assert.Contains(t, calls[2].Args, "release")
		assert.Contains(t, calls[2].Stdin, "into `release`")
	})
}

// TestPRDeliverer_WithBeadsClient wires the deliverer to a real
// *beads.Client backed by the bd shim, so the metadata and gate calls are
// the ones bd 1.2.2 accepts.
func TestPRDeliverer_WithBeadsClient(t *testing.T) {
	ctx := context.Background()
	f := dpSetup(t, true)
	t.Setenv("HOME", t.TempDir())
	// The captured gate-list sample blocks fx-0uu with a gate for PR #1.
	bead := domain.Bead{ID: "fx-0uu", Title: "Gated bead"}
	_, err := f.m.Create(ctx, bead)
	require.NoError(t, err)
	wt, _, err := f.m.BranchFor(ctx, bead)
	require.NoError(t, err)
	dpSet(t, f.gh, "prs.json", dpPRs(dpPR(t, 1, "OPEN", wt, func(pr map[string]any) {
		pr["body"] = "<!-- slop-cannon bead=fx-0uu run=run-1 branch=" + wt + " -->\n"
	})))

	samples := testutil.StageBDSamples(t, nil)
	bd := testutil.FakeBD(t, samples)
	client := beads.New(f.repo.Root, beads.Options{Actor: "sc:test"})
	d, err := NewPRDeliverer(f.m, client, WithPRLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	require.NoError(t, err)

	res, err := d.Deliver(ctx, PRRequest{Bead: bead, Run: "run-1", RunStarted: dpStarted})
	require.NoError(t, err)
	assert.Equal(t, PRExisting, res.Status)
	assert.Equal(t, 1, res.PR.Number)
	assert.Equal(t, domain.BeadID("fx-jts"), res.Gate, "the gate bd already has for PR #1 is reused")
	assert.Equal(t, domain.BeadStateDelivering, res.State)
	assert.Empty(t, testutil.MissingSamples(t, bd))

	var keys []string
	for _, inv := range bd.Invocations(t) {
		keys = append(keys, testutil.BDKey(inv.Args))
		if testutil.BDKey(inv.Args) == "update" {
			assert.Contains(t, inv.Args, "fx-0uu")
			assert.Contains(t, inv.Args, "--metadata")
		}
	}
	assert.Equal(t, []string{"update", "gate-list"}, keys)
}
