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
		require.Equal(t, 6, v.DetailOffset())
		gvGolden(t, "graph-detail-scrolled", gvFrame(t, v, st, 80, 24))
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
