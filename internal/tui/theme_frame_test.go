package tui_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/tui"
)

// P7-07 — the composed frame at a colour profile.
//
// The golden frames are all rendered with the no-colour theme, so they prove
// what the TUI looks like when colour is gone but say nothing about what it
// emits when colour is on. These tests fill that hole: they build the whole
// program — chrome, tab bar, every view, the overlays — at each colour depth
// and require every colour escape in the frame to be one the palette in
// theme.go could have produced. A region left on a hand-rolled palette, or a
// style built from the global Lip Gloss renderer instead of the theme's
// pinned one, emits a colour that is not in the palette and fails here.

// themeFrameWidth and themeFrameHeight are big enough for every view to draw
// its cards, its graph and its overlays.
const (
	themeFrameWidth  = 100
	themeFrameHeight = 30
)

// themeFrameDemo is the scripted run every frame below is built from, with
// no wall-clock pacing.
var themeFrameDemo = tui.DemoOptions{Delay: -1}

// themeFrameClock is the fixed clock the frames are rendered against.
func themeFrameClock() time.Time { return tui.DemoStart.Add(20 * time.Minute) }

// themeFrameScenario is one composed frame: the keys to press after the
// whole scripted run has been folded in.
type themeFrameScenario struct {
	// name says which part of the TUI the frame is showing.
	name string
	// keys are pressed in order once the run has been replayed.
	keys []string
}

// themeFrameScenarios covers every region the P7-07 palette has to reach:
// the graph, the agent cards and one transcript, the feedback and delivery
// views, the log, and both overlays.
func themeFrameScenarios() []themeFrameScenario {
	return []themeFrameScenario{
		{name: "graph", keys: nil},
		{name: "graph detail", keys: []string{"enter"}},
		{name: "agents", keys: []string{"2"}},
		{name: "agents transcript", keys: []string{"2", "enter"}},
		{name: "feedback", keys: []string{"3"}},
		{name: "deliveries", keys: []string{"4"}},
		{name: "log", keys: []string{"5"}},
		{name: "help overlay", keys: []string{"?"}},
	}
}

// themeFrameModel builds the TUI with theme th, replays the scripted run and
// presses keys. The animation layer is off so the frame is a still.
func themeFrameModel(t *testing.T, th *tui.Theme, keys ...string) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:    tui.DemoRun(themeFrameDemo),
		Theme:  th,
		NoAnim: true,
		Now:    themeFrameClock,
	})
	m = themeFrameSend(m, tea.WindowSizeMsg{Width: themeFrameWidth, Height: themeFrameHeight})
	for _, ev := range tui.DemoEvents(themeFrameDemo) {
		m = themeFrameSend(m, tui.EventMsg{Event: ev})
	}
	for _, k := range keys {
		m = themeFrameSend(m, themeFrameKey(k))
	}
	return m
}

// themeFrameSend applies messages the way the update loop would, dropping
// the commands: these frames are stills.
func themeFrameSend(m tui.Model, msgs ...tea.Msg) tui.Model {
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

// themeFrameKey turns a key name into the message Bubble Tea would deliver.
func themeFrameKey(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// themeFramePalette is every colour the theme can put on a frame: the
// semantic chrome entries and one per bead state, severity, agent state,
// role and lens.
func themeFramePalette(th *tui.Theme) []tui.Color {
	out := []tui.Color{
		tui.ColorText, tui.ColorMuted, tui.ColorAccent, tui.ColorRule,
		tui.ColorSuccess, tui.ColorWarning, tui.ColorDanger, tui.ColorFire,
	}
	for _, s := range domain.BeadStates() {
		out = append(out, th.StateColor(s))
	}
	for _, s := range domain.Severities() {
		out = append(out, th.SeverityColor(s))
	}
	for _, s := range domain.AgentStates() {
		out = append(out, th.AgentStateColor(s))
	}
	for _, r := range domain.Roles() {
		out = append(out, th.RoleColor(r))
	}
	for _, l := range domain.Lenses() {
		out = append(out, th.LensColor(l))
	}
	return out
}

// themeFrameAllowed is the set of SGR colour tokens the theme may emit at
// its own profile, mapped to the palette value that produced each. It is
// built by rendering the palette rather than by writing the escapes down, so
// it cannot drift from what [tui.Color.Resolve] actually does.
func themeFrameAllowed(t *testing.T, th *tui.Theme) map[string]string {
	t.Helper()
	allowed := map[string]string{}
	for _, c := range themeFramePalette(th) {
		for _, tok := range themeFrameColorsIn(th.Style(c).Render("x")) {
			allowed[tok] = c.String()
		}
	}
	return allowed
}

// themeFrameSGR matches one SGR sequence and captures its parameters.
var themeFrameSGR = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// themeFrameEscape matches any ANSI escape sequence.
var themeFrameEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// themeFrameColorsIn returns every colour an SGR sequence in s selects, each
// normalised to the parameters that name it: "38;2;r;g;b" for 24-bit,
// "38;5;n" for the 256-colour palette, and the bare code (30…37, 90…97, and
// the 40s and 100s for a background) for the 16-colour one. Attributes —
// reset, bold, underline, and the codes that switch them off — are not
// colours and are left out.
func themeFrameColorsIn(s string) []string {
	var out []string
	for _, m := range themeFrameSGR.FindAllStringSubmatch(s, -1) {
		ps := strings.Split(m[1], ";")
		for i := 0; i < len(ps); i++ {
			n, err := strconv.Atoi(ps[i])
			if err != nil {
				continue
			}
			switch {
			case n == 38 || n == 48:
				switch {
				case i+2 < len(ps) && ps[i+1] == "5":
					out = append(out, strings.Join(ps[i:i+3], ";"))
					i += 2
				case i+4 < len(ps) && ps[i+1] == "2":
					out = append(out, strings.Join(ps[i:i+5], ";"))
					i += 4
				default:
					out = append(out, ps[i])
				}
			case n >= 30 && n <= 37, n >= 40 && n <= 47,
				n >= 90 && n <= 97, n >= 100 && n <= 107:
				out = append(out, ps[i])
			}
		}
	}
	return out
}

// themeFrameStrip removes every escape, leaving the characters the user
// reads.
func themeFrameStrip(s string) string { return themeFrameEscape.ReplaceAllString(s, "") }

// TestThemeComposedFrameUsesOnlyPaletteColors is the P7-07 acceptance test
// for the whole display: at every colour depth, every colour in a composed
// frame comes from the palette, and the frame really is coloured. A view
// still building its own styles with 16-colour indices — which is what the
// header, the tab bar, the overlays and the Agents view used to do — puts a
// code in the frame that the palette never produces, and fails here.
func TestThemeComposedFrameUsesOnlyPaletteColors(t *testing.T) {
	t.Parallel()

	for _, p := range []tui.Profile{tui.ProfileANSI, tui.ProfileANSI256, tui.ProfileTrueColor} {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			th := tui.ThemeFor(p)
			allowed := themeFrameAllowed(t, th)
			require.NotEmpty(t, allowed, "a coloured profile must resolve to colours")

			for _, sc := range themeFrameScenarios() {
				t.Run(sc.name, func(t *testing.T) {
					t.Parallel()
					frame := themeFrameModel(t, th, sc.keys...).View()

					seen := map[string]bool{}
					for _, tok := range themeFrameColorsIn(frame) {
						require.Containsf(t, allowed, tok,
							"the %s frame at %s carries \x1b[%sm, which is not a palette colour: "+
								"build the style from the theme, not from lipgloss.NewStyle",
							sc.name, p, tok)
						seen[tok] = true
					}
					require.GreaterOrEqualf(t, len(seen), 3,
						"the %s frame at %s is barely coloured (%d palette colours): "+
							"the theme is not reaching it", sc.name, p, len(seen))
				})
			}
		})
	}
}

// TestThemeComposedFrameColorsNothingWithoutColor is the other end of the
// same promise: the no-colour theme puts no colour on a composed frame, so
// `--no-color` and NO_COLOR really do render the display the goldens hold.
func TestThemeComposedFrameColorsNothingWithoutColor(t *testing.T) {
	t.Parallel()

	th := tui.ThemeFor(tui.ProfileNoColor)
	for _, sc := range themeFrameScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			frame := themeFrameModel(t, th, sc.keys...).View()
			require.Emptyf(t, themeFrameColorsIn(frame),
				"the %s frame carries colour with the no-colour theme", sc.name)
		})
	}
}

// TestThemeComposedFrameReadsTheSameAtEveryDepth requires colour to be
// decoration and nothing else: strip the escapes and the frame a truecolor
// terminal shows is character-for-character the frame a NO_COLOR one shows.
// A palette that padded, aligned or truncated differently once colour was on
// would change the layout, and the glyph channel would stop being the second
// channel it is meant to be.
func TestThemeComposedFrameReadsTheSameAtEveryDepth(t *testing.T) {
	t.Parallel()

	for _, sc := range themeFrameScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			want := themeFrameStrip(themeFrameModel(t, tui.ThemeFor(tui.ProfileNoColor), sc.keys...).View())
			for _, p := range tui.Profiles() {
				got := themeFrameStrip(themeFrameModel(t, tui.ThemeFor(p), sc.keys...).View())
				require.Equalf(t, want, got,
					"the %s frame says something different at %s", sc.name, p)
			}
		})
	}
}
