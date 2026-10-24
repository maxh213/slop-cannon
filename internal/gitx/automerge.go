package gitx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"slop-cannon/internal/domain"
)

// Auto-merge (D-31). With safety on and auto_merge set, the run merges the
// pull requests it opened itself once they are green, instead of waiting for
// a human. The orchestrator's gate poller decides *when*; this file is the
// GitHub half of it, and the one place that decides *what* the run may
// touch:
//
//   - [OwnedByRun] is the ownership predicate. A pull request is the run's
//     own only when every clause holds — the bead's exact branch, the
//     default branch as base, the cannon's label, a body marker naming
//     this run and this bead, the number the bead recorded, created after
//     the run first started, and opened by the gh login itself. It guards
//     the merge, the adoption of an existing pull request by
//     [PRDeliverer.Deliver], and (through its creation-time clause) which
//     external pull requests [PRWatcher] offers for review: nothing that
//     existed before the run started is ever merged, reviewed, adopted or
//     pushed to.
//   - [GH.ViewPR] reads everything a merge decision needs in one call;
//     [PRView.Checks] judges the status-check rollup. `gh pr checks` is not
//     used: it exits non-zero while a check is pending or failed, and [GH]
//     drops standard output on a non-zero exit.
//   - [GH.MergePR] is the only merge this package has, and it is exactly
//     `gh pr merge <n> --squash --match-head-commit <sha>`: never --admin
//     (which bypasses branch protection), --auto (which would merge later,
//     on GitHub's say-so rather than the run's) or --delete-branch. The
//     head commit is the one the same poll read, so a push between the
//     read and the merge makes GitHub refuse it.
//   - [GH.UpdateBranch] asks GitHub to merge the base into a pull request
//     that has fallen behind (`gh pr update-branch`, a server-side merge,
//     never a force push), and [PRDeliverer.SyncBranch] fast-forwards the
//     bead's local branch onto what GitHub made of it before the bead goes
//     back to its engineer, so the engineer's next push is a plain
//     fast-forward again.

// prMarkerRe matches the machine-readable first line of [PRBody].
var prMarkerRe = regexp.MustCompile(`<!-- slop-cannon bead=(\S+) run=(\S*) branch=(\S+) -->`)

// PRMarkerFields is what the marker comment of a pull request body says.
type PRMarkerFields struct {
	Bead   domain.BeadID
	Run    domain.RunID
	Branch string
}

// ParsePRMarker reads the first slop-cannon marker comment in body. ok is
// false when there is none.
func ParsePRMarker(body string) (m PRMarkerFields, ok bool) {
	sub := prMarkerRe.FindStringSubmatch(body)
	if sub == nil {
		return PRMarkerFields{}, false
	}
	return PRMarkerFields{Bead: domain.BeadID(sub[1]), Run: domain.RunID(sub[2]), Branch: sub[3]}, true
}

// Ownership is what a pull request must match to be this run's own (D-31).
type Ownership struct {
	// Bead and Run are the bead and run the body marker must name.
	Bead domain.BeadID
	Run  domain.RunID
	// Branch is the bead's own sc/<id>-<slug> branch, as git has it; the
	// head must be exactly this.
	Branch string
	// Base is the repository's default branch; the base must be exactly
	// this.
	Base string
	// PRNumber is the bead's sc_pr_number; the pull request must carry it.
	PRNumber int
	// RunStarted is when the run first started (the original invocation,
	// not a resume); the pull request must have been created at or after
	// it.
	RunStarted time.Time
	// Login is the authenticated gh login; the pull request's author must
	// be it.
	Login string
}

// OwnedByRun reports whether pr is the run's own pull request for the bead
// o describes, and when it is not, the first clause it fails in words a
// person can act on. Every clause must hold; an unknown value (no login,
// no start time, no number, no creation time) fails its clause rather than
// passing it, so the predicate errs on "not ours".
func OwnedByRun(pr PullRequest, o Ownership) (bool, string) {
	switch {
	case o.Branch == "" || pr.HeadRef != o.Branch:
		return false, fmt.Sprintf("its head branch is %q, not the bead's %q", pr.HeadRef, o.Branch)
	case o.Base == "" || pr.BaseRef != o.Base:
		return false, fmt.Sprintf("its base branch is %q, not the default branch %q", pr.BaseRef, o.Base)
	case !pr.HasLabel(CannonPRLabel):
		return false, fmt.Sprintf("it does not carry the %q label", CannonPRLabel)
	}
	m, ok := ParsePRMarker(pr.Body)
	switch {
	case !ok:
		return false, "its body carries no slop-cannon marker"
	case o.Run == "" || m.Run != o.Run:
		return false, fmt.Sprintf("its marker names run %q, not this run %q", m.Run, o.Run)
	case o.Bead == "" || m.Bead != o.Bead:
		return false, fmt.Sprintf("its marker names bead %q, not %q", m.Bead, o.Bead)
	case o.PRNumber <= 0 || pr.Number != o.PRNumber:
		return false, fmt.Sprintf("it is #%d, not the bead's recorded pull request #%d", pr.Number, o.PRNumber)
	case o.RunStarted.IsZero() || pr.CreatedAt.IsZero() || pr.CreatedAt.Before(o.RunStarted):
		return false, fmt.Sprintf("it was created at %s, before this run started at %s",
			stamp(pr.CreatedAt), stamp(o.RunStarted))
	case o.Login == "" || !strings.EqualFold(pr.Author, o.Login):
		return false, fmt.Sprintf("its author is %q, not the gh login %q", pr.Author, o.Login)
	}
	return true, ""
}

// stamp renders a time for an ownership reason; the zero time is "unknown".
func stamp(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}

// CreatedBefore reports whether pr was opened before start, or cannot be
// shown not to have been: a pull request without a creation time counts as
// older. It is [OwnedByRun]'s creation-time clause on its own, which is
// what the external review needs (D-31): a pull request that existed when
// the run started is never reviewed.
func CreatedBefore(pr PullRequest, start time.Time) bool {
	return pr.CreatedAt.IsZero() || pr.CreatedAt.Before(start)
}

// Login returns the authenticated gh login (`gh api user --jq .login`). It
// is read once per GH and cached; a failure is not cached, so the next call
// asks again.
func (g *GH) Login(ctx context.Context) (string, error) {
	g.mu.Lock()
	login := g.login
	g.mu.Unlock()
	if login != "" {
		return login, nil
	}
	out, err := g.run(ctx, "", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login = strings.TrimSpace(out)
	if login == "" || strings.ContainsAny(login, " \t\n") {
		return "", fmt.Errorf("gitx: gh api user: unexpected login %q", out)
	}
	g.mu.Lock()
	g.login = login
	g.mu.Unlock()
	return login, nil
}

// ghPRViewFields are the `--json` fields [GH.ViewPR] reads: everything an
// auto-merge decision needs, in one call.
var ghPRViewFields = []string{
	"number", "state", "isDraft", "headRefName", "headRefOid", "baseRefName",
	"mergeable", "mergeStateStatus", "statusCheckRollup", "labels", "body",
	"createdAt", "author",
}

// Check kinds in a status-check rollup.
const (
	// CheckKindRun is a GitHub Actions (or Checks API) check run.
	CheckKindRun = "CheckRun"
	// CheckKindStatus is a commit status context.
	CheckKindStatus = "StatusContext"
)

// PRCheck is one entry of a pull request's status-check rollup.
type PRCheck struct {
	// Kind is [CheckKindRun] or [CheckKindStatus].
	Kind string `json:"kind"`
	// Name is the check run's name or the status context.
	Name string `json:"name"`
	// Status is a check run's status (QUEUED, IN_PROGRESS, COMPLETED …).
	Status string `json:"status,omitempty"`
	// Conclusion is a completed check run's conclusion (SUCCESS, FAILURE …).
	Conclusion string `json:"conclusion,omitempty"`
	// State is a status context's state (SUCCESS, PENDING, FAILURE, ERROR).
	State string `json:"state,omitempty"`
	// URL is where the check's details are.
	URL string `json:"url,omitempty"`
}

// checkOutcome is how one check stands.
type checkOutcome int

const (
	checkPending checkOutcome = iota
	checkPassed
	checkFailed
)

// failedConclusions are the check-run conclusions that mean the check
// failed. Anything not here and not a pass (STALE, an empty or unknown
// conclusion) is treated as still pending: the run waits rather than
// guessing.
var failedConclusions = map[string]bool{
	"FAILURE": true, "ERROR": true, "CANCELLED": true, "TIMED_OUT": true,
	"ACTION_REQUIRED": true, "STARTUP_FAILURE": true,
}

// outcome judges one check. A check run passes when it completed with
// SUCCESS, NEUTRAL or SKIPPED; a status context only with SUCCESS.
func (c PRCheck) outcome() checkOutcome {
	if c.Kind == CheckKindStatus {
		switch strings.ToUpper(c.State) {
		case "SUCCESS":
			return checkPassed
		case "FAILURE", "ERROR":
			return checkFailed
		}
		return checkPending
	}
	if !strings.EqualFold(c.Status, "COMPLETED") {
		return checkPending
	}
	conclusion := strings.ToUpper(c.Conclusion)
	switch {
	case conclusion == "SUCCESS" || conclusion == "NEUTRAL" || conclusion == "SKIPPED":
		return checkPassed
	case failedConclusions[conclusion]:
		return checkFailed
	}
	return checkPending
}

// succeeded reports whether the check really ran and succeeded, which is
// what a check named in auto_merge_checks must have done: a skipped or
// neutral check run passes the rollup but did not prove anything.
func (c PRCheck) succeeded() bool {
	if c.Kind == CheckKindStatus {
		return strings.EqualFold(c.State, "SUCCESS")
	}
	return strings.EqualFold(c.Status, "COMPLETED") && strings.EqualFold(c.Conclusion, "SUCCESS")
}

// Verdict renders the check's state in one word for a reason.
func (c PRCheck) Verdict() string {
	if c.Kind == CheckKindStatus {
		return strings.ToLower(c.State)
	}
	if !strings.EqualFold(c.Status, "COMPLETED") {
		return strings.ToLower(c.Status)
	}
	return strings.ToLower(c.Conclusion)
}

// CheckSummary is the judgement of a whole rollup against the names an
// auto-merge requires.
type CheckSummary struct {
	// Total is how many checks the rollup holds.
	Total int
	// Pending are the checks that have not finished (or whose result is
	// one the run does not recognise), Failed the ones that failed.
	Pending []PRCheck
	Failed  []PRCheck
	// Missing are the required names no successful check carries.
	Missing []string
}

// Green reports whether the rollup lets a merge through: at least one
// check, nothing pending, nothing failed, nothing required missing.
func (s CheckSummary) Green() bool {
	return s.Total > 0 && len(s.Pending) == 0 && len(s.Failed) == 0 && len(s.Missing) == 0
}

// PRView is what `gh pr view --json <ghPRViewFields>` says about one pull
// request.
type PRView struct {
	PullRequest
	// State is OPEN, CLOSED or MERGED.
	State string `json:"state"`
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string `json:"mergeable"`
	// MergeStateStatus is GitHub's verdict on merging now: CLEAN, BEHIND,
	// BLOCKED, DIRTY, DRAFT, HAS_HOOKS, UNKNOWN or UNSTABLE.
	MergeStateStatus string `json:"merge_state_status"`
	// Checks is the status-check rollup of the head commit.
	Checks []PRCheck `json:"checks,omitempty"`
}

// Summary judges the rollup against the check names required: every check
// present must have finished and passed, and every required name must be
// among the checks that succeeded.
func (v PRView) Summary(required []string) CheckSummary {
	s := CheckSummary{Total: len(v.Checks)}
	ok := map[string]bool{}
	for _, c := range v.Checks {
		switch c.outcome() {
		case checkPassed:
		case checkFailed:
			s.Failed = append(s.Failed, c)
		default:
			s.Pending = append(s.Pending, c)
		}
		if c.succeeded() {
			ok[c.Name] = true
		}
	}
	for _, name := range required {
		if name = strings.TrimSpace(name); name != "" && !ok[name] {
			s.Missing = append(s.Missing, name)
		}
	}
	return s
}

// ghRollupEntry is the raw gh JSON shape of one statusCheckRollup entry.
type ghRollupEntry struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
}

// ghPRView is the raw gh JSON shape of `gh pr view --json <ghPRViewFields>`.
type ghPRView struct {
	ghPR
	State             string          `json:"state"`
	Mergeable         string          `json:"mergeable"`
	MergeStateStatus  string          `json:"mergeStateStatus"`
	StatusCheckRollup []ghRollupEntry `json:"statusCheckRollup"`
}

// ParsePRView decodes the JSON printed by `gh pr view --json
// <ghPRViewFields>`.
func ParsePRView(data []byte) (*PRView, error) {
	var raw ghPRView
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
		return nil, fmt.Errorf("gitx: gh pr view: decoding output: %w", err)
	}
	v := &PRView{
		PullRequest:      raw.pullRequest(),
		State:            strings.ToUpper(raw.State),
		Mergeable:        strings.ToUpper(raw.Mergeable),
		MergeStateStatus: strings.ToUpper(raw.MergeStateStatus),
	}
	for _, e := range raw.StatusCheckRollup {
		c := PRCheck{Kind: e.Typename, Name: e.Name, Status: e.Status, Conclusion: e.Conclusion, URL: e.DetailsURL}
		if e.Typename == CheckKindStatus || (e.Typename == "" && e.Context != "") {
			c = PRCheck{Kind: CheckKindStatus, Name: e.Context, State: e.State, URL: e.TargetURL}
		}
		v.Checks = append(v.Checks, c)
	}
	return v, nil
}

// ViewPR reads one pull request with everything an auto-merge decision
// needs (`gh pr view <n> --json <fields>`).
func (g *GH) ViewPR(ctx context.Context, number int) (*PRView, error) {
	if number <= 0 {
		return nil, fmt.Errorf("gitx: ViewPR: invalid pull request number %d", number)
	}
	out, err := g.run(ctx, "", "pr", "view", fmt.Sprint(number), "--json", strings.Join(ghPRViewFields, ","))
	if err != nil {
		return nil, err
	}
	return ParsePRView([]byte(out))
}

// ErrNoHeadCommit refuses a merge that does not name the head commit it
// was judged on.
var ErrNoHeadCommit = errors.New("gitx: a merge must name the head commit it was judged on")

// MergePR squash-merges pull request number, exactly as `gh pr merge <n>
// --squash --match-head-commit <head>` (D-31). head is the commit the
// caller judged green; GitHub refuses the merge if the pull request has
// moved since. There is deliberately no option for --admin, --auto or
// --delete-branch.
func (g *GH) MergePR(ctx context.Context, number int, head string) error {
	if number <= 0 {
		return fmt.Errorf("gitx: MergePR: invalid pull request number %d", number)
	}
	if strings.TrimSpace(head) == "" {
		return ErrNoHeadCommit
	}
	_, err := g.run(ctx, "", "pr", "merge", fmt.Sprint(number), "--squash", "--match-head-commit", head)
	return err
}

// UpdateBranch asks GitHub to merge the base branch into pull request
// number (`gh pr update-branch <n>`): a merge made on the server, never a
// rebase and never a force push.
func (g *GH) UpdateBranch(ctx context.Context, number int) error {
	if number <= 0 {
		return fmt.Errorf("gitx: UpdateBranch: invalid pull request number %d", number)
	}
	_, err := g.run(ctx, "", "pr", "update-branch", fmt.Sprint(number))
	return err
}

// ErrBranchDiverged means the bead's local branch and the pull request's
// head have both moved: neither contains the other, so no fast-forward can
// bring them together.
var ErrBranchDiverged = errors.New("gitx: the bead's branch and its pull request's head have diverged")

// SyncBranch fast-forwards the bead's local branch, in its worktree, onto
// head — the pull request's head commit, which GitHub may have moved by
// merging the base into it ([GH.UpdateBranch]). Without it the engineer's
// next commits would sit on the old tip and their push would be refused as
// a non-fast-forward. The default remote's tracking refs are fetched first
// ([Repo.Fetch]), which is also what gives an engineer sent back to resolve
// a conflict a current <remote>/<default> to merge: agents may merge but
// not fetch. A branch that already contains head is left alone; one that
// has diverged from it fails with [ErrBranchDiverged], and nothing is
// changed. The fast-forward is `git reset --keep`, run in the bead's
// worktree only after head is proven to descend from the branch tip, so it
// can only move the bead's branch forward and refuses to touch uncommitted
// work.
func (d *PRDeliverer) SyncBranch(ctx context.Context, bead domain.Bead, head string) error {
	if strings.TrimSpace(head) == "" {
		return ErrNoHeadCommit
	}
	repo := d.m.Repo()
	branch, exists, err := d.m.BranchFor(ctx, bead)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: %s in %s", ErrBranchMissing, branch, repo.Root)
	}
	if err := repo.Fetch(ctx); err != nil {
		return err
	}
	local, err := repo.RevParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if local == head {
		return nil
	}
	if _, err := repo.RevParse(ctx, head); err != nil {
		return fmt.Errorf("gitx: the pull request's head %s is not on the remote's %s: %w", shortSHA(head), branch, err)
	}
	if contains, err := repo.isAncestor(ctx, head, local); err != nil {
		return err
	} else if contains {
		return nil
	}
	if forward, err := repo.isAncestor(ctx, local, head); err != nil {
		return err
	} else if !forward {
		return fmt.Errorf("%w: %s is at %s, the pull request at %s", ErrBranchDiverged, branch, shortSHA(local), shortSHA(head))
	}
	wt, err := d.m.Open(ctx, bead.ID)
	if err != nil {
		return err
	}
	if cur, err := wt.CurrentBranch(ctx); err != nil {
		return err
	} else if cur != branch {
		return fmt.Errorf("gitx: the worktree of %s has %q checked out, not %s", bead.ID, cur, branch)
	}
	_, err = wt.Git(ctx, "reset", "--keep", "--quiet", head)
	return err
}

// maxUpdateMerges bounds how many commits [PRDeliverer.ReviewedHead]
// walks between the reviewed commit and a pull request's head: GitHub's
// update merges come one per `gh pr update-branch`, and auto-merge asks
// for at most one per head.
const maxUpdateMerges = 20

// ReviewedHead reports whether head, a pull request's head commit, holds
// only work the run reviewed (D-31): it is reviewed itself — the bead
// branch's tip at delivery, sc_pr_head — or it descends from reviewed and
// every commit since that is not on base is a clean, automatic merge of its
// two parents, which is what `gh pr update-branch` makes. A commit someone
// else pushed onto the branch, or a merge whose tree is not the automatic
// result of its parents, makes it false, and why says which. The default
// remote's tracking refs are fetched first, so head and base are current.
func (d *PRDeliverer) ReviewedHead(ctx context.Context, reviewed, head, base string) (bool, string, error) {
	switch {
	case strings.TrimSpace(reviewed) == "":
		return false, "no reviewed commit is recorded for the bead", nil
	case strings.TrimSpace(head) == "":
		return false, "GitHub reported no head commit", nil
	case head == reviewed:
		return true, "", nil
	}
	repo := d.m.Repo()
	if err := repo.Fetch(ctx); err != nil {
		return false, "", err
	}
	ref, err := d.TrackingRef(ctx, base)
	if err != nil {
		return false, "", err
	}
	ref = "refs/remotes/" + ref
	if _, err := repo.RevParse(ctx, head); err != nil {
		return false, "", fmt.Errorf("gitx: the pull request's head %s is not on the remote: %w", shortSHA(head), err)
	}
	if ok, err := repo.isAncestor(ctx, reviewed, head); err != nil {
		return false, "", err
	} else if !ok {
		return false, fmt.Sprintf("its head %s does not contain the reviewed commit %s", shortSHA(head), shortSHA(reviewed)), nil
	}
	out, err := repo.Git(ctx, "rev-list", "--parents", "--max-count", strconv.Itoa(maxUpdateMerges+1), head, "^"+reviewed, "^"+ref)
	if err != nil {
		return false, "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		// Every commit since the reviewed one is already on base.
		return true, "", nil
	}
	commits := strings.Split(out, "\n")
	if len(commits) > maxUpdateMerges {
		return false, fmt.Sprintf("more than %d commits since the reviewed commit %s", maxUpdateMerges, shortSHA(reviewed)), nil
	}
	for _, line := range commits {
		f := strings.Fields(line)
		if len(f) != 3 {
			return false, fmt.Sprintf("commit %s since the reviewed commit %s is not a merge of %s", shortSHA(f[0]), shortSHA(reviewed), base), nil
		}
		tree, err := repo.Git(ctx, "rev-parse", "--verify", "--end-of-options", f[0]+"^{tree}")
		if err != nil {
			return false, "", err
		}
		merged, err := repo.Git(ctx, "merge-tree", "--write-tree", "--no-messages", f[1], f[2])
		if isExit(err, 1) {
			return false, fmt.Sprintf("merge commit %s resolves conflicts the run did not review", shortSHA(f[0])), nil
		} else if err != nil {
			return false, "", err
		}
		if tree0, _, _ := strings.Cut(strings.TrimSpace(merged), "\n"); tree0 != tree {
			return false, fmt.Sprintf("merge commit %s changes more than merging its parents", shortSHA(f[0])), nil
		}
	}
	return true, "", nil
}

// Conventional Commit types a pull request title may carry, the ones the
// common title checks accept (amannn/action-semantic-pull-request's
// defaults).
var conventionalTypes = []string{"feat", "fix", "docs", "style", "refactor", "perf", "test", "build", "ci", "chore", "revert"}

// conventionalRe matches a title that is already "<type>[(scope)][!]: …".
var conventionalRe = regexp.MustCompile(`^([a-z]+)(\([^)]*\))?!?: \S`)

// ConventionalType maps a bead's issue type onto a Conventional Commit type
// (D-31): bug → fix, feature → feat, chore and task → chore, anything else
// feat.
func ConventionalType(t domain.BeadType) string {
	switch t {
	case domain.BeadTypeBug:
		return "fix"
	case domain.BeadTypeChore, domain.BeadTypeTask:
		return "chore"
	}
	return "feat"
}

// ConventionalPRTitle is [PRTitle] in Conventional Commit form:
// "<type>: <title starting lowercase>", the type from [ConventionalType]. A
// title that is already conventional is kept as it is. The result is capped
// like PRTitle's.
func ConventionalPRTitle(bead domain.Bead) string {
	title := strings.Join(strings.Fields(bead.Title), " ")
	if title == "" {
		title = "bead " + string(bead.ID)
	}
	if m := conventionalRe.FindStringSubmatch(title); m != nil && isConventionalType(m[1]) {
		return capTitle(title)
	}
	r, size := utf8.DecodeRuneInString(title)
	title = string(unicode.ToLower(r)) + title[size:]
	return capTitle(ConventionalType(bead.Type) + ": " + title)
}

func isConventionalType(t string) bool {
	for _, k := range conventionalTypes {
		if k == t {
			return true
		}
	}
	return false
}

// BranchFor is [Manager.BranchFor] of the deliverer's manager: the branch
// the bead's work is on, which is the head its pull request must have.
func (d *PRDeliverer) BranchFor(ctx context.Context, bead domain.Bead) (string, bool, error) {
	return d.m.BranchFor(ctx, bead)
}

// TrackingRef names branch as the default remote's tracking ref
// ("origin/main"), the spelling an engineer merges after [PRDeliverer.SyncBranch]
// has fetched it.
func (d *PRDeliverer) TrackingRef(ctx context.Context, branch string) (string, error) {
	remote, err := d.m.Repo().DefaultRemote(ctx)
	if err != nil {
		return "", err
	}
	return remote + "/" + branch, nil
}
