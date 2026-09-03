package tui_test

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/tui"
)

// themeHexRGB splits a "#rrggbb" palette value into its channels.
func themeHexRGB(t *testing.T, hex string) (r, g, b int) {
	t.Helper()
	require.Truef(t, strings.HasPrefix(hex, "#") && len(hex) == 7, "%q is not #rrggbb", hex)
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	require.NoError(t, err)
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)
}

// themeAssertColor checks that th renders c the way its profile demands: no
// escape at all without colour, and the right SGR sequence with it.
func themeAssertColor(t *testing.T, th *tui.Theme, c tui.Color, what string) {
	t.Helper()
	got := th.Style(c).Render("x")
	where := what + " at " + th.Profile().String()

	switch th.Profile() {
	case tui.ProfileNoColor:
		require.Equalf(t, "x", got, "%s must render without any escape", where)
		return
	case tui.ProfileANSI:
		idx, err := strconv.Atoi(c.ANSI)
		require.NoErrorf(t, err, "%s: ANSI index %q is not a number", where, c.ANSI)
		require.GreaterOrEqualf(t, idx, 0, "%s: ANSI index must be 0…15", where)
		require.Lessf(t, idx, 16, "%s: ANSI index must be 0…15", where)
		code := 30 + idx
		if idx >= 8 {
			code = 90 + idx - 8
		}
		require.Containsf(t, got, fmt.Sprintf("\x1b[%dm", code), "%s: %q", where, got)
	case tui.ProfileANSI256:
		idx, err := strconv.Atoi(c.ANSI256)
		require.NoErrorf(t, err, "%s: ANSI256 index %q is not a number", where, c.ANSI256)
		require.GreaterOrEqualf(t, idx, 0, "%s: ANSI256 index must be 0…255", where)
		require.Lessf(t, idx, 256, "%s: ANSI256 index must be 0…255", where)
		require.Containsf(t, got, fmt.Sprintf("\x1b[38;5;%dm", idx), "%s: %q", where, got)
	case tui.ProfileTrueColor:
		wr, wg, wb := themeHexRGB(t, c.TrueColor)
		var gr, gg, gb int
		n, _ := fmt.Sscanf(got, "\x1b[38;2;%d;%d;%dm", &gr, &gg, &gb)
		require.Equalf(t, 3, n, "%s: not a 24-bit sequence: %q", where, got)
		// Lip Gloss round-trips the hex through a float colour, which can
		// lose one unit per channel; the palette value is still what shows.
		require.InDeltaf(t, wr, gr, 1, "%s red: %q", where, got)
		require.InDeltaf(t, wg, gg, 1, "%s green: %q", where, got)
		require.InDeltaf(t, wb, gb, 1, "%s blue: %q", where, got)
	default:
		t.Fatalf("%s: unknown profile", where)
	}
	require.Equalf(t, "x", stripThemeSGR(got), "%s lost its text: %q", where, got)
}

// themeIsBold reports whether any SGR sequence in s sets the bold attribute.
func themeIsBold(s string) bool {
	for _, seq := range strings.Split(s, "\x1b[") {
		i := strings.IndexByte(seq, 'm')
		if i < 0 {
			continue
		}
		for _, p := range strings.Split(seq[:i], ";") {
			if p == "1" {
				return true
			}
		}
	}
	return false
}

func TestThemeProfileEnum(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		[]tui.Profile{tui.ProfileNoColor, tui.ProfileANSI, tui.ProfileANSI256, tui.ProfileTrueColor},
		tui.Profiles())
	require.Equal(t, "no-color", tui.ProfileNoColor.String())
	require.Equal(t, "ansi", tui.ProfileANSI.String())
	require.Equal(t, "ansi256", tui.ProfileANSI256.String())
	require.Equal(t, "truecolor", tui.ProfileTrueColor.String())
	require.Equal(t, "invalid", tui.Profile(99).String())
	require.False(t, tui.Profile(99).Valid())
	require.False(t, tui.Profile(-1).Valid())
	for _, p := range tui.Profiles() {
		require.Truef(t, p.Valid(), "%d", int(p))
	}
}

func TestThemeDetectProfile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
		want tui.Profile
	}{
		{"empty environment", nil, tui.ProfileNoColor},
		{"dumb terminal", map[string]string{"TERM": "dumb"}, tui.ProfileNoColor},
		{"NO_COLOR beats everything", map[string]string{
			"NO_COLOR": "", "TERM": "xterm-256color", "COLORTERM": "truecolor", "CLICOLOR_FORCE": "1",
		}, tui.ProfileNoColor},
		{"CLICOLOR=0", map[string]string{"CLICOLOR": "0", "TERM": "xterm-256color"}, tui.ProfileNoColor},
		{"CLICOLOR_FORCE on a dumb terminal", map[string]string{"CLICOLOR_FORCE": "1", "TERM": "dumb"}, tui.ProfileANSI},
		{"CLICOLOR_FORCE=0 does not force", map[string]string{"CLICOLOR_FORCE": "0", "TERM": "dumb"}, tui.ProfileNoColor},
		{"plain xterm", map[string]string{"TERM": "xterm"}, tui.ProfileANSI},
		{"256 colours", map[string]string{"TERM": "screen-256color"}, tui.ProfileANSI256},
		{"COLORTERM truecolor", map[string]string{"TERM": "xterm-256color", "COLORTERM": "TrueColor"}, tui.ProfileTrueColor},
		{"COLORTERM 24bit", map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, tui.ProfileTrueColor},
		{"direct colour TERM", map[string]string{"TERM": "xterm-direct"}, tui.ProfileTrueColor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
			require.Equal(t, tc.want, tui.DetectProfile(lookup))
		})
	}
}

func TestThemeDetectProfileReadsTheRealEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	require.Equal(t, tui.ProfileNoColor, tui.DetectProfile(nil))
	require.Equal(t, tui.ProfileNoColor, tui.EnvTheme(false).Profile())
	require.Equal(t, tui.ProfileNoColor, tui.EnvTheme(true).Profile())
}

func TestThemeEnvThemeHonoursNoColorFlag(t *testing.T) {
	// t.Setenv records the previous value, so unsetting afterwards is still
	// restored when the test ends.
	for _, k := range []string{"NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "COLORTERM"} {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}
	t.Setenv("TERM", "xterm-256color")

	require.Equal(t, tui.ProfileNoColor, tui.EnvTheme(true).Profile())
	require.Equal(t, tui.ProfileANSI256, tui.EnvTheme(false).Profile())
}

func TestThemeCachesOnePerProfile(t *testing.T) {
	t.Parallel()

	for _, p := range tui.Profiles() {
		require.Same(t, tui.ThemeFor(p), tui.ThemeFor(p))
		require.Equal(t, p, tui.ThemeFor(p).Profile())
		require.NotNil(t, tui.ThemeFor(p).Renderer())
	}
	require.Equal(t, tui.ProfileNoColor, tui.ThemeFor(tui.Profile(99)).Profile())
	require.Equal(t, tui.ProfileNoColor, tui.NewTheme(tui.Profile(-3)).Profile())
}

// TestThemeRendersEveryStateInEveryProfile is the P7-07 "done when": every
// D-07 state (and every bd built-in the cannon reads) renders at every colour
// depth, with the right escape sequence and a colour-free glyph.
func TestThemeRendersEveryStateInEveryProfile(t *testing.T) {
	t.Parallel()

	for _, p := range tui.Profiles() {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			th := tui.ThemeFor(p)
			for _, s := range domain.BeadStates() {
				themeAssertColor(t, th, th.StateColor(s), "state "+s.String())

				label := th.StateLabel(s)
				require.Containsf(t, label, s.String(), "%s label lost its name", s)
				require.Containsf(t, label, tui.StateGlyph(s), "%s label lost its glyph", s)
				require.Equalf(t, lipgloss.Width(tui.StateGlyph(s))+1+len(s.String()), lipgloss.Width(label),
					"%s label is not glyph + space + name", s)

				if p == tui.ProfileNoColor {
					require.NotContainsf(t, label, "38;5;", "%s must not colour with NO_COLOR", s)
					require.NotContainsf(t, label, "38;2;", "%s must not colour with NO_COLOR", s)
					if !s.Terminal() {
						require.Equalf(t, tui.StateGlyph(s)+" "+s.String(), label,
							"%s must render bare with NO_COLOR", s)
					}
				}
			}
		})
	}
}

func TestThemeStateColorsAreDistinctPerProfile(t *testing.T) {
	t.Parallel()

	th := tui.ThemeFor(tui.ProfileTrueColor)
	for _, field := range []struct {
		name string
		of   func(tui.Color) string
	}{
		{"truecolor", func(c tui.Color) string { return c.TrueColor }},
		{"ansi256", func(c tui.Color) string { return c.ANSI256 }},
		{"ansi", func(c tui.Color) string { return c.ANSI }},
	} {
		seen := map[string]domain.BeadState{}
		for _, s := range domain.WorkflowBeadStates() {
			v := field.of(th.StateColor(s))
			require.NotEmptyf(t, v, "%s has no %s value", s, field.name)
			prev, dup := seen[v]
			require.Falsef(t, dup, "%s and %s share the %s value %q", prev, s, field.name, v)
			seen[v] = s
		}
	}
}

// TestThemeGlyphsFillOneColumnAndAreDistinct pins the colour-free channel:
// every glyph the palette hands out is exactly [tui.GlyphCells] wide, so the
// column a status, severity, role or agent state is drawn in never changes
// width with the value in it, and no two states of a set share a glyph.
//
// The column is one cell wherever East-Asian ambiguous characters are
// narrow, which is every terminal that has not been told otherwise; a
// process started with RUNEWIDTH_EASTASIAN=1 gets a two-cell column instead,
// and [tui.PadGlyph] is what keeps the mixed geometric (○ ● ▲) and dingbat
// (✓ ✗ ⚑) glyphs the same width there.
// TestThemeGlyphColumnHoldsUnderEastAsianWidth runs this whole file in that
// mode, so both are covered.
func TestThemeGlyphsFillOneColumnAndAreDistinct(t *testing.T) {
	t.Parallel()

	if !themeEastAsianWidth() {
		require.Equal(t, 1, tui.GlyphCells(),
			"an ordinary terminal must get a one-cell glyph column")
	}
	require.Positive(t, tui.GlyphCells())

	seen := map[string]domain.BeadState{}
	for _, s := range domain.BeadStates() {
		g := tui.StateGlyph(s)
		require.Equalf(t, tui.GlyphCells(), lipgloss.Width(g),
			"glyph %q of %s must fill the glyph column", g, s)
		prev, dup := seen[g]
		require.Falsef(t, dup, "%s and %s share the glyph %q", prev, s, g)
		seen[g] = s
	}
	require.Equal(t, tui.PadGlyph(tui.UnknownGlyph), tui.StateGlyph(domain.BeadState("not-a-status")))

	for _, s := range domain.Severities() {
		require.Equal(t, tui.GlyphCells(), lipgloss.Width(tui.SeverityGlyph(s)))
	}
	for _, s := range domain.AgentStates() {
		require.Equal(t, tui.GlyphCells(), lipgloss.Width(tui.AgentStateGlyph(s)))
	}
	for _, r := range domain.Roles() {
		require.Equal(t, tui.GlyphCells(), lipgloss.Width(tui.RoleGlyph(r)))
	}
	// Padding is a suffix, never a replacement: the glyph itself is intact.
	require.Equal(t, "?", strings.TrimRight(tui.PadGlyph(tui.UnknownGlyph), " "))
	require.Equal(t, "xx", tui.PadGlyph("xx"), "a glyph already wide enough is left alone")
}

// themeEAChildEnv marks the re-executed test binary of
// [TestThemeGlyphColumnHoldsUnderEastAsianWidth], so the child does not
// spawn a child of its own.
const themeEAChildEnv = "SC_TUI_THEME_EASTASIAN_CHILD"

// themeEastAsianWidth reports whether this process was started in the width
// mode where East-Asian ambiguous characters take two cells. The width
// library reads the variable once, at start-up, so this is fixed for the
// life of the process — which is why the test below needs a second one.
func themeEastAsianWidth() bool {
	v, err := strconv.ParseBool(os.Getenv("RUNEWIDTH_EASTASIAN"))
	return err == nil && v
}

// TestThemeGlyphColumnHoldsUnderEastAsianWidth re-runs every theme test in a
// child process started with RUNEWIDTH_EASTASIAN=1, the setting a terminal
// configured for ambiguous-width 2 exports. There the geometric glyphs are
// two cells and the dingbats are one, so a palette that assumed one cell
// everywhere would shift a status column and stretch a node box; the child
// proves the glyph column and the chrome hold up in both modes.
//
// It has to be a child: the width mode is read at package initialisation,
// so it cannot be changed from inside a running test.
func TestThemeGlyphColumnHoldsUnderEastAsianWidth(t *testing.T) {
	t.Parallel()

	if os.Getenv(themeEAChildEnv) != "" {
		t.Skip("already the re-executed child")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // re-runs this very test binary
		"-test.run", "^TestTheme", "-test.count=1", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=1", themeEAChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "the theme tests fail with RUNEWIDTH_EASTASIAN=1:\n%s", out)
}

func TestThemeRendersSeveritiesRolesLensesAndAgentStates(t *testing.T) {
	t.Parallel()

	for _, p := range tui.Profiles() {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			th := tui.ThemeFor(p)
			for _, s := range domain.Severities() {
				themeAssertColor(t, th, th.SeverityColor(s), "severity "+s.String())
				require.Contains(t, th.SeverityLabel(s), s.String())
				require.Contains(t, th.SeverityLabel(s), tui.SeverityGlyph(s))
			}
			for _, s := range domain.AgentStates() {
				themeAssertColor(t, th, th.AgentStateColor(s), "agent state "+s.String())
				require.NotEmpty(t, th.AgentStateStyle(s).Render(s.String()))
			}
			for _, r := range domain.Roles() {
				themeAssertColor(t, th, th.RoleColor(r), "role "+r.String())
				require.NotEmpty(t, th.RoleStyle(r).Render(r.String()))
			}
			for _, l := range domain.Lenses() {
				themeAssertColor(t, th, th.LensColor(l), "lens "+l.String())
				require.NotEmpty(t, th.LensStyle(l).Render(l.String()))
			}
		})
	}
}

func TestThemeChromeStylesFollowTheProfile(t *testing.T) {
	t.Parallel()

	for _, p := range tui.Profiles() {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			th := tui.ThemeFor(p)
			for name, c := range map[string]tui.Color{
				"text": tui.ColorText, "muted": tui.ColorMuted, "accent": tui.ColorAccent,
				"rule": tui.ColorRule, "success": tui.ColorSuccess, "warning": tui.ColorWarning,
				"danger": tui.ColorDanger, "fire": tui.ColorFire,
			} {
				themeAssertColor(t, th, c, name)
			}
			require.Equal(t, "x", stripThemeSGR(th.Text().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Muted().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Accent().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Success().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Warning().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Danger().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Fire().Render("x")))
			require.Equal(t, "x", stripThemeSGR(th.Rule().Render("x")))

			// Title and Selected stay legible without colour: both are bold.
			require.True(t, themeIsBold(th.Title().Render("x")))
			require.True(t, themeIsBold(th.Selected().Render("x")))

			// The box adds a rounded border, so it is three lines tall,
			// and one cell of padding on each side. Its width is measured
			// rather than written down: the border is drawn with box
			// characters, which a terminal configured for ambiguous-width 2
			// gives two cells each. Lip Gloss lays the box out with the same
			// measure, so the frame is still square there — it is only the
			// arithmetic in a test that would go wrong.
			border := lipgloss.RoundedBorder()
			box := th.Box().Render("x")
			require.Len(t, strings.Split(box, "\n"), 3)
			require.Equal(t,
				lipgloss.Width(border.Left)+1+lipgloss.Width("x")+1+lipgloss.Width(border.Right),
				lipgloss.Width(box))

			// Base is bound to the theme's renderer, not the global one.
			require.Equal(t, "x", th.Base().Render("x"))
		})
	}
}

func TestThemeUnknownValuesFallBackToMuted(t *testing.T) {
	t.Parallel()

	th := tui.ThemeFor(tui.ProfileANSI256)
	require.Equal(t, tui.ColorMuted, th.StateColor(domain.BeadState("nope")))
	require.Equal(t, tui.ColorMuted, th.SeverityColor(domain.Severity("nope")))
	require.Equal(t, tui.ColorMuted, th.AgentStateColor(domain.AgentState("nope")))
	require.Equal(t, tui.ColorMuted, th.RoleColor(domain.Role("nope")))
	require.Equal(t, tui.ColorMuted, th.LensColor(domain.Lens("nope")))
	require.Equal(t, tui.PadGlyph(tui.UnknownGlyph), tui.SeverityGlyph(domain.Severity("nope")))
	require.Equal(t, tui.PadGlyph(tui.UnknownGlyph), tui.AgentStateGlyph(domain.AgentState("nope")))
	require.Equal(t, tui.PadGlyph(tui.UnknownGlyph), tui.RoleGlyph(domain.Role("nope")))
}

func TestThemeColorResolve(t *testing.T) {
	t.Parallel()

	c := tui.Color{TrueColor: "#112233", ANSI256: "42", ANSI: "5"}
	require.Equal(t, "#112233", c.String())
	require.Equal(t, lipgloss.Color("#112233"), c.Resolve(tui.ProfileTrueColor))
	require.Equal(t, lipgloss.Color("42"), c.Resolve(tui.ProfileANSI256))
	require.Equal(t, lipgloss.Color("5"), c.Resolve(tui.ProfileANSI))
	require.Equal(t, lipgloss.NoColor{}, c.Resolve(tui.ProfileNoColor))
	require.Equal(t, lipgloss.NoColor{}, c.Resolve(tui.Profile(99)))
}

// stripThemeSGR removes every CSI ... m sequence, leaving the plain text.
func stripThemeSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
