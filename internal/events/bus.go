package events

import (
	"context"
	"sync"
	"sync/atomic"
)

// Bus fans published events out to any number of subscribers in-process.
//
// Publish never blocks: each subscription has a buffered channel and an
// event that does not fit is dropped for that subscriber alone, counted in
// [Subscription.Dropped] and [Stats.Dropped]. Publish is serialised, so every
// subscriber that keeps up sees the identical sequence, in the order Publish
// calls completed. The zero Bus is not usable; call [NewBus].
type Bus struct {
	mu     sync.Mutex
	subs   map[uint64]*Subscription
	nextID uint64
	closed bool

	published atomic.Uint64
	dropped   atomic.Uint64
}

// Stats is a point-in-time snapshot of a bus's counters.
type Stats struct {
	// Published counts Publish calls accepted since the bus was created,
	// including ones no subscriber was around to receive.
	Published uint64
	// Dropped counts events that a subscriber missed because its buffer was
	// full, summed over all subscribers past and present.
	Dropped uint64
	// Subscribers is the number of live subscriptions.
	Subscribers int
}

// NewBus returns an empty, open bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[uint64]*Subscription)}
}

// Publish offers ev to every live subscription and returns immediately. A
// subscriber whose buffer is full misses the event and its drop counter is
// incremented. A nil event and a Publish after [Bus.Close] are ignored.
func (b *Bus) Publish(ev Event) {
	if ev == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.published.Add(1)
	for _, s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Add(1)
			b.dropped.Add(1)
		}
	}
}

// Subscribe registers a new subscriber with a channel buffer of the given
// size (values below 1 are raised to 1). The subscription ends, and its
// channel is closed, when ctx is done, when [Subscription.Unsubscribe] is
// called, or when the bus is closed. Events published before Subscribe
// returned are not delivered. Subscribing to a closed bus yields a
// subscription whose channel is already closed.
func (b *Bus) Subscribe(ctx context.Context, buffer int) *Subscription {
	if buffer < 1 {
		buffer = 1
	}
	s := &Subscription{bus: b, ch: make(chan Event, buffer)}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		s.done.Store(true)
		close(s.ch)
		return s
	}
	b.nextID++
	s.id = b.nextID
	b.subs[s.id] = s
	if ctx != nil && ctx.Done() != nil {
		// Registered under the lock so that a context that is already done
		// (whose callback runs at once, in its own goroutine) cannot observe
		// s.stop before it is set: Unsubscribe reads it under the same lock.
		s.stop = context.AfterFunc(ctx, s.Unsubscribe)
	}
	return s
}

// Close ends every subscription (closing their channels) and makes further
// Publish calls no-ops. It is safe to call more than once.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	stops := make([]func() bool, 0, len(b.subs))
	for id, s := range b.subs {
		delete(b.subs, id)
		s.done.Store(true)
		close(s.ch)
		if s.stop != nil {
			stops = append(stops, s.stop)
		}
	}
	b.mu.Unlock()

	for _, stop := range stops {
		stop()
	}
}

// Stats returns the current counters.
func (b *Bus) Stats() Stats {
	b.mu.Lock()
	n := len(b.subs)
	b.mu.Unlock()
	return Stats{
		Published:   b.published.Load(),
		Dropped:     b.dropped.Load(),
		Subscribers: n,
	}
}

// remove detaches s from the bus and closes its channel, once. It returns
// the context cancellation hook to release, or nil when s was already gone.
func (b *Bus) remove(s *Subscription) func() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s.id]; !ok {
		return nil
	}
	delete(b.subs, s.id)
	s.done.Store(true)
	close(s.ch)
	return s.stop
}

// Subscription is one subscriber's view of a [Bus]: a channel of events plus
// a counter of the events it missed.
type Subscription struct {
	bus     *Bus
	id      uint64
	ch      chan Event
	dropped atomic.Uint64
	done    atomic.Bool
	stop    func() bool // releases the context.AfterFunc; guarded by bus.mu
}

// Events returns the channel events arrive on. It is closed when the
// subscription ends, so `for ev := range sub.Events()` terminates.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped returns how many events this subscriber has missed so far because
// its buffer was full. The counter only grows; renderers can diff it to show
// a "missed N events" notice.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Done reports whether the subscription has ended. Events already buffered
// can still be read from the channel until it drains.
func (s *Subscription) Done() bool { return s.done.Load() }

// Unsubscribe ends the subscription and closes its channel. It is safe to
// call more than once and after the bus is closed.
func (s *Subscription) Unsubscribe() {
	if s == nil || s.bus == nil {
		return
	}
	if stop := s.bus.remove(s); stop != nil {
		stop()
	}
}
