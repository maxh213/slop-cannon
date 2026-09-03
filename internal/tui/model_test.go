package tui_test

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// tuiNewModel builds a sized model reading from evs (nil for no stream).
func tuiNewModel(t *testing.T, evs <-chan events.Event) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Events:  evs,
		Run:     tui.DemoRun(tui.DemoOptions{}),
		NoColor: true,
	})
	return tuiResize(m, 100, 24)
}

// tuiModel narrows what Update returned back to the concrete model.
func tuiModel(next tea.Model) tui.Model {
	m, ok := next.(tui.Model)
	if !ok {
		panic(fmt.Sprintf("tui: Update returned %T, not tui.Model", next))
	}
	return m
}

// tuiUpdate applies one message and returns the model it produced.
func tuiUpdate(m tui.Model, msg tea.Msg) tui.Model {
	next, _ := m.Update(msg)
	return tuiModel(next)
}

// tuiUpdateCmd applies one message and returns the model and its command.
func tuiUpdateCmd(m tui.Model, msg tea.Msg) (tui.Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return tuiModel(next), cmd
}

// tuiResize tells the model the terminal size.
func tuiResize(m tui.Model, w, h int) tui.Model {
	return tuiUpdate(m, tea.WindowSizeMsg{Width: w, Height: h})
}

// tuiKey builds a key press from its printable spelling.
func tuiKey(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// tuiFeed applies every event of the scripted demo run to the model.
func tuiFeed(m tui.Model) tui.Model {
	for _, ev := range tui.DemoEvents(tui.DemoOptions{}) {
		m = tuiUpdate(m, tui.EventMsg{Event: ev})
	}
	return m
}

func TestModelTabsHaveStableNames(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(tui.Tabs()))
	for _, tab := range tui.Tabs() {
		require.True(t, tab.Valid())
		names = append(names, tab.String())
	}
	require.Equal(t, []string{"Graph", "Agents", "Feedback", "PRs", "Log"}, names)
	require.False(t, tui.Tab(99).Valid())
	require.Equal(t, "Tab(99)", tui.Tab(99).String())
}

func TestModelViewIsExactlyTheTerminalSize(t *testing.T) {
	t.Parallel()
	m := tuiFeed(tuiNewModel(t, nil))
	for _, size := range [][2]int{{80, 24}, {200, 50}, {40, 10}, {120, 6}} {
		w, h := size[0], size[1]
		sized := tuiResize(m, w, h)
		lines := strings.Split(sized.View(), "\n")
		require.Len(t, lines, h, "%dx%d", w, h)
		for i, l := range lines {
			require.LessOrEqual(t, lipgloss.Width(l), w, "line %d of %dx%d is too wide: %q", i, w, h, l)
		}
	}
}

func TestModelViewBeforeFirstResizeUsesAFallbackSize(t *testing.T) {
	t.Parallel()
	m := tui.New(t.Context(), tui.Options{NoColor: true, Run: tui.DemoRun(tui.DemoOptions{})})
	w, h := m.Size()
	require.Equal(t, [2]int{0, 0}, [2]int{w, h})
	require.Len(t, strings.Split(m.View(), "\n"), 24)
}

func TestModelHeaderShowsRunModeAmmoAndSafety(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	head := strings.Split(m.View(), "\n")[0]
	require.Contains(t, head, tui.CannonSprite)
	require.Contains(t, head, "slop-cannon")
	require.Contains(t, head, string(tui.DemoRunID))
	require.Contains(t, head, "autopilot")
	require.Contains(t, head, "ammo 0/10")
	require.Contains(t, head, "safety off")

	// Safety on, and two agents alive, are both reflected.
	live := tui.New(t.Context(), tui.Options{NoColor: true, Run: tui.DemoRun(tui.DemoOptions{Safety: true})})
	live = tuiResize(live, 120, 24)
	live = tuiUpdate(live, tui.EventMsg{Event: events.AgentSpawned{
		Meta:  events.NewMeta(tui.DemoRunID),
		Agent: domain.Agent{ID: "eng-1", Role: domain.RoleEngineer, State: domain.AgentStateRunning},
		Task:  events.TaskImplement,
	}})
	head = strings.Split(live.View(), "\n")[0]
	require.Contains(t, head, "ammo 1/10")
	require.Contains(t, head, "safety on")
}

func TestModelTabBarAndTabSwitching(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	bar := strings.Split(m.View(), "\n")[1]
	for i, tab := range tui.Tabs() {
		require.Contains(t, bar, tab.String())
		require.Contains(t, bar, string(rune('1'+i)))
	}

	require.Equal(t, tui.TabGraph, m.Tab())
	m = tuiUpdate(m, tuiKey("tab"))
	require.Equal(t, tui.TabAgents, m.Tab())
	m = tuiUpdate(m, tuiKey("shift+tab"))
	require.Equal(t, tui.TabGraph, m.Tab())
	m = tuiUpdate(m, tuiKey("shift+tab"))
	require.Equal(t, tui.TabLog, m.Tab(), "tabs wrap around")
	m = tuiUpdate(m, tuiKey("tab"))
	require.Equal(t, tui.TabGraph, m.Tab())
}

func TestModelEachTabRendersItsOwnRows(t *testing.T) {
	t.Parallel()
	m := tuiFeed(tuiNewModel(t, nil))
	want := map[tui.Tab]string{
		tui.TabGraph:    "demo-2",
		tui.TabAgents:   "HISTORY",
		tui.TabFeedback: "round 1",
		tui.TabPRs:      "merged",
		tui.TabLog:      "run finished",
	}
	for tab, text := range want {
		shown := tuiUpdate(m, tuiKey(string(rune('1'+int(tab)))))
		require.Equal(t, tab, shown.Tab())
		require.Contains(t, shown.View(), text, "tab %s", tab)
	}
}

func TestModelEmptyTabsSaySo(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	for _, tab := range tui.Tabs() {
		shown := tuiUpdate(m, tuiKey(string(rune('1'+int(tab)))))
		require.Contains(t, shown.View(), "no ", "tab %s should show an empty-state line", tab)
	}
}

func TestModelCursorMovesAndClamps(t *testing.T) {
	t.Parallel()
	m := tuiFeed(tuiNewModel(t, nil))
	require.Equal(t, 0, m.Cursor())
	m = tuiUpdate(m, tuiKey("up"))
	require.Equal(t, 0, m.Cursor(), "cursor stops at the top")
	m = tuiUpdate(m, tuiKey("down"))
	require.Equal(t, 1, m.Cursor())
	for range 100 {
		m = tuiUpdate(m, tuiKey("down"))
	}
	require.Equal(t, len(m.State().Beads)-1, m.Cursor(), "cursor stops at the last row")
	m = tuiUpdate(m, tuiKey("tab"))
	require.Equal(t, 0, m.Cursor(), "switching views resets the cursor")
}

func TestModelHelpOverlayTogglesWithQuestionMark(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	require.False(t, m.HelpVisible())
	require.Contains(t, m.View(), "next view", "the footer shows the short keymap")

	m = tuiUpdate(m, tuiKey("?"))
	require.True(t, m.HelpVisible())
	view := m.View()
	require.Contains(t, view, "Keys")
	require.Contains(t, view, "previous view", "the overlay shows the full keymap")

	// The overlay swallows navigation and closes on ?, esc and q.
	after := tuiUpdate(m, tuiKey("tab"))
	require.Equal(t, tui.TabGraph, after.Tab())
	require.True(t, after.HelpVisible())
	for _, k := range []string{"?", "esc", "q"} {
		require.False(t, tuiUpdate(m, tuiKey(k)).HelpVisible(), "key %q should close the overlay", k)
	}
}

func TestModelQuitConfirmsStopWhileTheRunIsLive(t *testing.T) {
	t.Parallel()
	stops := 0
	m := tui.New(t.Context(), tui.Options{
		NoColor: true,
		Run:     tui.DemoRun(tui.DemoOptions{}),
		OnStop:  func() { stops++ },
	})
	m = tuiResize(m, 100, 24)
	require.True(t, m.State().Live)

	asked, cmd := tuiUpdateCmd(m, tuiKey("q"))
	require.True(t, asked.ConfirmingStop())
	require.Nil(t, cmd, "asking must not quit")
	require.Contains(t, asked.View(), "Stop run "+string(tui.DemoRunID)+"?")
	require.Equal(t, 0, stops)

	// n and esc cancel; the run keeps firing.
	for _, k := range []string{"n", "esc"} {
		cancelled := tuiUpdate(asked, tuiKey(k))
		require.False(t, cancelled.ConfirmingStop(), "key %q should cancel", k)
		require.Equal(t, 0, stops)
	}

	// y confirms: OnStop runs and the program is asked to quit.
	confirmed, cmd := tuiUpdateCmd(asked, tuiKey("y"))
	require.False(t, confirmed.ConfirmingStop())
	require.Equal(t, 1, stops)
	require.NotNil(t, cmd)
	require.Empty(t, confirmed.View(), "a quitting model clears the screen")
}

func TestModelQuitIsImmediateOnceTheRunHasFinished(t *testing.T) {
	t.Parallel()
	m := tuiFeed(tuiNewModel(t, nil))
	require.False(t, m.State().Live)
	quit, cmd := tuiUpdateCmd(m, tuiKey("q"))
	require.False(t, quit.ConfirmingStop())
	require.NotNil(t, cmd)
	require.Equal(t, tea.Quit(), cmd())
}

func TestModelCtrlCQuitsWithoutConfirming(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	quit, cmd := tuiUpdateCmd(m, tuiKey("ctrl+c"))
	require.False(t, quit.ConfirmingStop())
	require.NotNil(t, cmd)
	require.Equal(t, tea.Quit(), cmd())

	// Even out of the confirmation prompt.
	asked := tuiUpdate(m, tuiKey("q"))
	require.True(t, asked.ConfirmingStop())
	_, cmd = tuiUpdateCmd(asked, tuiKey("ctrl+c"))
	require.NotNil(t, cmd)
	require.Equal(t, tea.Quit(), cmd())
}

func TestModelStreamClosedShowsDisconnected(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	require.NotContains(t, m.View(), "disconnected")
	m = tuiUpdate(m, tui.StreamClosedMsg{})
	require.Contains(t, strings.Split(m.View(), "\n")[0], "disconnected")
}

func TestModelDroppedEventsAreReportedInTheFooter(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	m = tuiUpdate(m, tui.EventMsg{Event: events.Warning{Meta: events.NewMeta(tui.DemoRunID), Message: "hi"}, Dropped: 7})
	lines := strings.Split(m.View(), "\n")
	require.Contains(t, lines[len(lines)-1], "missed 7 events")
}

func TestStateFoldsTheWholeDemoRun(t *testing.T) {
	t.Parallel()
	s := tui.NewState(tui.DemoRun(tui.DemoOptions{}))
	for _, ev := range tui.DemoEvents(tui.DemoOptions{}) {
		s.Apply(ev)
	}

	require.Equal(t, tui.DemoRunID, s.Run.ID)
	require.False(t, s.Live)
	require.False(t, s.Paused)
	require.True(t, s.Stopping)
	require.NotNil(t, s.Finish)
	require.Equal(t, events.FinishStopped, s.Finish.Reason)
	require.NotNil(t, s.Plan)
	require.Len(t, s.Plan.Beads, 2)
	require.NotEmpty(t, s.Epic)

	// The graph snapshot replaced the created beads, epic included.
	require.Len(t, s.Beads, 2)
	a, ok := s.Bead("demo-2")
	require.True(t, ok)
	require.Equal(t, domain.BeadStateClosed, a.State)
	require.Equal(t, 2, a.Round)
	b, ok := s.Bead("demo-3")
	require.True(t, ok)
	require.Equal(t, domain.BeadStateNeedsHuman, b.State)
	require.Equal(t, []domain.BeadID{"demo-2"}, b.Deps)

	// Agents: every one terminal by the end, with their outcome recorded.
	require.Empty(t, s.LiveAgents())
	po, ok := s.Agent("po-1")
	require.True(t, ok)
	require.Equal(t, domain.AgentStateFinished, po.State)
	require.Equal(t, events.TaskPlan, po.Task)
	require.Equal(t, domain.MessagePlan, po.Produced)
	require.Equal(t, 2, po.Lines)
	require.Equal(t, "note: no test command configured, using `make test`", po.LastLine)
	failed, ok := s.Agent("eng-demo-3-r1")
	require.True(t, ok)
	require.Equal(t, domain.AgentStateFailed, failed.State)
	require.Equal(t, events.FailureTimeout, failed.Failure)

	// Rounds: two on the first bead, one escalated on the second.
	require.Len(t, s.Rounds, 3)
	require.Equal(t, domain.BeadID("demo-2"), s.Rounds[0].Bead)
	require.Equal(t, 1, s.Rounds[0].Number)
	require.Len(t, s.Rounds[0].Verdicts, 2)
	require.NotNil(t, s.Rounds[0].Report)
	require.Len(t, s.Rounds[1].Rulings, 1)
	require.Len(t, s.Rounds[1].Responses, 2)
	require.NotNil(t, s.Rounds[2].Escalation)

	// Deliveries: the pull request opened and then merged.
	require.Len(t, s.Deliveries, 1)
	require.True(t, s.Deliveries[0].Merged)
	require.Equal(t, 42, s.Deliveries[0].Number)
	require.True(t, s.Deliveries[0].Pushed)

	require.Len(t, s.Notices, 2)
	require.Equal(t, events.KindWarning, s.Notices[0].Kind)
	require.Equal(t, events.KindError, s.Notices[1].Kind)
	require.Equal(t, len(tui.DemoEvents(tui.DemoOptions{})), s.Events)
	require.Len(t, s.Log, s.Events)
}

func TestStateIgnoresNilAndCapsTheLog(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	s.Apply(nil)
	require.Zero(t, s.Events)

	s.MaxLog = 3
	for i := range 10 {
		s.Apply(events.Warning{Meta: events.NewMeta("r"), Message: strings.Repeat("x", i+1)})
	}
	require.Equal(t, 10, s.Events)
	require.Len(t, s.Log, 3)
	require.Contains(t, s.Log[2].Text, strings.Repeat("x", 10), "the newest line is kept")
	require.Contains(t, s.Log[0].Text, strings.Repeat("x", 8))
}

func TestStateAcceptsEventsAboutUnknownBeadsAndAgents(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	s.Apply(events.BeadStateChanged{Meta: events.NewMeta("r"), Bead: "x-1", To: domain.BeadStateInProgress, Round: 2})
	b, ok := s.Bead("x-1")
	require.True(t, ok)
	require.Equal(t, domain.BeadStateInProgress, b.State)
	require.Equal(t, 2, b.Round)
	require.Equal(t, domain.PriorityDefault, b.Priority)

	s.Apply(events.AgentOutput{Meta: events.NewMeta("r"), Agent: "ghost", Stream: events.StreamStdout, Line: "hello"})
	a, ok := s.Agent("ghost")
	require.True(t, ok)
	require.Equal(t, "hello", a.LastLine)
	require.True(t, a.Live())

	s.Apply(events.AgentFailed{Meta: events.NewMeta("r"), Agent: "ghost", Failure: events.FailureKilled})
	a, _ = s.Agent("ghost")
	require.Equal(t, domain.AgentStateKilled, a.State)
	require.False(t, a.Live())
}

func TestStateAmmoAndMode(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	used, total := s.Ammo()
	require.Equal(t, [2]int{0, 0}, [2]int{used, total})
	require.Equal(t, domain.RunModeAutopilot, s.Mode(), "an undescribed run reads as autopilot")

	s.Apply(events.RunStarted{Meta: events.NewMeta("r"), Info: domain.Run{
		ID: "r", Mode: domain.RunModeBrief, Epic: "e-1", Config: domain.ConfigSnapshot{Ammo: 6},
	}, Resumed: true})
	require.Equal(t, domain.RunModeBrief, s.Mode())
	require.True(t, s.Resumed)
	require.Equal(t, domain.BeadID("e-1"), s.Epic)
	_, total = s.Ammo()
	require.Equal(t, 6, total)
}

func TestFormatEventCoversEveryKind(t *testing.T) {
	t.Parallel()
	seen := make(map[events.Kind]string, len(events.Kinds()))
	for _, ev := range tui.DemoEvents(tui.DemoOptions{}) {
		line := tui.FormatEvent(ev)
		require.NotEmpty(t, line, "%s formats as nothing", ev.Kind())
		require.NotContains(t, line, "\n", "%s formats as more than one line", ev.Kind())
		require.NotEqual(t, string(ev.Kind()), line, "%s has no formatter of its own", ev.Kind())
		seen[ev.Kind()] = line
	}
	for _, k := range events.Kinds() {
		require.Contains(t, seen, k, "no scripted event of kind %s", k)
	}
	// Zero values must not panic or print Go syntax.
	for _, k := range events.Kinds() {
		ev, ok := events.New(k)
		require.True(t, ok)
		require.NotContains(t, tui.FormatEvent(ev), "%!", "%s formats badly when empty", k)
	}
}
