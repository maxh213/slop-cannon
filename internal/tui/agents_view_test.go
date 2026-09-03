package tui_test

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
	"slop-cannon/internal/tui"
)

// avGoldenDir is where the Agents view's golden frames live, relative to
// this package.
const avGoldenDir = "../../testdata/tui"

// avRun is the run the frames are rendered against.
const avRun domain.RunID = "av-20260101-090000"

// avStart is the fixed simulated start of the scripted agents, and avNow is
// the clock the frames are rendered at, so elapsed times never drift.
var (
	avStart = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	avNow   = avStart.Add(3 * time.Minute)
)

// avGolden compares got with testdata/tui/agents-<name>.golden. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func avGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(avGoldenDir, "agents-"+name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "frame differs from %s; run `make update-golden` if the change is intended", path)
}

// avAgent builds the [domain.Agent] of one scripted agent.
func avAgent(id domain.AgentID, role domain.Role, lens domain.Lens, bead domain.BeadID, at time.Time) domain.Agent {
	return domain.Agent{
		ID: id, Role: role, Lens: lens, Cartridge: "claude", Model: "fable",
		Bead: bead, State: domain.AgentStateRunning, StartedAt: at,
	}
}

// avScript is the event stream the frames are rendered from: a product
// owner that planned and exited, an engineer still working with a long
// transcript, a tester still reviewing, and a tester that timed out.
func avScript() []events.Event {
	at := avStart
	meta := func() events.Meta {
		m := events.MetaAt(avRun, at)
		at = at.Add(5 * time.Second)
		return m
	}
	const (
		po   domain.AgentID = "po-1"
		eng  domain.AgentID = "eng-av-2-r1"
		qa   domain.AgentID = "tester-qa-av-2-r1"
		ux   domain.AgentID = "tester-ux-av-2-r1"
		bead domain.BeadID  = "av-2"
	)
	run := domain.Run{
		ID: avRun, RepoPath: "/tmp/av-repo", Mode: domain.RunModeAutopilot, StartedAt: avStart,
		Config: domain.ConfigSnapshot{Ammo: 6, Cartridge: "claude", Model: "fable"},
	}
	out := []events.Event{
		events.RunStarted{Meta: meta(), Info: run},
		events.AgentSpawned{Meta: meta(), Agent: avAgent(po, domain.RolePO, "", "", avStart), Task: events.TaskPlan, Attempt: 1},
		events.AgentOutput{Meta: meta(), Agent: po, Stream: events.StreamStdout, Line: "reading the repository…"},
		events.AgentFinished{Meta: meta(), Agent: po, Role: domain.RolePO, Duration: 42 * time.Second, Produced: domain.MessagePlan},
		events.AgentSpawned{
			Meta:  meta(),
			Agent: avAgent(eng, domain.RoleEngineer, "", bead, avStart.Add(time.Minute)),
			Task:  events.TaskImplement, BeadTitle: "Retry failed HTTP requests", Round: 1, Attempt: 1,
		},
	}
	for i := 1; i <= 12; i++ {
		out = append(out, events.AgentOutput{
			Meta: meta(), Agent: eng, Stream: events.StreamStdout,
			Line: fmt.Sprintf("step %02d: editing internal/http/client.go", i),
		})
	}
	out = append(out,
		events.AgentOutput{Meta: meta(), Agent: eng, Stream: events.StreamStderr, Line: "warning: no test command configured"},
		events.AgentSpawned{
			Meta:  meta(),
			Agent: avAgent(qa, domain.RoleTester, domain.LensQA, bead, avStart.Add(90*time.Second)),
			Task:  events.TaskReview, BeadTitle: "Retry failed HTTP requests", Round: 1, Attempt: 1,
		},
		events.AgentOutput{Meta: meta(), Agent: qa, Stream: events.StreamStdout, Line: "running go test ./..."},
		events.AgentSpawned{
			Meta:  meta(),
			Agent: avAgent(ux, domain.RoleTester, domain.LensUX, bead, avStart.Add(100*time.Second)),
			Task:  events.TaskReview, BeadTitle: "Retry failed HTTP requests", Round: 1, Attempt: 2,
		},
		events.AgentFailed{
			Meta: meta(), Agent: ux, Role: domain.RoleTester, Bead: bead,
			Failure: events.FailureTimeout, Err: "agent timeout after 15m",
			Duration: 15 * time.Minute, ExitCode: -1, Attempt: 2,
		},
	)
	return out
}

// avFold applies evs to a fresh state and a fresh view and returns both,
// sized to w×h with the fixed clock.
func avFold(t *testing.T, w, h int, evs []events.Event) (*tui.State, tui.AgentsModel) {
	t.Helper()
	st := tui.NewState(domain.Run{ID: avRun})
	v := tui.NewAgentsModel(tui.AgentsOptions{NoColor: true, Now: func() time.Time { return avNow }}).Resize(w, h)
	for _, ev := range evs {
		st.Apply(ev)
		v = v.Apply(ev)
	}
	return st, v
}

// avView folds the whole script and renders the card list.
func avView(t *testing.T, w, h int) (*tui.State, tui.AgentsModel) {
	t.Helper()
	return avFold(t, w, h, avScript())
}

// avKey builds a key press from its printable spelling.
func avKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// avPress sends a key and asserts the view consumed it.
func avPress(t *testing.T, v tui.AgentsModel, st *tui.State, key string) tui.AgentsModel {
	t.Helper()
	next, handled := v.Update(avKey(key), st, tui.DefaultKeyMap())
	require.True(t, handled, "the Agents view ignored %q", key)
	return next
}

// avLines splits a rendered frame into its lines.
func avLines(frame string) []string { return strings.Split(frame, "\n") }

// avBody returns the transcript lines of a frame: everything under the
// selected agent's card and the pane's rule.
func avBody(t *testing.T, frame string) string {
	t.Helper()
	lines := avLines(frame)
	for i, l := range lines {
		if strings.HasPrefix(l, "TRANSCRIPT") {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	t.Fatalf("the frame has no transcript pane:\n%s", frame)
	return ""
}

func TestAgentsViewEmptyGolden(t *testing.T) {
	st, v := avFold(t, 80, 20, nil)
	frame := v.View(st)
	require.Contains(t, frame, "no agents fired yet")
	avGolden(t, "empty", frame)
}

func TestAgentsViewLiveGolden(t *testing.T) {
	st, v := avView(t, 80, 20)
	avGolden(t, "live", v.View(st))
}

func TestAgentsViewWideGolden(t *testing.T) {
	st, v := avView(t, 120, 14)
	avGolden(t, "wide", v.View(st))
}

func TestAgentsViewNarrowGolden(t *testing.T) {
	st, v := avView(t, 44, 12)
	frame := v.View(st)
	// A narrow pane drops the optional fields instead of truncating them.
	require.NotContains(t, frame, "claude/fable")
	require.NotContains(t, frame, "implement r1")
	require.NotContains(t, frame, "agent timeout after 15m")
	require.Contains(t, frame, "⚙ engineer · av-2")
	avGolden(t, "narrow", frame)
}

func TestAgentsViewHistoryGolden(t *testing.T) {
	// Every agent has ended: the live section disappears and the whole pane
	// is the history list, newest first.
	evs := avScript()
	at := avStart.Add(10 * time.Minute)
	evs = append(evs,
		events.AgentFinished{Meta: events.MetaAt(avRun, at), Agent: "eng-av-2-r1", Role: domain.RoleEngineer, Bead: "av-2", Duration: 96 * time.Second, Produced: domain.MessageImplReport},
		events.AgentFinished{Meta: events.MetaAt(avRun, at.Add(time.Second)), Agent: "tester-qa-av-2-r1", Role: domain.RoleTester, Bead: "av-2", Duration: 58 * time.Second, Produced: domain.MessageVerdict},
	)
	st, v := avFold(t, 80, 20, evs)
	frame := v.View(st)
	require.NotContains(t, frame, "LIVE")
	require.Contains(t, frame, "HISTORY · 4")
	avGolden(t, "history", frame)
}

func TestAgentsViewTranscriptGolden(t *testing.T) {
	st, v := avView(t, 80, 20)
	v = avPress(t, v, st, "enter")
	require.True(t, v.ShowingTranscript())
	require.True(t, v.Following())
	avGolden(t, "transcript", v.View(st))
}

func TestAgentsViewTranscriptScrolledGolden(t *testing.T) {
	st, v := avView(t, 80, 12)
	v = avPress(t, v, st, "enter")
	v = avPress(t, v, st, "home")
	require.False(t, v.Following())
	require.Equal(t, 0, v.Offset(st))
	avGolden(t, "transcript-scrolled", v.View(st))
}

func TestAgentsViewFrameGeometry(t *testing.T) {
	sizes := [][2]int{{80, 20}, {120, 14}, {44, 12}, {20, 5}}
	for _, size := range sizes {
		w, h := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
			st, v := avView(t, w, h)
			gotW, gotH := v.Size()
			require.Equal(t, [2]int{w, h}, [2]int{gotW, gotH})
			for _, frame := range []string{v.View(st), avPress(t, v, st, "enter").View(st)} {
				lines := avLines(frame)
				require.Len(t, lines, h, "frame is not %d lines", h)
				for i, l := range lines {
					require.LessOrEqual(t, lipgloss.Width(l), w, "line %d is wider than %d columns: %q", i, w, l)
				}
			}
		})
	}
}

func TestAgentsViewOrderIsLiveThenNewestHistory(t *testing.T) {
	st, v := avView(t, 80, 20)
	var got []domain.AgentID
	for i := 0; i < len(st.Agents); i++ {
		a, ok := v.Selected(st)
		require.True(t, ok)
		got = append(got, a.ID)
		v = avPress(t, v, st, "down")
	}
	require.Equal(t, []domain.AgentID{
		"eng-av-2-r1",       // live, spawn order
		"tester-qa-av-2-r1", // live
		"tester-ux-av-2-r1", // ended last
		"po-1",              // ended first
	}, got)
}

func TestAgentsViewCursorClamps(t *testing.T) {
	st, v := avView(t, 80, 20)
	v = avPress(t, v, st, "up")
	require.Equal(t, 0, v.Cursor())
	for range len(st.Agents) + 3 {
		v = avPress(t, v, st, "down")
	}
	require.Equal(t, len(st.Agents)-1, v.Cursor())
	a, ok := v.Selected(st)
	require.True(t, ok)
	require.Equal(t, domain.AgentID("po-1"), a.ID)
}

func TestAgentsViewSelectedCardIsAlwaysVisible(t *testing.T) {
	// A short pane cannot show every card, so the window has to follow the
	// cursor down into the history list.
	st, v := avView(t, 80, 8)
	for range len(st.Agents) {
		v = avPress(t, v, st, "down")
	}
	frame := v.View(st)
	require.Contains(t, frame, "✔ po", "the selected history row is scrolled into view")
	require.Contains(t, frame, "▌ ", "and it carries the cursor gutter")
}

func TestAgentsViewTranscriptFollowsTheStream(t *testing.T) {
	st, v := avView(t, 80, 20)
	v = avPress(t, v, st, "enter")
	require.True(t, v.Following())
	require.Contains(t, avBody(t, v.View(st)), "warning: no test command configured")

	ev := events.AgentOutput{Meta: events.MetaAt(avRun, avNow), Agent: "eng-av-2-r1", Stream: events.StreamStdout, Line: "the newest line"}
	st.Apply(ev)
	v = v.Apply(ev)
	frame := v.View(st)
	require.Contains(t, frame, "the newest line")
	require.Equal(t, "the newest line", strings.TrimSpace(avLines(frame)[len(avLines(frame))-1]))
}

func TestAgentsViewScrollingLeavesAndRejoinsFollowMode(t *testing.T) {
	st, v := avView(t, 80, 12)
	v = avPress(t, v, st, "enter")
	bottom := v.Offset(st)
	require.Positive(t, bottom, "the engineer's transcript should not fit in the pane")

	v = avPress(t, v, st, "up")
	require.False(t, v.Following())
	require.Equal(t, bottom-1, v.Offset(st))
	require.NotContains(t, avBody(t, v.View(st)), "warning: no test command configured")

	// A line arriving while scrolled away must not move the viewport.
	ev := events.AgentOutput{Meta: events.MetaAt(avRun, avNow), Agent: "eng-av-2-r1", Stream: events.StreamStdout, Line: "arrived while scrolled"}
	st.Apply(ev)
	v = v.Apply(ev)
	require.Equal(t, bottom-1, v.Offset(st))
	require.NotContains(t, avBody(t, v.View(st)), "arrived while scrolled")

	v = avPress(t, v, st, "down")
	require.False(t, v.Following(), "one line down is still one line short of the newest")
	v = avPress(t, v, st, "down")
	require.True(t, v.Following(), "reaching the newest line re-enters follow mode")
	require.Contains(t, avBody(t, v.View(st)), "arrived while scrolled")
}

func TestAgentsViewTranscriptScrollKeys(t *testing.T) {
	st, v := avView(t, 80, 12)
	v = avPress(t, v, st, "enter")
	bottom := v.Offset(st)

	v = avPress(t, v, st, "home")
	require.Equal(t, 0, v.Offset(st))
	require.False(t, v.Following())
	require.Contains(t, v.View(st), "fired engineer(claude/fable) at av-2")

	v = avPress(t, v, st, "pgup")
	require.Equal(t, 0, v.Offset(st), "paging up at the top stays at the top")

	v = avPress(t, v, st, "pgdown")
	v = avPress(t, v, st, "pgdown")
	require.Equal(t, bottom, v.Offset(st), "two pages reach the end of this transcript")
	require.True(t, v.Following())

	v = avPress(t, v, st, "up")
	require.False(t, v.Following())
	v = avPress(t, v, st, "end")
	require.True(t, v.Following())
	require.Equal(t, bottom, v.Offset(st))
}

func TestAgentsViewFollowKeyToggles(t *testing.T) {
	st, v := avView(t, 80, 12)
	v = avPress(t, v, st, "enter")
	v = avPress(t, v, st, "f")
	require.False(t, v.Following())
	require.Contains(t, v.View(st), "· SCROLLED")

	v = avPress(t, v, st, "home")
	v = avPress(t, v, st, "f")
	require.True(t, v.Following())
	require.Contains(t, v.View(st), "· FOLLOWING")
}

func TestAgentsViewEscapeClosesTheTranscript(t *testing.T) {
	st, v := avView(t, 80, 20)
	v = avPress(t, v, st, "enter")
	require.True(t, v.ShowingTranscript())
	v = avPress(t, v, st, "esc")
	require.False(t, v.ShowingTranscript())
	require.Contains(t, v.View(st), "LIVE · AMMO 2/6")
}

func TestAgentsViewTranscriptOfAnAgentWithoutOutput(t *testing.T) {
	evs := []events.Event{
		events.AgentSpawned{
			Meta:  events.MetaAt(avRun, avStart),
			Agent: avAgent("eng-quiet", domain.RoleEngineer, "", "av-9", avStart),
			Task:  events.TaskImplement, Attempt: 1,
		},
	}
	st, _ := avFold(t, 60, 10, evs)
	// The state knows the agent, but this view never saw its spawn note, so
	// the transcript is empty.
	v := tui.NewAgentsModel(tui.AgentsOptions{NoColor: true, Now: func() time.Time { return avNow }}).Resize(60, 10)
	v = avPress(t, v, st, "enter")
	frame := v.View(st)
	require.Contains(t, frame, "0 LINES")
	require.Contains(t, frame, "no output yet")
}

func TestAgentsViewOpeningWithNoAgentsDoesNothing(t *testing.T) {
	st, v := avFold(t, 80, 20, nil)
	v = avPress(t, v, st, "enter")
	require.False(t, v.ShowingTranscript())
	require.Equal(t, "no agents fired yet", strings.TrimSpace(avLines(v.View(st))[0]))
}

func TestAgentsViewTranscriptKeepsOnlyTheNewestLines(t *testing.T) {
	v := tui.NewAgentsModel(tui.AgentsOptions{NoColor: true, MaxLines: 3, Now: func() time.Time { return avNow }})
	for i := range 6 {
		v = v.Apply(events.AgentOutput{
			Meta: events.MetaAt(avRun, avStart), Agent: "eng-1",
			Stream: events.StreamStdout, Line: fmt.Sprintf("line %d", i),
		})
	}
	require.Equal(t, []string{"line 3", "line 4", "line 5"}, v.Transcript("eng-1"))
}

func TestAgentsViewTranscriptRecordsStreamsAndNotes(t *testing.T) {
	_, v := avView(t, 80, 20)
	got := v.Transcript("po-1")
	require.Equal(t, []string{
		"fired po(claude/fable) to plan",
		"reading the repository…",
		"po-1 finished in 42s (exit 0, plan)",
	}, got)
	require.Empty(t, v.Transcript("nobody"))
}

func TestAgentsViewApplyIgnoresOtherEvents(t *testing.T) {
	v := tui.NewAgentsModel(tui.AgentsOptions{NoColor: true})
	before := v
	v = v.Apply(events.RunPaused{Meta: events.MetaAt(avRun, avStart), Reason: "cost_cap"})
	v = v.Apply(nil)
	require.Empty(t, v.Transcript("po-1"))
	require.Equal(t, before.Cursor(), v.Cursor())
}

func TestAgentsViewIgnoresNonKeyMessages(t *testing.T) {
	st, v := avView(t, 80, 20)
	next, handled := v.Update(tea.WindowSizeMsg{Width: 10, Height: 10}, st, tui.DefaultKeyMap())
	require.False(t, handled)
	require.Equal(t, v.Cursor(), next.Cursor())

	next, handled = v.Update(avKey("z"), st, tui.DefaultKeyMap())
	require.False(t, handled)
	require.Equal(t, v.Cursor(), next.Cursor())
}

func TestAgentsViewSpinnerAdvances(t *testing.T) {
	st, v := avView(t, 80, 20)
	first := v.View(st)
	for range 3 {
		v = v.Tick()
	}
	require.NotEqual(t, first, v.View(st), "the spinner frame should change")
	require.Equal(t, first, v.Tick().Tick().Tick().Tick().Tick().Tick().Tick().View(st), "ten ticks return to frame zero")
}

func TestAgentsViewDefaultsAreUsable(t *testing.T) {
	v := tui.NewAgentsModel(tui.AgentsOptions{})
	w, h := v.Size()
	require.Positive(t, w)
	require.Positive(t, h)
	require.True(t, v.Following())
	require.False(t, v.ShowingTranscript())
	require.Equal(t, "", v.View(nil)[:0])
	require.Contains(t, v.View(nil), "no agents fired yet")
}

func TestAgentsViewKeyMapOverride(t *testing.T) {
	keys := tui.DefaultAgentsKeyMap()
	require.NotEmpty(t, keys.ShortHelp())
	require.NotEmpty(t, keys.FullHelp())

	custom := keys
	custom.Open = tui.DefaultAgentsKeyMap().Bottom // "end"/"G" opens instead of enter
	st, _ := avView(t, 80, 20)
	v := tui.NewAgentsModel(tui.AgentsOptions{NoColor: true, Keys: &custom, Now: func() time.Time { return avNow }}).Resize(80, 20)
	next, handled := v.Update(avKey("enter"), st, tui.DefaultKeyMap())
	require.False(t, handled)
	require.False(t, next.ShowingTranscript())
	next = avPress(t, v, st, "end")
	require.True(t, next.ShowingTranscript())
}

func TestAgentsViewLiveElapsedUsesTheClock(t *testing.T) {
	st, v := avView(t, 80, 20)
	// The engineer started a minute into the run and the clock is three
	// minutes in, so its card shows two minutes.
	require.Contains(t, v.View(st), "2m0s")
	// A failed agent shows how long it ran, not how long ago it started.
	require.Contains(t, v.View(st), "15m0s")
}
