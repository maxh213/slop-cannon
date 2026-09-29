package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/textsafe"
)

// gvGoldenDir is where the graph view's golden frames live.
const gvGoldenDir = "../../testdata/tui"

// gvRunID names the run every graph-view fixture watches.
const gvRunID = domain.RunID("sc-graph-001")

// gvAt returns a deterministic timestamp, n minutes after a fixed start.
func gvAt(n int) time.Time {
	return time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// gvMeta stamps the nth event of the fixture run.
func gvMeta(n int) events.Meta { return events.MetaAt(gvRunID, gvAt(n)) }

// gvRunInfo builds the run the fixtures watch.
func gvRunInfo() domain.Run {
	return domain.Run{
		ID:        gvRunID,
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeBrief,
		Brief:     "fix the retry bugs",
		StartedAt: gvAt(0),
		Config:    domain.ConfigSnapshot{Ammo: 4, Safety: true, Cartridge: "mock"},
	}
}

// gvBead builds one fixture bead.
func gvBead(id domain.BeadID, title string, state domain.BeadState, round int, deps ...domain.BeadID) domain.Bead {
	return domain.Bead{
		ID:       id,
		Title:    title,
		Type:     domain.BeadTypeTask,
		Priority: domain.PriorityDefault,
		State:    state,
		Round:    round,
		Deps:     deps,
	}
}

// gvState folds a graph snapshot (and anything else) into a fresh state.
func gvState(beads []domain.Bead, extra ...events.Event) *State {
	st := NewState(gvRunInfo())
	st.Apply(events.RunStarted{Meta: gvMeta(0), Info: gvRunInfo()})
	st.Apply(events.GraphChanged{Meta: gvMeta(1), Epic: "sc-epic", Beads: beads})
	for _, ev := range extra {
		st.Apply(ev)
	}
	return st
}

// gvChain is a five-bead chain: every bead blocks the next, so the layering
// puts each on its own level.
func gvChain() []domain.Bead {
	return []domain.Bead{
		gvBead("sc-1a2b", "Add a retry budget", domain.BeadStateClosed, 2),
		gvBead("sc-3c4d", "Retry failed HTTP requests", domain.BeadStateInReview, 1, "sc-1a2b"),
		gvBead("sc-5e6f", "Back off exponentially", domain.BeadStateInProgress, 0, "sc-3c4d"),
		gvBead("sc-7g8h", "Document the retry policy", domain.BeadStateOpen, 0, "sc-5e6f"),
		gvBead("sc-9i0j", "Release notes for retries", domain.BeadStateOpen, 0, "sc-7g8h"),
	}
}

// gvDiamond is the classic diamond: one root, two independent middles and a
// sink that waits for both.
func gvDiamond() []domain.Bead {
	return []domain.Bead{
		gvBead("sc-root", "Extract the HTTP client", domain.BeadStateClosed, 1),
		gvBead("sc-left", "Retry idempotent requests", domain.BeadStateChangesRequested, 2, "sc-root"),
		gvBead("sc-rght", "Cache DNS lookups", domain.BeadStateApproved, 1, "sc-root"),
		gvBead("sc-sink", "Benchmark the client", domain.BeadStateOpen, 0, "sc-left", "sc-rght"),
	}
}

// gvFanOut is one root with eight beads waiting on it: the widest level the
// golden frames have to scroll through.
func gvFanOut() []domain.Bead {
	out := []domain.Bead{gvBead("sc-base", "Split the god object", domain.BeadStateClosed, 3)}
	titles := []string{
		"Move the parser out",
		"Move the printer out",
		"Move the linter out",
		"Move the loader out",
		"Move the cache out",
		"Move the metrics out",
		"Move the tracing out",
		"Move the config out",
	}
	states := []domain.BeadState{
		domain.BeadStateClosed, domain.BeadStateInReview, domain.BeadStateInProgress,
		domain.BeadStateOpen, domain.BeadStateOpen, domain.BeadStateFailed,
		domain.BeadStateNeedsHuman, domain.BeadStateOpen,
	}
	for i, title := range titles {
		out = append(out, gvBead(domain.BeadID(fmt.Sprintf("sc-f%02d", i+1)), title, states[i], i%3, "sc-base"))
	}
	return out
}

// gvBig is a fifty-bead graph: ten levels of five beads, each bead blocked
// by one bead of the level above, so every layout has to scroll.
func gvBig() []domain.Bead {
	states := []domain.BeadState{
		domain.BeadStateClosed, domain.BeadStateInProgress, domain.BeadStateInReview,
		domain.BeadStateOpen, domain.BeadStateBlocked,
	}
	var out []domain.Bead
	for level := 0; level < 10; level++ {
		for slot := 0; slot < 5; slot++ {
			id := domain.BeadID(fmt.Sprintf("sc-l%dn%d", level, slot))
			b := gvBead(id, fmt.Sprintf("Level %d task %d", level, slot+1), states[(level+slot)%len(states)], level%3)
			if level > 0 {
				b.Deps = []domain.BeadID{domain.BeadID(fmt.Sprintf("sc-l%dn%d", level-1, slot))}
			}
			out = append(out, b)
		}
	}
	return out
}

// gvSkip is a graph whose dependencies jump over levels: two beads wait on
// the root as well as on the bead in between them, so their edges to the root
// span two and four levels and cannot run straight through one gutter.
func gvSkip() []domain.Bead {
	return []domain.Bead{
		gvBead("sc-aaaa", "Extract the HTTP client", domain.BeadStateClosed, 0),
		gvBead("sc-bbbb", "Retry idempotent requests", domain.BeadStateInProgress, 1, "sc-aaaa"),
		gvBead("sc-cccc", "Benchmark the client", domain.BeadStateOpen, 0, "sc-aaaa", "sc-bbbb"),
		gvBead("sc-dddd", "Document the retry policy", domain.BeadStateOpen, 0, "sc-cccc"),
		gvBead("sc-eeee", "Release notes for retries", domain.BeadStateOpen, 0, "sc-aaaa", "sc-dddd"),
	}
}

// gvSkipEdges are the dependencies gvSkip declares, blocker first.
func gvSkipEdges() [][2]domain.BeadID {
	return [][2]domain.BeadID{
		{"sc-aaaa", "sc-bbbb"},
		{"sc-aaaa", "sc-cccc"},
		{"sc-bbbb", "sc-cccc"},
		{"sc-cccc", "sc-dddd"},
		{"sc-aaaa", "sc-eeee"},
		{"sc-dddd", "sc-eeee"},
	}
}

// gvCycle is a graph the layering cannot place: two beads block each other, a
// third waits on the pair, and a fourth waits on a blocker outside the run.
func gvCycle() []domain.Bead {
	return []domain.Bead{
		gvBead("sc-loop", "Rewrite the scheduler", domain.BeadStateOpen, 0, "sc-back"),
		gvBead("sc-back", "Rewrite the queue", domain.BeadStateInProgress, 1, "sc-loop"),
		gvBead("sc-heir", "Benchmark both halves", domain.BeadStateOpen, 0, "sc-loop"),
		gvBead("sc-free", "Document the queue", domain.BeadStateOpen, 0, "sc-out1"),
	}
}

// gvView builds the view sized to the main pane of a w×h terminal.
func gvView(w, h int, opts GraphOptions) GraphModel {
	opts.NoColor = true
	return NewGraphModel(opts).Resize(w, h-chromeHeight)
}

// gvFrame renders the whole terminal frame with the graph view in the main
// pane: what [Model.View] produces once it dispatches [TabGraph] to a
// [GraphModel].
func gvFrame(t *testing.T, v GraphModel, st *State, w, h int) string {
	t.Helper()
	m := New(t.Context(), Options{Run: st.Run, NoColor: true})
	m.state = st
	m.tab = TabGraph
	m.width, m.height = w, h
	return strings.Join([]string{
		m.header(w),
		m.tabBar(w),
		m.styles.Rule.Render(strings.Repeat("─", max(w, 1))),
		v.View(st),
		m.footer(w),
	}, "\n")
}

// gvGolden compares got with testdata/tui/<name>.golden; UPDATE_GOLDEN=1
// (make update-golden) rewrites it instead.
func gvGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(gvGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// gvKey returns a key press.
func gvKey(k string) tea.KeyMsg {
	if len(k) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// TestGraphViewGolden is the P7-02 acceptance test: golden frames for the
// diamond, the chain, the wide fan-out and a fifty-node graph, each at 80×24
// and 200×50.
func TestGraphViewGolden(t *testing.T) {
	shapes := []struct {
		name  string
		beads []domain.Bead
	}{
		{"diamond", gvDiamond()},
		{"chain", gvChain()},
		{"fanout", gvFanOut()},
		{"50", gvBig()},
	}
	sizes := []struct{ w, h int }{{80, 24}, {200, 50}}
	for _, shape := range shapes {
		for _, size := range sizes {
			name := fmt.Sprintf("%s-%dx%d", shape.name, size.w, size.h)
			t.Run(name, func(t *testing.T) {
				st := gvState(shape.beads)
				v := gvView(size.w, size.h, GraphOptions{})
				gvGolden(t, "graph-"+name, gvFrame(t, v, st, size.w, size.h))
			})
		}
	}
}

// TestGraphViewGoldenVariants covers the frames the four shapes do not: the
// empty run, the narrow fallback, the top→bottom layout, a moved cursor and
// the bead detail panel.
func TestGraphViewGoldenVariants(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		st := NewState(gvRunInfo())
		gvGolden(t, "graph-empty", gvFrame(t, gvView(80, 24, GraphOptions{}), st, 80, 24))
	})

	t.Run("narrow", func(t *testing.T) {
		st := gvState(gvDiamond())
		v := gvView(64, 20, GraphOptions{})
		require.Equal(t, GraphIndentedList, v.Orientation())
		gvGolden(t, "graph-narrow", gvFrame(t, v, st, 64, 20))
	})

	t.Run("vertical", func(t *testing.T) {
		st := gvState(gvDiamond())
		v := gvView(100, 60, GraphOptions{})
		require.Equal(t, GraphTopToBottom, v.Orientation())
		gvGolden(t, "graph-vertical", gvFrame(t, v, st, 100, 60))
	})

	t.Run("cursor", func(t *testing.T) {
		st := gvState(gvDiamond())
		v := gvView(80, 24, GraphOptions{})
		for i := 0; i < 2; i++ {
			var ok bool
			v, ok = v.Update(gvKey("down"), st, DefaultKeyMap())
			require.True(t, ok)
		}
		sel, ok := v.Selected(st)
		require.True(t, ok)
		require.Equal(t, domain.BeadID("sc-rght"), sel.ID)
		gvGolden(t, "graph-cursor", gvFrame(t, v, st, 80, 24))
	})

	t.Run("detail", func(t *testing.T) {
		st := gvState(gvDetailBeads(), gvDetailHistory()...)
		v := gvView(80, 24, GraphOptions{})
		var ok bool
		v, ok = v.Update(gvKey("down"), st, DefaultKeyMap())
		require.True(t, ok)
		v, ok = v.Update(gvKey("enter"), st, DefaultKeyMap())
		require.True(t, ok)
		require.True(t, v.ShowingDetail())
		gvGolden(t, "graph-detail", gvFrame(t, v, st, 80, 24))
	})

	t.Run("detail-scrolled", func(t *testing.T) {
		st := gvState(gvDetailBeads(), gvDetailHistory()...)
		v := gvView(80, 24, GraphOptions{})
		v, _ = v.Update(gvKey("down"), st, DefaultKeyMap())
		v, _ = v.Update(gvKey("enter"), st, DefaultKeyMap())
		for i := 0; i < 6; i++ {
			v, _ = v.Update(gvKey("down"), st, DefaultKeyMap())
		}
		require.Equal(t, 3, v.DetailOffset(), "the offset stops at the last page the panel can show")
		gvGolden(t, "graph-detail-scrolled", gvFrame(t, v, st, 80, 24))
	})

	t.Run("cycle", func(t *testing.T) {
		st := gvState(gvCycle())
		v := gvView(100, 24, GraphOptions{})
		require.Equal(t, GraphLeftToRight, v.Orientation())
		gvGolden(t, "graph-cycle", gvFrame(t, v, st, 100, 24))
	})

	t.Run("cycle-narrow", func(t *testing.T) {
		st := gvState(gvCycle())
		v := gvView(64, 20, GraphOptions{})
		require.Equal(t, GraphIndentedList, v.Orientation())
		gvGolden(t, "graph-cycle-narrow", gvFrame(t, v, st, 64, 20))
	})

	t.Run("skip", func(t *testing.T) {
		st := gvState(gvSkip())
		v := gvView(120, 30, GraphOptions{})
		require.Equal(t, GraphLeftToRight, v.Orientation())
		gvGolden(t, "graph-skip", gvFrame(t, v, st, 120, 30))
	})

	t.Run("skip-vertical", func(t *testing.T) {
		st := gvState(gvSkip())
		v := gvView(100, 60, GraphOptions{})
		require.Equal(t, GraphTopToBottom, v.Orientation())
		gvGolden(t, "graph-skip-vertical", gvFrame(t, v, st, 100, 60))
	})
}

// gvDetailBeads is the graph the detail-panel frames use: a closed blocker
// and the bead the panel is opened on, with a description and acceptance
// criteria long enough to wrap.
func gvDetailBeads() []domain.Bead {
	blocker := gvBead("sc-1a2b", "Extract the HTTP client", domain.BeadStateClosed, 1)
	target := gvBead("sc-3c4d", "Retry failed HTTP requests", domain.BeadStateChangesRequested, 2, "sc-1a2b", "sc-out1")
	target.Description = "The client gives up on the first 5xx. It should retry idempotent requests with an exponential back-off, and stop after the retry budget is spent."
	target.Acceptance = "A 503 followed by a 200 returns the body; the number of attempts is capped by the budget; nothing is retried after a POST."
	target.Size = domain.SizeM
	return []domain.Bead{blocker, target, gvBead("sc-5e6f", "Document the retry policy", domain.BeadStateOpen, 0, "sc-3c4d")}
}

// gvDetailHistory is the review history the detail panel renders.
func gvDetailHistory() []events.Event {
	return []events.Event{
		events.AgentSpawned{
			Meta:      gvMeta(2),
			Agent:     domain.Agent{ID: "engineer-1", Role: domain.RoleEngineer, Cartridge: "mock", Model: "fable", Bead: "sc-3c4d", State: domain.AgentStateRunning, StartedAt: gvAt(2)},
			Task:      events.TaskImplement,
			BeadTitle: "Retry failed HTTP requests",
			Round:     1,
		},
		events.RoundStarted{Meta: gvMeta(3), Bead: "sc-3c4d", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA, domain.LensTest}, Commit: "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"},
		events.VerdictReceived{Meta: gvMeta(4), Bead: "sc-3c4d", Round: 1, Agent: "qa-1", Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "the budget is never reset",
			Items: []domain.FeedbackItem{{ID: "qa-r1-1", Severity: domain.SeverityBlocker, Title: "Budget leaks between calls", Status: domain.ItemOpen}},
		}},
		events.VerdictReceived{Meta: gvMeta(5), Bead: "sc-3c4d", Round: 1, Agent: "test-1", Verdict: domain.Verdict{
			Lens: domain.LensTest, Round: 1, Outcome: domain.VerdictApprove, Summary: "tests cover the back-off",
		}},
		events.ResponseReceived{Meta: gvMeta(6), Bead: "sc-3c4d", Round: 1, Agent: "engineer-1", Responses: []domain.Response{
			{ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "the budget is now per call"},
		}},
		events.RoundStarted{Meta: gvMeta(7), Bead: "sc-3c4d", Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"},
		events.PROpened{Meta: gvMeta(8), Bead: "sc-3c4d", URL: "https://example.invalid/acme/app/pull/42", Number: 42, Branch: "sc/sc-3c4d-retry", Base: "main"},
	}
}

// TestBuildGraphLayout checks the layering, readiness and agent lookup the
// view draws from, using the diamond whose levels are known.
func TestBuildGraphLayout(t *testing.T) {
	st := gvState(gvDiamond())
	lay := BuildGraphLayout(st)

	require.Equal(t, 4, lay.Len())
	require.Len(t, lay.Layers, 3)
	require.Equal(t, []domain.BeadID{"sc-root"}, gvIDs(lay, lay.Layers[0]))
	require.Equal(t, []domain.BeadID{"sc-left", "sc-rght"}, gvIDs(lay, lay.Layers[1]))
	require.Equal(t, []domain.BeadID{"sc-sink"}, gvIDs(lay, lay.Layers[2]))

	sink, ok := lay.Node("sc-sink")
	require.True(t, ok)
	require.Equal(t, 2, sink.Layer)
	require.Equal(t, 0, sink.Slot)
	require.True(t, sink.Blocked)
	require.False(t, sink.Ready)
	require.Equal(t, []domain.BeadID{"sc-left", "sc-rght"}, sink.Blockers)

	root, ok := lay.Node("sc-root")
	require.True(t, ok)
	require.Equal(t, []domain.BeadID{"sc-left", "sc-rght"}, root.Dependents)

	ready, blocked, closed := lay.Counts()
	require.Equal(t, 0, ready, "the middles are past open and the sink still waits")
	require.Equal(t, 1, blocked, "only the sink has an open blocker")
	require.Equal(t, 1, closed)

	_, ok = lay.Index("sc-nope")
	require.False(t, ok)
}

// TestBuildGraphLayoutReadyAndAgents checks that a bead whose blockers are
// all closed is reported ready, and that the live agent on a bead wins over
// the finished one.
func TestBuildGraphLayoutReadyAndAgents(t *testing.T) {
	beads := []domain.Bead{
		gvBead("sc-1a2b", "Extract the HTTP client", domain.BeadStateClosed, 1),
		gvBead("sc-3c4d", "Retry failed HTTP requests", domain.BeadStateOpen, 0, "sc-1a2b"),
	}
	st := gvState(beads,
		events.AgentSpawned{Meta: gvMeta(2), Agent: domain.Agent{ID: "engineer-1", Role: domain.RoleEngineer, Bead: "sc-3c4d", State: domain.AgentStateRunning}, Task: events.TaskImplement},
		events.AgentFinished{Meta: gvMeta(3), Agent: "engineer-1", Role: domain.RoleEngineer, Bead: "sc-3c4d", ExitCode: 0},
		events.AgentSpawned{Meta: gvMeta(4), Agent: domain.Agent{ID: "qa-1", Role: domain.RoleTester, Lens: domain.LensQA, Bead: "sc-3c4d", State: domain.AgentStateRunning}, Task: events.TaskReview},
	)
	lay := BuildGraphLayout(st)

	n, ok := lay.Node("sc-3c4d")
	require.True(t, ok)
	require.True(t, n.Ready)
	require.False(t, n.Blocked)
	require.True(t, n.HasAgent)
	require.Equal(t, domain.AgentID("qa-1"), n.Agent.ID)
	require.Equal(t, RoleGlyph(domain.RoleTester), n.AgentGlyph())
	require.Equal(t, "", n.Badge())
}

// TestBuildGraphLayoutCycleAndDangling checks that a bead on a cycle still
// gets drawn, in a level of its own, and that a blocker outside the run is
// reported rather than silently dropped.
func TestBuildGraphLayoutCycleAndDangling(t *testing.T) {
	beads := []domain.Bead{
		gvBead("sc-1a2b", "First", domain.BeadStateOpen, 0, "sc-3c4d"),
		gvBead("sc-3c4d", "Second", domain.BeadStateOpen, 0, "sc-1a2b"),
		gvBead("sc-5e6f", "Third", domain.BeadStateOpen, 0, "sc-out1"),
	}
	lay := BuildGraphLayout(gvState(beads))

	require.Equal(t, 3, lay.Len())
	require.Len(t, lay.Cycles, 1)
	require.Equal(t, []domain.BeadID{"sc-out1"}, lay.Dangling)

	first, ok := lay.Node("sc-1a2b")
	require.True(t, ok)
	require.True(t, first.Cycle)
	require.Equal(t, len(lay.Layers)-1, first.Layer)

	third, ok := lay.Node("sc-5e6f")
	require.True(t, ok)
	require.False(t, third.Cycle)
	require.True(t, third.Blocked, "a blocker outside the run counts as open")
}

// TestBuildGraphLayoutEmpty checks the nil and empty cases every accessor
// has to survive before the first plan arrives.
func TestBuildGraphLayoutEmpty(t *testing.T) {
	for name, lay := range map[string]GraphLayout{
		"nil":   BuildGraphLayout(nil),
		"empty": BuildGraphLayout(NewState(gvRunInfo())),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, 0, lay.Len())
			require.Empty(t, lay.Layers)
			_, ok := lay.Node("sc-1a2b")
			require.False(t, ok)
			ready, blocked, closed := lay.Counts()
			require.Equal(t, 0, ready+blocked+closed)
		})
	}
}

// gvIDs maps node indices back to bead ids.
func gvIDs(lay GraphLayout, idx []int) []domain.BeadID {
	out := make([]domain.BeadID, 0, len(idx))
	for _, i := range idx {
		out = append(out, lay.Nodes[i].ID)
	}
	return out
}

// TestGraphModelOrientation checks the layout the view picks for a pane.
func TestGraphModelOrientation(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want GraphOrientation
	}{
		{"standard terminal", 80, 20, GraphLeftToRight},
		{"wide terminal", 200, 46, GraphLeftToRight},
		{"narrow terminal", 60, 20, GraphIndentedList},
		{"tall pane", 100, 56, GraphTopToBottom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewGraphModel(GraphOptions{NoColor: true}).Resize(tt.w, tt.h)
			require.Equal(t, tt.want, v.Orientation())
		})
	}

	pinned := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).Resize(40, 10)
	require.Equal(t, GraphTopToBottom, pinned.Orientation(), "a pinned layout survives a narrow pane")
	require.Equal(t, "auto", GraphAuto.String())
	require.Equal(t, "left→right", GraphLeftToRight.String())
}

// TestGraphModelNavigation checks the cursor: Up and Down walk the beads in
// level order, Left and Right change level, and g/G jump to the ends.
func TestGraphModelNavigation(t *testing.T) {
	st := gvState(gvDiamond())
	keys := DefaultKeyMap()
	v := gvView(120, 30, GraphOptions{})

	sel, ok := v.Selected(st)
	require.True(t, ok)
	require.Equal(t, domain.BeadID("sc-root"), sel.ID)

	v, ok = v.Update(gvKey("down"), st, keys)
	require.True(t, ok)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-left"), sel.ID)

	v, ok = v.Update(gvKey("right"), st, keys)
	require.True(t, ok)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-sink"), sel.ID, "the next level has one bead, so the slot clamps")

	v, ok = v.Update(gvKey("left"), st, keys)
	require.True(t, ok)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-left"), sel.ID)

	v, _ = v.Update(gvKey("G"), st, keys)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-sink"), sel.ID)

	v, _ = v.Update(gvKey("g"), st, keys)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-root"), sel.ID)

	// The ends hold: up from the first bead and left from level 0 stay put.
	v, ok = v.Update(gvKey("up"), st, keys)
	require.True(t, ok)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-root"), sel.ID)
	v, ok = v.Update(gvKey("left"), st, keys)
	require.True(t, ok)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-root"), sel.ID)

	// Keys the view has no use for are left to the global bindings.
	_, ok = v.Update(gvKey("z"), st, keys)
	require.False(t, ok)
	_, ok = v.Update(tea.WindowSizeMsg{Width: 10, Height: 10}, st, keys)
	require.False(t, ok)
}

// TestGraphModelDetailPanel checks that enter opens the panel, Up and Down
// scroll it, and esc closes it again.
func TestGraphModelDetailPanel(t *testing.T) {
	st := gvState(gvDetailBeads(), gvDetailHistory()...)
	keys := DefaultKeyMap()
	v := gvView(80, 24, GraphOptions{})
	v, _ = v.Update(gvKey("down"), st, keys)

	v, ok := v.Update(gvKey("enter"), st, keys)
	require.True(t, ok)
	require.True(t, v.ShowingDetail())

	body := v.View(st)
	require.Contains(t, body, "sc-3c4d")
	require.Contains(t, body, "Description")
	require.Contains(t, body, "exponential back-off")
	require.Contains(t, body, "Acceptance")
	require.Contains(t, body, "History")
	require.Contains(t, body, "sc-1a2b")
	require.Contains(t, body, "outside the run", "a blocker that is not in the run is named as such")

	v, ok = v.Update(gvKey("down"), st, keys)
	require.True(t, ok)
	require.Equal(t, 1, v.DetailOffset())
	v, ok = v.Update(gvKey("up"), st, keys)
	require.True(t, ok)
	require.Equal(t, 0, v.DetailOffset())
	v, ok = v.Update(gvKey("up"), st, keys)
	require.True(t, ok)
	require.Equal(t, 0, v.DetailOffset(), "the panel does not scroll above its first line")

	// Scrolling stops at the end: the last page stays on screen.
	for i := 0; i < 200; i++ {
		v, _ = v.Update(gvKey("down"), st, keys)
	}
	require.Contains(t, v.View(st), "esc close")

	v, ok = v.Update(gvKey("esc"), st, keys)
	require.True(t, ok)
	require.False(t, v.ShowingDetail())
	require.Equal(t, 0, v.DetailOffset())
}

// TestGraphModelDetailNeedsABead checks that enter on an empty run does
// nothing, so the panel can never open on a bead that is not there.
func TestGraphModelDetailNeedsABead(t *testing.T) {
	st := NewState(gvRunInfo())
	v, ok := gvView(80, 24, GraphOptions{}).Update(gvKey("enter"), st, DefaultKeyMap())
	require.False(t, ok)
	require.False(t, v.ShowingDetail())
	require.Contains(t, v.View(st), emptyText[TabGraph])
}

// TestGraphViewSizeAndScroll checks the pane's shape: the view always fills
// exactly the height it was resized to, and the viewport follows the cursor
// through a graph too big to fit.
func TestGraphViewSizeAndScroll(t *testing.T) {
	st := gvState(gvBig())
	keys := DefaultKeyMap()
	sizes := []struct{ w, h int }{{80, 24}, {200, 50}, {64, 20}, {100, 60}}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			v := gvView(size.w, size.h, GraphOptions{})
			w, h := v.Size()
			require.Equal(t, size.w, w)
			require.Equal(t, size.h-chromeHeight, h)
			for _, frame := range []string{v.View(st), gvFrame(t, v, st, size.w, size.h)} {
				for _, line := range strings.Split(frame, "\n") {
					require.LessOrEqual(t, lipgloss.Width(line), size.w)
				}
			}
			require.Len(t, strings.Split(v.View(st), "\n"), size.h-chromeHeight)
		})
	}

	v := gvView(80, 24, GraphOptions{})
	x, y := v.Viewport(st)
	require.Equal(t, 0, x)
	require.Equal(t, 0, y)
	// The last bead of the last level is off screen both ways.
	v, _ = v.Update(gvKey("G"), st, keys)
	x, y = v.Viewport(st)
	require.Greater(t, x, 0)
	require.Greater(t, y, 0)
	sel, _ := v.Selected(st)
	require.Contains(t, v.View(st), string(sel.ID), "the selected bead is scrolled into view")
}

// TestGraphViewBoxesAndEdges checks the drawing itself: every bead is boxed,
// the selected one has a heavy border, and the levels are joined by edges.
func TestGraphViewBoxesAndEdges(t *testing.T) {
	st := gvState(gvDiamond())
	v := gvView(120, 30, GraphOptions{})
	out := v.View(st)

	for _, id := range []domain.BeadID{"sc-root", "sc-left", "sc-rght", "sc-sink"} {
		require.Contains(t, out, string(id))
	}
	require.Contains(t, out, "┌", "unselected beads are boxed with a light border")
	require.Contains(t, out, "┏", "the selected bead is boxed with a heavy border")
	require.Contains(t, out, "─", "levels are joined by edges")
	require.Contains(t, out, "┬", "two edges leaving one bead share a channel")
	require.Contains(t, out, "└", "an edge turns into the bead it points at")
	require.Contains(t, out, StateGlyph(domain.BeadStateClosed))
	require.Contains(t, out, "4 beads")
	require.Contains(t, out, "3 levels")
	require.Contains(t, out, string(GraphLeftToRight))

	// The round badge and the bead title are inside the box.
	require.Contains(t, out, "r1")
	require.Contains(t, out, "Extract the HTTP")
}

// TestGraphViewVerticalEdges checks the top→bottom layout draws its edges
// downwards between the level rows.
func TestGraphViewVerticalEdges(t *testing.T) {
	st := gvState(gvDiamond())
	v := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).Resize(100, 40)
	out := v.View(st)
	require.Contains(t, out, string(GraphTopToBottom))
	require.Contains(t, out, "│")
	require.True(t, strings.ContainsAny(out, "┌┐└┘├┤┬┴┼"), "the edges turn between the level rows")
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 40)
}

// TestGraphViewNarrowList checks the indented fallback: one line per bead,
// grouped by level, with the blockers named.
func TestGraphViewNarrowList(t *testing.T) {
	st := gvState(gvDiamond())
	v := gvView(64, 20, GraphOptions{})
	out := v.View(st)

	require.Contains(t, out, "level 0 (1)")
	require.Contains(t, out, "level 1 (2)")
	require.Contains(t, out, "level 2 (1)")
	require.Contains(t, out, "← sc-left,sc-rght")
	require.NotContains(t, out, "┌", "the narrow fallback draws no boxes")
	require.Len(t, strings.Split(out, "\n"), 20-chromeHeight)
}

// TestGraphKeyMap checks the view's own bindings and their help.
func TestGraphKeyMap(t *testing.T) {
	k := DefaultGraphKeyMap()
	require.Len(t, k.ShortHelp(), 3)
	require.Len(t, k.FullHelp(), 3)
	require.Equal(t, k, NewGraphModel(GraphOptions{NoColor: true}).Keys())

	custom := DefaultGraphKeyMap()
	custom.Open.SetHelp("enter", "details")
	v := NewGraphModel(GraphOptions{NoColor: true, Keys: &custom})
	require.Equal(t, "details", v.Keys().Open.Help().Desc)
}

// TestGraphWrap checks the word wrapper the detail panel uses.
func TestGraphWrap(t *testing.T) {
	require.Equal(t, []string{"a b", "c"}, graphWrap("a b c", 3))
	require.Equal(t, []string{"one", "", "two"}, graphWrap("one\n\ntwo", 10))
	require.Equal(t, []string{"abcd", "efg"}, graphWrap("abcdefg", 4), "a word too long is split")
	require.Nil(t, graphWrap("anything", 0))
}

// TestGraphPadAndTrunc checks the column helpers keep their promise about
// width, ellipsis included.
func TestGraphPadAndTrunc(t *testing.T) {
	require.Equal(t, "ab  ", graphPad("ab", 4))
	require.Equal(t, "abcd", graphPad("abcdef", 4))
	require.Equal(t, "", graphPad("ab", 0))
	require.Equal(t, "abc…", graphTrunc("abcdef", 4))
	require.Equal(t, "abcd", graphTrunc("abcd", 4))
	require.Equal(t, "…", graphTrunc("abcd", 1))
	require.Equal(t, "", graphTrunc("abcd", 0))
}

// TestGraphCanvasJunctions checks that two edges crossing the same cell come
// out as one junction rune rather than the last one drawn.
func TestGraphCanvasJunctions(t *testing.T) {
	c := newGraphCanvas(5, 3)
	c.polyline([]graphPoint{{0, 1}, {4, 1}}, nil, false)
	c.polyline([]graphPoint{{2, 0}, {2, 2}}, nil, false)
	require.Equal(t, "──┼──", c.row(1, 0, 5))

	c.text(0, 0, "hi", nil)
	require.Equal(t, "hi│", c.row(0, 0, 5))
	require.Equal(t, "", c.row(9, 0, 5), "a row off the canvas is empty")
	require.Equal(t, "", c.row(0, 0, 0))
}

// TestGraphCanvasKeepsBoxes checks that an edge routed across a box is
// broken rather than drawn through its border.
func TestGraphCanvasKeepsBoxes(t *testing.T) {
	c := newGraphCanvas(5, 1)
	c.text(2, 0, "X", nil)
	c.polyline([]graphPoint{{0, 0}, {4, 0}}, nil, false)
	require.Equal(t, "──X──", c.row(0, 0, 5))
}

// gvDirections pairs each direction bit with its opposite and the step that
// follows it, so a walk over the drawn edges only crosses cells that agree
// they are joined.
var gvDirections = []struct {
	dir, opposite uint8
	dx, dy        int
}{
	{graphNorth, graphSouth, 0, -1},
	{graphSouth, graphNorth, 0, 1},
	{graphEast, graphWest, 1, 0},
	{graphWest, graphEast, -1, 0},
}

// gvReaches reports whether the edges drawn on the canvas join two cells: it
// walks the direction masks, so a route chopped in half by a box in the way
// does not count as joined.
func gvReaches(c *graphCanvas, from, to graphPoint) bool {
	if !c.inside(from.x, from.y) || c.maskAt(from.x, from.y) == 0 {
		return false
	}
	seen := map[graphPoint]bool{from: true}
	for queue := []graphPoint{from}; len(queue) > 0; queue = queue[1:] {
		p := queue[0]
		if p == to {
			return true
		}
		mask := c.maskAt(p.x, p.y)
		for _, d := range gvDirections {
			q := graphPoint{p.x + d.dx, p.y + d.dy}
			if mask&d.dir == 0 || !c.inside(q.x, q.y) || seen[q] {
				continue
			}
			if c.maskAt(q.x, q.y)&d.opposite == 0 {
				continue
			}
			seen[q] = true
			queue = append(queue, q)
		}
	}
	return false
}

// gvEnds returns the cell an edge leaves the blocker's box from and the cell
// it arrives at beside the dependent's box.
func gvEnds(src, dst graphBox, vertical bool) (from, to graphPoint) {
	if vertical {
		return graphPoint{src.midX(), src.y + src.h}, graphPoint{dst.midX(), dst.y - 1}
	}
	return graphPoint{src.x + src.w, src.midY()}, graphPoint{dst.x - 1, dst.midY()}
}

// gvAssertNoEdgeOnBoxes requires that no route was laid over a box, which is
// what used to chop an edge into stubs.
func gvAssertNoEdgeOnBoxes(t *testing.T, lay GraphLayout, canvas *graphCanvas, boxes []graphBox) {
	t.Helper()
	for i, b := range boxes {
		for y := b.y; y < b.y+b.h; y++ {
			for x := b.x; x < b.x+b.w; x++ {
				require.Zero(t, canvas.maskAt(x, y),
					"an edge was routed onto the box of %s at (%d,%d)", lay.Nodes[i].ID, x, y)
			}
		}
	}
}

// gvAssertEdgesDrawn requires every named forward dependency to be drawn as
// one unbroken route, and no route to have landed on a box.
func gvAssertEdgesDrawn(t *testing.T, lay GraphLayout, vertical bool, want [][2]domain.BeadID) {
	t.Helper()
	v := NewGraphModel(GraphOptions{NoColor: true}).Resize(400, 200)
	canvas, boxes := v.paint(lay, vertical, 0)
	for _, e := range want {
		si, ok := lay.Index(e[0])
		require.True(t, ok)
		di, ok := lay.Index(e[1])
		require.True(t, ok)
		from, to := gvEnds(boxes[si], boxes[di], vertical)
		require.True(t, gvReaches(canvas, from, to),
			"the edge %s → %s is not drawn as one unbroken route", e[0], e[1])
	}
	gvAssertNoEdgeOnBoxes(t, lay, canvas, boxes)
}

// TestGraphViewSkipLevelEdges is the regression test for edges whose blocker
// is more than one level back: they used to be chopped where they crossed the
// boxes in between, which both hid the real dependency and left stubs that
// read as dependencies that do not exist.
func TestGraphViewSkipLevelEdges(t *testing.T) {
	lay := BuildGraphLayout(gvState(gvSkip()))
	require.Len(t, lay.Layers, 5)
	for name, vertical := range map[string]bool{"left→right": false, "top→bottom": true} {
		t.Run(name, func(t *testing.T) {
			gvAssertEdgesDrawn(t, lay, vertical, gvSkipEdges())
		})
	}
}

// TestGraphViewSkipLevelEdgesMinimal checks the smallest graph with a skip:
// a root, a middle bead that waits on it, and a sink that waits on both.
func TestGraphViewSkipLevelEdgesMinimal(t *testing.T) {
	lay := BuildGraphLayout(gvState([]domain.Bead{
		gvBead("sc-root", "Root", domain.BeadStateClosed, 0),
		gvBead("sc-midd", "Middle", domain.BeadStateOpen, 0, "sc-root"),
		gvBead("sc-sink", "Sink", domain.BeadStateOpen, 0, "sc-root", "sc-midd"),
	}))
	want := [][2]domain.BeadID{{"sc-root", "sc-midd"}, {"sc-root", "sc-sink"}, {"sc-midd", "sc-sink"}}
	for name, vertical := range map[string]bool{"left→right": false, "top→bottom": true} {
		t.Run(name, func(t *testing.T) {
			gvAssertEdgesDrawn(t, lay, vertical, want)
		})
	}
}

// TestGraphEdges checks how each dependency is classified and that every
// detour gets a bypass lane of its own.
func TestGraphEdges(t *testing.T) {
	edges, skips, backs := graphEdges(BuildGraphLayout(gvState(gvSkip())))
	require.Len(t, edges, 6)
	require.Equal(t, 2, skips, "sc-aaaa blocks two beads more than one level along")
	require.Equal(t, 0, backs)
	lanes := map[int]bool{}
	for _, e := range edges {
		if e.kind != graphEdgeSkip {
			require.Equal(t, -1, e.lane, "an edge that needs no detour owns no lane")
			continue
		}
		require.False(t, lanes[e.lane], "two detours share lane %d", e.lane)
		lanes[e.lane] = true
	}
	require.Len(t, lanes, 2)

	// A blocker outside the run has no box to draw from, and the cycle
	// bucket's own edges are the only ones that do not move a level forward.
	edges, skips, backs = graphEdges(BuildGraphLayout(gvState(gvCycle())))
	require.Len(t, edges, 3, "the dangling blocker of sc-free is not drawn")
	require.Equal(t, 0, skips)
	require.Equal(t, 3, backs)
}

// TestGraphViewCycleAndDangling renders the graphs the layering cannot place:
// the cycle is named in the summary, on every box that is stuck in it and in
// the narrow list's level heading, and the blocker outside the run is counted
// rather than silently dropped.
func TestGraphViewCycleAndDangling(t *testing.T) {
	st := gvState(gvCycle())
	lay := BuildGraphLayout(st)
	require.Len(t, lay.Cycles, 1)
	require.Equal(t, []domain.BeadID{"sc-out1"}, lay.Dangling)

	boxed := gvView(100, 24, GraphOptions{}).View(st)
	require.Contains(t, boxed, "1 cycle", "the summary counts the cycle, in the singular")
	require.NotContains(t, boxed, "1 cycles")
	require.Contains(t, boxed, "1 outside the run")
	require.Equal(t, 3, strings.Count(boxed, "─"+graphCycleLabel),
		"every bead stuck in the cycle says so on its border")

	// The cycle's own edges are drawn too, through lanes past the last level.
	canvas, boxes := gvView(100, 24, GraphOptions{}).paint(lay, false, 0)
	gvAssertNoEdgeOnBoxes(t, lay, canvas, boxes)
	for _, e := range [][2]domain.BeadID{{"sc-loop", "sc-back"}, {"sc-back", "sc-loop"}, {"sc-loop", "sc-heir"}} {
		si, _ := lay.Index(e[0])
		di, _ := lay.Index(e[1])
		require.True(t, gvReaches(canvas,
			graphPoint{boxes[si].x + boxes[si].w, boxes[si].midY()},
			graphPoint{boxes[di].x + boxes[di].w, boxes[di].midY()}),
			"the cycle edge %s → %s leaves and re-enters on the far side", e[0], e[1])
	}

	// The same edges survive the top→bottom layout, where the lanes run below
	// the last level instead of beside it.
	tall := NewGraphModel(GraphOptions{NoColor: true, Orientation: GraphTopToBottom}).Resize(120, 60)
	canvas, boxes = tall.paint(lay, true, 0)
	gvAssertNoEdgeOnBoxes(t, lay, canvas, boxes)
	for _, e := range [][2]domain.BeadID{{"sc-loop", "sc-back"}, {"sc-back", "sc-loop"}, {"sc-loop", "sc-heir"}} {
		si, _ := lay.Index(e[0])
		di, _ := lay.Index(e[1])
		require.True(t, gvReaches(canvas,
			graphPoint{boxes[si].midX(), boxes[si].y + boxes[si].h},
			graphPoint{boxes[di].midX(), boxes[di].y + boxes[di].h}),
			"the cycle edge %s → %s is not drawn top→bottom", e[0], e[1])
	}

	// The detail panel names the cycle too, and says so when nothing has
	// happened to the bead yet.
	detail := gvView(80, 24, GraphOptions{})
	for i := 0; i < 2; i++ {
		detail, _ = detail.Update(gvKey("down"), st, DefaultKeyMap())
	}
	detail, _ = detail.Update(gvKey("enter"), st, DefaultKeyMap())
	sel, ok := detail.Selected(st)
	require.True(t, ok)
	require.True(t, sel.Cycle)
	pane := detail.View(st)
	require.Contains(t, pane, "on a dependency cycle")
	require.Contains(t, pane, "nothing has happened to this bead yet")
	require.Nil(t, detail.detailHistory(nil, sel), "a view with no run state has no history to show")

	list := gvView(64, 20, GraphOptions{}).View(st)
	require.Contains(t, list, "cycle (3)", "the narrow list names the bucket the cycle went in")
	require.Contains(t, list, "level 0 (1)")
	require.Contains(t, list, "← sc-out1", "the blocker outside the run is still named")
}

// TestGraphModelDetailScrollIsBounded checks that the detail panel's offset
// stops at the last page, so scrolling back up moves on the first press
// instead of first undoing every wasted keystroke.
func TestGraphModelDetailScrollIsBounded(t *testing.T) {
	st := gvState(gvDetailBeads(), gvDetailHistory()...)
	keys := DefaultKeyMap()
	v := gvView(80, 24, GraphOptions{})
	v, _ = v.Update(gvKey("down"), st, keys)
	v, _ = v.Update(gvKey("enter"), st, keys)

	lay := BuildGraphLayout(st)
	want := v.detailMaxOffset(lay, st, v.Cursor())
	require.Positive(t, want, "the fixture bead has more history than the pane can show")

	for i := 0; i < 30; i++ {
		v, _ = v.Update(gvKey("down"), st, keys)
	}
	require.Equal(t, want, v.DetailOffset())
	before := v.View(st)

	v, ok := v.Update(gvKey("up"), st, keys)
	require.True(t, ok)
	require.Equal(t, want-1, v.DetailOffset())
	require.NotEqual(t, before, v.View(st), "one press back up moves the panel")
}

// TestGraphDetailHistoryExtras covers the history lines the review loop only
// writes when it goes wrong or when the work lands: the PO's per-item
// arbitration, its escalation after the round cap, and a merged external PR.
func TestGraphDetailHistoryExtras(t *testing.T) {
	st := gvState(
		[]domain.Bead{gvBead("sc-3c4d", "Retry failed HTTP requests", domain.BeadStateApproved, 3)},
		events.RoundStarted{Meta: gvMeta(2), Bead: "sc-3c4d", Round: 3, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.ArbitrationReceived{Meta: gvMeta(3), Bead: "sc-3c4d", Round: 3, Agent: "po-1",
			Rulings:    []domain.Arbitration{{ItemID: "qa-r3-1", Ruling: domain.RulingUpholdTester, Rationale: "the leak is real"}},
			Escalation: &domain.Escalation{Outcome: domain.EscalationBlock, Summary: "the two cannot agree"},
		},
		events.PROpened{Meta: gvMeta(4), Bead: "sc-3c4d", URL: "https://example.invalid/acme/app/pull/7", Number: 7, External: true},
		events.Merged{Meta: gvMeta(5), Bead: "sc-3c4d", SHA: "abcdef1234567890", Pushed: true},
	)
	lay := BuildGraphLayout(st)
	n, ok := lay.Node("sc-3c4d")
	require.True(t, ok)

	out := strings.Join(gvView(80, 24, GraphOptions{}).detailHistory(st, n), "\n")
	require.Contains(t, out, "arbiter")
	require.Contains(t, out, "uphold_tester the leak is real")
	require.Contains(t, out, "escalated")
	require.Contains(t, out, "block the two cannot agree")
	require.Contains(t, out, "PR #7")
	require.Contains(t, out, "merged abcdef1")
	require.Contains(t, out, "(external)")
}

// TestGraphDetailFacts checks the readiness fact the panel puts under the
// bead's title for a bead nothing is holding up.
func TestGraphDetailFacts(t *testing.T) {
	st := gvState([]domain.Bead{gvBead("sc-1a2b", "Only bead", domain.BeadStateOpen, 0)})
	v, ok := gvView(80, 24, GraphOptions{}).Update(gvKey("enter"), st, DefaultKeyMap())
	require.True(t, ok)
	pane := v.View(st)
	require.Contains(t, pane, "level 0")
	require.Contains(t, pane, "ready")
}

// TestGraphEscalationNote checks that the escalation note comes from whichever
// field the outcome put it in.
func TestGraphEscalationNote(t *testing.T) {
	require.Equal(t, "cannot agree", graphEscalationNote(domain.Escalation{Outcome: domain.EscalationBlock, Summary: "cannot agree", Notes: "n"}))
	require.Equal(t, "shipped as is", graphEscalationNote(domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "shipped as is", NewAcceptance: "a"}))
	require.Equal(t, "just the 503 case", graphEscalationNote(domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "just the 503 case"}))
	require.Equal(t, "", graphEscalationNote(domain.Escalation{}))
}

// TestGraphPlural checks the summary line's word forms.
func TestGraphPlural(t *testing.T) {
	require.Equal(t, "beads", graphPlural(0, "bead", "beads"))
	require.Equal(t, "bead", graphPlural(1, "bead", "beads"))
	require.Equal(t, "beads", graphPlural(2, "bead", "beads"))

	st := gvState([]domain.Bead{gvBead("sc-1a2b", "Only bead", domain.BeadStateOpen, 0)})
	out := gvView(80, 24, GraphOptions{}).View(st)
	require.Contains(t, out, "1 bead")
	require.NotContains(t, out, "1 beads")
	require.Contains(t, out, "1 level")
	require.NotContains(t, out, "1 levels")
}

// gvReplan replaces the run's graph with beads, the way the product owner's
// second plan reaches the view (D-18: it re-plans whenever the ready queue
// empties).
func gvReplan(st *State, beads []domain.Bead) {
	st.Apply(events.GraphChanged{Meta: gvMeta(9), Epic: "sc-epic", Beads: beads})
}

// TestGraphSelectionIsPinnedToTheBead checks that a re-plan cannot move the
// cursor — or an open detail panel — onto a different bead. One new root bead
// renumbers every level after it, so the position the cursor was on belongs
// to somebody else afterwards.
func TestGraphSelectionIsPinnedToTheBead(t *testing.T) {
	st := gvState(gvDiamond())
	keys := DefaultKeyMap()
	v := gvView(120, 30, GraphOptions{})

	v, _ = v.Update(gvKey("down"), st, keys)
	v, _ = v.Update(gvKey("right"), st, keys)
	sel, ok := v.Selected(st)
	require.True(t, ok)
	require.Equal(t, domain.BeadID("sc-sink"), sel.ID)
	v, _ = v.Update(gvKey("enter"), st, keys)
	require.True(t, v.ShowingDetail())
	require.Equal(t, domain.BeadID("sc-sink"), v.SelectedID())
	_, ok = v.Update(gvKey("z"), st, keys)
	require.False(t, ok, "a key the panel has no use for is left to the global map")
	was := v.Cursor()

	// The re-plan puts a new bead on level 0, ahead of every bead that was
	// there: every index after it moves by one.
	replanned := append([]domain.Bead{gvBead("sc-newr", "Audit the timeouts", domain.BeadStateOpen, 0)}, gvDiamond()...)
	gvReplan(st, replanned)
	lay := BuildGraphLayout(st)
	require.Equal(t, 5, lay.Len())
	moved, ok := lay.Index("sc-sink")
	require.True(t, ok)
	require.NotEqual(t, was, moved, "the fixture must move the bead, or it proves nothing")
	require.Equal(t, domain.BeadID("sc-rght"), lay.Nodes[was].ID, "the old position now belongs to another bead")

	sel, ok = v.Selected(st)
	require.True(t, ok)
	require.Equal(t, domain.BeadID("sc-sink"), sel.ID, "the selection follows the bead, not the index")
	require.True(t, v.ShowingDetail())
	pane := v.View(st)
	require.Contains(t, pane, "sc-sink")
	require.NotContains(t, pane, "Cache DNS lookups", "the open panel still describes the bead it was opened on")

	// Moving again starts from where the bead is now, not from the stale row.
	v, _ = v.Update(gvKey("esc"), st, keys)
	require.Equal(t, moved, v.Cursor())
	v, _ = v.Update(gvKey("left"), st, keys)
	sel, _ = v.Selected(st)
	require.Equal(t, domain.BeadID("sc-left"), sel.ID, "left from the sink steps back a level from where it is now")

	// A bead that leaves the run altogether gives the cursor back: the
	// clamped row is used again, and the view keeps working.
	gvReplan(st, gvChain())
	sel, ok = v.Selected(st)
	require.True(t, ok)
	require.NotEmpty(t, sel.ID)
	require.NotPanics(t, func() { _ = v.View(st) })
}

// TestGraphSelectionSurvivesTheBoxedLayouts checks the pinned selection is
// what the scroll offset and the animation targets follow too, so a re-plan
// cannot scroll the pane onto a bead nobody selected.
func TestGraphSelectionSurvivesTheBoxedLayouts(t *testing.T) {
	st := gvState(gvBig())
	keys := DefaultKeyMap()
	v := gvView(80, 24, GraphOptions{Orientation: GraphLeftToRight})
	for range 12 {
		v, _ = v.Update(gvKey("down"), st, keys)
	}
	sel, ok := v.Selected(st)
	require.True(t, ok)
	x, y := v.Viewport(st)

	// A new root bead shifts every index; the viewport must not move.
	gvReplan(st, append([]domain.Bead{gvBead("sc-new0", "Audit the timeouts", domain.BeadStateOpen, 0)}, gvBig()...))
	again, ok := v.Selected(st)
	require.True(t, ok)
	require.Equal(t, sel.ID, again.ID)
	nx, ny := v.Viewport(st)
	require.Equal(t, [2]int{x, y}, [2]int{nx, ny}, "the pane still shows the selected bead")

	target, ok := graphNodeCell(v, st, sel.ID)
	require.True(t, ok, "the animator aims at the selected bead's own box")
	require.GreaterOrEqual(t, target.Row, animGraphTop)
}

// gvHostileTitle is a bead title with a line break in it: a plan is written
// by an LLM, so nothing stops one arriving.
const gvHostileTitle = "boom\nsecond"

// gvHostileEscape is a bead title carrying an ANSI colour sequence.
const gvHostileEscape = "esc \x1b[31mRED\x1b[0m x"

// gvHostile is a graph whose text would corrupt the frame if it were drawn
// as it arrived: a line break splits a row in two, an escape sequence
// repaints it, and Lip Gloss measures both as zero columns.
func gvHostile() []domain.Bead {
	a := gvBead("sc-boom", gvHostileTitle, domain.BeadStateOpen, 0)
	a.Description = "first\n\x1b[31mred\x1b[0m\ttabbed\nlast"
	a.Acceptance = "it \x1b]0;title\x07works"
	b := gvBead("sc-escp", gvHostileEscape, domain.BeadStateInProgress, 1, "sc-boom")
	b.Description = "carriage\rreturn"
	return []domain.Bead{a, b}
}

// gvLines splits a rendered pane and reports the width of each line.
func gvLines(t *testing.T, pane string) ([]string, []int) {
	t.Helper()
	lines := strings.Split(pane, "\n")
	widths := make([]int, len(lines))
	for i, l := range lines {
		widths[i] = lipgloss.Width(l)
	}
	return lines, widths
}

// TestGraphViewScrubsBeadText checks that bead text — which the product
// owner's LLM wrote — cannot split a row, repaint the frame or hide an
// escape sequence in it, in every layout the view has.
func TestGraphViewScrubsBeadText(t *testing.T) {
	st := gvState(gvHostile())
	layouts := []struct {
		name string
		w, h int
		opts GraphOptions
	}{
		{"left-to-right", 200, 50, GraphOptions{Orientation: GraphLeftToRight}},
		{"top-to-bottom", 100, 60, GraphOptions{Orientation: GraphTopToBottom}},
		{"list", 60, 24, GraphOptions{}},
	}
	for _, tc := range layouts {
		t.Run(tc.name, func(t *testing.T) {
			v := gvView(tc.w, tc.h, tc.opts)
			pane := v.View(st)
			lines, widths := gvLines(t, pane)
			require.Len(t, lines, tc.h-chromeHeight, "the pane is exactly as tall as it was sized")
			for i, w := range widths {
				require.LessOrEqualf(t, w, tc.w, "line %d is wider than the pane: %q", i, lines[i])
			}
			require.NotContains(t, pane, "\x1b[31m", "the bead's own escape sequence never reaches the frame")
			require.NotContains(t, pane, "\x1b]", "nor an operating-system command")
			require.Contains(t, pane, "boom second", "the line break became a space")
			require.NotContains(t, pane, "\rreturn")
		})
	}

	// The detail panel wraps the description itself, so it keeps the
	// paragraph breaks and drops everything else.
	v, ok := gvView(80, 24, GraphOptions{}).Update(gvKey("enter"), st, DefaultKeyMap())
	require.True(t, ok)
	pane := v.View(st)
	lines, widths := gvLines(t, pane)
	require.Len(t, lines, 24-chromeHeight)
	for i, w := range widths {
		require.LessOrEqualf(t, w, 80, "detail line %d is wider than the pane: %q", i, lines[i])
	}
	require.NotContains(t, pane, "\x1b[31m")
	require.NotContains(t, pane, "\x1b]0;")
	require.Contains(t, pane, "first")
	require.Contains(t, pane, "red")
	require.Contains(t, pane, "last")
	require.Contains(t, pane, "it works", "an OSC string is dropped whole")
}

// TestGraphScrubbers checks the two scrubbers themselves: one row of text
// and a block that keeps its paragraph breaks.
func TestGraphScrubbers(t *testing.T) {
	require.Equal(t, "plain title", graphText("plain title"))
	require.Equal(t, "", graphText(""))
	require.Equal(t, "boom second", graphText("boom\nsecond"))
	require.Equal(t, "esc RED x", graphText("esc \x1b[31mRED\x1b[0m x"))
	require.Equal(t, "a    b", graphText("a\tb"), "a tab becomes four spaces, as in a transcript")
	require.Equal(t, "carriage return", graphText("carriage\rreturn"))
	require.Equal(t, "no controls", graphBlock("no controls"))
	require.Equal(t, "one\ntwo", graphBlock("one\ntwo"), "paragraph breaks survive")
	require.Equal(t, "one\ntwo", graphBlock("one\r\ntwo"))
	require.Equal(t, "one\nred", graphBlock("one\n\x1b[31mred\x1b[0m"))
	require.True(t, textsafe.IsControl('\x1b'))
	require.True(t, textsafe.IsControl(0x9b))
	require.False(t, textsafe.IsControl('a'))
}

// gvWideTitle is a bead title made of double-width characters.
const gvWideTitle = "修复重试逻辑"

// TestGraphViewDoubleWidthTitles checks a box keeps its shape when the plan
// is not written in ASCII: a CJK or emoji rune takes two terminal cells, and
// a canvas that wrote one rune per cell would push the right border out.
func TestGraphViewDoubleWidthTitles(t *testing.T) {
	beads := []domain.Bead{
		// Long enough to be cut even in the widest box, so the cut lands in
		// the middle of the double-width runes.
		gvBead("sc-wide", strings.Repeat(gvWideTitle, 6), domain.BeadStateOpen, 0),
		gvBead("sc-emji", "ship it 🚀 now", domain.BeadStateInProgress, 1, "sc-wide"),
	}
	st := gvState(beads)
	v := gvView(200, 50, GraphOptions{Orientation: GraphLeftToRight})
	lines, widths := gvLines(t, v.View(st))

	// Lines 1…4 are the two boxes and the edge between them: every row of a
	// box is exactly as wide as the box.
	box := graphBoxWidth(200, 2, 1, false)
	require.Equal(t, GraphMaxBoxWidth, box, "a 200-column pane gives two layers the widest boxes")
	for i := 1; i <= GraphBoxHeight; i++ {
		require.Equalf(t, box, gvBoxWidth(lines[i]),
			"row %d of the box is not %d columns: %q", i, box, lines[i])
	}
	for i, w := range widths {
		require.LessOrEqual(t, w, 200, "line %d: %q", i, lines[i])
	}
	require.Contains(t, v.View(st), "修复重试", "the title is drawn, not dropped")
}

// gvBoxWidth measures the first box on a rendered row: from its left border
// to the matching right one.
func gvBoxWidth(line string) int {
	start := strings.IndexAny(line, "┌│└┏┃┗")
	if start < 0 {
		return 0
	}
	rs := []rune(line[start:])
	for i := 1; i < len(rs); i++ {
		if strings.ContainsRune("┐│┘┓┃┛", rs[i]) {
			return lipgloss.Width(string(rs[:i+1]))
		}
	}
	return lipgloss.Width(string(rs))
}

// TestGraphCanvasDoubleWidthCells checks the canvas itself: a double-width
// rune covers two cells, a window that cuts one in half draws a space in its
// place, and writing over either half never strands the other.
func TestGraphCanvasDoubleWidthCells(t *testing.T) {
	c := newGraphCanvas(6, 1)
	c.text(0, 0, "漢字", nil)
	require.Equal(t, "漢字", c.row(0, 0, 6))
	require.Equal(t, 4, lipgloss.Width(c.row(0, 0, 6)))
	require.Equal(t, "漢", c.row(0, 0, 3), "the second rune would not fit whole")
	require.Equal(t, " 字", c.row(0, 1, 3), "a half rune at the left edge becomes a space")

	// Writing over the left half blanks the right half with it.
	over := newGraphCanvas(6, 1)
	over.text(0, 0, "漢字", nil)
	over.text(0, 0, "ab", nil)
	require.Equal(t, "ab字", over.row(0, 0, 6))

	// …and writing over the right half blanks the left one.
	half := newGraphCanvas(6, 1)
	half.text(0, 0, "漢字", nil)
	half.text(1, 0, "x", nil)
	require.Equal(t, " x字", half.row(0, 0, 6))

	// A control character takes no column, so it is never written at all.
	ctrl := newGraphCanvas(4, 1)
	ctrl.text(0, 0, "a\x1bb", nil)
	require.Equal(t, "ab", ctrl.row(0, 0, 4))

	// An edge never lands on either half of a wide rune.
	edge := newGraphCanvas(6, 1)
	edge.text(0, 0, "漢", nil)
	edge.polyline([]graphPoint{{0, 0}, {5, 0}}, nil, true)
	require.Equal(t, "漢────", edge.row(0, 0, 6), "the edge starts after the wide rune's two cells")

	// A rune the edge of the canvas cuts in half is never drawn as half.
	off := newGraphCanvas(3, 1)
	off.text(2, 0, "漢字", nil)
	require.Equal(t, "", off.row(0, 0, 3), "the last column cannot hold either rune whole")
	off.text(-2, 0, "ab", nil)
	require.Equal(t, "", off.row(0, 0, 3))
	off.text(0, 3, "ab", nil)
	require.Equal(t, "", off.row(3, 0, 3))
}

// TestGraphRule checks the border rule: it is measured in cells, so a
// box-drawing character a terminal renders two cells wide is repeated half as
// often, and a width its glyph cannot divide is padded out with spaces.
func TestGraphRule(t *testing.T) {
	require.Equal(t, "─────", graphRule("─", 5))
	require.Equal(t, "", graphRule("─", 0))
	require.Equal(t, "", graphRule("─", -3))
	require.Equal(t, "", graphRule("", 5), "a glyph of no width would never finish")
	require.Equal(t, "漢漢 ", graphRule("漢", 5), "the odd cell is a space, not half a rune")
	require.Equal(t, 5, lipgloss.Width(graphRule("漢", 5)))
}

// TestGraphKeysAreListedInTheHelpOverlay checks the `?` overlay documents the
// bindings the graph view adds, which are otherwise invisible: `h` and `l`
// shadow the global previous/next view while the graph is up.
func TestGraphKeysAreListedInTheHelpOverlay(t *testing.T) {
	st := gvState(gvDiamond())
	m := New(t.Context(), Options{Run: st.Run, NoColor: true})
	m.state = st
	m.tab = TabGraph
	overlay := m.helpOverlay(80, 20)
	for _, want := range []string{"prev level", "next level", "bead detail", "close detail", "first bead", "last bead"} {
		require.Contains(t, overlay, want)
	}
	require.Contains(t, overlay, m.tab.String()+" keys")
}

// TestThemeGraphBoxesHoldUnderAmbiguousWidth checks a bead box is
// self-consistent in terminal cells: every row of it — the borders included —
// is exactly as wide as the box, and never wider than the pane.
//
// The name starts with "Theme" on purpose:
// [TestThemeGlyphColumnHoldsUnderEastAsianWidth] re-runs `^TestTheme` in a
// child process started with RUNEWIDTH_EASTASIAN=1, where the state glyphs
// and the box-drawing characters are two cells each, so this is the one test
// that sees the graph drawn in that mode. Both the canvas and the borders
// measure cells rather than runes, so the box holds together in either.
func TestThemeGraphBoxesHoldUnderAmbiguousWidth(t *testing.T) {
	st := gvState([]domain.Bead{
		gvBead("sc-1a2b", "Add a retry budget", domain.BeadStateOpen, 2),
		gvBead("sc-3c4d", "修复重试逻辑", domain.BeadStateInProgress, 1, "sc-1a2b"),
	})
	v := gvView(200, 50, GraphOptions{Orientation: GraphLeftToRight})
	lines, widths := gvLines(t, v.View(st))
	for i, w := range widths {
		require.LessOrEqualf(t, w, 200, "line %d is wider than the pane: %q", i, lines[i])
	}
	want := gvBoxWidth(lines[1])
	require.Positive(t, want)
	for i := 1; i <= GraphBoxHeight; i++ {
		require.Equalf(t, want, gvBoxWidth(lines[i]),
			"row %d of the box is %d cells, not %d: %q", i, gvBoxWidth(lines[i]), want, lines[i])
	}
}

// --- P7-24: only the pane is painted ------------------------------------

// TestGraphPaintDrawsOnlyWhatThePaneCanShow pins P7-24. The paint used to
// draw every box of every level onto a canvas the size of the whole graph
// and let [GraphModel.boxLines] copy the visible window out of it, which
// cost 6.8 MB and 68 000 allocations for a 200-bead frame showing a fifth of
// the run. The canvas now holds cells for the scroll window alone and the
// boxes and routes outside it are skipped.
//
// The test proves the two things that matters about the cull: the canvas is
// no bigger than the pane, and the rows it renders are exactly the rows the
// old, whole-graph paint rendered — a box or an edge dropped one cell too
// early would show up as a difference here rather than only in a golden.
func TestGraphPaintDrawsOnlyWhatThePaneCanShow(t *testing.T) {
	t.Parallel()

	beads := gvBig()
	st := gvState(beads)
	lay := BuildGraphLayout(st)
	require.Equal(t, len(beads), lay.Len())

	for _, tc := range []struct {
		name     string
		vertical bool
		cursor   int
	}{
		{"left-right at the top", false, 0},
		{"left-right at the far corner", false, lay.Len() - 1},
		{"top-bottom at the top", true, 0},
		{"top-bottom at the far corner", true, lay.Len() - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const w, h = 80, 24
			v := gvView(w, h, GraphOptions{})
			_, vh := v.Size()
			// The graph is drawn under the view's own summary line.
			pane := vh - 1

			canvas, boxes := v.paint(lay, tc.vertical, tc.cursor)
			require.LessOrEqual(t, len(canvas.runes), w*pane,
				"the canvas is bigger than the pane it is drawn for")
			require.Greater(t, canvas.w*canvas.h, 4*len(canvas.runes),
				"this fixture must be several panes wide, or the cull is not being exercised")

			// The same graph painted whole: a view large enough that its
			// window covers every cell.
			// Its boxes are pinned to the culled paint's width: a bigger pane
			// would otherwise share out wider ones (graphBoxWidth).
			whole := NewGraphModel(GraphOptions{NoColor: true}).Resize(canvas.w+GraphBoxWidth, canvas.h+GraphBoxHeight)
			whole.boxWidth = boxes[0].w
			ref, refBoxes := whole.paint(lay, tc.vertical, tc.cursor)
			require.Equal(t, boxes, refBoxes, "the cull moved a box")
			require.Equal(t, canvas.w*canvas.h, len(ref.runes), "the reference paint is not the whole graph")

			offX, offY := v.scroll(boxes[tc.cursor])
			for y := offY; y < offY+pane; y++ {
				require.Equalf(t, ref.row(y, offX, w), canvas.row(y, offX, w),
					"row %d of the culled paint differs from the whole-graph paint", y)
			}
		})
	}
}

// TestGraphCanvasWindowDropsWritesOutsideIt covers the windowed canvas
// itself: a write outside the window is dropped rather than corrupting a
// neighbouring cell, and reading such a cell back is blank.
func TestGraphCanvasWindowDropsWritesOutsideIt(t *testing.T) {
	t.Parallel()

	c := newGraphWindow(20, 10, 5, 4, 6, 3)
	require.Equal(t, 6*3, len(c.runes))
	require.Equal(t, 20, c.w)
	require.Equal(t, 10, c.h)

	style := ThemeFor(ProfileNoColor).Base()
	c.text(0, 0, "outside", &style)
	c.text(5, 4, "in", &style)
	c.line(0, 0, graphEast, &style, true)
	require.Zero(t, c.maskAt(0, 0), "a mask outside the window was kept")
	require.Equal(t, "", strings.TrimRight(c.row(0, 0, 20), " "), "a row outside the window is blank")
	require.Equal(t, "     in", strings.TrimRight(c.row(4, 0, 20), " "))

	// A window larger than the drawing is clamped to it, so the buffer is
	// never bigger than the canvas it backs.
	full := newGraphWindow(4, 2, 0, 0, 400, 200)
	require.Equal(t, 8, len(full.runes))
}

func TestGraphBoxWidthSharesOutThePane(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		w, layers, span int
		vertical        bool
		want            int
	}{
		{"two layers share 80 columns", 80, 2, 5, false, (80 - GraphGutter) / 2},
		{"too many layers keep the narrowest box", 80, 6, 1, false, GraphBoxWidth},
		{"a wide pane stops at the cap", 300, 2, 1, false, GraphMaxBoxWidth},
		{"top→bottom shares by the widest layer", 60, 9, 2, true, (60 - GraphColGap) / 2},
		{"nothing to lay out", 120, 0, 0, false, GraphBoxWidth},
	} {
		require.Equal(t, c.want, graphBoxWidth(c.w, c.layers, c.span, c.vertical), c.name)
	}
}
