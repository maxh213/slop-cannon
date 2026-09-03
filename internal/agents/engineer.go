package agents

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the engineer role (P5-03). An engineer owns exactly one bead
// in its own git worktree and is fired in one of three modes:
//
//   - [EngineerImplement] — write the code for the bead, run the check
//     command, commit, and report an `impl_report` for the testers.
//   - [EngineerAddressFeedback] — answer every feedback item a round of
//     reviews raised, with `fixed`, `pushback` or `wontfix` and a reason
//     (the README requires pushback to be possible, D-08).
//   - [EngineerResolveConflict] — fix the conflicts that stopped the bead
//     from landing (D-10) and report the resolution as an `impl_report`.
//
// Every mode runs under the read_write tool policy ([Policy]); the caller
// builds the firing with [EngineerRequest.Firing], fires it, and reads the
// reply back with [EngineerRequest.ParseResult].

// EngineerMode is which of the three jobs an engineer firing is.
type EngineerMode string

// Engineer modes.
const (
	// EngineerImplement is the first pass at a bead: implement it, verify
	// it, commit it, report it.
	EngineerImplement EngineerMode = "implement"
	// EngineerAddressFeedback is a later round: answer the feedback items
	// the testers raised.
	EngineerAddressFeedback EngineerMode = "address_feedback"
	// EngineerResolveConflict is delivery repair: the rebase onto the
	// default branch stopped on conflicts (D-10).
	EngineerResolveConflict EngineerMode = "resolve_conflict"
)

// engineerModeInfo is the fixed data behind a mode: the task it is fired as
// and the structured message it must produce.
type engineerModeInfo struct {
	task    events.Task
	message domain.MessageType
}

var engineerModeInfos = map[EngineerMode]engineerModeInfo{
	EngineerImplement:       {events.TaskImplement, domain.MessageImplReport},
	EngineerAddressFeedback: {events.TaskAddressFeedback, domain.MessageResponse},
	EngineerResolveConflict: {events.TaskResolveConflict, domain.MessageImplReport},
}

// engineerModeOrder is the declaration order of the modes.
var engineerModeOrder = []EngineerMode{EngineerImplement, EngineerAddressFeedback, EngineerResolveConflict}

// EngineerModes returns every engineer mode in declaration order.
func EngineerModes() []EngineerMode {
	return append([]EngineerMode(nil), engineerModeOrder...)
}

// ParseEngineerMode parses a mode name leniently: trimmed, case-insensitive,
// hyphens accepted for underscores. The error wraps domain.ErrUnknownValue.
func ParseEngineerMode(s string) (EngineerMode, error) {
	n := EngineerMode(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_"))
	if _, ok := engineerModeInfos[n]; !ok {
		return "", fmt.Errorf("%w: engineer mode %q (want one of %s)", domain.ErrUnknownValue, s, joinEngineerModes())
	}
	return n, nil
}

// String returns the canonical name.
func (m EngineerMode) String() string { return string(m) }

// Valid reports whether m is one of the declared modes.
func (m EngineerMode) Valid() bool {
	_, ok := engineerModeInfos[m]
	return ok
}

// Task returns the events.Task the mode is fired as, or the empty task for
// an unknown mode.
func (m EngineerMode) Task() events.Task { return engineerModeInfos[m].task }

// MessageType returns the structured message the mode must produce:
// impl_report for implement and resolve_conflict, response for address
// feedback.
func (m EngineerMode) MessageType() domain.MessageType { return engineerModeInfos[m].message }

// Contract returns the output contract for the mode's message type.
func (m EngineerMode) Contract() (*structured.Contract, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("%w: engineer mode %q (want one of %s)", domain.ErrUnknownValue, string(m), joinEngineerModes())
	}
	return ContractFor(m.MessageType())
}

// EngineerModeForTask returns the mode an engineer task is fired in. A task
// that belongs to another role, or none, yields an error wrapping
// domain.ErrUnknownValue.
func EngineerModeForTask(t events.Task) (EngineerMode, error) {
	for _, m := range engineerModeOrder {
		if engineerModeInfos[m].task == t {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w: task %q is not an engineer task (want one of %s)",
		domain.ErrUnknownValue, string(t), joinEngineerTasks())
}

// joinEngineerModes lists the modes for error messages.
func joinEngineerModes() string {
	parts := make([]string, len(engineerModeOrder))
	for i, m := range engineerModeOrder {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}

// joinEngineerTasks lists the engineer tasks for error messages.
func joinEngineerTasks() string {
	parts := make([]string, len(engineerModeOrder))
	for i, m := range engineerModeOrder {
		parts[i] = engineerModeInfos[m].task.String()
	}
	return strings.Join(parts, ", ")
}

// DefaultConflictContinue is the command an engineer runs to carry on once
// the conflicts of an interrupted rebase are resolved (D-10).
const DefaultConflictContinue = "git rebase --continue"

// EngineerConflict describes the failed rebase an EngineerResolveConflict
// firing has to repair. The orchestrator fills it from the delivery error
// (gitx.DeliveryError: Conflict, Files, Output) so the engineer sees exactly
// what git saw.
type EngineerConflict struct {
	// Target is the branch the work was being rebased onto, normally the
	// repository's default branch.
	Target string
	// Files are the conflicted paths, relative to the worktree root.
	Files []string
	// Output is what git printed, quoted verbatim.
	Output string
	// Continue is the command that resumes the rebase once the conflicts
	// are staged; empty means DefaultConflictContinue.
	Continue string
}

// Empty reports whether the conflict carries nothing an engineer could act
// on.
func (c EngineerConflict) Empty() bool {
	return strings.TrimSpace(c.Target) == "" && len(c.Files) == 0 &&
		strings.TrimSpace(c.Output) == "" && strings.TrimSpace(c.Continue) == ""
}

// continueCommand returns the command that resumes the rebase.
func (c EngineerConflict) continueCommand() string {
	if s := strings.TrimSpace(c.Continue); s != "" {
		return s
	}
	return DefaultConflictContinue
}

// EngineerRequest is one engineer firing: the mode, the bead, and every
// piece of context the prompt is assembled from. Build the firing with
// [EngineerRequest.Firing] and read the reply with
// [EngineerRequest.ParseResult].
type EngineerRequest struct {
	// Mode selects the job; see the EngineerMode constants.
	Mode EngineerMode
	// RunID and AgentID identify the firing.
	RunID   domain.RunID
	AgentID domain.AgentID
	// Bead is the bead the engineer owns, its worktree and its branch. The
	// worktree is also the working directory of the subprocess, so it must
	// be an absolute path.
	Bead prompt.BeadContext
	// Repo describes the repository (prompt.ScanRepo).
	Repo prompt.RepoContext
	// Brief is the human's -m text, empty in autopilot.
	Brief string
	// Rules and Vibes are the configured prompt documents (prompt.LoadDocs).
	Rules []prompt.Doc
	Vibes []prompt.Doc
	// History is the feedback loop so far, oldest round first.
	History []prompt.Round
	// Items are the feedback items this firing must answer, consolidated
	// across the testers of the round. Required by
	// EngineerAddressFeedback and ignored by the other modes.
	Items []domain.FeedbackItem
	// Arbitrations are the product owner's binding rulings on items that
	// stayed disputed (D-08); shown with the items they decide.
	Arbitrations []domain.Arbitration
	// Conflict describes the failed rebase; required by
	// EngineerResolveConflict and ignored by the other modes.
	Conflict EngineerConflict
	// CheckCommand overrides the command the engineer must leave green;
	// empty means Repo.CheckCommand.
	CheckCommand string
	// Model, MaxTurns, Timeout, Env and InheritEnv are passed through to the
	// run spec; the zero values mean the cartridge's defaults.
	Model      string
	MaxTurns   int
	Timeout    time.Duration
	Env        map[string]string
	InheritEnv []string
	// Budget bounds the assembled prompt; the zero value means the
	// prompt package's defaults.
	Budget prompt.Budget
}

// EngineerFiring is a validated engineer firing: the prompt, the run spec
// the cartridge is given, and the contract its reply must satisfy. Before
// firing, check the cartridge can really enforce the spec's tool policy with
// [EnforcementFor] and [Enforcement.Check].
type EngineerFiring struct {
	// Mode and Task are what the engineer was fired to do.
	Mode EngineerMode
	Task events.Task
	// Prompt is the assembled prompt; write a debug copy with Prompt.Write.
	Prompt prompt.Prompt
	// Spec is the run spec, with Workdir set to the bead's worktree and
	// ToolPolicy set by ApplyPolicy (always read_write for an engineer).
	Spec cartridge.RunSpec
	// Contract is the output contract the reply must satisfy.
	Contract *structured.Contract
	// Message is the message type the reply carries.
	Message domain.MessageType
}

// Validate reports the first problem with the request: an unknown mode, a
// missing identity or bead, a worktree that is not an absolute path, or
// mode-specific context that is missing or malformed.
func (r EngineerRequest) Validate() error {
	if !r.Mode.Valid() {
		return fmt.Errorf("%w: engineer mode %q (want one of %s)", domain.ErrUnknownValue, string(r.Mode), joinEngineerModes())
	}
	if strings.TrimSpace(string(r.AgentID)) == "" {
		return errors.New("agents: engineer request has no agent id")
	}
	if strings.TrimSpace(string(r.Bead.Bead.ID)) == "" {
		return errors.New("agents: engineer request has no bead")
	}
	switch wt := r.Bead.Worktree; {
	case strings.TrimSpace(wt) == "":
		return fmt.Errorf("agents: engineer request for bead %s has no worktree", r.Bead.Bead.ID)
	case !filepath.IsAbs(wt):
		return fmt.Errorf("agents: engineer worktree %q is not an absolute path", wt)
	}
	switch r.Mode {
	case EngineerAddressFeedback:
		return r.validateItems()
	case EngineerResolveConflict:
		if r.Conflict.Empty() {
			return fmt.Errorf("agents: engineer request for bead %s is a %s with no conflict to resolve",
				r.Bead.Bead.ID, EngineerResolveConflict)
		}
	}
	return nil
}

// validateItems checks the feedback items an address-feedback firing must
// answer: there has to be at least one, each needs an id, and the ids have
// to be distinct so the responses can refer to them.
func (r EngineerRequest) validateItems() error {
	if len(r.Items) == 0 {
		return fmt.Errorf("agents: engineer request for bead %s is an %s with no feedback items",
			r.Bead.Bead.ID, EngineerAddressFeedback)
	}
	seen := make(map[string]bool, len(r.Items))
	for i, it := range r.Items {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			return fmt.Errorf("agents: feedback item %d (%q) has no id", i+1, it.Title)
		}
		if seen[id] {
			return fmt.Errorf("agents: feedback item %q appears twice", id)
		}
		seen[id] = true
	}
	return nil
}

// checkCommand is the command the engineer must leave green.
func (r EngineerRequest) checkCommand() string {
	if s := strings.TrimSpace(r.CheckCommand); s != "" {
		return s
	}
	return strings.TrimSpace(r.Repo.CheckCommand)
}

// PromptRequest builds the prompt.Request for the firing: the engineer role,
// the mode's task and contract, and the mode's instruction sections. Callers
// that only want to inspect or extend the prompt layers use this; everyone
// else uses Firing.
func (r EngineerRequest) PromptRequest() (prompt.Request, error) {
	if err := r.Validate(); err != nil {
		return prompt.Request{}, err
	}
	contract, err := r.Mode.Contract()
	if err != nil {
		return prompt.Request{}, err
	}
	policy, err := Policy(domain.RoleEngineer, r.Mode.Task())
	if err != nil {
		return prompt.Request{}, err
	}
	bead := r.Bead
	return prompt.Request{
		Role:         domain.RoleEngineer,
		Task:         r.Mode.Task(),
		AgentID:      r.AgentID,
		RunID:        r.RunID,
		ToolPolicy:   policy,
		Objective:    r.objective(),
		Brief:        r.Brief,
		Rules:        r.Rules,
		Vibes:        r.Vibes,
		Repo:         r.Repo,
		Bead:         &bead,
		History:      r.History,
		Instructions: r.instructions(),
		Contract:     contract,
		Budget:       r.Budget,
	}, nil
}

// Prompt assembles the prompt for the firing.
func (r EngineerRequest) Prompt() (prompt.Prompt, error) {
	pr, err := r.PromptRequest()
	if err != nil {
		return prompt.Prompt{}, err
	}
	return prompt.Build(pr)
}

// RunSpec builds the cartridge run spec for the firing; see Firing, which
// returns the prompt alongside it. An over-budget prompt comes back as a
// valid spec plus an [ErrPromptOverBudget] error, exactly as it does there.
func (r EngineerRequest) RunSpec() (cartridge.RunSpec, error) {
	f, err := r.Firing()
	return f.Spec, err
}

// Firing assembles everything needed to fire the engineer: the prompt, the
// run spec (workdir = the bead's worktree, tool policy set by ApplyPolicy)
// and the output contract. It fails on an invalid request or a run spec the
// cartridge layer would reject.
//
// A prompt that is still over its budget once the builder has degraded
// everything it may comes back complete, alongside an [ErrPromptOverBudget]
// error: the firing is usable and only the caller knows whether the tokens
// are worth spending.
func (r EngineerRequest) Firing() (EngineerFiring, error) {
	pr, err := r.PromptRequest()
	if err != nil {
		return EngineerFiring{}, err
	}
	p, err := prompt.Build(pr)
	if err != nil {
		return EngineerFiring{}, err
	}
	task := r.Mode.Task()
	agent := domain.Agent{ID: r.AgentID, Role: domain.RoleEngineer, Bead: r.Bead.Bead.ID}
	spec := cartridge.RunSpec{
		Prompt:     p.Text,
		Workdir:    r.Bead.Worktree,
		Model:      r.Model,
		MaxTurns:   r.MaxTurns,
		Timeout:    r.Timeout,
		Env:        cloneEngineerEnv(r.Env),
		InheritEnv: append([]string(nil), r.InheritEnv...),
		Labels:     cartridge.LabelsFor(r.RunID, agent, task, r.Bead.Round),
	}
	if err := ApplyPolicy(&spec, domain.RoleEngineer, task); err != nil {
		return EngineerFiring{}, err
	}
	if err := spec.Validate(); err != nil {
		return EngineerFiring{}, fmt.Errorf("agents: engineer %s: %w", r.AgentID, err)
	}
	f := EngineerFiring{
		Mode:     r.Mode,
		Task:     task,
		Prompt:   p,
		Spec:     spec,
		Contract: pr.Contract,
		Message:  r.Mode.MessageType(),
	}
	if p.OverBudget {
		return f, promptBudgetError(fmt.Sprintf("engineer %s", r.Mode), p)
	}
	return f, nil
}

// cloneEngineerEnv copies an environment map so the caller's map is never retained.
func cloneEngineerEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// EngineerResult is what an engineer firing produced: an impl_report for
// implement and resolve-conflict firings, or one response per feedback item
// for an address-feedback firing.
type EngineerResult struct {
	// Mode is the mode the result came from.
	Mode EngineerMode
	// Report is the implementation report (implement, resolve_conflict).
	Report domain.ImplReport
	// Responses are the answers to the feedback items (address_feedback),
	// in the order the engineer gave them.
	Responses []domain.Response
	// Parsed is the validated result block, carrying the raw JSON the bead
	// comment stores.
	Parsed structured.Parsed
}

// MessageType is the type of the message the result carries.
func (r EngineerResult) MessageType() domain.MessageType { return r.Mode.MessageType() }

// Payload returns the value the bead comment writer stores: a
// domain.ImplReport or a []domain.Response, matching MessageType.
func (r EngineerResult) Payload() any {
	if r.Mode.MessageType() == domain.MessageResponse {
		return r.Responses
	}
	return r.Report
}

// ParseEngineerOutput extracts and validates the result block of an engineer
// reply against the mode's schema. It checks the contract only; use
// [EngineerRequest.ParseResult] to also check the responses against the
// items that were asked about. Every contract-level failure is a
// *structured.ParseError, so a caller can feed ParseError.Feedback back to
// the model on a retry (P5-08).
func ParseEngineerOutput(mode EngineerMode, output string) (EngineerResult, error) {
	if !mode.Valid() {
		return EngineerResult{}, fmt.Errorf("%w: engineer mode %q (want one of %s)",
			domain.ErrUnknownValue, string(mode), joinEngineerModes())
	}
	res := EngineerResult{Mode: mode}
	if mode.MessageType() == domain.MessageResponse {
		responses, parsed, err := ParseResponses(output)
		res.Responses, res.Parsed = responses, parsed
		return res, err
	}
	report, parsed, err := ParseImplReport(output)
	res.Report, res.Parsed = report, parsed
	return res, err
}

// ParseResult reads an engineer reply back. On top of the schema it enforces
// what the schema cannot: an address-feedback reply must answer every
// feedback item it was given, exactly once, with no invented ids, and it may
// not push back on a rules violation (domain.CategoryRules items are always
// blockers, D-08/P9). Those failures are reported as a *structured.ParseError
// with structured.ReasonSchema, so the retry path treats them like any other
// contract failure.
func (r EngineerRequest) ParseResult(output string) (EngineerResult, error) {
	if err := r.Validate(); err != nil {
		return EngineerResult{}, err
	}
	res, err := ParseEngineerOutput(r.Mode, output)
	if err != nil {
		return res, err
	}
	if r.Mode != EngineerAddressFeedback {
		return res, nil
	}
	if err := ValidateEngineerResponses(r.Items, res.Responses); err != nil {
		return res, err
	}
	return res, nil
}

// ValidateEngineerResponses checks a set of responses against the feedback
// items they answer: every item answered exactly once, no unknown item ids,
// and no pushback or wontfix on a rules violation. The error is a
// *structured.ParseError listing one problem per violation (errors.Is ~
// structured.ErrSchema), or nil.
func ValidateEngineerResponses(items []domain.FeedbackItem, responses []domain.Response) error {
	byID := make(map[string]domain.FeedbackItem, len(items))
	for _, it := range items {
		byID[strings.TrimSpace(it.ID)] = it
	}
	answered := make(map[string]int, len(responses))
	var problems []string
	for i, resp := range responses {
		id := strings.TrimSpace(resp.ItemID)
		item, known := byID[id]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf(
				"at '/responses/%d/item_id': %q is not one of the feedback items you were given (%s)",
				i, resp.ItemID, joinEngineerItemIDs(items)))
			continue
		case answered[id] > 0:
			problems = append(problems, fmt.Sprintf(
				"at '/responses/%d/item_id': item %q is answered more than once", i, id))
		}
		answered[id]++
		if item.Category == domain.CategoryRules && resp.Action != domain.ActionFixed {
			problems = append(problems, fmt.Sprintf(
				"at '/responses/%d/action': item %q is a %s violation, which is always a blocker: it must be %q, not %q",
				i, id, domain.CategoryRules, domain.ActionFixed, resp.Action))
		}
	}
	for _, it := range items {
		if id := strings.TrimSpace(it.ID); answered[id] == 0 {
			problems = append(problems, fmt.Sprintf(
				"at '/responses': feedback item %q has no response; answer every item, even the ones you disagree with", id))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return &structured.ParseError{
		Reason:   structured.ReasonSchema,
		Contract: domain.MessageResponse.String(),
		Detail:   "the responses do not match the feedback items that were raised",
		Problems: problems,
	}
}

// joinEngineerItemIDs lists the item ids for an error message.
func joinEngineerItemIDs(items []domain.FeedbackItem) string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if id := strings.TrimSpace(it.ID); id != "" {
			ids = append(ids, strconv.Quote(id))
		}
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// objective is the one-or-two sentence statement of what this firing must
// achieve, rendered at the top of the assignment block.
func (r EngineerRequest) objective() string {
	id := r.Bead.Bead.ID
	switch r.Mode {
	case EngineerAddressFeedback:
		return fmt.Sprintf(
			"Answer every piece of feedback the testers raised on bead %s%s: fix what is right, "+
				"and push back with a concrete reason where you believe they are wrong.", id, r.roundSuffix())
	case EngineerResolveConflict:
		return fmt.Sprintf(
			"Bead %s could not land: rebasing it%s stopped on conflicts. Resolve them so the work applies "+
				"cleanly, without losing either side's intent.", id, r.targetSuffix())
	default:
		return fmt.Sprintf(
			"Implement bead %s in your worktree, verify it, commit it, and report what you did so three "+
				"testers can review it.", id)
	}
}

// roundSuffix names the review round when the caller set one.
func (r EngineerRequest) roundSuffix() string {
	if r.Bead.Round <= 0 {
		return ""
	}
	return " in round " + strconv.Itoa(r.Bead.Round)
}

// targetSuffix names the branch the rebase was onto when the caller set one.
func (r EngineerRequest) targetSuffix() string {
	if t := strings.TrimSpace(r.Conflict.Target); t != "" {
		return " onto " + t
	}
	return " onto the default branch"
}

// instructions are the mode-specific sections of the prompt, rendered after
// the context and before the output contract.
func (r EngineerRequest) instructions() []prompt.Section {
	switch r.Mode {
	case EngineerAddressFeedback:
		return r.addressFeedbackSections()
	case EngineerResolveConflict:
		return r.resolveConflictSections()
	default:
		return r.implementSections()
	}
}

// implementSections are the instructions for a first implementation pass.
func (r EngineerRequest) implementSections() []prompt.Section {
	var b strings.Builder
	b.WriteString("1. Re-read the bead and its acceptance criteria above. They are the definition of done.\n")
	b.WriteString("2. Read the code you are about to change before you change it, and follow the conventions already there.\n")
	b.WriteString("3. Make the smallest change that satisfies every acceptance criterion, tests included.\n")
	fmt.Fprintf(&b, "4. %s\n", r.checkStep())
	fmt.Fprintf(&b, "5. Commit your work in this worktree, for example `git commit -am %q`. "+
		"Anything you leave uncommitted is committed for you and reviewed anyway.\n", r.commitMessage())
	b.WriteString("6. Finish with the result block described below. Do not ask for confirmation first; nobody is watching to answer.\n")

	return []prompt.Section{
		{Title: "How to implement this bead", Body: b.String()},
		{Title: "Out of bounds", Body: r.outOfBounds()},
		{Title: "What to report", Body: implementReportBody},
	}
}

// addressFeedbackSections are the instructions for a feedback round.
func (r EngineerRequest) addressFeedbackSections() []prompt.Section {
	sections := []prompt.Section{{
		Title: "Feedback to answer",
		Body: "These are the items the testers raised. Answer every one of them, by id, in the result block.\n\n" +
			renderEngineerItems(r.Items),
	}}
	if body := renderEngineerArbitrations(r.Arbitrations); body != "" {
		sections = append(sections, prompt.Section{
			Title: "Binding rulings from the product owner",
			Body: "The product owner has already ruled on these items. The ruling is binding: " +
				"do what it says and record the item as fixed.\n\n" + body,
		})
	}

	var b strings.Builder
	b.WriteString("For each item, choose exactly one action:\n\n")
	b.WriteString("- `fixed` — you changed the code. Say briefly what you changed; the tester who raised it re-reviews.\n")
	b.WriteString("- `pushback` — you disagree. Give the concrete reason: what the tester missed, or why the change would be wrong. " +
		"Pushing back is expected when you are right, and the tester will reconsider.\n")
	b.WriteString("- `wontfix` — the item is real but out of scope for this bead. Say why it belongs elsewhere.\n\n")
	b.WriteString("Rules for this round:\n\n")
	b.WriteString("- One response per item, using the item id exactly as it is written above. Never invent an id, " +
		"never answer the same item twice, and never leave one out.\n")
	fmt.Fprintf(&b, "- An item in the `%s` category is a violation of a rules file. It is always a blocker: "+
		"fix it, do not push back on it.\n", domain.CategoryRules)
	b.WriteString("- Do not use this round to make unrelated changes. Fix what was raised, nothing else.\n")
	fmt.Fprintf(&b, "- %s\n", r.checkStep())
	fmt.Fprintf(&b, "- Commit before you finish, for example `git commit -am %q`.\n", r.commitMessage())

	sections = append(sections,
		prompt.Section{Title: "How to answer", Body: b.String()},
		prompt.Section{Title: "Out of bounds", Body: r.outOfBounds()},
		prompt.Section{Title: "What to report", Body: responseReportBody},
	)
	return sections
}

// resolveConflictSections are the instructions for repairing a failed
// rebase (D-10).
func (r EngineerRequest) resolveConflictSections() []prompt.Section {
	sections := []prompt.Section{{Title: "The conflict", Body: renderEngineerConflict(r.Conflict)}}

	var b strings.Builder
	b.WriteString("1. Look at both sides of every conflict before you resolve it. The other side is somebody else's " +
		"finished, reviewed work: keep its intent as well as yours.\n")
	b.WriteString("2. Resolve the markers by hand. Never take one side wholesale just to make the conflict disappear, " +
		"and never revert a change that is not yours.\n")
	b.WriteString("3. Stage each resolved file with `git add <file>`.\n")
	fmt.Fprintf(&b, "4. %s\n", r.checkStep())
	fmt.Fprintf(&b, "5. Continue the rebase with `%s`. Do not start a new branch, do not `git rebase --abort`, "+
		"and never force-push anything.\n", r.Conflict.continueCommand())
	b.WriteString("6. If the two changes genuinely cannot be reconciled, stop, leave the tree as it is, and say so " +
		"in your report instead of guessing.\n")

	sections = append(sections,
		prompt.Section{Title: "How to resolve it", Body: b.String()},
		prompt.Section{Title: "Out of bounds", Body: r.outOfBounds()},
		prompt.Section{Title: "What to report", Body: conflictReportBody},
	)
	return sections
}

// checkStep is the sentence telling the engineer how to verify the work,
// which depends on whether a check command is known.
func (r EngineerRequest) checkStep() string {
	if cmd := r.checkCommand(); cmd != "" {
		return fmt.Sprintf("Run the check command `%s` and leave it green. A red check is a failed round, "+
			"so fix the failures rather than reporting them as someone else's problem.", cmd)
	}
	return "No check command is configured for this repository, so verify the work the way the repository itself does " +
		"(its test target, its linter) and say in your report exactly what you ran and what it printed."
}

// commitMessage is the commit message the prompt suggests; it matches the
// convention the orchestrator uses for leftovers (D-14).
func (r EngineerRequest) commitMessage() string {
	title := oneEngineerLine(r.Bead.Bead.Title)
	if title == "" {
		title = "work on " + string(r.Bead.Bead.ID)
	}
	return fmt.Sprintf("sc(%s): %s", r.Bead.Bead.ID, title)
}

// outOfBounds lists what the engineer must not do, in the terms of this
// firing.
func (r EngineerRequest) outOfBounds() string {
	var b strings.Builder
	b.WriteString("- Do not change anything the bead did not ask for. Scope creep is a review failure, not a bonus.\n")
	b.WriteString("- Do not disable, delete or skip a test, or weaken an assertion, to make the suite pass.\n")
	fmt.Fprintf(&b, "- Stay inside your worktree (`%s`). Another agent's worktree, the default branch and anything "+
		"outside the repository are not yours to touch.\n", r.Bead.Worktree)
	b.WriteString("- Do not push, open a pull request, merge, or close the bead: the orchestrator delivers the work.\n")
	b.WriteString("- Do not edit the beads database. Your `bd` is read-only.\n")
	return b.String()
}

// implementReportBody, responseReportBody and conflictReportBody describe
// the result block each mode owes, in the engineer's terms; the schema
// itself is appended after them as the output contract.
const implementReportBody = "End your turn with the result block: a summary of what you did and why, the files you " +
	"changed, how a tester verifies it (the exact commands), the risks you are aware of, any questions for the " +
	"testers or the product owner, and the sha of your commit. Write it for somebody who has not read your diff.\n\n" +
	"If you could not meet an acceptance criterion, say so in the summary and explain why. A truthful report of a " +
	"partial change is worth more than a confident report of one that does not work."

const responseReportBody = "End your turn with the result block: one entry per feedback item, each with the item id, " +
	"your action (`fixed`, `pushback` or `wontfix`) and your rationale. Keep each rationale to a couple of " +
	"sentences that a tester can act on."

const conflictReportBody = "End your turn with the result block: summarise how you resolved each conflict and what " +
	"you kept from each side, list the files you touched, say how the result was verified, and flag any resolution " +
	"you are unsure about as a risk. If you stopped without finishing the rebase, say that in the summary."

// renderEngineerItems renders the feedback items an address-feedback firing
// must answer.
func renderEngineerItems(items []domain.FeedbackItem) string {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- `%s` [%s", it.ID, it.Severity)
		if it.Category != "" {
			b.WriteString("/" + it.Category)
		}
		b.WriteString("] " + oneEngineerLine(it.Title))
		if it.File != "" {
			b.WriteString(" (" + it.File + engineerLocation(it.Line) + ")")
		}
		b.WriteString("\n")
		if d := oneEngineerLine(it.Detail); d != "" {
			b.WriteString("  " + d + "\n")
		}
		if s := oneEngineerLine(it.Suggestion); s != "" {
			b.WriteString("  Suggestion: " + s + "\n")
		}
		if it.Status != "" {
			b.WriteString("  Status so far: " + it.Status.String() + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderEngineerArbitrations renders the product owner's binding rulings.
func renderEngineerArbitrations(as []domain.Arbitration) string {
	if len(as) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range as {
		fmt.Fprintf(&b, "- `%s` — %s", a.ItemID, a.Ruling)
		if s := oneEngineerLine(a.Instruction); s != "" {
			b.WriteString(": " + s)
		}
		b.WriteString("\n")
		if s := oneEngineerLine(a.Rationale); s != "" {
			b.WriteString("  Reason: " + s + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderEngineerConflict renders what git reported about the failed rebase.
func renderEngineerConflict(c EngineerConflict) string {
	var b strings.Builder
	if t := strings.TrimSpace(c.Target); t != "" {
		fmt.Fprintf(&b, "- Rebasing onto: %s\n", t)
	}
	for _, f := range c.Files {
		if s := strings.TrimSpace(f); s != "" {
			fmt.Fprintf(&b, "- Conflicted file: %s\n", s)
		}
	}
	fmt.Fprintf(&b, "- Resume the rebase with: `%s`\n", c.continueCommand())
	if out := strings.TrimRight(strings.TrimSpace(c.Output), "\n"); out != "" {
		fmt.Fprintf(&b, "\nWhat git reported:\n\n%s", prompt.Fenced("", out))
	}
	return strings.TrimRight(b.String(), "\n")
}

// oneEngineerLine collapses a value onto a single line so list items stay
// readable.
func oneEngineerLine(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// engineerLocation renders ":<line>" for a positive line number.
func engineerLocation(line int) string {
	if line <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(line)
}
