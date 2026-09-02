package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// BeadID is the identifier bd mints for an issue, for example "sc-68s" or the
// hierarchical "sc-dml.1". It is opaque to slop-cannon.
type BeadID string

// String returns the id as bd prints it.
func (id BeadID) String() string { return string(id) }

// BeadState is a bd status as slop-cannon understands it. Per D-07 every
// workflow state maps one to one onto a bd status: open, in_progress and
// closed are bd built-ins; in_review … failed are custom statuses that
// slop-cannon registers at run start. bd 1.2.2 also ships blocked, hooked,
// deferred and pinned (Appendix A); slop-cannon never sets those, but a bead
// carrying one must still decode because `bd list --all`, `bd show` and
// `bd export` return them from any repository the cannon is fired at. Use
// Workflow to tell the two groups apart.
//
// Flow: open → in_progress → in_review → (changes_requested → in_progress …)
// → approved → delivering → closed, with escalated, needs_human and failed as
// side exits.
type BeadState string

// Bead states: the D-07 workflow states in the order they normally occur,
// followed by the remaining bd built-ins.
const (
	// BeadStateOpen means the bead is ready to be claimed (bd built-in).
	BeadStateOpen BeadState = "open"
	// BeadStateInProgress means an engineer has claimed it and is
	// implementing (bd built-in, set atomically by `bd update --claim`).
	BeadStateInProgress BeadState = "in_progress"
	// BeadStateInReview means the diff is committed and testers are reviewing.
	BeadStateInReview BeadState = "in_review"
	// BeadStateChangesRequested means at least one tester asked for changes
	// and the engineer has feedback to address.
	BeadStateChangesRequested BeadState = "changes_requested"
	// BeadStateApproved means every assigned tester approved.
	BeadStateApproved BeadState = "approved"
	// BeadStateDelivering means the work is being pushed, PR'd or merged
	// (and, with safety on, waits for the human merge behind a bd gate).
	BeadStateDelivering BeadState = "delivering"
	// BeadStateClosed means the code landed on the default branch (bd built-in).
	BeadStateClosed BeadState = "closed"
	// BeadStateEscalated means the round cap was hit and the PO must rule.
	BeadStateEscalated BeadState = "escalated"
	// BeadStateNeedsHuman means the cannon cannot proceed without a person.
	BeadStateNeedsHuman BeadState = "needs_human"
	// BeadStateFailed means the bead was abandoned after unrecoverable errors.
	BeadStateFailed BeadState = "failed"

	// BeadStateBlocked is bd's built-in "blocked by a dependency" status
	// (category wip). slop-cannon never sets it (D-07).
	BeadStateBlocked BeadState = "blocked"
	// BeadStateHooked is bd's built-in "attached to an agent's hook" status
	// (category wip). slop-cannon never sets it.
	BeadStateHooked BeadState = "hooked"
	// BeadStateDeferred is bd's built-in "put on ice for later" status
	// (category frozen). slop-cannon never sets it (D-07).
	BeadStateDeferred BeadState = "deferred"
	// BeadStatePinned is bd's built-in "stays open indefinitely" status
	// (category frozen). slop-cannon never sets it.
	BeadStatePinned BeadState = "pinned"
)

// workflowBeadStates are the D-07 states, in flow order.
var workflowBeadStates = []BeadState{
	BeadStateOpen, BeadStateInProgress, BeadStateInReview, BeadStateChangesRequested,
	BeadStateApproved, BeadStateDelivering, BeadStateClosed,
	BeadStateEscalated, BeadStateNeedsHuman, BeadStateFailed,
}

var beadStateEnum = enum[BeadState]{kind: "bead state", values: append(
	append([]BeadState(nil), workflowBeadStates...),
	BeadStateBlocked, BeadStateHooked, BeadStateDeferred, BeadStatePinned,
)}

// BeadStates returns every valid BeadState: the D-07 workflow states in flow
// order, then the bd-only built-ins (blocked, hooked, deferred, pinned).
func BeadStates() []BeadState { return beadStateEnum.all() }

// WorkflowBeadStates returns only the D-07 states, in flow order: the ones
// the orchestrator's state machine moves beads through.
func WorkflowBeadStates() []BeadState {
	return append([]BeadState(nil), workflowBeadStates...)
}

// CustomBeadStates returns the states that are not bd built-ins and must be
// registered with `bd config set status.custom` at run start (D-07).
func CustomBeadStates() []BeadState {
	var out []BeadState
	for _, s := range beadStateEnum.values {
		if s.Custom() {
			out = append(out, s)
		}
	}
	return out
}

// ParseBeadState parses a state name leniently; see ParseRole for the rules.
func ParseBeadState(s string) (BeadState, error) { return beadStateEnum.parse(s) }

// String returns the bd status name.
func (s BeadState) String() string { return string(s) }

// Valid reports whether s is one of the declared states.
func (s BeadState) Valid() bool { return beadStateEnum.valid(s) }

// Workflow reports whether s is one of the D-07 states the cannon moves beads
// through, as opposed to a bd built-in it only ever reads (blocked, hooked,
// deferred, pinned) or an invalid value.
func (s BeadState) Workflow() bool {
	for _, w := range workflowBeadStates {
		if w == s {
			return true
		}
	}
	return false
}

// Custom reports whether s is a slop-cannon custom status, i.e. a workflow
// state that is not one of bd's built-ins and therefore has to be registered
// with `bd config set status.custom` (D-07).
func (s BeadState) Custom() bool {
	switch s {
	case BeadStateOpen, BeadStateInProgress, BeadStateClosed:
		return false
	default:
		return s.Workflow()
	}
}

// Terminal reports whether the bead needs no further work from the cannon:
// closed, needs_human or failed.
func (s BeadState) Terminal() bool {
	switch s {
	case BeadStateClosed, BeadStateNeedsHuman, BeadStateFailed:
		return true
	default:
		return false
	}
}

// Category returns the bd status category that controls `bd ready` and
// default `bd list` visibility (Appendix A). Invalid states return "".
func (s BeadState) Category() StateCategory {
	switch s {
	case BeadStateOpen:
		return CategoryActive
	case BeadStateInProgress, BeadStateInReview, BeadStateChangesRequested,
		BeadStateApproved, BeadStateDelivering, BeadStateBlocked, BeadStateHooked:
		return CategoryWIP
	case BeadStateClosed:
		return CategoryDone
	case BeadStateEscalated, BeadStateNeedsHuman, BeadStateFailed,
		BeadStateDeferred, BeadStatePinned:
		return CategoryFrozen
	default:
		return ""
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s BeadState) MarshalText() ([]byte, error) { return beadStateEnum.marshal(s) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *BeadState) UnmarshalText(b []byte) error {
	v, err := beadStateEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// StateCategory is bd's status category: it decides whether a status counts
// as ready, in flight, done or parked.
type StateCategory string

// The four bd status categories.
const (
	// CategoryActive statuses are claimable and appear in `bd ready`.
	CategoryActive StateCategory = "active"
	// CategoryWIP statuses are in flight and hidden from `bd ready`.
	CategoryWIP StateCategory = "wip"
	// CategoryDone statuses are finished and hidden from default listings.
	CategoryDone StateCategory = "done"
	// CategoryFrozen statuses are parked until someone intervenes.
	CategoryFrozen StateCategory = "frozen"
)

var stateCategoryEnum = enum[StateCategory]{kind: "state category", values: []StateCategory{
	CategoryActive, CategoryWIP, CategoryDone, CategoryFrozen,
}}

// StateCategories returns every valid StateCategory in declaration order.
func StateCategories() []StateCategory { return stateCategoryEnum.all() }

// ParseStateCategory parses a category name leniently; see ParseRole.
func ParseStateCategory(s string) (StateCategory, error) { return stateCategoryEnum.parse(s) }

// String returns the bd category name.
func (c StateCategory) String() string { return string(c) }

// Valid reports whether c is one of the declared categories.
func (c StateCategory) Valid() bool { return stateCategoryEnum.valid(c) }

// MarshalText implements encoding.TextMarshaler.
func (c StateCategory) MarshalText() ([]byte, error) { return stateCategoryEnum.marshal(c) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *StateCategory) UnmarshalText(b []byte) error {
	v, err := stateCategoryEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// BeadType is a bd issue type. The list covers everything bd 1.2.2 returns
// (Appendix A): the core types a person or the PO may create, plus the gate
// and event beads bd mints itself (`bd gate create`, which slop-cannon uses
// under D-20, and `bd set-state`). Only the Plannable types may appear in a
// PO plan (P5-02); the run epic is created separately (D-22).
type BeadType string

// Bead types: the four plannable core types first, then the rest of bd's
// core types, then the two bd-minted infrastructure types.
const (
	// BeadTypeTask is generic work; the default.
	BeadTypeTask BeadType = "task"
	// BeadTypeBug is a defect to fix.
	BeadTypeBug BeadType = "bug"
	// BeadTypeFeature is new user-visible behaviour.
	BeadTypeFeature BeadType = "feature"
	// BeadTypeChore is maintenance with no user-visible change.
	BeadTypeChore BeadType = "chore"
	// BeadTypeEpic is a parent bead; each run owns one (D-22).
	BeadTypeEpic BeadType = "epic"
	// BeadTypeDecision is a bd core type recording a decision to be made.
	BeadTypeDecision BeadType = "decision"
	// BeadTypeSpike is a bd core type for time-boxed research.
	BeadTypeSpike BeadType = "spike"
	// BeadTypeStory is a bd core type for a user story.
	BeadTypeStory BeadType = "story"
	// BeadTypeMilestone is a bd core type marking a milestone.
	BeadTypeMilestone BeadType = "milestone"
	// BeadTypeGate is minted by `bd gate create`; slop-cannon creates one per
	// pull request with safety on (D-20).
	BeadTypeGate BeadType = "gate"
	// BeadTypeEvent is minted by `bd set-state` for each state-cache change;
	// slop-cannon never creates one but may list them with --include-infra.
	BeadTypeEvent BeadType = "event"
)

// plannableBeadTypes are the types the PO may emit in a plan (P5-02).
var plannableBeadTypes = []BeadType{BeadTypeTask, BeadTypeBug, BeadTypeFeature, BeadTypeChore}

var beadTypeEnum = enum[BeadType]{kind: "bead type", values: append(
	append([]BeadType(nil), plannableBeadTypes...),
	BeadTypeEpic, BeadTypeDecision, BeadTypeSpike, BeadTypeStory, BeadTypeMilestone,
	BeadTypeGate, BeadTypeEvent,
)}

// BeadTypes returns every valid BeadType in declaration order.
func BeadTypes() []BeadType { return beadTypeEnum.all() }

// PlannableBeadTypes returns the types a PO plan may use: task, bug, feature
// and chore.
func PlannableBeadTypes() []BeadType { return append([]BeadType(nil), plannableBeadTypes...) }

// Plannable reports whether t may appear in a PO plan (Plan.Validate
// enforces this). The run epic and bd's infrastructure types are not.
func (t BeadType) Plannable() bool {
	for _, p := range plannableBeadTypes {
		if p == t {
			return true
		}
	}
	return false
}

// ParseBeadType parses a type name leniently; see ParseRole for the rules.
func ParseBeadType(s string) (BeadType, error) { return beadTypeEnum.parse(s) }

// String returns the bd issue type name.
func (t BeadType) String() string { return string(t) }

// Valid reports whether t is one of the declared types.
func (t BeadType) Valid() bool { return beadTypeEnum.valid(t) }

// MarshalText implements encoding.TextMarshaler.
func (t BeadType) MarshalText() ([]byte, error) { return beadTypeEnum.marshal(t) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *BeadType) UnmarshalText(b []byte) error {
	v, err := beadTypeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// Size is the PO's effort estimate for a bead, stored in `sc_size` metadata.
type Size string

// Sizes, matching the S/M/L scale used throughout the project.
const (
	// SizeS is under an hour of focused work.
	SizeS Size = "s"
	// SizeM is one to three hours.
	SizeM Size = "m"
	// SizeL is three to eight hours.
	SizeL Size = "l"
)

var sizeEnum = enum[Size]{kind: "size", values: []Size{SizeS, SizeM, SizeL}}

// Sizes returns every valid Size from smallest to largest.
func Sizes() []Size { return sizeEnum.all() }

// ParseSize parses a size leniently ("S", "m", " L " all work).
func ParseSize(s string) (Size, error) { return sizeEnum.parse(s) }

// String returns the canonical lower-case letter.
func (s Size) String() string { return string(s) }

// Valid reports whether s is one of the declared sizes.
func (s Size) Valid() bool { return sizeEnum.valid(s) }

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) { return sizeEnum.marshal(s) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(b []byte) error {
	v, err := sizeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// Priority uses bd's scale: 0 is the most urgent, 4 the least, 2 the default
// (D-22). It marshals as a plain JSON number, exactly as bd expects.
//
// The Go zero value is PriorityHighest, not the default, so code that builds
// a Bead or PlanBead by hand must set Priority explicitly (PriorityDefault
// when it has no opinion). JSON is safer: a "priority" field that is absent,
// null or an empty string decodes to PriorityDefault when the enclosing Bead
// or PlanBead is unmarshalled, so a PO plan that omits priorities creates
// default-urgency beads rather than top-urgency ones. A quoted number such as
// "1" is accepted too, since LLM output is not always well typed.
type Priority int

// Priority bounds and default on bd's scale.
const (
	// PriorityHighest is the most urgent priority bd accepts.
	PriorityHighest Priority = 0
	// PriorityDefault is what bd assigns when none is given.
	PriorityDefault Priority = 2
	// PriorityLowest is the least urgent priority bd accepts.
	PriorityLowest Priority = 4
)

// Valid reports whether p lies within bd's 0–4 range.
func (p Priority) Valid() bool { return p >= PriorityHighest && p <= PriorityLowest }

// UnmarshalJSON implements json.Unmarshaler. A JSON number is taken as is
// (range is checked by Plan.Validate, not here); null and "" mean
// PriorityDefault; a quoted integer such as "1" is parsed. Anything else is
// an error wrapping ErrUnknownValue.
func (p *Priority) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" {
		*p = PriorityDefault
		return nil
	}
	if strings.HasPrefix(text, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("%w: priority %s: %w", ErrUnknownValue, text, err)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*p = PriorityDefault
			return nil
		}
		text = s
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("%w: priority %s (want an integer %d..%d)", ErrUnknownValue, string(data), PriorityHighest, PriorityLowest)
	}
	*p = Priority(n)
	return nil
}

// Bead is one unit of work: an issue in the target repository's beads
// database, as slop-cannon sees it. The beads client (P3-02) converts bd's
// JSON into this shape; sc_* metadata such as the round and PR are surfaced
// as first-class fields. Bead is not a byte-for-byte image of a bd issue:
// field names differ (state for status, type for issue_type, acceptance for
// acceptance_criteria) and bd's bookkeeping fields are dropped, but State and
// Type accept every status and issue type bd 1.2.2 returns.
type Bead struct {
	// ID is the bd-minted identifier.
	ID BeadID `json:"id"`
	// Title is the one-line summary.
	Title string `json:"title"`
	// Description explains the work in full.
	Description string `json:"description,omitempty"`
	// Acceptance holds the acceptance criteria the testers hold the engineer to.
	Acceptance string `json:"acceptance,omitempty"`
	// Type is the bd issue type; empty means task.
	Type BeadType `json:"type,omitempty"`
	// Priority is on bd's 0 (highest) to 4 (lowest) scale.
	Priority Priority `json:"priority"`
	// Size is the PO's S/M/L estimate.
	Size Size `json:"size,omitempty"`
	// Deps lists the beads this one depends on ("blocks" edges pointing at
	// this bead); it is ready only when all of them are closed (D-20).
	Deps []BeadID `json:"deps,omitempty"`
	// State is the workflow state, mirrored from the bd status.
	State BeadState `json:"state"`
	// Assignee is the agent id currently holding the claim, if any.
	Assignee string `json:"assignee,omitempty"`
	// Labels are bd labels, used only for cheap filtering (D-06).
	Labels []string `json:"labels,omitempty"`
	// Round is the number of review rounds completed so far (sc_round).
	Round int `json:"round"`
	// PRURL is the pull request opened for this bead, if any (sc_pr_url).
	PRURL string `json:"pr_url,omitempty"`
	// PRNumber is the pull request number, if any (sc_pr_number).
	PRNumber int `json:"pr_number,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler. It decodes like the plain struct
// except that a missing "priority" yields PriorityDefault rather than
// PriorityHighest (see Priority).
func (b *Bead) UnmarshalJSON(data []byte) error {
	type plain Bead // no methods, so the default decoder is used
	v := plain{Priority: PriorityDefault}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*b = Bead(v)
	return nil
}
