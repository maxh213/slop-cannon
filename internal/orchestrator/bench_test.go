package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Benchmarks for the run loop and the event bus (P9-07). The budgets the
// task sets are a scheduler tick under 5 ms with 50 agents live, and an
// event bus that carries 10 000 events a second — the two paths that run on
// every tick of a real run, so a regression in either is felt everywhere.
//
// The numbers are written up in docs/performance.md. Run them with:
//
//	go test ./internal/orchestrator -run '^$' -bench 'Scheduler|Bus|Publish|EventLog' -benchmem
//
// [TestOrchestratorPerformanceBudgets] turns the same measurements into a
// pass/fail assertion, but only under SC_PERF_BUDGETS=1: a wall-clock
// threshold is not a deterministic test on a machine that is also running
// other work, so it is opt-in rather than part of `make check`.
//
// Every helper here is prefixed "perf" so it cannot collide with another
// task's file in this package.

// The P9-07 budgets.
const (
	// perfTickBudget is the longest a dispatch may take with perfAmmo
	// agents already live.
	perfTickBudget = 5 * time.Millisecond
	// perfBusBudget is the slowest a single publish may be and still carry
	// 10 000 events a second.
	perfBusBudget = 100 * time.Microsecond
)

// perfAmmo is the ammo the scheduler benchmarks run with: 50 live agents,
// the size the P9-07 tick budget is stated for.
const perfAmmo = 50

// perfDemands is how much work the source offers every tick. A real run's
// source offers far more than the ammo can take — every ready bead, every
// open review round — and the tick walks all of it, so the benchmark does
// too.
const perfDemands = 300

// perfEpoch is the fixed clock the benchmark fixtures are stamped with.
var perfEpoch = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// perfClock is a clock that never moves: the benchmarks drive dispatch
// directly rather than waiting for ticks, so nothing here reads the wall
// clock.
type perfClock struct{ now time.Time }

func (c perfClock) Now() time.Time { return c.now }

func (c perfClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// perfRun builds a hermetic run in temp directories (D-23) publishing to
// bus, which may be nil: a benchmark that wants to measure the loop rather
// than the log passes nil, and one that wants what a real run pays passes
// the teed publisher [perfTeedRun] builds.
func perfRun(tb testing.TB, ammo int, bus Publisher) *Run {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	tb.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		tb.Fatalf("could not make the repository directory: %v", err)
	}

	cfg := config.Defaults()
	cfg.Ammo = ammo
	cfg.ActiveCartridge = "mock"
	cfg.Cartridges = map[string]config.Cartridge{"mock": {Model: "mock"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260401-090000-perf", Brief: "measure the loop", StartedAt: perfEpoch,
	})
	if err != nil {
		tb.Fatalf("could not build the run config: %v", err)
	}
	run, err := New(rc, Options{
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return perfEpoch },
	})
	if err != nil {
		tb.Fatalf("could not build the run: %v", err)
	}
	if err := os.MkdirAll(run.Dir(), 0o755); err != nil {
		tb.Fatalf("could not make the run directory: %v", err)
	}
	return run
}

// perfTeedRun builds a run wired the way `sc run` wires one: every event it
// publishes goes to the JSONL run log first and to a live bus behind it,
// with one subscriber draining. A benchmark that fires agents needs this,
// because [Scheduler.start] publishes an [events.AgentSpawned] per agent on
// the dispatch goroutine — a cost a run with a nil publisher does not pay
// and a real cold tick does.
func perfTeedRun(tb testing.TB, ammo int) *Run {
	tb.Helper()
	dir := tb.TempDir()
	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx, 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sub.Events() {
		}
	}()
	opts := Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	log, err := TeeRunLog(&opts, dir, bus)
	if err != nil {
		tb.Fatalf("could not open the run log: %v", err)
	}
	tb.Cleanup(func() {
		cancel()
		bus.Close()
		<-done
		if err := log.Close(); err != nil {
			tb.Errorf("could not close the run log: %v", err)
		}
	})
	return perfRun(tb, ammo, opts.Bus)
}

// perfAllocation is the D-05 split of perfAmmo ammo the scheduler
// benchmarks enforce.
func perfAllocation(tb testing.TB) config.Allocation {
	tb.Helper()
	a, err := config.Allocate(perfAmmo)
	if err != nil {
		tb.Fatalf("could not allocate %d ammo: %v", perfAmmo, err)
	}
	return a
}

// perfWork is the demand list a real source would offer: one implement
// demand per bead, plus a full review round (one demand per lens) on the
// beads already in review, so the tick exercises the ammo split, the
// per-bead review width and the in-flight check.
func perfWork(n int) []Demand {
	lenses := domain.Lenses()
	out := make([]Demand, 0, n)
	// One planning demand, so the PO slot of the D-05 split is taken too
	// and the tick benchmarks really do run with every slot full.
	out = append(out, Demand{Kind: DemandPlan, Order: -1})
	for i := 0; len(out) < n; i++ {
		bead := domain.BeadID(fmt.Sprintf("sc-%04d", i))
		out = append(out, Demand{
			Kind:      DemandImplement,
			Bead:      bead,
			BeadTitle: "Bead " + strconv.Itoa(i),
			Round:     i % 3,
			Order:     i,
		})
		for _, lens := range lenses {
			if len(out) >= n {
				break
			}
			out = append(out, Demand{
				Kind:      DemandReview,
				Bead:      bead,
				BeadTitle: "Bead " + strconv.Itoa(i),
				Lens:      lens,
				Round:     i % 3,
				Order:     i,
			})
		}
	}
	return out
}

// perfSource returns a work source shaped like the one a real run wires up:
// [WorkSources] over the feedback engine, the planning loop and the delivery
// handoff, which allocates a fresh slice and concatenates the parts into it
// on every call.
//
// The allocation is the point, not an accident of the fixture.
// [Scheduler.dispatch] calls sortDemands on the slice its source hands back,
// in place. A source that returned the same backing array every tick would
// be sorted once and then asked to re-sort already-sorted input for the rest
// of the benchmark, which is not what any real tick does and understates the
// tick by about a third. Splitting perfWork's interleaved list into three
// contiguous parts keeps the concatenation unsorted with respect to the D-05
// order, exactly as a real run's three sources are.
func perfSource(n int) WorkSource {
	all := perfWork(n)
	third := len(all) / 3
	parts := [][]Demand{all[:third], all[third : 2*third], all[2*third:]}
	sources := make([]WorkSource, 0, len(parts))
	for _, part := range parts {
		sources = append(sources, WorkSourceFunc(func(context.Context) ([]Demand, error) {
			return part, nil
		}))
	}
	return WorkSources(sources...)
}

// perfGate is a [Firer] whose agents hold their slot until the benchmark
// releases them, which is how the tick benchmarks get a scheduler with
// exactly perfAmmo agents live.
type perfGate struct {
	release chan struct{}
	once    sync.Once
}

func newPerfGate() *perfGate { return &perfGate{release: make(chan struct{})} }

// Fire implements [Firer].
func (g *perfGate) Fire(ctx context.Context, _ Firing) (FireResult, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return FireResult{Failure: events.FailureKilled}, ctx.Err()
	}
	return FireResult{Produced: domain.MessageImplReport}, nil
}

// Open lets every held agent finish. It is safe to call more than once.
func (g *perfGate) Open() { g.once.Do(func() { close(g.release) }) }

// perfSaturated returns a scheduler with every slot taken, along with the
// run it dispatches into and the demands its source offers. The caller must
// call the returned stop function.
func perfSaturated(tb testing.TB) (*Scheduler, *Run, func()) {
	tb.Helper()
	gate := newPerfGate()
	sched := NewScheduler(SchedulerOptions{
		Allocation: perfAllocation(tb),
		Work:       perfSource(perfDemands),
		Fire:       gate,
		Clock:      perfClock{now: perfEpoch},
	})
	run := perfRun(tb, perfAmmo, nil)

	ctx, cancel := context.WithCancel(context.Background())
	sched.dispatch(ctx, run)
	if live := sched.Live(); live != perfAmmo {
		gate.Open()
		cancel()
		tb.Fatalf("the scheduler filled %d slots, want %d", live, perfAmmo)
	}
	return sched, run, func() {
		gate.Open()
		cancel()
		sched.wg.Wait()
	}
}

// BenchmarkSchedulerTick measures the steady-state dispatch: 50 agents are
// live, the source still offers everything it knows about, and the tick has
// to walk the lot and hand out the slots it has (none). This is the tick
// the P9-07 budget is about — it runs once a second for the length of a run,
// and again on every agent that finishes.
func BenchmarkSchedulerTick(b *testing.B) {
	sched, run, stop := perfSaturated(b)
	defer stop()

	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		sched.dispatch(ctx, run)
	}
}

// BenchmarkSchedulerTickCold measures the opposite end: an empty scheduler
// filling all 50 slots in one dispatch, which is what the first tick of a
// run and the tick after a wave of agents finish both do. It runs against a
// run wired as a real one is ([perfTeedRun]), so it includes both of the
// per-agent costs that land on the dispatch goroutine: the state.json write
// [Run.AgentStarted] does, and the [events.AgentSpawned] publish through the
// teed run log.
func BenchmarkSchedulerTickCold(b *testing.B) {
	src := perfSource(perfDemands)
	alloc := perfAllocation(b)
	run := perfTeedRun(b, perfAmmo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		gate := newPerfGate()
		sched := NewScheduler(SchedulerOptions{
			Allocation: alloc,
			Work:       src,
			Fire:       gate,
			Clock:      perfClock{now: perfEpoch},
		})
		b.StartTimer()

		sched.dispatch(ctx, run)

		b.StopTimer()
		if live := sched.Live(); live != perfAmmo {
			b.Fatalf("the scheduler filled %d slots, want %d", live, perfAmmo)
		}
		gate.Open()
		sched.wg.Wait()
		b.StartTimer()
	}
}

// BenchmarkSortDemands measures the ordering the tick does before it hands
// out a single slot: the D-05 kind priority, then the source's order, then
// the tie-breaks.
func BenchmarkSortDemands(b *testing.B) {
	src := perfWork(perfDemands)
	buf := make([]Demand, len(src))
	b.ReportAllocs()
	for b.Loop() {
		copy(buf, src)
		sortDemands(buf)
	}
}

// perfEvent builds the event the bus carries most of: a line of agent
// output.
func perfEvent(i int) events.Event {
	return events.AgentOutput{
		Meta:   events.MetaAt("20260401-090000-perf", perfEpoch.Add(time.Duration(i)*time.Millisecond)),
		Agent:  domain.AgentID("eng-sc-0001-r1"),
		Stream: events.StreamStdout,
		Line:   "wrote internal/orchestrator/scheduler.go, running the tests now",
	}
}

// BenchmarkBusPublish measures the fan-out itself with a varying number of
// subscribers — a real run has the TUI, and a `sc status --json` reader can
// add more. The subscribers drain as fast as they can, so what is measured
// is the publish, not the render.
func BenchmarkBusPublish(b *testing.B) {
	for _, subs := range []int{0, 1, 4, 16} {
		b.Run(strconv.Itoa(subs)+"subs", func(b *testing.B) {
			bus := events.NewBus()
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for range subs {
				sub := bus.Subscribe(ctx, 1024)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range sub.Events() {
					}
				}()
			}
			ev := perfEvent(0)
			b.ReportAllocs()
			for b.Loop() {
				bus.Publish(ev)
			}
			b.StopTimer()
			cancel()
			bus.Close()
			wg.Wait()
		})
	}
}

// BenchmarkRunPublish measures a publish as the run makes it: the sequence
// number and last-event bookkeeping in [Run.Publish] on top of the bus
// fan-out, with one subscriber draining.
func BenchmarkRunPublish(b *testing.B) {
	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx, 1024)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range sub.Events() {
		}
	}()
	run := perfRun(b, config.MinAmmo, bus)

	ev := perfEvent(0)
	b.ReportAllocs()
	for b.Loop() {
		run.Publish(ev)
	}
	b.StopTimer()
	cancel()
	bus.Close()
	wg.Wait()
}

// BenchmarkRunPublishTeed measures the wiring a real run uses: the JSONL run
// log first (so a line is durable before anything renders it) and the live
// bus behind it. This is the slowest publish path there is, and the one the
// 10 000 events a second budget has to hold for.
func BenchmarkRunPublishTeed(b *testing.B) {
	dir := b.TempDir()
	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx, 1024)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range sub.Events() {
		}
	}()

	opts := Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	log, err := TeeRunLog(&opts, dir, bus)
	if err != nil {
		b.Fatalf("could not open the run log: %v", err)
	}
	run := perfRun(b, config.MinAmmo, opts.Bus)

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		run.Publish(perfEvent(i))
	}
	b.StopTimer()
	cancel()
	bus.Close()
	wg.Wait()
	if err := log.Close(); err != nil {
		b.Fatalf("could not close the run log: %v", err)
	}
}

// BenchmarkEventLogWrite measures the run log on its own: encode one event
// and append the line, rotation check included.
func BenchmarkEventLogWrite(b *testing.B) {
	log, err := OpenEventLog(b.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatalf("could not open the run log: %v", err)
	}
	defer func() { _ = log.Close() }()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := log.Write(perfEvent(i)); err != nil {
			b.Fatalf("could not write the event: %v", err)
		}
	}
}

// BenchmarkAgentBusForward measures the forwarding bus the supervisor and
// the bead watcher publish to (P6-27): a concrete *events.Bus whose events
// are republished through [Run.Publish]. It is the busiest producer in a
// run — every line every agent writes — so its cost is the one that decides
// whether the 10 000 events a second budget holds under load.
func BenchmarkAgentBusForward(b *testing.B) {
	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx, 4096)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range sub.Events() {
		}
	}()
	run := perfRun(b, config.MinAmmo, bus)
	agentBus := run.AgentBus()

	ev := perfEvent(0)
	b.ReportAllocs()
	for b.Loop() {
		agentBus.Publish(ev)
	}
	b.StopTimer()
	cancel()
	bus.Close()
	wg.Wait()
}

// TestOrchestratorPerformanceBudgets asserts the P9-07 budgets: a dispatch
// with 50 live agents under 5 ms, and a publish fast enough for 10 000
// events a second. It is opt-in — a wall-clock threshold is not
// deterministic on a loaded machine — so it runs only under
// SC_PERF_BUDGETS=1, which is how the numbers in docs/performance.md are
// re-checked.
func TestOrchestratorPerformanceBudgets(t *testing.T) {
	if os.Getenv("SC_PERF_BUDGETS") != "1" {
		t.Skip("set SC_PERF_BUDGETS=1 to check the P9-07 timing budgets")
	}

	t.Run("scheduler tick", func(t *testing.T) {
		sched, run, stop := perfSaturated(t)
		defer stop()
		ctx := context.Background()
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				sched.dispatch(ctx, run)
			}
		})
		perfAssert(t, "a dispatch with 50 live agents", res, perfTickBudget)
	})

	// The budget is asserted over the path a real run publishes through —
	// [Run.Publish] in front of [TeeRunLog], so the JSONL line is durable
	// before the bus fans it out — and not over a bare bus publish. A bare
	// publish is three orders of magnitude inside the budget, so nothing
	// could ever regress it; the teed path is the one that carries the run
	// log's cost and the only one where a regression is plausible.
	t.Run("event wiring", func(t *testing.T) {
		run := perfTeedRun(t, config.MinAmmo)
		res := testing.Benchmark(func(b *testing.B) {
			for i := 0; b.Loop(); i++ {
				run.Publish(perfEvent(i))
			}
		})
		perfAssert(t, "a publish through the run log and the bus", res, perfBusBudget)
	})
}

// perfAssert fails the test when a benchmark's time per operation is over
// its budget, and records the measurement either way.
func perfAssert(t *testing.T, what string, res testing.BenchmarkResult, budget time.Duration) {
	t.Helper()
	per := res.T / time.Duration(max(res.N, 1))
	if per > budget {
		t.Errorf("%s took %s, over the %s budget", what, per, budget)
		return
	}
	t.Logf("%s: %s (budget %s)", what, per, budget)
}
