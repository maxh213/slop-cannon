package tui_test

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// P7-09 — whole-frame golden tests of the TUI.
//
// Every frame here is rendered from the scripted demo run ([tui.DemoEvents]),
// which carries fixed UTC timestamps, at a fixed terminal size and with
// colour off, so a frame is byte-for-byte reproducible on any machine. The
// per-view goldens (agents-*, graph-*, feedback-*, prs-*, anim-*) belong to
// the tasks that own those views; these are the composed frames: chrome,
// pane and overlays together, exactly as the user sees them.
//
// Updating: `make update-golden-tui` (or UPDATE_GOLDEN=1 go test
// ./internal/tui/...) rewrites the files; `make check-golden` is the check
// that they are current, and `make check` (what CI runs) fails on a stale
// frame like any other test failure.

// gfGoldenDir is where the golden frames live, relative to this file.
const gfGoldenDir = "../../testdata/tui"

// gfPrefix keeps the composed frames apart from the per-view goldens in the
// same directory.
const gfPrefix = "frame-"

// gfWidth and gfHeight are the deterministic terminal every frame is
// rendered in unless a scenario asks for another size.
const (
	gfWidth  = 80
	gfHeight = 24
)

// gfWaitTimeout is how long the teatest program may take to draw a frame. It
// is generous because a loaded CI box renders slowly.
const gfWaitTimeout = 10 * time.Second

// gfDemo is the scripted run every frame is built from: fixed run id, fixed
// start time, no wall-clock pacing.
var gfDemo = tui.DemoOptions{Delay: -1}

// gfBrief is the same script as a brief run with safety on, so the header
// and the delivery it renders differ from the autopilot frames.
var gfBrief = tui.DemoOptions{Delay: -1, Brief: "make the http client retry", Safety: true}

// gfScenario is one golden frame: a model built from the scripted run,
// rendered at a fixed size.
type gfScenario struct {
	// name is the golden file, without the frame- prefix and .golden suffix.
	name string
	// w and h are the terminal size; zero means gfWidth × gfHeight.
	w, h int
	// build returns the model to render.
	build func(t *testing.T) tui.Model
}

// gfScenarios is every composed frame the suite pins.
func gfScenarios() []gfScenario {
	return []gfScenario{{
		// Before a single event: the header is already populated from the
		// run, and every view is empty.
		name:  "startup",
		build: func(t *testing.T) tui.Model { return gfNew(t, gfDemo, gfWidth, gfHeight) },
	}, {
		// Mid-run: the plan landed, bead A is in review, agents are live.
		name: "midrun",
		build: func(t *testing.T) tui.Model {
			return gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindVerdictReceived))
		},
	}, {
		name:  "graph",
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, gfWidth, gfHeight) },
	}, {
		name: "agents",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("2"))
		},
	}, {
		name: "feedback",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("3"))
		},
	}, {
		name: "prs",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("4"))
		},
	}, {
		// The log view follows the stream, so it is pinned to the last line.
		name: "log",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("5"))
		},
	}, {
		// …until the cursor is moved, which stops it following.
		name: "log-scrolled",
		build: func(t *testing.T) tui.Model {
			m := gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("5"))
			for range 6 {
				m = gfSend(m, gfKey("up"))
			}
			return m
		},
	}, {
		name: "help",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("?"))
		},
	}, {
		// `q` on a live run asks first; the confirmation replaces the pane.
		name: "confirm",
		build: func(t *testing.T) tui.Model {
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindAgentSpawned))
			return gfSend(m, gfKey("q"))
		},
	}, {
		// A subscription that fell behind is reported in the footer.
		name: "dropped",
		build: func(t *testing.T) tui.Model {
			evs := tui.DemoEvents(gfDemo)
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), evs[:len(evs)-1])
			return gfSend(m, tui.EventMsg{Event: evs[len(evs)-1], Dropped: 7})
		},
	}, {
		// A stream that ended before the run did says so in the header.
		name: "disconnected",
		build: func(t *testing.T) tui.Model {
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindRunStopped))
			return gfSend(m, tui.StreamClosedMsg{})
		},
	}, {
		// A brief run with safety on, on the deliveries view.
		name: "brief-prs",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfBrief, gfWidth, gfHeight), gfKey("4"))
		},
	}, {
		name: "wide",
		w:    120, h: 40,
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, 120, 40) },
	}, {
		name: "narrow",
		w:    60, h: 14,
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, 60, 14), gfKey("2"))
		},
	}, {
		// The smallest terminal the model still draws chrome in.
		name: "tiny",
		w:    24, h: 5,
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, 24, 5) },
	}}
}

// TestGoldenFrames is the P7-09 acceptance test: every composed frame of the
// scripted run, at a fixed size, compared with its golden file.
func TestGoldenFrames(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			w, h := sc.size()
			frame := sc.build(t).View()
			gfRequireGeometry(t, frame, w, h)
			gfGolden(t, sc.name, frame)
		})
	}
}

// TestGoldenFramesAreDeterministic renders every frame twice from two
// independent models and requires the two to be identical: a wall clock, a
// map iteration or a random seed leaking into a view would show up here
// rather than as a golden that only fails on someone else's machine.
func TestGoldenFramesAreDeterministic(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			first := sc.build(t).View()
			second := sc.build(t).View()
			require.Equal(t, first, second, "frame %q is not reproducible", sc.name)
		})
	}
}

// gfColorEscape matches an SGR sequence that sets a colour: 30–37, 40–47,
// 90–97, 100–107, and the 38/48 extended forms.
var gfColorEscape = regexp.MustCompile(`\x1b\[[0-9;]*(?:3[0-79]|4[0-79]|9[0-7]|10[0-7])[0-9;]*m`)

// TestGoldenFramesCarryNoEscapes proves the frames the goldens hold carry no
// colour: with [tui.Options.NoColor] set nothing may emit a colour escape, so
// a golden reads the same on a truecolor terminal, under NO_COLOR, and in a
// CI log. Bold survives, deliberately: it is how the cursor and the selected
// box stay visible when colour is gone (P7-07).
func TestGoldenFramesCarryNoEscapes(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			frame := sc.build(t).View()
			require.Empty(t, gfColorEscape.FindAllString(frame, -1),
				"frame %q was rendered with colour escapes", sc.name)
			for _, esc := range gfAnyEscape.FindAllString(frame, -1) {
				require.Contains(t, []string{"\x1b[1m", "\x1b[0m"}, esc,
					"frame %q carries an escape that is neither bold nor reset", sc.name)
			}
		})
	}
}

// gfAnyEscape matches any ANSI escape sequence.
var gfAnyEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// gfPlain strips every ANSI escape, so a comparison sees the characters the
// user reads rather than how the renderer chose to group its SGR runs.
func gfPlain(s string) string { return gfAnyEscape.ReplaceAllString(s, "") }

// TestGoldenFrameFilesAreReadable guards the golden files themselves: a
// frame is text, never empty, and never carries a carriage return that would
// make the comparison platform-dependent.
func TestGoldenFrameFilesAreReadable(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join(gfGoldenDir, gfPrefix+"*.golden"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "no composed frames in %s; run `make update-golden-tui`", gfGoldenDir)

	want := make(map[string]bool, len(gfScenarios()))
	for _, sc := range gfScenarios() {
		want[filepath.Join(gfGoldenDir, gfPrefix+sc.name+".golden")] = true
	}
	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a test-owned path
		require.NoError(t, err)
		require.NotEmpty(t, body, "%s is empty", path)
		require.True(t, utf8.Valid(body), "%s is not valid UTF-8", path)
		require.NotContains(t, string(body), "\r", "%s has a carriage return", path)
		require.True(t, want[path], "%s is not produced by any scenario; delete it or add the scenario back", path)
	}
	require.Len(t, matches, len(want), "every scenario needs a golden; run `make update-golden-tui`")
}

// TestGoldenFramesMatchTheProgram runs the real Bubble Tea program over the
// same scripted run and requires it to draw the frame the goldens hold: the
// goldens describe what the user sees, not just what Model.View returns in a
// test.
func TestGoldenFramesMatchTheProgram(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	m := tui.New(t.Context(), tui.Options{
		Bus:     bus,
		Run:     tui.DemoRun(gfDemo),
		NoColor: true,
		NoAnim:  true,
		Now:     gfClock,
	})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(gfWidth, gfHeight))

	evs := tui.DemoEvents(gfDemo)
	for _, ev := range evs {
		bus.Publish(ev)
	}

	// The graph view is the one the program opens on, so its golden is the
	// frame the program has to end up drawing.
	want := gfLoad(t, "graph")
	out := gfWaitFor(t, tm, gfMeaningfulLines(want)...)

	tm.Send(gfKey("q"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(gfWaitTimeout))
	final, ok := tm.FinalModel(t).(tui.Model)
	require.True(t, ok, "the final model is not a tui.Model")
	require.Equal(t, tui.TabGraph, final.Tab())
	require.Equal(t, len(evs), final.State().Events, "the program missed events; drawn so far:\n%s", out)
	require.Zero(t, final.State().Dropped)
	require.False(t, final.State().Live, "the scripted run finishes, so `q` must not have asked")
}

// TestGoldenHelperRewritesTheFileWhenAsked covers the update path itself, in
// a temporary directory: with UPDATE_GOLDEN set the helper writes what it
// was given, and the file it wrote then compares equal.
func TestGoldenHelperRewritesTheFileWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame-example.golden")

	t.Setenv("UPDATE_GOLDEN", "1")
	gfCompare(t, path, "first\nsecond\n")
	body, err := os.ReadFile(path) //nolint:gosec // a temporary test path
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", string(body))

	t.Setenv("UPDATE_GOLDEN", "")
	gfCompare(t, path, "first\nsecond\n")
}

// size returns the terminal the scenario renders in.
func (sc gfScenario) size() (int, int) {
	w, h := sc.w, sc.h
	if w <= 0 {
		w = gfWidth
	}
	if h <= 0 {
		h = gfHeight
	}
	return w, h
}

// gfNew builds a model with no event stream, sized to w×h, with colour and
// animation off so the frame is reproducible.
func gfNew(t *testing.T, demo tui.DemoOptions, w, h int) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:     tui.DemoRun(demo),
		NoColor: true,
		NoAnim:  true,
		Now:     gfClock,
	})
	return gfSend(m, tea.WindowSizeMsg{Width: w, Height: h})
}

// gfClock is the fixed clock the frames are rendered against, so an elapsed
// time in a live agent's card cannot make a frame depend on when it ran.
func gfClock() time.Time { return tui.DemoStart.Add(20 * time.Minute) }

// gfWhole builds a model that has seen the whole scripted run.
func gfWhole(t *testing.T, demo tui.DemoOptions, w, h int) tui.Model {
	t.Helper()
	return gfFeed(gfNew(t, demo, w, h), tui.DemoEvents(demo))
}

// gfFeed applies every event as the update loop would.
func gfFeed(m tui.Model, evs []events.Event) tui.Model {
	for _, ev := range evs {
		m = gfSend(m, tui.EventMsg{Event: ev})
	}
	return m
}

// gfUntil returns the scripted events up to and including the first one of
// the given kind, so a frame can be taken from the middle of the run.
func gfUntil(demo tui.DemoOptions, kind events.Kind) []events.Event {
	evs := tui.DemoEvents(demo)
	for i, ev := range evs {
		if ev.Kind() == kind {
			return evs[:i+1]
		}
	}
	return evs
}

// gfSend applies messages to the model and returns the model they produced,
// dropping the commands: a golden frame is a still.
func gfSend(m tui.Model, msgs ...tea.Msg) tui.Model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		out, ok := next.(tui.Model)
		if !ok {
			panic("tui: Update returned something that is not a tui.Model")
		}
		m = out
	}
	return m
}

// gfKey builds a key press from its printable spelling.
func gfKey(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// gfRequireGeometry proves the frame is exactly the terminal: h lines, none
// of them wider than w columns.
func gfRequireGeometry(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	require.Len(t, lines, h, "the frame is not %d lines", h)
	for i, l := range lines {
		require.LessOrEqual(t, lipgloss.Width(l), w, "line %d is wider than %d columns: %q", i, w, l)
	}
}

// gfGolden compares got with testdata/tui/frame-<name>.golden. Set
// UPDATE_GOLDEN=1 (make update-golden-tui) to rewrite the file instead.
func gfGolden(t *testing.T, name, got string) {
	t.Helper()
	gfCompare(t, filepath.Join(gfGoldenDir, gfPrefix+name+".golden"), got)
}

// gfCompare is the golden comparison itself, on an explicit path.
func gfCompare(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644)) //nolint:gosec // goldens are readable fixtures
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a test-owned path
	require.NoError(t, err, "missing golden %s; run `make update-golden-tui`", path)
	require.Equal(t, string(want), got, "frame differs from %s; run `make update-golden-tui` if the change is intended", path)
}

// gfLoad reads a golden frame, failing the test when it is missing.
func gfLoad(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(gfGoldenDir, gfPrefix+name+".golden")
	body, err := os.ReadFile(path) //nolint:gosec // a test-owned path
	require.NoError(t, err, "missing golden %s; run `make update-golden-tui`", path)
	return string(body)
}

// gfMeaningfulLines returns the lines of a frame worth looking for in a
// program's output: blank and one-glyph lines say nothing, and the renderer
// drops trailing spaces, so they are trimmed off.
func gfMeaningfulLines(frame string) []string {
	out := make([]string, 0, 8)
	for _, l := range strings.Split(frame, "\n") {
		l = strings.TrimRight(l, " ")
		if len([]rune(strings.TrimSpace(l))) < 8 {
			continue
		}
		out = append(out, l)
	}
	return out
}

// gfWaitFor blocks until every string has appeared in the program's output,
// and returns everything drawn so far. teatest's reader is drained by each
// read, so the output is accumulated here and every expectation is checked
// against the whole of it.
func gfWaitFor(t *testing.T, tm *teatest.TestModel, want ...string) string {
	t.Helper()
	var seen strings.Builder
	deadline := time.Now().Add(gfWaitTimeout)
	for {
		chunk, err := io.ReadAll(tm.Output())
		require.NoError(t, err, "reading the program output")
		seen.Write(chunk)
		out := seen.String()

		plain := gfPlain(out)
		missing := ""
		for _, w := range want {
			if !strings.Contains(plain, gfPlain(w)) {
				missing = w
				break
			}
		}
		if missing == "" {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %q; drawn so far:\n%s", gfWaitTimeout, missing, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
