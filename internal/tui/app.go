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
	// OnStop is called when the user confirms a stop. P7-10 points it at the
	// orchestrator; until then it is how `sc tui-demo` reports the request.
	OnStop func()
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
	m := Model{
		opts:   opts,
		keys:   opts.keyMap(),
		help:   newHelpModel(opts.NoColor),
		styles: newStyles(opts.NoColor),
		state:  NewState(opts.Run),
		follow: true,
	}
	m.graph = NewGraphModel(GraphOptions{NoColor: opts.NoColor})
	m.agents = NewAgentsModel(AgentsOptions{NoColor: opts.NoColor, Now: opts.Now})
	m.feedback = NewFeedbackModel(FeedbackOptions{NoColor: opts.NoColor})
	m.prs = NewPRsModel(PRsOptions{NoColor: opts.NoColor})
	m.anim = NewAnimator(AnimOptions{
		Disabled: AnimDisabled(opts.NoAnim, nil),
		NoColor:  opts.NoColor,
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
// `?` overlay, stripped of colour when the caller asked for none.
func newHelpModel(noColor bool) help.Model {
	h := help.New()
	h.ShowAll = false
	if noColor {
		plain := lipgloss.NewStyle()
		h.Styles = help.Styles{
			Ellipsis:       plain,
			ShortKey:       plain,
			ShortDesc:      plain,
			ShortSeparator: plain,
			FullKey:        plain,
			FullDesc:       plain,
			FullSeparator:  plain,
		}
	}
	return h
}

// styles is the TUI's Lip Gloss palette. P7-07 replaces it with the full
// colour-blind-safe theme; the skeleton needs only the chrome.
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
	// TabActive is the current tab's label.
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

// newStyles returns the palette. With noColor every style is plain, so the
// rendering is identical to a NO_COLOR terminal.
func newStyles(noColor bool) styles {
	if noColor {
		plain := lipgloss.NewStyle()
		return styles{
			Title: plain.Bold(true), Value: plain, Sep: plain, Cannon: plain, Status: plain,
			Tab: plain, TabActive: plain.Bold(true).Underline(true), Rule: plain,
			Cursor: plain, Muted: plain, Warn: plain,
			Box: plain.Border(lipgloss.RoundedBorder()).Padding(0, 2),
		}
	}
	const (
		fg     = lipgloss.Color("15")
		accent = lipgloss.Color("12")
		dim    = lipgloss.Color("8")
		warn   = lipgloss.Color("11")
		fire   = lipgloss.Color("9")
	)
	return styles{
		Title:     lipgloss.NewStyle().Bold(true).Foreground(fg),
		Value:     lipgloss.NewStyle().Foreground(accent),
		Sep:       lipgloss.NewStyle().Foreground(dim),
		Cannon:    lipgloss.NewStyle().Foreground(fire),
		Status:    lipgloss.NewStyle().Foreground(warn),
		Tab:       lipgloss.NewStyle().Foreground(dim),
		TabActive: lipgloss.NewStyle().Bold(true).Foreground(fg).Underline(true),
		Rule:      lipgloss.NewStyle().Foreground(dim),
		Cursor:    lipgloss.NewStyle().Foreground(accent),
		Muted:     lipgloss.NewStyle().Foreground(dim),
		Warn:      lipgloss.NewStyle().Foreground(warn),
		Box:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 2),
	}
}
