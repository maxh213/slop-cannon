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
// during a review leaves the bead in_review instead of forcing it open.
//
// Exactly three bead-status writes in a run are not decided by that table:
//
//   - the scheduler's claim (P6-13): bd's `update --claim` is atomic and sets
//     in_progress itself, so [Scheduler.announceClaim] publishes that
//     transition rather than performing it. The hand-over release the claim
//     does first, when the bead carries an assignee that is not the agent
//     taking it, is part of the same exchange: it is only reached for a
//     ClaimNew demand, whose bead is open, so it is open -> open;
//   - the --resume reconciliation (P6-15): [gitx.Resumer] turns branch state
//     into bead state and writes it through its own gitx.ResumeTracker seam,
//     off this table. Most edges it takes D-07 has too (delivering -> closed,
//     X -> needs_human), but "the recorded pull request is no longer open or
//     merged" is reached from any non-closed state, failed included, which
//     D-07 has no edge out of. [Run.reconcileResume] therefore announces an
//     entry only when gitx marked it applied, so a write bd refused is a
//     warning and no event there too;
//   - the external review bead's close (D-11, P8-04): [PRReview.close]
//     writes `bd close` itself, because D-07 has no open -> closed edge and
//     an external pull request is never delivered by us, so walking the
//     bead through approved and delivering would record five transitions
//     that did not happen. It publishes its own [events.BeadStateChanged],
//     like the scheduler's claim does.
//
// The second and the third are also the two writes the P9-06 rules gate
// never sees, and [RulesGate.GuardStore] says why neither is the run
// shipping its own work. The first cannot ship anything: a claim lands the
// bead in_progress.
//
// The [Conductor] (run.go) is the glue above all of that — the answer to
// "what connects bd to those three work sources?" (P8-14). Beside the
// scheduler, once a second, it lists the run's beads, publishes the
// inventory and the graph the TUI relays out of (P6-32), offers the
// `bd ready` set to [Feedback.Track], rebuilds a resumed run's threads from
// the beads' own comments on its first pass ([Feedback.Restore], P8-16),
// queues the delivery of every approved bead — one at a time, so two
// deliveries never rebase and fast-forward the same repository at once —
// and routes each [DeliveryOutcome] back through [Conductor.ApplyOutcome],
// retrying a delivery the deliverer left in delivering and handing it to a
// person when the attempts are spent. It is also
// where the prompt context comes from ([Conductor.BeadContext], the
// [FeedbackOptions.Context] of the loop), which is why the D-14 leftovers
// commit and the P9-05 secret filter both sit there: it is the only
// production producer of the change set a prompt renders.
//
// The [GatePoller] (gates.go) is a sidecar rather than a loop: it resolves
// the gh:pr gates of delivered beads (D-20), taking the P6-12 guard on a
// status read off a listing from [ResolveBeadStatus] rather than
// re-implementing it. The [EventLog] (eventlog.go) is a [Publisher] the run's
// bus tees into, and [Summarise] folds it back for `sc status`.
//
// # Shared seams
//
// host.go owns the concepts more than one component needs — [Publisher],
// [EventHost], [RunHost] — so there is one definition of each; the narrow
// host interfaces ([DeliveryHost], [SupervisorHost]) embed [EventHost] and
// exist so a test can pass a small fake.
package orchestrator
