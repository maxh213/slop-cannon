package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
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

// TestStateResolveBeadStatusIsTheOneP612Guard pins ResolveBeadStatus, the
// single implementation of the P6-12 rule that both TransitionStatus and the
// gate poller (gates.go, which reads a bead before it knows which edge the
// gate implies) go through, so the guard is exercised in production and is
// not open-coded twice. It answers with SafeState's state whatever it
// decides, so a caller can name the state in its own diagnostic.
func TestStateResolveBeadStatusIsTheOneP612Guard(t *testing.T) {
	t.Parallel()

	m := beads.StatusMap{
		"open":        domain.CategoryActive,
		"in_progress": domain.CategoryWIP,
		"in_review":   domain.CategoryWIP,
		"blocked":     domain.CategoryWIP,
		"shipped":     domain.CategoryDone, // the repository's own "done"
		"reviewing":   domain.CategoryWIP,  // the repository's own WIP
	}
	cases := []struct {
		name      string
		status    string
		wantState domain.BeadState
		wantErr   error
	}{
		{"own status", "open", domain.BeadStateOpen, nil},
		{"own wip status", "in_review", domain.BeadStateInReview, nil},
		{"bd built-in", "blocked", domain.BeadStateBlocked, ErrNotWorkflowState},
		{"foreign done maps onto closed", "shipped", domain.BeadStateClosed, ErrForeignStatus},
		{"foreign wip", "reviewing", domain.BeadStateBlocked, ErrForeignStatus},
		{"unknown to the repository too", "whatever", domain.BeadStateDeferred, ErrForeignStatus},
		// An issue with no status at all is not the cannon's either: it
		// takes the unknown-category mapping and is refused on origin.
		{"no status at all", "", domain.BeadStateDeferred, ErrForeignStatus},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			state, err := ResolveBeadStatus(c.status, m)
			if state != c.wantState {
				t.Fatalf("ResolveBeadStatus(%q) state = %q, want %q", c.status, state, c.wantState)
			}
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("ResolveBeadStatus(%q) error = %v, want nil", c.status, err)
				}
			} else if !errors.Is(err, c.wantErr) {
				t.Fatalf("ResolveBeadStatus(%q) error = %v, want %v", c.status, err, c.wantErr)
			}
			// TransitionStatus is exactly this guard plus Transition: it
			// reports the same refusal, and never a state, when refused.
			to, terr := TransitionStatus(c.status, m, BeadEventClaimed)
			if c.wantErr != nil {
				if !errors.Is(terr, c.wantErr) {
					t.Fatalf("TransitionStatus(%q) error = %v, want %v", c.status, terr, c.wantErr)
				}
				if to != "" {
					t.Fatalf("TransitionStatus(%q) = %q on a refusal, want the empty state", c.status, to)
				}
				return
			}
			want, wantErr := Transition(c.wantState, BeadEventClaimed)
			if to != want {
				t.Fatalf("TransitionStatus(%q) = %q; want Transition's %q", c.status, to, want)
			}
			// Both errors are freshly wrapped sentinels, so they are
			// compared by what they say rather than by identity.
			if fmt.Sprint(terr) != fmt.Sprint(wantErr) {
				t.Fatalf("TransitionStatus(%q) error = %v; want Transition's %v", c.status, terr, wantErr)
			}
		})
	}

	// A nil map is the "no registered statuses" case: the cannon's own names
	// still resolve, because SafeState knows them.
	if state, err := ResolveBeadStatus("in_review", nil); err != nil || state != domain.BeadStateInReview {
		t.Fatalf("ResolveBeadStatus(in_review, nil) = %q, %v; want in_review, nil", state, err)
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
	}
	for _, bad := range stateBogusEvents {
		// Valid is exact: neither a spelling with different case nor one
		// with space around it is a BeadEvent.
		if bad.Valid() {
			t.Fatalf("%q should not be Valid", bad)
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
			if w.Source != SourceState {
				t.Fatalf("warning source = %q, want %q", w.Source, SourceState)
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

// ----------------------------------------------------- the shutdown release

// stateReleaser is a LeaseReleaser that records the beads it released and
// can fail on demand.
type stateReleaser struct {
	got []domain.BeadID
	err error
}

func (r *stateReleaser) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	r.got = append(r.got, id)
	if r.err != nil {
		return beads.Issue{}, r.err
	}
	return beads.Issue{ID: id, Status: string(domain.BeadStateOpen)}, nil
}

// TestStateHeldBeadsKeepsOnlyTheClaimHolders pins which of a shutting-down
// run's live agents actually hold a bead: the engineer does, because the
// scheduler claimed the bead for it; the testers reviewing it and the product
// owner ruling on it do not (ClaimNone). One entry per bead, in agent order,
// stamped with the state the listing has.
func TestStateHeldBeadsKeepsOnlyTheClaimHolders(t *testing.T) {
	t.Parallel()

	for _, r := range domain.Roles() {
		if got, want := RoleHoldsBeadClaim(r), r == domain.RoleEngineer; got != want {
			t.Fatalf("RoleHoldsBeadClaim(%q) = %v, want %v", r, got, want)
		}
	}
	if RoleHoldsBeadClaim("") {
		t.Fatal("an agent with no role holds no claim")
	}

	agents := []domain.Agent{
		{ID: "po-1", Role: domain.RolePO},                                        // no bead at all
		{ID: "ux-1", Role: domain.RoleTester, Lens: domain.LensUX, Bead: "sc-1"}, // reviewing
		{ID: "eng-1", Role: domain.RoleEngineer, Bead: "sc-1"},                   // holds sc-1
		{ID: "eng-1b", Role: domain.RoleEngineer, Bead: "sc-1"},                  // duplicate bead
		{ID: "eng-2", Role: domain.RoleEngineer, Bead: "sc-2"},                   // holds sc-2, state unknown
		{ID: "po-2", Role: domain.RolePO, Bead: "sc-3"},                          // escalating
		{ID: "nobody", Bead: "sc-4"},                                             // no role: no claim
	}
	states := []domain.Bead{
		{ID: "sc-1", State: domain.BeadStateInProgress},
		{ID: "sc-3", State: domain.BeadStateEscalated},
		{ID: "sc-4", State: domain.BeadStateInProgress},
	}
	want := []HeldBead{
		{Bead: "sc-1", Agent: "eng-1", Role: domain.RoleEngineer, From: domain.BeadStateInProgress},
		{Bead: "sc-2", Agent: "eng-2", Role: domain.RoleEngineer},
	}
	if got := HeldBeads(agents, states); !reflect.DeepEqual(got, want) {
		t.Fatalf("HeldBeads = %+v, want %+v", got, want)
	}
	if got := HeldBeads(agents, nil); len(got) != 2 || got[0].From != "" {
		t.Fatalf("with no listing the states are unknown, got %+v", got)
	}
	if got := HeldBeads(nil, states); got != nil {
		t.Fatalf("HeldBeads(nil, …) = %+v, want nil", got)
	}
}

// TestStateReleaseHeldTakesOnlyTheD07Edge is the release side of "every
// status write goes through the table": a stop hands back the beads that have
// an in_progress --released--> open edge and publishes each one, and leaves
// every other bead exactly where it was, writing nothing to bd.
func TestStateReleaseHeldTakesOnlyTheD07Edge(t *testing.T) {
	t.Parallel()

	m, store, rec := newStateHarness(t)
	held := []HeldBead{
		{Bead: "sc-1", Agent: "eng-1", Role: domain.RoleEngineer, From: domain.BeadStateInProgress},
		{Bead: "sc-2", Agent: "eng-2", Role: domain.RoleEngineer, From: domain.BeadStateInReview},
		{Bead: "sc-3", Agent: "eng-3", Role: domain.RoleEngineer, From: domain.BeadStateEscalated},
		{Bead: "sc-4", Agent: "eng-4", Role: domain.RoleEngineer, From: domain.BeadStateDelivering},
		{Bead: "sc-5", Agent: "eng-5", Role: domain.RoleEngineer, From: domain.BeadStateBlocked},
		{Bead: "sc-6", Agent: "eng-6", Role: domain.RoleEngineer}, // unknown: the claim's own state
		{Bead: "sc-1", Agent: "eng-1b", Role: domain.RoleEngineer, From: domain.BeadStateInProgress},
		{Bead: "", Agent: "eng-7", Role: domain.RoleEngineer},
	}

	out := m.ReleaseHeld(context.Background(), held, "the run stopped")
	if len(out) != 6 {
		t.Fatalf("got %d outcomes, want one per distinct bead: %+v", len(out), out)
	}
	wantReleased := map[domain.BeadID]bool{"sc-1": true, "sc-6": true}
	for _, o := range out {
		if o.Err != nil {
			t.Fatalf("%s: unexpected error %v", o.Bead, o.Err)
		}
		if o.Released != wantReleased[o.Bead] {
			t.Fatalf("%s: released = %v, want %v", o.Bead, o.Released, wantReleased[o.Bead])
		}
		if o.Released == (o.Kept != "") {
			t.Fatalf("%s: exactly one of Released and Kept must be set: %+v", o.Bead, o)
		}
		if !o.Released && !strings.Contains(o.Kept, string(o.From)) {
			t.Fatalf("%s: the reason %q should name the state", o.Bead, o.Kept)
		}
	}
	if out[5].From != domain.BeadStateInProgress {
		t.Fatalf("an unknown state falls back to in_progress, got %q", out[5].From)
	}

	wantCalls := []stateCall{
		{Op: "Release", Bead: "sc-1", State: domain.BeadStateOpen},
		{Op: "Release", Bead: "sc-6", State: domain.BeadStateOpen},
	}
	if !reflect.DeepEqual(store.calls, wantCalls) {
		t.Fatalf("bd calls = %+v, want %+v", store.calls, wantCalls)
	}

	var moved []events.BeadStateChanged
	for _, ev := range rec.evs {
		switch e := ev.(type) {
		case events.BeadStateChanged:
			moved = append(moved, e)
		default:
			t.Fatalf("a kept bead published %s: %+v", ev.Kind(), ev)
		}
	}
	if len(moved) != 2 {
		t.Fatalf("published %d state changes, want 2: %+v", len(moved), moved)
	}
	for _, e := range moved {
		if e.From != domain.BeadStateInProgress || e.To != domain.BeadStateOpen {
			t.Fatalf("%s: published %s → %s", e.Bead, e.From, e.To)
		}
		if e.Reason != "the run stopped" || e.At() != stateClock {
			t.Fatalf("%s: reason %q at %s", e.Bead, e.Reason, e.At())
		}
	}
}

// TestStateReleaseHeldReportsBDFailures checks a bd failure is reported per
// bead and does not stop the rest of the shutdown.
func TestStateReleaseHeldReportsBDFailures(t *testing.T) {
	t.Parallel()

	m, store, rec := newStateHarness(t)
	store.err = fmt.Errorf("bd is busy")
	out := m.ReleaseHeld(context.Background(), []HeldBead{
		{Bead: "sc-1", Agent: "eng-1", Role: domain.RoleEngineer, From: domain.BeadStateInProgress},
		{Bead: "sc-2", Agent: "eng-2", Role: domain.RoleEngineer, From: domain.BeadStateInProgress},
	}, "the run stopped")
	if len(out) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(out))
	}
	for _, o := range out {
		if o.Released || o.Err == nil || !strings.Contains(o.Err.Error(), "bd is busy") {
			t.Fatalf("%s: %+v", o.Bead, o)
		}
	}
	if len(rec.evs) != 0 {
		t.Fatalf("a failed release publishes nothing, got %v", rec.kinds())
	}
	if m.ReleaseHeld(context.Background(), nil, "") != nil {
		t.Fatal("nothing held, nothing to do")
	}
}

// TestStateReleaseOnlyStoreOnlyReleases pins the adapter the run lifecycle
// wraps its lease releaser in: the release reaches bd, and every other write
// is refused rather than silently taking an edge behind the table's back.
func TestStateReleaseOnlyStoreOnlyReleases(t *testing.T) {
	t.Parallel()

	rel := &stateReleaser{}
	store := ReleaseOnlyStore{Releaser: rel}
	ctx := context.Background()

	issue, err := store.Release(ctx, "sc-1")
	if err != nil || issue.ID != "sc-1" {
		t.Fatalf("Release = %+v, %v", issue, err)
	}
	if !reflect.DeepEqual(rel.got, []domain.BeadID{"sc-1"}) {
		t.Fatalf("released %v", rel.got)
	}

	refused := map[string]error{}
	_, refused["SetStatus"] = store.SetStatus(ctx, "sc-1", domain.BeadStateInReview)
	_, refused["ClaimFor"] = store.ClaimFor(ctx, "sc-1", "eng-1")
	_, refused["Reclaim"] = store.Reclaim(ctx, "sc-1", "eng-1")
	_, refused["Close"] = store.Close(ctx, "sc-1", "done")
	for op, err := range refused {
		requireStateErr(t, err, ErrReleaseOnlyStore)
		if !strings.Contains(err.Error(), "sc-1") {
			t.Fatalf("%s: %q should name the bead", op, err)
		}
	}
	if _, err := (ReleaseOnlyStore{}).Release(ctx, "sc-1"); !errors.Is(err, ErrReleaseOnlyStore) {
		t.Fatalf("a store with no releaser = %v", err)
	}

	// The machine's own release goes through it and nothing else does.
	m := NewStateMachine(ReleaseOnlyStore{Releaser: rel}, WithStateClock(stateNow))
	if _, err := m.Apply(ctx, StateRequest{Bead: "sc-2", From: domain.BeadStateInProgress,
		Event: BeadEventReleased}); err != nil {
		t.Fatalf("release through the machine: %v", err)
	}
	if !reflect.DeepEqual(rel.got, []domain.BeadID{"sc-1", "sc-2"}) {
		t.Fatalf("released %v", rel.got)
	}
}

// TestStateTransitionCountIsTheOneTheRecordClaims pins the size of the D-07
// table: 15 flow edges, plus a blocked and a failed exit for each of the
// seven non-terminal workflow states, is 29. The progress note for P6-03 said
// 24 until this test was written; a number in prose that nothing checks is a
// number that drifts.
func TestStateTransitionCountIsTheOneTheRecordClaims(t *testing.T) {
	t.Parallel()

	trs := BeadTransitions()
	var flow, exits, nonTerminal int
	for _, tr := range trs {
		if tr.Event == BeadEventBlocked || tr.Event == BeadEventFailed {
			exits++
			continue
		}
		flow++
	}
	for _, s := range domain.WorkflowBeadStates() {
		if !s.Terminal() {
			nonTerminal++
		}
	}
	if nonTerminal != 7 {
		t.Fatalf("non-terminal workflow states = %d, want 7", nonTerminal)
	}
	if flow != 15 || exits != 2*nonTerminal || len(trs) != 29 {
		t.Fatalf("table = %d edges (%d flow, %d exits), want 29 (15 flow, %d exits)",
			len(trs), flow, exits, 2*nonTerminal)
	}
	var spec int
	for _, evs := range stateWantEdges {
		spec += len(evs)
	}
	if spec != len(trs) {
		t.Fatalf("the hand-written spec has %d edges, the table %d", spec, len(trs))
	}
}

// ------------------------------------------- P6-03: no event without a write

// TestStateResumeAnnouncesOnlyTheTransitionsBdAccepted is the resume half of
// the invariant the rest of this file pins for StateMachine.Apply: a bd write
// that failed is a warning and no event. gitx reconciles branch state into
// bead state on --resume and writes it through its own tracker seam, marking
// each entry Applied only when that write landed; an entry that reconciled but
// could not be written must not publish events.BeadStateChanged, because
// Summarise folds the event into the bead's state and nothing ever re-reads —
// the run log, state.json, `sc status` and the TUI would show a state bd
// disagrees with for the rest of the run.
func TestStateResumeAnnouncesOnlyTheTransitionsBdAccepted(t *testing.T) {
	f := lifecycleSetup(t)
	landed := domain.Bead{ID: "sc-1", State: domain.BeadStateDelivering}
	refused := domain.Bead{ID: "sc-3", State: domain.BeadStateInReview}
	list := []domain.Bead{landed, refused}

	opts := f.options()
	opts.Resumer = &lifecycleResumer{res: &gitx.ResumeResult{
		Default: "main",
		Entries: []gitx.ResumeEntry{
			// bd took the close.
			{Bead: landed, State: domain.BeadStateClosed, Applied: true,
				Reason: "the branch landed on main"},
			// bd refused it: Summary() prints "(not applied)".
			{Bead: refused, State: domain.BeadStateClosed,
				Err:    fmt.Errorf("applying closed: bd is busy"),
				Reason: "the branch landed on main"},
		},
	}}
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return list, nil }

	rc := f.runConfig(t, config.RunSettings{})
	rc.Resume = true
	r, err := New(rc, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	all := f.drain()
	var changed []events.BeadStateChanged
	var warned bool
	for _, ev := range all {
		switch e := ev.(type) {
		case events.BeadStateChanged:
			changed = append(changed, e)
		case events.Warning:
			if strings.Contains(e.Message, string(refused.ID)) {
				warned = true
			}
		}
	}
	if len(changed) != 1 {
		t.Fatalf("BeadStateChanged events = %d (%+v), want only the applied one", len(changed), changed)
	}
	if changed[0].Bead != landed.ID || changed[0].From != domain.BeadStateDelivering || changed[0].To != domain.BeadStateClosed {
		t.Fatalf("announced %+v, want sc-1 delivering -> closed", changed[0])
	}
	if !warned {
		t.Fatalf("the entry bd refused was not warned about")
	}

	// And the fold `sc status`, state.json's view and the TUI all read is
	// left saying nothing about the bead bd refused, rather than saying
	// closed (P6-08).
	sum := Summarise(all, -1)
	for _, b := range sum.Beads {
		if b.ID == refused.ID && b.State != "" {
			t.Fatalf("the folded view has %s as %q; bd still has it %s",
				b.ID, b.State, refused.State)
		}
	}

	if err := r.Stop("done"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := r.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// ------------------------------------- P6-28: the beads the run passes over

// foreignRecorder is a thread-safe event sink, because a
// ForeignStatusReporter is shared by every component of a run and its tests
// drive it from several goroutines.
type foreignRecorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *foreignRecorder) Publish(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

// warnings returns the Warning events recorded, in order.
func (r *foreignRecorder) warnings() []events.Warning {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Warning
	for _, ev := range r.evs {
		if w, ok := ev.(events.Warning); ok {
			out = append(out, w)
		}
	}
	return out
}

// all returns everything recorded.
func (r *foreignRecorder) all() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.evs...)
}

// newForeignHarness builds a reporter over a recorder and a fixed clock.
func newForeignHarness(t *testing.T, tweak func(*ForeignStatusOptions)) (*ForeignStatusReporter, *foreignRecorder) {
	t.Helper()
	rec := &foreignRecorder{}
	opts := ForeignStatusOptions{
		Sink:     rec,
		Run:      "run-1",
		Statuses: beads.StatusMap{"reviewing": domain.CategoryWIP},
		Clock:    stateNow,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&opts)
	}
	return NewForeignStatusReporter(opts), rec
}

// P6-28: a bead in a status slop-cannon did not register is reported once,
// however many times the run meets it, and the report is a Warning a reader
// can find by its Source rather than by its wording.
func TestForeignStatusReporterReportsEachBeadOnce(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	state, first, err := r.Resolve("sc-1", "reviewing")
	require.ErrorIs(t, err, ErrForeignStatus)
	assert.True(t, first, "the first sighting is news")
	assert.Equal(t, domain.BeadStateBlocked, state, "the state is still SafeState's answer")

	for i := 0; i < 5; i++ {
		_, again, err := r.Resolve("sc-1", "reviewing")
		require.ErrorIs(t, err, ErrForeignStatus)
		assert.False(t, again, "the same bead in the same status is not news again")
	}

	warns := rec.warnings()
	require.Len(t, warns, 1, "one Warning per bead, not one per sighting")
	assert.Equal(t, SourceForeignStatus, warns[0].Source)
	assert.Equal(t, domain.BeadID("sc-1"), warns[0].Bead)
	assert.Equal(t, stateClock, warns[0].At())
	assert.Equal(t, domain.RunID("run-1"), warns[0].RunID())
	assert.Contains(t, warns[0].Message, `carries the status "reviewing"`)
	assert.Contains(t, warns[0].Message, "leaving it alone")

	assert.Equal(t, 1, r.Count())
	require.Len(t, r.Beads(), 1)
	assert.Equal(t, ForeignStatus{
		Bead: "sc-1", Status: "reviewing", State: domain.BeadStateBlocked, At: stateClock,
	}, r.Beads()[0])
}

// One of bd's own read-only states is reported too, in its own words: the
// repository did not invent it, so "did not register" would be a lie.
func TestForeignStatusReporterReportsBDBuiltIns(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	state, first, err := r.Resolve("sc-2", string(domain.BeadStateDeferred))

	require.ErrorIs(t, err, ErrNotWorkflowState)
	assert.True(t, first)
	assert.Equal(t, domain.BeadStateDeferred, state)
	require.Len(t, rec.warnings(), 1)
	assert.Contains(t, rec.warnings()[0].Message, "sc-2 is deferred, which slop-cannon only reads")
	require.Len(t, r.Beads(), 1)
	assert.True(t, r.Beads()[0].BuiltIn, "a bd built-in is not a status the repository invented")
}

// A bead the cannon owns is silent: the guard's whole point is that it is
// not news when a run may act on its own bead.
func TestForeignStatusReporterIsSilentAboutOwnBeads(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	state, first, err := r.Resolve("sc-3", string(domain.BeadStateInReview))

	require.NoError(t, err)
	assert.False(t, first)
	assert.Equal(t, domain.BeadStateInReview, state)
	assert.Empty(t, rec.all())
	assert.Zero(t, r.Count())
	assert.Empty(t, r.Beads())
}

// A bead that moves from one foreign status to another is news again: the
// key is the bead *and* the status, so a repository that shuffles a bead
// through its own workflow is reported each time, while a poll that meets
// the same bead unchanged is not.
func TestForeignStatusReporterReportsAChangedStatusAgain(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, func(o *ForeignStatusOptions) {
		o.Statuses = beads.StatusMap{"reviewing": domain.CategoryWIP, "parked": domain.CategoryFrozen}
	})

	_, _, err := r.Resolve("sc-1", "reviewing")
	require.Error(t, err)
	_, first, err := r.Resolve("sc-1", "parked")
	require.Error(t, err)

	assert.True(t, first)
	assert.Len(t, rec.warnings(), 2)
	assert.Equal(t, 2, r.Count())
	beadList := r.Beads()
	require.Len(t, beadList, 2)
	assert.Equal(t, "parked", beadList[0].Status, "Beads is ordered by bead and then status")
	assert.Equal(t, "reviewing", beadList[1].Status)
}

// Observe is the one call a bd listing needs: it reports every bead in it
// the run may not act on and answers how many of them were news.
func TestForeignStatusReporterObservesAListing(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	issues := []beads.Issue{
		{ID: "sc-1", Status: "reviewing"},
		{ID: "sc-2", Status: string(domain.BeadStateInProgress)},
		{ID: "sc-3", Status: string(domain.BeadStateBlocked)},
		{ID: "", Status: "reviewing"},
	}

	assert.Equal(t, 2, r.Observe(issues...), "the two beads the cannon may not touch")
	assert.Equal(t, 0, r.Observe(issues...), "and nothing is news the second time round")
	assert.Equal(t, 2, r.Count())
	assert.Len(t, rec.warnings(), 2)

	var got []domain.BeadID
	for _, f := range r.Beads() {
		got = append(got, f.Bead)
	}
	assert.Equal(t, []domain.BeadID{"sc-1", "sc-3"}, got, "a bead with no id is not a report")
}

// The once-only bookkeeping is bounded, like the gate poller's: an
// autopilot run never ends, so the map may not grow with the repository.
// The count stays honest across the reset.
func TestForeignStatusReporterBookkeepingIsBounded(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, func(o *ForeignStatusOptions) { o.Limit = 4 })

	for i := 0; i < 10; i++ {
		_, first, err := r.Resolve(domain.BeadID(fmt.Sprintf("sc-%d", i)), "reviewing")
		require.Error(t, err)
		assert.True(t, first)
	}

	assert.Equal(t, 10, r.Count(), "the count is of beads reported, not of beads remembered")
	assert.Len(t, rec.warnings(), 10)
	assert.LessOrEqual(t, len(r.Beads()), 4, "the list is bounded by the limit")

	// A bead reported before the reset may be reported once more; a bead
	// reported since is still remembered.
	_, first, err := r.Resolve("sc-9", "reviewing")
	require.Error(t, err)
	assert.False(t, first)
}

// A nil reporter is usable: a component with none configured resolves the
// status and reports nothing, rather than panicking.
func TestForeignStatusReporterNilIsUsable(t *testing.T) {
	t.Parallel()
	var r *ForeignStatusReporter

	state, first, err := r.Resolve("sc-1", string(domain.BeadStateApproved))
	require.NoError(t, err)
	assert.Equal(t, domain.BeadStateApproved, state)
	assert.False(t, first)

	_, _, err = r.Resolve("sc-2", "reviewing")
	require.ErrorIs(t, err, ErrForeignStatus)
	assert.Zero(t, r.Count())
	assert.Empty(t, r.Beads())
	assert.Zero(t, r.Observe(beads.Issue{ID: "sc-2", Status: "reviewing"}))
}

// A reporter with a host publishes through it and takes the run's own
// header, so the report lands in the run log with every other event.
func TestForeignStatusReporterPublishesThroughItsHost(t *testing.T) {
	t.Parallel()
	host := &foreignHost{meta: events.MetaAt("host-run", stateClock.Add(time.Minute))}
	r := NewForeignStatusReporter(ForeignStatusOptions{Host: host, Sink: &foreignRecorder{}, Run: "run-1", Clock: stateNow})

	_, _, err := r.Resolve("sc-1", "reviewing")

	require.Error(t, err)
	require.Len(t, host.evs, 1, "the host is the sink when there is one")
	w, ok := host.evs[0].(events.Warning)
	require.True(t, ok)
	assert.Equal(t, domain.RunID("host-run"), w.RunID())
	assert.Equal(t, stateClock.Add(time.Minute), w.At())
}

// foreignHost is an EventHost that records what it is given.
type foreignHost struct {
	mu   sync.Mutex
	meta events.Meta
	evs  []events.Event
}

func (h *foreignHost) Publish(ev events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evs = append(h.evs, ev)
}
func (h *foreignHost) Meta() events.Meta { return h.meta }
func (h *foreignHost) Log() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The reporter is shared by every component of a run, so it has to survive
// being driven from all of them at once — and still report each bead once.
func TestForeignStatusReporterIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_, _, _ = r.Resolve(domain.BeadID(fmt.Sprintf("sc-%d", j%5)), "reviewing")
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 5, r.Count())
	assert.Len(t, rec.warnings(), 5)
	assert.Len(t, r.Beads(), 5)
}

// P6-28's reader half: `sc status` folds a run log into the beads the run
// said it was leaving alone, by Source and never by wording.
func TestForeignStatusBeadsFoldsARunLog(t *testing.T) {
	t.Parallel()
	meta := events.MetaAt("run-1", stateClock)
	evs := []events.Event{
		events.RunStarted{Meta: meta},
		events.Warning{Meta: meta, Source: SourceForeignStatus, Bead: "sc-2", Message: "left alone"},
		events.Warning{Meta: meta, Source: SourceGates, Bead: "sc-9", Message: "gh is unavailable"},
		events.Warning{Meta: meta, Source: SourceForeignStatus, Bead: "sc-1", Message: "left alone"},
		events.Warning{Meta: meta, Source: SourceForeignStatus, Bead: "sc-2", Message: "left alone again"},
		events.Warning{Meta: meta, Source: SourceForeignStatus, Message: "no bead named"},
		events.Error{Meta: meta, Source: SourceForeignStatus, Bead: "sc-3", Message: "not a warning"},
	}

	assert.Equal(t, []domain.BeadID{"sc-2", "sc-1"}, ForeignStatusBeads(evs),
		"distinct beads, in the order they were first reported")

	id, ok := ForeignStatusOf(evs[1])
	assert.True(t, ok)
	assert.Equal(t, domain.BeadID("sc-2"), id)
	_, ok = ForeignStatusOf(evs[2])
	assert.False(t, ok, "another component's warning is not a foreign-status report")
	_, ok = ForeignStatusOf(evs[0])
	assert.False(t, ok)
	assert.Empty(t, ForeignStatusBeads(nil))
}

// The live count and the folded count are the same number, which is what
// lets `sc status` report a finished run off disk and the TUI report a
// running one from memory.
func TestForeignStatusCountMatchesTheFoldedLog(t *testing.T) {
	t.Parallel()
	r, rec := newForeignHarness(t, nil)

	for _, id := range []domain.BeadID{"sc-1", "sc-2", "sc-1", "sc-3"} {
		_, _, _ = r.Resolve(id, "reviewing")
	}

	assert.Equal(t, 3, r.Count())
	assert.Len(t, ForeignStatusBeads(rec.all()), r.Count())
}
