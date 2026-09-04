package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the D-07 bead state machine (P6-03, P6-12).
//
// [Transition] is a pure function over a table of (state, event) pairs: it
// never touches bd, never blocks and never panics, so the scheduler, the
// feedback loop and the delivery handoff can all ask "what would happen"
// before doing anything. [StateMachine] is the thin persisting wrapper: it
// runs the same table and then writes the answer through internal/beads with
// SetStatus, ClaimFor/Reclaim, Release or Close, publishing a
// [events.BeadStateChanged] on success and a [events.Warning] when the
// transition was refused.
//
// The flow the table encodes (D-07):
//
//	open → in_progress → in_review → (changes_requested → in_progress …)
//	     → approved → delivering → closed
//
// with escalated, needs_human and failed as side exits. Only the ten states
// [domain.WorkflowBeadStates] returns take part. bd's own blocked, hooked,
// deferred and pinned are read-only for the cannon (D-07), and every status a
// target repository defined itself lands on one of those four (or on closed)
// through [beads.SafeState]; none of them may be transitioned (P3-14, P6-12).
// Use [TransitionStatus] when the state came off a listing rather than out of
// the cannon's own bookkeeping, so a foreign status that happens to map onto a
// workflow state is still refused.
//
// [StateMachine.ReleaseHeld] is the same rule applied to a shutting-down run:
// the beads its live agents hold go back to the ready set through the table
// (in_progress --released--> open) or not at all, because the bd update the
// release performs would just as happily drag an in_review bead out of review.

// Errors reported by the state machine, matched with errors.Is.
var (
	// ErrInvalidTransition means the (state, event) pair has no edge in the
	// D-07 table: the bead is in a state that event cannot act on. It is an
	// error and a Warning event, never a panic.
	ErrInvalidTransition = errors.New("orchestrator: invalid bead state transition")
	// ErrNotWorkflowState means the state is not one of the D-07 workflow
	// states: an empty or unknown value, or one of bd's read-only built-ins
	// (blocked, hooked, deferred, pinned) that the cannon never sets.
	ErrNotWorkflowState = errors.New("orchestrator: not a workflow bead state")
	// ErrForeignStatus means the bd status belongs to the target repository's
	// own custom statuses, so the bead is none of the cannon's business even
	// when beads.SafeState maps it onto a state the table knows (P6-12).
	ErrForeignStatus = errors.New("orchestrator: bead carries a status slop-cannon did not register")
	// ErrUnknownBeadEvent means the trigger is not one of [BeadEvents].
	ErrUnknownBeadEvent = errors.New("orchestrator: unknown bead event")
	// ErrNoAgent means a claiming transition was applied without the agent
	// that is taking the bead.
	ErrNoAgent = errors.New("orchestrator: transition needs an agent id")
)

// BeadEvent is a trigger of the D-07 state machine: the thing that happened
// to a bead, as opposed to the state it is in. Each one is produced by a
// specific part of the orchestrator (the scheduler claims, the feedback loop
// reviews, the delivery handoff ships) and drives at most one edge out of any
// given state, so [Transition] is a function and not a search.
type BeadEvent string

// The bead events, grouped by the part of the flow that raises them.
const (
	// BeadEventClaimed: the scheduler handed the bead to an engineer with
	// `bd update --claim` (P6-02, P6-13). open → in_progress.
	BeadEventClaimed BeadEvent = "claimed"
	// BeadEventReleased: the engineer's claim went away without work landing
	// — the agent crashed, was stopped, or its lease was recovered — so the
	// bead goes back to the ready set. in_progress → open.
	BeadEventReleased BeadEvent = "released"
	// BeadEventSubmitted: the engineer committed its work and filed an
	// impl_report, so the bead is ready for review. in_progress → in_review.
	BeadEventSubmitted BeadEvent = "submitted"

	// BeadEventChangesRequested: at least one tester's verdict asked for
	// changes (D-08). in_review → changes_requested.
	BeadEventChangesRequested BeadEvent = "changes_requested"
	// BeadEventReworkStarted: the engineer picked the bead back up for the
	// next round, through beads.Reclaim (P6-13). changes_requested →
	// in_progress.
	BeadEventReworkStarted BeadEvent = "rework_started"
	// BeadEventApproved: every assigned tester approved.
	// in_review → approved.
	BeadEventApproved BeadEvent = "approved"

	// BeadEventEscalated: the review round cap was reached without approval,
	// so the PO must rule (D-08). in_review, changes_requested → escalated.
	BeadEventEscalated BeadEvent = "escalated"
	// BeadEventScopeNarrowed: the PO's escalation ruling was narrow_scope, so
	// the narrowed acceptance criteria are reviewed again
	// (agents.EscalationBeadState). escalated → in_review.
	BeadEventScopeNarrowed BeadEvent = "scope_narrowed"
	// BeadEventApprovedWithNotes: the PO's escalation ruling was
	// approve_with_notes, so the work ships as it stands
	// (agents.EscalationBeadState). escalated → approved.
	BeadEventApprovedWithNotes BeadEvent = "approved_with_notes"

	// BeadEventDeliveryStarted: the delivery handoff took the bead (P6-06).
	// approved → delivering.
	BeadEventDeliveryStarted BeadEvent = "delivery_started"
	// BeadEventDeliveryRetried: a delivery that had been handed to a human
	// (a push-only or no-remote hand-off, P6-16) is being retried, so the
	// bead is put back in flight before the retry. needs_human → delivering.
	BeadEventDeliveryRetried BeadEvent = "delivery_retried"
	// BeadEventConflict: the rebase or merge hit a conflict, so the bead goes
	// back to its engineer in resolve-conflict mode (D-10). delivering →
	// in_progress.
	BeadEventConflict BeadEvent = "conflict"
	// BeadEventCheckFailed: check_command failed during delivery, which is
	// treated as a qa change-request round (P6-06). delivering →
	// changes_requested.
	BeadEventCheckFailed BeadEvent = "check_failed"
	// BeadEventDelivered: the work landed on the default branch — merged
	// directly with safety off, or the gh:pr gate resolved with safety on
	// (D-20, P6-10). delivering → closed.
	BeadEventDelivered BeadEvent = "delivered"

	// BeadEventBlocked: the cannon cannot go further without a person (a PO
	// block ruling, an escalated gate, a delivery hand-off). Valid from every
	// non-terminal state. → needs_human.
	BeadEventBlocked BeadEvent = "blocked"
	// BeadEventFailed: the bead is abandoned after unrecoverable errors.
	// Valid from every non-terminal state. → failed.
	//
	// TODO(P6-31): nothing in the assembled cannon raises this yet. The
	// trigger D-07 wants for it is a demand whose firing keeps failing —
	// the consecutive-failure budget P6-31 owns — so until that lands the
	// failed state is reachable only when a person sets it in bd by hand.
	// The edges are here because P6-05's stop condition and
	// [BeadStateSettled] already treat failed as terminal, so the day the
	// budget parks a bead the write must go through this table like every
	// other.
	BeadEventFailed BeadEvent = "failed"
)

// beadEvents lists every BeadEvent in the order the flow raises them.
var beadEvents = []BeadEvent{
	BeadEventClaimed, BeadEventReleased, BeadEventSubmitted,
	BeadEventChangesRequested, BeadEventReworkStarted, BeadEventApproved,
	BeadEventEscalated, BeadEventScopeNarrowed, BeadEventApprovedWithNotes,
	BeadEventDeliveryStarted, BeadEventDeliveryRetried, BeadEventConflict,
	BeadEventCheckFailed, BeadEventDelivered,
	BeadEventBlocked, BeadEventFailed,
}

// BeadEvents returns every valid BeadEvent, in flow order.
func BeadEvents() []BeadEvent { return append([]BeadEvent(nil), beadEvents...) }

// String returns the event name as it appears in logs and events.
func (e BeadEvent) String() string { return string(e) }

// Valid reports whether e is one of [BeadEvents].
func (e BeadEvent) Valid() bool {
	for _, v := range beadEvents {
		if v == e {
			return true
		}
	}
	return false
}

// ParseBeadEvent parses an event name, tolerating surrounding space and case
// the way domain's enums do. An unknown name is [ErrUnknownBeadEvent].
func ParseBeadEvent(s string) (BeadEvent, error) {
	e := BeadEvent(strings.ToLower(strings.TrimSpace(s)))
	if !e.Valid() {
		return "", fmt.Errorf("%w: %q (want one of %s)", ErrUnknownBeadEvent, s, joinBeadEvents(beadEvents))
	}
	return e, nil
}

// BeadTransition is one edge of the D-07 table, as [BeadTransitions] reports
// it.
type BeadTransition struct {
	// From is the state the bead must be in.
	From domain.BeadState
	// Event is the trigger.
	Event BeadEvent
	// To is the state the bead ends up in.
	To domain.BeadState
}

// transitionKey identifies an edge by its left-hand side.
type transitionKey struct {
	from  domain.BeadState
	event BeadEvent
}

// beadTransitionTable is the D-07 transition table. It is built once at
// package init and never mutated, so [Transition] is a map lookup with no
// locking.
var beadTransitionTable = buildBeadTransitionTable()

// buildBeadTransitionTable spells out the flow, then adds the two side exits
// (blocked, failed) for every non-terminal state. That is 15 flow edges plus
// two exits for each of the seven non-terminal workflow states: 29 in all, as
// [TestStateTransitionCountIsTheOneTheRecordClaims] pins.
func buildBeadTransitionTable() map[transitionKey]domain.BeadState {
	edges := []BeadTransition{
		// The happy path.
		{domain.BeadStateOpen, BeadEventClaimed, domain.BeadStateInProgress},
		{domain.BeadStateInProgress, BeadEventSubmitted, domain.BeadStateInReview},
		{domain.BeadStateInReview, BeadEventApproved, domain.BeadStateApproved},
		{domain.BeadStateApproved, BeadEventDeliveryStarted, domain.BeadStateDelivering},
		{domain.BeadStateDelivering, BeadEventDelivered, domain.BeadStateClosed},

		// The review loop (D-08).
		{domain.BeadStateInReview, BeadEventChangesRequested, domain.BeadStateChangesRequested},
		{domain.BeadStateChangesRequested, BeadEventReworkStarted, domain.BeadStateInProgress},

		// A claim that went away before any work landed.
		{domain.BeadStateInProgress, BeadEventReleased, domain.BeadStateOpen},

		// The round cap and the PO's ruling (D-08, agents.EscalationBeadState).
		{domain.BeadStateInReview, BeadEventEscalated, domain.BeadStateEscalated},
		{domain.BeadStateChangesRequested, BeadEventEscalated, domain.BeadStateEscalated},
		{domain.BeadStateEscalated, BeadEventScopeNarrowed, domain.BeadStateInReview},
		{domain.BeadStateEscalated, BeadEventApprovedWithNotes, domain.BeadStateApproved},

		// Delivery going sideways (D-10, P6-06, P6-16).
		{domain.BeadStateDelivering, BeadEventConflict, domain.BeadStateInProgress},
		{domain.BeadStateDelivering, BeadEventCheckFailed, domain.BeadStateChangesRequested},
		{domain.BeadStateNeedsHuman, BeadEventDeliveryRetried, domain.BeadStateDelivering},
	}
	table := make(map[transitionKey]domain.BeadState, len(edges)+2*len(domain.WorkflowBeadStates()))
	for _, e := range edges {
		table[transitionKey{e.From, e.Event}] = e.To
	}
	// Every state that still has work in it can be handed to a human or
	// abandoned. The terminal states (closed, needs_human, failed) cannot:
	// they are already where those exits lead.
	for _, s := range domain.WorkflowBeadStates() {
		if s.Terminal() {
			continue
		}
		table[transitionKey{s, BeadEventBlocked}] = domain.BeadStateNeedsHuman
		table[transitionKey{s, BeadEventFailed}] = domain.BeadStateFailed
	}
	return table
}

// Transition is the pure D-07 state machine: given the state a bead is in and
// something that happened to it, it returns the state the bead moves to.
//
// It is a total function over its inputs — every combination either returns a
// state or an error, and nothing panics:
//
//   - a state outside [domain.WorkflowBeadStates] (the empty state, an
//     unknown value, or one of bd's read-only built-ins blocked, hooked,
//     deferred and pinned) is [ErrNotWorkflowState]; the orchestrator leaves
//     such beads alone (P3-14, P6-12);
//   - an event outside [BeadEvents] is [ErrUnknownBeadEvent];
//   - a known state and a known event with no edge between them is
//     [ErrInvalidTransition].
//
// Transition never reports a no-op: a self-edge does not exist in the table,
// so re-applying an event that already happened is an invalid transition the
// caller can detect rather than a silent success.
func Transition(from domain.BeadState, ev BeadEvent) (domain.BeadState, error) {
	if !from.Workflow() {
		return "", fmt.Errorf("%w: %q (want one of %s)", ErrNotWorkflowState, string(from), joinBeadStates(domain.WorkflowBeadStates()))
	}
	if !ev.Valid() {
		return "", fmt.Errorf("%w: %q (want one of %s)", ErrUnknownBeadEvent, string(ev), joinBeadEvents(beadEvents))
	}
	to, ok := beadTransitionTable[transitionKey{from, ev}]
	if !ok {
		return "", fmt.Errorf("%w: %s cannot handle %q (it accepts %s)", ErrInvalidTransition, string(from), string(ev), joinBeadEvents(EventsFrom(from)))
	}
	return to, nil
}

// CanTransition reports whether [Transition] would succeed.
func CanTransition(from domain.BeadState, ev BeadEvent) bool {
	_, err := Transition(from, ev)
	return err == nil
}

// TransitionStatus is [Transition] for a raw bd status read off a listing.
// It resolves the status through [beads.SafeState] with the repository's
// [beads.StatusMap] (a nil map is fine; foreign statuses then take the
// unknown-category mapping) and refuses anything the cannon does not own:
//
//   - a status of the repository's own making is [ErrForeignStatus], even
//     when SafeState maps it onto a workflow state — a foreign "done" status
//     becomes closed, and closing it again is not the cannon's business
//     (P6-12);
//   - one of bd's read-only built-ins is [ErrNotWorkflowState].
//
// Otherwise it returns the same answer [Transition] would for the resolved
// state.
func TransitionStatus(status string, m beads.StatusMap, ev BeadEvent) (domain.BeadState, error) {
	state, origin := beads.SafeState(status, m[status])
	if origin == beads.OriginForeign {
		return "", fmt.Errorf("%w: %q (mapped to %s by category %q)", ErrForeignStatus, status, string(state), string(m[status]))
	}
	return Transition(state, ev)
}

// EventsFrom returns the events the given state accepts, in [BeadEvents]
// order. A state outside the workflow accepts none, so the result is empty.
func EventsFrom(from domain.BeadState) []BeadEvent {
	if !from.Workflow() {
		return nil
	}
	var out []BeadEvent
	for _, ev := range beadEvents {
		if _, ok := beadTransitionTable[transitionKey{from, ev}]; ok {
			out = append(out, ev)
		}
	}
	return out
}

// BeadTransitions returns every edge of the table, sorted by the state's
// position in the D-07 flow and then by the event's position in [BeadEvents].
// It exists so documentation and tests can render the machine without
// reaching into package internals.
func BeadTransitions() []BeadTransition {
	states := domain.WorkflowBeadStates()
	rank := make(map[domain.BeadState]int, len(states))
	for i, s := range states {
		rank[s] = i
	}
	evRank := make(map[BeadEvent]int, len(beadEvents))
	for i, e := range beadEvents {
		evRank[e] = i
	}
	out := make([]BeadTransition, 0, len(beadTransitionTable))
	for k, to := range beadTransitionTable {
		out = append(out, BeadTransition{From: k.from, Event: k.event, To: to})
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].From] != rank[out[j].From] {
			return rank[out[i].From] < rank[out[j].From]
		}
		return evRank[out[i].Event] < evRank[out[j].Event]
	})
	return out
}

// StateStore is the slice of *beads.Client the state machine writes through
// (P3-03, P3-05). Every method matches the client's signature, so
// *beads.Client satisfies it; tests use a fake.
type StateStore interface {
	// SetStatus moves a bead to a workflow status other than closed.
	SetStatus(ctx context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error)
	// ClaimFor atomically assigns the bead and sets in_progress.
	ClaimFor(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
	// Reclaim moves a bead that came back from review to in_progress for a
	// (possibly different) agent, which bd's atomic claim cannot do.
	Reclaim(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
	// Release returns a bead to the ready set and drops the lease.
	Release(ctx context.Context, id domain.BeadID) (beads.Issue, error)
	// Close closes the bead with a reason.
	Close(ctx context.Context, id domain.BeadID, reason string) (beads.Issue, error)
}

// *beads.Client is the production StateStore.
var _ StateStore = (*beads.Client)(nil)

// StateWarningSource is the events.Warning Source the state machine stamps on
// a refused transition.
const StateWarningSource = "state"

// StateMachine applies [Transition] and persists the answer through
// internal/beads. It holds no bead state of its own: the caller says where
// the bead is and the machine says, and writes, where it goes. That keeps
// beads the single source of truth (D-06) and makes the machine safe to use
// from the scheduler, the feedback loop and the delivery handoff at once —
// the serialisation that matters lives in the beads client.
type StateMachine struct {
	store StateStore
	sink  Publisher
	run   domain.RunID
	now   func() time.Time
}

// StateMachineOption configures a [StateMachine].
type StateMachineOption func(*StateMachine)

// WithStatePublisher sets where the machine publishes BeadStateChanged and
// Warning events. With no sink the events are dropped and the transitions
// still happen.
func WithStatePublisher(s Publisher) StateMachineOption {
	return func(m *StateMachine) { m.sink = s }
}

// WithStateRun stamps the run id on every event the machine publishes.
func WithStateRun(run domain.RunID) StateMachineOption {
	return func(m *StateMachine) { m.run = run }
}

// WithStateClock replaces time.Now, so tests can run under a fake clock and
// events carry a deterministic timestamp.
func WithStateClock(now func() time.Time) StateMachineOption {
	return func(m *StateMachine) {
		if now != nil {
			m.now = now
		}
	}
}

// NewStateMachine returns a machine writing through store. A nil store is
// allowed and makes [StateMachine.Apply] report the transition without
// persisting it, which is what a dry run wants.
func NewStateMachine(store StateStore, opts ...StateMachineOption) *StateMachine {
	m := &StateMachine{store: store, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

// StateRequest is one transition to apply.
type StateRequest struct {
	// Bead is the bead to move. Required.
	Bead domain.BeadID
	// From is the state the bead is in, as the caller last saw it. Required:
	// the machine does not read bd to find out, so the caller decides how
	// fresh its view is.
	From domain.BeadState
	// Event is what happened. Required.
	Event BeadEvent
	// Agent is the agent taking the bead over. Required for the two
	// transitions that claim (BeadEventClaimed, BeadEventReworkStarted) and
	// for the conflict round, which also hands the bead to an engineer;
	// ignored otherwise.
	Agent domain.AgentID
	// Reason is a few words for the event and, when the bead closes, for
	// `bd close --reason`.
	Reason string
	// Round is the review round the bead is in, recorded on the event.
	Round int
}

// StateResult reports what a transition did.
type StateResult struct {
	// From and To are the ends of the edge that was taken.
	From domain.BeadState
	// To is the state the bead now holds.
	To domain.BeadState
	// Issue is the bead as bd returned it, zero when the machine had no
	// store.
	Issue beads.Issue
	// Persisted says whether bd was written to.
	Persisted bool
}

// Next reports where the request would take the bead without touching bd or
// publishing anything. It is [Transition] plus the request's own validation,
// so a caller can test the water with the exact rules Apply will use.
func (m *StateMachine) Next(req StateRequest) (domain.BeadState, error) {
	if req.Bead == "" {
		return "", fmt.Errorf("%w: empty bead id", ErrInvalidTransition)
	}
	to, err := Transition(req.From, req.Event)
	if err != nil {
		return "", fmt.Errorf("bead %s: %w", req.Bead, err)
	}
	if claimsBead(req.Event) && req.Agent == "" {
		return "", fmt.Errorf("bead %s: %w: %s", req.Bead, ErrNoAgent, req.Event)
	}
	return to, nil
}

// Apply runs the transition and writes it to bd.
//
// A refused transition (an unknown or non-workflow state, an unknown event,
// no edge, a missing bead id or a claim with no agent) returns the error and
// publishes an [events.Warning] with Source [StateWarningSource]; nothing is
// written and nothing panics. That is the contract P6-03 asks for: the
// orchestrator can drive the machine from whatever it believes about a bead
// and find out it was wrong without taking the run down.
//
// On success the write goes through the call the destination deserves:
//
//	in_progress via BeadEventClaimed        → ClaimFor (atomic claim + lease)
//	in_progress via rework or conflict      → Reclaim  (bd's claim refuses these)
//	open        via BeadEventReleased       → Release  (drops assignee + lease)
//	closed                                  → Close    (records the reason)
//	everything else                         → SetStatus
//
// and an [events.BeadStateChanged] is published afterwards. A bd failure is
// returned to the caller unwrapped of any extra meaning (errors.Is still
// finds beads.ErrNotClaimable and friends) and publishes no event: the bead
// did not move, and whether that is worth a warning is the caller's call.
func (m *StateMachine) Apply(ctx context.Context, req StateRequest) (StateResult, error) {
	to, err := m.Next(req)
	if err != nil {
		m.warn(req, err)
		return StateResult{From: req.From}, err
	}
	res := StateResult{From: req.From, To: to}
	if m.store != nil {
		issue, err := m.persist(ctx, req, to)
		if err != nil {
			return StateResult{From: req.From, To: to}, err
		}
		res.Issue = issue
		res.Persisted = true
	}
	m.publish(events.BeadStateChanged{
		Meta:   m.meta(),
		Bead:   req.Bead,
		From:   req.From,
		To:     to,
		Round:  req.Round,
		Reason: req.Reason,
	})
	return res, nil
}

// persist writes the new state with the call that fits it.
func (m *StateMachine) persist(ctx context.Context, req StateRequest, to domain.BeadState) (beads.Issue, error) {
	switch {
	case req.Event == BeadEventClaimed:
		return m.store.ClaimFor(ctx, req.Bead, req.Agent)
	case claimsBead(req.Event):
		return m.store.Reclaim(ctx, req.Bead, req.Agent)
	case req.Event == BeadEventReleased:
		return m.store.Release(ctx, req.Bead)
	case to == domain.BeadStateClosed:
		return m.store.Close(ctx, req.Bead, req.Reason)
	default:
		return m.store.SetStatus(ctx, req.Bead, to)
	}
}

// claimsBead reports whether the event hands the bead to an agent, so the
// write must claim rather than merely set the status and the request must
// name that agent.
func claimsBead(ev BeadEvent) bool {
	switch ev {
	case BeadEventClaimed, BeadEventReworkStarted, BeadEventConflict:
		return true
	default:
		return false
	}
}

// ----------------------------------------------------- the shutdown release

// RoleHoldsBeadClaim reports whether an agent in this role owns the bd claim
// on the bead it is working on.
//
// Only the engineer does: the scheduler claims a bead for a DemandImplement
// firing (ClaimNew or ClaimRework) and fires testers and the product owner
// with ClaimNone, because a review or a ruling reads a bead, it does not take
// it (D-05, P6-13). The distinction matters at shutdown: a live agent's bead
// may only be handed back by the agent that holds it, and the D-07 edge that
// hands it back — in_progress --released--> open — is the engineer's.
func RoleHoldsBeadClaim(r domain.Role) bool { return r == domain.RoleEngineer }

// HeldBead is a bead one of a run's live agents still holds when the run
// shuts down, with the state bd has that bead in.
type HeldBead struct {
	// Bead is the bead the agent was working on.
	Bead domain.BeadID
	// Agent is the agent holding it.
	Agent domain.AgentID
	// Role is that agent's role; see [RoleHoldsBeadClaim].
	Role domain.Role
	// From is the state bd has the bead in, empty when the caller could not
	// find out. [StateMachine.ReleaseHeld] then falls back to in_progress,
	// which is what the claim itself set.
	From domain.BeadState
}

// HeldBeads works out which beads a shutting-down run actually holds: one
// entry per bead, for the live agents whose role holds the claim
// ([RoleHoldsBeadClaim]), stamped with the state the bead listing has that
// bead in.
//
// It is deliberately narrow. A tester reviewing an in_review bead and a
// product owner ruling on an escalated one are registered as live agents with
// the bead set, but they hold no claim, and releasing their bead would take
// an edge D-07 does not have — driving a reviewed bead with a committed diff
// back into `bd ready` to be implemented from scratch on the next resume.
//
// states may be nil (or miss the bead): the entry then carries no From.
func HeldBeads(agents []domain.Agent, states []domain.Bead) []HeldBead {
	if len(agents) == 0 {
		return nil
	}
	known := make(map[domain.BeadID]domain.BeadState, len(states))
	for _, b := range states {
		known[b.ID] = b.State
	}
	seen := make(map[domain.BeadID]bool, len(agents))
	var out []HeldBead
	for _, a := range agents {
		if a.Bead == "" || seen[a.Bead] || !RoleHoldsBeadClaim(a.Role) {
			continue
		}
		seen[a.Bead] = true
		out = append(out, HeldBead{Bead: a.Bead, Agent: a.ID, Role: a.Role, From: known[a.Bead]})
	}
	return out
}

// ReleaseOutcome is what [StateMachine.ReleaseHeld] did with one held bead.
type ReleaseOutcome struct {
	// HeldBead is the bead the outcome is about, with the state the release
	// judged it from.
	HeldBead
	// Released says the bead went back to the ready set: bd was written and
	// an events.BeadStateChanged published.
	Released bool
	// Kept says, in a few words, why the bead was left where it was; empty
	// when it was released.
	Kept string
	// Err is the bd failure that stopped the release, if any.
	Err error
}

// ReleaseHeld hands the beads a shutting-down run holds back to the ready set
// along the one D-07 edge that exists for it, in_progress --released--> open
// (P6-01's graceful stop, P3-05's lease).
//
// It is the release side of the guarantee the claim side already keeps: every
// status write goes through the table, and every status write that happens is
// published. A bead whose state has no released edge — an in_review bead the
// testers were still judging, an escalated bead the product owner was ruling
// on, a delivering bead mid-handoff — is kept, with the reason, and nothing is
// written for it: bd itself would take an update to open with the assignee
// cleared quite happily, so the table is the only thing standing between a
// stop and a reviewed bead losing its round.
//
// A bead whose From is empty is treated as in_progress, which is what the
// claim that made this agent live set atomically.
//
// The outcomes come back in the order they were given, one per distinct bead,
// so the caller can log or warn about each; nothing here logs or warns
// itself. A bd failure is reported in [ReleaseOutcome.Err] and does not stop
// the other releases.
func (m *StateMachine) ReleaseHeld(ctx context.Context, held []HeldBead, reason string) []ReleaseOutcome {
	var out []ReleaseOutcome
	done := make(map[domain.BeadID]bool, len(held))
	for _, h := range held {
		if h.Bead == "" || done[h.Bead] {
			continue
		}
		done[h.Bead] = true
		if h.From == "" {
			h.From = domain.BeadStateInProgress
		}
		if !CanTransition(h.From, BeadEventReleased) {
			out = append(out, ReleaseOutcome{HeldBead: h,
				Kept: fmt.Sprintf("a bead in %q cannot be released (D-07)", string(h.From))})
			continue
		}
		if _, err := m.Apply(ctx, StateRequest{
			Bead: h.Bead, From: h.From, Event: BeadEventReleased, Reason: reason,
		}); err != nil {
			out = append(out, ReleaseOutcome{HeldBead: h, Err: err})
			continue
		}
		out = append(out, ReleaseOutcome{HeldBead: h, Released: true})
	}
	return out
}

// ErrReleaseOnlyStore is returned by [ReleaseOnlyStore] for every write other
// than a release. Reaching it is a bug: the only event that store is ever
// asked for is BeadEventReleased.
var ErrReleaseOnlyStore = errors.New("orchestrator: this state store can only release beads")

// ReleaseOnlyStore adapts a bare lease releaser — all a shutting-down run is
// given ([Options.Leases]) — to [StateStore], so the shutdown release can go
// through the state machine rather than writing a status behind its back.
// *beads.Client satisfies both, so nothing is lost in production.
type ReleaseOnlyStore struct {
	// Releaser performs the release; a nil one makes every call an error.
	Releaser LeaseReleaser
}

// ReleaseOnlyStore is a StateStore.
var _ StateStore = ReleaseOnlyStore{}

// Release returns the bead to the ready set.
func (s ReleaseOnlyStore) Release(ctx context.Context, id domain.BeadID) (beads.Issue, error) {
	if s.Releaser == nil {
		return beads.Issue{}, fmt.Errorf("%w: no releaser", ErrReleaseOnlyStore)
	}
	return s.Releaser.Release(ctx, id)
}

// SetStatus is not available on a release-only store.
func (s ReleaseOnlyStore) SetStatus(_ context.Context, id domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("%w: set %s to %q", ErrReleaseOnlyStore, id, string(st))
}

// ClaimFor is not available on a release-only store.
func (s ReleaseOnlyStore) ClaimFor(_ context.Context, id domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("%w: claim %s for %s", ErrReleaseOnlyStore, id, a)
}

// Reclaim is not available on a release-only store.
func (s ReleaseOnlyStore) Reclaim(_ context.Context, id domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("%w: reclaim %s for %s", ErrReleaseOnlyStore, id, a)
}

// Close is not available on a release-only store.
func (s ReleaseOnlyStore) Close(_ context.Context, id domain.BeadID, _ string) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("%w: close %s", ErrReleaseOnlyStore, id)
}

// warn publishes the Warning that a refused transition owes a human.
func (m *StateMachine) warn(req StateRequest, err error) {
	m.publish(events.Warning{
		Meta:    m.meta(),
		Source:  StateWarningSource,
		Message: fmt.Sprintf("ignored %q on a bead in state %q", string(req.Event), string(req.From)),
		Err:     err.Error(),
		Bead:    req.Bead,
		Agent:   req.Agent,
	})
}

// publish sends ev to the sink, if there is one.
func (m *StateMachine) publish(ev events.Event) {
	if m.sink != nil {
		m.sink.Publish(ev)
	}
}

// meta stamps an event with the machine's run and clock.
func (m *StateMachine) meta() events.Meta { return events.MetaAt(m.run, m.now()) }

// joinBeadEvents renders events for error messages.
func joinBeadEvents(evs []BeadEvent) string {
	if len(evs) == 0 {
		return "none"
	}
	parts := make([]string, len(evs))
	for i, e := range evs {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}

// joinBeadStates renders states for error messages.
func joinBeadStates(states []domain.BeadState) string {
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}
