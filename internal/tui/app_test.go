package tui_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// tuiWaitTimeout is how long a teatest expectation may take. It is generous
// because a loaded CI box renders slowly, not because anything here is slow.
const tuiWaitTimeout = 10 * time.Second

// tuiTerm drives a program under teatest. teatest's own reader is drained by
// every WaitFor, so this keeps one accumulating copy of the output and every
// expectation is checked against everything drawn so far.
type tuiTerm struct {
	t   *testing.T
	tm  *teatest.TestModel
	buf bytes.Buffer
}

// tuiStart runs the model in a 120×24 terminal.
func tuiStart(t *testing.T, m tui.Model) *tuiTerm {
	t.Helper()
	return &tuiTerm{t: t, tm: teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 24))}
}

// drain copies whatever the program has drawn since the last call.
func (x *tuiTerm) drain() string {
	if _, err := io.Copy(&x.buf, x.tm.Output()); err != nil {
		x.t.Fatalf("reading the program output: %v", err)
	}
	return x.buf.String()
}

// wait blocks until every substring has appeared in the output.
func (x *tuiTerm) wait(want ...string) {
	x.t.Helper()
	deadline := time.Now().Add(tuiWaitTimeout)
	for {
		out := x.drain()
		missing := ""
		for _, w := range want {
			if !strings.Contains(out, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("waited %s for %q; last output:\n%s", tuiWaitTimeout, missing, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// key sends one key press.
func (x *tuiTerm) key(s string) { x.tm.Send(tuiKey(s)) }

// finalModel waits for the program to exit and returns the model it left.
func (x *tuiTerm) finalModel() tui.Model {
	x.t.Helper()
	x.tm.WaitFinished(x.t, teatest.WithFinalTimeout(tuiWaitTimeout))
	m, ok := x.tm.FinalModel(x.t).(tui.Model)
	require.True(x.t, ok, "the final model is not a tui.Model")
	return m
}

// TestAppSmoke is the P7-01 smoke test: a real Bubble Tea program driven by
// the scripted event stream, navigated with the keyboard and quit cleanly.
func TestAppSmoke(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	demo := tui.DemoOptions{Delay: -1}
	term := tuiStart(t, tui.New(t.Context(), tui.Options{
		Bus:     bus,
		Run:     tui.DemoRun(demo),
		NoColor: true,
	}))

	// The header and the tab bar are up before a single event lands.
	term.wait("slop-cannon", string(tui.DemoRunID), "autopilot", "ammo 0/10", "safety off",
		"Graph", "Agents", "Feedback", "PRs", "Log", "q stop / quit")

	for _, ev := range tui.DemoEvents(demo) {
		bus.Publish(ev)
	}

	// The graph view lists the beads the plan created…
	term.wait("demo-2", "Retry failed HTTP", "closed")
	// …the agents view the agents that were fired…
	term.key("2")
	term.wait("HISTORY", "engineer", "tester/qa")
	// …the feedback view the review rounds…
	term.key("3")
	term.wait("round 1", "changes", "approve")
	// …the PRs view the delivery…
	term.key("4")
	term.wait("#42", "merged")
	// …and the log view follows the stream to its end.
	term.key("5")
	term.wait("run finished")

	// The help overlay opens on `?`.
	term.key("?")
	term.wait("Keys", "previous view", "quit now")
	term.key("esc")

	// The run has finished, so `q` leaves without asking.
	term.key("q")
	final := term.finalModel()
	require.Equal(t, tui.TabLog, final.Tab())
	require.False(t, final.HelpVisible())
	require.False(t, final.ConfirmingStop())
	require.False(t, final.State().Live)
	require.Equal(t, len(tui.DemoEvents(demo)), final.State().Events)
	require.Zero(t, final.State().Dropped)
}

// TestAppSmokeStopConfirmation covers the other half of `q`: a live run is
// stopped only after the user confirms.
func TestAppSmokeStopConfirmation(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	var stops atomic.Int64
	term := tuiStart(t, tui.New(t.Context(), tui.Options{
		Bus:     bus,
		Run:     tui.DemoRun(tui.DemoOptions{}),
		NoColor: true,
		OnStop:  func() { stops.Add(1) },
	}))

	bus.Publish(events.RunStarted{Meta: events.MetaAt(tui.DemoRunID, tui.DemoStart), Info: tui.DemoRun(tui.DemoOptions{})})
	bus.Publish(events.AgentSpawned{
		Meta:  events.MetaAt(tui.DemoRunID, tui.DemoStart),
		Agent: domain.Agent{ID: "eng-1", Role: domain.RoleEngineer, Bead: "demo-2", State: domain.AgentStateRunning},
		Task:  events.TaskImplement,
	})
	term.wait("ammo 1/10")

	term.key("q")
	term.wait("Stop run "+string(tui.DemoRunID)+"?", "1 agent(s) will be killed", "y stop", "n keep firing")
	require.Zero(t, stops.Load(), "asking must not stop anything")

	term.key("y")
	final := term.finalModel()
	require.Equal(t, int64(1), stops.Load())
	require.False(t, final.ConfirmingStop())
	require.True(t, final.State().Live, "the TUI does not decide the run is over; the orchestrator does")
}

// TestAppSubscriptionEndsWithTheContext proves the TUI keeps rendering after
// its subscription is gone, and says so in the header.
func TestAppSubscriptionEndsWithTheContext(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	ctx, cancel := context.WithCancel(t.Context())
	term := tuiStart(t, tui.New(ctx, tui.Options{Bus: bus, Run: tui.DemoRun(tui.DemoOptions{}), NoColor: true}))
	term.wait("slop-cannon")

	cancel()
	term.wait("disconnected")

	term.key("q")
	term.wait("Stop run")
	term.key("y")
	term.finalModel()
}

func TestAppNewWithoutAStreamStillRenders(t *testing.T) {
	t.Parallel()
	m := tui.New(t.Context(), tui.Options{NoColor: true})
	require.Nil(t, m.Init(), "there is nothing to wait for")
	require.NotEmpty(t, m.View())
	require.Equal(t, tui.DefaultKeyMap().Quit.Keys(), m.Keys().Quit.Keys())
}

func TestAppNewFromAPlainChannel(t *testing.T) {
	t.Parallel()
	ch := make(chan events.Event, 2)
	ch <- events.Warning{Meta: events.NewMeta("r"), Source: "test", Message: "watch out"}
	close(ch)

	m := tui.New(t.Context(), tui.Options{Events: ch, NoColor: true})
	msg := m.Init()()
	ev, ok := msg.(tui.EventMsg)
	require.True(t, ok)
	require.Equal(t, events.KindWarning, ev.Event.Kind())
	require.Zero(t, ev.Dropped)

	next, cmd := m.Update(msg)
	require.NotNil(t, cmd)
	require.IsType(t, tui.StreamClosedMsg{}, cmd(), "a closed channel ends the stream")
	require.Len(t, tuiModel(next).State().Notices, 1)
}

// TestAppDemoStopsWithItsContext runs the whole program, not just the model,
// and proves a cancelled context takes it down without an error.
func TestAppDemoStopsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var out tuiSyncBuffer
	in, inWriter := io.Pipe()
	t.Cleanup(func() { _ = inWriter.Close() })

	done := make(chan error, 1)
	go func() {
		done <- tui.Demo(ctx, tui.DemoOptions{Delay: -1}, tui.Options{
			Output:  &out,
			Input:   in,
			NoColor: true,
		})
	}()

	require.Eventually(t, func() bool { return strings.Contains(out.String(), "Retry failed HTTP") },
		tuiWaitTimeout, 10*time.Millisecond, "the scripted run never reached the graph view")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled context is not a failure")
	case <-time.After(tuiWaitTimeout):
		t.Fatal("Demo ignored the cancellation")
	}
}

// tuiSyncBuffer is a bytes.Buffer safe to read while the program writes.
type tuiSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *tuiSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *tuiSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// compile-time proof that the model satisfies the Bubble Tea interface.
var _ tea.Model = tui.Model{}
