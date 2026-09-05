package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Benchmarks for the graph view (P9-07). The budget the task sets is that
// laying a 200-bead run out takes less than 100 ms, which three benchmarks
// measure at three widths: [BenchmarkGraphLayout] is the layering
// [BuildGraphLayout] does, [BenchmarkGraphView] is the graph pane
// [GraphModel.View] paints from it — canvas paint, edge routing and the clip
// to the pane — and [BenchmarkFullFrame] is the whole terminal frame
// [Model.View] draws around that pane, header, tab bar, footer and animation
// layer included. The last is the one a key press really costs.
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
	st := NewState(benchRun())
	for _, ev := range benchGraphEvents(n) {
		st.Apply(ev)
	}
	return st
}

// benchRun is the run every benchmark fixture is built for.
func benchRun() domain.Run {
	return domain.Run{
		ID:        "sc-bench-001",
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeAutopilot,
		Brief:     "make the thing work",
		StartedAt: benchEpoch,
		Config:    domain.ConfigSnapshot{Ammo: 8, Safety: true, Cartridge: "mock"},
	}
}

// benchGraphEvents is the event stream behind [benchGraphState], so a
// benchmark that drives the whole [Model] can replay exactly the run that
// the graph-only benchmarks fold straight into a [State].
func benchGraphEvents(n int) []events.Event {
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

	out := []events.Event{
		events.RunStarted{Meta: events.MetaAt("sc-bench-001", benchEpoch), Info: benchRun()},
		events.GraphChanged{
			Meta:  events.MetaAt("sc-bench-001", benchEpoch.Add(time.Minute)),
			Epic:  "sc-epic",
			Beads: beads,
		},
	}
	for i, b := range beads {
		if b.State != domain.BeadStateInProgress && b.State != domain.BeadStateInReview {
			continue
		}
		out = append(out, events.AgentSpawned{
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
	return out
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

// benchBigBeadCount is the run size P7-24 states its budget for: ten times
// the pane's own capacity, so all but a fiftieth of the graph is off screen
// and the cull is what decides the frame's cost.
const benchBigBeadCount = 1000

// BenchmarkGraphView measures the graph pane alone — the layering plus the
// canvas paint, the edge routing and the clip to the pane, in every layout
// the view can pick, at the P9-07 run size and the P7-24 one. It is not a
// whole frame: [GraphModel.View] draws no header, tab bar, footer or
// overlay, so a real frame costs this plus what [BenchmarkFullFrame]
// measures on top.
//
// P7-24 is what the 1000-bead rows record: the paint used to draw every box
// of every level and let the clip throw away the ones the pane cannot show
// (6.8 MB and 68 000 allocations for a 200-bead frame), and now draws only
// the boxes and routes the scroll window reaches. What is left at 200 beads
// is the layering, which [BenchmarkGraphLayout] measures on its own.
func BenchmarkGraphView(b *testing.B) {
	for _, n := range []int{benchBeadCount, benchBigBeadCount} {
		st := benchGraphState(n)
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
			b.Run(strconv.Itoa(n)+"beads/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if v.View(st) == "" {
						b.Fatal("the graph view drew nothing")
					}
				}
			})
		}
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

// benchFullModel builds the whole TUI over the [benchGraphState] run: the
// model, its five views and the animation layer, sized to the benchmark
// pane and fed the run's events the way the subscription feeds them. It has
// no bus and no input, so nothing but the caller drives it.
func benchFullModel(tb testing.TB, n int) Model {
	tb.Helper()
	m := New(context.Background(), Options{
		NoColor: true,
		NoAnim:  true,
		Run:     benchRun(),
		Now:     func() time.Time { return benchEpoch.Add(time.Hour) },
	})
	next, _ := m.Update(tea.WindowSizeMsg{Width: benchPaneWidth, Height: benchPaneHeight})
	m = benchAsModel(tb, next)
	for _, ev := range benchGraphEvents(n) {
		next, _ = m.Update(EventMsg{Event: ev})
		m = benchAsModel(tb, next)
	}
	if m.tab != TabGraph {
		tb.Fatalf("the model opened on tab %v, want the graph tab", m.tab)
	}
	return m
}

// benchAsModel is the type assertion [Model.Update]'s tea.Model return needs,
// with the failure reported rather than swallowed.
func benchAsModel(tb testing.TB, msg tea.Model) Model {
	tb.Helper()
	m, ok := msg.(Model)
	if !ok {
		tb.Fatalf("Model.Update returned a %T, want a tui.Model", msg)
	}
	return m
}

// BenchmarkFullFrame measures what pressing a key with the graph tab open
// actually costs: [Model.View] over a 200-bead run, which is the graph pane
// plus the header, the tab bar, the footer and the overlay layer. This is
// the number the 100 ms budget is really about — [BenchmarkGraphView] is
// only the pane inside it.
func BenchmarkFullFrame(b *testing.B) {
	m := benchFullModel(b, benchBeadCount)
	b.ReportAllocs()
	for b.Loop() {
		if m.View() == "" {
			b.Fatal("the frame drew nothing")
		}
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
	big := benchGraphState(benchBigBeadCount)
	full := benchFullModel(t, benchBeadCount)
	cases := []struct {
		name string
		n    int
		fn   func()
	}{
		{"layout", benchBeadCount, func() { BuildGraphLayout(st) }},
		{"view left→right", benchBeadCount, func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphLeftToRight}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(st)
		}},
		{"view top→bottom", benchBeadCount, func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(st)
		}},
		{"whole frame", benchBeadCount, func() { _ = full.View() }},
		// P7-24's own budget: a run ten times the size, where the pane can
		// show a fiftieth of the graph.
		{"view left→right", benchBigBeadCount, func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphLeftToRight}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(big)
		}},
		{"view top→bottom", benchBigBeadCount, func() {
			v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).
				Resize(benchPaneWidth, benchPaneHeight)
			_ = v.View(big)
		}},
	}
	for _, tc := range cases {
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				tc.fn()
			}
		})
		if per := res.T / time.Duration(max(res.N, 1)); per > benchGraphBudget {
			t.Errorf("%s of %d beads took %s, over the %s budget", tc.name, tc.n, per, benchGraphBudget)
		} else {
			t.Logf("%s of %d beads: %s (budget %s)", tc.name, tc.n, per, benchGraphBudget)
		}
	}
}
