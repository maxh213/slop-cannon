package tui_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

func TestDemoScriptCoversEveryEventKind(t *testing.T) {
	t.Parallel()
	seen := make(map[events.Kind]int)
	for _, ev := range tui.DemoEvents(tui.DemoOptions{}) {
		seen[ev.Kind()]++
	}
	for _, k := range events.Kinds() {
		require.Positive(t, seen[k], "the script never publishes %s", k)
	}
}

func TestDemoScriptIsDeterministic(t *testing.T) {
	t.Parallel()
	render := func() []byte {
		var buf bytes.Buffer
		w := events.NewJSONLWriter(&buf)
		for _, ev := range tui.DemoEvents(tui.DemoOptions{}) {
			require.NoError(t, w.Write(ev))
		}
		require.NoError(t, w.Err())
		return buf.Bytes()
	}
	require.Equal(t, string(render()), string(render()))
}

func TestDemoScriptEventsAreWellFormed(t *testing.T) {
	t.Parallel()
	script := tui.DemoScript(tui.DemoOptions{})
	require.NotEmpty(t, script)
	require.Zero(t, script[0].Delay, "the first event lands immediately")

	var last time.Time
	for i, s := range script {
		ev := s.Event
		require.Equal(t, tui.DemoRunID, ev.RunID(), "event %d", i)
		require.Equal(t, time.UTC, ev.At().Location(), "event %d", i)
		require.False(t, ev.At().Before(last), "event %d goes back in time", i)
		last = ev.At()
		if i > 0 {
			require.Equal(t, tui.DemoDelay, s.Delay, "event %d", i)
		}
	}
	require.Equal(t, tui.DemoStart, script[0].Event.At())
}

func TestDemoScriptRoundTripsThroughJSONL(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := events.NewJSONLWriter(&buf)
	want := tui.DemoEvents(tui.DemoOptions{})
	for _, ev := range want {
		require.NoError(t, w.Write(ev))
	}
	got, err := events.ReadAll(&buf)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDemoOptionsAreHonoured(t *testing.T) {
	t.Parallel()
	opts := tui.DemoOptions{
		RunID:    "run-42",
		Start:    time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC),
		Step:     time.Minute,
		Delay:    -1,
		Ammo:     6,
		Safety:   true,
		Brief:    "make it faster",
		RepoPath: "/srv/repo",
	}
	run := tui.DemoRun(opts)
	require.Equal(t, domain.RunID("run-42"), run.ID)
	require.Equal(t, domain.RunModeBrief, run.Mode)
	require.Equal(t, "make it faster", run.Brief)
	require.Equal(t, "/srv/repo", run.RepoPath)
	require.Equal(t, 6, run.Config.Ammo)
	require.True(t, run.Config.Safety)
	require.Equal(t, opts.Start, run.StartedAt)

	script := tui.DemoScript(opts)
	require.Equal(t, opts.Start, script[0].Event.At())
	require.Equal(t, opts.Start.Add(time.Minute), script[1].Event.At())
	for _, s := range script {
		require.Zero(t, s.Delay, "a negative delay means no pause at all")
		require.Equal(t, domain.RunID("run-42"), s.Event.RunID())
	}
	require.Equal(t, domain.RunModeAutopilot, tui.DemoRun(tui.DemoOptions{}).Mode, "no brief means autopilot")
}

func TestDemoOptionsNoStep(t *testing.T) {
	t.Parallel()
	script := tui.DemoScript(tui.DemoOptions{Step: -1})
	for _, s := range script {
		require.Equal(t, tui.DemoStart, s.Event.At())
	}
}

func TestPlayScriptPublishesEveryEventInOrder(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	defer bus.Close()
	script := tui.DemoScript(tui.DemoOptions{Delay: -1})
	sub := bus.Subscribe(t.Context(), len(script))

	require.NoError(t, tui.PlayScript(t.Context(), bus, script))
	bus.Close()

	var got []events.Event
	for ev := range sub.Events() {
		got = append(got, ev)
	}
	require.Equal(t, tui.DemoEvents(tui.DemoOptions{}), got)
	require.Zero(t, sub.Dropped())
}

func TestPlayScriptStopsOnContextCancellation(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(t.Context(), 16)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- tui.PlayScript(ctx, bus, tui.DemoScript(tui.DemoOptions{Delay: time.Hour})) }()

	// The first event has no delay, so it always lands; the second waits an
	// hour, which the cancellation cuts short.
	select {
	case ev := <-sub.Events():
		require.Equal(t, events.KindRunStarted, ev.Kind())
	case <-time.After(5 * time.Second):
		t.Fatal("the first event never arrived")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("PlayScript ignored the cancellation")
	}
}

func TestPlayScriptWithoutABusIsANoOp(t *testing.T) {
	t.Parallel()
	require.NoError(t, tui.PlayScript(t.Context(), nil, tui.DemoScript(tui.DemoOptions{Delay: -1})))
}
