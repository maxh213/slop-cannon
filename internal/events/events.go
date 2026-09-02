package events

import (
	"time"

	"slop-cannon/internal/domain"
)

// RunStarted is published once, first, when a run (fresh or resumed) begins.
type RunStarted struct {
	Meta
	// Info is the run as recorded at start, including its config snapshot.
	Info domain.Run `json:"run"`
	// Resumed is true when the run was picked up with --resume rather than
	// started from scratch.
	Resumed bool `json:"resumed,omitempty"`
}

// Kind implements [Event].
func (RunStarted) Kind() Kind { return KindRunStarted }

// RunPaused is published when the scheduler stops firing new agents. Running
// agents finish; nothing new starts until [RunResumed].
type RunPaused struct {
	Meta
	// Reason says who or what paused the run: "user", or a cap such as
	// "cost_cap" or "token_cap" (D-18).
	Reason string `json:"reason"`
}

// Kind implements [Event].
func (RunPaused) Kind() Kind { return KindRunPaused }

// RunResumed is published when a paused run starts firing agents again.
type RunResumed struct {
	Meta
	// Reason mirrors [RunPaused.Reason].
	Reason string `json:"reason,omitempty"`
}

// Kind implements [Event].
func (RunResumed) Kind() Kind { return KindRunResumed }

// RunStopped is published when a stop has been requested and the orchestrator
// begins shutting down: agents are being killed, leases released. It is
// always followed by [RunFinished].
type RunStopped struct {
	Meta
	// Reason says what triggered the stop: "key", "sc stop", "SIGINT", ...
	Reason string `json:"reason"`
}

// Kind implements [Event].
func (RunStopped) Kind() Kind { return KindRunStopped }

// RunFinished is the last event of a run. The headless renderer maps Reason
// to the process exit code (P7-08).
type RunFinished struct {
	Meta
	// Reason is why the run ended.
	Reason FinishReason `json:"reason"`
	// Summary is a short human-readable account of what was achieved.
	Summary string `json:"summary,omitempty"`
	// Duration is how long the run was live.
	Duration time.Duration `json:"duration,omitempty"`
}

// Kind implements [Event].
func (RunFinished) Kind() Kind { return KindRunFinished }

// PlanProduced is published when the PO's plan has been validated and turned
// into beads (D-22).
type PlanProduced struct {
	Meta
	// Agent is the PO that produced the plan.
	Agent domain.AgentID `json:"agent"`
	// Epic is the run's parent bead the new beads were created under.
	Epic domain.BeadID `json:"epic,omitempty"`
	// Plan is the validated plan as the PO returned it.
	Plan domain.Plan `json:"plan"`
	// Created maps each plan key to the bd-minted id. Empty for an empty
	// plan (autopilot "nothing to add", D-18).
	Created map[string]domain.BeadID `json:"created,omitempty"`
}

// Kind implements [Event].
func (PlanProduced) Kind() Kind { return KindPlanProduced }

// BeadCreated is published for every bead the run creates, including the
// epic and external-PR review beads (D-11).
type BeadCreated struct {
	Meta
	// Bead is the bead as created.
	Bead domain.Bead `json:"bead"`
}

// Kind implements [Event].
func (BeadCreated) Kind() Kind { return KindBeadCreated }

// BeadUpdated is published when fields other than the state change: title,
// acceptance, priority, assignee, labels, round, PR.
type BeadUpdated struct {
	Meta
	// Bead is the bead after the update.
	Bead domain.Bead `json:"bead"`
	// Fields names the JSON fields of [domain.Bead] that changed.
	Fields []string `json:"fields,omitempty"`
}

// Kind implements [Event].
func (BeadUpdated) Kind() Kind { return KindBeadUpdated }

// BeadStateChanged is published on every workflow transition (D-07).
type BeadStateChanged struct {
	Meta
	// Bead is the bead that moved.
	Bead domain.BeadID `json:"bead"`
	// From is the previous state; empty for a newly created bead.
	From domain.BeadState `json:"from,omitempty"`
	// To is the new state.
	To domain.BeadState `json:"to"`
	// Round is the review round the bead is in, when relevant.
	Round int `json:"round,omitempty"`
	// Reason explains the transition in a few words ("all testers approved").
	Reason string `json:"reason,omitempty"`
}

// Kind implements [Event].
func (BeadStateChanged) Kind() Kind { return KindBeadStateChanged }

// GraphChanged carries a full snapshot of the run's beads whenever the shape
// of the dependency graph changes: beads added or removed, dependencies
// added. The graph view (P7-02) relays out from it.
type GraphChanged struct {
	Meta
	// Epic is the run's parent bead.
	Epic domain.BeadID `json:"epic,omitempty"`
	// Beads is every work bead of the run with its Deps populated.
	Beads []domain.Bead `json:"beads"`
}

// Kind implements [Event].
func (GraphChanged) Kind() Kind { return KindGraphChanged }

// AgentSpawned is published when a subprocess has been started ("fired").
// The TUI animates a shot from the cannon to the bead on it (P7-06).
type AgentSpawned struct {
	Meta
	// Agent describes the subprocess, including its PID and start time.
	Agent domain.Agent `json:"agent"`
	// Task is what it was asked to do.
	Task Task `json:"task"`
	// BeadTitle is the title of Agent.Bead, for renderers that do not keep a
	// bead index; empty for a planning PO.
	BeadTitle string `json:"bead_title,omitempty"`
	// Round is the review round the agent works in, when relevant.
	Round int `json:"round,omitempty"`
	// Attempt is 1 for the first try and higher for crash retries (P6-07).
	Attempt int `json:"attempt,omitempty"`
}

// Kind implements [Event].
func (AgentSpawned) Kind() Kind { return KindAgentSpawned }

// AgentOutput is one line of an agent's stdout or stderr. It is the most
// frequent event by far; subscribers that do not render transcripts should
// skip it cheaply by Kind.
type AgentOutput struct {
	Meta
	// Agent is the subprocess that wrote the line.
	Agent domain.AgentID `json:"agent"`
	// Stream is stdout or stderr.
	Stream Stream `json:"stream"`
	// Line is the text without its trailing newline; already redacted (P9-05).
	Line string `json:"line"`
}

// Kind implements [Event].
func (AgentOutput) Kind() Kind { return KindAgentOutput }

// AgentFinished is published when a subprocess exited and its structured
// output was parsed successfully.
type AgentFinished struct {
	Meta
	// Agent is the subprocess that finished.
	Agent domain.AgentID `json:"agent"`
	// Role is the agent's role.
	Role domain.Role `json:"role"`
	// Bead is the bead it worked on; empty for a planning PO.
	Bead domain.BeadID `json:"bead,omitempty"`
	// Duration is wall-clock time from spawn to exit.
	Duration time.Duration `json:"duration"`
	// ExitCode is the subprocess exit status.
	ExitCode int `json:"exit_code"`
	// Produced is the type of structured message the agent returned.
	Produced domain.MessageType `json:"produced,omitempty"`
}

// Kind implements [Event].
func (AgentFinished) Kind() Kind { return KindAgentFinished }

// AgentFailed is published when a subprocess did not yield a usable result.
// The supervisor (P6-07) decides on retry, backoff or cartridge retirement
// from Failure and reports it in WillRetry.
type AgentFailed struct {
	Meta
	// Agent is the subprocess that failed.
	Agent domain.AgentID `json:"agent"`
	// Role is the agent's role.
	Role domain.Role `json:"role"`
	// Bead is the bead it worked on; empty for a planning PO.
	Bead domain.BeadID `json:"bead,omitempty"`
	// Failure classifies what went wrong.
	Failure Failure `json:"failure"`
	// Err is the underlying error message, already redacted.
	Err string `json:"error,omitempty"`
	// Duration is wall-clock time from spawn to failure.
	Duration time.Duration `json:"duration,omitempty"`
	// ExitCode is the subprocess exit status, when it exited at all.
	ExitCode int `json:"exit_code,omitempty"`
	// Attempt is which try this was, starting at 1.
	Attempt int `json:"attempt,omitempty"`
	// WillRetry is true when the supervisor is going to fire it again.
	WillRetry bool `json:"will_retry,omitempty"`
}

// Kind implements [Event].
func (AgentFailed) Kind() Kind { return KindAgentFailed }

// RoundStarted is published when a bead enters a review round: the engineer's
// diff is committed and testers are about to be fired.
type RoundStarted struct {
	Meta
	// Bead is the bead under review.
	Bead domain.BeadID `json:"bead"`
	// Round is the 1-based round number. Conflict rounds keep the number of
	// the review round they interrupt (D-10).
	Round int `json:"round"`
	// RoundKind says what the round is for.
	RoundKind RoundKind `json:"round_kind"`
	// Lenses lists the tester lenses that will review this round (D-09:
	// only testers who did not approve last round, unless the diff ratio
	// forces a full re-review).
	Lenses []domain.Lens `json:"lenses,omitempty"`
	// Commit is the sha the testers review.
	Commit string `json:"commit,omitempty"`
}

// Kind implements [Event].
func (RoundStarted) Kind() Kind { return KindRoundStarted }

// VerdictReceived is published when a tester's verdict has been parsed and
// recorded as a bead comment (D-06).
type VerdictReceived struct {
	Meta
	// Bead is the bead reviewed.
	Bead domain.BeadID `json:"bead"`
	// Round is the review round.
	Round int `json:"round"`
	// Agent is the tester.
	Agent domain.AgentID `json:"agent"`
	// Verdict is the structured review.
	Verdict domain.Verdict `json:"verdict"`
}

// Kind implements [Event].
func (VerdictReceived) Kind() Kind { return KindVerdictReceived }

// ResponseReceived is published when the engineer has answered a round of
// feedback. The first round of a bead has no items to answer, so Responses
// is empty then and Report carries the implementation report instead.
type ResponseReceived struct {
	Meta
	// Bead is the bead worked on.
	Bead domain.BeadID `json:"bead"`
	// Round is the review round the responses belong to.
	Round int `json:"round"`
	// Agent is the engineer.
	Agent domain.AgentID `json:"agent"`
	// Responses answers the round's feedback items one by one.
	Responses []domain.Response `json:"responses,omitempty"`
	// Report is the engineer's implementation report for this round, if any.
	Report *domain.ImplReport `json:"report,omitempty"`
}

// Kind implements [Event].
func (ResponseReceived) Kind() Kind { return KindResponseReceived }

// ArbitrationReceived is published when the PO has ruled: on held items
// (Rulings, D-08) or on a bead that hit the round cap (Escalation, D-08).
// At least one of the two is set.
type ArbitrationReceived struct {
	Meta
	// Bead is the disputed bead.
	Bead domain.BeadID `json:"bead"`
	// Round is the review round the ruling applies to.
	Round int `json:"round"`
	// Agent is the PO.
	Agent domain.AgentID `json:"agent"`
	// Rulings are the per-item arbitrations.
	Rulings []domain.Arbitration `json:"rulings,omitempty"`
	// Escalation is the PO's decision on a bead that exhausted its rounds.
	Escalation *domain.Escalation `json:"escalation,omitempty"`
}

// Kind implements [Event].
func (ArbitrationReceived) Kind() Kind { return KindArbitrationReceived }

// PROpened is published when a pull request has been created for a bead
// (safety on, D-10) or when an external PR review bead is opened (D-11).
type PROpened struct {
	Meta
	// Bead is the delivered bead, or the review bead for an external PR.
	Bead domain.BeadID `json:"bead"`
	// URL is the pull request URL.
	URL string `json:"url"`
	// Number is the pull request number.
	Number int `json:"number"`
	// Branch is the head branch, e.g. "sc/sc-68s-add-retry".
	Branch string `json:"branch,omitempty"`
	// Base is the base branch, normally the default branch.
	Base string `json:"base,omitempty"`
	// External is true for a PR the cannon did not create (D-11).
	External bool `json:"external,omitempty"`
}

// Kind implements [Event].
func (PROpened) Kind() Kind { return KindPROpened }

// Merged is published when a bead's code has landed on the default branch:
// either the cannon fast-forwarded it (safety off) or the PR gate resolved
// after a human merge (safety on, D-20).
type Merged struct {
	Meta
	// Bead is the bead whose code landed.
	Bead domain.BeadID `json:"bead"`
	// SHA is the commit now on the default branch.
	SHA string `json:"sha,omitempty"`
	// Branch is the default branch name.
	Branch string `json:"branch,omitempty"`
	// PRNumber is the pull request that was merged, when there was one.
	PRNumber int `json:"pr_number,omitempty"`
	// Pushed is true when the merge was pushed to a remote.
	Pushed bool `json:"pushed,omitempty"`
}

// Kind implements [Event].
func (Merged) Kind() Kind { return KindMerged }

// Warning is published for anything recoverable that a human should still
// see: a cartridge that failed detection and was substituted, a gate check
// skipped because gh is missing, an invalid transition that was ignored.
type Warning struct {
	Meta
	// Source is the component that raised it ("cartridge", "gates", "state").
	Source string `json:"source,omitempty"`
	// Message is the human-readable text.
	Message string `json:"message"`
	// Err is the underlying error message, if any.
	Err string `json:"error,omitempty"`
	// Bead is the bead concerned, if any.
	Bead domain.BeadID `json:"bead,omitempty"`
	// Agent is the agent concerned, if any.
	Agent domain.AgentID `json:"agent,omitempty"`
}

// Kind implements [Event].
func (Warning) Kind() Kind { return KindWarning }

// Error is published for a failure the orchestrator could not recover from
// at the point it happened, even if the run as a whole continues (for
// example a bead moved to failed). It has the same shape as [Warning].
type Error struct {
	Meta
	// Source is the component that raised it.
	Source string `json:"source,omitempty"`
	// Message is the human-readable text.
	Message string `json:"message"`
	// Err is the underlying error message, if any.
	Err string `json:"error,omitempty"`
	// Bead is the bead concerned, if any.
	Bead domain.BeadID `json:"bead,omitempty"`
	// Agent is the agent concerned, if any.
	Agent domain.AgentID `json:"agent,omitempty"`
}

// Kind implements [Event].
func (Error) Kind() Kind { return KindError }
