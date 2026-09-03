package agents

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the product owner's two judicial modes (P5-05), the ones the
// feedback loop falls back on when the agents cannot settle something
// between themselves (D-08):
//
//   - arbitrate: one feedback item is deadlocked — the engineer pushed back,
//     the tester held, and the pushback limit is spent. The PO rules
//     uphold_tester, uphold_engineer or compromise, and the ruling is
//     binding.
//   - escalate: a whole bead used up max_review_rounds without approval. The
//     PO ships it with notes, narrows its acceptance criteria, or blocks it
//     for a human.
//
// Both modes are read-only firings of the product owner (P5-06) that must
// answer with exactly one structured message: `arbitration` or `escalation`
// (D-25). This file builds their prompts, parses their replies, and applies
// the two rules the JSON Schemas cannot express: a rules-file violation is
// never decided in the engineer's favour, and it is never shipped with
// notes.

// ErrIncompleteRuling is returned by [ArbitrationRequest.Prompt] and
// [EscalationRequest.Prompt] when the request is missing something the
// product owner cannot rule without, such as the id of the disputed item.
var ErrIncompleteRuling = errors.New("agents: incomplete ruling request")

// ItemDispute is one feedback item that outlived the engineer/tester
// exchange: what the tester raised, what the engineer answered, and why the
// tester was not satisfied. It is the unit both ruling modes work on — one
// dispute for an arbitration, every unresolved dispute for an escalation.
type ItemDispute struct {
	// Item is the feedback item as the tester last recorded it.
	Item domain.FeedbackItem
	// Lens is the tester who raised it; empty when the caller does not know.
	Lens domain.Lens
	// Round is the review round the item was raised in, 1-based; 0 when
	// unknown.
	Round int
	// Response is the engineer's answer to the item (pushback or wontfix for
	// a real dispute). The zero value means the engineer has not answered.
	Response domain.Response
	// HoldReason is why the tester rejected that answer (P5-04's evaluate
	// pushback mode). Empty when the tester has not held it.
	HoldReason string
	// PushbackRounds is how many times the item has been pushed back and
	// held so far; 0 when unknown.
	PushbackRounds int
	// Rulings are earlier product owner rulings on this same item, oldest
	// first. Normally empty: one arbitration settles an item.
	Rulings []domain.Arbitration
}

// answered reports whether the engineer said anything about the item.
func (d ItemDispute) answered() bool {
	return d.Response.Action != "" || strings.TrimSpace(d.Response.Rationale) != ""
}

// rulesViolation reports whether the item is a rules-file violation, which
// no ruling may wave away (domain.CategoryRules, D-08, P5-04).
func (d ItemDispute) rulesViolation() bool {
	return strings.EqualFold(strings.TrimSpace(d.Item.Category), domain.CategoryRules)
}

// RulingContext is the run, repository and bead context both ruling modes
// need. It mirrors the corresponding fields of prompt.Request; fill in what
// the orchestrator has and leave the rest zero, and the prompt builder omits
// the layers it cannot render.
type RulingContext struct {
	// RunID identifies the run.
	RunID domain.RunID
	// AgentID identifies this firing of the product owner; it also names the
	// debug copy of the prompt (prompt.Prompt.Write).
	AgentID domain.AgentID
	// Brief is the human's -m text, when the run has one.
	Brief string
	// Rules are the configured rules files (prompt.LoadDocs); they are what
	// makes a `rules` item a blocker, so pass them.
	Rules []prompt.Doc
	// Vibes are the configured vibes files.
	Vibes []prompt.Doc
	// Repo describes the repository (prompt.ScanRepo).
	Repo prompt.RepoContext
	// Bead is the bead under dispute. A ruling is always about a bead, so
	// callers should set it; an empty one only makes the prompt vaguer.
	Bead *prompt.BeadContext
	// History is the feedback loop so far, oldest round first.
	History []prompt.Round
	// Budget bounds the prompt; the zero value means the defaults.
	Budget prompt.Budget
}

// request turns the shared context into the common part of a prompt.Request.
func (c RulingContext) request(task events.Task, contract *structured.Contract) (prompt.Request, error) {
	policy, err := Policy(domain.RolePO, task)
	if err != nil {
		return prompt.Request{}, err
	}
	return prompt.Request{
		Role:       domain.RolePO,
		Task:       task,
		AgentID:    c.AgentID,
		RunID:      c.RunID,
		ToolPolicy: policy,
		Brief:      c.Brief,
		Rules:      c.Rules,
		Vibes:      c.Vibes,
		Repo:       c.Repo,
		Bead:       c.Bead,
		History:    c.History,
		Contract:   contract,
		Budget:     c.Budget,
	}, nil
}

// ArbitrationRequest fires the product owner to settle one disputed feedback
// item. The engineer pushed back, the tester held, and after
// max_pushback_rounds the ruling is the PO's (D-08).
type ArbitrationRequest struct {
	RulingContext
	// Dispute is the item to rule on. Dispute.Item.ID must be set: the
	// ruling names it.
	Dispute ItemDispute
	// MaxPushbackRounds is the configured limit that sent the item here
	// (config limits.max_pushback_rounds, default 2); 0 omits it from the
	// prompt.
	MaxPushbackRounds int
}

// PromptRequest assembles the prompt.Request for the firing, so a caller
// that wants to add a section or change the budget can do so before
// building. Most callers want [ArbitrationRequest.Prompt].
func (r ArbitrationRequest) PromptRequest() (prompt.Request, error) {
	if strings.TrimSpace(r.Dispute.Item.ID) == "" {
		return prompt.Request{}, fmt.Errorf("%w: an arbitration needs the id of the disputed item", ErrIncompleteRuling)
	}
	contract, err := ContractFor(domain.MessageArbitration)
	if err != nil {
		return prompt.Request{}, err
	}
	req, err := r.request(events.TaskArbitrate, contract)
	if err != nil {
		return prompt.Request{}, err
	}
	req.Objective = "One feedback item on this bead is deadlocked: the engineer pushed back and the tester " +
		"held. Rule on it. Your ruling is binding and ends the dispute."
	req.Instructions = []prompt.Section{
		{Title: "The disputed item", Body: arbRenderItem(r.Dispute)},
		{Title: "The engineer's position", Body: arbRenderEngineer(r.Dispute)},
		{Title: "The tester's position", Body: arbRenderTester(r.Dispute, r.MaxPushbackRounds)},
		{Title: "How to rule", Body: arbRenderGuidance(r.Dispute)},
	}
	return req, nil
}

// Prompt builds the arbitration prompt. It fails when the request is
// incomplete (ErrIncompleteRuling) or the embedded schemas do not compile.
func (r ArbitrationRequest) Prompt() (prompt.Prompt, error) {
	req, err := r.PromptRequest()
	if err != nil {
		return prompt.Prompt{}, err
	}
	return prompt.Build(req)
}

// Parse reads the product owner's reply, validates it against the
// `arbitration` schema and then against the dispute it was fired for: the
// ruling must name the same item, a compromise must carry its instruction,
// and a rules-file violation may not be ruled in the engineer's favour.
// Every failure is a *structured.ParseError, so P5-08 can feed
// ParseError.Feedback back to the model on the retry.
func (r ArbitrationRequest) Parse(output string) (domain.Arbitration, structured.Parsed, error) {
	a, parsed, err := ParseArbitration(output)
	if err != nil {
		return a, parsed, err
	}
	if err := ValidateArbitration(a, r.Dispute); err != nil {
		return a, parsed, err
	}
	return a, parsed, nil
}

// ValidateArbitration checks a ruling against the dispute it settles. It
// enforces what the JSON Schema cannot: that the ruling names the item it
// was asked about, and that a violation of a rules file is never decided in
// the engineer's favour (domain.CategoryRules is a hard constraint, D-08).
// The error is a *structured.ParseError matching structured.ErrSchema.
func ValidateArbitration(a domain.Arbitration, d ItemDispute) error {
	var problems []string
	switch {
	case strings.TrimSpace(a.ItemID) == "":
		problems = append(problems, "at '/item_id': the ruling must name the item it settles")
	case d.Item.ID != "" && a.ItemID != d.Item.ID:
		problems = append(problems, fmt.Sprintf("at '/item_id': ruled on %q but the disputed item is %q", a.ItemID, d.Item.ID))
	}
	if !a.Ruling.Valid() {
		problems = append(problems, fmt.Sprintf("at '/ruling': %q is not one of %s", string(a.Ruling), arbJoinRulings()))
	}
	if a.Ruling == domain.RulingCompromise && strings.TrimSpace(a.Instruction) == "" {
		problems = append(problems, "at '/instruction': a compromise needs the instruction the engineer follows instead")
	}
	if a.Ruling == domain.RulingUpholdEngineer && d.rulesViolation() {
		problems = append(problems, fmt.Sprintf(
			"at '/ruling': item %s is a %q violation, which is a hard constraint: uphold the tester or impose a compromise that still satisfies the rule",
			d.Item.ID, domain.CategoryRules))
	}
	if len(problems) == 0 {
		return nil
	}
	return &structured.ParseError{
		Reason:   structured.ReasonSchema,
		Contract: domain.MessageArbitration.String(),
		Detail:   "the ruling does not settle the item it was asked about",
		Problems: problems,
	}
}

// ArbitrationItemStatus is the status the disputed item takes once the
// ruling lands: overruled when the engineer prevails, open when the engineer
// still has work to do (P6-04 records it on the item). An unknown ruling is
// an error wrapping domain.ErrUnknownValue.
func ArbitrationItemStatus(ruling domain.ArbitrationRuling) (domain.ItemStatus, error) {
	switch ruling {
	case domain.RulingUpholdTester, domain.RulingCompromise:
		return domain.ItemOpen, nil
	case domain.RulingUpholdEngineer:
		return domain.ItemOverruled, nil
	default:
		return "", fmt.Errorf("%w: arbitration ruling %q (want one of %s)", domain.ErrUnknownValue, string(ruling), arbJoinRulings())
	}
}

// ArbitrationDirective is the one line the feedback loop shows the engineer
// and the tester next round: what the ruling means for the item, in the
// imperative. It is empty for an unknown ruling.
func ArbitrationDirective(a domain.Arbitration, d ItemDispute) string {
	title := arbOneLine(d.Item.Title)
	ref := "`" + a.ItemID + "`"
	if title != "" {
		ref += " (" + title + ")"
	}
	switch a.Ruling {
	case domain.RulingUpholdTester:
		s := "The product owner upheld the tester on " + ref + ": address it as raised."
		return s + arbSentence(a.Instruction) + arbSentence(a.Rationale)
	case domain.RulingUpholdEngineer:
		s := "The product owner overruled " + ref + ": it no longer blocks and needs no further work."
		return s + arbSentence(a.Rationale)
	case domain.RulingCompromise:
		s := "The product owner imposed a compromise on " + ref + ": " + arbOneLine(a.Instruction)
		if !strings.HasSuffix(s, ".") {
			s += "."
		}
		return s + arbSentence(a.Rationale)
	default:
		return ""
	}
}

// EscalationRequest fires the product owner on a bead that used up its
// review rounds without approval (D-08). The PO decides whether the work
// ships with notes, whether the bead's acceptance criteria were too
// ambitious, or whether a human has to take over.
type EscalationRequest struct {
	RulingContext
	// Round is the review round that just ended, 1-based. Required: the
	// ruling exists because this round was the last one.
	Round int
	// MaxRounds is the configured cap that was reached
	// (config limits.max_review_rounds, default 5); 0 omits it.
	MaxRounds int
	// Unresolved are the items still blocking approval, most serious first
	// if the caller has an order in mind.
	Unresolved []ItemDispute
	// Notes are extra lines the orchestrator wants the product owner to see,
	// for example "the check command has failed in every round".
	Notes []string
}

// PromptRequest assembles the prompt.Request for the firing; see
// [ArbitrationRequest.PromptRequest].
func (r EscalationRequest) PromptRequest() (prompt.Request, error) {
	if r.Round <= 0 {
		return prompt.Request{}, fmt.Errorf("%w: an escalation needs the review round that ran out (got %d)", ErrIncompleteRuling, r.Round)
	}
	contract, err := ContractFor(domain.MessageEscalation)
	if err != nil {
		return prompt.Request{}, err
	}
	req, err := r.request(events.TaskEscalate, contract)
	if err != nil {
		return prompt.Request{}, err
	}
	req.Objective = "This bead has run out of review rounds without every tester approving. Decide what " +
		"happens to it: ship it with notes, narrow what it has to achieve, or hand it to a human."
	req.Instructions = []prompt.Section{
		{Title: "Why this reached you", Body: escRenderReason(r)},
		{Title: "What is still unresolved", Body: escRenderUnresolved(r.Unresolved)},
		{Title: "How to rule", Body: escRenderGuidance(r.Unresolved)},
	}
	return req, nil
}

// Prompt builds the escalation prompt. It fails when the request is
// incomplete (ErrIncompleteRuling) or the embedded schemas do not compile.
func (r EscalationRequest) Prompt() (prompt.Prompt, error) {
	req, err := r.PromptRequest()
	if err != nil {
		return prompt.Prompt{}, err
	}
	return prompt.Build(req)
}

// Parse reads the product owner's reply, validates it against the
// `escalation` schema and then against the unresolved items: an outstanding
// rules-file violation cannot be shipped with notes. Every failure is a
// *structured.ParseError (P5-08).
func (r EscalationRequest) Parse(output string) (domain.Escalation, structured.Parsed, error) {
	e, parsed, err := ParseEscalation(output)
	if err != nil {
		return e, parsed, err
	}
	if err := ValidateEscalation(e, r.Unresolved); err != nil {
		return e, parsed, err
	}
	return e, parsed, nil
}

// ValidateEscalation checks a ruling against the items that were still
// blocking. It enforces the outcome's required field and the one rule the
// JSON Schema cannot express: work that still violates a rules file is never
// approved with notes — narrow the scope or block it for a human. The error
// is a *structured.ParseError matching structured.ErrSchema.
func ValidateEscalation(e domain.Escalation, unresolved []ItemDispute) error {
	var problems []string
	if !e.Outcome.Valid() {
		problems = append(problems, fmt.Sprintf("at '/outcome': %q is not one of %s", string(e.Outcome), arbJoinOutcomes()))
	}
	switch e.Outcome {
	case domain.EscalationApproveWithNotes:
		if strings.TrimSpace(e.Notes) == "" {
			problems = append(problems, "at '/notes': approve_with_notes must record what is left unaddressed")
		}
		if ids := arbRulesViolations(unresolved); len(ids) > 0 {
			problems = append(problems, fmt.Sprintf(
				"at '/outcome': %s still violate%s a rules file, which is a hard constraint: narrow the scope or block the bead instead of approving it",
				arbJoinIDs(ids), arbPlural(len(ids))))
		}
	case domain.EscalationNarrowScope:
		if strings.TrimSpace(e.NewAcceptance) == "" {
			problems = append(problems, "at '/new_acceptance': narrow_scope must state the acceptance criteria that replace the bead's")
		}
	case domain.EscalationBlock:
		if strings.TrimSpace(e.Summary) == "" {
			problems = append(problems, "at '/summary': block must summarise the situation for the human taking over")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &structured.ParseError{
		Reason:   structured.ReasonSchema,
		Contract: domain.MessageEscalation.String(),
		Detail:   "the escalation ruling is not actionable",
		Problems: problems,
	}
}

// EscalationBeadState is the state the bead moves to once the ruling lands
// (D-07): approved when the work ships with notes, in_review when the
// narrowed acceptance criteria are reviewed again, needs_human when the PO
// blocks it. An unknown outcome is an error wrapping domain.ErrUnknownValue.
func EscalationBeadState(o domain.EscalationOutcome) (domain.BeadState, error) {
	switch o {
	case domain.EscalationApproveWithNotes:
		return domain.BeadStateApproved, nil
	case domain.EscalationNarrowScope:
		return domain.BeadStateInReview, nil
	case domain.EscalationBlock:
		return domain.BeadStateNeedsHuman, nil
	default:
		return "", fmt.Errorf("%w: escalation outcome %q (want one of %s)", domain.ErrUnknownValue, string(o), arbJoinOutcomes())
	}
}

// renderDispute renders the item under dispute for the prompt.
func arbRenderItem(d ItemDispute) string {
	var b strings.Builder
	item := d.Item
	fmt.Fprintf(&b, "- Item id: `%s` (your ruling must use this id exactly)\n", item.ID)
	if item.Severity != "" {
		fmt.Fprintf(&b, "- Severity: %s\n", item.Severity)
	}
	if c := strings.TrimSpace(item.Category); c != "" {
		line := "- Category: " + c
		if d.rulesViolation() {
			line += " — this is a violation of a rules file, a hard constraint for this repository"
		}
		b.WriteString(line + "\n")
	}
	if t := arbOneLine(item.Title); t != "" {
		fmt.Fprintf(&b, "- Title: %s\n", t)
	}
	if item.File != "" {
		fmt.Fprintf(&b, "- Where: %s%s\n", item.File, arbLineRef(item.Line))
	}
	if l := d.Lens; l != "" {
		lens := l.String() + " tester"
		if d.Round > 0 {
			lens += ", round " + strconv.Itoa(d.Round)
		}
		fmt.Fprintf(&b, "- Raised by: %s\n", lens)
	} else if d.Round > 0 {
		fmt.Fprintf(&b, "- Raised in round: %d\n", d.Round)
	}
	if item.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", item.Status)
	}
	if detail := strings.TrimSpace(item.Detail); detail != "" {
		b.WriteString("\nWhat the tester says is wrong:\n\n" + detail + "\n")
	}
	if s := strings.TrimSpace(item.Suggestion); s != "" {
		b.WriteString("\nThe tester's suggested fix:\n\n" + s + "\n")
	}
	for _, a := range d.Rulings {
		fmt.Fprintf(&b, "\nEarlier ruling on this item: %s%s\n", a.Ruling, arbSentence(a.Instruction))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderEngineerPosition renders the engineer's answer to the item.
func arbRenderEngineer(d ItemDispute) string {
	if !d.answered() {
		return "The engineer has not answered this item in writing. Rule on the item as the tester raised it."
	}
	var b strings.Builder
	if d.Response.Action != "" {
		fmt.Fprintf(&b, "The engineer answered `%s`.\n", d.Response.Action)
	}
	if r := strings.TrimSpace(d.Response.Rationale); r != "" {
		b.WriteString("\nTheir rationale, verbatim:\n\n" + r + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTesterPosition renders why the tester refused the engineer's answer.
func arbRenderTester(d ItemDispute, maxPushback int) string {
	var b strings.Builder
	if r := strings.TrimSpace(d.HoldReason); r != "" {
		b.WriteString("The tester rejected that answer. Their reason, verbatim:\n\n" + r + "\n")
	} else {
		b.WriteString("The tester rejected that answer without giving a further reason; their item above is their position.\n")
	}
	switch {
	case d.PushbackRounds > 0 && maxPushback > 0:
		fmt.Fprintf(&b, "\nThe item has been pushed back and held %d time(s); the limit is %d, which is why it reached you.\n",
			d.PushbackRounds, maxPushback)
	case d.PushbackRounds > 0:
		fmt.Fprintf(&b, "\nThe item has been pushed back and held %d time(s), so it is now yours to settle.\n", d.PushbackRounds)
	case maxPushback > 0:
		fmt.Fprintf(&b, "\nThe pushback limit for one item is %d rounds, and it is spent, so the item is now yours to settle.\n", maxPushback)
	}
	return strings.TrimRight(b.String(), "\n")
}

// arbitrationGuidance renders the rules of the arbitrate mode.
func arbRenderGuidance(d ItemDispute) string {
	var b strings.Builder
	b.WriteString("Choose exactly one ruling:\n\n")
	b.WriteString("- `uphold_tester` — the item stands and the engineer must address it as raised.\n")
	b.WriteString("- `uphold_engineer` — the item is overruled: it stops blocking and nobody works on it again.\n")
	b.WriteString("- `compromise` — neither position wins; you give one `instruction` and the engineer follows it instead.\n")
	b.WriteString("\nHow to decide:\n\n")
	b.WriteString("- Judge the item on the evidence: read the code, the diff and the bead's acceptance criteria before you rule. You are read-only, so read as much as you need.\n")
	b.WriteString("- Weigh the cost of the change against what it buys the product. A cheap fix that removes a real risk goes to the tester; a rewrite demanded over a preference does not.\n")
	b.WriteString("- Rule only on this item. Do not raise new problems, do not reopen settled items, and do not ask questions: nobody will answer you.\n")
	if d.rulesViolation() {
		fmt.Fprintf(&b, "- This item is a `%s` violation. A rules file is a hard constraint, so `uphold_engineer` is not available to you: uphold the tester, or impose a compromise that still satisfies the rule.\n",
			domain.CategoryRules)
	} else {
		fmt.Fprintf(&b, "- A `%s` item is a violation of a rules file and can never be ruled in the engineer's favour; any other category is yours to weigh.\n",
			domain.CategoryRules)
	}
	b.WriteString("- Write one sentence of `rationale` that the losing side can act on. Your ruling is binding and the loop moves on immediately.\n")
	fmt.Fprintf(&b, "\nAnswer with exactly one `arbitration` result for item `%s` and nothing else.", d.Item.ID)
	return b.String()
}

// renderEscalationReason explains why the escalation firing happened.
func escRenderReason(r EscalationRequest) string {
	var b strings.Builder
	switch {
	case r.MaxRounds > 0:
		fmt.Fprintf(&b, "Review round %d of %d ended without every tester approving, so the bead is escalated to you.\n", r.Round, r.MaxRounds)
	default:
		fmt.Fprintf(&b, "Review round %d ended without every tester approving, so the bead is escalated to you.\n", r.Round)
	}
	b.WriteString("\nNo further review rounds will be spent on it: whatever you decide is what happens.\n")
	for _, n := range r.Notes {
		if s := strings.TrimSpace(n); s != "" {
			b.WriteString("\nNote: " + s + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderUnresolved renders the items still blocking approval.
func escRenderUnresolved(items []ItemDispute) string {
	if len(items) == 0 {
		return "The testers recorded no unresolved items; the round history above is the whole story. " +
			"Read it before you rule."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d item(s) still block approval:\n", len(items))
	for _, d := range items {
		item := d.Item
		fmt.Fprintf(&b, "\n- `%s` [%s", item.ID, item.Severity)
		if c := strings.TrimSpace(item.Category); c != "" {
			b.WriteString("/" + c)
		}
		b.WriteString("] " + arbOneLine(item.Title))
		if item.File != "" {
			b.WriteString(" (" + item.File + arbLineRef(item.Line) + ")")
		}
		if d.Lens != "" {
			b.WriteString(" — raised by the " + d.Lens.String() + " tester")
		}
		if d.rulesViolation() {
			b.WriteString("\n  This is a rules-file violation: it cannot be shipped with notes.")
		}
		if detail := arbOneLine(item.Detail); detail != "" {
			b.WriteString("\n  " + detail)
		}
		if d.answered() {
			b.WriteString("\n  Engineer: " + string(d.Response.Action) + arbColon(d.Response.Rationale))
		}
		if h := arbOneLine(d.HoldReason); h != "" {
			b.WriteString("\n  Tester held: " + h)
		}
		for _, a := range d.Rulings {
			b.WriteString("\n  Your earlier ruling: " + a.Ruling.String() + arbColon(a.Instruction))
		}
	}
	return b.String()
}

// escalationGuidance renders the rules of the escalate mode.
func escRenderGuidance(unresolved []ItemDispute) string {
	var b strings.Builder
	b.WriteString("Choose exactly one outcome and fill the field that goes with it:\n\n")
	b.WriteString("- `approve_with_notes` + `notes` — the work is good enough to land and what is left is cosmetic, arguable or better as a follow-up. Say in the notes exactly what is unaddressed.\n")
	b.WriteString("- `narrow_scope` + `new_acceptance` — the bead asked for too much. Write the acceptance criteria the work must meet instead; they replace the bead's and the testers review against them.\n")
	b.WriteString("- `block` + `summary` — the decision is a human's to make, or the loop is going in circles. Say what was tried, where it stalled and what you recommend.\n")
	b.WriteString("\nHow to decide:\n\n")
	b.WriteString("- Read the diff and the round history before you rule; you are read-only and have as much time as you need.\n")
	b.WriteString("- Prefer landing working code over perfect code. Rounds of disagreement this deep usually mean the bead was too big, not that the engineer is wrong.\n")
	b.WriteString("- Narrow the scope only to something the work can actually satisfy; anything you cut should be obvious enough that a new bead can pick it up.\n")
	b.WriteString("- Block only when no agent can settle it: a product or API decision, a missing credential, or a genuine deadlock.\n")
	if ids := arbRulesViolations(unresolved); len(ids) > 0 {
		fmt.Fprintf(&b, "- %s violate%s a rules file, which is a hard constraint, so `approve_with_notes` is not available to you: narrow the scope or block the bead.\n",
			arbJoinIDs(ids), arbPlural(len(ids)))
	} else {
		fmt.Fprintf(&b, "- Work that still violates a rules file (`%s` items) is never approved with notes; that is a hard constraint.\n", domain.CategoryRules)
	}
	b.WriteString("- Do not ask questions and do not propose new beads here; this ruling is only about this bead.\n")
	b.WriteString("\nAnswer with exactly one `escalation` result and nothing else.")
	return b.String()
}

// rulesViolations returns the ids of the disputes that are rules-file
// violations, in the order they were given.
func arbRulesViolations(items []ItemDispute) []string {
	var ids []string
	for _, d := range items {
		if d.rulesViolation() {
			ids = append(ids, d.Item.ID)
		}
	}
	return ids
}

// joinIDs renders item ids for a sentence: "`a`", "`a` and `b`", "`a`, `b`
// and `c`".
func arbJoinIDs(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}

// plural returns "s" for anything but one.
func arbPlural(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// joinRulings lists the arbitration rulings for error messages.
func arbJoinRulings() string {
	all := domain.ArbitrationRulings()
	parts := make([]string, len(all))
	for i, r := range all {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

// joinOutcomes lists the escalation outcomes for error messages.
func arbJoinOutcomes() string {
	all := domain.EscalationOutcomes()
	parts := make([]string, len(all))
	for i, o := range all {
		parts[i] = o.String()
	}
	return strings.Join(parts, ", ")
}

// oneLine collapses a value onto a single line so list items stay readable.
func arbOneLine(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// suffixSentence appends " <value>" as its own sentence when value is not
// empty.
func arbSentence(s string) string {
	v := arbOneLine(s)
	if v == "" {
		return ""
	}
	if !strings.HasSuffix(v, ".") && !strings.HasSuffix(v, "!") && !strings.HasSuffix(v, "?") {
		v += "."
	}
	return " " + v
}

// suffixColon appends ": <value>" when value is not empty.
func arbColon(s string) string {
	v := arbOneLine(s)
	if v == "" {
		return ""
	}
	return ": " + v
}

// lineRef renders ":<line>" for a positive line number.
func arbLineRef(line int) string {
	if line <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(line)
}
