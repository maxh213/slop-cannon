package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// The rules compliance gate (P9-06).
//
// A repository's rules files are the one thing in the loop nobody may
// negotiate away. A tester that finds the change breaking one raises the
// finding in the [domain.CategoryRules] category, and from that moment the
// only thing that can settle it is code: the engineer changes the diff and
// the tester that raised it says `resolved`.
//
// The roles already know that. [agents.ValidateTesterVerdict] refuses a
// rules item that is not a blocker or that a tester tried to accept,
// [agents.ValidateArbitration] refuses `uphold_engineer` on one, and
// [agents.ValidateEscalation] refuses `approve_with_notes` while one is
// unresolved — all inside structured.Fire's retry loop, so a model that
// tries it is told why and asked again.
//
// Those are checks on one message at a time, made from what the firing's
// request happened to carry. This gate is the run-level backstop underneath
// them, and it is deliberately not derived from the same state:
//
//   - It remembers every rules item it has ever seen on a bead, from the
//     verdict that raised it, and holds it open until the tester that
//     raised it reports it resolved. `accepted` and `overruled` do not
//     settle a rules item here, whoever wrote them and whenever they arrive
//     — so a thread rebuilt from bd comments a resume replays
//     ([Feedback.Restore] validates nothing, by design: the comments are
//     already the record) cannot ship work a live run would have refused.
//   - It refuses the two rulings D-08 forbids ([RulesGate.CheckArbitration],
//     [RulesGate.CheckEscalation]) wherever they come from, and it refuses
//     them twice: once inside the retry loop, where the model can still fix
//     its answer ([RulesGate.Check]), and once as the reply is applied,
//     where the firing fails terminally rather than the loop taking it.
//   - It refuses the D-07 transitions that ship the work — anything landing
//     in approved, delivering or closed — while a violation is open, at the
//     [StateStore] the [StateMachine] writes through
//     ([RulesGate.GuardStore]). That is the last line: no work of the run's
//     own can be moved past review with a rules blocker open, whether it
//     went through the feedback loop, the delivery handoff, a resume or a
//     gate poller. The two status writes that do not go through that store
//     are named, and are not the run's own work, in [RulesGate.GuardStore].
//
// Wiring is four lines and nothing else, so nothing has to know the gate
// exists:
//
//	gate := orchestrator.NewRulesGate(orchestrator.RulesGateOptions{Run: run.ID(), Bus: run, Now: run.Now})
//	states := orchestrator.NewStateMachine(gate.GuardStore(client), …)
//	sup, _ := orchestrator.NewSupervisor(orchestrator.SupervisorOptions{Planner: gate.GuardPlanner(loop), …})
//	cond, _ := orchestrator.NewConductor(orchestrator.ConductorOptions{RulesGate: gate, States: states, …})
//
// The third is the resume: [ConductorOptions.RulesGate] is what makes the
// conductor tell the gate about a thread ([RulesGate.RestoreMessages])
// before [Feedback.Restore] replays it, which is what closes the hole the
// replay leaves — bd comments applied with no role check at all. The
// [ConductorOptions.States] beside it is what the conductor parks such a
// bead with when the replay is refused: a refusal there is permanent, so
// the bead goes to needs_human rather than sitting in in_review with a
// thread the loop believes is settled ([Conductor.restoreThreads]).
//
// `sc run` is the production assembly, and internal/cli's own tests pin
// each of those four wire points, so removing one fails a test rather than
// only a review.

// SourceRules is the [events.Warning] Source the rules compliance gate
// stamps on a message or a transition it refused.
const SourceRules = "rules"

// ErrRulesBlocker is wrapped by every refusal the rules compliance gate
// makes: a message that would settle a violation of a rules file in the
// engineer's favour, or a transition that would ship a bead while one is
// still open (P9-06, D-08).
var ErrRulesBlocker = errors.New("orchestrator: a rules-file violation is a hard constraint")

// IsRulesItem reports whether a feedback item is a violation of a rules
// file, which is the one category the loop treats as non-negotiable
// ([domain.CategoryRules]). The comparison ignores case and surrounding
// space, because the category is free text everywhere else.
func IsRulesItem(it domain.FeedbackItem) bool {
	return strings.EqualFold(strings.TrimSpace(it.Category), domain.CategoryRules)
}

// RulesShippingState reports whether moving a bead into this state ships its
// work: approved is the promise that it may be delivered, delivering is the
// delivery in flight, and closed is the work landed. Those are the three the
// gate will not let a bead reach with a rules violation open.
func RulesShippingState(s domain.BeadState) bool {
	switch s {
	case domain.BeadStateApproved, domain.BeadStateDelivering, domain.BeadStateClosed:
		return true
	default:
		return false
	}
}

// RulesShippingEvent reports whether a [BeadEvent] can land a bead in a
// [RulesShippingState] from anywhere in the D-07 table — approved,
// approved_with_notes, delivery_started, delivery_retried and delivered. It
// is derived from [BeadTransitions] rather than listed, so an edge added to
// the table cannot slip past the gate.
func RulesShippingEvent(ev BeadEvent) bool {
	for _, t := range BeadTransitions() {
		if t.Event == ev && RulesShippingState(t.To) {
			return true
		}
	}
	return false
}

// RulesViolation is one open violation of a rules file on one bead.
type RulesViolation struct {
	// Bead is the bead the violation is on.
	Bead domain.BeadID
	// Item is the feedback item's id, as the verdict that raised it wrote
	// it.
	Item string
	// Title is the item's one-line summary.
	Title string
	// Round is the review round it was first raised in.
	Round int
	// Lenses are the testers that raised it, in the order they did.
	Lenses []domain.Lens
}

// String renders the violation for an error message or a log line.
func (v RulesViolation) String() string {
	s := v.Item
	if v.Title != "" {
		s += " (" + fbOneLine(v.Title) + ")"
	}
	return s
}

// RulesGateStats is what the gate has seen and done. It is folded into the
// run's vitals ([RunVitals.WithRules]), so it reaches state.json and the
// `sc status` line [RunVitals.RulesLine] prints; a run that never saw a
// rules item is zero here and prints nothing.
type RulesGateStats struct {
	// Beads is how many beads the gate has recorded a rules item on.
	Beads int `json:"beads,omitempty"`
	// Blocked is how many of those still have one open.
	Blocked int `json:"blocked,omitempty"`
	// Violations is how many open violations there are in all.
	Violations int `json:"violations,omitempty"`
	// Refused counts every message and transition the gate refused.
	Refused int `json:"refused,omitempty"`
	// Narrowed counts the open violations a narrow_scope ruling dropped:
	// the one thing that clears the gate's memory without the code being
	// fixed, and the reason it is counted rather than forgotten quietly
	// ([RulesGate.ObserveEscalation]).
	Narrowed int `json:"narrowed,omitempty"`
}

// Zero reports whether the gate has seen nothing and refused nothing, which
// is what a run with no violation of a rules file leaves in state.json and
// what [RunVitals.RulesLine] prints nothing for.
func (s RulesGateStats) Zero() bool { return s == RulesGateStats{} }

// RulesGateOptions configures [NewRulesGate]. Every field is optional.
type RulesGateOptions struct {
	// Run is the run id stamped on the warnings the gate publishes.
	Run domain.RunID
	// Bus is where those warnings go; nil publishes nothing.
	Bus Publisher
	// Logger is the run's logger; nil discards.
	Logger *slog.Logger
	// Now is the clock the warnings are stamped with; nil means time.Now.
	Now func() time.Time
}

// RulesGate is the run's rules compliance gate: the one place that knows
// which beads carry an open violation of a rules file, and the one that
// refuses to let such a bead be argued or shipped past it. It is safe for
// concurrent use.
type RulesGate struct {
	run domain.RunID
	bus Publisher
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	tracked  map[domain.BeadID]*rulesBead
	refusals int
	narrowed int
}

// rulesBead is one bead's rules items, in the order they were raised.
type rulesBead struct {
	order []string
	items map[string]*rulesItem
}

// rulesItem is one rules-file violation and where each tester that raised
// it stands on it.
type rulesItem struct {
	id     string
	title  string
	round  int
	lenses []domain.Lens
	per    map[domain.Lens]domain.ItemStatus
}

// open reports whether the violation still blocks. Only [domain.ItemResolved]
// settles it, and only from every tester that raised it: a rules violation
// leaves the code broken until the code changes, so an `accepted` pushback
// and an `overruled` ruling are, here, still open.
func (i *rulesItem) open() bool {
	if len(i.lenses) == 0 {
		return true
	}
	for _, l := range i.lenses {
		if i.per[l] != domain.ItemResolved {
			return true
		}
	}
	return false
}

// violation renders the item.
func (i *rulesItem) violation(bead domain.BeadID) RulesViolation {
	return RulesViolation{
		Bead: bead, Item: i.id, Title: i.title, Round: i.round,
		Lenses: append([]domain.Lens(nil), i.lenses...),
	}
}

// NewRulesGate returns a gate that has seen nothing yet.
func NewRulesGate(opts RulesGateOptions) *RulesGate {
	g := &RulesGate{
		run: opts.Run, bus: opts.Bus, log: opts.Logger, now: opts.Now,
		tracked: map[domain.BeadID]*rulesBead{},
	}
	if g.log == nil {
		g.log = slog.New(slog.DiscardHandler)
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

// --- what the gate knows ----------------------------------------------

// ObserveVerdict folds a tester's verdict into the gate: every item in the
// rules category is recorded against the bead, and the status the tester
// gave its own occurrence is remembered.
//
// An item the gate already knows as a rules violation stays one whatever
// category a later verdict gives it: a violation cannot be relabelled out
// of existence, which is also what [agents.ValidateTesterVerdict] refuses
// one message at a time.
func (g *RulesGate) ObserveVerdict(bead domain.BeadID, v domain.Verdict) {
	if bead == "" {
		return
	}
	lens := v.Lens
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, it := range v.Items {
		if !IsRulesItem(it) {
			// Either an ordinary item, which is the loop's business and not
			// the gate's, or a verdict renaming a violation out of its
			// category — which [RulesGate.CheckVerdict] refuses, and whose
			// word on the item is not taken here either: a tester that will
			// not call it a rules item is not the one to say the rules file
			// is satisfied again. Both leave what the gate remembers alone.
			continue
		}
		known := g.itemLocked(bead, it.ID)
		if known == nil {
			known = g.addLocked(bead, it, v.Round)
		}
		if known.title == "" {
			known.title = it.Title
		}
		if _, seen := known.per[lens]; !seen {
			known.lenses = append(known.lenses, lens)
		}
		known.per[lens] = it.Status
	}
}

// ObserveEscalation folds the product owner's ruling on a bead that ran out
// of review rounds into the gate.
//
// Only narrow_scope changes anything: it replaces the bead's acceptance
// criteria and the loop drops every item raised against the old ones
// (D-08), so the gate drops them too and the narrowed rounds start clean.
// approve_with_notes is refused while a violation is open
// ([RulesGate.CheckEscalation]) and settles nothing if it arrives anyway;
// block leaves the bead with a human, violation and all.
//
// This is the gate's one deliberate exception, and it is deliberate in both
// directions:
//
//   - It has to clear them. The narrowed rounds review a different bead:
//     the items raised against the old criteria no longer exist, so no
//     tester will ever mark one resolved, and a violation the gate kept
//     would block that bead's every shipping transition for the rest of the
//     run with nothing anyone could do about it. That is a worse failure
//     than the one below, and it is not the kind [Conductor.restoreThreads]
//     can hand to a person either, because the bead is live work.
//   - It is not free. [agents.ValidateEscalation] only asks a narrow_scope
//     ruling for a non-empty new_acceptance, so a product owner that
//     answers the round cap with narrow_scope plus restated criteria does
//     clear the gate's memory of a violation no tester ever resolved. So
//     the drop is counted ([RulesGateStats.Narrowed], which reaches
//     state.json and the `sc status` rules line) and announced as a warning
//     naming every violation it dropped. Nothing is silently laundered:
//     the narrowed bead still has to earn its approvals again, and the
//     record says what the ruling wrote off.
func (g *RulesGate) ObserveEscalation(bead domain.BeadID, e domain.Escalation) {
	if e.Outcome != domain.EscalationNarrowScope {
		return
	}
	g.mu.Lock()
	dropped := g.openLocked(bead)
	g.narrowed += len(dropped)
	n := g.narrowed
	delete(g.tracked, bead)
	g.mu.Unlock()
	if len(dropped) == 0 {
		return
	}
	g.log.Warn("a narrow_scope ruling dropped open rules-file violations with the criteria that raised them",
		slog.String("bead", bead.String()), slog.Int("dropped", len(dropped)),
		slog.Int("narrowed", n), slog.String("violations", rulesList(dropped)))
	if g.bus == nil {
		return
	}
	g.bus.Publish(events.Warning{
		Meta:    events.MetaAt(g.run, g.now()),
		Source:  SourceRules,
		Message: "a narrow_scope ruling dropped open rules-file violations along with the criteria that raised them",
		Err:     fmt.Sprintf("the gate no longer holds %s; the narrowed rounds start clean and have to earn their approvals again", rulesList(dropped)),
		Bead:    bead,
	})
}

// Observe folds whichever of the loop's structured messages the payload is
// into the gate, and ignores the rest. It is what [RulesGate.GuardPlanner]
// calls on every reply it lets through, and what [RulesGate.RestoreMessages]
// calls on every comment it replays.
//
// A [domain.Arbitration] is deliberately inert: no ruling settles a rules
// violation, so there is nothing to fold. The ruling the gate does care
// about — uphold_engineer on a rules item — never reaches here, because
// [RulesGate.CheckArbitration] refused it first.
func (g *RulesGate) Observe(bead domain.BeadID, payload any) {
	switch v := payload.(type) {
	case domain.Verdict:
		g.ObserveVerdict(bead, v)
	case domain.Escalation:
		g.ObserveEscalation(bead, v)
	}
}

// RulesRestorer is the slice of [RulesGate] the run's glue holds: what a
// resume must tell the gate before a thread rebuilt from bd's comments is
// replayed. [ConductorOptions.RulesGate] is this interface, so the conductor
// depends on the one method rather than on the gate.
type RulesRestorer interface {
	// RestoreMessages folds a bead's replayed comments into the gate.
	RestoreMessages(bead domain.BeadID, msgs []beads.Message)
}

// The gate is the production restorer.
var _ RulesRestorer = (*RulesGate)(nil)

// RestoreMessages rebuilds a bead's rules state from the comments a resume
// read back from bd (D-06, [Feedback.Restore]), in the order they were
// written. Messages that failed to decode are skipped, like the loop skips
// them.
//
// A resumed run needs this: the replay applies the thread's own record
// without re-validating it, so without the gate's memory a violation that
// an old comment settled the wrong way would come back settled. It is
// [Conductor.restoreThreads] that calls it, from [ConductorOptions.RulesGate],
// on the same messages it then hands [Feedback.Restore].
func (g *RulesGate) RestoreMessages(bead domain.BeadID, msgs []beads.Message) {
	for _, m := range msgs {
		if m.Err != nil {
			continue
		}
		g.Observe(bead, m.Payload)
	}
}

// Blocked reports whether the bead has an open violation of a rules file.
func (g *RulesGate) Blocked(bead domain.BeadID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.openLocked(bead)) > 0
}

// Open lists the bead's open violations, oldest first.
func (g *RulesGate) Open(bead domain.BeadID) []RulesViolation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openLocked(bead)
}

// Forget drops everything the gate remembers about a bead. Call it when the
// bead's items no longer exist — a narrow_scope ruling replaced them — not
// when a bead is merely settled: a gate that forgets a shipped bead cannot
// answer for it afterwards, and remembering costs one map entry.
func (g *RulesGate) Forget(bead domain.BeadID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tracked, bead)
}

// Stats reports what the gate has seen and refused.
func (g *RulesGate) Stats() RulesGateStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := RulesGateStats{Beads: len(g.tracked), Refused: g.refusals, Narrowed: g.narrowed}
	for bead := range g.tracked {
		if open := g.openLocked(bead); len(open) > 0 {
			st.Blocked++
			st.Violations += len(open)
		}
	}
	return st
}

// itemLocked returns the tracked rules item with that id, or nil.
func (g *RulesGate) itemLocked(bead domain.BeadID, id string) *rulesItem {
	b, ok := g.tracked[bead]
	if !ok {
		return nil
	}
	return b.items[id]
}

// addLocked starts tracking a rules item.
func (g *RulesGate) addLocked(bead domain.BeadID, it domain.FeedbackItem, round int) *rulesItem {
	b, ok := g.tracked[bead]
	if !ok {
		b = &rulesBead{items: map[string]*rulesItem{}}
		g.tracked[bead] = b
	}
	i := &rulesItem{id: it.ID, title: it.Title, round: round, per: map[domain.Lens]domain.ItemStatus{}}
	b.items[it.ID] = i
	b.order = append(b.order, it.ID)
	return i
}

// openLocked lists the bead's open violations in the order they were raised.
func (g *RulesGate) openLocked(bead domain.BeadID) []RulesViolation {
	b, ok := g.tracked[bead]
	if !ok {
		return nil
	}
	var out []RulesViolation
	for _, id := range b.order {
		if i := b.items[id]; i != nil && i.open() {
			out = append(out, i.violation(bead))
		}
	}
	return out
}

// --- what a person sees -----------------------------------------------

// RulesReporter is the slice of [RulesGate] the run's vitals read.
type RulesReporter interface {
	// Stats is the gate's counters.
	Stats() RulesGateStats
}

// The gate is the production reporter.
var _ RulesReporter = (*RulesGate)(nil)

// WithRules folds a rules gate's counters into the vitals, so what the gate
// has refused reaches state.json and `sc status` with everything else the
// run is waiting on:
//
//	run.UseVitals(func() orchestrator.RunVitals {
//		return orchestrator.Vitals(now(), planner, poller, foreign).WithRules(gate)
//	})
//
// A nil reporter leaves the snapshot alone, which is what a run assembled
// without a gate produces.
func (v RunVitals) WithRules(g RulesReporter) RunVitals {
	if g == nil {
		return v
	}
	v.Rules = g.Stats()
	return v
}

// RulesLine says what the rules compliance gate is holding and what it has
// refused, or "" when it has seen nothing — which is every run in a
// repository whose rules files nobody broke, so the line appears only when
// it means something.
func (v RunVitals) RulesLine() string {
	r := v.Rules
	if r.Zero() {
		return ""
	}
	var parts []string
	// A gate whose only entry was dropped by a narrow_scope ruling has no
	// bead left to count, so it says what happened rather than "0 beads".
	if r.Beads > 0 {
		parts = append(parts, rulesPlural(r.Beads, "bead", "beads")+" with a rules item")
		if r.Blocked > 0 {
			parts = append(parts, fmt.Sprintf("%s blocked by %s",
				rulesPlural(r.Blocked, "bead", "beads"),
				rulesPlural(r.Violations, "open violation", "open violations")))
		} else {
			parts = append(parts, "none blocked")
		}
	}
	if r.Refused > 0 {
		parts = append(parts, rulesPlural(r.Refused, "refusal", "refusals"))
	}
	if r.Narrowed > 0 {
		parts = append(parts, rulesPlural(r.Narrowed, "violation", "violations")+" dropped by narrow_scope")
	}
	return "rules: " + strings.Join(parts, " · ")
}

// rulesPlural renders a count with the noun that agrees with it.
func rulesPlural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// --- what the gate refuses --------------------------------------------

// CheckVerdict refuses a verdict that mishandles a violation of a rules
// file: one raised at any severity but blocker, one a tester marked
// accepted or overruled (only code resolves a rules item, and only the
// product owner overrules anything at all), and one restated under some
// other category once the gate has seen it as a rules violation.
//
// It is [agents.ValidateTesterVerdict]'s rule, applied to the run's memory
// of the thread rather than to the one request the firing carried.
func (g *RulesGate) CheckVerdict(bead domain.BeadID, v domain.Verdict) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, it := range v.Items {
		known := g.itemLocked(bead, it.ID)
		if known == nil && !IsRulesItem(it) {
			continue
		}
		where := fmt.Sprintf("item %q", it.ID)
		switch {
		case known != nil && !IsRulesItem(it):
			return fmt.Errorf("%w: %s was raised as a %q violation and cannot be moved to category %q",
				ErrRulesBlocker, where, domain.CategoryRules, strings.TrimSpace(it.Category))
		case it.Severity != domain.SeverityBlocker:
			return fmt.Errorf("%w: %s is a %q violation, so its severity is %q, not %q",
				ErrRulesBlocker, where, domain.CategoryRules, domain.SeverityBlocker, string(it.Severity))
		case it.Status == domain.ItemAccepted || it.Status == domain.ItemOverruled:
			return fmt.Errorf("%w: %s is a %q violation and cannot be %s; it stays open until the diff fixes it and the tester marks it %s",
				ErrRulesBlocker, where, domain.CategoryRules, string(it.Status), domain.ItemResolved)
		}
	}
	return nil
}

// CheckArbitration refuses a per-item ruling that decides a violation of a
// rules file in the engineer's favour. A rules file is a hard constraint,
// so the product owner may uphold the tester or impose a compromise that
// still satisfies the rule, and nothing else (D-08, P5-05).
func (g *RulesGate) CheckArbitration(bead domain.BeadID, a domain.Arbitration) error {
	if a.Ruling != domain.RulingUpholdEngineer {
		return nil
	}
	g.mu.Lock()
	known := g.itemLocked(bead, a.ItemID)
	g.mu.Unlock()
	if known == nil {
		return nil
	}
	return fmt.Errorf("%w: item %q is a %q violation, so it cannot be ruled %q; uphold the tester or impose a compromise that satisfies the rule",
		ErrRulesBlocker, a.ItemID, domain.CategoryRules, domain.RulingUpholdEngineer)
}

// CheckEscalation refuses approve_with_notes on a bead that still carries an
// open violation of a rules file. Work that breaks a rules file is not
// shippable with a note about it: the product owner narrows the scope or
// blocks the bead (D-08, P5-05).
func (g *RulesGate) CheckEscalation(bead domain.BeadID, e domain.Escalation) error {
	if e.Outcome != domain.EscalationApproveWithNotes {
		return nil
	}
	g.mu.Lock()
	open := g.openLocked(bead)
	g.mu.Unlock()
	if len(open) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s still %s a rules file, so %q is not available: narrow the scope or block the bead",
		ErrRulesBlocker, rulesList(open), rulesVerb(len(open)), domain.EscalationApproveWithNotes)
}

// CheckPayload refuses whichever of the loop's structured messages the
// payload is, and passes anything else. It is the one entry point
// [RulesGate.GuardPlanner] and [RulesGate.Check] both go through, so the
// retry loop and the reply that is about to be applied are judged by
// exactly the same rules.
func (g *RulesGate) CheckPayload(bead domain.BeadID, payload any) error {
	switch v := payload.(type) {
	case domain.Verdict:
		return g.CheckVerdict(bead, v)
	case domain.Arbitration:
		return g.CheckArbitration(bead, v)
	case domain.Escalation:
		return g.CheckEscalation(bead, v)
	default:
		return nil
	}
}

// CheckTransition refuses a D-07 event that would ship a bead carrying an
// open violation of a rules file ([RulesShippingEvent]). Every other event
// passes: a blocked bead may still be escalated, sent back to the engineer,
// handed to a human or failed.
func (g *RulesGate) CheckTransition(bead domain.BeadID, ev BeadEvent) error {
	if !RulesShippingEvent(ev) {
		return nil
	}
	return g.checkShip(bead, fmt.Sprintf("the %q transition", string(ev)))
}

// CheckState refuses a move into a state that ships the work
// ([RulesShippingState]) while a violation of a rules file is open. It is
// what [RulesGate.GuardStore] applies to every write, so the refusal does
// not depend on the caller having gone through [Transition].
func (g *RulesGate) CheckState(bead domain.BeadID, to domain.BeadState) error {
	if !RulesShippingState(to) {
		return nil
	}
	return g.checkShip(bead, fmt.Sprintf("a move to %q", string(to)))
}

// checkShip is the shared refusal of anything that would ship the bead.
func (g *RulesGate) checkShip(bead domain.BeadID, what string) error {
	g.mu.Lock()
	open := g.openLocked(bead)
	g.mu.Unlock()
	if len(open) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the rules gate refuses %s on %s: %s still %s a rules file, so fix the code first",
		ErrRulesBlocker, what, bead, rulesList(open), rulesVerb(len(open)))
}

// rulesList renders the open violations for an error message.
func rulesList(vs []RulesViolation) string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.String())
	}
	return strings.Join(out, ", ")
}

// rulesVerb agrees the verb with however many violations there are.
func rulesVerb(n int) string {
	if n == 1 {
		return "violates"
	}
	return "violate"
}

// --- the seams --------------------------------------------------------

// Check wraps a firing's [structured.ResultCheck] with the gate's own, so a
// reply that a rules file forbids is refused inside structured.Fire's retry
// loop and the model is told why and asked again. next may be nil.
//
// The gate's check runs first, on purpose: it is the constraint that cannot
// be argued with, so it is the feedback the model should see before any
// other complaint about the same reply.
//
// agent is the firing's agent, so a refusal made inside the retry loop is
// attributed in the run log and on the TUI exactly like the one made as the
// reply is applied ([RulesGate.Guard]); the empty id is allowed, for a
// caller that has no agent record yet.
//
// msg says which payload the reply carries; a firing with no message type
// (a caller that compiled its own contract) is left alone, because the gate
// has no way to decode it.
func (g *RulesGate) Check(bead domain.BeadID, agent domain.AgentID, msg domain.MessageType, next structured.ResultCheck) structured.ResultCheck {
	if !rulesChecks(msg) {
		return next
	}
	return func(p structured.Parsed, output string) error {
		payload, err := rulesDecode(msg, p.JSON)
		if err == nil {
			if err := g.CheckPayload(bead, payload); err != nil {
				g.refused(bead, agent, err)
				return err
			}
		}
		if next == nil {
			return nil
		}
		return next(p, output)
	}
}

// rulesChecks reports whether the gate has anything to say about a message
// type.
func rulesChecks(msg domain.MessageType) bool {
	switch msg {
	case domain.MessageVerdict, domain.MessageArbitration, domain.MessageEscalation:
		return true
	default:
		return false
	}
}

// rulesDecode turns a validated block into the payload the gate checks. It
// decodes the three message types the gate judges and nothing else; the
// schema has already accepted the JSON, so a failure here means the block
// is not what the firing said it was, and the caller lets the role's own
// check produce the complaint.
//
// Anything else decodes to nil rather than to a complaint of its own: the
// only caller checks [rulesChecks] first, which admits exactly the three
// types below, so the last branch is reachable only if the two ever
// disagree — and the honest answer then is the one
// [RulesGate.CheckPayload] gives an unknown payload anyway, which is to
// leave it alone. Building a message here would put a sentence in front of
// a user that no path produces and that P9-08's grep cannot see (it scans
// internal/cli and internal/config, and sentinels elsewhere).
func rulesDecode(msg domain.MessageType, raw json.RawMessage) (any, error) {
	switch msg {
	case domain.MessageVerdict:
		var v domain.Verdict
		return v, json.Unmarshal(raw, &v)
	case domain.MessageArbitration:
		var a domain.Arbitration
		return a, json.Unmarshal(raw, &a)
	case domain.MessageEscalation:
		var e domain.Escalation
		return e, json.Unmarshal(raw, &e)
	default:
		return nil, nil
	}
}

// Guard wraps one [AgentFiring] with the gate: the firing's own result
// check gains the gate's ([RulesGate.Check]), and the reply is judged once
// more as it is applied, where a refusal fails the firing terminally
// ([AgentFiring.Result]) rather than letting the loop take it.
//
// The reply that passes is folded into the gate before the firing's own
// Result sees it, so the loop and the gate never disagree about what the
// tester just said.
//
// bead names the bead the firing is about; the empty id falls back to the
// firing's own agent record.
func (g *RulesGate) Guard(bead domain.BeadID, af AgentFiring) AgentFiring {
	if bead == "" {
		bead = af.Agent.Bead
	}
	if build := af.Build; build != nil {
		af.Build = func(ctx context.Context, fc FiringContext) (agents.AgentRun, error) {
			run, err := build(ctx, fc)
			if err != nil {
				return run, err
			}
			run.Check = g.Check(bead, af.Agent.ID, run.Message, run.Check)
			return run, nil
		}
	}
	if result := af.Result; result != nil {
		af.Result = func(payload any, tr agents.Transcript) error {
			if err := g.CheckPayload(bead, payload); err != nil {
				g.refused(bead, af.Agent.ID, err)
				return err
			}
			g.Observe(bead, payload)
			return result(payload, tr)
		}
	}
	return af
}

// GuardPlanner wraps a [FiringPlanner] so that every firing it plans is
// guarded ([RulesGate.Guard]). Give the supervisor the wrapper instead of
// the feedback loop and no reply reaches the loop unjudged.
func (g *RulesGate) GuardPlanner(next FiringPlanner) FiringPlanner {
	return FiringPlannerFunc(func(ctx context.Context, f Firing) (AgentFiring, error) {
		af, err := next.PlanFiring(ctx, f)
		if err != nil {
			return af, err
		}
		return g.Guard(f.Demand.Bead, af), nil
	})
}

// GuardStore wraps the [StateStore] the [StateMachine] writes through so
// that no component of the run — the loop, the delivery handoff, a resume,
// the gate poller — can move a bead into a state that ships its work while
// a violation of a rules file is open ([RulesGate.CheckState]).
//
// The other writes pass untouched: claiming, reclaiming and releasing a
// bead with an open violation are exactly what has to keep happening for
// the engineer to fix it.
//
// Two status writes of a run do not go through this store, and neither is
// the run shipping its own work, so neither is guarded (doc.go lists them
// with the scheduler's atomic claim):
//
//   - [PRReview.close] closes an *external* review bead through the raw bd
//     client. Nothing of ours ships: the cannon never merges somebody
//     else's pull request, and the bead is the record of a review that is
//     over. A rules item raised on such a review is a comment on the pull
//     request, not a change the loop can make.
//   - [ResumeTracker] reconciles a killed run's branches through its own
//     D-07 guard ([gitx.Resumer], P6-15). Its writes record what git and
//     GitHub already did — a branch that merged is merged — so refusing
//     them would leave bd disagreeing with the repository rather than
//     unship anything. The gate still holds the violation, so the *next*
//     move the resumed run tries to make on that bead is refused.
func (g *RulesGate) GuardStore(next StateStore) StateStore {
	return rulesGuardedStore{gate: g, next: next}
}

// rulesGuardedStore is [RulesGate.GuardStore]'s wrapper.
type rulesGuardedStore struct {
	gate *RulesGate
	next StateStore
}

// The wrapper is a [StateStore] itself, so it can stand where the client
// stands (state.go asserts *beads.Client is one).
var _ StateStore = rulesGuardedStore{}

// SetStatus refuses a status that would ship a bead with an open violation.
func (s rulesGuardedStore) SetStatus(ctx context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	if err := s.gate.CheckState(id, state); err != nil {
		s.gate.refused(id, "", err)
		return beads.Issue{}, err
	}
	return s.next.SetStatus(ctx, id, state)
}

// ClaimFor passes through: a claim puts the bead in front of an engineer,
// which is what an open violation wants.
func (s rulesGuardedStore) ClaimFor(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	return s.next.ClaimFor(ctx, id, agent)
}

// Reclaim passes through, for the same reason as ClaimFor.
func (s rulesGuardedStore) Reclaim(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	return s.next.Reclaim(ctx, id, agent)
}

// Release passes through: releasing a bead ships nothing.
func (s rulesGuardedStore) Release(ctx context.Context, id domain.BeadID) (beads.Issue, error) {
	return s.next.Release(ctx, id)
}

// Close refuses to close a bead with an open violation: closed is where the
// work has landed.
func (s rulesGuardedStore) Close(ctx context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	if err := s.gate.CheckState(id, domain.BeadStateClosed); err != nil {
		s.gate.refused(id, "", err)
		return beads.Issue{}, err
	}
	return s.next.Close(ctx, id, reason)
}

// refused counts a refusal, logs it and publishes it as a warning, so a
// refusal is visible in the TUI and in the run log rather than only in the
// error the caller got back.
func (g *RulesGate) refused(bead domain.BeadID, agent domain.AgentID, err error) {
	g.mu.Lock()
	g.refusals++
	n := g.refusals
	g.mu.Unlock()
	g.log.Warn("the rules compliance gate refused work on a bead with an open rules violation",
		slog.String("bead", bead.String()), slog.String("agent", agent.String()),
		slog.Int("refusals", n), slog.String("error", err.Error()))
	if g.bus == nil {
		return
	}
	g.bus.Publish(events.Warning{
		Meta:    events.MetaAt(g.run, g.now()),
		Source:  SourceRules,
		Message: "refused work that would settle a rules-file violation in the engineer's favour",
		Err:     err.Error(),
		Bead:    bead,
		Agent:   agent,
	})
}
