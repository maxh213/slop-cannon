package tui

import (
	"strconv"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// KeyMap is every key binding of the TUI. It is a value so a caller can copy
// the default and rebind a key before handing it to [Options.Keys]; later
// tasks (P7-10) add the run-control keys to it rather than reading keys
// directly out of [tea.KeyMsg].
type KeyMap struct {
	// NextTab moves one view to the right (tab, l, →).
	NextTab key.Binding
	// PrevTab moves one view to the left (shift+tab, h, ←).
	PrevTab key.Binding
	// SelectTab jumps straight to a view by number (1…5).
	SelectTab key.Binding
	// Up moves the cursor up inside the current view.
	Up key.Binding
	// Down moves the cursor down inside the current view.
	Down key.Binding
	// Help toggles the help overlay.
	Help key.Binding
	// Quit stops a live run (after a confirmation) or leaves the TUI.
	Quit key.Binding
	// ForceQuit leaves immediately, without confirming.
	ForceQuit key.Binding
	// Confirm answers a confirmation prompt with yes.
	Confirm key.Binding
	// Cancel answers a confirmation prompt with no, and closes the overlay.
	Cancel key.Binding
}

// DefaultKeyMap returns the standard bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		NextTab: key.NewBinding(
			key.WithKeys("tab", "right", "l"),
			key.WithHelp("tab", "next view"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("shift+tab", "left", "h"),
			key.WithHelp("shift+tab", "previous view"),
		),
		SelectTab: key.NewBinding(
			key.WithKeys("1", "2", "3", "4", "5"),
			key.WithHelp("1…5", "jump to view"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q"),
			key.WithHelp("q", "stop / quit"),
		),
		ForceQuit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit now"),
		),
		Confirm: key.NewBinding(
			key.WithKeys("y", "enter"),
			key.WithHelp("y", "yes"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("n", "esc"),
			key.WithHelp("n/esc", "no"),
		),
	}
}

// ShortHelp implements the [help.KeyMap] interface: the one-line footer.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.NextTab, k.SelectTab, k.Up, k.Down, k.Help, k.Quit}
}

// FullHelp implements the [help.KeyMap] interface: the columns of the `?`
// overlay.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.NextTab, k.PrevTab, k.SelectTab},
		{k.Up, k.Down},
		{k.Help, k.Quit, k.ForceQuit},
	}
}

// tabFor reports which view a number key selects. It returns false for any
// other key, and for a number with no view behind it.
func tabFor(msg tea.KeyMsg, k KeyMap) (Tab, bool) {
	if !key.Matches(msg, k.SelectTab) {
		return 0, false
	}
	n, err := strconv.Atoi(msg.String())
	if err != nil || n < 1 || n > len(Tabs()) {
		return 0, false
	}
	return Tabs()[n-1], true
}
