package tui

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Benchmarks for the graph view (P9-07). The budget the task sets is that
// laying a 200-bead run out takes less than 100 ms, which is what
// [BenchmarkGraphLayout] and [BenchmarkGraphView] measure: the layering that
// [BuildGraphLayout] does, and the whole frame — layering, canvas paint,
// edge routing and clipping — that [GraphModel.View] renders from it.
//
// The numbers these produce are written up in docs/performance.md. Run them
// with:
//
//	go test ./internal/tui -run '^$' -bench 'Graph' -benchmem
//
// [TestGraphPerformanceBudgets] turns the same measurements into a pass/fail
// assertion, but only when SC_PERF_BUDGETS=1 is set: a wall-clock threshold
// is not a deterministic test on a machine that is also running other work,
// so it is opt-in rather than part of `make check`.

// benchBeadCount is the run size the P9-07 budget is stated for.
const benchBeadCount = 200

// benchGraphBudget is the P9-07 budget for laying out and drawing a
// [benchBeadCount]-bead run.
const benchGraphBudget = 100 * time.Millisecond

// benchPaneWidth and benchPaneHeight are the pane the frame benchmarks draw
// into: a large terminal, because a bigger pane means more canvas rows to
// clip and copy, which is the more expensive case.
const (
	benchPaneWidth  = 200
	benchPaneHeight = 50
)

// benchEpoch is the fixed clock every benchmark fixture is stamped with.
var benchEpoch = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// benchGraphState builds a deterministic n-bead run shaped like a real one:
// waves of benchWave beads, each bead blocked by two beads of the wave
// before it, with the earliest waves closed, the middle in flight and the
// rest open, and an agent on every bead that is in progress or in review.
//
// It is deliberately not a chain and not a fan: a chain gives n levels of
// one bead and a fan gives one level of n, and both are cheaper to lay out
// and to paint than the layered diamond a planned run actually produces.
func benchGraphState(n int) *State {
	const wave = 20
	beads := make([]domain.Bead, 0, n)
	for i := range n {
		b := domain.Bead{
			ID:       domain.BeadID(fmt.Sprintf("sc-%04d", i)),
			Title:    "Bead " + strconv.Itoa(i) + ": make the thing work end to end",
			Type:     domain.BeadTypeTask,
			Priority: domain.PriorityDefault,
			State:    benchBeadState(i, n),
			Round:    i % 3,
		}
		if i >= wave {
			b.Deps = []domain.BeadID{
				domain.BeadID(fmt.Sprintf("sc-%04d", i-wave)),
				domain.BeadID(fmt.Sprintf("sc-%04d", i-wave+(i%(wave-1)))),
			}
		}
		beads = append(beads, b)
	}

	st := NewState(domain.Run{
		ID:        "sc-bench-001",
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeAutopilot,
		Brief:     "make the thing work",
		StartedAt: benchEpoch,
		Config:    domain.ConfigSnapshot{Ammo: 8, Safety: true, Cartridge: "mock"},
	})
	st.Apply(events.RunStarted{Meta: events.MetaAt("sc-bench-001", benchEpoch), Info: st.Run})
	st.Apply(events.GraphChanged{
		Meta:  events.MetaAt("sc-bench-001", benchEpoch.Add(time.Minute)),
		Epic:  "sc-epic",
		Beads: beads,
	})
	for i, b := range beads {
		if b.State != domain.BeadStateInProgress && b.State != domain.BeadStateInReview {
			continue
		}
		st.Apply(events.AgentSpawned{
			Meta: events.MetaAt("sc-bench-001", benchEpoch.Add(time.Duration(i)*time.Second)),
			Agent: domain.Agent{
				ID:        domain.AgentID("eng-" + string(b.ID)),
				Role:      domain.RoleEngineer,
				Cartridge: "mock",
				Bead:      b.ID,
				State:     domain.AgentStateRunning,
				StartedAt: benchEpoch.Add(time.Duration(i) * time.Second),
			},
			Task:      events.TaskImplement,
			BeadTitle: b.Title,
			Round:     b.Round,
			Attempt:   1,
		})
	}
	return st
}

// benchBeadState spreads the D-07 states over the run the way a run halfway
// through looks: the early waves finished, the middle in flight, the tail
// untouched.
func benchBeadState(i, n int) domain.BeadState {
	switch {
	case i < n/3:
		return domain.BeadStateClosed
	case i < n/2:
		return domain.BeadStateInReview
	case i < 2*n/3:
		return domain.BeadStateInProgress
	default:
		return domain.BeadStateOpen
	}
}

// BenchmarkGraphLayout measures the layering alone: the [beads.Graph] build,
// the topological levels, the cycle scan and the per-bead readiness rule.
func BenchmarkGraphLayout(b *testing.B) {
	for _, n := range []int{20, benchBeadCount, 1000} {
		st := benchGraphState(n)
		b.Run(strconv.Itoa(n)+"beads", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				lay := BuildGraphLayout(st)
				if lay.Len() != n {
					b.Fatalf("laid out %d beads, want %d", lay.Len(), n)
				}
			}
		})
	}
}

// BenchmarkGraphView measures a whole frame, which is what the 100 ms budget
// is really about: the layering plus the canvas paint, the edge routing and
// the clip to the pane, in every layout the view can pick.
func BenchmarkGraphView(b *testing.B) {
	st := benchGraphState(benchBeadCount)
	views := []struct {
		name string
		view GraphModel
	}{
		{"left-right", NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphLeftToRight}).
			Resize(benchPaneWidth, benchPaneHeight)},
		{"top-bottom", NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).
			Resize(benchPaneWidth, benchPaneHeight)},
		{"list", NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphIndentedList}).
			Resize(60, 24)},
		{"themed", NewGraphModel(GraphOptions{Theme: ThemeFor(ProfileTrueColor), Orientation: GraphLeftToRight}).
			Resize(benchPaneWidth, benchPaneHeight)},
	}
	for _, tc := range views {
		v := tc.view
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if v.View(st) == "" {
					b.Fatal("the graph view drew nothing")
				}
			}
		})
	}
}

// BenchmarkGraphViewDetail measures the bead detail panel, which reads the
// run's rounds and deliveries as well as the layout.
func BenchmarkGraphViewDetail(b *testing.B) {
	st := benchGraphState(benchBeadCount)
	v := NewGraphModel(GraphOptions{NoColor: true}).Resize(benchPaneWidth, benchPaneHeight)
	v, _ = v.Update(gvKey("enter"), st, DefaultKeyMap())
	if !v.ShowingDetail() {
		b.Fatal("the detail panel did not open")
	}
	b.ReportAllocs()
	for b.Loop() {
		if v.View(st) == "" {
			b.Fatal("the detail panel drew nothing")
		}
	}
}

// BenchmarkGraphScroll measures the cursor walking a level at a time, the
// key path a person holds down: every step re-lays the graph out and
// re-paints it.
func BenchmarkGraphScroll(b *testing.B) {
	st := benchGraphState(benchBeadCount)
	v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphLeftToRight}).
		Resize(benchPaneWidth, benchPaneHeight)
	keys := DefaultKeyMap()
	b.ReportAllocs()
	for b.Loop() {
		v, _ = v.Update(gvKey("right"), st, keys)
		_ = v.View(st)
	}
}

// BenchmarkStateApplyGraph measures folding a graph snapshot into the state,
// which happens on every GraphChanged the orchestrator publishes.
func BenchmarkStateApplyGraph(b *testing.B) {
	src := benchGraphState(benchBeadCount)
	snapshot := events.GraphChanged{
		Meta:  events.MetaAt("sc-bench-001", benchEpoch),
		Epic:  "sc-epic",
		Beads: src.Beads,
	}
	b.ReportAllocs()
	for b.Loop() {
		st := NewState(src.Run)
		st.Apply(snapshot)
	}
}

// TestGraphPerformanceBudgets asserts the P9-07 budget: a 200-bead run lays
// out and draws in under 100 ms. It is opt-in — a wall-clock threshold is
// not deterministic on a loaded machine — so it runs only under
// SC_PERF_BUDGETS=1, which is how the numbers in docs/performance.md are
// re-checked.
func TestGraphPerformanceBudgets(t *testing.T) {
	if os.Getenv("SC_PERF_BUDGETS") != "1" {
		t.Skip("set SC_PERF_BUDGETS=1 to check the P9-07 timing budgets")
	}
	st := benchGraphState(benchBeadCount)
	cases := []struct {
		name string
		fn   func()
	}{
		{"layout", func() { BuildGraphLayout(st) }},
		{"view left→right", func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphLeftToRight}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(st)
		}},
		{"view top→bottom", func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(st)
		}},
	}
	for _, tc := range cases {
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				tc.fn()
			}
		})
		if per := res.T / time.Duration(max(res.N, 1)); per > benchGraphBudget {
			t.Errorf("%s of %d beads took %s, over the %s budget", tc.name, benchBeadCount, per, benchGraphBudget)
		} else {
			t.Logf("%s of %d beads: %s (budget %s)", tc.name, benchBeadCount, per, benchGraphBudget)
		}
	}
}
