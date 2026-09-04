# Performance

`sc` spends almost all of its wall-clock time waiting for LLM subprocesses,
so the only speed that matters is the speed of the machinery around them:
the scheduler tick that decides who fires next, the event bus that carries
everything a run says, and the graph the TUI redraws while you watch. P9-07
sets three budgets for those three paths:

| Budget | What it means |
|---|---|
| 200-bead graph layout under 100 ms | A big run's dependency graph lays out and draws inside one frame, so the TUI never stutters. |
| Scheduler tick under 5 ms with 50 agents | The dispatch that runs every second, and again whenever an agent finishes, is free next to the work it schedules. |
| Event bus 10 000 events a second | An agent writing output as fast as it can never becomes the bottleneck, and never makes the run log the bottleneck either. |

All three hold with a wide margin. The numbers below are what the committed
benchmarks measure.

## Where the benchmarks are

| File | Covers |
|---|---|
| `internal/tui/bench_test.go` | `BuildGraphLayout`, `GraphModel.View` in every layout, `Model.View` (the whole terminal frame), the detail panel, cursor scrolling, `State.Apply` of a graph snapshot |
| `internal/orchestrator/bench_test.go` | `Scheduler.dispatch` saturated and cold, `sortDemands`, `events.Bus.Publish`, `Run.Publish`, the teed run log, `EventLog.Write`, `Run.AgentBus` forwarding |
| `internal/logging/redact_bench_test.go` | `SafeText` and `Redactor.SafeEvent`, the publication-time redaction hook every event goes through before the run log sees it (P9-09) |

Each file builds its own fixtures — a 200-bead run shaped like a planned one
(waves of twenty, each bead blocked by two of the wave before it, the early
waves closed and the middle in flight), and a scheduler with 50 ammo whose
source offers 300 demands a tick. Nothing reads the wall clock, the network,
`$HOME` or a real repository.

Two details of the scheduler fixture are load-bearing, because getting them
wrong measures something a run never does. Its work source is `WorkSources`
over three parts, so it allocates a **fresh, unsorted** slice on every tick
exactly as a real run's three sources do: `Scheduler.dispatch` sorts the
slice it is handed in place, so a source that returned one shared array would
be sorting already-sorted input from the second tick on, and understate the
tick by more than half. And the cold-tick fixture publishes through the real
wiring (`TeeRunLog` in front of a live bus), so the `AgentSpawned` publish
the dispatch goroutine pays per agent is inside the measurement rather than
thrown away by a nil publisher.

`TestPerformanceDocNamesBenchmarksThatExist` in `internal/orchestrator` reads
this page and holds it to the code both ways: every benchmark file it names
must exist, every benchmark in those files must be reported here, every
benchmark name it prints must exist, and the `-bench` patterns in the next
section must still select all of them.

## Running them

```sh
go test ./internal/tui -run '^$' -bench 'Graph|FullFrame|StateApply' -benchmem
go test ./internal/orchestrator -run '^$' -bench 'SchedulerTick|SortDemands|Bus|Publish|EventLog' -benchmem
go test ./internal/logging -run '^$' -bench 'SafeText|SafeEvent' -benchmem
```

To compare two commits, run each with `-count=10` and feed the two outputs to
[`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat): a single
run on a laptop that is also compiling something is worth about one
significant figure.

Two of those packages also carry a budget test — `TestGraphPerformanceBudgets`
and `TestOrchestratorPerformanceBudgets` — that turns the measurements above
into a pass/fail assertion. Each asserts the path a real run takes, not the
cheapest one nearby: the whole terminal frame as well as the graph pane, and
a publish through the run log and the bus rather than a bare bus publish.
They are **opt-in**:

```sh
SC_PERF_BUDGETS=1 go test ./internal/tui ./internal/orchestrator -run PerformanceBudgets -v
```

Without `SC_PERF_BUDGETS=1` they skip, and `make check` skips them. A
wall-clock threshold is not a deterministic test — on a busy machine, or
under `-race`, it fails for reasons that have nothing to do with the change
under test — so the benchmarks are the artefact and the budget test is the
tool you reach for when you suspect a regression. Run it on an idle machine
and without `-race`: the race detector costs this code roughly 5× on the
event bus and 10× on a cold scheduler tick, which is enough on its own to
blow the 5 ms tick budget.

## Results

Measured on 2026-09-04, Go 1.27, linux/amd64, Intel Core Ultra 9 275HX
(24 threads), `-benchtime=1s -count=1`, with other work running on the same
machine. Times are per operation.

### Graph layout and drawing (budget: 200 beads under 100 ms)

| Benchmark | Time | Allocated | Allocs |
|---|---|---|---|
| `GraphLayout/20beads` | 35 µs | 72 KB | 92 |
| `GraphLayout/200beads` | **0.70 ms** | 872 KB | 1 848 |
| `GraphLayout/1000beads` | 4.7 ms | 5.1 MB | 9 461 |
| `GraphView/left-right` (graph pane only, 200 beads) | 9.5 ms | 6.8 MB | 68 559 |
| `GraphView/top-bottom` (graph pane only, 200 beads) | 11.7 ms | 9.7 MB | 63 933 |
| `GraphView/list` (200 beads, narrow pane) | 2.2 ms | 1.0 MB | 7 699 |
| `GraphView/themed` (200 beads, true colour) | 11.7 ms | 7.1 MB | 75 896 |
| `FullFrame` (**the whole terminal frame**, 200 beads) | **10.6 ms** | 6.9 MB | 67 112 |
| `GraphViewDetail` | 0.78 ms | 881 KB | 2 079 |
| `GraphScroll` (one key press plus a pane redraw) | 11.1 ms | 7.6 MB | 66 519 |
| `StateApplyGraph` (fold a 200-bead snapshot) | 24 µs | 56 KB | 10 |

The layering itself — the `beads.Graph` build, the topological levels, the
cycle scan and the per-bead readiness rule — is 0.70 ms for 200 beads, 140×
inside the budget, and grows roughly linearly (1 000 beads is 4.7 ms).

Two rows measure different things and the distinction matters.
`BenchmarkGraphView` is the graph **pane**: `GraphModel.View` draws no
header, no tab bar, no footer and no overlay, so on its own it is not a
frame. `BenchmarkFullFrame` is `Model.View` over the same run in a 200×50 terminal — the pane plus
everything drawn around it — and that is the honest cost of pressing a key
with the graph tab open: **10.6 ms, 9× inside the budget**. It is close to
the pane's own number because the pane is nearly all of the frame: the
chrome around it is a few hundred microseconds, and the pane it leaves is a
little shorter than the standalone benchmark's.

That 10.6 ms is the number to watch. The graph is redrawn from scratch on
every event the run publishes, so a run that is talking hard can ask for it
many times a second. It is fine today and it is where the next 10× would
have to come from.

### Scheduler (budget: a tick under 5 ms with 50 agents)

| Benchmark | Time | Allocated | Allocs |
|---|---|---|---|
| `SchedulerTick` (50 live, 300 demands offered) | **184 µs** | 170 KB | 864 |
| `SchedulerTickCold` (fill all 50 slots at once, real wiring) | **3.9 ms** | 1.2 MB | 7 259 |
| `SortDemands` (300 demands, fresh copy each time) | 83 µs | 15 KB | 311 |

The steady-state tick — every slot full, the source still offering
everything it knows about, nothing to hand out — is 184 µs, 27× inside the
budget. This is the tick that actually runs once a second for the length of
a run. Half of it is the sort: by CPU profile `sortDemands` is 49 % of the
dispatch, the in-flight check (`reserve`) 35 %, and building the demand
slice 14 %. The standalone `BenchmarkSortDemands` row agrees — 83 µs of the
184 —
which is the check that the tick benchmark is sorting real input rather than
re-sorting its own output.

The cold tick, which fires 50 agents in one dispatch, is 3.9 ms: inside the
budget, but only just. By CPU profile 79 % of it is `state.json` (see below)
and a further 8 % is the `AgentSpawned` publish through the teed run log,
both of which land on the dispatch goroutine. A run only pays it on its
first tick and after a whole wave of agents finishes at once.

### Event bus (budget: 10 000 events a second)

| Benchmark | Time | Events/s | Allocs |
|---|---|---|---|
| `BusPublish/0subs` | 35 ns | 29 000 000 | 0 |
| `BusPublish/1subs` | 161 ns | 6 200 000 | 0 |
| `BusPublish/4subs` | 927 ns | 1 100 000 | 0 |
| `BusPublish/16subs` | 3.8 µs | 264 000 | 0 |
| `RunPublish` (sequence bookkeeping plus one subscriber) | 184 ns | 5 400 000 | 0 |
| `AgentBusForward` (the P6-27 forwarding bus) | 164 ns | 6 100 000 | 0 |
| `EventLogWrite` (JSONL append, rotation check) | 2.0 µs | 490 000 | 7 |
| `RunPublishTeed` (run log **and** live bus, the real wiring) | **2.6 µs** | **380 000** | 7 |

The path a real run uses is the last one: `Options.Bus = Tee(log, bus)`, so
the JSONL line is durable before anything renders it. At 2.6 µs a publish
that is 380 000 events a second, 38× the budget, and the run log — not the
fan-out — is the whole of the cost. It is also the row the opt-in budget test
asserts: a bare `Bus.Publish` is three orders of magnitude inside the budget
and could never regress it, so `TestOrchestratorPerformanceBudgets` measures
`Run.Publish` in front of `TeeRunLog` instead — the same path
`BenchmarkRunPublishTeed` reports. Fan-out stays free until about a dozen
subscribers, which no run has: the TUI is one, and `sc status` reads the log
rather than subscribing.

The bus allocates nothing per publish, so a busy run does not push the
garbage collector around; the seven allocations in the teed path are the
JSON encoding of the event.

### Redaction at the publication point (same budget)

The table above measures the wiring below the hook. A real run has one more
layer: `sc run` wraps `Options.Bus` with `(*logging.Redactor).SafeEvent`
after `TeeRunLog`, so **every** event is scrubbed of terminal escapes and put
through the secret rules once, before the JSONL line is written and before
any subscriber sees it (P7-17, P9-09). That is the layer these numbers are
about; nothing else in the run pays it, and nothing below it can skip it.

| Benchmark | Time | Events/s | Allocs |
|---|---|---|---|
| `SafeEventClean` (an output line with nothing to mask) | 2.6 µs | 385 000 | 1 |
| `SafeEvent/agent_output` (an escape sequence and a credential in the line) | 8.4 µs | **120 000** | 9 |
| `SafeEvent/verdict_received` (a summary and four items: twenty fields) | 110 µs | 9 100 | 10 |
| `SafeText` (one line, both halves of the hook) | 6.3 µs | — | 8 |

So the whole path a run publishes through — the hook, then the run log, then
the bus — costs about 11 µs for an agent output line: **90 000 events a
second, 9× the budget**, rather than the 380 000 the teed row alone suggests.
The margin is smaller than the rest of this page and it still holds
comfortably: an agent that wrote a line every 11 µs would be producing 90 000
lines a second on its own.

Two things are worth knowing rather than fixing. The hook, not the run log,
is now the larger half of a publish: three regular expressions and an escape
scan per untrusted field, at roughly 6 µs for a hundred-character line.
And its cost is per *field*, not per event, which is why a verdict — twenty
pieces of prose in one event — costs twelve times an output line; verdicts
arrive a handful per round, so the run never notices, but a future event kind
carrying hundreds of fields would want a cheaper rewriter (one combined
regexp, or a fast-path scan for the token prefixes before any regexp runs).

## Hot spots found

Three, in code this task does not own. None of them breaks a budget, so
none was fixed here; each was filed as a task of its own instead — P9-11,
P7-24 and P6-53. That is a **deviation** from P9-07's "fix hot spots found":
each fix belongs to a file another lane owns, none is needed to hold a
budget, and a benchmark that changes in the same commit as the code it
measures is worth less as a baseline. The numbers below are the brief each
of those three tasks starts from.

1. **`state.json` is rewritten per agent, and marshalled with indentation.**
   `Run.AgentStarted` and `Run.AgentStopped` each call `persistLocked`,
   which is `json.MarshalIndent` of the whole run state plus an atomic file
   write. A cold tick that fires 50 agents therefore writes the file 50
   times on the dispatch goroutine (and 50 more as they finish), and each
   write marshals a list that is one agent longer: 79 % of the 3.9 ms cold
   tick, by CPU profile, is `persistLocked` → `WriteState` → `MarshalIndent`
   → `config.WriteAtomic`. Coalescing the writes — mark the state dirty and
   flush once at the end of a dispatch, or debounce on a short timer — would
   take the cold tick to roughly 1 ms and remove the quadratic term. The
   file is a crash-recovery record (P9-03), not a transaction log, so a
   flush per dispatch is as safe as a flush per agent.

2. **A graph frame paints the whole canvas and then clips it.**
   `GraphModel.paint` draws every box of every level, including the ones
   scrolled off the pane, and `boxLines` then copies the visible window out
   of it — 6.8 MB and 68 000 allocations for a frame that shows perhaps a
   fifth of the graph. By profile, about a quarter of the time is
   `lipgloss.Style.Render` drawing box borders and a further 13 % is
   `ansi.stringWidth` measuring what it just drew. Culling boxes outside the
   scroll window before painting them, and caching the rendered border of a
   box (they differ only by state and selection), are both worth roughly a
   halving. Nothing needs doing at 200 beads; a run with 1 000 would want it.

3. **`sortDemands` allocates in its comparison function.** `lensRank` calls
   `domain.Lenses()`, which allocates a fresh slice, on every comparison
   between two demands of the same kind and order — 311 allocations to sort
   300 demands. A package-level index (`map[domain.Lens]int`, built once)
   removes them. The sort is 83 µs, which is **49 % of the 184 µs
   steady-state tick** — the single largest item in the tick a run pays
   every second — so unlike the first draft of this page this is worth
   doing for speed and not only for tidiness. It is a rounding error in the
   3.9 ms cold tick, where `state.json` dominates.

## Keeping it honest

The budgets are checked by the two opt-in tests above, not by CI: a timing
assertion in CI is a flake generator. What CI does guarantee is that the
benchmarks still compile and still run — they are ordinary `testing.B`
functions in packages `make check` builds — so a change that breaks a
fixture is caught immediately, and a change that makes something slower is
caught the next time somebody runs the commands at the top of this document.

CI also guarantees that this page still describes the benchmarks that exist:
`TestPerformanceDocNamesBenchmarksThatExist` fails if a benchmark named here
has been renamed or removed, if a benchmark exists that this page does not
report, if a file named here has moved, or if the `-bench` patterns above
have stopped selecting everything. What it cannot check is the numbers, so
re-measure rather than trusting them.

If you are about to make one of these paths do more work, run the relevant
benchmark before and after and put the two lines in the pull request. If a
number moves by more than about 20 %, say why in the same breath.
