package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// stateClock is the fixed instant every state test stamps its events with, so
// no test reads the wall clock.
var stateClock = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// stateNow is the fake clock passed to WithStateClock.
func stateNow() time.Time { return stateClock }

// stateWantEdges is the D-07 transition table written out by hand, edge by
// edge, independently of the production table in state.go: state → event →
// destination. Every (state, event) pair that is not in here must be refused.
// Keep it in flow order so it reads as the specification it is.
var stateWantEdges = map[domain.BeadState]map[BeadEvent]domain.BeadState{
	domain.BeadStateOpen: {
		BeadEventClaimed: domain.BeadStateInProgress,
		BeadEventBlocked: domain.BeadStateNeedsHuman,
		BeadEventFailed:  domain.BeadStateFailed,
	},
	domain.BeadStateInProgress: {
		BeadEventSubmitted: domain.BeadStateInReview,
		BeadEventReleased:  domain.BeadStateOpen,
		BeadEventBlocked:   domain.BeadStateNeedsHuman,
		BeadEventFailed:    domain.BeadStateFailed,
	},
	domain.BeadStateInReview: {
		BeadEventApproved:         domain.BeadStateApproved,
		BeadEventChangesRequested: domain.BeadStateChangesRequested,
		BeadEventEscalated:        domain.BeadStateEscalated,
		BeadEventBlocked:          domain.BeadStateNeedsHuman,
		BeadEventFailed:           domain.BeadStateFailed,
	},
	domain.BeadStateChangesRequested: {
		BeadEventReworkStarted: domain.BeadStateInProgress,
		BeadEventEscalated:     domain.BeadStateEscalated,
		BeadEventBlocked:       domain.BeadStateNeedsHuman,
		BeadEventFailed:        domain.BeadStateFailed,
	},
	domain.BeadStateApproved: {
		BeadEventDeliveryStarted: domain.BeadStateDelivering,
		BeadEventBlocked:         domain.BeadStateNeedsHuman,
		BeadEventFailed:          domain.BeadStateFailed,
	},
	domain.BeadStateDelivering: {
		BeadEventDelivered:   domain.BeadStateClosed,
		BeadEventConflict:    domain.BeadStateInProgress,
		BeadEventCheckFailed: domain.BeadStateChangesRequested,
		BeadEventBlocked:     domain.BeadStateNeedsHuman,
		BeadEventFailed:      domain.BeadStateFailed,
	},
	domain.BeadStateEscalated: {
		BeadEventScopeNarrowed:     domain.BeadStateInReview,
		BeadEventApprovedWithNotes: domain.BeadStateApproved,
		BeadEventBlocked:           domain.BeadStateNeedsHuman,
		BeadEventFailed:            domain.BeadStateFailed,
	},
	domain.BeadStateNeedsHuman: {
		BeadEventDeliveryRetried: domain.BeadStateDelivering,
	},
	// closed and failed are terminal: no edge leaves them.
	domain.BeadStateClosed: {},
	domain.BeadStateFailed: {},
}

// stateNonWorkflowStates are the states the cannon only ever reads: bd's own
// built-ins (D-07) and, through beads.SafeState, the target repository's
// custom statuses (P3-14, P6-12).
var stateNonWorkflowStates = []domain.BeadState{
	domain.BeadStateBlocked, domain.BeadStateHooked,
	domain.BeadStateDeferred, domain.BeadStatePinned,
}

// stateBogusStates are values that are not bd statuses at all.
var stateBogusStates = []domain.BeadState{"", " ", "OPEN", "in progress", "review", "🙂"}

// stateBogusEvents are values that are not BeadEvents.
var stateBogusEvents = []BeadEvent{"", " ", "CLAIMED", "claim", "merged", "🙂"}

// TestStateTransitionEveryStateEventPair is the P6-03 acceptance test: it
// enumerates every (state, event) pair over every state domain.BeadStates
// knows plus a handful of nonsense values, and every BeadEvent plus a handful
// of nonsense triggers, and checks each answer against stateWantEdges. Nothing
// is skipped and nothing may panic.
func TestStateTransitionEveryStateEventPair(t *testing.T) {
	t.Parallel()

	states := append(append([]domain.BeadState(nil), domain.BeadStates()...), stateBogusStates...)
	evs := append(append([]BeadEvent(nil), BeadEvents()...), stateBogusEvents...)
	if got, want := len(states), len(domain.BeadStates())+len(stateBogusStates); got != want {
		t.Fatalf("state list length = %d, want %d", got, want)
	}

	pairs := 0
	for _, from := range states {
		for _, ev := range evs {
			pairs++
			name := fmt.Sprintf("%s/%s", stateLabel(from), stateLabel(domain.BeadState(ev)))
			t.Run(name, func(t *testing.T) {
				to, err := Transition(from, ev)
				switch {
				case !from.Workflow():
					// Every non-workflow state, valid bd built-in or not,
					// refuses every event before the event is even looked at.
					requireStateErr(t, err, ErrNotWorkflowState)
					if to != "" {
						t.Fatalf("Transition(%q, %q) = %q, want no state", from, ev, to)
					}
				case !ev.Valid():
					requireStateErr(t, err, ErrUnknownBeadEvent)
				default:
					want, ok := stateWantEdges[from][ev]
					if !ok {
						requireStateErr(t, err, ErrInvalidTransition)
						return
					}
					if err != nil {
						t.Fatalf("Transition(%q, %q) = error %v, want %q", from, ev, err, want)
					}
					if to != want {
						t.Fatalf("Transition(%q, %q) = %q, want %q", from, ev, to, want)
					}
				}
				if got := CanTransition(from, ev); got != (err == nil) {
					t.Fatalf("CanTransition(%q, %q) = %v, but Transition error = %v", from, ev, got, err)
				}
			})
		}
	}
	if want := len(states) * len(evs); pairs != want {
		t.Fatalf("enumerated %d pairs, want %d", pairs, want)
	}
	t.Logf("enumerated %d (state, event) pairs", pairs)
}

// TestStateTransitionTableMatchesTheSpec checks the production table holds
// exactly the edges stateWantEdges lists — so an edge added to state.go
// without a line in the spec above fails, not just a missing one.
func TestStateTransitionTableMatchesTheSpec(t *testing.T) {
	t.Parallel()

	got := map[string]domain.BeadState{}
	for _, tr := range BeadTransitions() {
		key := string(tr.From) + "/" + string(tr.Event)
		if prev, dup := got[key]; dup {
			t.Fatalf("duplicate edge %s: %q and %q", key, prev, tr.To)
		}
		got[key] = tr.To
	}
	want := map[string]domain.BeadState{}
	for from, m := range stateWantEdges {
		for ev, to := range m {
			want[string(from)+"/"+string(ev)] = to
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("table edges = %v\nwant %v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("table is empty")
	}
}

// TestStateTransitionsAreOrderedAndWellFormed pins the reporting helpers:
// BeadTransitions is sorted by the D-07 flow, every destination is itself a
// workflow state, and EventsFrom agrees with the table.
func TestStateTransitionsAreOrderedAndWellFormed(t *testing.T) {
	t.Parallel()

	rank := map[domain.BeadState]int{}
	for i, s := range domain.WorkflowBeadStates() {
		rank[s] = i
	}
	evRank := map[BeadEvent]int{}
	for i, e := range BeadEvents() {
		evRank[e] = i
	}
	trs := BeadTransitions()
	for i, tr := range trs {
		if !tr.From.Workflow() {
			t.Fatalf("edge %d starts at non-workflow state %q", i, tr.From)
		}
		if !tr.To.Workflow() {
			t.Fatalf("edge %d ends at non-workflow state %q", i, tr.To)
		}
		if !tr.Event.Valid() {
			t.Fatalf("edge %d uses unknown event %q", i, tr.Event)
		}
		if tr.From == tr.To {
			t.Fatalf("edge %d is a self-loop on %q", i, tr.From)
		}
		if i == 0 {
			continue
		}
		prev := trs[i-1]
		if rank[prev.From] > rank[tr.From] || (prev.From == tr.From && evRank[prev.Event] >= evRank[tr.Event]) {
			t.Fatalf("edges %d and %d are out of order: %v then %v", i-1, i, prev, tr)
		}
	}

	for _, s := range append(append([]domain.BeadState(nil), domain.BeadStates()...), stateBogusStates...) {
		want := []BeadEvent(nil)
		for _, ev := range BeadEvents() {
			if _, ok := stateWantEdges[s][ev]; ok {
				want = append(want, ev)
			}
		}
		if got := EventsFrom(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("EventsFrom(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestStateHappyPathReachesEveryWorkflowState walks the D-07 flow the way the
// orchestrator will and ends on closed, then checks every workflow state is
// reachable from open, so no state in the vocabulary is stranded.
func TestStateHappyPathReachesEveryWorkflowState(t *testing.T) {
	t.Parallel()

	steps := []struct {
		ev   BeadEvent
		want domain.BeadState
	}{
		{BeadEventClaimed, domain.BeadStateInProgress},
		{BeadEventSubmitted, domain.BeadStateInReview},
		{BeadEventChangesRequested, domain.BeadStateChangesRequested},
		{BeadEventReworkStarted, domain.BeadStateInProgress},
		{BeadEventSubmitted, domain.BeadStateInReview},
		{BeadEventApproved, domain.BeadStateApproved},
		{BeadEventDeliveryStarted, domain.BeadStateDelivering},
		{BeadEventDelivered, domain.BeadStateClosed},
	}
	state := domain.BeadStateOpen
	for i, s := range steps {
		next, err := Transition(state, s.ev)
		if err != nil {
			t.Fatalf("step %d %q from %q: %v", i, s.ev, state, err)
		}
		if next != s.want {
			t.Fatalf("step %d %q from %q = %q, want %q", i, s.ev, state, next, s.want)
		}
		state = next
	}

	seen := map[domain.BeadState]bool{domain.BeadStateOpen: true}
	queue := []domain.BeadState{domain.BeadStateOpen}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ev := range EventsFrom(cur) {
			to, err := Transition(cur, ev)
			if err != nil {
				t.Fatalf("Transition(%q, %q): %v", cur, ev, err)
			}
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	for _, s := range domain.WorkflowBeadStates() {
		if !seen[s] {
			t.Fatalf("workflow state %q is unreachable from open", s)
		}
	}
	if len(seen) != len(domain.WorkflowBeadStates()) {
		t.Fatalf("reached %d states, want the %d workflow states", len(seen), len(domain.WorkflowBeadStates()))
	}
}

// TestStateRefusesBDBuiltIns is the first half of P6-12: bd's read-only
// built-ins are never transitioned, whatever happens to their bead.
func TestStateRefusesBDBuiltIns(t *testing.T) {
	t.Parallel()

	for _, s := range stateNonWorkflowStates {
		if !s.Valid() {
			t.Fatalf("%q should still be a valid bd status", s)
		}
		if s.Workflow() {
			t.Fatalf("%q should not be a workflow state", s)
		}
		for _, ev := range BeadEvents() {
			if _, err := Transition(s, ev); !errors.Is(err, ErrNotWorkflowState) {
				t.Fatalf("Transition(%q, %q) error = %v, want ErrNotWorkflowState", s, ev, err)
			}
		}
		if got := EventsFrom(s); got != nil {
			t.Fatalf("EventsFrom(%q) = %v, want none", s, got)
		}
	}
}

// TestStateRefusesForeignMappedStatuses is the second half of P6-12: every
// beads.SafeState output for a status the repository defined itself is
// refused, including the "done" mapping that lands on closed — a workflow
// state the table knows, but not one the cannon may act on for a bead it does
// not own.
func TestStateRefusesForeignMappedStatuses(t *testing.T) {
	t.Parallel()

	// One foreign status per bd category, plus one the repository's status
	// list does not mention at all (unknown category).
	cases := []struct {
		status    string
		category  domain.StateCategory
		wantState domain.BeadState
		wantErr   error
	}{
		{"triage", domain.CategoryActive, domain.BeadStatePinned, ErrNotWorkflowState},
		{"code_review", domain.CategoryWIP, domain.BeadStateBlocked, ErrNotWorkflowState},
		{"icebox", domain.CategoryFrozen, domain.BeadStateDeferred, ErrNotWorkflowState},
		{"shipped", domain.CategoryDone, domain.BeadStateClosed, ErrInvalidTransition},
		{"whatever", "", domain.BeadStateDeferred, ErrNotWorkflowState},
	}
	m := beads.StatusMap{}
	for _, c := range cases {
		if c.category != "" {
			m[c.status] = c.category
		}
	}

	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			state, origin := beads.SafeState(c.status, c.category)
			if origin != beads.OriginForeign {
				t.Fatalf("SafeState(%q, %q) origin = %v, want foreign", c.status, c.category, origin)
			}
			if state != c.wantState {
				t.Fatalf("SafeState(%q, %q) = %q, want %q", c.status, c.category, state, c.wantState)
			}
			for _, ev := range BeadEvents() {
				// TransitionStatus refuses on the origin alone, so even the
				// done → closed mapping is ErrForeignStatus and not a
				// transition the cannon might accidentally allow.
				if _, err := TransitionStatus(c.status, m, ev); !errors.Is(err, ErrForeignStatus) {
					t.Fatalf("TransitionStatus(%q, %q) error = %v, want ErrForeignStatus", c.status, ev, err)
				}
				// And the mapped state on its own is never transitionable.
				if _, err := Transition(state, ev); !errors.Is(err, c.wantErr) {
					t.Fatalf("Transition(%q, %q) error = %v, want %v", state, ev, err, c.wantErr)
				}
			}
		})
	}
}

// TestStateTransitionStatusAcceptsOwnStatuses checks TransitionStatus is a
// plain pass-through for the statuses slop-cannon does own, and still refuses
// bd's built-ins by state rather than by origin.
func TestStateTransitionStatusAcceptsOwnStatuses(t *testing.T) {
	t.Parallel()

	m := beads.StatusMap{
		"open":        domain.CategoryActive,
		"in_progress": domain.CategoryWIP,
		"in_review":   domain.CategoryWIP,
		"blocked":     domain.CategoryWIP,
	}
	to, err := TransitionStatus("open", m, BeadEventClaimed)
	if err != nil || to != domain.BeadStateInProgress {
		t.Fatalf("TransitionStatus(open, claimed) = %q, %v; want in_progress, nil", to, err)
	}
	if _, err := TransitionStatus("in_review", nil, BeadEventApproved); err != nil {
		t.Fatalf("TransitionStatus with a nil map: %v", err)
	}
	if _, err := TransitionStatus("blocked", m, BeadEventClaimed); !errors.Is(err, ErrNotWorkflowState) {
		t.Fatalf("TransitionStatus(blocked, claimed) error = %v, want ErrNotWorkflowState", err)
	}
	if _, err := TransitionStatus("open", m, "nonsense"); !errors.Is(err, ErrUnknownBeadEvent) {
		t.Fatalf("TransitionStatus with a bogus event error = %v, want ErrUnknownBeadEvent", err)
	}
}

// TestStateBeadEventVocabulary pins the event enum: the list, String, Valid
// and the lenient parser.
func TestStateBeadEventVocabulary(t *testing.T) {
	t.Parallel()

	evs := BeadEvents()
	if len(evs) == 0 {
		t.Fatal("no bead events")
	}
	evs[0] = "clobbered"
	if BeadEvents()[0] == "clobbered" {
		t.Fatal("BeadEvents returns the package's own slice")
	}
	seen := map[BeadEvent]bool{}
	for _, ev := range BeadEvents() {
		if seen[ev] {
			t.Fatalf("duplicate event %q", ev)
		}
		seen[ev] = true
		if ev.String() != string(ev) {
			t.Fatalf("String of %q = %q", ev, ev.String())
		}
		if !ev.Valid() {
			t.Fatalf("%q is not Valid", ev)
		}
		for _, in := range []string{string(ev), " " + string(ev) + " ", strings.ToUpper(string(ev))} {
			got, err := ParseBeadEvent(in)
			if err != nil || got != ev {
				t.Fatalf("ParseBeadEvent(%q) = %q, %v; want %q", in, got, err, ev)
			}
		}
	}
	for _, bad := range stateBogusEvents {
		// Valid is exact, so "CLAIMED" is not a BeadEvent even though the
		// lenient parser accepts the spelling.
		if bad.Valid() {
			t.Fatalf("%q should not be Valid", bad)
		}
		if BeadEvent(strings.ToLower(strings.TrimSpace(string(bad)))).Valid() {
			continue
		}
		if _, err := ParseBeadEvent(string(bad)); !errors.Is(err, ErrUnknownBeadEvent) {
			t.Fatalf("ParseBeadEvent(%q) error = %v, want ErrUnknownBeadEvent", bad, err)
		}
	}
}

// stateCall records one write the fake store received.
type stateCall struct {
	// Op is the method name ("SetStatus", "ClaimFor", "Reclaim", "Release",
	// "Close").
	Op string
	// Bead is the bead written.
	Bead domain.BeadID
	// State is the status SetStatus was given, empty for the other calls.
	State domain.BeadState
	// Agent is the agent a claim was made for.
	Agent domain.AgentID
	// Reason is the close reason.
	Reason string
}

// stateFakeStore is a StateStore that records calls and can fail on demand.
type stateFakeStore struct {
	calls []stateCall
	err   error
}

func (s *stateFakeStore) record(c stateCall) (beads.Issue, error) {
	s.calls = append(s.calls, c)
	if s.err != nil {
		return beads.Issue{}, s.err
	}
	return beads.Issue{ID: c.Bead, Status: string(c.State)}, nil
}

func (s *stateFakeStore) SetStatus(_ context.Context, id domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	return s.record(stateCall{Op: "SetStatus", Bead: id, State: st})
}

func (s *stateFakeStore) ClaimFor(_ context.Context, id domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record(stateCall{Op: "ClaimFor", Bead: id, State: domain.BeadStateInProgress, Agent: a})
}

func (s *stateFakeStore) Reclaim(_ context.Context, id domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record(stateCall{Op: "Reclaim", Bead: id, State: domain.BeadStateInProgress, Agent: a})
}

func (s *stateFakeStore) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	return s.record(stateCall{Op: "Release", Bead: id, State: domain.BeadStateOpen})
}

func (s *stateFakeStore) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	return s.record(stateCall{Op: "Close", Bead: id, State: domain.BeadStateClosed, Reason: reason})
}

// stateRecorder collects published events.
type stateRecorder struct{ evs []events.Event }

func (r *stateRecorder) Publish(ev events.Event) { r.evs = append(r.evs, ev) }

// kinds lists the kinds recorded, for assertions.
func (r *stateRecorder) kinds() []events.Kind {
	out := make([]events.Kind, len(r.evs))
	for i, ev := range r.evs {
		out[i] = ev.Kind()
	}
	return out
}

// newStateHarness builds a machine over a fake store and recorder.
func newStateHarness(t *testing.T) (*StateMachine, *stateFakeStore, *stateRecorder) {
	t.Helper()
	store := &stateFakeStore{}
	rec := &stateRecorder{}
	m := NewStateMachine(store,
		WithStatePublisher(rec),
		WithStateRun("run-1"),
		WithStateClock(stateNow),
	)
	return m, store, rec
}

// TestStateMachinePersistsEveryEdgeThroughTheRightCall drives every edge of
// the table through Apply and checks which beads call it made: claims go
// through ClaimFor/Reclaim, a release through Release, a close through Close
// and everything else through SetStatus (P3-03, P3-05, P6-13).
func TestStateMachinePersistsEveryEdgeThroughTheRightCall(t *testing.T) {
	t.Parallel()

	wantOp := func(ev BeadEvent, to domain.BeadState) string {
		switch {
		case ev == BeadEventClaimed:
			return "ClaimFor"
		case ev == BeadEventReworkStarted, ev == BeadEventConflict:
			return "Reclaim"
		case ev == BeadEventReleased:
			return "Release"
		case to == domain.BeadStateClosed:
			return "Close"
		default:
			return "SetStatus"
		}
	}

	for _, tr := range BeadTransitions() {
		t.Run(string(tr.From)+"/"+string(tr.Event), func(t *testing.T) {
			m, store, rec := newStateHarness(t)
			req := StateRequest{
				Bead:   "sc-1",
				From:   tr.From,
				Event:  tr.Event,
				Agent:  "engineer-1",
				Reason: "because",
				Round:  2,
			}
			next, err := m.Next(req)
			if err != nil || next != tr.To {
				t.Fatalf("Next = %q, %v; want %q", next, err, tr.To)
			}
			res, err := m.Apply(context.Background(), req)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if res.From != tr.From || res.To != tr.To || !res.Persisted {
				t.Fatalf("result = %+v, want %q → %q persisted", res, tr.From, tr.To)
			}
			if len(store.calls) != 1 {
				t.Fatalf("store calls = %+v, want exactly one", store.calls)
			}
			call := store.calls[0]
			if got, want := call.Op, wantOp(tr.Event, tr.To); got != want {
				t.Fatalf("wrote with %s, want %s", got, want)
			}
			if call.Bead != "sc-1" {
				t.Fatalf("wrote bead %q", call.Bead)
			}
			switch call.Op {
			case "SetStatus":
				if call.State != tr.To {
					t.Fatalf("SetStatus(%q), want %q", call.State, tr.To)
				}
				if tr.To == domain.BeadStateClosed || !tr.To.Workflow() {
					t.Fatalf("SetStatus must never be handed %q", tr.To)
				}
			case "ClaimFor", "Reclaim":
				if call.Agent != "engineer-1" {
					t.Fatalf("claimed for %q", call.Agent)
				}
			case "Close":
				if call.Reason != "because" {
					t.Fatalf("close reason = %q", call.Reason)
				}
			}
			if got := rec.kinds(); !reflect.DeepEqual(got, []events.Kind{events.KindBeadStateChanged}) {
				t.Fatalf("events = %v, want one bead_state_changed", got)
			}
			ev, ok := rec.evs[0].(events.BeadStateChanged)
			if !ok {
				t.Fatalf("event type %T", rec.evs[0])
			}
			want := events.BeadStateChanged{
				Meta:   events.MetaAt("run-1", stateClock),
				Bead:   "sc-1",
				From:   tr.From,
				To:     tr.To,
				Round:  2,
				Reason: "because",
			}
			if !reflect.DeepEqual(ev, want) {
				t.Fatalf("event = %+v\nwant %+v", ev, want)
			}
		})
	}
}

// TestStateMachineRefusesInvalidTransitions checks a refused Apply writes
// nothing, returns the error and publishes exactly one Warning naming the
// state machine — never a panic (P6-03).
func TestStateMachineRefusesInvalidTransitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  StateRequest
		want error
	}{
		{
			name: "no such edge",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateOpen, Event: BeadEventDelivered},
			want: ErrInvalidTransition,
		},
		{
			name: "terminal bead",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateClosed, Event: BeadEventClaimed, Agent: "e1"},
			want: ErrInvalidTransition,
		},
		{
			name: "bd built-in",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateDeferred, Event: BeadEventClaimed, Agent: "e1"},
			want: ErrNotWorkflowState,
		},
		{
			name: "foreign status mapped to blocked",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateBlocked, Event: BeadEventSubmitted},
			want: ErrNotWorkflowState,
		},
		{
			name: "empty state",
			req:  StateRequest{Bead: "sc-1", Event: BeadEventClaimed, Agent: "e1"},
			want: ErrNotWorkflowState,
		},
		{
			name: "unknown event",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateOpen, Event: "teleported"},
			want: ErrUnknownBeadEvent,
		},
		{
			name: "no bead id",
			req:  StateRequest{From: domain.BeadStateOpen, Event: BeadEventClaimed, Agent: "e1"},
			want: ErrInvalidTransition,
		},
		{
			name: "claim without an agent",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateOpen, Event: BeadEventClaimed},
			want: ErrNoAgent,
		},
		{
			name: "rework without an agent",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateChangesRequested, Event: BeadEventReworkStarted},
			want: ErrNoAgent,
		},
		{
			name: "conflict round without an agent",
			req:  StateRequest{Bead: "sc-1", From: domain.BeadStateDelivering, Event: BeadEventConflict},
			want: ErrNoAgent,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, store, rec := newStateHarness(t)
			if _, err := m.Next(c.req); !errors.Is(err, c.want) {
				t.Fatalf("Next error = %v, want %v", err, c.want)
			}
			res, err := m.Apply(context.Background(), c.req)
			if !errors.Is(err, c.want) {
				t.Fatalf("Apply error = %v, want %v", err, c.want)
			}
			if res.To != "" || res.Persisted {
				t.Fatalf("result = %+v, want nothing done", res)
			}
			if len(store.calls) != 0 {
				t.Fatalf("bd was written to: %+v", store.calls)
			}
			if got := rec.kinds(); !reflect.DeepEqual(got, []events.Kind{events.KindWarning}) {
				t.Fatalf("events = %v, want one warning", got)
			}
			w, ok := rec.evs[0].(events.Warning)
			if !ok {
				t.Fatalf("event type %T", rec.evs[0])
			}
			if w.Source != StateWarningSource {
				t.Fatalf("warning source = %q, want %q", w.Source, StateWarningSource)
			}
			if w.Bead != c.req.Bead || w.Agent != c.req.Agent {
				t.Fatalf("warning names bead %q agent %q, want %q/%q", w.Bead, w.Agent, c.req.Bead, c.req.Agent)
			}
			if !strings.Contains(w.Err, c.want.Error()) {
				t.Fatalf("warning error %q does not mention %q", w.Err, c.want)
			}
			if w.Time != stateClock || w.Run != "run-1" {
				t.Fatalf("warning meta = %+v", w.Meta)
			}
		})
	}
}

// TestStateMachineSurfacesStoreFailures checks a bd failure comes back to the
// caller intact and publishes nothing: the bead did not move.
func TestStateMachineSurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	m, store, rec := newStateHarness(t)
	store.err = fmt.Errorf("beads: claim sc-1: %w", beads.ErrAlreadyClaimed)
	res, err := m.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateOpen, Event: BeadEventClaimed, Agent: "e1",
	})
	if !errors.Is(err, beads.ErrAlreadyClaimed) {
		t.Fatalf("error = %v, want beads.ErrAlreadyClaimed", err)
	}
	if res.Persisted {
		t.Fatalf("result claims it persisted: %+v", res)
	}
	if res.From != domain.BeadStateOpen || res.To != domain.BeadStateInProgress {
		t.Fatalf("result should still report the attempted edge: %+v", res)
	}
	if len(rec.evs) != 0 {
		t.Fatalf("published %v on a failed write", rec.kinds())
	}
	if len(store.calls) != 1 {
		t.Fatalf("store calls = %+v", store.calls)
	}
}

// TestStateMachineWithoutAStoreIsADryRun checks a nil store reports the
// transition and publishes it without writing anything, and that a machine
// with no sink still works.
func TestStateMachineWithoutAStoreIsADryRun(t *testing.T) {
	t.Parallel()

	rec := &stateRecorder{}
	m := NewStateMachine(nil, WithStatePublisher(rec), WithStateClock(stateNow))
	res, err := m.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateInReview, Event: BeadEventApproved,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Persisted || res.To != domain.BeadStateApproved {
		t.Fatalf("result = %+v", res)
	}
	if res.Issue.ID != "" || res.Issue.Status != "" {
		t.Fatalf("issue = %+v, want the zero value", res.Issue)
	}
	if got := rec.kinds(); !reflect.DeepEqual(got, []events.Kind{events.KindBeadStateChanged}) {
		t.Fatalf("events = %v", got)
	}

	silent := NewStateMachine(&stateFakeStore{})
	if _, err := silent.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateApproved, Event: BeadEventDeliveryStarted,
	}); err != nil {
		t.Fatalf("Apply without a sink: %v", err)
	}
	if _, err := silent.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateApproved, Event: BeadEventDelivered,
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("refusal without a sink = %v", err)
	}
}

// TestStateMachineOptionsAreDefensive checks the clock option ignores nil and
// that a machine built with no options still stamps a usable event.
func TestStateMachineOptionsAreDefensive(t *testing.T) {
	t.Parallel()

	rec := &stateRecorder{}
	m := NewStateMachine(&stateFakeStore{}, WithStatePublisher(rec), WithStateClock(nil))
	before := time.Now()
	if _, err := m.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateOpen, Event: BeadEventClaimed, Agent: "e1",
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev, ok := rec.evs[0].(events.BeadStateChanged)
	if !ok {
		t.Fatalf("event type %T", rec.evs[0])
	}
	if ev.Time.Before(before.Add(-time.Minute)) || ev.Time.After(time.Now().Add(time.Minute)) {
		t.Fatalf("nil clock did not fall back to time.Now: %v", ev.Time)
	}
	if ev.Run != "" {
		t.Fatalf("run = %q, want empty without WithStateRun", ev.Run)
	}
}

// TestStatePublisherFuncAdaptsAFunction covers the function adapter.
func TestStatePublisherFuncAdaptsAFunction(t *testing.T) {
	t.Parallel()

	var got []events.Kind
	sink := PublisherFunc(func(ev events.Event) { got = append(got, ev.Kind()) })
	m := NewStateMachine(nil, WithStatePublisher(sink), WithStateClock(stateNow))
	if _, err := m.Apply(context.Background(), StateRequest{
		Bead: "sc-1", From: domain.BeadStateEscalated, Event: BeadEventApprovedWithNotes,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(got, []events.Kind{events.KindBeadStateChanged}) {
		t.Fatalf("kinds = %v", got)
	}
}

// TestStateMachineAgreesWithTheAgentsEscalationMapping keeps the escalation
// edges in step with agents.EscalationBeadState (P5-05) without importing it:
// the outcomes it names must be exactly what the escalated state accepts.
func TestStateMachineAgreesWithTheAgentsEscalationMapping(t *testing.T) {
	t.Parallel()

	// agents.EscalationBeadState: approve_with_notes → approved,
	// narrow_scope → in_review, block → needs_human.
	want := map[BeadEvent]domain.BeadState{
		BeadEventApprovedWithNotes: domain.BeadStateApproved,
		BeadEventScopeNarrowed:     domain.BeadStateInReview,
		BeadEventBlocked:           domain.BeadStateNeedsHuman,
	}
	for ev, to := range want {
		got, err := Transition(domain.BeadStateEscalated, ev)
		if err != nil || got != to {
			t.Fatalf("Transition(escalated, %q) = %q, %v; want %q", ev, got, err, to)
		}
	}
	// Every escalation outcome domain knows must have an edge here.
	if n := len(domain.EscalationOutcomes()); n != len(want) {
		t.Fatalf("domain has %d escalation outcomes but the table covers %d", n, len(want))
	}
}

// TestStateErrorMessagesNameTheAlternatives keeps the error text useful: a
// refused transition says what the state does accept, and the state and event
// errors list the vocabulary.
func TestStateErrorMessagesNameTheAlternatives(t *testing.T) {
	t.Parallel()

	_, err := Transition(domain.BeadStateOpen, BeadEventDelivered)
	msg := err.Error()
	for _, want := range []string{"open", "delivered", "claimed", "blocked", "failed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("%q does not mention %q", msg, want)
		}
	}
	_, err = Transition(domain.BeadStateClosed, BeadEventClaimed)
	if !strings.Contains(err.Error(), "none") {
		t.Fatalf("a terminal state should say it accepts none: %q", err)
	}
	_, err = Transition("nope", BeadEventClaimed)
	if !strings.Contains(err.Error(), "in_review") {
		t.Fatalf("%q should list the workflow states", err)
	}
	_, err = Transition(domain.BeadStateOpen, "nope")
	if !strings.Contains(err.Error(), "rework_started") {
		t.Fatalf("%q should list the events", err)
	}
	if got := joinBeadEvents(nil); got != "none" {
		t.Fatalf("joinBeadEvents(nil) = %q", got)
	}
}

// TestStateWorkflowStatesAreCoveredByTheTable checks every workflow state
// appears in the hand-written spec, so a state added to domain without an
// entry here fails rather than silently having no edges.
func TestStateWorkflowStatesAreCoveredByTheTable(t *testing.T) {
	t.Parallel()

	var missing []string
	for _, s := range domain.WorkflowBeadStates() {
		if _, ok := stateWantEdges[s]; !ok {
			missing = append(missing, string(s))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("workflow states with no spec entry: %s", strings.Join(missing, ", "))
	}
	for s := range stateWantEdges {
		if !s.Workflow() {
			t.Fatalf("spec has a non-workflow state %q", s)
		}
	}
}

// requireStateErr fails unless err wraps want.
func requireStateErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// stateLabel renders a state (or an event cast to one) for a subtest name,
// keeping empty and space-only values readable.
func stateLabel(s domain.BeadState) string {
	switch string(s) {
	case "":
		return "empty"
	case " ":
		return "space"
	}
	return strings.ReplaceAll(string(s), " ", "_")
}
