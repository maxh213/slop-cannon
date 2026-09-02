package domain

// VerdictOutcome is a tester's overall decision about a round.
type VerdictOutcome string

// Verdict outcomes.
const (
	// VerdictApprove means the tester is satisfied.
	VerdictApprove VerdictOutcome = "approve"
	// VerdictChanges means the tester wants the items addressed first.
	VerdictChanges VerdictOutcome = "changes"
)

var verdictOutcomeEnum = enum[VerdictOutcome]{kind: "verdict outcome", values: []VerdictOutcome{VerdictApprove, VerdictChanges}}

// VerdictOutcomes returns every valid VerdictOutcome in declaration order.
func VerdictOutcomes() []VerdictOutcome { return verdictOutcomeEnum.all() }

// ParseVerdictOutcome parses an outcome leniently; see ParseRole for the rules.
func ParseVerdictOutcome(s string) (VerdictOutcome, error) { return verdictOutcomeEnum.parse(s) }

// String returns the canonical name.
func (o VerdictOutcome) String() string { return string(o) }

// Valid reports whether o is one of the declared outcomes.
func (o VerdictOutcome) Valid() bool { return verdictOutcomeEnum.valid(o) }

// MarshalText implements encoding.TextMarshaler.
func (o VerdictOutcome) MarshalText() ([]byte, error) { return verdictOutcomeEnum.marshal(o) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (o *VerdictOutcome) UnmarshalText(b []byte) error {
	v, err := verdictOutcomeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*o = v
	return nil
}

// Severity ranks a feedback item. Blockers must be resolved before approval;
// nits never block on their own.
type Severity string

// Severities from most to least serious.
const (
	// SeverityBlocker must be fixed; a rules violation is always a blocker.
	SeverityBlocker Severity = "blocker"
	// SeverityMajor is a real problem the tester expects fixed.
	SeverityMajor Severity = "major"
	// SeverityMinor is worth fixing but not worth a round on its own.
	SeverityMinor Severity = "minor"
	// SeverityNit is a matter of taste.
	SeverityNit Severity = "nit"
)

var severityEnum = enum[Severity]{kind: "severity", values: []Severity{SeverityBlocker, SeverityMajor, SeverityMinor, SeverityNit}}

// Severities returns every valid Severity from most to least serious.
func Severities() []Severity { return severityEnum.all() }

// ParseSeverity parses a severity leniently; see ParseRole for the rules.
func ParseSeverity(s string) (Severity, error) { return severityEnum.parse(s) }

// String returns the canonical name.
func (s Severity) String() string { return string(s) }

// Valid reports whether s is one of the declared severities.
func (s Severity) Valid() bool { return severityEnum.valid(s) }

// Rank orders severities: 0 for blocker up to 3 for nit; -1 when invalid.
func (s Severity) Rank() int {
	for i, v := range severityEnum.values {
		if v == s {
			return i
		}
	}
	return -1
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) { return severityEnum.marshal(s) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	v, err := severityEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ItemStatus tracks one feedback item across rounds from the tester's point
// of view.
type ItemStatus string

// Item statuses.
const (
	// ItemOpen means the item has been raised and not yet settled.
	ItemOpen ItemStatus = "open"
	// ItemResolved means the tester confirmed the engineer's fix.
	ItemResolved ItemStatus = "resolved"
	// ItemAccepted means the tester accepted the engineer's pushback or
	// wontfix, so the item no longer blocks.
	ItemAccepted ItemStatus = "accepted"
	// ItemHeld means the tester rejected the pushback; after
	// max_pushback_rounds the PO arbitrates (D-08).
	ItemHeld ItemStatus = "held"
	// ItemOverruled means a PO arbitration settled the item against the
	// tester, so it no longer blocks.
	ItemOverruled ItemStatus = "overruled"
)

var itemStatusEnum = enum[ItemStatus]{kind: "item status", values: []ItemStatus{
	ItemOpen, ItemResolved, ItemAccepted, ItemHeld, ItemOverruled,
}}

// ItemStatuses returns every valid ItemStatus in declaration order.
func ItemStatuses() []ItemStatus { return itemStatusEnum.all() }

// ParseItemStatus parses a status leniently; see ParseRole for the rules.
func ParseItemStatus(s string) (ItemStatus, error) { return itemStatusEnum.parse(s) }

// String returns the canonical name.
func (s ItemStatus) String() string { return string(s) }

// Valid reports whether s is one of the declared statuses.
func (s ItemStatus) Valid() bool { return itemStatusEnum.valid(s) }

// Settled reports whether the item no longer needs anyone's attention.
func (s ItemStatus) Settled() bool {
	switch s {
	case ItemResolved, ItemAccepted, ItemOverruled:
		return true
	default:
		return false
	}
}

// MarshalText implements encoding.TextMarshaler.
func (s ItemStatus) MarshalText() ([]byte, error) { return itemStatusEnum.marshal(s) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *ItemStatus) UnmarshalText(b []byte) error {
	v, err := itemStatusEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// CategoryRules is the mandatory FeedbackItem category for violations of a
// rules file. Such items are always blockers and arbitration cannot decide
// them in the engineer's favour (P5-04, P9).
const CategoryRules = "rules"

// FeedbackItem is one concrete point a tester raises in a verdict.
type FeedbackItem struct {
	// ID is unique within the bead across rounds (for example "qa-r1-3") so
	// responses and arbitrations can refer to it.
	ID string `json:"id"`
	// Severity says how serious it is.
	Severity Severity `json:"severity"`
	// Category is a free-form grouping; CategoryRules is special.
	Category string `json:"category,omitempty"`
	// Title is the one-line summary.
	Title string `json:"title"`
	// Detail explains the problem.
	Detail string `json:"detail,omitempty"`
	// File is the path, relative to the repo root, the item refers to, if any.
	File string `json:"file,omitempty"`
	// Line is the 1-based line in File, or 0 when not applicable.
	Line int `json:"line,omitempty"`
	// Suggestion is the tester's proposed fix, possibly including code.
	Suggestion string `json:"suggestion,omitempty"`
	// Status is where the item stands.
	Status ItemStatus `json:"status"`
}

// Verdict is a tester's structured review of one round.
type Verdict struct {
	// Lens is the perspective the tester reviewed from.
	Lens Lens `json:"lens,omitempty"`
	// Round is the review round the verdict belongs to (1-based).
	Round int `json:"round,omitempty"`
	// Outcome is approve or changes.
	Outcome VerdictOutcome `json:"outcome"`
	// Summary is the tester's overall note.
	Summary string `json:"summary,omitempty"`
	// Items are the individual findings; empty on a clean approval.
	Items []FeedbackItem `json:"items,omitempty"`
}

// ResponseAction is what the engineer did about a feedback item.
type ResponseAction string

// Response actions.
const (
	// ActionFixed means the engineer changed the code; the originating
	// tester re-reviews.
	ActionFixed ResponseAction = "fixed"
	// ActionPushback means the engineer disagrees; the tester evaluates the
	// rationale and accepts or holds.
	ActionPushback ResponseAction = "pushback"
	// ActionWontfix means the engineer declines the item as out of scope.
	ActionWontfix ResponseAction = "wontfix"
)

var responseActionEnum = enum[ResponseAction]{kind: "response action", values: []ResponseAction{
	ActionFixed, ActionPushback, ActionWontfix,
}}

// ResponseActions returns every valid ResponseAction in declaration order.
func ResponseActions() []ResponseAction { return responseActionEnum.all() }

// ParseResponseAction parses an action leniently; see ParseRole for the rules.
func ParseResponseAction(s string) (ResponseAction, error) { return responseActionEnum.parse(s) }

// String returns the canonical name.
func (a ResponseAction) String() string { return string(a) }

// Valid reports whether a is one of the declared actions.
func (a ResponseAction) Valid() bool { return responseActionEnum.valid(a) }

// MarshalText implements encoding.TextMarshaler.
func (a ResponseAction) MarshalText() ([]byte, error) { return responseActionEnum.marshal(a) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *ResponseAction) UnmarshalText(b []byte) error {
	v, err := responseActionEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*a = v
	return nil
}

// Response is the engineer's answer to one feedback item.
type Response struct {
	// ItemID refers to FeedbackItem.ID.
	ItemID string `json:"item_id"`
	// Action is fixed, pushback or wontfix.
	Action ResponseAction `json:"action"`
	// Rationale explains the action; required for pushback and wontfix.
	Rationale string `json:"rationale,omitempty"`
}

// ArbitrationRuling is the PO's binding decision on a disputed item (D-08).
type ArbitrationRuling string

// Arbitration rulings.
const (
	// RulingUpholdTester means the engineer must address the item.
	RulingUpholdTester ArbitrationRuling = "uphold_tester"
	// RulingUpholdEngineer means the item is overruled.
	RulingUpholdEngineer ArbitrationRuling = "uphold_engineer"
	// RulingCompromise means the engineer follows the PO's instruction instead.
	RulingCompromise ArbitrationRuling = "compromise"
)

var arbitrationRulingEnum = enum[ArbitrationRuling]{kind: "arbitration ruling", values: []ArbitrationRuling{
	RulingUpholdTester, RulingUpholdEngineer, RulingCompromise,
}}

// ArbitrationRulings returns every valid ArbitrationRuling in declaration order.
func ArbitrationRulings() []ArbitrationRuling { return arbitrationRulingEnum.all() }

// ParseArbitrationRuling parses a ruling leniently; see ParseRole for the rules.
func ParseArbitrationRuling(s string) (ArbitrationRuling, error) {
	return arbitrationRulingEnum.parse(s)
}

// String returns the canonical name.
func (r ArbitrationRuling) String() string { return string(r) }

// Valid reports whether r is one of the declared rulings.
func (r ArbitrationRuling) Valid() bool { return arbitrationRulingEnum.valid(r) }

// MarshalText implements encoding.TextMarshaler.
func (r ArbitrationRuling) MarshalText() ([]byte, error) { return arbitrationRulingEnum.marshal(r) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *ArbitrationRuling) UnmarshalText(b []byte) error {
	v, err := arbitrationRulingEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// Arbitration is the PO's ruling on one item the engineer and a tester could
// not settle between themselves.
type Arbitration struct {
	// ItemID refers to FeedbackItem.ID.
	ItemID string `json:"item_id"`
	// Ruling is who prevails.
	Ruling ArbitrationRuling `json:"ruling"`
	// Instruction tells the engineer what to do; required for compromise.
	Instruction string `json:"instruction,omitempty"`
	// Rationale explains the ruling.
	Rationale string `json:"rationale,omitempty"`
}

// EscalationOutcome is the PO's decision when a bead hits the review round
// cap without approval (D-08).
type EscalationOutcome string

// Escalation outcomes.
const (
	// EscalationApproveWithNotes accepts the work as is, recording the notes.
	EscalationApproveWithNotes EscalationOutcome = "approve_with_notes"
	// EscalationNarrowScope replaces the acceptance criteria and restarts review.
	EscalationNarrowScope EscalationOutcome = "narrow_scope"
	// EscalationBlock hands the bead to a human (needs_human).
	EscalationBlock EscalationOutcome = "block"
)

var escalationOutcomeEnum = enum[EscalationOutcome]{kind: "escalation outcome", values: []EscalationOutcome{
	EscalationApproveWithNotes, EscalationNarrowScope, EscalationBlock,
}}

// EscalationOutcomes returns every valid EscalationOutcome in declaration order.
func EscalationOutcomes() []EscalationOutcome { return escalationOutcomeEnum.all() }

// ParseEscalationOutcome parses an outcome leniently; see ParseRole for the rules.
func ParseEscalationOutcome(s string) (EscalationOutcome, error) {
	return escalationOutcomeEnum.parse(s)
}

// String returns the canonical name.
func (o EscalationOutcome) String() string { return string(o) }

// Valid reports whether o is one of the declared outcomes.
func (o EscalationOutcome) Valid() bool { return escalationOutcomeEnum.valid(o) }

// MarshalText implements encoding.TextMarshaler.
func (o EscalationOutcome) MarshalText() ([]byte, error) { return escalationOutcomeEnum.marshal(o) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (o *EscalationOutcome) UnmarshalText(b []byte) error {
	v, err := escalationOutcomeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*o = v
	return nil
}

// Escalation is the PO's ruling on a bead that exhausted its review rounds.
type Escalation struct {
	// Outcome is what happens to the bead.
	Outcome EscalationOutcome `json:"outcome"`
	// Notes accompany approve_with_notes.
	Notes string `json:"notes,omitempty"`
	// NewAcceptance replaces the bead's acceptance criteria for narrow_scope.
	NewAcceptance string `json:"new_acceptance,omitempty"`
	// Summary is written for the human when the outcome is block.
	Summary string `json:"summary,omitempty"`
}

// ImplReport is what an engineer hands to the testers after implementing.
type ImplReport struct {
	// Summary describes what was done and why.
	Summary string `json:"summary"`
	// FilesChanged lists paths relative to the repo root.
	FilesChanged []string `json:"files_changed,omitempty"`
	// HowToVerify tells testers how to exercise the change.
	HowToVerify string `json:"how_to_verify,omitempty"`
	// Risks lists anything the engineer is unsure about.
	Risks []string `json:"risks,omitempty"`
	// Questions are open points for the testers or the PO.
	Questions []string `json:"questions,omitempty"`
	// Commit is the sha the report describes, if the engineer reported it.
	Commit string `json:"commit,omitempty"`
}

// MessageType tags the fenced JSON block inside a bead comment so readers
// know which struct to decode (D-06).
//
// Deviation note (P0-05): D-06 lists impl_report, verdict, response,
// arbitration and plan. This package adds escalation, because the PO's
// ruling on a bead that exhausted its review rounds (D-08, Escalation) is a
// distinct message from a per-item Arbitration and must also survive a
// resume. P3-03's comment reader has to accept all six; the D-06 list should
// be read as including escalation until the Decisions section records it.
type MessageType string

// Message types, matching the `type` field in bead comments.
const (
	// MessagePlan wraps a Plan.
	MessagePlan MessageType = "plan"
	// MessageImplReport wraps an ImplReport.
	MessageImplReport MessageType = "impl_report"
	// MessageVerdict wraps a Verdict.
	MessageVerdict MessageType = "verdict"
	// MessageResponse wraps a list of Response values.
	MessageResponse MessageType = "response"
	// MessageArbitration wraps an Arbitration.
	MessageArbitration MessageType = "arbitration"
	// MessageEscalation wraps an Escalation. Not in the D-06 list; see the
	// deviation note on MessageType.
	MessageEscalation MessageType = "escalation"
)

var messageTypeEnum = enum[MessageType]{kind: "message type", values: []MessageType{
	MessagePlan, MessageImplReport, MessageVerdict, MessageResponse, MessageArbitration, MessageEscalation,
}}

// MessageTypes returns every valid MessageType in declaration order.
func MessageTypes() []MessageType { return messageTypeEnum.all() }

// ParseMessageType parses a type leniently; see ParseRole for the rules.
func ParseMessageType(s string) (MessageType, error) { return messageTypeEnum.parse(s) }

// String returns the canonical name.
func (t MessageType) String() string { return string(t) }

// Valid reports whether t is one of the declared types.
func (t MessageType) Valid() bool { return messageTypeEnum.valid(t) }

// MarshalText implements encoding.TextMarshaler.
func (t MessageType) MarshalText() ([]byte, error) { return messageTypeEnum.marshal(t) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *MessageType) UnmarshalText(b []byte) error {
	v, err := messageTypeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*t = v
	return nil
}
