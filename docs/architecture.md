# Architecture

How `sc` is put together: what each package owns, what happens to one bead
from plan to merge, how ammo is divided, what states a bead can be in, and
what a resumed run does with the wreckage of the last one.

Everything here is one process. There is no daemon, no server and no
database of our own: the task graph lives in the target repository's `bd`
tracker (D-06), the code lives in git worktrees, and the only thing `sc`
keeps for itself is a run directory outside the repository (D-23).

Four tables in this document — the ammo allocation, the scheduler priority,
the bead statuses and the state machine's edges — are **generated from the
code** by `internal/docsgen` and checked by a test. See
[Generated tables](#generated-tables) at the end.

## The picture

```
                     you, a terminal
                            │
                            ▼
  ┌───────────────────────────────────────────────────────────────────┐
  │ internal/cli — cobra commands: run, resume, status, stop, logs,   │
  │ doctor, config, ammo, safety, vibes, rules, cartridge, version    │
  │   · merges flags + repo config + global config    (internal/config)│
  │   · takes the repository's run lock, builds every part below      │
  └───────────────────────────────┬───────────────────────────────────┘
                                  │ config.RunConfig
                                  ▼
  ┌──────────────────── internal/orchestrator ────────────────────────┐
  │ Run           lifecycle: state.json, lock, pause/stop, resume     │
  │  └ Scheduler  the loop: demands → free ammo slots → firings       │
  │      ├ Planner     product owner: the plan, D-18 re-planning      │
  │      ├ Feedback    one review thread per bead: rounds, pushback   │
  │      ├ Deliverer   approved bead → pull request or merge (D-10)   │
  │      ├ Conductor   once a second: bd inventory, graph, queues     │
  │      ├ Supervisor  fires an agent: cartridge, shim, timeout       │
  │      ├ StateMachine  the D-07 table; every status write goes here │
  │      └ GatePoller  the gh:pr gates of delivered beads (D-20)      │
  └────┬─────────────┬──────────────┬─────────────────┬───────────────┘
       │             │              │                 │
       ▼             ▼              ▼                 ▼
 internal/agents  internal/beads  internal/gitx   internal/events.Bus
 prompts, tool    the bd CLI:     git + gh:       one immutable value
 policies and     one serialised  worktrees,      per notable thing;
 the JSON reply   writer (D-21)   diffs, rebase,  Publish never blocks
 schemas          │               merge, PRs      │
       │          ▼                               ├──────────┬─────────┐
       ▼      .beads/ in the                      ▼          ▼         ▼
 internal/    target repository            internal/tui  EventLog   headless
 cartridge                                 bubbletea:    events     renderer
 claude, mock                              graph, agents .jsonl     (D-15)
       │                                   feedback, PRs    │
       ▼                                   log, cannon      ▼
 one subprocess per agent,                       │      sc status,
 in that bead's worktree                         │      sc logs
                                                 │
                                    run controls (p/s/r/x) call back
                                    into Run through tui.Controller
```

Read it as three directions of travel:

- **Down**: the CLI resolves configuration into a `RunConfig` and assembles
  the orchestrator; the orchestrator asks beads for work, git for worktrees
  and diffs, and a cartridge for an agent.
- **Right**: everything notable is published on the event bus. The bus fans
  out to the run log (`events.jsonl`), the TUI and, headless, a line
  renderer. A subscriber that falls behind drops events and increments its
  own counter — a stalled TUI can never slow the run down.
- **Back up**: only two things flow the other way. The TUI's run-control
  keys call `Run` through the `tui.Controller` seam, and `sc stop` signals
  the process holding the lock.

### Package responsibilities

| package | owns |
|---|---|
| `internal/cli` | the cobra command tree, flag/config resolution, run assembly, the run lock, `sc status`/`logs`/`doctor` |
| `internal/config` | TOML load/merge/save with the D-03 precedence, `RunConfig`, the D-05 `Allocate`, the state-dir paths (D-23) |
| `internal/domain` | the vocabulary: `Bead`, `BeadState`, `Role`, `Lens`, `Agent`, `Run`, the structured message types (D-25) |
| `internal/events` | the event model, the in-process `Bus`, the JSONL codec |
| `internal/beads` | every `bd` call, serialised through one writer (D-21): init, statuses, graph plans, claims, comments, gates, the read-only agent shim |
| `internal/gitx` | git and `gh`: worktrees, branches, diffs, the leftovers commit (D-14), rebase/merge, pull requests, resume reconciliation |
| `internal/agents` | prompts per role and round, tool policies, the JSON schemas an agent's reply must satisfy, secret filtering |
| `internal/cartridge` | the adapter contract (`Cartridge`, `RunSpec`, `Session`, typed error kinds) plus the `claude`, `cursor` and `mock` adapters (D-29) |
| `internal/orchestrator` | the run itself: lifecycle, scheduler, state machine, feedback, delivery, supervision, gates, run log |
| `internal/tui` | the bubbletea program, the headless renderer, the cannon animations |
| `internal/proc`, `internal/logging`, `internal/textsafe` | subprocess plumbing, run and agent logs, terminal-safe text |

## One bead's life

The columns are the bead's status before and after each step; the D-07
machine ([below](#bead-states-d-07)) is what makes each move legal.

```
 #  who            what happens                             bead status
 ─  ─────────────  ───────────────────────────────────────  ─────────────────
 1  planner (PO)   one agent reads the repo and answers      —
                   with a plan; `bd create --graph` mints
                   the epic and its children atomically
                   (D-22, D-24)                              → open
 2  conductor      lists the run's beads once a second,
                   publishes the inventory and the graph,
                   offers `bd ready` to the feedback loop    open
 3  scheduler      an engineer slot is free and the demand
                   wins the D-05 priority; `bd update
                   --claim` takes the bead atomically        open → in_progress
 4  supervisor     picks the cartridge, makes the worktree,
                   installs the read-only `bd` shim, runs
                   one subprocess in that worktree
 5  engineer       commits its own work; the orchestrator
                   commits any leftovers (D-14) so the
                   reviewers always see a committed diff     in_progress
 6  feedback       the round is submitted for review         → in_review
 7  testers        one per lens (ux, qa, test), up to the
                   review width the ammo affords; each
                   posts a `verdict` comment to the bead     in_review
 8a all approved   → step 10                                 → approved
 8b changes asked  the items are consolidated (one item per
                   place, D-08/P6-21) and handed back        → changes_requested
 9  engineer       answers the round: fixes, or pushes back.
                   A disputed item goes to the tester that
                   raised it; a tester that holds its ground
                   past max_pushback_rounds hands the item
                   to the PO, whose ruling is binding. Only
                   the lenses that did not approve re-review
                   (D-09) unless the diff moved more than
                   the re-review threshold. Round n+1        → in_progress → …
                   At max_review_rounds the PO rules on the
                   whole bead instead                        → escalated
10  deliverer      safety **on**: push `sc/<bead>-<slug>`,
                   open the PR, create a `gh:pr` gate        approved → delivering
                   safety **off**: rebase, run the check
                   command, fast-forward, push               approved → delivering
11a gate poller    the human merges the PR; the gate
                   resolves and the bead is closed (D-20)    delivering → closed
                   with auto_merge (D-31) the poller merges
                   the run's own green, CLEAN PR itself at
                   the head its testers approved (or GitHub's
                   clean update of it), at most one per
                   auto_merge_spacing; a failed
                   check or a conflict found there goes back
                   to the engineer                           delivering → changes_requested
11b safety off     the merge landed; `bd close` with
                   `sc_merge_sha`                            delivering → closed
12  went sideways  rebase conflict → engineer, as a conflict
                   round that does not count against the cap delivering → in_progress
                   check command failed → a qa feedback item delivering → changes_requested
                   no remote, no `gh`, PR closed unmerged    delivering → needs_human
```

Every step publishes: `bead_state_changed` for each move, `agent_spawned` /
`agent_finished` / `agent_failed` around each firing, `round_started`,
`verdict_received`, `response_received`, `arbitration_received`,
`pr_opened`, `merged`, and `warning` / `error` for anything that deserves a
human's eye. That stream is the whole of what the TUI and `sc status` know.

Two kinds of bead never take that delivery path: an **external PR** review
bead (D-11) — the cannon reviews pull requests it did not open, posts one
consolidated review with `gh pr review`, and never merges them, so the bead
is simply closed; a pull request already open when the run started is not
reviewed at all (D-31) — and a bead a person marked done with the TUI's `x` key.

A **brief** run (`sc <repo> -m "…"`) finishes when every bead it planned has
settled. An **autopilot** run (a bare repository path) never stops on its own
(D-18): when the ready queue empties the product owner plans more, and the
run ends only when you stop it or every cartridge is out of credits. With a
`signals_command` configured (D-32), the planner also runs that command
itself — never the product owner, whose shell stays read-only — at most once
per `signals_interval`, and hands every autopilot plan the last reading,
fenced as untrusted production evidence; a reading that changed since the
last plan saw one may ask for a plan while engineers are still busy. Either
way the lifecycle recorded in `state.json` is `new` → `running`
(⇄ `paused`) → `stopping` → `finished`.

## Ammo (D-05)

`ammo` is a concurrency cap, not a roster. One product-owner slot when one
is needed, `max(1, floor((ammo-1)/4))` engineers in flight, and every
remaining slot available to testers, so an engineer can have the ideal three
reviewers. The minimum is 3: one of each.

<!-- BEGIN GENERATED: d05-allocation (internal/docsgen) -->

| ammo | PO | engineers in flight | testers | testers per bead |
| ---: | ---: | ---: | ---: | ---: |
| 3 | 1 | 1 | 1 | 1 |
| 4 | 1 | 1 | 2 | 2 |
| 5 | 1 | 1 | 3 | 3 |
| 8 | 1 | 1 | 6 | 3 |
| 9 | 1 | 2 | 6 | 3 |
| 10 | 1 | 2 | 7 | 3 |
| 12 | 1 | 2 | 9 | 3 |
| 13 | 1 | 3 | 9 | 3 |
| 16 | 1 | 3 | 12 | 3 |
| 17 | 1 | 4 | 12 | 3 |

<!-- END GENERATED: d05-allocation -->

`testers per bead` is the review width: one tester per lens (ux, qa, test),
fewer when the ammo cannot afford three, never zero.

PO + engineers + testers always add up to exactly `ammo`, so honouring the
per-role caps is what keeps live agents within budget. A slot is taken when
the scheduler dispatches, on the loop goroutine, and given back when the
agent's goroutine returns — finished, failed, cancelled or panicked — so a
crashing agent frees its slot and the counts cannot drift.

When slots free up, demands are served in this order:

<!-- BEGIN GENERATED: d05-priority (internal/docsgen) -->

| order | demand | role | event task |
| ---: | --- | --- | --- |
| 1 | `arbitrate` | po | `arbitrate` |
| 2 | `review` | tester | `review` |
| 3 | `implement` | engineer | `implement` |
| 4 | `plan` | po | `plan` |

<!-- END GENERATED: d05-priority -->

`arbitrate` comes first because a held item blocks a whole thread;
`review` beats `implement` because a bead in review is nearly done; and
`plan` is last, because planning is what makes more work.

## Bead states (D-07)

The workflow state of a bead **is** its `bd` status (D-06). Nothing that
matters for a resume is held only in memory. The statuses `sc` does not get
from `bd` are registered at run start with `bd config set status.custom`,
merged with whatever the repository already had, never replacing it.

<!-- BEGIN GENERATED: d07-states (internal/docsgen) -->

| status | bd category | origin | terminal |
| --- | --- | --- | --- |
| `open` | `active` | bd built-in | no |
| `in_progress` | `wip` | bd built-in | no |
| `in_review` | `wip` | registered by `sc` | no |
| `changes_requested` | `wip` | registered by `sc` | no |
| `approved` | `wip` | registered by `sc` | no |
| `delivering` | `wip` | registered by `sc` | no |
| `closed` | `done` | bd built-in | yes |
| `escalated` | `frozen` | registered by `sc` | no |
| `needs_human` | `frozen` | registered by `sc` | yes |
| `failed` | `frozen` | registered by `sc` | yes |

<!-- END GENERATED: d07-states -->

The `wip` and `frozen` categories are what keep these beads out of
`bd ready` without any filtering of our own. `bd`'s own `blocked`, `hooked`,
`deferred` and `pinned` statuses are read but never set, and a status of the
repository's own making is left strictly alone.

```
   ┌──────┐ claimed  ┌─────────────┐ submitted ┌───────────┐ approved
   │ open │─────────▶│ in_progress │──────────▶│ in_review │─────────┐
   └──────┘          └─────────────┘           └───────────┘         │
                                                                     ▼
   ┌────────┐ delivered ┌────────────┐ delivery_started ┌──────────┐ │
   │ closed │◀──────────│ delivering │◀─────────────────│ approved │◀┘
   └────────┘           └────────────┘                  └──────────┘

   the review loop (D-08)
     in_review          ──changes_requested──▶ changes_requested
     changes_requested  ──rework_started─────▶ in_progress ──submitted──▶ …
     in_review          ──escalated──────────▶ escalated
     changes_requested  ──escalated──────────▶ escalated
     escalated          ──scope_narrowed─────▶ in_review
     escalated          ──approved_with_notes▶ approved

   delivery going sideways (D-10)
     delivering         ──conflict───────────▶ in_progress
     delivering         ──check_failed───────▶ changes_requested
     needs_human        ──delivery_retried───▶ delivering

   letting go
     in_progress        ──released───────────▶ open
     any non-terminal   ──blocked────────────▶ needs_human
     any non-terminal   ──failed─────────────▶ failed
```

The full machine, which is what `orchestrator.Transition` implements:

<!-- BEGIN GENERATED: d07-transitions (internal/docsgen) -->

| from | event | to |
| --- | --- | --- |
| `open` | `claimed` | `in_progress` |
| `open` | `blocked` | `needs_human` |
| `open` | `failed` | `failed` |
| `in_progress` | `released` | `open` |
| `in_progress` | `submitted` | `in_review` |
| `in_progress` | `blocked` | `needs_human` |
| `in_progress` | `failed` | `failed` |
| `in_review` | `changes_requested` | `changes_requested` |
| `in_review` | `approved` | `approved` |
| `in_review` | `escalated` | `escalated` |
| `in_review` | `blocked` | `needs_human` |
| `in_review` | `failed` | `failed` |
| `changes_requested` | `rework_started` | `in_progress` |
| `changes_requested` | `escalated` | `escalated` |
| `changes_requested` | `blocked` | `needs_human` |
| `changes_requested` | `failed` | `failed` |
| `approved` | `delivery_started` | `delivering` |
| `approved` | `blocked` | `needs_human` |
| `approved` | `failed` | `failed` |
| `delivering` | `conflict` | `in_progress` |
| `delivering` | `check_failed` | `changes_requested` |
| `delivering` | `delivered` | `closed` |
| `delivering` | `blocked` | `needs_human` |
| `delivering` | `failed` | `failed` |
| `escalated` | `scope_narrowed` | `in_review` |
| `escalated` | `approved_with_notes` | `approved` |
| `escalated` | `blocked` | `needs_human` |
| `escalated` | `failed` | `failed` |
| `needs_human` | `delivery_retried` | `delivering` |

<!-- END GENERATED: d07-transitions -->

Three properties are worth knowing:

- It is a **total function**: every (state, event) pair either returns a
  state or a typed error, and nothing panics. There are no self-edges, so
  re-applying an event that already happened is a refusal the caller can
  see rather than a silent success.
- A refused move is one `warning` event and **no write**. That is why
  stopping a run in the middle of a review leaves the bead `in_review`
  instead of forcing it back to `open`.
- `needs_human` is terminal in the sense that the cannon will not pick the
  bead up again on its own, but a person can: `delivery_retried` is the edge
  the TUI's retry key and the resume reconciliation take.

Exactly three status writes in a run are not decided by this table: the
scheduler's atomic `bd update --claim` (which sets `in_progress` itself, so
the transition is announced rather than performed), the resume
reconciliation's "the pull request is gone" edge, and the external-review
bead's close (D-11). Each publishes its own `bead_state_changed`.

## Resume

A run is resumable because almost nothing about it is in memory:

| what | where it lives |
|---|---|
| the task graph and every bead's state | `bd`, in the target repository (D-06) |
| verdicts, responses, rulings, reports | bead comments, one fenced JSON block each (D-25) |
| run bookkeeping (round, PR number, lease, agent) | bead metadata, `sc_*` keys |
| the frozen configuration of the run | `<run-dir>/state.json` |
| everything that was published | `<run-dir>/events.jsonl` |
| each bead's work in progress | `<run-dir>/wt/<bead-id>`, a git worktree on `sc/<bead-id>-<slug>` |

```
<state-home>/slop-cannon/<repo-id>/
├── run.lock                     one run per repository at a time
└── runs/<run-id>/
    ├── state.json               phase, frozen config, live agents, seq
    ├── events.jsonl             every event, in order (+ .1 when rotated)
    ├── sc.log                   the orchestrator's slog output
    ├── agents/<agent-id>.log    one transcript per agent
    ├── bin/                     the read-only bd shim each agent gets
    └── wt/<bead-id>/            one git worktree per bead
```

`sc resume` (or `sc run --resume`) picks the most recent run of the
repository and:

1. **Restores the frozen configuration** from `state.json` rather than
   re-reading the config files, so a resumed run behaves like the original
   even if `ammo` or `safety` changed on disk in the meantime.
2. **Reattaches the worktrees**, recreating from the branch any that the
   directory lost, and clearing a stale `.git/index.lock` left behind by a
   process that was killed mid-commit or mid-rebase — without that, every
   later firing on that bead fails before it reaches the cartridge.
3. **Reconciles git into bead state**, which is the part that needs care:
   - a `delivering` bead whose branch tip has landed on the default branch
     is closed with `sc_merge_sha` — the crash hit after the fast-forward.
     A branch that is an ancestor only because it never received a commit
     does not count as landed;
   - a bead with a pull request, open or merged, stays `delivering`: the
     delivery is re-run (it is idempotent) so the `gh:pr` gate exists, and
     the poller closes the bead when the merge is seen;
   - a bead whose recorded pull request is no longer open or merged moves to
     `needs_human` — somebody closed it without merging;
   - a `needs_human` bead whose branch has acquired a pull request nobody
     recorded goes back to `delivering` with the PR written down: the
     delivery could only push, and the human opened the PR themselves;
   - a `delivering` bead with no branch and no pull request left to speak
     for it moves to `needs_human`;
   - every other state is left alone, with the worktree's condition (dirty,
     mid-rebase with conflicts) reported for the orchestrator to act on.
   Worktrees belonging to beads that are not part of this run are reported
   as orphans and never touched.
4. **Rebuilds the review threads** from the beads' own comments on the
   conductor's first pass, so round numbers, held items and pushback
   history survive.
5. **Hands the leftovers back to the loop**: a mid-rebase worktree becomes
   a conflict round for an engineer, an interrupted delivery is queued
   again.

A resume that cannot even list the repository is fatal — the loop would
work from a wrong picture — but a per-bead failure is a warning and the run
goes on.

The lock is what makes this safe: `run.lock` lives in the repository's state
directory (never in the repository), carries the holder's pid, and a run
whose process is gone leaves a stale lock that `sc status` reports and a
resume takes over.

## Generated tables

The four tables above are rendered from the code by `internal/docsgen`:

| table | authority |
|---|---|
| ammo allocation | `config.Allocate` and `orchestrator.CapsFor(...).TestersPerBead` |
| scheduler priority | `orchestrator.DemandKinds` |
| bead statuses | `domain.WorkflowBeadStates` |
| transitions | `orchestrator.BeadTransitions` |

Each sits between a pair of `BEGIN GENERATED` / `END GENERATED` HTML
comments; everything outside them is written by hand. Change an edge or an
allocation row in the code and `TestArchitectureTablesAreCurrent` fails until
this file is regenerated, which `make update-golden` (or
`UPDATE_GOLDEN=1 go test ./internal/docsgen/`) does in place.
