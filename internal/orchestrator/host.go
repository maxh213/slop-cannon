package orchestrator

import (
	"log/slog"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// The seams every component of the run shares (the wave-1 reconciliation).
//
// The package is assembled from parts written against interfaces rather
// than against each other: the scheduler (P6-02) fires through a [Firer],
// the supervisor (P6-07) plans a firing through a [FiringPlanner], the
// feedback loop (P6-04), the planner (P6-05) and the deliverer (P6-06)
// offer work through a [WorkSource], and every one of them reaches the run
// itself only through the narrow host interfaces below. This file owns the
// concepts more than one of them needs, so that there is exactly one
// definition of each:
//
//   - [Publisher] — anything an event can be handed to (the bus, the run,
//     the run log, a [Tee] of them). It is the only event-sink type in the
//     package.
//   - [EventHost] — the run's event stream as a component sees it: a
//     run-stamped header, a place to publish, and the run's logger.
//     [DeliveryHost] and [SupervisorHost] embed it, and the gate poller
//     takes it as its host.
//   - [RunHost] — everything *Run offers. Nothing takes it as a parameter
//     (a component asks for the slice it uses, which is what lets tests
//     pass small fakes); it is the one place the whole surface is written
//     down, and the compile-time assertion that *Run satisfies every slice.
//
// Two package-wide conventions hold everything else together. A component
// names the events.Warning/events.Error Source it stamps in one exported
// constant beside its own type, all spelled Source<Component>
// ([SourceRun], [SourceScheduler], [SourcePlanning], [SourceSupervisor],
// [SourceFeedback], [SourceDelivery], [SourceGates], [SourceState]); and a
// component that reads or writes bd asks for the narrowest interface it
// uses ([StateStore], [ClaimStore], [LeaseRenewer], [LeaseReleaser],
// [GateStore], [GateBeadReader], [FeedbackMessageStore], [PlanPrioritiser]),
// each asserted against *beads.Client where it is declared, so a test can
// fake one method without standing in for the whole client.

// Publisher is the event sink of this package: *events.Bus, [*Run],
// [*EventLog] and [Tee] all satisfy it.
type Publisher interface {
	Publish(ev events.Event)
}

// PublisherFunc adapts a function to [Publisher].
type PublisherFunc func(events.Event)

// Publish implements [Publisher].
func (f PublisherFunc) Publish(ev events.Event) { f(ev) }

// EventHost is the run's event stream as a component of the run sees it:
// the header that stamps an event with the run id and the current time,
// the sink the event goes to, and the run's logger. *Run satisfies it.
type EventHost interface {
	Publisher
	// Meta stamps an event with the run id and the run's clock.
	Meta() events.Meta
	// Log is the run's logger.
	Log() *slog.Logger
}

// RunHost is the whole slice of [Run] the run's components use. Each of
// them asks only for the part it needs — [EventHost] for the gate poller,
// [DeliveryHost] for the delivery handoff, [SupervisorHost] for the agent
// supervisor — so that a test can pass a small fake; this interface exists
// to write the full surface down in one place and to pin, at compile time,
// that *Run really is the production host for every one of them.
type RunHost interface {
	EventHost

	// ID is the run id.
	ID() domain.RunID
	// Now is the run's clock.
	Now() time.Time
	// Dirs are the run's directories (D-23).
	Dirs() RunDirs
	// Snapshot is the frozen configuration the run behaves by.
	Snapshot() domain.ConfigSnapshot
	// Warn publishes an events.Warning from source.
	Warn(source, message string, err error)

	// AgentStarted, AgentUpdated and AgentStopped keep state.json's live
	// agent list honest; Agents reads it back.
	AgentStarted(a domain.Agent) error
	AgentUpdated(a domain.Agent) error
	AgentStopped(id domain.AgentID) error
	Agents() []domain.Agent
}

// *Run is the production host for every slice of it this package defines.
var (
	_ RunHost        = (*Run)(nil)
	_ EventHost      = (*Run)(nil)
	_ Publisher      = (*Run)(nil)
	_ DeliveryHost   = (*Run)(nil)
	_ SupervisorHost = (*Run)(nil)
)

// mergeEnv returns base with the entries of extra that base does not
// already set. base wins: it is the environment the run computed for the
// firing (the bd shim's PATH, the run identity), and extra is whatever the
// caller's context suggested.
func mergeEnv(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

// BeadStateSettled reports whether a bead in this state needs nothing more
// from the run: it is closed, parked for a human, failed, or in a bd status
// the cannon never drives (blocked, hooked, deferred, pinned — D-07). An
// escalated bead is not settled: the product owner still owes it a ruling
// (D-08). [BeadSettled] is the same question about a whole bead, and also
// counts the run's bookkeeping beads as settled.
//
// A state that is not a bd status at all — the empty string a listing bd
// could not parse leaves behind, or a status from a newer sc — is never
// settled (P6-36). This answer ends a brief run (planSettlement) and
// counts the beads `sc status` says are done, so the safe reading of "I do
// not know what this is" is "the run is not finished with it".
func BeadStateSettled(s domain.BeadState) bool {
	if !s.Valid() {
		return false
	}
	return s.Terminal() || !s.Workflow()
}
