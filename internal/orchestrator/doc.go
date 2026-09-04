// Package orchestrator runs the cannon: run lifecycle, scheduler and ammo
// allocation, bead state machine, feedback loops, delivery, supervision and
// gate polling.
//
// # How the parts fit together
//
// A [Run] (run.go) owns the lifecycle: the run directory under the state
// dir (D-23), state.json, the lock, the live agent list and the transitions
// between running, paused, stopping and finished. It drives one [Loop].
//
// The [Scheduler] (scheduler.go) is that loop. Every tick it asks a
// [WorkSource] what wants doing, orders the [Demand]s by the D-05 priority,
// claims the beads (P6-13) and fires as many as the ammo allocation allows
// through a [Firer]. The three work sources of a real run are:
//
//   - [Planner] (planning.go) — the product owner's plan, the D-18 autopilot
//     re-planning and the run's stop condition. It is also the [Firer] for
//     its own [DemandPlan]; [PlanRouter] splits the two.
//   - [Feedback] (feedback.go) — one thread per bead: rounds, testers per
//     lens, consolidated engineer requests (P6-21), pushback, arbitration
//     and escalation. It is also the supervisor's [FiringPlanner].
//   - [Deliverer] (delivery.go) — approved beads to a pull request or a
//     merge, and the conflict rounds a rebase sends back (D-10). It is also
//     the [ResumeHandler] a resumed run acts through (P6-15).
//
// The [Supervisor] (supervisor.go) is the [Firer] of everything else: it
// picks the cartridge, installs the read-only bd shim (P6-17), applies the
// per-agent timeout, retries crashes, pauses rate-limited cartridges and
// retires exhausted ones (D-18).
//
// Every status write goes through the [StateMachine] (state.go), the pure
// D-07 table plus persistence, so a refused move is one warning and no
// write — the shutdown release of a held bead included, which is why a stop
// during a review leaves the bead in_review instead of forcing it open. The
// one exception is the scheduler's claim (P6-13): bd's `update --claim` is
// atomic and sets in_progress itself, so [Scheduler.announceClaim] publishes
// that transition rather than performing it. The [GatePoller] (gates.go) is
// a sidecar rather than a loop: it
// resolves the gh:pr gates of delivered beads (D-20). The [EventLog]
// (eventlog.go) is a [Publisher] the run's bus tees into, and [Summarise]
// folds it back for `sc status`.
//
// # Shared seams
//
// host.go owns the concepts more than one component needs — [Publisher],
// [EventHost], [RunHost] — so there is one definition of each; the narrow
// host interfaces ([DeliveryHost], [SupervisorHost]) embed [EventHost] and
// exist so a test can pass a small fake.
package orchestrator
