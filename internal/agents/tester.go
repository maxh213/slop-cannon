package agents

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// This file is the tester role (P5-04). A tester reviews one engineer's work
// through a single [domain.Lens] — ux, qa or test — and always answers with
// one `verdict` message.
//
// It has three modes ([TesterMode]):
//
//   - [TesterModeReview]: the first look at a round. Input is the engineer's
//     impl_report and the diff (internal/gitx, P4-04); output is a verdict
//     whose items are all open.
//   - [TesterModeReReview]: the items this tester raised before, the
//     engineer's responses and the new diff; output is a verdict that carries
//     every one of those items forward with an updated status.
//   - [TesterModeEvaluatePushback]: the engineer disagreed. The tester accepts
//     the argument (the item stops blocking) or holds it with a reason, which
//     is what the product owner reads when it arbitrates (D-08).
//
// Testers run under cartridge.ToolReadExec (P5-06): they may read files and
// run the suite, but never edit. Build the firing with [TesterRunSpec] or
// [TesterFiring] and read the answer back with [ParseTesterVerdict], which
// enforces the mode's rules on top of the JSON Schema.

// TesterMode is what a tester was fired to do.
type TesterMode string

// Tester modes.
const (
	// TesterModeReview is the first review of a round: diff plus impl report
	// in, verdict out.
	TesterModeReview TesterMode = "review"
	// TesterModeReReview is a later look at the same bead: the previous
	// items and the engineer's responses in, a verdict with updated item
	// statuses out.
	TesterModeReReview TesterMode = "re_review"
	// TesterModeEvaluatePushback judges the engineer's pushback on items
	// this tester raised: accept, or hold with a reason (D-08).
	TesterModeEvaluatePushback TesterMode = "evaluate_pushback"
)

var testerModes = []TesterMode{TesterModeReview, TesterModeReReview, TesterModeEvaluatePushback}

// TesterModes returns every valid mode in declaration order.
func TesterModes() []TesterMode { return append([]TesterMode(nil), testerModes...) }

// ParseTesterMode parses a mode name leniently: trimmed, case-insensitive,
// hyphens and spaces accepted for underscores. The error wraps
// domain.ErrUnknownValue.
func ParseTesterMode(s string) (TesterMode, error) {
	n := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(s)))
	for _, m := range testerModes {
		if string(m) == n {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w: tester mode %q (want one of %s)", domain.ErrUnknownValue, s, joinTesterModes())
}

// String returns the canonical name.
func (m TesterMode) String() string { return string(m) }

// Valid reports whether m is one of the declared modes.
func (m TesterMode) Valid() bool {
	for _, x := range testerModes {
		if x == m {
			return true
		}
	}
	return false
}

// Task returns the events.Task the mode is fired as: a re-review is still a
// review as far as the orchestrator and the tool policy are concerned.
func (m TesterMode) Task() events.Task {
	if m == TesterModeEvaluatePushback {
		return events.TaskEvaluatePushback
	}
	return events.TaskReview
}

// joinTesterModes renders the modes for error messages.
func joinTesterModes() string {
	names := make([]string, len(testerModes))
	for i, m := range testerModes {
		names[i] = m.String()
	}
	return strings.Join(names, ", ")
}

// TesterRequest is one firing of a tester: which lens, which mode, and
// everything the agent needs to see. Build the prompt with
// [BuildTesterPrompt], the whole firing with [TesterRunSpec] or
// [TesterFiring], and read the answer back with [ParseTesterVerdict].
type TesterRequest struct {
	// Mode is what this firing does; see [TesterMode].
	Mode TesterMode
	// Lens is the perspective the tester reviews from. Required.
	Lens domain.Lens
	// RunID and AgentID identify the firing.
	RunID   domain.RunID
	AgentID domain.AgentID
	// Workdir is the absolute directory the cartridge runs in: the engineer's
	// worktree, which the tester reads but must not write (D-23).
	Workdir string
	// Round is the review round, 1-based.
	Round int
	// Bead is the bead under review, with its worktree and branch.
	Bead prompt.BeadContext
	// Repo is the repository context (prompt.ScanRepo).
	Repo prompt.RepoContext
	// Brief is the human's -m text, when the run has one.
	Brief string
	// Rules are the hard constraints; a violation of any of them is a
	// blocker in the [domain.CategoryRules] category, every round.
	Rules []prompt.Doc
	// Vibes are the humans' stylistic preferences.
	Vibes []prompt.Doc
	// History is the feedback loop so far, oldest round first.
	History []prompt.Round
	// Report is the engineer's impl_report for the round under review.
	Report *domain.ImplReport
	// Diff is the change set the tester reviews (gitx.Repo.Diff or
	// gitx.Manager.Diff, P4-04). Required for review and re-review; ignored
	// when evaluating pushback, where the code has not moved.
	Diff *gitx.Diff
	// Items are the feedback items this tester carries into the firing: the
	// ones it raised in the previous round (re-review) or the ones the
	// engineer pushed back on (evaluate pushback).
	Items []domain.FeedbackItem
	// Responses are the engineer's answers to Items.
	Responses []domain.Response
	// Notes are extra lines for the agent, for example "the check command
	// failed at delivery time".
	Notes []string
	// Budget bounds the assembled prompt; the zero value means the defaults.
	Budget prompt.Budget
	// Model, MaxTurns and Timeout are passed to the cartridge; zero values
	// mean the adapter's defaults.
	Model    string
	MaxTurns int
	Timeout  time.Duration
	// Env holds variables set on the cartridge process on top of the
	// inherited allowlist, and is where the read-only bd shim goes: build
	// it with UseShim so `bd` on the agent's PATH refuses every write
	// (D-21). TesterRunSpec copies the map; it is never retained.
	Env map[string]string
	// InheritEnv is the allowlist of parent variables the cartridge passes
	// through; nil means cartridge.DefaultInheritEnv(). PATH is always
	// inherited, which is exactly why Env must carry the shim's PATH.
	InheritEnv []string
}

// UseShim puts the read-only bd shim in front of the environment this
// firing hands the cartridge, creating Env when it is nil. It is how a
// caller honours D-21 for a tester firing: the tester prompt carries
// beads.PromptNote, which promises the agent that the `bd` on its PATH
// refuses every write. Install the shim for the worktree with
// beads.InstallForWorktree (or write one with beads.WriteShim) and pass it
// here before TesterRunSpec.
func (r *TesterRequest) UseShim(s beads.Shim) {
	if r.Env == nil {
		r.Env = make(map[string]string, 1)
	}
	s.Apply(r.Env)
}

// Validate reports the first problem with the request that would make the
// prompt wrong: an unknown mode or lens, a missing bead, a missing diff, or
// missing items in a mode that judges them.
func (r TesterRequest) Validate() error {
	switch {
	case !r.Mode.Valid():
		return fmt.Errorf("%w: tester mode %q (want one of %s)", domain.ErrUnknownValue, string(r.Mode), joinTesterModes())
	case !r.Lens.Valid():
		return fmt.Errorf("%w: tester lens %q (want one of %s)", domain.ErrUnknownValue, string(r.Lens), joinTesterLenses())
	case r.Bead.Bead.ID == "":
		return errors.New("agents: tester request has no bead")
	case r.Round < 1:
		return fmt.Errorf("agents: tester request has round %d; rounds are 1-based", r.Round)
	}
	if r.Mode != TesterModeEvaluatePushback && r.Diff == nil {
		return fmt.Errorf("agents: tester %s needs the diff to review (gitx.Diff)", r.Mode)
	}
	if r.Mode == TesterModeReReview && len(r.Items) == 0 {
		return errors.New("agents: tester re_review needs the items raised in the previous round")
	}
	if r.Mode == TesterModeEvaluatePushback {
		if len(r.Items) == 0 {
			return errors.New("agents: tester evaluate_pushback needs the items the engineer pushed back on")
		}
		if len(TesterPushbackItems(r.Items, r.Responses)) == 0 {
			return errors.New("agents: tester evaluate_pushback needs at least one pushback or wontfix response")
		}
	}
	return nil
}

// joinTesterLenses lists the lenses for error messages.
func joinTesterLenses() string {
	all := domain.Lenses()
	names := make([]string, len(all))
	for i, l := range all {
		names[i] = l.String()
	}
	return strings.Join(names, ", ")
}

// Agent describes the firing as an events/cartridge agent, for labels and
// for the event stream.
func (r TesterRequest) Agent() domain.Agent {
	return domain.Agent{
		ID:    r.AgentID,
		Role:  domain.RoleTester,
		Lens:  r.Lens,
		Model: r.Model,
		Bead:  r.Bead.Bead.ID,
	}
}

// BuildTesterPrompt assembles the prompt for one tester firing: the tester
// role template, the lens template, the rules and vibes, the repository and
// bead context, the round history, this round's report and diff, the items
// under judgement, the mode's instructions and the `verdict` output
// contract, in that order (see internal/agents/prompt).
// The diff is the one part of a tester prompt internal/agents/prompt cannot
// bound: its degradation ladder trims the repository tree, the older rounds,
// the README and the bead list, none of which is the patch. A patch under
// gitx's byte cap can still be several times the prompt budget, so
// [BuildTesterPrompt] shrinks the patch itself as a last resort, after the
// prompt package has already dropped everything it can.
func BuildTesterPrompt(r TesterRequest) (prompt.Prompt, error) {
	if err := r.Validate(); err != nil {
		return prompt.Prompt{}, err
	}
	steps := testerDiffLadder(r)
	for i, keep := range steps {
		p, err := buildTesterPrompt(r, keep)
		if err != nil {
			return prompt.Prompt{}, err
		}
		if p.Fits() || i == len(steps)-1 {
			if note := testerDiffTruncation(r.Diff, keep); note != "" {
				p.Truncations = append(p.Truncations, note)
			}
			return p, nil
		}
	}
	// testerDiffLadder always returns at least one step, so the loop above
	// always returns.
	return prompt.Prompt{}, errors.New("agents: tester diff ladder is empty")
}

// buildTesterPrompt assembles one candidate prompt, keeping at most keep
// characters of the unified diff (a negative keep means all of it).
func buildTesterPrompt(r TesterRequest, keep int) (prompt.Prompt, error) {
	policy, err := Policy(domain.RoleTester, r.Mode.Task())
	if err != nil {
		return prompt.Prompt{}, err
	}
	contract, err := ContractFor(domain.MessageVerdict)
	if err != nil {
		return prompt.Prompt{}, err
	}
	bead := r.Bead
	if bead.Round == 0 {
		bead.Round = r.Round
	}
	return prompt.Build(prompt.Request{
		Role:         domain.RoleTester,
		Lens:         r.Lens,
		Task:         r.Mode.Task(),
		AgentID:      r.AgentID,
		RunID:        r.RunID,
		ToolPolicy:   policy,
		Objective:    testerObjective(r),
		Brief:        r.Brief,
		Rules:        r.Rules,
		Vibes:        r.Vibes,
		Repo:         r.Repo,
		Bead:         &bead,
		History:      r.History,
		Instructions: testerSections(r, keep),
		Contract:     contract,
		Budget:       r.Budget,
	})
}

// testerDiffSteps are the characters of unified diff kept at each rung of
// the diff ladder, largest first; the last rung, 0, shows the per-file
// summary alone. The ladder starts above them with the whole patch.
var testerDiffSteps = []int{60000, 20000, 6000, 0}

// testerDiffLadder returns the diff sizes to try, whole patch first. Rungs
// that would not shrink this patch are skipped, so the sequence never
// rebuilds the same prompt twice.
func testerDiffLadder(r TesterRequest) []int {
	steps := []int{-1}
	if r.Diff == nil || r.Mode == TesterModeEvaluatePushback {
		return steps
	}
	n := len(testerFullPatch(r.Diff))
	for _, s := range testerDiffSteps {
		if s < n {
			steps = append(steps, s)
		}
	}
	return steps
}

// ErrPromptOverBudget is returned by [TesterRunSpec] and [TesterFiring] when
// the assembled prompt is still larger than its budget after everything that
// can be dropped has been. The spec and the prompt come back with the error,
// because the prompt is usable and only the caller knows whether firing it
// is worth the tokens: check with errors.Is and fire anyway, or raise
// TesterRequest.Budget.MaxTokens and build again.
var ErrPromptOverBudget = errors.New("agents: prompt is over its token budget")

// TesterRunSpec builds the prompt and the cartridge firing for a tester: the
// worktree as the working directory, the read_exec tool policy from
// [ApplyPolicy], and the standard labels. It does not resolve how the
// cartridge will enforce that policy; use [TesterFiring] when a cartridge is
// in hand.
//
// A prompt that is over budget after the diff ladder has run comes back with
// the valid spec and an [ErrPromptOverBudget] error, so no tester is ever
// fired over its budget by accident.
func TesterRunSpec(r TesterRequest) (cartridge.RunSpec, prompt.Prompt, error) {
	if r.AgentID == "" {
		return cartridge.RunSpec{}, prompt.Prompt{}, errors.New("agents: tester request has no agent id")
	}
	if !filepath.IsAbs(r.Workdir) {
		return cartridge.RunSpec{}, prompt.Prompt{}, fmt.Errorf("agents: tester workdir %q is not an absolute path", r.Workdir)
	}
	p, err := BuildTesterPrompt(r)
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	spec := cartridge.RunSpec{
		Prompt:     p.Text,
		Workdir:    r.Workdir,
		Model:      r.Model,
		MaxTurns:   r.MaxTurns,
		Timeout:    r.Timeout,
		Env:        cloneEngineerEnv(r.Env),
		InheritEnv: append([]string(nil), r.InheritEnv...),
		Labels:     cartridge.LabelsFor(r.RunID, r.Agent(), r.Mode.Task(), r.Round),
	}
	if err := ApplyPolicy(&spec, domain.RoleTester, r.Mode.Task()); err != nil {
		return cartridge.RunSpec{}, p, err
	}
	if err := spec.Validate(); err != nil {
		return cartridge.RunSpec{}, p, err
	}
	if p.OverBudget {
		return spec, p, fmt.Errorf("%w: the %s %s prompt is ~%d tokens against a budget of %d, even with %s; "+
			"raise the budget or fire it deliberately", ErrPromptOverBudget, r.Lens, r.Mode, p.Tokens, p.MaxTokens,
			testerTruncationSummary(p))
	}
	return spec, p, nil
}

// testerTruncationSummary lists what the budget already forced out, for the
// over-budget error message.
func testerTruncationSummary(p prompt.Prompt) string {
	if len(p.Truncations) == 0 {
		return "nothing left to drop"
	}
	return strings.Join(p.Truncations, "; ")
}

// TesterFiring is [TesterRunSpec] plus the P5-06 caller contract: it resolves
// how c will hold the read_exec policy and refuses the firing when the answer
// is the unimplemented copied-worktree degrade. Log [Enforcement.String] and
// fire the returned spec.
//
// An over-budget prompt is reported the way [TesterRunSpec] reports it — the
// spec, the enforcement and an [ErrPromptOverBudget] error together — but
// only once the tool policy has been checked, so a caller that decides to
// fire anyway is never firing an unenforced one.
func TesterFiring(r TesterRequest, c cartridge.Cartridge) (cartridge.RunSpec, prompt.Prompt, Enforcement, error) {
	spec, p, specErr := TesterRunSpec(r)
	if specErr != nil && !errors.Is(specErr, ErrPromptOverBudget) {
		return cartridge.RunSpec{}, p, Enforcement{}, specErr
	}
	en, err := EnforcementFor(c, spec.ToolPolicy)
	if err != nil {
		return cartridge.RunSpec{}, p, Enforcement{}, err
	}
	if err := en.Check(); err != nil {
		return cartridge.RunSpec{}, p, en, err
	}
	return spec, p, en, specErr
}

// testerObjective is the one-or-two-sentence statement of what this firing
// must achieve, shown at the top of the assignment block.
func testerObjective(r TesterRequest) string {
	round := strconv.Itoa(r.Round)
	switch r.Mode {
	case TesterModeReReview:
		return "Re-review round " + round + " of this bead through your " + r.Lens.String() +
			" lens: check what the engineer did about the items you raised, then give one updated verdict."
	case TesterModeEvaluatePushback:
		return "The engineer pushed back on items you raised in round " + round +
			". Judge each argument on its merits and give one verdict that accepts or holds every one of them."
	default:
		return "Review round " + round + " of this bead through your " + r.Lens.String() +
			" lens and give one verdict."
	}
}

// testerSections are the mode-specific instruction sections, rendered after
// the shared context and before the output contract. keep bounds the unified
// diff; a negative keep renders all of it.
func testerSections(r TesterRequest, keep int) []prompt.Section {
	var out []prompt.Section
	if body := renderTesterNotes(r.Notes); body != "" {
		out = append(out, prompt.Section{Title: "Notes from the orchestrator", Body: body})
	}
	if r.Report != nil {
		out = append(out, prompt.Section{Title: "The engineer's report for this round", Body: renderTesterReport(*r.Report)})
	}
	if r.Mode != TesterModeEvaluatePushback {
		out = append(out, prompt.Section{Title: "The change under review", Body: renderTesterDiff(r.Diff, keep)})
	}
	if len(r.Items) > 0 {
		out = append(out, prompt.Section{Title: testerItemsTitle(r.Mode), Body: renderTesterItems(r.Items, r.Responses)})
	}
	out = append(out, prompt.Section{Title: "What to do now", Body: testerInstructions(r)})
	return out
}

// testerItemsTitle names the items section for the mode.
func testerItemsTitle(m TesterMode) string {
	if m == TesterModeEvaluatePushback {
		return "The items the engineer pushed back on"
	}
	return "The items you raised last round"
}

// testerInstructions is the body of the "What to do now" section: the
// mode's rules, in the order the tester should apply them.
func testerInstructions(r TesterRequest) string {
	idHint := fmt.Sprintf("`%s-r%d-1`, `%s-r%d-2`, …", r.Lens, r.Round, r.Lens, r.Round)
	common := []string{
		"Read the diff, then verify rather than guess: your tool policy lets you run the test suite, " +
			"the check command and anything else that reads the tree, but you must not edit a single file.",
		"Hold the work to the bead's acceptance criteria. An unmet criterion is at least `major`; " +
			"work you cannot verify at all is a `blocker`.",
		"Check the change against every line of the Rules section. A violation is category `" +
			domain.CategoryRules + "` and severity `blocker`, always, and you must look for one every round.",
		"Stay inside your lens and inside this diff. Another tester owns the other lenses, and problems the " +
			"diff does not touch belong in a new bead, not in this verdict.",
		"Every item needs `id`, `severity`, `title` and enough `detail` for the engineer to act without asking you " +
			"a question; add `file` and `line` when you can point at one, and `suggestion` when you know the fix.",
	}
	var lines []string
	switch r.Mode {
	case TesterModeReReview:
		lines = append(common,
			"Carry every item listed above into your `items` list with its **original id**, `severity` and `category`, "+
				"unchanged, and set its `status`: "+
				"`resolved` when the fix is real and complete, `accepted` when you accept the engineer's pushback or wontfix, "+
				"`held` when the pushback does not hold (say why in `detail`), `open` when it is simply still wrong.",
			"An item in the category `"+domain.CategoryRules+"` — a violation of a rules file, always severity `blocker` — "+
				"cannot be `accepted` and cannot be downgraded: it is `resolved` once the diff fixes it and `open` until then.",
			"Judge the fix, not the promise: a response saying `fixed` that the diff does not show is still `open`.",
			"New problems this round get new ids ("+idHint+") and status `open`; only an item you raised before may be settled.",
			"Set `outcome` to `changes` while any item is `open` or `held`, and to `approve` only when none is. "+
				"Approving costs the engineer nothing; holding out for perfection costs it a whole round.",
		)
	case TesterModeEvaluatePushback:
		lines = []string{
			"Judge each argument above on its merits. You are deciding whether the item still blocks, not who is right.",
			"Set the item's `status` to `accepted` when the engineer's reasoning is sound — say so in `detail` — " +
				"and to `held` when it is not, with the reason it does not hold. That reason is what the product owner reads " +
				"when it arbitrates, so make it specific.",
			"An item in the category `" + domain.CategoryRules + "` — a violation of a rules file, always severity `blocker` — " +
				"cannot be accepted: it stays a blocker until the code changes.",
			"Answer every item listed above, with its **original id** and its original `severity` and `category`. " +
				"Do not raise new items in this mode and do not renumber anything.",
			"Set `outcome` to `changes` while any item is `held` or `open`, and to `approve` when every one is `accepted`.",
		}
	default:
		lines = append(common,
			"Give each item a stable id of the form `<lens>-r<round>-<n>` ("+idHint+"); the engineer answers you by that id "+
				"and later rounds refer back to it.",
			"Leave every item's `status` as `open`: this is the first look at this round, so nothing is settled yet.",
			"Set `outcome` to `changes` when you raised anything that must be addressed, and to `approve` when the work meets "+
				"the acceptance criteria and your lens is satisfied. Do not raise items just to look thorough.",
		)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTesterNotes renders the orchestrator's notes as bullets.
func renderTesterNotes(notes []string) string {
	var b strings.Builder
	for _, n := range notes {
		if s := strings.TrimSpace(n); s != "" {
			b.WriteString("- " + s + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTesterReport renders the engineer's report for the round under
// review. The round history renders earlier reports itself; this is the one
// the tester is judging now.
func renderTesterReport(rep domain.ImplReport) string {
	var b strings.Builder
	if s := strings.TrimSpace(rep.Summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	if len(rep.FilesChanged) > 0 {
		fmt.Fprintf(&b, "- Files changed: %s\n", strings.Join(rep.FilesChanged, ", "))
	}
	if rep.Commit != "" {
		fmt.Fprintf(&b, "- Commit: %s\n", rep.Commit)
	}
	if s := strings.TrimSpace(rep.HowToVerify); s != "" {
		fmt.Fprintf(&b, "- How to verify: %s\n", s)
	}
	for _, x := range rep.Risks {
		if s := strings.TrimSpace(x); s != "" {
			fmt.Fprintf(&b, "- Risk the engineer flagged: %s\n", s)
		}
	}
	for _, q := range rep.Questions {
		if s := strings.TrimSpace(q); s != "" {
			fmt.Fprintf(&b, "- Question for you: %s\n", s)
		}
	}
	b.WriteString("\nThe report is the engineer's own account. Check it against the diff.")
	return strings.TrimRight(b.String(), "\n")
}

// renderTesterDiff renders the change set under review: the patch itself
// when gitx kept it and it fits the prompt budget, and the per-file summary
// when the patch was over gitx's cap (gitx.DefaultMaxDiffBytes) or has to be
// dropped for the budget. keep bounds the patch in characters; a negative
// keep renders all of it and 0 renders the summary alone.
func renderTesterDiff(d *gitx.Diff, keep int) string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Committed changes on the branch, measured against the merge-base `%s` (head `%s`).\n", d.Base(), d.Head())
	if d.Empty() {
		b.WriteString("\nThe branch has **no committed changes** against that base. There is nothing to approve: " +
			"say so and ask for the work.")
		return b.String()
	}
	fmt.Fprintf(&b, "\n%s\n", d.Stats.String())
	if d.Truncated {
		fmt.Fprintf(&b, "\nThe unified diff is %d bytes, over the %d-byte cap, so only the file summary is included. "+
			"Open the files in your worktree to read the change.\n\n%s", d.Bytes, d.MaxBytes, prompt.Fenced("", d.Stat()))
		return strings.TrimRight(b.String(), "\n")
	}
	full := testerFullPatch(d)
	patch := full
	cut := keep >= 0 && keep < len(full)
	if cut {
		patch = testerCutPatch(full, keep)
	}
	switch {
	case cut && patch == "":
		fmt.Fprintf(&b, "\nThe unified diff is %d bytes, too large for this prompt's budget, so only the file summary is "+
			"included. Read the change in your worktree — you have the tools to.\n\n%s", len(full), prompt.Fenced("", d.Stat()))
	case cut:
		fmt.Fprintf(&b, "\nThe unified diff is %d bytes, too large for this prompt's budget, so only its first %d bytes "+
			"are shown. Read the rest in your worktree before you judge it.\n\n%s", len(full), len(patch), prompt.Fenced("diff", patch))
	default:
		fmt.Fprintf(&b, "\n%s", prompt.Fenced("diff", patch))
	}
	return strings.TrimRight(b.String(), "\n")
}

// testerFullPatch is the patch as it would be rendered, with the trailing
// newlines the renderer drops already gone, so sizes match what the prompt
// carries.
func testerFullPatch(d *gitx.Diff) string {
	if d == nil || d.Truncated {
		return ""
	}
	return strings.TrimRight(d.Patch, "\n")
}

// testerCutPatch keeps at most keep characters of patch, cut at the last
// line boundary so the tester never reads half a hunk line.
func testerCutPatch(patch string, keep int) string {
	if keep <= 0 {
		return ""
	}
	if keep >= len(patch) {
		return patch
	}
	head := patch[:keep]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		return head[:i]
	}
	return head
}

// testerDiffTruncation describes what the diff ladder dropped, in the shape
// prompt.Prompt.Truncations uses; it returns "" when nothing was dropped.
func testerDiffTruncation(d *gitx.Diff, keep int) string {
	full := testerFullPatch(d)
	if keep < 0 || keep >= len(full) {
		return ""
	}
	kept := len(testerCutPatch(full, keep))
	if kept == 0 {
		return fmt.Sprintf("diff patch omitted (%d bytes); only the per-file summary is shown", len(full))
	}
	return fmt.Sprintf("diff patch cut to %d of %d bytes", kept, len(full))
}

// renderTesterItems renders the items under judgement with the engineer's
// response to each, so the tester sees claim and evidence together.
func renderTesterItems(items []domain.FeedbackItem, responses []domain.Response) string {
	byItem := make(map[string]domain.Response, len(responses))
	for _, resp := range responses {
		byItem[resp.ItemID] = resp
	}
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "### `%s` [%s", it.ID, it.Severity)
		if it.Category != "" {
			b.WriteString("/" + it.Category)
		}
		b.WriteString("] " + testerOneLine(it.Title) + "\n")
		if it.File != "" {
			fmt.Fprintf(&b, "\n- Where: %s%s\n", it.File, testerLineSuffix(it.Line))
		}
		if it.Status != "" {
			fmt.Fprintf(&b, "- Status you left it at: %s\n", it.Status)
		}
		if s := strings.TrimSpace(it.Detail); s != "" {
			b.WriteString("\n" + s + "\n")
		}
		if s := strings.TrimSpace(it.Suggestion); s != "" {
			b.WriteString("\nYour suggestion: " + s + "\n")
		}
		if resp, ok := byItem[it.ID]; ok {
			fmt.Fprintf(&b, "\nEngineer: **%s**", resp.Action)
			if s := strings.TrimSpace(resp.Rationale); s != "" {
				b.WriteString(" — " + s)
			}
			b.WriteString("\n")
		} else {
			b.WriteString("\nEngineer: no response to this item.\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// testerLineSuffix renders ":<line>" for a positive line number.
func testerLineSuffix(line int) string {
	if line <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(line)
}

// testerOneLine collapses a value onto a single line.
func testerOneLine(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// TesterPushbackItems returns the items whose response was pushback or
// wontfix, in the order of items. It is how the orchestrator builds a
// [TesterModeEvaluatePushback] request out of the previous round.
func TesterPushbackItems(items []domain.FeedbackItem, responses []domain.Response) []domain.FeedbackItem {
	actions := make(map[string]domain.ResponseAction, len(responses))
	for _, r := range responses {
		actions[r.ItemID] = r.Action
	}
	var out []domain.FeedbackItem
	for _, it := range items {
		switch actions[it.ID] {
		case domain.ActionPushback, domain.ActionWontfix:
			out = append(out, it)
		}
	}
	return out
}

// TesterOpenItems returns the items of a verdict that still need somebody's
// attention: everything whose status is not settled (open and held).
func TesterOpenItems(v domain.Verdict) []domain.FeedbackItem {
	var out []domain.FeedbackItem
	for _, it := range v.Items {
		if !it.Status.Settled() {
			out = append(out, it)
		}
	}
	return out
}

// NormaliseTesterVerdict fills in what the tester may leave out because the
// orchestrator already knows it: the lens, the round, an item status of
// open, and the category of a carried item, which is the request's when the
// tester did not restate it (the schema makes category optional, and an
// omission must not read as a rewrite). It never overrides a lens the tester
// stated (see [ValidateTesterVerdict], which rejects a mismatched one) and
// never rewrites a category the tester did state.
func NormaliseTesterVerdict(r TesterRequest, v domain.Verdict) domain.Verdict {
	if v.Lens == "" {
		v.Lens = r.Lens
	}
	v.Round = r.Round
	if len(v.Items) > 0 {
		carried := map[string]domain.FeedbackItem{}
		if r.Mode == TesterModeReReview || r.Mode == TesterModeEvaluatePushback {
			for _, it := range r.Items {
				carried[it.ID] = it
			}
		}
		items := make([]domain.FeedbackItem, len(v.Items))
		copy(items, v.Items)
		for i := range items {
			if items[i].Status == "" {
				items[i].Status = domain.ItemOpen
			}
			if items[i].Category == "" {
				items[i].Category = carried[items[i].ID].Category
			}
		}
		v.Items = items
	}
	return v
}

// ValidateTesterVerdict checks what the JSON Schema cannot: that the verdict
// belongs to this firing, that its item statuses are consistent with its
// outcome, and that the mode's own rules hold — a first review settles
// nothing; a re-review carries every earlier item forward with its original
// severity and category and starts any new item open; a pushback evaluation
// answers exactly the disputed items; and in neither of the two modes that
// judge carried items may a [domain.CategoryRules] item be accepted, because
// a rules violation stays a blocker until the code changes. Call
// [NormaliseTesterVerdict] first.
//
// Failures come back as a *structured.ParseError with
// structured.ReasonSchema, so P5-08's retry loop can feed
// ParseError.Feedback straight back to the model.
func ValidateTesterVerdict(r TesterRequest, v domain.Verdict) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if !v.Outcome.Valid() {
		add("outcome %q is not one of approve, changes", string(v.Outcome))
	}
	if v.Lens != "" && r.Lens != "" && v.Lens != r.Lens {
		add("lens is %q but you are the %q tester; report only your own lens", string(v.Lens), r.Lens.String())
	}

	seen := make(map[string]bool, len(v.Items))
	known := make(map[string]domain.FeedbackItem, len(r.Items))
	for _, it := range r.Items {
		known[it.ID] = it
	}
	unsettled := 0
	for i, it := range v.Items {
		where := fmt.Sprintf("item %d", i+1)
		if it.ID != "" {
			where = fmt.Sprintf("item %q", it.ID)
		}
		if it.ID == "" {
			add("%s has no id", where)
		} else if seen[it.ID] {
			add("%s appears twice; every item needs its own id", where)
		}
		seen[it.ID] = true
		if !it.Severity.Valid() {
			add("%s has severity %q, which is not one of blocker, major, minor, nit", where, string(it.Severity))
		}
		if strings.TrimSpace(it.Title) == "" {
			add("%s has no title", where)
		}
		if it.Category == domain.CategoryRules && it.Severity != domain.SeverityBlocker {
			add("%s is in the %q category but has severity %q; a rules violation is always a blocker",
				where, domain.CategoryRules, string(it.Severity))
		}
		if !it.Status.Valid() {
			add("%s has status %q, which is not one of open, resolved, accepted, held, overruled", where, string(it.Status))
			continue
		}
		if !it.Status.Settled() {
			unsettled++
		}
		if it.Status == domain.ItemOverruled {
			add("%s is marked overruled; only the product owner overrules an item (D-08)", where)
		}
		validateTesterItemForMode(r, it, where, known, add)
	}

	switch {
	case v.Outcome == domain.VerdictApprove && unsettled > 0:
		add("the outcome is approve but %d item(s) are still open or held; approve only when nothing is left unsettled", unsettled)
	case v.Outcome == domain.VerdictChanges && len(v.Items) == 0:
		add("the outcome is changes but no items are listed; say what must change")
	case v.Outcome == domain.VerdictChanges && unsettled == 0:
		add("the outcome is changes but every item is settled; approve instead, or say what is still open")
	}
	problems = append(problems, testerCoverageProblems(r, seen)...)

	if len(problems) == 0 {
		return nil
	}
	return &structured.ParseError{
		Reason:   structured.ReasonSchema,
		Contract: domain.MessageVerdict.String(),
		Detail:   fmt.Sprintf("the verdict does not follow the rules of a %s in the %s lens", r.Mode, r.Lens),
		Problems: problems,
	}
}

// validateTesterItemForMode applies the per-mode rules to one item.
func validateTesterItemForMode(r TesterRequest, it domain.FeedbackItem, where string,
	known map[string]domain.FeedbackItem, add func(string, ...any)) {
	switch r.Mode {
	case TesterModeReview:
		if it.Status != domain.ItemOpen {
			add("%s has status %q; this is a first review, so every item you raise is open", where, string(it.Status))
		}
	case TesterModeReReview:
		orig, carried := known[it.ID]
		if !carried {
			if it.Status != domain.ItemOpen {
				add("%s has status %q; it is not one of the items you raised last round, so a new problem starts open",
					where, string(it.Status))
			}
			return
		}
		validateTesterCarriedItem(it, orig, where, add)
		switch it.Status {
		case domain.ItemAccepted:
			if orig.Category == domain.CategoryRules || it.Category == domain.CategoryRules {
				add("%s is in the %q category and cannot be accepted; a rules violation stays a blocker until the code changes, "+
					"so mark it resolved once the diff fixes it and open until then", where, domain.CategoryRules)
			}
		case domain.ItemHeld:
			if strings.TrimSpace(it.Detail) == "" {
				add("%s is held but gives no reason; the product owner arbitrates on that reason (D-08)", where)
			}
		}
	case TesterModeEvaluatePushback:
		orig, ok := known[it.ID]
		if !ok {
			add("%s was not one of the items under dispute; judge only the items listed and do not raise new ones", where)
			return
		}
		validateTesterCarriedItem(it, orig, where, add)
		switch it.Status {
		case domain.ItemAccepted:
			if orig.Category == domain.CategoryRules {
				add("%s is in the %q category and cannot be accepted; a rules violation stays a blocker until the code changes",
					where, domain.CategoryRules)
			}
		case domain.ItemHeld:
			if strings.TrimSpace(it.Detail) == "" {
				add("%s is held but gives no reason; the product owner arbitrates on that reason (D-08)", where)
			}
		default:
			add("%s has status %q; when you judge a pushback the answer is accepted or held", where, string(it.Status))
		}
	}
}

// validateTesterCarriedItem checks that an item carried out of a previous
// round is still the same item: a tester updates its status and its detail,
// never its severity or its category. Without this a `rules` blocker could
// come back as a `style` nit and be marked resolved, which is the same hole
// as accepting it.
func validateTesterCarriedItem(it, orig domain.FeedbackItem, where string, add func(string, ...any)) {
	if orig.Severity != "" && it.Severity != orig.Severity {
		add("%s came to you with severity %q but you sent it back as %q; carry an item forward with its original severity",
			where, string(orig.Severity), string(it.Severity))
	}
	if orig.Category != it.Category {
		add("%s came to you with category %s but you sent it back as %s; carry an item forward with its original category",
			where, testerCategoryName(orig.Category), testerCategoryName(it.Category))
	}
}

// testerCategoryName renders a category for an error message, naming the
// empty one rather than quoting nothing.
func testerCategoryName(c string) string {
	if strings.TrimSpace(c) == "" {
		return "no category"
	}
	return strconv.Quote(c)
}

// testerCoverageProblems reports the carried items a re-review or pushback
// evaluation forgot to answer.
func testerCoverageProblems(r TesterRequest, seen map[string]bool) []string {
	var want []domain.FeedbackItem
	switch r.Mode {
	case TesterModeReReview:
		want = r.Items
	case TesterModeEvaluatePushback:
		want = TesterPushbackItems(r.Items, r.Responses)
	default:
		return nil
	}
	var missing []string
	for _, it := range want {
		if !seen[it.ID] {
			missing = append(missing, it.ID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("item(s) %s are missing from your list; carry every item you were given forward with its original id",
		"`"+strings.Join(missing, "`, `")+"`")}
}

// ParseTesterVerdict reads the `verdict` block out of a tester's output,
// validates it against the schema, fills in the lens, round and default item
// statuses, and then checks the mode's own rules ([ValidateTesterVerdict]).
// Every failure is a *structured.ParseError, so P5-08 can retry with
// ParseError.Feedback; the returned structured.Parsed carries the raw block
// even when the error is non-nil.
func ParseTesterVerdict(r TesterRequest, output string) (domain.Verdict, structured.Parsed, error) {
	if err := r.Validate(); err != nil {
		return domain.Verdict{}, structured.Parsed{}, err
	}
	v, p, err := ParseVerdict(output)
	if err != nil {
		return v, p, err
	}
	v = NormaliseTesterVerdict(r, v)
	if err := ValidateTesterVerdict(r, v); err != nil {
		return v, p, err
	}
	return v, p, nil
}
