package mock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock is the time source the adapter uses for step delays, the session
// timeout and event timestamps, so tests can script long pauses without
// waiting for them.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed. A
	// non-positive d fires immediately.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the wall clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a Clock that only moves when a test calls Advance or Set. It
// is safe for concurrent use: sessions register waiters from their own
// goroutines while the test drives time.
type FakeClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock returns a clock stopped at start.
func NewFakeClock(start time.Time) *FakeClock {
	c := &FakeClock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now implements Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After implements Clock. The channel fires when Advance or Set moves the
// clock to or past now+d; a non-positive d fires at once.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, &fakeWaiter{at: at, ch: ch})
	c.cond.Broadcast()
	return ch
}

// Advance moves the clock forward by d and fires every waiter whose
// deadline has been reached, earliest first. Negative values are ignored.
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(c.now.Add(d))
}

// Set moves the clock to t (never backwards) and fires the waiters that are
// due, earliest first.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(t)
}

func (c *FakeClock) setLocked(t time.Time) {
	if !t.After(c.now) {
		return
	}
	c.now = t
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].at.Before(c.waiters[j].at) })
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- w.at
	}
	c.waiters = kept
	c.cond.Broadcast()
}

// Waiters returns how many After channels are pending.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// BlockUntil blocks until at least n waiters are pending, which is how a
// test knows a session has reached its next delay before advancing time.
func (c *FakeClock) BlockUntil(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.waiters) < n {
		c.cond.Wait()
	}
}

// BlockUntilContext is BlockUntil with a deadline: it returns ctx.Err() if
// the waiters do not appear before ctx is done.
func (c *FakeClock) BlockUntilContext(ctx context.Context, n int) error {
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.waiters) < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.cond.Wait()
	}
	return nil
}
