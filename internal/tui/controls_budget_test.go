package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
)

// The `b` key: raising the budget of a run a cap stopped (P9-14).
//
// An autopilot run paused on `limits.max_tokens` or `limits.max_cost_usd`
// could be resumed into another grant of the same size or stopped, and
// nothing else: the cap is read once, at start-up, from the configuration
// the run was frozen with. This is the third answer, and the only one that
// makes the pause banner actionable where the user is looking at it.

// budgetController is a [Controller] that can also raise a budget.
type budgetController struct {
	ctlController
	mu     sync.Mutex
	raised []float64
	err    error
}

func (c *budgetController) SetBudget(_ context.Context, amount float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raised = append(c.raised, amount)
	return c.err
}

func (c *budgetController) amounts() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float64(nil), c.raised...)
}

// budgetPaused is the run the key is pressed on: paused over a cap, with
// the pause reason the budget gave it.
func budgetPaused(t *testing.T, reason string) *State {
	t.Helper()
	st := ctlState(t, true, 10)
	st.Apply(events.RunPaused{Meta: events.MetaAt(ctlRunID, ctlAt(5)), Reason: reason})
	return st
}

func TestBudgetKeyRaisesTheCapThatStoppedTheRun(t *testing.T) {
	t.Parallel()
	c := &budgetController{}
	st := budgetPaused(t, "token_cap")

	v, _, ok := ctlPress(t, ctlModel(c), st, "b", ControlTarget{})
	require.True(t, ok, "`b` must be consumed")
	require.True(t, v.Prompting(), "`b` opens the budget prompt")
	require.Equal(t, ControlBudget, v.PromptFor())
	require.Empty(t, v.AmmoInput(), "the prompt starts empty: the TUI does not know the run's cap")

	overlay, drawn := v.Overlay(st, 60, 20)
	require.True(t, drawn)
	require.Contains(t, overlay, "Raise the run's budget")
	require.Contains(t, overlay, "limits.max_tokens")

	v, res := ctlType(t, v, st, ControlTarget{}, "2", "0", "0", "0", "0", "0", "enter")
	require.NotNil(t, res)
	require.NoError(t, res.Err)
	require.Equal(t, []float64{200000}, c.amounts())
	require.False(t, v.Prompting(), "applying closes the prompt")
	flash, isErr := v.Flash()
	require.False(t, isErr)
	require.Equal(t, "budget raised to 200000 tokens", flash)
}

func TestBudgetKeyAsksForDollarsWhenACostCapStoppedTheRun(t *testing.T) {
	t.Parallel()
	c := &budgetController{}
	st := budgetPaused(t, ControlBudgetCostReason)

	v, _, _ := ctlPress(t, ctlModel(c), st, "b", ControlTarget{})
	overlay, drawn := v.Overlay(st, 60, 20)
	require.True(t, drawn)
	require.Contains(t, overlay, "limits.max_cost_usd")

	_, res := ctlType(t, v, st, ControlTarget{}, "2", "5", "enter")
	require.NotNil(t, res)
	require.Equal(t, []float64{25}, c.amounts())
	require.Equal(t, "budget raised to $25.00", res.Request.Summary())
}

func TestBudgetPromptRefusesWhatIsNotANumber(t *testing.T) {
	t.Parallel()
	c := &budgetController{}
	st := budgetPaused(t, "token_cap")

	// Enter on an empty field says so and keeps the prompt up.
	v, res := ctlType(t, ctlModel(c), st, ControlTarget{}, "b", "enter")
	require.Nil(t, res, "an empty field calls nothing")
	require.True(t, v.Prompting())
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Equal(t, "a budget must be a number above zero", flash)

	// Letters are swallowed, digits are kept, and backspace edits.
	v, _ = ctlType(t, v, st, ControlTarget{}, "x", "5", "0", "0", "backspace")
	require.Equal(t, "50", v.AmmoInput())

	// Escape closes it without calling anything.
	v, _ = ctlType(t, v, st, ControlTarget{}, "esc")
	require.False(t, v.Prompting())
	require.Empty(t, c.amounts())
	flash, isErr = v.Flash()
	require.False(t, isErr)
	require.Equal(t, "cancelled", flash)
}

// TestBudgetKeySaysSoWhenTheRunCannotBeRaised: a recorded run and the demo
// attach a controller that is not a [BudgetController], and the key must
// report that rather than appear to work.
func TestBudgetKeySaysSoWhenTheRunCannotBeRaised(t *testing.T) {
	t.Parallel()
	c := &ctlController{} // no SetBudget
	st := budgetPaused(t, "token_cap")

	v, res := ctlType(t, ctlModel(c), st, ControlTarget{}, "b", "1", "0", "0", "0", "enter")
	require.NotNil(t, res)
	require.Error(t, res.Err)
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Contains(t, flash, "could not raise the budget to 1000 tokens")
	require.Contains(t, flash, "cannot be raised from here")
}

// TestBudgetKeyIsRefusedOnARunThatHasEnded is the refusal every control key
// shares: there is nothing to raise once the run is over.
func TestBudgetKeyIsRefusedOnARunThatHasEnded(t *testing.T) {
	t.Parallel()
	c := &budgetController{}
	st := ctlState(t, true, 10)
	st.Apply(events.RunFinished{
		Meta: events.MetaAt(ctlRunID, ctlAt(9)), Reason: events.FinishBudgetReached,
		Summary: "the run has spent its budget",
	})

	v, _, ok := ctlPress(t, ctlModel(c), st, "b", ControlTarget{})
	require.True(t, ok, "the key is consumed even when it refuses")
	require.False(t, v.Prompting())
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Equal(t, "the run has finished", flash)
	require.Empty(t, c.amounts())
}

// TestBudgetKeyIsInTheHelp: a key nothing advertises is a key nobody presses.
func TestBudgetKeyIsInTheHelp(t *testing.T) {
	t.Parallel()
	keys := DefaultControlsKeyMap()
	var found bool
	for _, col := range keys.FullHelp() {
		for _, b := range col {
			if b.Help().Key == "b" {
				found = true
				require.Equal(t, "raise budget", b.Help().Desc)
			}
		}
	}
	require.True(t, found, "the `?` overlay must list the budget key")
	require.True(t, strings.Contains(strings.Join(budgetActionNames(), " "), "budget"),
		"ControlActions must name the budget action")
	require.False(t, ControlBudget.Confirms(), "the prompt is the confirmation")
}

func budgetActionNames() []string {
	var out []string
	for _, a := range ControlActions() {
		out = append(out, a.String())
	}
	return out
}
