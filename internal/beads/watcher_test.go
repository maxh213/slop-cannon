package beads

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// watcherClock is a Clock that only moves when the test advances it. Tick
// waits for the watcher to be sleeping (a registered After) before moving
// time, so a test never advances past a wake-up the watcher has not yet
// asked for.
type watcherClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []watcherWaiter
}

type watcherWaiter struct {
	at time.Time
	ch chan time.Time
}

func newWatcherClock(start time.Time) *watcherClock {
	c := &watcherClock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *watcherClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *watcherClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waiters = append(c.waiters, watcherWaiter{at: c.now.Add(d), ch: ch})
	c.cond.Broadcast()
	return ch
}

// Tick waits until a sleeper is registered, then advances the clock by d
// and fires every sleeper whose deadline passed.
func (c *watcherClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := time.AfterFunc(5*time.Second, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer deadline.Stop()
	start := time.Now()
	for len(c.waiters) == 0 {
		if time.Since(start) > 5*time.Second {
			t.Fatal("no sleeper registered within 5s")
		}
		c.cond.Wait()
	}
	c.now = c.now.Add(d)
	var keep []watcherWaiter
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
}

// watcherLister replays scripted listings: each List call returns the
// next entry (the last one repeats) and reports on Calls.
type watcherLister struct {
	mu      sync.Mutex
	script  []watcherListing
	index   int
	filters []ListFilter
	Calls   chan struct{}
}

type watcherListing struct {
	issues []Issue
	err    error
}

func newWatcherLister(script ...watcherListing) *watcherLister {
	return &watcherLister{script: script, Calls: make(chan struct{}, 64)}
}

func (l *watcherLister) List(_ context.Context, f ListFilter) ([]Issue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.filters = append(l.filters, f)
	entry := l.script[len(l.script)-1]
	if l.index < len(l.script) {
		entry = l.script[l.index]
		l.index++
	}
	l.Calls <- struct{}{}
	return append([]Issue(nil), entry.issues...), entry.err
}

func (l *watcherLister) Filters() []ListFilter {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ListFilter(nil), l.filters...)
}

// watcherWait receives from ch or fails after a short deadline.
func watcherWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		require.True(t, ok, "%s: channel closed", what)
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// watcherNothing asserts nothing arrives on ch for a short while.
func watcherNothing[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected %s: %+v", what, v)
	case <-time.After(50 * time.Millisecond):
	}
}

var watcherT0 = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// watcherIssue builds a listing entry updated at t0 plus n seconds.
func watcherIssue(id string, status domain.BeadState, n int, blockers ...string) Issue {
	is := graphIssue(id, status, blockers...)
	is.UpdatedAt = watcherT0.Add(time.Duration(n) * time.Second)
	return is
}

func watcherEpic(id string) Issue {
	is := graphIssue(id, domain.BeadStateOpen)
	is.Type = string(domain.BeadTypeEpic)
	is.Metadata = Metadata{MetaRun: "r1", MetaKind: KindEpic}
	return is
}

func TestWatcherDiffIssues(t *testing.T) {
	prev := []Issue{
		watcherIssue("b-1", domain.BeadStateOpen, 0),
		watcherIssue("b-2", domain.BeadStateOpen, 0),
		watcherIssue("b-3", domain.BeadStateOpen, 0),
		watcherIssue("b-4", domain.BeadStateOpen, 0),
		watcherIssue("b-5", domain.BeadStateOpen, 0, "b-1"),
		watcherIssue("b-6", domain.BeadStateOpen, 0),
	}
	next := []Issue{
		watcherIssue("b-6", domain.BeadStateOpen, 0),               // untouched
		watcherIssue("b-1", domain.BeadStateOpen, 5),               // updated_at moved
		watcherIssue("b-2", domain.BeadStateInProgress, 0),         // status moved, updated_at not
		watcherIssue("b-3", domain.BeadStateOpen, 0),               // assignee set below
		watcherIssue("b-5", domain.BeadStateOpen, 0, "b-1", "b-6"), // blocker added
		watcherIssue("b-9", domain.BeadStateOpen, 0),               // new
		watcherIssue("b-8", domain.BeadStateOpen, 0),               // new
		watcherIssue("b-8", domain.BeadStateOpen, 0),               // duplicate row is ignored
	}
	next[3].Assignee = "sc:engineer-1"

	ch := DiffIssues(prev, next)
	assert.Equal(t, graphIDs("b-8", "b-9"), ch.Added)
	assert.Equal(t, graphIDs("b-1", "b-2", "b-3", "b-5"), ch.Changed)
	assert.Equal(t, graphIDs("b-4"), ch.Removed)
	assert.Equal(t, graphIDs("b-1", "b-2", "b-3", "b-4", "b-5", "b-8", "b-9"), ch.IDs())
	assert.False(t, ch.Empty())
	assert.False(t, ch.Initial)
	assert.Len(t, ch.Issues, len(next))
	assert.Equal(t, 7, ch.Graph().Len())

	same := DiffIssues(next, next)
	assert.True(t, same.Empty())
	assert.Empty(t, same.IDs())

	fromNothing := DiffIssues(nil, prev)
	assert.Len(t, fromNothing.Added, len(prev))
	assert.Empty(t, fromNothing.Changed)
	assert.Empty(t, fromNothing.Removed)
}

func TestWatcherDiffBlockerOrderIsIrrelevant(t *testing.T) {
	a := watcherIssue("b-1", domain.BeadStateOpen, 0, "x", "y")
	b := watcherIssue("b-1", domain.BeadStateOpen, 0, "y", "x")
	assert.True(t, DiffIssues([]Issue{a}, []Issue{b}).Empty())
}

func TestWatcherDiffSwappedBlocker(t *testing.T) {
	// Same number of blockers, one of them replaced: updated_at, status and
	// assignee are all identical, so only the blocker comparison catches it.
	a := watcherIssue("b-1", domain.BeadStateOpen, 0, "x", "y")
	b := watcherIssue("b-1", domain.BeadStateOpen, 0, "x", "z")
	ch := DiffIssues([]Issue{a}, []Issue{b})
	assert.Equal(t, graphIDs("b-1"), ch.Changed)
	assert.Empty(t, ch.Added)
	assert.Empty(t, ch.Removed)
}

func TestWatcherMergeChanges(t *testing.T) {
	earlier := Change{
		At:      watcherT0,
		Initial: true,
		Added:   graphIDs("a", "b"),
		Changed: graphIDs("c", "d"),
		Removed: graphIDs("e", "f"),
		Issues:  []Issue{graphIssue("a", domain.BeadStateOpen)},
	}
	later := Change{
		At:      watcherT0.Add(time.Second),
		Added:   graphIDs("e", "g"), // e: removed then re-added -> changed
		Changed: graphIDs("a", "f"), // a: still added; f: was removed, now changed
		Removed: graphIDs("b", "c"), // b: added then removed -> gone; c: changed then removed
		Issues:  []Issue{graphIssue("g", domain.BeadStateOpen)},
	}
	m := MergeChanges(earlier, later)
	assert.Equal(t, graphIDs("a", "g"), m.Added)
	assert.Equal(t, graphIDs("d", "e", "f"), m.Changed)
	assert.Equal(t, graphIDs("c"), m.Removed)
	assert.True(t, m.Initial)
	assert.Equal(t, later.At, m.At)
	assert.Equal(t, later.Issues, m.Issues)

	empty := MergeChanges(Change{}, Change{})
	assert.True(t, empty.Empty())
	assert.Nil(t, empty.Added)
}

func TestWatcherNewDefaults(t *testing.T) {
	w := NewWatcher(newWatcherLister(watcherListing{}), WatcherOptions{})
	assert.Equal(t, DefaultPollInterval, w.Interval())
	assert.True(t, w.filter.All, "All is forced on so closed beads are seen")
	assert.NotNil(t, w.clock)
	assert.NotNil(t, w.logger)
	assert.Equal(t, 2*time.Second, DefaultPollInterval)
	assert.Panics(t, func() { NewWatcher(nil, WatcherOptions{}) })

	w = NewWatcher(newWatcherLister(watcherListing{}), WatcherOptions{Interval: -1})
	assert.Equal(t, DefaultPollInterval, w.Interval())

	assert.WithinDuration(t, time.Now(), SystemClock().Now(), time.Minute)
	select {
	case <-SystemClock().After(0):
	case <-time.After(time.Second):
		t.Fatal("system clock After(0) did not fire")
	}
}

func TestWatcherRunWithFakeClock(t *testing.T) {
	epic := watcherEpic("fx-epic")
	s0 := []Issue{epic, watcherIssue("fx-1", domain.BeadStateOpen, 0), watcherIssue("fx-2", domain.BeadStateOpen, 0, "fx-1")}
	s1 := append([]Issue(nil), s0...)
	s1[1] = watcherIssue("fx-1", domain.BeadStateInProgress, 3)
	s1[1].Assignee = "sc:engineer-1"
	s2 := []Issue{epic, s1[1], watcherIssue("fx-3", domain.BeadStateOpen, 4)}

	lister := newWatcherLister(
		watcherListing{issues: s0},
		watcherListing{issues: s0}, // unchanged: nothing delivered
		watcherListing{issues: s1},
		watcherListing{issues: s2},
	)
	clock := newWatcherClock(watcherT0)
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 16)
	var logs bytes.Buffer
	w := NewWatcher(lister, WatcherOptions{
		Interval: time.Second,
		Filter:   ListFilter{Labels: []string{RunLabel("r1")}},
		Run:      "r1",
		Epic:     epic.ID,
		Bus:      bus,
		Clock:    clock,
		Logger:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Poll 1: the initial snapshot, everything added.
	watcherWait(t, lister.Calls, "poll 1")
	ch := watcherWait(t, w.Changes(), "initial change")
	assert.True(t, ch.Initial)
	assert.Equal(t, graphIDs("fx-1", "fx-2", "fx-epic"), ch.Added)
	assert.Empty(t, ch.Changed)
	assert.Equal(t, watcherT0, ch.At)
	ev := watcherWait(t, sub.Events(), "initial GraphChanged")
	gc, ok := ev.(events.GraphChanged)
	require.True(t, ok, "got %T", ev)
	assert.Equal(t, domain.RunID("r1"), gc.RunID())
	assert.Equal(t, watcherT0, gc.At())
	assert.Equal(t, epic.ID, gc.Epic)
	require.Len(t, gc.Beads, 2, "the epic is not a work bead")
	assert.Equal(t, domain.BeadID("fx-1"), gc.Beads[0].ID)
	assert.Equal(t, graphIDs("fx-1"), gc.Beads[1].Deps)

	// Poll 2: identical listing, so nothing is delivered or published.
	clock.Tick(t, time.Second)
	watcherWait(t, lister.Calls, "poll 2")
	watcherNothing(t, w.Changes(), "change")
	watcherNothing(t, sub.Events(), "event")

	// Poll 3: fx-1 claimed.
	clock.Tick(t, time.Second)
	watcherWait(t, lister.Calls, "poll 3")
	ch = watcherWait(t, w.Changes(), "claim change")
	assert.False(t, ch.Initial)
	assert.Empty(t, ch.Added)
	assert.Equal(t, graphIDs("fx-1"), ch.Changed)
	assert.Empty(t, ch.Removed)
	assert.Equal(t, watcherT0.Add(2*time.Second), ch.At)
	ev = watcherWait(t, sub.Events(), "claim GraphChanged")
	gc, ok = ev.(events.GraphChanged)
	require.True(t, ok, "got %T", ev)
	assert.Equal(t, domain.BeadStateInProgress, gc.Beads[0].State)
	assert.Equal(t, "sc:engineer-1", gc.Beads[0].Assignee)

	// Poll 4: fx-2 gone, fx-3 new.
	clock.Tick(t, time.Second)
	watcherWait(t, lister.Calls, "poll 4")
	ch = watcherWait(t, w.Changes(), "add/remove change")
	assert.Equal(t, graphIDs("fx-3"), ch.Added)
	assert.Empty(t, ch.Changed)
	assert.Equal(t, graphIDs("fx-2"), ch.Removed)
	assert.Equal(t, graphIDs("fx-epic", "fx-1", "fx-3"), IssueIDs(w.Issues()), "the snapshot keeps bd's order")
	assert.Equal(t, 3, w.Graph().Len())
	watcherWait(t, sub.Events(), "add/remove GraphChanged")

	// The clock ticked before the watcher slept only once per tick, so a
	// tick that is too short must not wake it.
	clock.Tick(t, 400*time.Millisecond)
	watcherNothing(t, lister.Calls, "poll")

	cancel()
	assert.ErrorIs(t, watcherWait(t, done, "Run to return"), context.Canceled)
	_, open := <-w.Changes()
	assert.False(t, open, "Changes is closed when Run returns")

	stats := w.Stats()
	assert.Equal(t, 4, stats.Polls)
	assert.Equal(t, 0, stats.Failures)
	assert.Equal(t, 3, stats.Changes)
	assert.Equal(t, 0, stats.Coalesced)
	assert.Equal(t, watcherT0.Add(3*time.Second), stats.LastPoll)
	assert.NoError(t, stats.LastError)

	for _, f := range lister.Filters() {
		assert.True(t, f.All, "every listing includes closed beads")
		assert.Equal(t, []string{"sc:run:r1"}, f.Labels)
	}
	assert.Contains(t, logs.String(), "graph poll")
}

func TestWatcherCoalescesUnreadChanges(t *testing.T) {
	s0 := []Issue{watcherIssue("a", domain.BeadStateOpen, 0), watcherIssue("b", domain.BeadStateOpen, 0)}
	s1 := []Issue{watcherIssue("a", domain.BeadStateOpen, 1), watcherIssue("b", domain.BeadStateOpen, 0), watcherIssue("c", domain.BeadStateOpen, 0)}
	s2 := []Issue{watcherIssue("a", domain.BeadStateOpen, 1), watcherIssue("c", domain.BeadStateOpen, 2)}
	s3 := []Issue{watcherIssue("a", domain.BeadStateOpen, 1), watcherIssue("c", domain.BeadStateOpen, 2), watcherIssue("b", domain.BeadStateClosed, 5)}
	lister := newWatcherLister(watcherListing{issues: s0}, watcherListing{issues: s1}, watcherListing{issues: s2}, watcherListing{issues: s3})
	clock := newWatcherClock(watcherT0)
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 16)
	w := NewWatcher(lister, WatcherOptions{Interval: time.Second, Clock: clock, Bus: bus, Run: "r1"})
	ctx := context.Background()

	// Nobody reads Changes between the polls.
	for i := 0; i < 4; i++ {
		_, err := w.Poll(ctx)
		require.NoError(t, err)
		clock.now = clock.now.Add(time.Second)
	}
	ch := watcherWait(t, w.Changes(), "coalesced change")
	assert.True(t, ch.Initial, "the initial flag survives coalescing")
	// The consumer has seen nothing yet, so every bead in the final
	// listing is new to it: a (changed since the baseline), c (added
	// later) and b (removed, then back) are all Added, none Changed.
	assert.Equal(t, graphIDs("a", "b", "c"), ch.Added)
	assert.Empty(t, ch.Changed)
	assert.Empty(t, ch.Removed)
	assert.Equal(t, s3, ch.Issues)
	assert.Equal(t, watcherT0.Add(3*time.Second), ch.At)
	watcherNothing(t, w.Changes(), "second change")

	stats := w.Stats()
	assert.Equal(t, 4, stats.Polls)
	assert.Equal(t, 4, stats.Changes)
	assert.Equal(t, 3, stats.Coalesced)

	// The bus still saw every snapshot: coalescing is for the channel only.
	var kinds []events.Kind
	for i := 0; i < 4; i++ {
		kinds = append(kinds, watcherWait(t, sub.Events(), "GraphChanged").Kind())
	}
	assert.Equal(t, []events.Kind{events.KindGraphChanged, events.KindGraphChanged, events.KindGraphChanged, events.KindGraphChanged}, kinds)
}

func TestWatcherKick(t *testing.T) {
	s0 := []Issue{watcherIssue("a", domain.BeadStateOpen, 0)}
	s1 := []Issue{watcherIssue("a", domain.BeadStateClosed, 1)}
	lister := newWatcherLister(watcherListing{issues: s0}, watcherListing{issues: s0}, watcherListing{issues: s1})
	clock := newWatcherClock(watcherT0)
	w := NewWatcher(lister, WatcherOptions{Interval: time.Hour, Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Kicks before Run collapse into one, honoured right after the first poll.
	w.Kick()
	w.Kick()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	watcherWait(t, lister.Calls, "poll 1")
	watcherWait(t, w.Changes(), "initial change")
	watcherWait(t, lister.Calls, "poll 2 from the pending kick")
	watcherNothing(t, w.Changes(), "change from an identical listing")
	watcherNothing(t, lister.Calls, "a third poll")

	// A kick while sleeping polls at once, hours before the next tick.
	w.Kick()
	watcherWait(t, lister.Calls, "kicked poll")
	ch := watcherWait(t, w.Changes(), "kicked change")
	assert.Equal(t, graphIDs("a"), ch.Changed)
	watcherNothing(t, lister.Calls, "a poll without a kick or tick")
	assert.Equal(t, 3, w.Stats().Polls)

	cancel()
	assert.ErrorIs(t, watcherWait(t, done, "Run to return"), context.Canceled)
}

func TestWatcherPollFailures(t *testing.T) {
	boom := errors.New("bd list: exit status 1")
	s0 := []Issue{watcherIssue("a", domain.BeadStateOpen, 0)}
	lister := newWatcherLister(
		watcherListing{err: boom},
		watcherListing{err: boom},
		watcherListing{issues: s0},
		watcherListing{err: boom},
		watcherListing{issues: s0},
	)
	clock := newWatcherClock(watcherT0)
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 16)
	var logs bytes.Buffer
	w := NewWatcher(lister, WatcherOptions{
		Interval: time.Second, Clock: clock, Bus: bus, Run: "r1",
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	ctx := context.Background()

	_, err := w.Poll(ctx)
	assert.ErrorIs(t, err, boom)
	ev := watcherWait(t, sub.Events(), "warning")
	warn, ok := ev.(events.Warning)
	require.True(t, ok, "got %T", ev)
	assert.Equal(t, WatcherSource, warn.Source)
	assert.Equal(t, boom.Error(), warn.Err)
	assert.Equal(t, domain.RunID("r1"), warn.RunID())
	assert.Equal(t, watcherT0, warn.At())

	_, err = w.Poll(ctx)
	assert.ErrorIs(t, err, boom)
	watcherNothing(t, sub.Events(), "second warning in the same streak")
	watcherNothing(t, w.Changes(), "change on failure")
	stats := w.Stats()
	assert.Equal(t, 2, stats.Polls)
	assert.Equal(t, 2, stats.Failures)
	assert.ErrorIs(t, stats.LastError, boom)
	assert.Empty(t, w.Issues(), "a failed poll leaves the snapshot alone")

	ch, err := w.Poll(ctx)
	require.NoError(t, err)
	assert.True(t, ch.Initial, "the first success is still the initial snapshot")
	assert.Equal(t, graphIDs("a"), ch.Added)
	watcherWait(t, w.Changes(), "change after recovery")
	assert.Equal(t, events.KindGraphChanged, watcherWait(t, sub.Events(), "GraphChanged after recovery").Kind())
	assert.NoError(t, w.Stats().LastError)
	assert.Contains(t, logs.String(), "graph poll recovered")

	// A new streak warns again; the next success with no diff delivers nothing.
	_, err = w.Poll(ctx)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, events.KindWarning, watcherWait(t, sub.Events(), "second streak warning").Kind())
	ch, err = w.Poll(ctx)
	require.NoError(t, err)
	assert.True(t, ch.Empty())
	assert.False(t, ch.Initial)
	watcherNothing(t, w.Changes(), "empty change")
	assert.Equal(t, 5, w.Stats().Polls)
	assert.Equal(t, 3, w.Stats().Failures)
}

func TestWatcherRunReturnsOnCancelDuringPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lister := newWatcherLister(watcherListing{err: context.Canceled})
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 4)
	w := NewWatcher(lister, WatcherOptions{Clock: newWatcherClock(watcherT0), Bus: bus})
	cancel()
	assert.ErrorIs(t, w.Run(ctx), context.Canceled)
	watcherNothing(t, sub.Events(), "warning for a cancelled poll")
	assert.Equal(t, 1, w.Stats().Failures)
}

func TestWatcherEmptyInitialListing(t *testing.T) {
	lister := newWatcherLister(watcherListing{})
	w := NewWatcher(lister, WatcherOptions{Clock: newWatcherClock(watcherT0)})
	ch, err := w.Poll(context.Background())
	require.NoError(t, err)
	assert.True(t, ch.Initial)
	assert.True(t, ch.Empty())
	got := watcherWait(t, w.Changes(), "initial empty change")
	assert.True(t, got.Initial)
	ch, err = w.Poll(context.Background())
	require.NoError(t, err)
	assert.False(t, ch.Initial)
	watcherNothing(t, w.Changes(), "repeat of the empty change")
}

func TestWatcherForeignStatusesAreSafe(t *testing.T) {
	foreign := watcherIssue("z-1", domain.BeadStateOpen, 0)
	foreign.Status = "triage"
	lister := newWatcherLister(watcherListing{issues: []Issue{foreign}})
	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 4)
	w := NewWatcher(lister, WatcherOptions{
		Clock:    newWatcherClock(watcherT0),
		Bus:      bus,
		Statuses: StatusMap{"triage": domain.CategoryWIP},
	})
	_, err := w.Poll(context.Background())
	require.NoError(t, err)
	gc, ok := watcherWait(t, sub.Events(), "GraphChanged").(events.GraphChanged)
	require.True(t, ok)
	require.Len(t, gc.Beads, 1)
	assert.Equal(t, domain.BeadStateBlocked, gc.Beads[0].State)
}

func TestWatcherWithClient(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"list": "list-all"})
	w := NewWatcher(c, WatcherOptions{Clock: newWatcherClock(watcherT0), Filter: ListFilter{Labels: []string{"sc:run:r1"}}})
	ch, err := w.Poll(context.Background())
	require.NoError(t, err)
	assert.True(t, ch.Initial)
	assert.Len(t, ch.Added, 5)
	assert.Contains(t, ch.Added, domain.BeadID("fx-nvi"))
	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "list", "--json", "-n", "0", "--flat", "--all", "-l", "sc:run:r1"}, args)

	ch, err = w.Poll(context.Background())
	require.NoError(t, err)
	assert.True(t, ch.Empty(), "replaying the same sample changes nothing")
	assert.Equal(t, 5, w.Graph().Len())
}
