package domain

// BeadID is the identifier bd mints for an issue, for example "sc-68s" or the
// hierarchical "sc-dml.1". It is opaque to slop-cannon.
type BeadID string

// String returns the id as bd prints it.
func (id BeadID) String() string { return string(id) }

// BeadState is the workflow state of a bead. Per D-07 every state maps one to
// one onto a bd status: open, in_progress and closed are bd built-ins; the
// rest are custom statuses that slop-cannon registers at run start.
//
// Flow: open → in_progress → in_review → (changes_requested → in_progress …)
// → approved → delivering → closed, with escalated, needs_human and failed as
// side exits.
type BeadState string

// Bead states, in the order they normally occur.
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
)

var beadStateEnum = enum[BeadState]{kind: "bead state", values: []BeadState{
	BeadStateOpen, BeadStateInProgress, BeadStateInReview, BeadStateChangesRequested,
	BeadStateApproved, BeadStateDelivering, BeadStateClosed,
	BeadStateEscalated, BeadStateNeedsHuman, BeadStateFailed,
}}

// BeadStates returns every valid BeadState in workflow order.
func BeadStates() []BeadState { return beadStateEnum.all() }

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

// Custom reports whether s is a slop-cannon custom status rather than one of
// bd's built-in open / in_progress / closed.
func (s BeadState) Custom() bool {
	switch s {
	case BeadStateOpen, BeadStateInProgress, BeadStateClosed:
		return false
	default:
		return s.Valid()
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
		BeadStateApproved, BeadStateDelivering:
		return CategoryWIP
	case BeadStateClosed:
		return CategoryDone
	case BeadStateEscalated, BeadStateNeedsHuman, BeadStateFailed:
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

// BeadType is the bd issue type slop-cannon uses. bd has more core types;
// these are the ones the PO may emit (P5-02) plus the run epic (D-22).
type BeadType string

// Bead types.
const (
	// BeadTypeTask is generic work; the default.
	BeadTypeTask BeadType = "task"
	// BeadTypeBug is a defect to fix.
	BeadTypeBug BeadType = "bug"
	// BeadTypeFeature is new user-visible behaviour.
	BeadTypeFeature BeadType = "feature"
	// BeadTypeChore is maintenance with no user-visible change.
	BeadTypeChore BeadType = "chore"
	// BeadTypeEpic is the parent bead each run owns (D-22).
	BeadTypeEpic BeadType = "epic"
)

var beadTypeEnum = enum[BeadType]{kind: "bead type", values: []BeadType{
	BeadTypeTask, BeadTypeBug, BeadTypeFeature, BeadTypeChore, BeadTypeEpic,
}}

// BeadTypes returns every valid BeadType in declaration order.
func BeadTypes() []BeadType { return beadTypeEnum.all() }

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
// (D-22). It is a plain int so it marshals exactly as bd expects.
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

// Bead is one unit of work: an issue in the target repository's beads
// database, as slop-cannon sees it. The beads client (P3-02) converts bd's
// JSON into this shape; sc_* metadata such as the round and PR are surfaced
// as first-class fields.
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
