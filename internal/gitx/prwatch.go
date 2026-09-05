package gitx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/proc"
)

// DefaultPRPollInterval is how often the watcher asks GitHub for open pull
// requests when no [WithPollInterval] option is given. It is an alias of
// [config.DefaultPRPollInterval], the default of the `pr_poll_interval`
// configuration key, so the documented number and the built-in one cannot
// drift apart (P8-04).
const DefaultPRPollInterval = config.DefaultPRPollInterval

// CannonPRLabel is the GitHub label the cannon puts on every pull request it
// opens (P4-05). The watcher skips pull requests carrying it, so the cannon
// never reviews its own work as an external PR (D-11).
const CannonPRLabel = "slop-cannon"

// KindExternalPR is the sc_kind metadata value of a review bead the watcher
// creates for an external pull request.
const KindExternalPR = "external_pr"

// MetaExternalRef is the metadata key holding bd's external reference for
// the pull request ("gh-<n>"). It mirrors what `bd create --external-ref`
// records so the reference survives even when the bd client cannot pass the
// flag (see the P4-07 deviation note).
const MetaExternalRef = "sc_external_ref"

// MetaPRHead is the metadata key holding the head commit of the pull
// request at the time the review bead was created.
const MetaPRHead = "sc_pr_head"

// ghListLimit caps one `gh pr list` page. gh's default of 30 is too small
// for a busy repository; anything above this is not something the cannon
// can review anyway.
const ghListLimit = 200

// Sentinel errors for the gh CLI. Every error returned by [GH] that
// corresponds to one of these matches it with errors.Is, even when wrapped
// in a [*GHError].
var (
	// ErrGHNotFound means the gh binary is not on PATH. Errors carrying it
	// also satisfy proc.IsNotFound.
	ErrGHNotFound = errors.New("gitx: gh is not installed")
	// ErrGHUnauthenticated means gh is installed but not logged in to the
	// host of the repository's remote.
	ErrGHUnauthenticated = errors.New("gitx: gh is not authenticated (run gh auth login or set GH_TOKEN)")
	// ErrNoGitHubRemote means gh could not map the repository's remotes to
	// a GitHub repository.
	ErrNoGitHubRemote = errors.New("gitx: no git remote points at a GitHub repository")
)

// GHError describes a gh command that failed. It wraps both the
// classifying sentinel (when one applies) and the underlying error from
// the process runner, so errors.Is works for either.
type GHError struct {
	// Args are the gh arguments that were run (without "gh" itself).
	Args []string
	// Dir is the directory gh ran in.
	Dir string
	// ExitCode is gh's exit status, -1 if it was killed or never started.
	ExitCode int
	// Stderr is gh's captured standard error, trimmed.
	Stderr string
	// Cause is the sentinel this failure was classified as, or nil.
	Cause error
	// Err is the error from the process runner.
	Err error
}

// Error renders "gitx: gh <args>: <cause or exit status>: <last stderr line>".
func (e *GHError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gitx: gh %s", strings.Join(e.Args, " "))
	switch {
	case e.Cause != nil:
		fmt.Fprintf(&b, ": %v", e.Cause)
	case e.ExitCode >= 0:
		fmt.Fprintf(&b, ": exit status %d", e.ExitCode)
	default:
		b.WriteString(": killed")
	}
	if tail := lastLine(e.Stderr); tail != "" {
		fmt.Fprintf(&b, ": %s", tail)
	}
	return b.String()
}

// Unwrap exposes Cause and Err so errors.Is matches both the sentinel and
// the runner's causes (proc.ErrTimeout, exec.ErrNotFound, cancellation).
func (e *GHError) Unwrap() []error {
	var errs []error
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	if e.Err != nil {
		errs = append(errs, e.Err)
	}
	return errs
}

// GH runs the GitHub CLI inside a repository. It is the only way this
// package talks to GitHub: no API client, no tokens of its own. gh resolves
// the repository from the working tree's remotes, so a GH bound to a
// [Repo] acts on that repository's project.
//
// A GH never merges anything. D-11 forbids merging pull requests the cannon
// did not open, and the cannon's own pull requests are merged by humans
// (D-10), so no merge method exists.
type GH struct {
	repo *Repo
	exec func(context.Context, proc.Spec) (proc.Result, error)
}

// NewGH returns a GH that runs gh inside repo with the repository's
// environment and timeout (see [WithEnv], [WithTimeout]).
func NewGH(repo *Repo) *GH {
	return &GH{repo: repo, exec: proc.Run}
}

// Repo returns the repository gh runs in.
func (g *GH) Repo() *Repo { return g.repo }

// ghInherited names the parent variables gh may see on top of git's: its
// own configuration and token variables. GH_TOKEN and GITHUB_TOKEN are
// passed through because gh reads them for authentication; they are never
// logged.
func ghInherited(base []string) []string {
	names := append([]string(nil), base...)
	names = append(names, "GITHUB_TOKEN", "GITHUB_API_URL", "GITHUB_SERVER_URL")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GH_") {
			names = append(names, name)
		}
	}
	return names
}

// run executes gh with args in the repository root, with a stable and
// non-interactive environment. A non-zero exit is reported as a
// [*GHError] classified by its stderr; a missing binary is a GHError
// carrying [ErrGHNotFound].
func (g *GH) run(ctx context.Context, stdin string, args ...string) (string, error) {
	env := map[string]string{
		"LC_ALL":                 "C",
		"NO_COLOR":               "1",
		"CLICOLOR":               "0",
		"TERM":                   "dumb",
		"GH_NO_UPDATE_NOTIFIER":  "1",
		"GH_PROMPT_DISABLED":     "1",
		"GH_PAGER":               "cat",
		"PAGER":                  "cat",
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_OPTIONAL_LOCKS":     "0",
		"GH_FORCE_TTY":           "0",
		"GH_SPINNER_DISABLED":    "1",
		"GH_BROWSER":             "true",
		"GH_EDITOR":              "true",
		"GIT_EDITOR":             "true",
		"BROWSER":                "true",
		"GH_NO_EXTENSION_UPDATE": "1",
	}
	for k, v := range g.repo.env {
		env[k] = v
	}
	spec := proc.Spec{
		Command:    "gh",
		Args:       args,
		Dir:        g.repo.Root,
		Env:        env,
		InheritEnv: ghInherited(g.repo.inherit),
		Timeout:    g.repo.timeout,
	}
	if stdin != "" {
		spec.Stdin = strings.NewReader(stdin)
	}
	res, err := g.exec(ctx, spec)
	if err != nil {
		ghe := &GHError{Args: args, Dir: g.repo.Root, ExitCode: -1, Err: err}
		var ee *proc.ExitError
		switch {
		case errors.As(err, &ee):
			ghe.ExitCode = ee.Result.ExitCode
			ghe.Stderr = strings.TrimSpace(ee.Result.Stderr)
			ghe.Cause = classifyGH(ghe.Stderr)
		case proc.IsNotFound(err):
			ghe.Cause = ErrGHNotFound
		}
		return "", ghe
	}
	return strings.TrimRight(res.Stdout, "\r\n"), nil
}

// ghProblem is one row of the [ghProblems] table: a lower-case fragment of
// gh's stderr and the sentinel it means.
type ghProblem struct {
	fragment string
	cause    error
}

// ghProblems is the single classification table for gh's stderr (P4-09).
// It is ordered, and the order is the point: gh's message for a repository
// with no GitHub remote ends "To tell gh about a new GitHub host, please
// use `gh auth login`", so every fragment that means "no usable GitHub
// remote" has to be tested before the "gh auth login" hint, or a delivery
// to a GitLab remote tells the user to log in to a host that has nothing
// to do with the failure.
var ghProblems = []ghProblem{
	// No usable GitHub remote.
	{"known github host", ErrNoGitHubRemote},
	{"no git remotes found", ErrNoGitHubRemote},
	{"none of the git remotes", ErrNoGitHubRemote},
	{"could not determine base repository", ErrNoGitHubRemote},
	{"unable to determine base repository", ErrNoGitHubRemote},
	{"not a git repository", ErrNoGitHubRemote},
	// Nobody is logged in.
	{"gh auth login", ErrGHUnauthenticated},
	{"not logged into", ErrGHUnauthenticated},
	{"not logged in to", ErrGHUnauthenticated},
	{"authentication required", ErrGHUnauthenticated},
	{"authentication token", ErrGHUnauthenticated},
	{"gh_token", ErrGHUnauthenticated},
	{"http 401", ErrGHUnauthenticated},
	{"bad credentials", ErrGHUnauthenticated},
}

// classifyGH maps gh's stderr onto a sentinel, or nil when the failure is
// something else (a network error, a rate limit, a bad argument).
func classifyGH(stderr string) error {
	lower := strings.ToLower(stderr)
	for _, p := range ghProblems {
		if strings.Contains(lower, p.fragment) {
			return p.cause
		}
	}
	return nil
}

// ghUnavailable classifies an error from [GH] as "gh cannot be used here"
// — not installed, not authenticated, or no GitHub remote — and returns
// the reason in the words the user reads on a push-only delivery. Other
// failures (network, rate limits, a bad argument) are not, and the caller
// must report them. It is the second half of the one classifier [classifyGH]
// starts: that one names the cause, this one says what it means (P4-09).
func ghUnavailable(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrGHNotFound) || proc.IsNotFound(err):
		return "gh is not installed", true
	case errors.Is(err, ErrNoGitHubRemote):
		return "the remote is not on a GitHub host gh knows", true
	case errors.Is(err, ErrGHUnauthenticated):
		return "gh is not authenticated (run `gh auth login`)", true
	}
	return "", false
}

// Check verifies that gh is installed and logged in (`gh auth status`).
// It fails with [ErrGHNotFound] or [ErrGHUnauthenticated]; whether the
// repository has a remote at all is [Repo.HasRemote]'s business. P8-04
// uses both to decide whether `--review-prs` defaults on (D-11).
func (g *GH) Check(ctx context.Context) error {
	if _, err := proc.LookPath("gh"); err != nil {
		return &GHError{Args: []string{"auth", "status"}, Dir: g.repo.Root, ExitCode: -1, Cause: ErrGHNotFound, Err: err}
	}
	_, err := g.run(ctx, "", "auth", "status")
	if err != nil {
		var ghe *GHError
		if errors.As(err, &ghe) && ghe.Cause == nil && ghe.ExitCode > 0 {
			// gh auth status exits 1 for "not logged in" even when the
			// wording changes between releases.
			ghe.Cause = ErrGHUnauthenticated
		}
		return err
	}
	return nil
}

// PullRequest is what `gh pr list --json` reports about one open pull
// request.
type PullRequest struct {
	// Number is the pull request number.
	Number int `json:"number"`
	// Title is the pull request title.
	Title string `json:"title"`
	// URL is the HTML URL of the pull request.
	URL string `json:"url"`
	// Body is the pull request description (markdown), possibly empty.
	Body string `json:"body,omitempty"`
	// Author is the GitHub login of the author.
	Author string `json:"author,omitempty"`
	// HeadRef is the source branch name; BaseRef the target branch.
	HeadRef string `json:"head_ref"`
	BaseRef string `json:"base_ref"`
	// HeadSHA is the commit the pull request currently points at.
	HeadSHA string `json:"head_sha,omitempty"`
	// Labels are the label names on the pull request.
	Labels []string `json:"labels,omitempty"`
	// IsDraft reports whether the pull request is a draft.
	IsDraft bool `json:"is_draft,omitempty"`
	// CreatedAt and UpdatedAt are GitHub's timestamps (zero when absent).
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// HasLabel reports whether the pull request carries label (case-insensitive,
// as GitHub treats label names).
func (pr PullRequest) HasLabel(label string) bool {
	for _, l := range pr.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

// ExternalRef is the bd external reference for the pull request: "gh-<n>".
func (pr PullRequest) ExternalRef() string { return fmt.Sprintf("gh-%d", pr.Number) }

// IsCannonPR reports whether the cannon opened this pull request itself:
// it carries [CannonPRLabel], or its head branch uses the cannon's
// [BranchPrefix] (a branch pushed when gh was unavailable, P4-05, and later
// turned into a pull request by a human). Such pull requests are never
// reviewed as external ones (D-11).
func IsCannonPR(pr PullRequest) bool {
	return pr.HasLabel(CannonPRLabel) || strings.HasPrefix(pr.HeadRef, BranchPrefix)
}

// ghPRFields are the `--json` fields requested from gh pr list.
var ghPRFields = []string{
	"number", "title", "url", "body", "author", "headRefName", "baseRefName",
	"headRefOid", "labels", "isDraft", "createdAt", "updatedAt",
}

// ghPR is the raw gh JSON shape of a pull request.
type ghPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	IsDraft   bool      `json:"isDraft"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ParsePRList decodes the JSON printed by `gh pr list --json <ghPRFields>`
// into pull requests, in gh's order.
func ParsePRList(data []byte) ([]PullRequest, error) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil, nil
	}
	var raw []ghPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gitx: gh pr list: decoding output: %w", err)
	}
	prs := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		pr := PullRequest{
			Number:    r.Number,
			Title:     r.Title,
			URL:       r.URL,
			Body:      r.Body,
			Author:    r.Author.Login,
			HeadRef:   r.HeadRefName,
			BaseRef:   r.BaseRefName,
			HeadSHA:   r.HeadRefOid,
			IsDraft:   r.IsDraft,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
		}
		for _, l := range r.Labels {
			if l.Name != "" {
				pr.Labels = append(pr.Labels, l.Name)
			}
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// ListOpenPRs returns every open pull request of the repository (`gh pr
// list --state open --json …`), including the cannon's own; use
// [IsCannonPR] or [PRWatcher.Poll] to separate them.
func (g *GH) ListOpenPRs(ctx context.Context) ([]PullRequest, error) {
	out, err := g.run(ctx, "", "pr", "list", "--state", "open", "--limit", fmt.Sprint(ghListLimit), "--json", strings.Join(ghPRFields, ","))
	if err != nil {
		return nil, err
	}
	return ParsePRList([]byte(out))
}

// ReviewEvent is the kind of review posted with `gh pr review`.
type ReviewEvent string

// Review events. The cannon never posts a bare comment review: a
// consolidated verdict either approves or requests changes.
const (
	// ReviewApprove maps to `gh pr review --approve`.
	ReviewApprove ReviewEvent = "approve"
	// ReviewRequestChanges maps to `gh pr review --request-changes`.
	ReviewRequestChanges ReviewEvent = "request-changes"
)

// Flag returns the gh pr review flag for the event.
func (e ReviewEvent) Flag() string { return "--" + string(e) }

// Review is the consolidated review the testers' verdicts produce for one
// pull request (D-11): one event and one markdown body.
type Review struct {
	// PR is the pull request reviewed.
	PR PullRequest `json:"pr"`
	// Event is approve when every verdict approved, else request-changes.
	Event ReviewEvent `json:"event"`
	// Body is the markdown posted as the review comment.
	Body string `json:"body"`
	// Verdicts are the per-lens verdicts the review was built from, in the
	// order they appear in Body.
	Verdicts []domain.Verdict `json:"verdicts"`
}

// ErrNoVerdicts is returned when a review is requested without any verdict.
var ErrNoVerdicts = errors.New("gitx: no verdicts to post")

// BuildReview consolidates verdicts into one review for pr without posting
// it. The event is approve only when every verdict approved. Verdicts are
// ordered by lens (ux, qa, test, then any others in the given order) so
// the body is stable, and the body passes through logging.Redact so a
// secret quoted by a tester never reaches GitHub. It fails with
// [ErrNoVerdicts] when verdicts is empty.
func BuildReview(pr PullRequest, verdicts []domain.Verdict) (Review, error) {
	if len(verdicts) == 0 {
		return Review{}, ErrNoVerdicts
	}
	ordered := append([]domain.Verdict(nil), verdicts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return lensRank(ordered[i].Lens) < lensRank(ordered[j].Lens)
	})
	event := ReviewApprove
	for _, v := range ordered {
		if v.Outcome != domain.VerdictApprove {
			event = ReviewRequestChanges
			break
		}
	}
	return Review{PR: pr, Event: event, Body: logging.Redact(reviewBody(pr, event, ordered)), Verdicts: ordered}, nil
}

// lensRank orders lenses ux, qa, test; unknown lenses sort last.
func lensRank(l domain.Lens) int {
	for i, known := range domain.Lenses() {
		if known == l {
			return i
		}
	}
	return len(domain.Lenses())
}

// reviewBody renders the markdown body of a consolidated review.
func reviewBody(pr PullRequest, event ReviewEvent, verdicts []domain.Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## slop-cannon review of #%d: %s\n\n", pr.Number, strings.TrimSpace(pr.Title))

	var approved, changes []string
	for _, v := range verdicts {
		name := lensName(v)
		if v.Outcome == domain.VerdictApprove {
			approved = append(approved, name)
		} else {
			changes = append(changes, name)
		}
	}
	switch {
	case event == ReviewApprove:
		fmt.Fprintf(&b, "**Verdict: approved** by %s.\n", joinNames(approved))
	case len(approved) == 0:
		fmt.Fprintf(&b, "**Verdict: changes requested** by %s.\n", joinNames(changes))
	default:
		fmt.Fprintf(&b, "**Verdict: changes requested** by %s; approved by %s.\n", joinNames(changes), joinNames(approved))
	}

	for _, v := range verdicts {
		outcome := "approved"
		if v.Outcome != domain.VerdictApprove {
			outcome = "changes requested"
		}
		fmt.Fprintf(&b, "\n### %s: %s\n", lensName(v), outcome)
		if s := strings.TrimSpace(v.Summary); s != "" {
			fmt.Fprintf(&b, "\n%s\n", s)
		}
		if len(v.Items) > 0 {
			b.WriteString("\n")
			for _, it := range v.Items {
				writeItem(&b, it)
			}
		}
	}

	b.WriteString("\n---\n")
	b.WriteString("Generated by slop-cannon from ")
	fmt.Fprintf(&b, "%d %s. ", len(verdicts), plural(len(verdicts), "tester verdict", "tester verdicts"))
	b.WriteString("This review is automated; it never merges pull requests it did not open.\n")
	return b.String()
}

// writeItem renders one feedback item as a markdown list entry.
func writeItem(b *strings.Builder, it domain.FeedbackItem) {
	fmt.Fprintf(b, "- **[%s]**", it.Severity)
	if it.Category != "" {
		fmt.Fprintf(b, " %s:", it.Category)
	}
	fmt.Fprintf(b, " %s", strings.TrimSpace(it.Title))
	if it.File != "" {
		if it.Line > 0 {
			fmt.Fprintf(b, " (`%s:%d`)", it.File, it.Line)
		} else {
			fmt.Fprintf(b, " (`%s`)", it.File)
		}
	}
	if it.Status != "" && it.Status != domain.ItemOpen {
		fmt.Fprintf(b, " _(%s)_", it.Status)
	}
	b.WriteString("\n")
	if d := strings.TrimSpace(it.Detail); d != "" {
		fmt.Fprintf(b, "  %s\n", indentContinuation(d))
	}
	if s := strings.TrimSpace(it.Suggestion); s != "" {
		if strings.Contains(s, "\n") {
			// Multi-line suggestions are usually code: fence them inside
			// the list item so GitHub keeps the layout.
			fmt.Fprintf(b, "  Suggestion:\n  ```\n  %s\n  ```\n", indentContinuation(s))
		} else {
			fmt.Fprintf(b, "  Suggestion: %s\n", s)
		}
	}
}

// indentContinuation indents every line after the first by two spaces so a
// multi-line detail stays inside its list item.
func indentContinuation(s string) string {
	return strings.ReplaceAll(s, "\n", "\n  ")
}

// lensName returns the lens label for a verdict, "tester" when unset.
func lensName(v domain.Verdict) string {
	if v.Lens == "" {
		return "tester"
	}
	return string(v.Lens)
}

// joinNames renders "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "nobody"
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// PostReview consolidates verdicts with [BuildReview] and posts the result
// with `gh pr review <n> --approve|--request-changes --body-file -` (the
// body goes through stdin, so its size is not bound by the argument
// limit). It returns the review that was posted. It never merges.
func (g *GH) PostReview(ctx context.Context, pr PullRequest, verdicts []domain.Verdict) (Review, error) {
	if pr.Number <= 0 {
		return Review{}, fmt.Errorf("gitx: post review: invalid pull request number %d", pr.Number)
	}
	review, err := BuildReview(pr, verdicts)
	if err != nil {
		return Review{}, err
	}
	if _, err := g.run(ctx, review.Body, "pr", "review", fmt.Sprint(pr.Number), review.Event.Flag(), "--body-file", "-"); err != nil {
		return Review{}, err
	}
	return review, nil
}

// BeadStore is the slice of the beads client the watcher needs. A
// *beads.Client satisfies it; tests use an in-memory store.
type BeadStore interface {
	// Create creates one bead and returns it as bd printed it.
	Create(ctx context.Context, in beads.BeadInput) (beads.Issue, error)
	// List returns the beads matching the filter.
	List(ctx context.Context, f beads.ListFilter) ([]beads.Issue, error)
}

// ReviewBeadTitle is the title of the review bead for pr.
func ReviewBeadTitle(pr PullRequest) string {
	return fmt.Sprintf("Review PR #%d: %s", pr.Number, strings.TrimSpace(pr.Title))
}

// ReviewBeadInput describes the review bead the watcher creates for an
// external pull request: type task, label sc:external-pr (plus the run
// label when run is set), sc_pr_url/sc_pr_number/sc_pr_head/sc_kind
// metadata and the "gh-<n>" external reference. The description carries
// the pull request's URL, author, branches and body so testers need no
// GitHub access to know what they are looking at.
func ReviewBeadInput(pr PullRequest, run domain.RunID) beads.BeadInput {
	var d strings.Builder
	fmt.Fprintf(&d, "External pull request #%d", pr.Number)
	if pr.Author != "" {
		fmt.Fprintf(&d, " by %s", pr.Author)
	}
	fmt.Fprintf(&d, ": %s\n", pr.URL)
	fmt.Fprintf(&d, "Branch: %s -> %s", pr.HeadRef, pr.BaseRef)
	if pr.HeadSHA != "" {
		fmt.Fprintf(&d, " (head %s)", shortSHA(pr.HeadSHA))
	}
	d.WriteString("\n")
	if pr.IsDraft {
		d.WriteString("Status: draft\n")
	}
	if body := strings.TrimSpace(pr.Body); body != "" {
		d.WriteString("\n")
		d.WriteString(body)
		d.WriteString("\n")
	}

	meta := beads.MergeMeta(
		beads.PRMetadata(pr.URL, pr.Number),
		map[string]any{
			beads.MetaKind:  KindExternalPR,
			MetaExternalRef: pr.ExternalRef(),
		},
	)
	if pr.HeadSHA != "" {
		meta[MetaPRHead] = pr.HeadSHA
	}
	labels := []string{beads.LabelExternalPR}
	if run != "" {
		meta[beads.MetaRun] = string(run)
		labels = append(labels, beads.RunLabel(run))
	}
	return beads.BeadInput{
		Title:       ReviewBeadTitle(pr),
		Type:        domain.BeadTypeTask,
		Priority:    domain.PriorityDefault,
		Description: d.String(),
		// bd's own field for "this bead stands for something outside bd"
		// (P3-19); the same reference is in the metadata for readers that
		// only look there.
		ExternalRef: pr.ExternalRef(),
		Acceptance: "Every tester lens has reviewed the pull request's diff and a consolidated review " +
			"(approve or request changes) has been posted on GitHub with gh pr review. " +
			"The pull request is never merged by slop-cannon.",
		Labels:   labels,
		Metadata: meta,
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ReviewBead pairs a review bead with the pull request it tracks.
type ReviewBead struct {
	// Issue is the bead as bd returned it.
	Issue beads.Issue
	// PR is the open pull request the bead reviews.
	PR PullRequest
}

// PollResult is what one [PRWatcher.Poll] found.
type PollResult struct {
	// External are the open pull requests the cannon did not open, in gh's
	// order.
	External []PullRequest
	// Skipped are the open pull requests the cannon opened itself
	// (IsCannonPR), never reviewed as external.
	Skipped []PullRequest
	// New are the review beads created by this poll, one per pull request
	// that had none.
	New []ReviewBead
	// Existing are the review beads that already tracked an open external
	// pull request before this poll.
	Existing []ReviewBead
	// Stale are the not-yet-closed review beads this poll could not match
	// to an open external pull request, each with the reason why. The
	// watcher leaves them alone; P8-04 decides what to do with them.
	Stale []StaleReview
}

// StaleReason says why a review bead was not matched to an open external
// pull request. It exists because "stale" is not one condition: the reason
// is written into bd's close reason and into the run log (P8-11), so it has
// to be the true one.
type StaleReason string

// The reasons a review bead can go stale.
const (
	// StaleClosed: the pull request is not in the open listing any more,
	// so it has been merged or closed on GitHub.
	StaleClosed StaleReason = "closed"
	// StaleCannonPR: the pull request is still open, but it now looks like
	// one of the cannon's own ([IsCannonPR]) — it acquired the
	// [CannonPRLabel], or its head branch was renamed under
	// [BranchPrefix] — so it is no longer reviewed as an external one
	// (D-11). Nothing has happened to the pull request itself.
	StaleCannonPR StaleReason = "cannon_pr"
	// StaleUnknownPR: the review bead records neither a pull request
	// number nor a URL, so no listing could ever match it.
	StaleUnknownPR StaleReason = "unknown_pr"
)

// StaleReview is one review bead the poll could not match to an open
// external pull request.
type StaleReview struct {
	// Issue is the review bead as bd returned it.
	Issue beads.Issue
	// Reason says which of the three conditions applies.
	Reason StaleReason
	// PR is the pull request the bead was matched to when it is still open
	// and only skipped ([StaleCannonPR]); the zero value otherwise.
	PR PullRequest
	// ForeignRun is set when the bead carries another run's id: the
	// dedupe listing spans every run, so a review bead an earlier run left
	// open is reported here too, and whatever settles it should not claim
	// this run raised it.
	ForeignRun bool
}

// Number is the pull request the bead tracks, from bd's metadata or from
// the still-open pull request it was matched to; zero when neither says.
func (s StaleReview) Number() int {
	if n := s.Issue.PRNumber(); n > 0 {
		return n
	}
	return s.PR.Number
}

// Explain is the reason in one clause, for bd's close reason and for the
// run log. It never claims a pull request has gone when it has not.
func (s StaleReview) Explain() string {
	return StaleSentence(s.Reason, s.Number(), s.ForeignRun)
}

// StaleSentence renders one stale reason as the clause that goes into bd's
// close reason and the run log. A number of zero or less leaves the pull
// request unnamed rather than writing "#0"; foreign says the review bead
// belongs to an earlier run. It is exported so a caller that knows the
// pull request number when the bead's metadata has lost it (the reviewer
// remembers what it adopted) still words the reason the same way.
func StaleSentence(reason StaleReason, number int, foreign bool) string {
	var msg string
	switch {
	case reason == StaleUnknownPR:
		msg = "the review bead records no pull request number or URL, so no pull request could be matched to it"
	case number <= 0 && reason == StaleCannonPR:
		msg = "the review bead's pull request is still open but is now one of slop-cannon's own, so it is not reviewed as an external pull request"
	case reason == StaleCannonPR:
		msg = fmt.Sprintf("pull request #%d is still open but is now one of slop-cannon's own, so it is not reviewed as an external pull request", number)
	case number <= 0:
		msg = "the review bead's pull request is no longer open on GitHub"
	default:
		msg = fmt.Sprintf("pull request #%d is no longer open on GitHub", number)
	}
	if foreign {
		msg += " (its review bead was raised by an earlier run)"
	}
	return msg
}

// PRWatcher implements D-11's first half: it polls GitHub for open pull
// requests the cannon did not open and creates exactly one review bead per
// pull request, deduplicated against the beads database, so a restart or
// a second poll never creates a duplicate.
type PRWatcher struct {
	gh       *GH
	store    BeadStore
	interval time.Duration
	run      domain.RunID
	logger   *slog.Logger
}

// WatcherOption customises [NewPRWatcher].
type WatcherOption func(*PRWatcher)

// WithPollInterval sets how often [PRWatcher.Run] polls. Zero or negative
// keeps [DefaultPRPollInterval].
func WithPollInterval(d time.Duration) WatcherOption {
	return func(w *PRWatcher) {
		if d > 0 {
			w.interval = d
		}
	}
}

// WithRun tags every review bead with the run: the sc:run:<id> label and
// sc_run metadata, so `bd list` can show the run's external reviews.
func WithRun(run domain.RunID) WatcherOption {
	return func(w *PRWatcher) { w.run = run }
}

// WithWatcherLogger sets the logger for poll outcomes; nil keeps
// slog.Default().
func WithWatcherLogger(l *slog.Logger) WatcherOption {
	return func(w *PRWatcher) {
		if l != nil {
			w.logger = l
		}
	}
}

// NewPRWatcher returns a watcher that lists pull requests through gh and
// records review beads in store.
func NewPRWatcher(gh *GH, store BeadStore, opts ...WatcherOption) *PRWatcher {
	w := &PRWatcher{gh: gh, store: store, interval: DefaultPRPollInterval, logger: slog.Default()}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Interval returns the poll interval in force.
func (w *PRWatcher) Interval() time.Duration { return w.interval }

// Poll lists the open pull requests once, skips the cannon's own, and
// creates a review bead for every external pull request that has none
// yet. Existing review beads are found by the sc:external-pr label and
// matched on sc_pr_number (falling back to sc_pr_url), closed ones
// included, so a pull request gets one bead for its lifetime. A failure
// to create one bead does not stop the others; the first error is
// returned together with the partial result.
func (w *PRWatcher) Poll(ctx context.Context) (*PollResult, error) {
	prs, err := w.gh.ListOpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	res := &PollResult{}
	for _, pr := range prs {
		if IsCannonPR(pr) {
			res.Skipped = append(res.Skipped, pr)
		} else {
			res.External = append(res.External, pr)
		}
	}

	existing, err := w.store.List(ctx, beads.ListFilter{All: true, Labels: []string{beads.LabelExternalPR}})
	if err != nil {
		return res, fmt.Errorf("gitx: listing review beads: %w", err)
	}
	byNumber := map[int]beads.Issue{}
	byURL := map[string]beads.Issue{}
	for _, is := range existing {
		if n := is.PRNumber(); n > 0 {
			if _, dup := byNumber[n]; !dup {
				byNumber[n] = is
			}
		}
		if u := is.PRURL(); u != "" {
			if _, dup := byURL[u]; !dup {
				byURL[u] = is
			}
		}
	}

	open := map[domain.BeadID]bool{}
	var firstErr error
	for _, pr := range res.External {
		is, ok := byNumber[pr.Number]
		if !ok && pr.URL != "" {
			is, ok = byURL[pr.URL]
		}
		if ok {
			open[is.ID] = true
			res.Existing = append(res.Existing, ReviewBead{Issue: is, PR: pr})
			continue
		}
		created, err := w.store.Create(ctx, ReviewBeadInput(pr, w.run))
		if err != nil {
			w.logger.WarnContext(ctx, "creating review bead failed", "pr", pr.Number, "url", pr.URL, "error", err.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("gitx: creating review bead for PR #%d: %w", pr.Number, err)
			}
			continue
		}
		open[created.ID] = true
		res.New = append(res.New, ReviewBead{Issue: created, PR: pr})
		w.logger.InfoContext(ctx, "review bead created for external pull request", "bead", created.ID, "pr", pr.Number, "url", pr.URL)
	}

	skipped := map[int]PullRequest{}
	skippedURL := map[string]PullRequest{}
	for _, pr := range res.Skipped {
		if _, dup := skipped[pr.Number]; !dup {
			skipped[pr.Number] = pr
		}
		if pr.URL != "" {
			if _, dup := skippedURL[pr.URL]; !dup {
				skippedURL[pr.URL] = pr
			}
		}
	}
	for _, is := range existing {
		if open[is.ID] || is.Status == string(domain.BeadStateClosed) {
			continue
		}
		res.Stale = append(res.Stale, w.staleReview(is, skipped, skippedURL))
	}
	return res, firstErr
}

// staleReview classifies one unmatched review bead. A pull request that is
// still open and was only skipped as the cannon's own is [StaleCannonPR],
// not a pull request that has gone; a bead with no pull request at all is
// [StaleUnknownPR]; everything else really is no longer in the open
// listing. A bead carrying another run's id is marked so, because the
// dedupe listing deliberately spans every run.
func (w *PRWatcher) staleReview(is beads.Issue, skipped map[int]PullRequest, skippedURL map[string]PullRequest) StaleReview {
	st := StaleReview{Issue: is, Reason: StaleClosed}
	if w.run != "" {
		if run := is.RunID(); run != "" && run != w.run {
			st.ForeignRun = true
		}
	}
	number, url := is.PRNumber(), is.PRURL()
	if number <= 0 && url == "" {
		st.Reason = StaleUnknownPR
		return st
	}
	if pr, ok := skipped[number]; ok && number > 0 {
		st.Reason, st.PR = StaleCannonPR, pr
		return st
	}
	if pr, ok := skippedURL[url]; ok && url != "" {
		st.Reason, st.PR = StaleCannonPR, pr
		return st
	}
	return st
}

// Run polls immediately and then every interval until ctx is done,
// calling onPoll (when non-nil) with each result, including partial ones.
// A poll that fails because gh is missing, not authenticated, or the
// repository has no GitHub remote ends the loop with that error, since
// polling again cannot help; any other failure is logged and retried at
// the next tick. When ctx ends, Run returns nil.
func (w *PRWatcher) Run(ctx context.Context, onPoll func(*PollResult)) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		res, err := w.Poll(ctx)
		if res != nil && onPoll != nil {
			onPoll(res)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrGHNotFound) || errors.Is(err, ErrGHUnauthenticated) || errors.Is(err, ErrNoGitHubRemote) {
				return err
			}
			w.logger.WarnContext(ctx, "pull request poll failed; retrying next tick", "error", err.Error(), "interval", w.interval)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
