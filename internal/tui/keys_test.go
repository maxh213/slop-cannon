package tui_test

import (
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/tui"
)

func TestKeysDefaultKeyMapIsComplete(t *testing.T) {
	t.Parallel()
	k := tui.DefaultKeyMap()
	bindings := map[string]key.Binding{
		"NextTab": k.NextTab, "PrevTab": k.PrevTab, "SelectTab": k.SelectTab,
		"Up": k.Up, "Down": k.Down, "Help": k.Help, "Quit": k.Quit,
		"ForceQuit": k.ForceQuit, "Confirm": k.Confirm, "Cancel": k.Cancel,
	}
	for name, b := range bindings {
		require.NotEmpty(t, b.Keys(), "%s has no keys", name)
		require.True(t, b.Enabled(), "%s is disabled", name)
		require.NotEmpty(t, b.Help().Key, "%s has no help key", name)
		require.NotEmpty(t, b.Help().Desc, "%s has no help description", name)
	}
	require.NotEmpty(t, k.ShortHelp())
	require.NotEmpty(t, k.FullHelp())
	for _, col := range k.FullHelp() {
		require.NotEmpty(t, col)
	}
}

func TestKeysSelectTabHasOneKeyPerView(t *testing.T) {
	t.Parallel()
	require.Len(t, tui.DefaultKeyMap().SelectTab.Keys(), len(tui.Tabs()))
}

func TestKeysNumbersSelectTheMatchingView(t *testing.T) {
	t.Parallel()
	for i, want := range tui.Tabs() {
		m := tuiNewModel(t, nil)
		got := tuiUpdate(m, tuiKey(string(rune('1'+i))))
		require.Equal(t, want, got.Tab(), "key %d", i+1)
	}
}

func TestKeysUnboundNumberIsIgnored(t *testing.T) {
	t.Parallel()
	m := tuiNewModel(t, nil)
	got := tuiUpdate(m, tuiKey("9"))
	require.Equal(t, tui.TabGraph, got.Tab())
}

func TestKeysCustomKeyMapIsUsed(t *testing.T) {
	t.Parallel()
	custom := tui.DefaultKeyMap()
	custom.Help = key.NewBinding(key.WithKeys("F1"), key.WithHelp("F1", "help"))
	m := tui.New(t.Context(), tui.Options{Keys: &custom, NoColor: true})
	require.Equal(t, custom.Help.Keys(), m.Keys().Help.Keys())

	got := tuiUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F1")})
	require.True(t, got.HelpVisible())
	got = tuiUpdate(got, tuiKey("?"))
	require.True(t, got.HelpVisible(), "the default key no longer toggles help")
}
