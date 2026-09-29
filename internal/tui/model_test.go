package tui_test

import (
	"context"
	"fmt"
	"io"
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

func TestStateResumeEndsAgentsThePreviousProcessLeftRunning(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	s.Apply(events.RunStarted{Meta: events.NewMeta("r"), Info: domain.Run{ID: "r", Config: domain.ConfigSnapshot{Ammo: 4}}})
	s.Apply(events.AgentSpawned{Meta: events.NewMeta("r"), Agent: domain.Agent{ID: "eng-x-1", Role: domain.RoleEngineer, Bead: "x-1"}})
	s.Apply(events.AgentSpawned{Meta: events.NewMeta("r"), Agent: domain.Agent{ID: "eng-x-2", Role: domain.RoleEngineer, Bead: "x-2"}})
	s.Apply(events.AgentFinished{Meta: events.NewMeta("r"), Agent: "eng-x-2", Role: domain.RoleEngineer, Bead: "x-2"})

	s.Apply(events.RunStarted{Meta: events.NewMeta("r"), Info: domain.Run{ID: "r", Config: domain.ConfigSnapshot{Ammo: 4}}, Resumed: true})
	orphan, _ := s.Agent("eng-x-1")
	require.Equal(t, domain.AgentStateKilled, orphan.State)
	done, _ := s.Agent("eng-x-2")
	require.Equal(t, domain.AgentStateFinished, done.State, "an agent that exited keeps its outcome")
	used, _ := s.Ammo()
	require.Zero(t, used)

	s.Apply(events.AgentSpawned{Meta: events.NewMeta("r"), Agent: domain.Agent{ID: "eng-x-3", Role: domain.RoleEngineer, Bead: "x-3"}})
	used, _ = s.Ammo()
	require.Equal(t, 1, used, "agents the resumed process fires are live")
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

// tuiNastyLine is one line of subprocess output the way a real cartridge CLI
// writes it: SGR colour, a screen-clearing CSI the terminal would honour, an
// OSC string, a C1 control, a tab and an embedded newline that would take a
// second body row the log draws without a cursor gutter.
const tuiNastyLine = "boom \x1b[31mRED\x1b[0m and \x1b[2J\ttabbed \x1b]0;title\x07\x9bx a newline\nSECOND LINE"

// tuiControls returns every character of s a terminal reads as an
// instruction rather than as text.
func tuiControls(s string) []string {
	var out []string
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			out = append(out, fmt.Sprintf("%q", r))
		}
	}
	return out
}

// tuiRequireSafeLine fails when s could do anything to the terminal but put
// printable characters in the row it was drawn in.
func tuiRequireSafeLine(t *testing.T, s, what string) {
	t.Helper()
	require.Empty(t, tuiControls(s), "%s carries control characters: %q", what, s)
}

func TestFormatEventSanitizesUntrustedSubprocessText(t *testing.T) {
	t.Parallel()
	meta := events.NewMeta("r")
	for _, tc := range []struct {
		name  string
		event events.Event
	}{{
		name:  "agent output",
		event: events.AgentOutput{Meta: meta, Agent: "av-1", Stream: events.StreamStdout, Line: tuiNastyLine},
	}, {
		name:  "warning",
		event: events.Warning{Meta: meta, Source: "cartridge", Message: tuiNastyLine, Err: tuiNastyLine},
	}, {
		name:  "error",
		event: events.Error{Meta: meta, Source: "cartridge", Message: tuiNastyLine, Err: tuiNastyLine},
	}, {
		name: "implementation report summary",
		event: events.ResponseReceived{Meta: meta, Bead: "b-1", Round: 1, Agent: "av-1",
			Report: &domain.ImplReport{Summary: tuiNastyLine}},
	}, {
		name:  "bead title",
		event: events.BeadCreated{Meta: meta, Bead: domain.Bead{ID: "b-1", Title: tuiNastyLine}},
	}, {
		name:  "run finished summary",
		event: events.RunFinished{Meta: meta, Summary: tuiNastyLine},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line := tui.FormatEvent(tc.event)
			tuiRequireSafeLine(t, line, tc.name)
			require.NotContains(t, line, "\x1b[2J", "the clear-screen survived")
			require.Contains(t, line, "SECOND LINE", "the text after the newline is kept, on the same row")
		})
	}
}

func TestFormatEventLeavesPrintableTextAlone(t *testing.T) {
	t.Parallel()
	line := tui.FormatEvent(events.AgentOutput{
		Meta: events.NewMeta("r"), Agent: "av-1", Stream: events.StreamStdout,
		Line: "step 12: editing internal/http/client.go — 100% done",
	})
	require.Equal(t, "av-1 stdout: step 12: editing internal/http/client.go — 100% done", line)
}

func TestModelLogTabDrawsNoEscapeFromAnAgent(t *testing.T) {
	t.Parallel()
	const w, h = 120, 24
	m := tuiResize(tuiNewModel(t, nil), w, h)
	m = tuiUpdate(m, tui.EventMsg{Event: events.AgentOutput{
		Meta: events.NewMeta("r"), Agent: "av-1", Stream: events.StreamStdout, Line: tuiNastyLine,
	}})
	m = tuiUpdate(m, tui.EventMsg{Event: events.Warning{
		Meta: events.NewMeta("r"), Source: "cartridge", Message: tuiNastyLine,
	}})
	m = tuiUpdate(m, tuiKey("5"))
	require.Equal(t, tui.TabLog, m.Tab())

	frame := m.View()
	require.NotContains(t, frame, "\x1b[2J", "the log tab drew a clear-screen from an agent")
	require.NotContains(t, frame, "\x1b[31m", "the log tab drew an agent's colour")
	require.NotContains(t, frame, "\x1b]0;title", "the log tab drew an OSC string")
	lines := strings.Split(frame, "\n")
	require.Len(t, lines, h, "an embedded newline took an extra row")
	for i, l := range lines {
		require.LessOrEqual(t, lipgloss.Width(l), w, "line %d is too wide: %q", i, l)
	}

	rows := 0
	for _, l := range lines {
		if strings.Contains(l, "SECOND LINE") {
			rows++
			require.Contains(t, l, "boom", "the line was split over two rows")
			require.True(t, strings.HasPrefix(l, "▌ ") || strings.HasPrefix(l, "  "),
				"a log row lost its cursor gutter: %q", l)
		}
	}
	require.Equal(t, 2, rows, "one row for the agent line and one for the warning")
}

func TestStateCapsNoticesLikeTheLog(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	for i := range 5000 {
		s.Apply(events.Warning{Meta: events.NewMeta("r"), Message: fmt.Sprintf("w%d", i)})
	}
	require.Len(t, s.Log, tui.DefaultLogLines)
	require.Len(t, s.Notices, tui.DefaultLogLines, "notices grow without bound")
	require.Equal(t, "w4999", s.Notices[len(s.Notices)-1].Message, "the newest notice is kept")
	require.Equal(t, "w4500", s.Notices[0].Message, "the oldest notices are dropped")

	small := tui.NewState(domain.Run{ID: "r"})
	small.MaxLog = 3
	for i := range 10 {
		small.Apply(events.Error{Meta: events.NewMeta("r"), Message: fmt.Sprintf("e%d", i)})
	}
	require.Len(t, small.Notices, 3)
	require.Equal(t, "e7", small.Notices[0].Message)
}

func TestStateSanitizesNoticeText(t *testing.T) {
	t.Parallel()
	s := tui.NewState(domain.Run{ID: "r"})
	s.Apply(events.Error{Meta: events.NewMeta("r"), Source: "cartridge", Message: tuiNastyLine, Err: tuiNastyLine})
	require.Len(t, s.Notices, 1)
	tuiRequireSafeLine(t, s.Notices[0].Message, "notice message")
	tuiRequireSafeLine(t, s.Notices[0].Err, "notice error")
	require.Contains(t, s.Notices[0].Message, "SECOND LINE")
}

func TestModelHelpOverlayShowsTheActiveViewsKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tab  tui.Tab
		key  string
		want []string
	}{
		{tui.TabGraph, "1", []string{"prev level", "next level", "bead detail", "first bead", "last bead"}},
		{tui.TabAgents, "2", []string{"transcript", "follow", "page up", "newest"}},
		{tui.TabFeedback, "3", []string{"details", "scroll up", "scroll down", "first round", "last round"}},
		{tui.TabPRs, "4", []string{"open PR"}},
		{tui.TabLog, "5", nil},
	} {
		t.Run(tc.tab.String(), func(t *testing.T) {
			t.Parallel()
			m := tuiFeed(tuiResize(tuiNewModel(t, nil), 120, 40))
			m = tuiUpdate(tuiUpdate(m, tuiKey(tc.key)), tuiKey("?"))
			require.Equal(t, tc.tab, m.Tab())
			require.True(t, m.HelpVisible())
			view := m.View()
			require.Contains(t, view, "next view", "the global keys stay in the overlay")
			if len(tc.want) == 0 {
				require.NotContains(t, view, tc.tab.String()+" keys", "the log view adds no keys of its own")
				return
			}
			require.Contains(t, view, tc.tab.String()+" keys", "the overlay does not name the view's keys")
			for _, want := range tc.want {
				require.Contains(t, view, want, "the %s overlay omits %q", tc.tab, want)
			}
		})
	}
}

// --- P7-13, P7-19, P7-21: what the footer is for ------------------------

// tuiFooterOf returns the footer line of a frame, without escapes.
func tuiFooterOf(m tui.Model) string {
	lines := strings.Split(gfPlain(m.View()), "\n")
	return strings.TrimRight(lines[len(lines)-1], " ")
}

// TestModelFooterNamesTheActiveViewsKeys pins P7-13. Each view carries its
// own KeyMap, and until now the footer showed only the global bindings: `o`
// (open the pull request), `enter` (the bead detail) and the transcript keys
// were on the `?` overlay and nowhere else, so nothing on screen said they
// existed. The footer now ends with the active view's own ShortHelp, at 80
// columns as well as at 120 — which is what the composed goldens pin.
func TestModelFooterNamesTheActiveViewsKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tab  tui.Tab
		key  string
		want string
	}{
		{tui.TabGraph, "1", "bead detail"},
		{tui.TabAgents, "2", "transcript"},
		{tui.TabFeedback, "3", "details"},
		{tui.TabPRs, "4", "open PR"},
	} {
		t.Run(tc.tab.String(), func(t *testing.T) {
			t.Parallel()
			for _, w := range []int{80, 120, 200} {
				m := tuiUpdate(tuiFeed(tuiResize(tuiNewModel(t, nil), w, 24)), tuiKey(tc.key))
				require.Equal(t, tc.tab, m.Tab())
				require.Containsf(t, tuiFooterOf(m), tc.want,
					"the %d-column footer does not name the %s view's own keys", w, tc.tab)
				require.Contains(t, tuiFooterOf(m), "? help", "the help key is the one binding that never goes")
			}
		})
	}

	// The log view is drawn by the model itself and adds no keys, so its
	// footer keeps the global map alone.
	m := tuiUpdate(tuiFeed(tuiResize(tuiNewModel(t, nil), 80, 24)), tuiKey("5"))
	require.Equal(t, tui.TabLog, m.Tab())
	require.Contains(t, tuiFooterOf(m), "next view")
}

// TestModelFooterShowsTheRunsNewestNotice pins P7-19: a warning published
// while the Graph tab is up is visible without switching to the Log tab, and
// stays visible — the flash that used to carry such news was cleared by the
// next key press.
func TestModelFooterShowsTheRunsNewestNotice(t *testing.T) {
	t.Parallel()

	m := tuiResize(tuiNewModel(t, nil), 100, 24)
	require.Equal(t, tui.TabGraph, m.Tab())
	require.NotContains(t, tuiFooterOf(m), "!")

	m = tuiUpdate(m, tui.EventMsg{Event: events.Warning{
		Meta: events.NewMeta(tui.DemoRunID), Source: "beads", Message: "bd update refused the round",
	}})
	require.Contains(t, tuiFooterOf(m), "! bd update refused the round")

	// It outlives a key press, unlike a flash.
	m = tuiUpdate(m, tuiKey("j"))
	require.Contains(t, tuiFooterOf(m), "! bd update refused the round")

	// A second notice replaces it and says how many are behind it.
	m = tuiUpdate(m, tui.EventMsg{Event: events.Error{
		Meta: events.NewMeta(tui.DemoRunID), Source: "supervisor", Message: "the engineer timed out",
	}})
	footer := tuiFooterOf(m)
	require.Contains(t, footer, "! the engineer timed out")
	require.Contains(t, footer, "(+1 more)")

	// On a terminal with no room for the sentence, the count still says
	// there is something to read on the Log tab.
	narrow := tuiFooterOf(tuiResize(m, 64, 24))
	require.Contains(t, narrow, "! 2 notices")
	require.NotContains(t, narrow, "the engineer timed out")
}

// TestModelFooterGivesTheFlashItsRoom pins P7-21. The short keymap is 76
// columns, so an 80-column footer used to clip a control's answer after a
// character or two: the reader saw "m" where the run had said "run paused".
// The flash is composed first now and the keymap takes what is left.
func TestModelFooterGivesTheFlashItsRoom(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	for _, w := range []int{80, 120} {
		m := gfSend(gfFeed(tui.New(t.Context(), tui.Options{
			Run: tui.DemoRun(gfDemo), NoColor: true, Theme: tui.ThemeFor(tui.ProfileNoColor),
			NoAnim: true, Now: gfClock, Controller: c,
		}), cwLiveEvents()), tea.WindowSizeMsg{Width: w, Height: 24})

		paused := cwPress(t, m, "p")
		require.Containsf(t, tuiFooterOf(paused), "run paused",
			"the %d-column footer clipped the control's answer", w)

		safety := cwPress(t, m, "S", "y")
		require.Containsf(t, tuiFooterOf(safety), "safety on for future deliveries",
			"the %d-column footer clipped the control's answer", w)
		require.LessOrEqual(t, lipgloss.Width(tuiFooterOf(safety)), w)
	}

	// The longest answer of all — the re-fire refusal — is 90 columns and
	// cannot be read in full at 80 however the line is arranged. It takes
	// the whole footer there and is clipped from the right, so the reader
	// still gets the start of the sentence rather than none of it.
	m := gfSend(gfFeed(tui.New(t.Context(), tui.Options{
		Run: tui.DemoRun(gfDemo), NoColor: true, Theme: tui.ThemeFor(tui.ProfileNoColor),
		NoAnim: true, Now: gfClock, Controller: c,
	}), cwLiveEvents()), tea.WindowSizeMsg{Width: 80, Height: 24})
	refused := tuiFooterOf(cwPress(t, m, "g", "r"))
	require.Regexp(t, `^demo-\d is \S+ — only a failed, needs_human`, refused)
	require.NotContains(t, refused, "? help", "the refusal takes the whole line")
	require.LessOrEqual(t, lipgloss.Width(refused), 80)
}

// tuiFooterGolden is the 80-column frame P7-21 asks for: a control's answer
// read in full on the terminal size every other composed frame is pinned at.
// It is registered in golden_test.go's gfViewGoldens like every other
// per-view golden.
const tuiFooterGolden = "footer-control-80"

// TestModelFooterControlGoldenFrame pins that frame.
func TestModelFooterControlGoldenFrame(t *testing.T) {
	t.Parallel()

	build := func() tui.Model {
		m := gfSend(gfFeed(tui.New(t.Context(), tui.Options{
			Run: tui.DemoRun(gfDemo), NoColor: true, Theme: tui.ThemeFor(tui.ProfileNoColor),
			NoAnim: true, Now: gfClock, Controller: &cwController{},
		}), cwLiveEvents()), tea.WindowSizeMsg{Width: gfWidth, Height: gfHeight})
		return cwPress(t, m, "S", "y")
	}
	frame := build().View()
	gfRequireGeometry(t, frame, gfWidth, gfHeight)
	require.Contains(t, gfPlain(frame), "safety on for future deliveries")
	require.Equal(t, frame, build().View(), "the frame is not reproducible")
	gfCompare(t, filepath.Join(gfGoldenDir, tuiFooterGolden+".golden"), frame)
}

// --- P7-12: replaying a recorded run ------------------------------------

// TestReplayDrivesAFrameFromARecordedRun pins P7-12: a run log written by
// `sc run` (or by `sc tui-demo --events`) is read back into the same frames
// the scripted demo draws, so the goldens and P6-08 can be driven from what
// the orchestrator really published rather than only from a hand-written
// script.
func TestReplayDrivesAFrameFromARecordedRun(t *testing.T) {
	t.Parallel()

	// The recording: the demo run written out as JSONL, exactly as
	// `sc tui-demo --events` and the run log write it.
	var buf strings.Builder
	w := events.NewJSONLWriter(&buf)
	for _, ev := range tui.DemoEvents(gfDemo) {
		require.NoError(t, w.Write(ev))
	}
	require.NoError(t, w.Err())
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(buf.String()), 0o600))

	script, err := tui.ReplayFile(path, -1)
	require.NoError(t, err)
	require.Len(t, script, len(tui.DemoEvents(gfDemo)))
	for i, s := range script {
		require.Zerof(t, s.Delay, "event %d of an unpaced replay waits", i)
	}

	// The run the header is drawn from comes out of the recording itself.
	run := tui.ReplayRun(script)
	require.Equal(t, tui.DemoRun(gfDemo).ID, run.ID)
	require.Equal(t, tui.DemoRun(gfDemo).Config.Ammo, run.Config.Ammo)

	// And the frame is the frame the same events drew when the script was
	// played from memory: the graph golden of P7-09.
	m := gfFeed(gfSend(tui.New(t.Context(), tui.Options{
		Run: run, NoColor: true, Theme: tui.ThemeFor(tui.ProfileNoColor), NoAnim: true, Now: gfClock,
	}), tea.WindowSizeMsg{Width: gfWidth, Height: gfHeight}), tui.ReplayEvents(script))
	require.Equal(t, gfLoad(t, "graph"), m.View(),
		"the recorded run does not draw the frame the scripted one does")

	// Pacing is honoured, and every event but the first waits for it.
	paced, err := tui.ReplayFile(path, 5*time.Millisecond)
	require.NoError(t, err)
	require.Zero(t, paced[0].Delay)
	require.Equal(t, 5*time.Millisecond, paced[1].Delay)
}

// TestReplayRejectsWhatItCannotRead covers the two ways a recording is not
// a recording, both of which `sc tui-demo --replay` reports before it claims
// the terminal.
func TestReplayRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	_, err := tui.ReplayFile(filepath.Join(t.TempDir(), "missing.jsonl"), 0)
	require.ErrorContains(t, err, "opening the recorded run")

	_, err = tui.ReplayScript(strings.NewReader(""), 0)
	require.ErrorContains(t, err, "no events")

	_, err = tui.ReplayScript(strings.NewReader("{not json\n"), 0)
	require.ErrorContains(t, err, "reading the recorded run")
}

// TestReplayRendersARecordedRunInTheProgram drives the real Bubble Tea
// program over a recording, which is what `sc tui-demo --replay` does: the
// frames the user sees come out of the file, and `q` leaves.
func TestReplayRendersARecordedRunInTheProgram(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	w := events.NewJSONLWriter(&buf)
	for _, ev := range tui.DemoEvents(gfDemo) {
		require.NoError(t, w.Write(ev))
	}
	require.NoError(t, w.Err())
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(buf.String()), 0o600))
	script, err := tui.ReplayFile(path, -1)
	require.NoError(t, err)

	var out tuiSyncBuffer
	in, inWriter := io.Pipe()
	t.Cleanup(func() { _ = inWriter.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tui.Replay(ctx, script, tui.Options{
			NoColor: true, Theme: tui.ThemeFor(tui.ProfileNoColor), NoAnim: true,
			Input: in, Output: &out,
		})
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(gfPlain(out.String()), string(tui.DemoRunID))
	}, 10*time.Second, 10*time.Millisecond, "the program never drew the recorded run:\n%s", out.String())
	cancel()
	require.NoError(t, <-done)
}
