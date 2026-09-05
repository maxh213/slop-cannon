package events

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// ErrUnknownKind is wrapped by every failure to resolve a [Kind], whether it
// came from user input or from a JSONL line written by a newer sc.
var ErrUnknownKind = errors.New("unknown event kind")

// Kind is the stable snake_case discriminator of an event type. It is what a
// type switch on the concrete event tells you, but usable in JSON, log lines
// and command-line filters.
type Kind string

// Every event kind, in roughly the order they occur in a run.
const (
	// KindRunStarted is a [RunStarted] event.
	KindRunStarted Kind = "run_started"
	// KindRunPaused is a [RunPaused] event.
	KindRunPaused Kind = "run_paused"
	// KindRunResumed is a [RunResumed] event.
	KindRunResumed Kind = "run_resumed"
	// KindRunStopped is a [RunStopped] event.
	KindRunStopped Kind = "run_stopped"
	// KindRunFinished is a [RunFinished] event.
	KindRunFinished Kind = "run_finished"
	// KindPlanProduced is a [PlanProduced] event.
	KindPlanProduced Kind = "plan_produced"
	// KindBeadCreated is a [BeadCreated] event.
	KindBeadCreated Kind = "bead_created"
	// KindBeadUpdated is a [BeadUpdated] event.
	KindBeadUpdated Kind = "bead_updated"
	// KindBeadStateChanged is a [BeadStateChanged] event.
	KindBeadStateChanged Kind = "bead_state_changed"
	// KindGraphChanged is a [GraphChanged] event.
	KindGraphChanged Kind = "graph_changed"
	// KindAgentSpawned is an [AgentSpawned] event.
	KindAgentSpawned Kind = "agent_spawned"
	// KindAgentOutput is an [AgentOutput] event.
	KindAgentOutput Kind = "agent_output"
	// KindAgentFinished is an [AgentFinished] event.
	KindAgentFinished Kind = "agent_finished"
	// KindAgentFailed is an [AgentFailed] event.
	KindAgentFailed Kind = "agent_failed"
	// KindRoundStarted is a [RoundStarted] event.
	KindRoundStarted Kind = "round_started"
	// KindVerdictReceived is a [VerdictReceived] event.
	KindVerdictReceived Kind = "verdict_received"
	// KindResponseReceived is a [ResponseReceived] event.
	KindResponseReceived Kind = "response_received"
	// KindArbitrationReceived is an [ArbitrationReceived] event.
	KindArbitrationReceived Kind = "arbitration_received"
	// KindPROpened is a [PROpened] event.
	KindPROpened Kind = "pr_opened"
	// KindMerged is a [Merged] event.
	KindMerged Kind = "merged"
	// KindWarning is a [Warning] event.
	KindWarning Kind = "warning"
	// KindError is an [Error] event.
	KindError Kind = "error"
)

// String returns the snake_case name.
func (k Kind) String() string { return string(k) }

// Valid reports whether k names a registered event type.
func (k Kind) Valid() bool {
	_, ok := registry[k]
	return ok
}

// ParseKind parses a kind name leniently: case, surrounding whitespace and
// hyphens are ignored ("Bead-State-Changed" works). Anything unregistered,
// including the empty string, returns an error wrapping [ErrUnknownKind].
func ParseKind(s string) (Kind, error) {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	if n == "" {
		return "", fmt.Errorf("%w: empty kind", ErrUnknownKind)
	}
	k := Kind(n)
	if !k.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, s)
	}
	return k, nil
}

// Event is one immutable thing that happened during a run. Concrete events
// are plain structs embedding [Meta]; use a type switch on the value to get at
// their fields, or [Event.Kind] when only the discriminator matters.
//
// Events are published and decoded as values, never pointers, so a type
// switch needs only the value cases.
type Event interface {
	// Kind is the stable discriminator of the concrete type.
	Kind() Kind
	// At is when the event happened.
	At() time.Time
	// RunID is the run the event belongs to.
	RunID() domain.RunID
}

// Meta is embedded by every concrete event and supplies the [Event.At] and
// [Event.RunID] methods. The JSON field names are "at" and "run_id".
type Meta struct {
	// Time is when the event happened. Publishers stamp it; the bus never does.
	Time time.Time `json:"at"`
	// Run is the run the event belongs to.
	Run domain.RunID `json:"run_id"`
}

// NewMeta stamps a Meta with the current UTC time and the given run.
func NewMeta(run domain.RunID) Meta { return MetaAt(run, time.Now()) }

// MetaAt builds a Meta for the given run at an explicit time, normalised to
// UTC so JSONL lines are stable regardless of the local zone. Orchestrator
// code driven by a fake clock uses this.
func MetaAt(run domain.RunID, t time.Time) Meta { return Meta{Time: t.UTC(), Run: run} }

// At implements [Event].
func (m Meta) At() time.Time { return m.Time }

// RunID implements [Event].
func (m Meta) RunID() domain.RunID { return m.Run }

// FinishReason says why a run ended (D-18).
type FinishReason string

// Finish reasons.
const (
	// FinishCompleted means a brief-mode run settled every bead it created.
	FinishCompleted FinishReason = "completed"
	// FinishStopped means the user stopped the run (key, `sc stop`, or signal).
	FinishStopped FinishReason = "stopped"
	// FinishCreditsExhausted means every configured cartridge reported its
	// quota or billing as exhausted; the only way autopilot ends on its own.
	FinishCreditsExhausted FinishReason = "credits_exhausted"
	// FinishBudgetReached means a brief run crossed `limits.max_tokens` or
	// `limits.max_cost_usd` and finished once the reviews in flight had
	// drained (P9-12). It is not a user stop and not a failure: the run did
	// what the cap told it to, so a machine consumer can tell "I set a
	// budget and it was reached" from "somebody pressed `s`". An autopilot
	// run never ends this way — a cap pauses it (D-18).
	FinishBudgetReached FinishReason = "budget_reached"
	// FinishFailed means the orchestrator hit an unrecoverable error.
	FinishFailed FinishReason = "failed"
)

// String returns the canonical name.
func (r FinishReason) String() string { return string(r) }

// Failure classifies why an agent did not produce a usable result.
type Failure string

// Agent failure classes. Rate limits and credit exhaustion are distinct on
// purpose: the first backs off, the second retires the cartridge (D-18).
const (
	// FailureCrash means the subprocess exited non-zero or was killed by a
	// signal it did not ask for.
	FailureCrash Failure = "crash"
	// FailureTimeout means the per-agent timeout expired.
	FailureTimeout Failure = "timeout"
	// FailureRateLimited means the cartridge reported a rate limit.
	FailureRateLimited Failure = "rate_limited"
	// FailureCreditsExhausted means the cartridge reported no credits or quota.
	FailureCreditsExhausted Failure = "credits_exhausted"
	// FailureBadOutput means the subprocess exited cleanly but its structured
	// output could not be parsed or validated.
	FailureBadOutput Failure = "bad_output"
	// FailureKilled means the orchestrator killed it deliberately (stop,
	// pause, or zombie reaping).
	FailureKilled Failure = "killed"
	// FailureInternal means the session itself succeeded — the agent
	// answered and its reply was accepted — but the orchestrator could not
	// use the result afterwards: a bead comment, a state transition or
	// another bookkeeping step failed. It is never the cartridge's fault,
	// so the session is not fired again (P6-07).
	FailureInternal Failure = "internal"
)

// String returns the canonical name.
func (f Failure) String() string { return string(f) }

// Stream names which output stream of an agent subprocess a line came from.
type Stream string

// Output streams.
const (
	// StreamStdout is the subprocess's standard output.
	StreamStdout Stream = "stdout"
	// StreamStderr is the subprocess's standard error.
	StreamStderr Stream = "stderr"
)

// String returns the canonical name.
func (s Stream) String() string { return string(s) }

// RoundKind says what a review round is for.
type RoundKind string

// Round kinds.
const (
	// RoundReview is an ordinary feedback round; it counts against
	// max_review_rounds (D-08).
	RoundReview RoundKind = "review"
	// RoundConflict sends the bead back to the engineer to resolve a merge
	// conflict; it does not count against max_review_rounds (D-10).
	RoundConflict RoundKind = "conflict"
	// RoundCheckFailed is a qa change request raised because the repo's
	// check_command failed at delivery time (P6-06).
	RoundCheckFailed RoundKind = "check_failed"
)

// String returns the canonical name.
func (k RoundKind) String() string { return string(k) }

// Task names the job an agent was fired to do. It is free-form so cartridges
// and future roles can add their own; these are the ones the orchestrator uses.
type Task string

// Agent tasks.
const (
	// TaskPlan is the PO researching and producing a plan.
	TaskPlan Task = "plan"
	// TaskImplement is an engineer implementing a bead.
	TaskImplement Task = "implement"
	// TaskAddressFeedback is an engineer answering a round of feedback.
	TaskAddressFeedback Task = "address_feedback"
	// TaskResolveConflict is an engineer resolving a merge conflict (D-10).
	TaskResolveConflict Task = "resolve_conflict"
	// TaskReview is a tester reviewing through its lens.
	TaskReview Task = "review"
	// TaskEvaluatePushback is a tester judging an engineer's pushback.
	TaskEvaluatePushback Task = "evaluate_pushback"
	// TaskArbitrate is the PO ruling on held items (D-08).
	TaskArbitrate Task = "arbitrate"
	// TaskEscalate is the PO deciding a bead that hit the round cap (D-08).
	TaskEscalate Task = "escalate"
	// TaskReviewPR is a tester reviewing an external pull request (D-11).
	TaskReviewPR Task = "review_pr"
)

// String returns the canonical name.
func (t Task) String() string { return string(t) }
