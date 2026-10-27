# Architecture

How `sc` is put together: what each package owns, what happens to one bead
from plan to merge, how ammo is divided, what states a bead can be in, and
what a resumed run does with the wreckage of the last one.

The orchestrator is one process. `sc dash` is a separate, explicitly started
read-only observer with an HTTP server; there is no always-on daemon and no
database of our own: the task graph lives in the target repository's `bd` tracker (D-06),
the code lives in git worktrees, and the only thing `sc` keeps for itself is
a run directory outside the repository (D-23).

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
| `internal/cli` | the cobra command tree, flag/config resolution, run assembly, the run lock, `sc status`/`logs`/`dash`/`doctor` |
| `internal/config` | TOML load/merge/save with the D-03 precedence, `RunConfig`, the D-05 `Allocate`, the state-dir paths (D-23) |
| `internal/domain` | the vocabulary: `Bead`, `BeadState`, `Role`, `Lens`, `Agent`, `Run`, the structured message types (D-25) |
| `internal/events` | the event model, the in-process `Bus`, the JSONL codec |
| `internal/beads` | every `bd` call, serialised through one writer (D-21): init, statuses, graph plans, claims, comments, gates, the read-only agent shim |
| `internal/gitx` | git and `gh`: worktrees, branches, diffs, the leftovers commit (D-14), rebase/merge, pull requests, resume reconciliation |
| `internal/agents` | prompts per role and round, tool policies, the JSON schemas an agent's reply must satisfy, secret filtering |
| `internal/cartridge` | the adapter contract (`Cartridge`, `RunSpec`, `Session`, typed error kinds), the `claude`, `cursor`, `agy`, `codex`, `kimi`, `grok`, `junie`, `hermes`, `kilo` and `mock` adapters, and `dandelion`'s policy- and binary-aware driver dispatch (D-30, D-37, D-40) |
| `internal/orchestrator` | the run itself: lifecycle, scheduler, state machine, feedback, delivery, supervision, gates, run log |
| `internal/tui` | the bubbletea program, the headless renderer, the cannon animations |
| `internal/dash` | the read-only browser observer: event follower, cached redacted snapshots, HTTP Host validation and server |
| `internal/proc`, `internal/logging`, `internal/textsafe` | subprocess plumbing, run and agent logs, terminal-safe text |

### Kilo policy and the Dandelion balance boundary

The direct `kilo` adapter (`internal/cartridge/kilo`) targets Kilo CLI 7.5.6.
It runs `kilo run … --pure --auto --format json --agent sc --dir <workdir>`
with the complete combined prompt on stdin. Each child gets a generated
in-memory agent configuration, `KILO_NO_DAEMON=1`, and disabled project
configuration and automatic updates. The generated permissions support
`read_exec` and `read_write`; `read_only` is refused because Bash is not a
read-only execution boundary. Timeout, cancellation and kill use process and
descendant cleanup without issuing daemon lifecycle commands.

Only at the CLI argument boundary, the pinned Dandelion model
`deepseek/deepseek-v4.1-flash-1017` becomes
`kilo/deepseek/deepseek-v4.1-flash-1017`. Already-qualified and direct custom
IDs are not rewritten, and `RunSpec.Model` and the Dandelion launch log retain
the unqualified route model. Real direct checks covered structured output,
native file creation, large-prompt stdin, and timeout/kill behavior;
delegated checks covered fresh profiles, below-allowance cost and fail-closed
rerouting.

Dandelion registers the same adapter as `DriverKilo` for `read_exec` and
`read_write`; its `read_only` route set remains Claude-only. Kilo is available
only to the standard route, never the high chain. The delegated route mapper
passes a supported effort as `--variant` and sets
`SC_KILO_MIN_BALANCE_USD=10`. That control is Dandelion-only and is removed
from the Kilo child environment.

A cached Kilo route is not launch authorization: sc discards it and obtains a
fresh route. Immediately before every actual Kilo launch, the adapter makes a
fresh `kilo profile --json` request with a 20-second timeout. A profile
failure, malformed or missing balance, invalid floor, or balance at or below
the floor fails closed; Dandelion excludes Kilo and reroutes without starting
its provider process. The launch allowance is `balance - floor`, checked
against cumulative assistant-message cost when present and root step cost
otherwise. This is a best-effort in-session guard, not an account-wide
spending guarantee: a final request can overshoot before its cost is
reported, concurrent sessions can spend the same balance, and other clients
or later billed steps can still take the account below $10.

### Codex capability versus account availability

Codex and Codex Animus have real `read_exec` and `read_write` drivers. A
personal Codex logout and the Codex Animus app-server probe error are current
account availability issues, not missing adapters. `codex-animus` is not in
Dandelion's high chain.

## Read-only browser dashboard

`sc dash` attaches a second process to one selected run directory. It never
claims the run lock, writes run state, reconciles beads or attaches a
`tui.Controller`. Beneath its normalised base path it serves only the page,
`GET /api/snapshot` and `GET /health`; no route accepts a run id or filesystem
path, and there are no control endpoints.

One `dash.Observer` owns one `FollowEventLog` stream and one `tui.State` fold.
The follower replays the rotated segment and then the live log, follows
rotations and partial-line completion, and uses `FollowResumed` so a
`RunFinished` record does not end the stream: a later resumed incarnation
continues the same fold without replaying it on every refresh. Once a second
the observer refreshes `state.json` and the hardened lock/liveness probes and
publishes a new revision of its cached snapshot. HTTP handlers return a deep
copy of that cache; they do no file I/O and never replay the log. A transient
state-read failure leaves the last good durable snapshot in place, and a
closed event stream does not shut down the server.

Snapshots use explicit, allow-listed DTOs with bounded collection windows,
full totals plus omitted counts, and capped, terminal-safe text. They carry
workflow and lifecycle metadata, not briefs, configuration documents,
prompts, diffs, agent output or error prose, review reports and transcripts,
or raw warning/error strings. A notice contains only its time, kind, source
and one fixed category: `rate_limited`, `conflict`, `check`, `review` or
`generic`. Lifecycle activity is rendered from allow-listed event metadata,
not the TUI's transcript renderer.

### Host resources and quota-time presentation

An always-on host collector feeds complete samples through the main observer
select every two seconds. On Linux it reads `/proc/stat`, `/proc/meminfo`, the
root filesystem and the hostname. CPU usage is an aggregate and bounded
per-core delta between two samples with matching topology (at most 256 cores,
with omitted counts); memory, swap and root-disk usage are used/total byte
pairs. A zero-swap system omits `swap`. Only a complete successful sample
replaces the cached last-good value; an unsupported platform leaves `host`
absent, and collection failure does not change observer health.

The Dandelion frontend treats missing, malformed and zero timestamps as
absent. It emits no year-one date or countdown and clears the corresponding
text, `title` and `dateTime` metadata when content is cleared.

### Exact-attempt Dandelion route labels

The dashboard's routed-model label is observer metadata, not another routing
decision and not a change to `domain.Agent.Model`. For a Dandelion agent that
existing field remains the requested `high`/`standard` configuration; it is
never presented as the model actually selected. The observer backfills the
selected run's ordered structured `sc.log` from the beginning, then follows
complete appended lines. Route-log results enter the same `Observer.Run`
select as event and quota results. Parsing is read-only, bounded and
metadata-only; HTTP handlers still serve only deep-copied cached snapshots
and never read either log.

Each `agentLaunch` freezes `spawnAt`, the current direct event start, and the
previous direct event start, each boundary retaining its serial and timestamp.
Generation selection uses the timestamp interval **`(previous event start,
current event start]`** — previous excluded, current included — and only then
selects exact agent/supervisor-attempt history within that generation.

The current start and spawn time must be nonzero, and the current start must
not be after spawn. A nonzero previous boundary must have a different serial,
a nonzero timestamp, and a time strictly before the current start. With a
previous boundary, exactly one retained direct start may satisfy
`previous.at < start.at <= current.at`; zero or multiple matches are `unknown`.
Without a previous boundary, the latest unambiguous eligible start is used.
Equal or reversed boundaries are invalid, and two direct starts in one
interval are ambiguous.

A direct `agent fired` establishes the exact attempt within the selected
generation, and only subsequent direct `dandelion routed a firing` records in
that context supply account, model, effective effort and route time, with a
per-attempt structured-session ordinal. Prior generations' history is
preserved rather than reset. The latest ordinal is selected only within one
exact attempt, never globally or by a latest-route guess. Agent name alone, a
timestamp without this launch-interval correlation, nested output, error prose
and the global quota snapshot's `route`/`route_high` never establish or join a
route. A new fired attempt starts with no route metadata, so it cannot inherit
an earlier attempt's model.

This fixes rotation when **five direct starts have only four retained event
starts**: current-run replay recovers generation five, and a last-seen card
keeps its own frozen interval and model four. Older cards retain their own
valid frozen intervals and models rather than inheriting a newer generation's
route. Arrival skew — the event start is ahead of `sc.log` — is `unknown` and
must not select the previous generation.

`AgentSnapshot` always includes `route_status`: empty for non-Dandelion agents
and otherwise one of `routed`, `pending`, `no_route` or `unknown`. Optional
bounded `routed_model`, `routed_account`, `routed_effort`, `routed_at`,
`route_attempt` and `route_ordinal` carry the selected route metadata; the
browser accepts a routed label only for the matching incarnation and
`route_attempt`. The states have deliberately narrow meanings:

- `routed`: at least one route is proven for this exact attempt, and the
  latest selected route and ordinal are exposed.
- `pending`: the exact fired attempt is live and has zero routes.
- `no_route`: the exact fired attempt has zero routes and a typed
  `rate_limited` failure. A warning's wording, a prose match or a failure from
  a mixed multi-session or different attempt never establishes this state.
- `unknown`: the exact history is missing, ambiguous or damaged, including a
  duplicate attempt within one generation or an invalid generation interval.

Replay and following are part of correlation, not best-effort cleanup.
Malformed, wrong-run, nested and oversized records, truncation or replacement,
and context/route cap overflow are detected. A partial trailing line waits for
its newline. Missing, ambiguous, malformed, overflowing or damaged history is
`unknown`, never guessed into `pending`, `no_route` or `routed`, and old
metadata is never guessed forward. This restores valid routes previously
suppressed after resumes without changing adapters or `domain.Agent.Model`.
Route-log damage changes the affected labels only, never observer `/health`.

The routed subtitle has the form
`engineer · claude-opus-5-5 · dandelion/claude · effort max`; the fixed
fallbacks are `dandelion · routing…`, `dandelion · no route` and
`dandelion · route unknown`. A multi-session attempt may expose its route
count in a tooltip. Selection does not prove delegated process start or
success. Non-Dandelion rendering is unchanged, route text uses safe DOM APIs
and `textContent`, and the existing responsive layout and essential-text
contrast requirements apply.

### Optional Dandelion quota

`--dandelion` opts into quota collection; without it the snapshot's
`dandelion` member is omitted and the panel is hidden. `--dandelion-refresh`
is a Go duration with a default of `10m` and a CLI-enforced minimum of `1m`.
An enabled observer probes immediately and then sequentially, with no
overlapping collection, using `dandelion --json --max-age
<refresh-seconds>`, a fixed five-minute timeout and bounded captured output.
The age argument lets Dandelion reuse a fresh cache; an older cache requires
real external provider probes. `sc` itself has no Dandelion disk cache: its
last-good normalized result lives only in the observer process.

Collection results enter the existing `Observer.Run` select, so the observer
goroutine applies them beside its ordinary publish/fold work rather than
letting a collector or HTTP handler mutate that state concurrently. Every
attempt updates `last_attempt_at`; only a valid result updates
`last_success_at`. A failure therefore cannot refresh the apparent age of
last-good data. The retained result can become stale while `probing`
describes work currently in flight. Requests never trigger probes, including
when data is absent, stale or from a failed attempt.

When enabled, the `dandelion` object contains only this allow-listed shape:

- Quota metadata: `probing`, `last_attempt_at` and `last_success_at` when
  available, `stale`, an optional safe `error`, `generated_at`, `route`,
  `route_high`, bounded `providers`, and `providers_omitted`. The only error
  values are `not_installed`, `timed_out`, `probe_failed` and
  `invalid_snapshot`.
- Each provider: `id`, `display_name`, `status`, `plan_label`, `eligible`
  (defaulting to true when the input omits it), bounded `reason`,
  `fetched_at`, optional `balance`, bounded `windows`, and `windows_omitted`.
  A projected balance contains only `amount` and `currency`, with literal `$`
  normalized to `USD`; invalid balances are omitted and valid balances are
  deep-copied like every other projected field.
- Each window: `label`, `kind`, numeric `used_pct`, `resets_at`, and
  `projected_full_at`, using the same snake_case and timestamp conventions as
  the rest of the snapshot API.

Provider, balance and window data are deep-copied, and response, string and
collection bounds are enforced before publication. The DTO never serializes
the raw Dandelion response, notes, `routesError`, unknown fields, or raw
collector subprocess stderr/`err.Error()` text. A missing binary, timeout,
failed probe or invalid response is represented only by its fixed code.
Separately, an unavailable/error provider's bounded `reason` is explicitly
allowlisted Dandelion diagnostic text, not collector subprocess stderr.

The Marestail-like panel renders both routes, probing state, attempt and
success ages, and the current safe error. Provider rows include display name,
plan, status, reason and routing-off state; every window includes its label,
an accessible gauge with a clamped percentage, and reset and projected-full
countdowns. Tones are `calm` below 50%, `warm` from 50% to below 80%, `hot`
from 80% to below 95%, and `crit` at 95% or above. A balance is independent
of that window presentation: it produces no synthetic gauge, reset,
percentage or binding score. The literal-dollar normalization regression is
covered by `TestDandelionLiteralDollarBalanceProjectionAndCloneIsolation`.
The final live balance was `31.232105 USD`, rendered as `balance $31.23`
exactly once with no meter or percentage. A `usage only` toggle is on by
default and persists in `localStorage`; zero-percent windows count as usage,
and balance-bearing providers such as Kilo remain visible with zero windows.
Quota content uses safe DOM APIs and `textContent`, never untrusted markup,
with a responsive layout and at least 4.5:1 contrast for essential text.

### Health and network boundary

`/health` reports observer status, stream, revision and last event only. It
stays `ok` for a quiet, finished or crashed run; run status and lock liveness
are separate snapshot fields. Dandelion collection failures likewise affect
only the quota snapshot, never observer health. `quiet` means a running run
has had no event for two minutes, not that its process died. The listener
defaults to `127.0.0.1:8790`, and every request's HTTP `Host` must be loopback
or an exact hostname/IP supplied by repeatable `--allowed-host` (wildcards,
schemes, ports and paths are refused). That blocks DNS-rebinding requests but
is not authentication: shared use belongs behind an authenticating reverse
proxy, normally with the dashboard still listening on loopback.

The dashboard service retains `ProtectHome=read-only` and
`ProtectSystem=strict`. Its only Kilo write exceptions are
`ReadWritePaths` for `~/.local/share/kilo`, `~/.cache/kilo` and
`~/.local/state/kilo`, needed by Kilo's runtime startup/auth paths; they do
not create a general home write or a Dandelion quota cache.

## One bead's life

The columns are the bead's status before and after each step; the D-07
machine ([below](#bead-states-d-07)) is what makes each move legal.

```
 #  who            what happens                             bead status
 ─  ─────────────  ───────────────────────────────────────  ─────────────────
 1  planner (PO)   one producer (N in a combined round)      —
                   reads the repo; the reconciled plan is
                   applied with `bd create --graph`, which
                   mints the epic and children atomically
                   (D-22, D-24, D-41)                       → open
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
                   the head its testers approved (or the
                   update it asked GitHub for), at most one per
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

The shim's direct proof that bd enforces `--readonly` is cached per binary
identity. A process-wide double-checked mutex makes the load/probe/store
sequence atomic, so concurrent first installs cannot all run that write
probe; a failure is not cached and the next caller retries. This coordinates
only one `sc` process; independent processes can still overlap and rely on
bd's own locking.

Conflict and check recovery clears the conductor's `shipped` mark as well as
its in-flight delivery (`Conductor.release`). The send-back and re-approval
can both happen between two bd listings; without that reset no non-approved
listing would clear the mark, and the newly approved bead would sit
undelivered.

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

## Parallel PO planning (D-41)

`po_planners` defaults to **1**, which deliberately preserves the legacy
pipeline: the exact old planning demand has zero `Slots` (interpreted as
one), `po-plan-1` is the only producer, the graph cap is 8, and there is no
semantic judge, candidate namespacing, or contributor list. Q-04
deprioritisation is offered only on this path. Animus intends `po_planners =
2`; no deployment of that intended setting is implied.

Above 1, one planning demand coordinates N independent read-only producers.
Their IDs are unique (`po-plan-1-p1`, `po-plan-1-p2`, …), and their candidate
keys and local dependencies are namespaced `p1:…`, `p2:…` by planner
position, not completion order. `produceParallel` waits for **every**
producer before reviewing or writing anything. A failed producer warns
`a planning producer failed; retaining the other candidates`; one valid
survivor is enough. If none survives, the errors are joined and no judge or
graph apply runs.

After that barrier, `freshPlanSnapshot` reads the current run graph without
planning infrastructure. The judge and deterministic reconciler share that
fresh snapshot rather than the potentially stale producer-prompt snapshots.
The combined proposed **and** accepted cap is
`N × max_beads_per_plan` (**16** at N=2 with the default 8). Exceeding it
returns `agents.ErrCombinedPlanQuota`; no graph is trimmed to fit.

The combined round makes at most one semantic judge attempt, skipped when
deterministic matching resolves everything. This is read-only PO work
(`events.TaskPlanReview`) with a prose-free `decisions` contract: the judge
classifies candidates as `duplicate`, `conflict`, or `independent`, with an
optional `canonical` reference used only for duplicates. It cannot rewrite
plans, IDs, prose, dependencies, or quota. There is deliberately no judge
retry loop, even for malformed output. A judge failure warns
`the semantic plan judge failed; using the no-retry reconciliation fallback`;
failure or an invalid response selects that fallback, which retains
deterministic duplicate resolution and treats the remaining candidates as
independent without another semantic attempt. When a judge answer is
otherwise usable, a candidate it leaves unresolved becomes `conflict`; its
dependents also become conflicts rather than retaining a dangling edge. A
valid `conflict` is the judge's fail-closed answer to uncertainty, not a
retryable error.

Deterministic reconciliation prefers existing normalized-title matches, then
the earliest candidate. A duplicate canonical target must be another known
candidate or a fresh open existing bead. Dependencies on a canonical
candidate are remapped to its key; dependencies on an existing canonical
bead become external `to_id` edges. Invalid references, duplicate
classifications, unknown decisions, and prose/graph edits are rejected.
Existing canonical beads are only read, never rewritten, and multi-producer
planning performs no Q-04 deprioritisation.

All contributors in one round share one signals reading; the next round
takes at most one new reading, not one per contributor. The signals-backlog
threshold uses the combined cap: at N=2, 8 outstanding beads still permits a
signals plan while 16 blocks it; at N=1, 8 blocks it. The planning demand's
weight is N, while zero retains the legacy one-slot meaning. A combined
round reserves all N slots atomically, never partial capacity; `usedSlots`
cannot exceed `ammo`, and a round larger than ammo never reserves. Planning
and arbitration have independent caps. A planning cap above ammo is served
in ammo-sized waves, but an individual weighted round still must fit whole.
The CLI passes `cfg.POPlanners` to both the planner and scheduler.

The retained `planningCommit` makes graph application recoverable. Adoption
first uses positive returned-ID evidence, otherwise a fresh run-scoped
listing. Acceptance finalisation can return IDs and `Unfinished` together
with an error: those prove creation, not successful finalisation. If the old
graph is not proven present, the unfinished round regenerates. If present,
it is adopted without a second graph apply, with epic handling and
review/plan publication occurring once. Resume therefore does not duplicate
planning or graph creation.

## Ammo (D-05)

`ammo` is a concurrency cap, not a roster. The ordinary allocation has one
product-owner slot when one is needed, `max(1, floor((ammo-1)/4))` engineers
in flight, and every remaining slot available to testers, so an engineer can
have the ideal three reviewers. The minimum is 3: one of each. The weighted
parallel-planning exception is described above; it reserves whole rounds
within the same overall cap and never changes these generated tables.

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
   even if `ammo` or `safety` changed on disk in the meantime. The active
   cartridge and its default model/mode stay frozen. Role models are the
   exception: they are effective configuration, not `ConfigSnapshot` fields,
   so current repo role-model changes apply on the next ordinary resume.
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
