package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
)

// P7-01 — the scripted run renders elapsed times against its own timeline.
//
// The script is stamped with a fixed simulated clock ([DemoStart]), so a
// view that read the wall clock would show a live agent as having run for
// the years between that date and today. [Demo] therefore hands the views a
// clock that follows the script.

// demoClockAt returns a scripted event stamped at t.
func demoClockAt(t time.Time) events.Event {
	return events.RunPaused{Meta: events.MetaAt(DemoRunID, t), Reason: "test"}
}

func TestDemoClockStartsAtTheScriptedStart(t *testing.T) {
	t.Parallel()
	c := newDemoClock(DemoStart)
	require.Equal(t, DemoStart, c.Now())
}

func TestDemoClockFollowsTheEventsItIsGiven(t *testing.T) {
	t.Parallel()
	c := newDemoClock(DemoStart)

	c.at(demoClockAt(DemoStart.Add(DemoStep)))
	require.Equal(t, DemoStart.Add(DemoStep), c.Now())

	c.at(demoClockAt(DemoStart.Add(2 * DemoStep)))
	require.Equal(t, DemoStart.Add(2*DemoStep), c.Now())
}

func TestDemoClockNeverGoesBackwards(t *testing.T) {
	t.Parallel()
	c := newDemoClock(DemoStart)
	c.at(demoClockAt(DemoStart.Add(time.Hour)))
	c.at(demoClockAt(DemoStart))
	require.Equal(t, DemoStart.Add(time.Hour), c.Now(),
		"an out-of-order event must not rewind the elapsed times on screen")
}

func TestDemoFillsInTheSimulatedClock(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	var opts Options
	demo := DemoOptions{Delay: -1}
	clock := demoClockFor(demo, &opts, bus)

	require.Same(t, bus, opts.Bus)
	require.Equal(t, DemoRun(demo), opts.Run)
	require.NotNil(t, opts.Now, "without a clock the views measure elapsed against time.Now")
	require.Equal(t, DemoStart, opts.Now())

	// Playing an event moves the clock the views read, so a live agent's
	// elapsed time is a scripted duration, not a gap to the wall clock.
	clock.at(demoClockAt(DemoStart.Add(9 * time.Second)))
	require.Equal(t, DemoStart.Add(9*time.Second), opts.Now())
	require.Less(t, opts.Now().Sub(DemoStart), time.Hour)
}

func TestDemoKeepsAClockTheCallerSupplied(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	pinned := time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)
	opts := Options{Now: func() time.Time { return pinned }}
	demoClockFor(DemoOptions{Delay: -1}, &opts, bus)
	require.Equal(t, pinned, opts.Now(), "the golden frames pin their own clock")
}

func TestDemoPlayScriptReportsEveryEventToTheClock(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	sub := bus.Subscribe(t.Context(), 8)

	script := []ScriptedEvent{
		{Event: demoClockAt(DemoStart)},
		{Event: demoClockAt(DemoStart.Add(DemoStep))},
	}
	var seen []time.Time
	require.NoError(t, playScript(t.Context(), bus, script, func(ev events.Event) { seen = append(seen, ev.At()) }))

	require.Equal(t, []time.Time{DemoStart, DemoStart.Add(DemoStep)}, seen)
	for range script {
		require.NotNil(t, <-sub.Events(), "the hook must not swallow the event")
	}
}
