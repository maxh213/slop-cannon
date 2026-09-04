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
| `internal/tui/bench_test.go` | `BuildGraphLayout`, `GraphModel.View` in every layout, the detail panel, cursor scrolling, `State.Apply` of a graph snapshot |
| `internal/orchestrator/bench_test.go` | `Scheduler.dispatch` saturated and cold, `sortDemands`, `events.Bus.Publish`, `Run.Publish`, the teed run log, `EventLog.Write`, `Run.AgentBus` forwarding |

Both files build their own fixtures — a 200-bead run shaped like a planned
one (waves of twenty, each bead blocked by two of the wave before it, the
early waves closed and the middle in flight), and a scheduler with 50 ammo
whose source offers 300 demands a tick. Nothing reads the wall clock, the
network, `$HOME` or a real repository.

## Running them

```sh
go test ./internal/tui -run '^$' -bench 'Graph|StateApply' -benchmem
go test ./internal/orchestrator -run '^$' -bench 'SchedulerTick|SortDemands|Bus|Publish|EventLog' -benchmem
```

To compare two commits, run each with `-count=10` and feed the two outputs to
[`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat): a single
run on a laptop that is also compiling something is worth about one
significant figure.

Each file also carries a budget test — `TestGraphPerformanceBudgets` and
`TestOrchestratorPerformanceBudgets` — that turns the measurements above into
a pass/fail assertion. They are **opt-in**:

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
| `GraphLayout/20beads` | 37 µs | 72 KB | 92 |
| `GraphLayout/200beads` | **0.73 ms** | 872 KB | 1 848 |
| `GraphLayout/1000beads` | 4.8 ms | 5.1 MB | 9 461 |
| `GraphView/left-right` (200 beads) | **11.0 ms** | 6.8 MB | 68 558 |
| `GraphView/top-bottom` (200 beads) | **12.3 ms** | 9.7 MB | 63 934 |
| `GraphView/list` (200 beads, narrow pane) | 2.3 ms | 1.0 MB | 7 699 |
| `GraphView/themed` (200 beads, true colour) | 11.8 ms | 7.1 MB | 75 896 |
| `GraphViewDetail` | 0.84 ms | 881 KB | 2 078 |
| `GraphScroll` (one key press plus redraw) | 11.8 ms | 7.6 MB | 66 506 |
| `StateApplyGraph` (fold a 200-bead snapshot) | 27 µs | 56 KB | 10 |

The layering itself — the `beads.Graph` build, the topological levels, the
cycle scan and the per-bead readiness rule — is 0.73 ms for 200 beads, 140×
inside the budget, and grows roughly linearly (1 000 beads is 4.8 ms).

A whole frame is 11–12 ms, still 8× inside the budget. That is the number to
watch: it is the cost of pressing a key with the graph tab open, and the
graph is redrawn from scratch on every event the run publishes, so a run
that is talking hard can ask for it many times a second. It is fine today
and it is where the next 10× would have to come from.

### Scheduler (budget: a tick under 5 ms with 50 agents)

| Benchmark | Time | Allocated | Allocs |
|---|---|---|---|
| `SchedulerTick` (50 live, 300 demands offered) | **81 µs** | 28 KB | 853 |
| `SchedulerTickCold` (fill all 50 slots at once) | **3.9 ms** | 969 KB | 6 799 |
| `SortDemands` (300 demands) | 88 µs | 15 KB | 311 |

The steady-state tick — every slot full, the source still offering
everything it knows about, nothing to hand out — is 81 µs, 60× inside the
budget. This is the tick that actually runs once a second for the length of
a run.

The cold tick, which fires 50 agents in one dispatch, is 3.9 ms: inside the
budget, but only just, and 67 % of it is `state.json` (see below). A run only
pays it on its first tick and after a whole wave of agents finishes at once.

### Event bus (budget: 10 000 events a second)

| Benchmark | Time | Events/s | Allocs |
|---|---|---|---|
| `BusPublish/0subs` | 34 ns | 29 000 000 | 0 |
| `BusPublish/1subs` | 166 ns | **6 000 000** | 0 |
| `BusPublish/4subs` | 907 ns | 1 100 000 | 0 |
| `BusPublish/16subs` | 3.6 µs | 275 000 | 0 |
| `RunPublish` (sequence bookkeeping plus one subscriber) | 183 ns | 5 500 000 | 0 |
| `AgentBusForward` (the P6-27 forwarding bus) | 164 ns | 6 100 000 | 0 |
| `EventLogWrite` (JSONL append, rotation check) | 2.2 µs | 460 000 | 7 |
| `RunPublishTeed` (run log **and** live bus, the real wiring) | 2.7 µs | **375 000** | 7 |

The path a real run uses is the last one: `Options.Bus = Tee(log, bus)`, so
the JSONL line is durable before anything renders it. At 2.7 µs a publish
that is 375 000 events a second, 37× the budget, and the run log — not the
fan-out — is the whole of the cost. Fan-out stays free until about a dozen
subscribers, which no run has: the TUI is one, and `sc status` reads the log
rather than subscribing.

The bus allocates nothing per publish, so a busy run does not push the
garbage collector around; the seven allocations in the teed path are the
JSON encoding of the event.

## Hot spots found

Three, in code this task does not own. None of them breaks a budget, so
none was fixed here; each is worth a task of its own.

1. **`state.json` is rewritten per agent, and marshalled with indentation.**
   `Run.AgentStarted` and `Run.AgentStopped` each call `persistLocked`,
   which is `json.MarshalIndent` of the whole run state plus an atomic file
   write. A cold tick that fires 50 agents therefore writes the file 50
   times on the dispatch goroutine (and 50 more as they finish), and each
   write marshals a list that is one agent longer: 67 % of the 3.9 ms cold
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
   removes them. It is 88 µs of an 81 µs–3.9 ms tick, so this is tidiness
   rather than speed.

## Keeping it honest

The budgets are checked by the two opt-in tests above, not by CI: a timing
assertion in CI is a flake generator. What CI does guarantee is that the
benchmarks still compile and still run — they are ordinary `testing.B`
functions in packages `make check` builds — so a change that breaks a
fixture is caught immediately, and a change that makes something slower is
caught the next time somebody runs the two commands at the top of this
document.

If you are about to make one of these paths do more work, run the relevant
benchmark before and after and put the two lines in the pull request. If a
number moves by more than about 20 %, say why in the same breath.
