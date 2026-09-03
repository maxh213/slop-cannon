package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Gates are bd's native "wait for something outside the tracker" issues
// (D-20). With safety on, `sc` opens a pull request for a bead, keeps the
// bead in `delivering`, and creates a `gh:pr` gate that blocks it; the gate
// keeps the bead out of `bd ready` and `bd close` for its dependents until
// the human merges. `bd gate check --type=gh:pr` asks `gh pr view <n>
// --json state,title` and closes the gate when the PR is MERGED, reports
// ESCALATE when it is CLOSED without merging (or no longer exists), and
// reports an error per gate when `gh` is missing, not authenticated or
// cannot reach GitHub. bd 1.2.2 changes nothing on the gate for an
// escalation (it only shells out
// to Gas Town's `gt escalate` under `--escalate`, which is a warning when
// `gt` is absent), so the caller acts on GateEscalated itself: move the
// bead to `needs_human` and, if it wants the gate out of the way, resolve
// it.

// GateTypePR is the bd gate type that waits for a pull request to merge.
const GateTypePR = "gh:pr"

// ErrNoGate is returned by GateFor when no open gate blocks the bead.
var ErrNoGate = errors.New("no open gate blocks the bead")

// Gate is a gate issue as `bd gate create/list/show --json` print it
// (issue_type "gate"). bd 1.2.2 prints no field naming the blocked issue;
// Blocks is recovered from the description ("Ad-hoc gate blocking <id>").
type Gate struct {
	// ID is the gate's own issue id.
	ID domain.BeadID `json:"id"`
	// Title is bd's "Gate: gh:pr <n>".
	Title string `json:"title"`
	// Description is bd's "Ad-hoc gate blocking <id>\n\nReason: <reason>".
	Description string `json:"description,omitempty"`
	// Status is "open" until the gate resolves ("closed").
	Status string `json:"status"`
	// Priority is bd's default 2 for gates.
	Priority domain.Priority `json:"priority"`
	// Type is the issue type, always "gate".
	Type string `json:"issue_type,omitempty"`
	// Owner and CreatedBy are bd's bookkeeping of who made the gate.
	Owner     string `json:"owner,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedAt, UpdatedAt and ClosedAt are bd's timestamps.
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	// CloseReason is what closed the gate: bd writes "PR '<title>' was
	// merged" when a check resolves it.
	CloseReason string `json:"close_reason,omitempty"`
	// AwaitType is the gate type (GateTypePR, "gh:run", "timer", "human").
	AwaitType string `json:"await_type,omitempty"`
	// AwaitID is the condition identifier: the PR number for gh:pr gates,
	// as the string bd stores.
	AwaitID string `json:"await_id,omitempty"`
	// Blocks is the issue the gate blocks, taken from the description.
	Blocks domain.BeadID `json:"-"`
}

// gateBlocksRe matches the blocked issue in bd's gate description.
var gateBlocksRe = regexp.MustCompile(`(?m)^Ad-hoc gate blocking (\S+)`)

// UnmarshalJSON implements json.Unmarshaler: a missing priority decodes as
// domain.PriorityDefault and Blocks is filled from the description.
func (g *Gate) UnmarshalJSON(data []byte) error {
	type plain Gate
	v := plain{Priority: domain.PriorityDefault}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if m := gateBlocksRe.FindStringSubmatch(v.Description); m != nil {
		v.Blocks = domain.BeadID(m[1])
	}
	*g = Gate(v)
	return nil
}

// IsOpen reports whether the gate is still waiting.
func (g Gate) IsOpen() bool { return g.Status != string(domain.BeadStateClosed) }

// IsPR reports whether the gate waits for a pull request merge.
func (g Gate) IsPR() bool { return g.AwaitType == GateTypePR }

// PRNumber returns the pull request number of a gh:pr gate; the second
// result is false for other gate types or when no --await-id was given.
func (g Gate) PRNumber() (int, bool) {
	if !g.IsPR() {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(g.AwaitID))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// CreatePRGate runs `bd gate create --type=gh:pr --blocks <bead> --await-id
// <prNumber> --reason <reason> --json` (D-20) and returns the gate. The
// bead is blocked by the gate from then on: it leaves `bd ready`, and
// `bd close` refuses it until the gate resolves. An unknown bead is
// ErrNotFound; a non-positive PR number is ErrInvalid. An empty reason
// defaults to "PR #<n>".
func (c *Client) CreatePRGate(ctx context.Context, bead domain.BeadID, prNumber int, reason string) (Gate, error) {
	if strings.TrimSpace(string(bead)) == "" {
		return Gate{}, fmt.Errorf("beads: gate create: %w: no bead id", ErrInvalid)
	}
	if prNumber <= 0 {
		return Gate{}, fmt.Errorf("beads: gate create: %w: PR number %d", ErrInvalid, prNumber)
	}
	if strings.TrimSpace(reason) == "" {
		reason = fmt.Sprintf("PR #%d", prNumber)
	}
	args := []string{"gate", "create", "--type=" + GateTypePR, "--blocks", string(bead), "--await-id", strconv.Itoa(prNumber), "--reason", reason, "--json"}
	var g Gate
	if err := c.call(ctx, &g, args...); err != nil {
		return Gate{}, err
	}
	if g.Blocks == "" {
		g.Blocks = bead
	}
	return g, nil
}

// Gates runs `bd gate list -n 0 --json` (plus `--all` when all is set, to
// include closed gates) and returns every gate of the repository, of any
// type. bd prints `null` when there are none, which decodes as an empty
// list.
func (c *Client) Gates(ctx context.Context, all bool) ([]Gate, error) {
	args := []string{"gate", "list", "-n", "0"}
	if all {
		args = append(args, "--all")
	}
	args = append(args, "--json")
	var gates []Gate
	if err := c.call(ctx, &gates, args...); err != nil {
		return nil, err
	}
	return gates, nil
}

// GateFor returns the open gate blocking bead, found through `bd gate list
// --json`. gh:pr gates win over other types and, among several, the most
// recently created one. ErrNoGate (wrapped) is returned when there is none.
func (c *Client) GateFor(ctx context.Context, bead domain.BeadID) (Gate, error) {
	if strings.TrimSpace(string(bead)) == "" {
		return Gate{}, fmt.Errorf("beads: gate for: %w: no bead id", ErrInvalid)
	}
	gates, err := c.Gates(ctx, false)
	if err != nil {
		return Gate{}, err
	}
	var best *Gate
	for i := range gates {
		g := &gates[i]
		if g.Blocks != bead || !g.IsOpen() {
			continue
		}
		if best == nil || (g.IsPR() && !best.IsPR()) || (g.IsPR() == best.IsPR() && g.CreatedAt.After(best.CreatedAt)) {
			best = g
		}
	}
	if best == nil {
		return Gate{}, fmt.Errorf("beads: %w: %s", ErrNoGate, bead)
	}
	return *best, nil
}

// ShowGate runs `bd gate show <id> --json` and returns the gate, open or
// closed. An unknown id is ErrNotFound.
func (c *Client) ShowGate(ctx context.Context, id domain.BeadID) (Gate, error) {
	if strings.TrimSpace(string(id)) == "" {
		return Gate{}, fmt.Errorf("beads: gate show: %w: no gate id", ErrInvalid)
	}
	var g Gate
	if err := c.call(ctx, &g, "gate", "show", string(id), "--json"); err != nil {
		return Gate{}, err
	}
	return g, nil
}

// ResolveGate runs `bd gate resolve <id> [--reason <reason>]`, which closes
// the gate by hand (the equivalent of `bd close` on it) and unblocks the
// bead it guards. bd 1.2.2 prints text even with --json, so nothing is
// decoded. An unknown id is ErrNotFound.
func (c *Client) ResolveGate(ctx context.Context, id domain.BeadID, reason string) error {
	if strings.TrimSpace(string(id)) == "" {
		return fmt.Errorf("beads: gate resolve: %w: no gate id", ErrInvalid)
	}
	args := []string{"gate", "resolve", string(id)}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	args = append(args, "--json")
	_, err := c.run(ctx, args...)
	return err
}

// GateOutcome is what one `bd gate check` run found for one gate.
type GateOutcome string

const (
	// GateResolved means the PR merged; bd closed the gate and the bead is
	// unblocked, so the caller closes it (D-20).
	GateResolved GateOutcome = "resolved"
	// GatePending means the PR is still open; nothing changed.
	GatePending GateOutcome = "pending"
	// GateEscalated means the PR was closed without merging (or no longer
	// exists). bd leaves the gate open; the caller moves the bead to
	// `needs_human`.
	GateEscalated GateOutcome = "escalated"
	// GateError means the check itself failed: `gh` is not installed or not
	// authenticated (see GateCheck.GHUnavailable), GitHub could not be
	// reached, or gh printed something bd could not parse. The gate stays
	// open and is checked again next time.
	GateError GateOutcome = "error"
)

// GateResult is one gate's line of a `bd gate check` run.
type GateResult struct {
	// GateID is the gate issue.
	GateID domain.BeadID
	// Outcome classifies bd's verdict.
	Outcome GateOutcome
	// Detail is bd's explanation: "PR 'title' was merged", "PR 'title' is
	// still open", "PR 'title' was closed without merging", "pull request
	// not found", "gh CLI not installed", "gh pr view failed: <gh's
	// stderr>" (bd's prefix for every non-zero gh exit, whatever the cause).
	Detail string
}

// GateCheckOptions tunes CheckGates.
type GateCheckOptions struct {
	// Escalate passes `--escalate`, which makes bd hand escalated gates to
	// Gas Town's `gt escalate`; when `gt` is absent bd prints a warning per
	// gate (surfaced in Warnings) and the outcome is unchanged.
	Escalate bool
	// DryRun passes `--dry-run`: bd reports what it would do and closes no
	// gate.
	DryRun bool
}

// GateCheck is the outcome of one `bd gate check --type=gh:pr --json` run:
// bd's counts, one GateResult per gate (parsed from the text lines bd
// prints before the JSON envelope, which is the only place the per-gate
// verdicts appear), and the warnings bd printed.
type GateCheck struct {
	// Checked, Resolved, Escalated and Errors are bd's counts.
	Checked   int `json:"checked"`
	Resolved  int `json:"resolved"`
	Escalated int `json:"escalated"`
	Errors    int `json:"errors"`
	// DryRun echoes the --dry-run flag.
	DryRun bool `json:"dry_run"`
	// Results holds one entry per gate bd reported on, in bd's order.
	Results []GateResult `json:"-"`
	// Warnings are the problems that did not fail the call: every gate
	// whose check errored (one line each, "<gate>: <detail>") and bd's own
	// "Warning:" lines (escalation without `gt`). Empty when all is well.
	Warnings []string `json:"-"`
}

// ByOutcome returns the ids of the gates with the given outcome.
func (g GateCheck) ByOutcome(outcome GateOutcome) []domain.BeadID {
	var out []domain.BeadID
	for _, r := range g.Results {
		if r.Outcome == outcome {
			out = append(out, r.GateID)
		}
	}
	return out
}

// GHUnavailable reports whether the checks failed because the `gh` CLI is
// missing or not authenticated (every checked gate errored, and every
// error names gh itself: "gh CLI not installed", "gh auth login",
// GH_TOKEN, "not logged into", "authentication"), together with bd's
// explanation. Callers skip gate polling with a single Warning in that
// case (P6-10) instead of repeating it per gate. A transient failure such
// as a network outage ("gh pr view failed: error connecting to
// api.github.com"), a PR gh could not parse, or an error on only some of
// the gates is not gh being unavailable: those checks are worth retrying
// on the next poll, so the method reports false and the per-gate
// Warnings carry the detail.
func (g GateCheck) GHUnavailable() (string, bool) {
	if g.Checked == 0 || g.Errors == 0 {
		return "", false
	}
	reason := ""
	for _, r := range g.Results {
		if r.Outcome != GateError {
			return "", false
		}
		if !isGHProblem(r.Detail) {
			return "", false
		}
		if reason == "" {
			reason = r.Detail
		}
	}
	if reason == "" {
		return "", false
	}
	return reason, true
}

// ghProblems are the substrings (lower-cased) of bd's per-gate error text
// that mean `gh` itself is missing or unauthenticated. bd prefixes every
// non-zero gh exit with "gh pr view failed: ", so that prefix alone is
// deliberately not in the list: it also covers network outages and other
// transient failures that must not be mistaken for a missing gh.
var ghProblems = []string{
	"gh cli not installed",
	"gh auth login",
	"gh_token",
	"not logged into",
	"authentication",
	"executable file not found",
}

// isGHProblem reports whether detail describes a missing or
// unauthenticated gh (as opposed to a failed check).
func isGHProblem(detail string) bool {
	lower := strings.ToLower(detail)
	for _, p := range ghProblems {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// gateLineRe matches bd's per-gate verdict line: a marker glyph, the gate
// id, the verdict word and the explanation.
var gateLineRe = regexp.MustCompile(`^\S+\s+(\S+): (resolved|pending|ESCALATE|error checking) - (.*)$`)

// gateSummaryRe matches bd's "Checked N gates: …" summary line.
var gateSummaryRe = regexp.MustCompile(`^Checked \d+ gates?: `)

// CheckGates runs `bd gate check --type=gh:pr -l 0 --json` (with
// `--escalate` and `--dry-run` per opts), which asks `gh pr view <n>` for
// every open gh:pr gate and closes the gates whose PR merged. `-l 0` lifts
// bd's default limit of 100 gates per check (D-21: bd's listing defaults
// truncate; with safety on every delivered bead holds an open gate until a
// human merges, so an autopilot run passes 100 easily and a truncated
// check would leave the later beads in `delivering` for ever). The result
// lists every gate's outcome; a gate whose check failed is not an error of
// the call but a GateError and a Warning each, and GateCheck.GHUnavailable
// tells a missing or unauthenticated gh apart from a transient failure.
// Only a failure to run bd is an error. With no open gh:pr gate bd prints a
// text line and no JSON; that is an empty GateCheck.
func (c *Client) CheckGates(ctx context.Context, opts GateCheckOptions) (GateCheck, error) {
	args := []string{"gate", "check", "--type=" + GateTypePR, "-l", "0"}
	if opts.Escalate {
		args = append(args, "--escalate")
	}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, "--json")
	res, err := c.run(ctx, args...)
	if err != nil {
		return GateCheck{}, err
	}
	check, err := ParseGateCheck(res.Stdout, res.Stderr)
	if err != nil {
		return GateCheck{}, fmt.Errorf("bd gate check: %w", err)
	}
	return check, nil
}

// ParseGateCheck decodes the output of `bd gate check --json`: the counts
// from the envelope, one GateResult per verdict line (resolved, pending
// and ESCALATE lines go to stdout, "error checking" lines to stderr, each
// possibly followed by indented or plain continuation lines that belong to
// the detail), and bd's "Warning:" lines. Output without JSON is accepted
// when it is the "No open gates" notice.
func ParseGateCheck(stdout, stderr string) (GateCheck, error) {
	var check GateCheck
	if err := Decode(stdout, &check); err != nil {
		if !errors.Is(err, ErrNoJSON) || !strings.Contains(strings.ToLower(stdout), "no open gates") {
			return GateCheck{}, err
		}
	}
	check.Results = append(check.Results, parseGateLines(stdout)...)
	check.Results = append(check.Results, parseGateLines(stderr)...)
	for _, r := range check.Results {
		if r.Outcome == GateError {
			check.Warnings = append(check.Warnings, fmt.Sprintf("%s: %s", r.GateID, r.Detail))
		}
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "warning:") && !strings.Contains(line, "beads.role") {
			check.Warnings = append(check.Warnings, strings.TrimSpace(line[len("warning:"):]))
		}
	}
	return check, nil
}

// parseGateLines extracts the per-gate verdict lines from one output
// stream, stopping at the summary line or the JSON. Lines that follow a
// verdict without being one themselves (the second line of gh's auth
// hint) extend the previous detail.
func parseGateLines(text string) []GateResult {
	var out []GateResult
	open := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			open = false
			continue
		case gateSummaryRe.MatchString(trimmed), strings.HasPrefix(trimmed, "{"), strings.HasPrefix(trimmed, "["):
			return out
		}
		if m := gateLineRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, GateResult{GateID: domain.BeadID(m[1]), Outcome: gateOutcome(m[2]), Detail: strings.TrimSpace(m[3])})
			open = true
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "warning:") || strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "thanks for using bd") {
			open = false
			continue
		}
		if open && len(out) > 0 {
			out[len(out)-1].Detail += " " + trimmed
		}
	}
	return out
}

// gateOutcome maps bd's verdict word to a GateOutcome.
func gateOutcome(word string) GateOutcome {
	switch word {
	case "resolved":
		return GateResolved
	case "pending":
		return GatePending
	case "ESCALATE":
		return GateEscalated
	default:
		return GateError
	}
}
