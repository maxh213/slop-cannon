package tui

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"slop-cannon/internal/domain"
)

// Profile is the colour depth a [Theme] renders for. It is the TUI's own
// spelling of a terminal colour profile so callers never have to import
// termenv, and so tests can render every palette entry at every depth
// without a terminal.
type Profile int

// The supported colour depths, from none to 24-bit.
const (
	// ProfileNoColor emits no colour at all. It is what NO_COLOR,
	// `--no-color` and a dumb terminal select; attributes that are not
	// colour (bold, underline) still apply, and every state keeps its
	// glyph, so the display stays readable.
	ProfileNoColor Profile = iota
	// ProfileANSI is the 16-colour palette every terminal has.
	ProfileANSI
	// ProfileANSI256 is the 256-colour palette (`TERM=*-256color`).
	ProfileANSI256
	// ProfileTrueColor is 24-bit colour (`COLORTERM=truecolor`).
	ProfileTrueColor
)

// profileNames are the names [Profile.String] prints, indexed by [Profile].
var profileNames = [...]string{"no-color", "ansi", "ansi256", "truecolor"}

// Profiles returns every colour depth, shallowest first.
func Profiles() []Profile {
	out := make([]Profile, len(profileNames))
	for i := range profileNames {
		out[i] = Profile(i)
	}
	return out
}

// String returns the profile's name, e.g. "ansi256".
func (p Profile) String() string {
	if !p.Valid() {
		return "invalid"
	}
	return profileNames[p]
}

// Valid reports whether p is one of the declared profiles.
func (p Profile) Valid() bool { return p >= 0 && int(p) < len(profileNames) }

// termenv converts p to the profile lipgloss speaks. ProfileNoColor maps to
// termenv.ANSI rather than termenv.Ascii on purpose: Ascii would drop bold
// and underline too, and NO_COLOR only asks for colour to go away. Colour
// itself is removed one level up, by [Color.Resolve] returning
// [lipgloss.NoColor] — which is why every style must be built from the
// theme's helpers rather than from a raw lipgloss colour.
func (p Profile) termenv() termenv.Profile {
	switch p {
	case ProfileANSI, ProfileNoColor:
		return termenv.ANSI
	case ProfileANSI256:
		return termenv.ANSI256
	case ProfileTrueColor:
		return termenv.TrueColor
	default:
		return termenv.ANSI
	}
}

// DetectProfile picks a colour depth from the environment. lookup reads an
// environment variable the way [os.LookupEnv] does; a nil lookup reads the
// real environment.
//
// The rules, in order: NO_COLOR (present, at any value — the same test
// package cli applies) turns colour off; CLICOLOR=0 turns it off;
// CLICOLOR_FORCE (present and not "0") turns it on even for a dumb or unset
// TERM; TERM=dumb or an empty TERM turns it off; COLORTERM=truecolor/24bit
// or a TERM naming direct colour selects 24-bit; a TERM naming 256 colours
// selects 256; anything else gets the 16-colour palette.
func DetectProfile(lookup func(string) (string, bool)) Profile {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(k string) string { v, _ := lookup(k); return strings.ToLower(strings.TrimSpace(v)) }

	if _, set := lookup("NO_COLOR"); set {
		return ProfileNoColor
	}
	if v, set := lookup("CLICOLOR"); set && strings.TrimSpace(v) == "0" {
		return ProfileNoColor
	}
	forced := false
	if v, set := lookup("CLICOLOR_FORCE"); set && strings.TrimSpace(v) != "0" {
		forced = true
	}
	term := get("TERM")
	if !forced && (term == "" || term == "dumb") {
		return ProfileNoColor
	}
	switch colorterm := get("COLORTERM"); colorterm {
	case "truecolor", "24bit":
		return ProfileTrueColor
	}
	switch {
	case strings.Contains(term, "truecolor"), strings.Contains(term, "direct"):
		return ProfileTrueColor
	case strings.Contains(term, "256color"):
		return ProfileANSI256
	default:
		return ProfileANSI
	}
}

// Color is one palette entry with an explicit value per colour depth, so the
// 256- and 16-colour fallbacks are chosen here rather than by a lossy
// automatic down-conversion. TrueColor is a "#rrggbb" hex string, ANSI256 a
// 0…255 index and ANSI a 0…15 index, both written as decimal strings.
type Color struct {
	// TrueColor is the 24-bit value as "#rrggbb".
	TrueColor string
	// ANSI256 is the closest 256-palette index, as a decimal string.
	ANSI256 string
	// ANSI is the closest 16-palette index, as a decimal string.
	ANSI string
}

// Resolve returns the colour to use at depth p, or [lipgloss.NoColor] when p
// has no colour.
func (c Color) Resolve(p Profile) lipgloss.TerminalColor {
	switch p {
	case ProfileTrueColor:
		return lipgloss.Color(c.TrueColor)
	case ProfileANSI256:
		return lipgloss.Color(c.ANSI256)
	case ProfileANSI:
		return lipgloss.Color(c.ANSI)
	case ProfileNoColor:
		return lipgloss.NoColor{}
	default:
		return lipgloss.NoColor{}
	}
}

// String returns the 24-bit value, which is how the palette is documented.
func (c Color) String() string { return c.TrueColor }

// The palette. The hues come from the Okabe–Ito colour-blind-safe set, which
// stays distinguishable under deuteranopia, protanopia and tritanopia; the
// 256- and 16-colour entries are the nearest members of those palettes that
// keep neighbouring states apart (in_review's bright yellow against
// changes_requested's dim yellow-orange, approved's green against closed's
// bright green). Nothing in the TUI relies on colour alone: every state also
// has a glyph from [StateGlyph].
var (
	// ColorText is ordinary foreground text.
	ColorText = Color{TrueColor: "#E6E6E6", ANSI256: "254", ANSI: "15"}
	// ColorMuted is de-emphasised text: hints, empty views, dependencies.
	ColorMuted = Color{TrueColor: "#9E9E9E", ANSI256: "246", ANSI: "8"}
	// ColorAccent highlights a value or the cursor row.
	ColorAccent = Color{TrueColor: "#56B4E9", ANSI256: "117", ANSI: "12"}
	// ColorRule draws separators and box borders.
	ColorRule = Color{TrueColor: "#5C5C5C", ANSI256: "240", ANSI: "8"}
	// ColorSuccess marks something that went right.
	ColorSuccess = Color{TrueColor: "#009E73", ANSI256: "36", ANSI: "2"}
	// ColorWarning marks something that needs attention but is not fatal.
	ColorWarning = Color{TrueColor: "#E69F00", ANSI256: "214", ANSI: "3"}
	// ColorDanger marks a failure.
	ColorDanger = Color{TrueColor: "#D55E00", ANSI256: "166", ANSI: "9"}
	// ColorFire is the cannon's own colour.
	ColorFire = Color{TrueColor: "#E69F00", ANSI256: "208", ANSI: "11"}
)

// stateColors maps every [domain.BeadState] — the D-07 workflow states and
// the bd built-ins the cannon only ever reads — to its palette entry.
var stateColors = map[domain.BeadState]Color{
	domain.BeadStateOpen:             {TrueColor: "#9E9E9E", ANSI256: "246", ANSI: "8"},  // todo grey
	domain.BeadStateInProgress:       {TrueColor: "#56B4E9", ANSI256: "117", ANSI: "12"}, // implementing blue
	domain.BeadStateInReview:         {TrueColor: "#F0E442", ANSI256: "227", ANSI: "11"}, // in_review yellow
	domain.BeadStateChangesRequested: {TrueColor: "#E69F00", ANSI256: "214", ANSI: "3"},  // changes orange
	domain.BeadStateApproved:         {TrueColor: "#009E73", ANSI256: "36", ANSI: "2"},   // approved green
	domain.BeadStateDelivering:       {TrueColor: "#0072B2", ANSI256: "32", ANSI: "4"},   // delivering deep blue
	domain.BeadStateClosed:           {TrueColor: "#00D3A0", ANSI256: "48", ANSI: "10"},  // done bright green
	domain.BeadStateEscalated:        {TrueColor: "#CC79A7", ANSI256: "176", ANSI: "13"}, // escalated magenta
	domain.BeadStateNeedsHuman:       {TrueColor: "#B07AA1", ANSI256: "170", ANSI: "5"},  // needs_human dim magenta
	domain.BeadStateFailed:           {TrueColor: "#D55E00", ANSI256: "166", ANSI: "9"},  // failed red
	domain.BeadStateBlocked:          {TrueColor: "#C0392B", ANSI256: "160", ANSI: "1"},  // blocked red
	domain.BeadStateHooked:           {TrueColor: "#7F8C8D", ANSI256: "244", ANSI: "8"},
	domain.BeadStateDeferred:         {TrueColor: "#7F8C8D", ANSI256: "244", ANSI: "8"},
	domain.BeadStatePinned:           {TrueColor: "#7F8C8D", ANSI256: "244", ANSI: "8"},
}

// stateGlyphs is the second, colour-free channel for a bead state. Every
// glyph is one cell wide so a node box keeps its width in any profile.
var stateGlyphs = map[domain.BeadState]string{
	domain.BeadStateOpen:             "○",
	domain.BeadStateInProgress:       "◐",
	domain.BeadStateInReview:         "◔",
	domain.BeadStateChangesRequested: "↺",
	domain.BeadStateApproved:         "✓",
	domain.BeadStateDelivering:       "↑",
	domain.BeadStateClosed:           "●",
	domain.BeadStateEscalated:        "▲",
	domain.BeadStateNeedsHuman:       "⚑",
	domain.BeadStateFailed:           "✗",
	domain.BeadStateBlocked:          "⊘",
	domain.BeadStateHooked:           "⊙",
	domain.BeadStateDeferred:         "◇",
	domain.BeadStatePinned:           "◈",
}

// severityColors colours a feedback item by how serious it is (P7-04).
var severityColors = map[domain.Severity]Color{
	domain.SeverityBlocker: {TrueColor: "#D55E00", ANSI256: "166", ANSI: "9"},
	domain.SeverityMajor:   {TrueColor: "#E69F00", ANSI256: "214", ANSI: "3"},
	domain.SeverityMinor:   {TrueColor: "#F0E442", ANSI256: "227", ANSI: "11"},
	domain.SeverityNit:     {TrueColor: "#9E9E9E", ANSI256: "246", ANSI: "8"},
}

// severityGlyphs is the colour-free channel for a feedback item's severity.
var severityGlyphs = map[domain.Severity]string{
	domain.SeverityBlocker: "!",
	domain.SeverityMajor:   "▲",
	domain.SeverityMinor:   "•",
	domain.SeverityNit:     "·",
}

// agentStateColors colours an agent card by its lifecycle state (P7-03).
var agentStateColors = map[domain.AgentState]Color{
	domain.AgentStatePending:  {TrueColor: "#9E9E9E", ANSI256: "246", ANSI: "8"},
	domain.AgentStateRunning:  {TrueColor: "#56B4E9", ANSI256: "117", ANSI: "12"},
	domain.AgentStateFinished: {TrueColor: "#009E73", ANSI256: "36", ANSI: "2"},
	domain.AgentStateFailed:   {TrueColor: "#D55E00", ANSI256: "166", ANSI: "9"},
	domain.AgentStateKilled:   {TrueColor: "#CC79A7", ANSI256: "176", ANSI: "13"},
}

// agentStateGlyphs is the colour-free channel for an agent's state.
var agentStateGlyphs = map[domain.AgentState]string{
	domain.AgentStatePending:  "○",
	domain.AgentStateRunning:  "◐",
	domain.AgentStateFinished: "✓",
	domain.AgentStateFailed:   "✗",
	domain.AgentStateKilled:   "⊗",
}

// roleColors and roleGlyphs identify who an agent is at a glance.
var (
	roleColors = map[domain.Role]Color{
		domain.RolePO:       {TrueColor: "#CC79A7", ANSI256: "176", ANSI: "13"},
		domain.RoleEngineer: {TrueColor: "#56B4E9", ANSI256: "117", ANSI: "12"},
		domain.RoleTester:   {TrueColor: "#009E73", ANSI256: "36", ANSI: "2"},
	}
	roleGlyphs = map[domain.Role]string{
		domain.RolePO:       "◆",
		domain.RoleEngineer: "■",
		domain.RoleTester:   "▲",
	}
)

// lensColors keep the three tester lenses apart in the feedback view.
var lensColors = map[domain.Lens]Color{
	domain.LensUX:   {TrueColor: "#56B4E9", ANSI256: "117", ANSI: "12"},
	domain.LensQA:   {TrueColor: "#009E73", ANSI256: "36", ANSI: "2"},
	domain.LensTest: {TrueColor: "#CC79A7", ANSI256: "176", ANSI: "13"},
}

// UnknownGlyph marks a value the palette does not know, so an unexpected bd
// status still renders instead of leaving a hole in the layout.
const UnknownGlyph = "?"

// Theme is the TUI's single source of colour and glyphs. It owns a Lip Gloss
// renderer pinned to one [Profile], so the styles it hands out render the
// same way in a test as in a terminal of that depth, whatever the process's
// own terminal is. A Theme is immutable and safe for concurrent use; get one
// from [ThemeFor] or [EnvTheme] rather than building a new renderer per view.
type Theme struct {
	profile  Profile
	renderer *lipgloss.Renderer
}

// themeCache memoises one Theme per profile: building a renderer is cheap but
// a view must not build one on every frame.
var themeCache [len(profileNames)]struct {
	once sync.Once
	t    *Theme
}

// NewTheme builds a theme for the given colour depth. An invalid profile is
// treated as [ProfileNoColor]. Prefer [ThemeFor], which caches.
func NewTheme(p Profile) *Theme {
	if !p.Valid() {
		p = ProfileNoColor
	}
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p.termenv())
	// Pin the background too: nothing here uses an adaptive colour, and
	// querying the terminal would make rendering depend on the tty.
	r.SetHasDarkBackground(true)
	return &Theme{profile: p, renderer: r}
}

// ThemeFor returns the shared theme for a colour depth, building it once.
func ThemeFor(p Profile) *Theme {
	if !p.Valid() {
		p = ProfileNoColor
	}
	e := &themeCache[p]
	e.once.Do(func() { e.t = NewTheme(p) })
	return e.t
}

// EnvTheme returns the theme for this process: [ProfileNoColor] when noColor
// is set (`--no-color`), otherwise whatever [DetectProfile] finds in the
// environment (which honours NO_COLOR as well).
func EnvTheme(noColor bool) *Theme {
	if noColor {
		return ThemeFor(ProfileNoColor)
	}
	return ThemeFor(DetectProfile(nil))
}

// Profile returns the colour depth t renders for.
func (t *Theme) Profile() Profile { return t.profile }

// Renderer returns the Lip Gloss renderer every style of t is bound to.
func (t *Theme) Renderer() *lipgloss.Renderer { return t.renderer }

// Base returns an unstyled style bound to t's renderer. Build every style
// from it (or from one of the helpers below): a style from
// [lipgloss.NewStyle] uses the global renderer and would ignore t's profile.
func (t *Theme) Base() lipgloss.Style { return t.renderer.NewStyle() }

// Style returns [Theme.Base] with c as the foreground colour.
func (t *Theme) Style(c Color) lipgloss.Style { return t.Base().Foreground(c.Resolve(t.profile)) }

// Text returns the style of ordinary foreground text. It and the helpers
// below (Muted, Accent, Success, Warning, Danger, Fire, Rule, Title,
// Selected, Box) are the semantic chrome styles every view shares.
func (t *Theme) Text() lipgloss.Style { return t.Style(ColorText) }

// Muted returns the style for de-emphasised text.
func (t *Theme) Muted() lipgloss.Style { return t.Style(ColorMuted) }

// Accent returns the style for a highlighted value.
func (t *Theme) Accent() lipgloss.Style { return t.Style(ColorAccent) }

// Success returns the style for a good outcome.
func (t *Theme) Success() lipgloss.Style { return t.Style(ColorSuccess) }

// Warning returns the style for something that needs attention.
func (t *Theme) Warning() lipgloss.Style { return t.Style(ColorWarning) }

// Danger returns the style for a failure.
func (t *Theme) Danger() lipgloss.Style { return t.Style(ColorDanger) }

// Fire returns the cannon sprite's style.
func (t *Theme) Fire() lipgloss.Style { return t.Style(ColorFire) }

// Rule returns the style for separators.
func (t *Theme) Rule() lipgloss.Style { return t.Style(ColorRule) }

// Title returns the emphasised style of headings and overlay titles.
func (t *Theme) Title() lipgloss.Style { return t.Text().Bold(true) }

// Selected returns the style of the row or node under the cursor. It is bold
// as well as coloured so the cursor is visible with NO_COLOR.
func (t *Theme) Selected() lipgloss.Style { return t.Accent().Bold(true) }

// Box returns a rounded border for panels and overlays.
func (t *Theme) Box() lipgloss.Style {
	return t.Base().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorRule.Resolve(t.profile)).
		Padding(0, 1)
}

// StateColor returns the palette entry for a bead state, falling back to the
// muted grey for a status the cannon does not know.
func (t *Theme) StateColor(s domain.BeadState) Color {
	if c, ok := stateColors[s]; ok {
		return c
	}
	return ColorMuted
}

// StateStyle returns the style for a bead state. Terminal states
// (closed, needs_human, failed) are bold, so the states that need a human's
// eye stand out without relying on hue.
func (t *Theme) StateStyle(s domain.BeadState) lipgloss.Style {
	st := t.Style(t.StateColor(s))
	if s.Terminal() {
		st = st.Bold(true)
	}
	return st
}

// StateGlyph returns the one-cell marker for a bead state: the channel that
// survives NO_COLOR and colour blindness. Unknown statuses get
// [UnknownGlyph].
func StateGlyph(s domain.BeadState) string {
	if g, ok := stateGlyphs[s]; ok {
		return g
	}
	return UnknownGlyph
}

// StateLabel renders "<glyph> <status>" in the state's style, e.g. "✓ approved".
func (t *Theme) StateLabel(s domain.BeadState) string {
	return t.StateStyle(s).Render(StateGlyph(s) + " " + s.String())
}

// SeverityColor returns the palette entry for a feedback severity.
func (t *Theme) SeverityColor(s domain.Severity) Color {
	if c, ok := severityColors[s]; ok {
		return c
	}
	return ColorMuted
}

// SeverityStyle returns the style for a feedback severity; blockers are bold.
func (t *Theme) SeverityStyle(s domain.Severity) lipgloss.Style {
	st := t.Style(t.SeverityColor(s))
	if s == domain.SeverityBlocker {
		st = st.Bold(true)
	}
	return st
}

// SeverityGlyph returns the one-cell marker for a feedback severity.
func SeverityGlyph(s domain.Severity) string {
	if g, ok := severityGlyphs[s]; ok {
		return g
	}
	return UnknownGlyph
}

// SeverityLabel renders "<glyph> <severity>" in the severity's style.
func (t *Theme) SeverityLabel(s domain.Severity) string {
	return t.SeverityStyle(s).Render(SeverityGlyph(s) + " " + s.String())
}

// AgentStateColor returns the palette entry for an agent lifecycle state.
func (t *Theme) AgentStateColor(s domain.AgentState) Color {
	if c, ok := agentStateColors[s]; ok {
		return c
	}
	return ColorMuted
}

// AgentStateStyle returns the style for an agent lifecycle state.
func (t *Theme) AgentStateStyle(s domain.AgentState) lipgloss.Style {
	return t.Style(t.AgentStateColor(s))
}

// AgentStateGlyph returns the one-cell marker for an agent lifecycle state.
func AgentStateGlyph(s domain.AgentState) string {
	if g, ok := agentStateGlyphs[s]; ok {
		return g
	}
	return UnknownGlyph
}

// RoleColor returns the palette entry for an agent role.
func (t *Theme) RoleColor(r domain.Role) Color {
	if c, ok := roleColors[r]; ok {
		return c
	}
	return ColorMuted
}

// RoleStyle returns the style for an agent role.
func (t *Theme) RoleStyle(r domain.Role) lipgloss.Style { return t.Style(t.RoleColor(r)) }

// RoleGlyph returns the one-cell marker for an agent role: ◆ product owner,
// ■ engineer, ▲ tester.
func RoleGlyph(r domain.Role) string {
	if g, ok := roleGlyphs[r]; ok {
		return g
	}
	return UnknownGlyph
}

// LensColor returns the palette entry for a tester's lens.
func (t *Theme) LensColor(l domain.Lens) Color {
	if c, ok := lensColors[l]; ok {
		return c
	}
	return ColorMuted
}

// LensStyle returns the style for a tester's lens.
func (t *Theme) LensStyle(l domain.Lens) lipgloss.Style { return t.Style(t.LensColor(l)) }
