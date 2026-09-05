package tui_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/tui"
)

// P9-14 inside the program that ships: `b` on a run held at its cap has to
// reach the orchestrator through [tui.Model], not only through the control
// layer on its own. The layer's own tests are in controls_budget_test.go;
// this is the wiring — the dispatch ahead of the views, the prompt overlay
// and the footer fold — driven the way P7-10's round-2 tests drive the
// other six keys.

// cwBudgetController is [cwController] plus the optional half of the
// interface a live run implements ([tui.BudgetController]).
type cwBudgetController struct{ cwController }

var _ tui.BudgetController = (*cwBudgetController)(nil)

func (c *cwBudgetController) SetBudget(_ context.Context, amount float64) error {
	return c.record("budget %.0f", amount)
}

func TestModelRaisesTheBudgetFromTheBudgetKey(t *testing.T) {
	t.Parallel()
	c := &cwBudgetController{}

	prompt := cwPress(t, cwModel(t, c), "b")
	frame := cwFrame(prompt)
	require.Contains(t, frame, "Raise the run's budget")
	require.Contains(t, frame, "enter apply")
	require.Contains(t, frame, "Graph", "the tab bar is still drawn under the overlay")
	require.Empty(t, c.Calls(), "the prompt asks before it calls anything")

	// The overlay is modal, exactly as the ammo prompt is.
	require.Contains(t, cwFrame(cwPress(t, prompt, "4")), "Raise the run's budget")

	done := cwPress(t, prompt, "5", "0", "0", "0", "0", "enter")
	require.Equal(t, []string{"budget 50000"}, c.Calls())
	require.Contains(t, cwFooter(done), "budget raised to 50000 tokens")
	require.NotContains(t, cwFrame(done), "Raise the run's budget", "applying closes the prompt")
}

// TestModelSaysWhenARunsBudgetCannotBeRaised: a controller with no
// SetBudget — a recorded run, `sc tui-demo` — refuses in the footer rather
// than silently doing nothing.
func TestModelSaysWhenARunsBudgetCannotBeRaised(t *testing.T) {
	t.Parallel()
	c := &cwController{}

	done := cwPress(t, cwModel(t, c), "b", "9", "9", "enter")
	require.Empty(t, c.Calls())
	require.Contains(t, cwFooter(done), "could not raise the budget to 99 tokens")
}
