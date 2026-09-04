package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/help"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// DefaultBuffer is the subscription buffer [New] uses when [Options.Buffer]
// is zero. It is generous because the busiest event by far is
// [events.AgentOutput] and a dropped line is a visible gap in a transcript.
const DefaultBuffer = 2048

// subscription is the part of [events.Subscription] the model needs, so a
// test can drive the update loop from a plain channel.
type subscription interface {
	// Events returns the channel events arrive on, closed when it ends.
	Events() <-chan events.Event
	// Dropped returns how many events this subscriber missed.
	Dropped() uint64
}

// Options configure the TUI program.
type Options struct {
	// Bus is the event bus of the run to watch. [New] subscribes to it for
	// the lifetime of the context it is given. A nil bus renders a static
	// view of Run, which is what the golden-frame tests want.
	Bus *events.Bus
	// Events replaces Bus when a caller already has a stream of events (a
	// recorded JSONL run, a test). Bus wins when both are set.
	Events <-chan events.Event
	// Run describes the run before its first event arrives, so the header is
	// populated from the start. [events.RunStarted] overwrites it.
	Run domain.Run
	// Buffer is the subscription buffer; zero means [DefaultBuffer].
	Buffer int
	// Keys overrides the key bindings; the zero value means
	// [DefaultKeyMap].
	Keys *KeyMap
	// NoAnim disables the cannon animations (--no-anim, SC_NO_ANIM, P7-06).
	NoAnim bool
	// Now is the clock the views read for elapsed times; nil means
	// [time.Now]. The golden-frame tests pin it so a frame is reproducible.
	Now func() time.Time
	// NoColor renders without any ANSI colour (--no-color, NO_COLOR).
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor. Every
	// style the TUI draws with comes from it, so a frame never depends on
	// the global Lip Gloss renderer's profile.
	Theme *Theme
	// Open replaces the OS opener the PRs view's `o` key hands a pull
	// request URL to (P7-05); nil means [OpenURL]. A test passes its own so
	// nothing ever launches a browser.
	Open URLOpener
	// OnStop is called when the user confirms a stop with `q` on a live run.
	// It is how `sc tui-demo` reports the request; a real run also attaches
	// a Controller, whose `s` key stops the orchestrator itself.
	OnStop func()
	// Controller is the orchestrator the run-control keys act on (P7-10):
	// `p` pause/resume, `s` stop, `r` re-fire, `x` mark done, `S` safety and
	// `a` ammo. A nil Controller disables the whole keymap, so a recorded
	// run never offers a key that cannot do anything.
	Controller Controller
	// ControlKeys overrides the run-control bindings; nil means
	// [DefaultControlsKeyMap].
	ControlKeys *ControlsKeyMap
	// Output is where the program draws; nil means os.Stdout.
	Output io.Writer
	// Input is where key presses are read from; nil means os.Stdin.
	Input io.Reader
	// AltScreen puts the program on the alternate screen buffer, leaving the
	// user's scrollback untouched.
	AltScreen bool
	// ProgramOptions are appended to the options [NewProgram] builds, for
	// callers that need something unusual.
	ProgramOptions []tea.ProgramOption
}

// keyMap returns the bindings to use.
func (o Options) keyMap() KeyMap {
	if o.Keys != nil {
		return *o.Keys
	}
	return DefaultKeyMap()
}

// theme returns the palette every part of the TUI renders through: the one
// [Options.Theme] names, or the process theme for [Options.NoColor].
func (o Options) theme() *Theme {
	if o.Theme != nil {
		return o.Theme
	}
	return EnvTheme(o.NoColor)
}

// buffer returns the subscription buffer to use.
func (o Options) buffer() int {
	if o.Buffer > 0 {
		return o.Buffer
	}
	return DefaultBuffer
}

// New builds the model, subscribing to opts.Bus (or reading opts.Events).
// The subscription ends when ctx is done, when the bus is closed, or when
// the program exits; the model then keeps rendering the state it has and
// says "disconnected" in the header.
func New(ctx context.Context, opts Options) Model {
	theme := opts.theme()
	m := Model{
		opts:   opts,
		keys:   opts.keyMap(),
		theme:  theme,
		help:   newHelpModel(theme),
		styles: newStyles(theme),
		state:  NewState(opts.Run),
		follow: true,
	}
	m.graph = NewGraphModel(GraphOptions{NoColor: opts.NoColor, Theme: theme})
	m.agents = NewAgentsModel(AgentsOptions{NoColor: opts.NoColor, Theme: theme, Now: opts.Now})
	m.feedback = NewFeedbackModel(FeedbackOptions{NoColor: opts.NoColor, Theme: theme})
	m.prs = NewPRsModel(PRsOptions{NoColor: opts.NoColor, Theme: theme, Open: opts.Open})
	m.controls = NewControlsModel(ControlsOptions{
		NoColor:    opts.NoColor,
		Theme:      theme,
		Keys:       opts.ControlKeys,
		Controller: opts.Controller,
	})
	m.anim = NewAnimator(AnimOptions{
		Disabled: AnimDisabled(opts.NoAnim, nil),
		NoColor:  opts.NoColor,
		Theme:    theme,
		Tab:      m.tab,
	})
	m.resizeViews()
	switch {
	case opts.Bus != nil:
		m.sub = opts.Bus.Subscribe(ctx, opts.buffer())
	case opts.Events != nil:
		m.sub = chanSubscription{ch: opts.Events}
	}
	return m
}

// NewProgram wires the model into a Bubble Tea program with the input,
// output and screen mode from opts.
func NewProgram(ctx context.Context, opts Options) *tea.Program {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	in := opts.Input
	if in == nil {
		in = os.Stdin
	}
	popts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(out), tea.WithInput(in)}
	if opts.AltScreen {
		popts = append(popts, tea.WithAltScreen())
	}
	popts = append(popts, opts.ProgramOptions...)
	return tea.NewProgram(New(ctx, opts), popts...)
}

// Run renders the run until the user quits or ctx is cancelled. A cancelled
// context is not an error: it is how a signal or a finished orchestrator
// takes the TUI down.
//
// The subscription is made by [New], which Run calls: events published
// before Run starts are not delivered. A caller that also produces the
// events must build the program with [NewProgram] first and publish after.
func Run(ctx context.Context, opts Options) error {
	return runProgram(ctx, NewProgram(ctx, opts))
}

// runProgram runs a built program and maps its error: a program killed by
// its context ended the way the caller asked it to, not in failure.
func runProgram(ctx context.Context, p *tea.Program) error {
	if _, err := p.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// chanSubscription adapts a plain channel to [subscription]; it never
// reports drops because nothing between it and the model can drop.
type chanSubscription struct{ ch <-chan events.Event }

// Events implements [subscription].
func (c chanSubscription) Events() <-chan events.Event { return c.ch }

// Dropped implements [subscription].
func (c chanSubscription) Dropped() uint64 { return 0 }

// newHelpModel builds the bubbles help renderer used by the footer and the
// `?` overlay. Every style comes from t, so the help line renders for t's
// colour profile rather than for whatever profile the global Lip Gloss
// renderer happened to detect (P7-09).
func newHelpModel(t *Theme) help.Model {
	h := help.New()
	h.ShowAll = false
	sep, key, desc := t.Muted(), t.Accent(), t.Text()
	h.Styles = help.Styles{
		Ellipsis:       sep,
		ShortKey:       key,
		ShortDesc:      desc,
		ShortSeparator: sep,
		FullKey:        key,
		FullDesc:       desc,
		FullSeparator:  sep,
	}
	return h
}

// styles names the chrome the [Model] itself draws: the header, the tab bar,
// the rule, the cursor, the dropped-event notice and the two overlays. Every
// entry is a semantic style of the [Theme] (P7-07), so the chrome is drawn
// from the same colour-blind-safe palette as the views and pins the same
// renderer profile.
type styles struct {
	// Title is the emphasised text of the header and the overlays.
	Title lipgloss.Style
	// Value is a highlighted value inside a label/value pair.
	Value lipgloss.Style
	// Sep is the separator between header fields.
	Sep lipgloss.Style
	// Cannon is the header's cannon sprite.
	Cannon lipgloss.Style
	// Status is the run status word in the header.
	Status lipgloss.Style
	// Tab is an inactive tab label.
	Tab lipgloss.Style
	// TabActive is the current tab's label. It is bold rather than
	// underlined: Lip Gloss underlines each space with its own escape run,
	// which both bloats a golden frame and confuses width measurement, and
	// bold already separates it from the muted inactive labels with colour
	// and without.
	TabActive lipgloss.Style
	// Rule is the line between the tab bar and the main pane.
	Rule lipgloss.Style
	// Cursor marks the selected row.
	Cursor lipgloss.Style
	// Muted is de-emphasised text: empty views, dependency lists.
	Muted lipgloss.Style
	// Warn is the dropped-event notice.
	Warn lipgloss.Style
	// Box frames the help and confirmation overlays.
	Box lipgloss.Style
}

// newStyles returns the chrome palette, bound to t's renderer. Nothing here
// may call [lipgloss.NewStyle]: that style would use the global renderer and
// so the frame would depend on the ambient terminal (P7-09).
func newStyles(t *Theme) styles {
	return styles{
		Title:     t.Title(),
		Value:     t.Accent(),
		Sep:       t.Muted(),
		Cannon:    t.Fire(),
		Status:    t.Warning(),
		Tab:       t.Muted(),
		TabActive: t.Title(),
		Rule:      t.Rule(),
		Cursor:    t.Accent(),
		Muted:     t.Muted(),
		Warn:      t.Warning(),
		Box:       t.Box().Padding(0, 2),
	}
}
