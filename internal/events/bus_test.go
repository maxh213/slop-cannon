package events

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"slop-cannon/internal/domain"
)

// syncBuffer is a bytes.Buffer safe for use from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond until it holds or five seconds pass.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// seqEvent is what the bus tests publish: AgentOutput with the publisher in
// Agent and the sequence number in Line, cheap and easy to inspect.
func seqEvent(publisher string, n int) AgentOutput {
	return AgentOutput{Meta: sampleMeta(), Agent: domain.AgentID("pub-" + publisher), Stream: StreamStdout, Line: itoa(n)}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// output asserts ev is an AgentOutput and returns it.
func output(t *testing.T, ev Event) AgentOutput {
	t.Helper()
	o, ok := ev.(AgentOutput)
	if !ok {
		t.Fatalf("got %T, want AgentOutput", ev)
	}
	return o
}

func collect(sub *Subscription) []Event {
	var out []Event
	for ev := range sub.Events() {
		out = append(out, ev)
	}
	return out
}

func TestBusDeliversInPublishOrder(t *testing.T) {
	bus := NewBus()
	const n = 1000
	sub := bus.Subscribe(context.Background(), n)
	for i := 0; i < n; i++ {
		bus.Publish(seqEvent("a", i))
	}
	bus.Close()

	got := collect(sub)
	if len(got) != n {
		t.Fatalf("received %d events, want %d (dropped %d)", len(got), n, sub.Dropped())
	}
	for i, ev := range got {
		if line := output(t, ev).Line; atoi(line) != i {
			t.Fatalf("event %d is %q: out of order", i, line)
		}
	}
	if sub.Dropped() != 0 {
		t.Errorf("Dropped = %d", sub.Dropped())
	}
}

func TestBusConcurrentPublishersTotalOrderPerSubscriber(t *testing.T) {
	bus := NewBus()
	const publishers, perPublisher, subscribers = 8, 500, 4
	total := publishers * perPublisher

	subs := make([]*Subscription, subscribers)
	for i := range subs {
		subs[i] = bus.Subscribe(context.Background(), total)
	}

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			name := string(rune('a' + p))
			for i := 0; i < perPublisher; i++ {
				bus.Publish(seqEvent(name, i))
			}
		}(p)
	}
	wg.Wait()
	bus.Close()

	var reference []Event
	for si, sub := range subs {
		got := collect(sub)
		if len(got) != total {
			t.Fatalf("subscriber %d got %d events, want %d", si, len(got), total)
		}
		// Each publisher's own sequence is monotone from this subscriber's
		// point of view.
		last := map[string]int{}
		for _, ev := range got {
			o := output(t, ev)
			key := string(o.Agent)
			n := atoi(o.Line)
			if prev, seen := last[key]; seen && n != prev+1 {
				t.Fatalf("subscriber %d: %s jumped from %d to %d", si, key, prev, n)
			} else if !seen && n != 0 {
				t.Fatalf("subscriber %d: %s started at %d", si, key, n)
			}
			last[key] = n
		}
		// And every subscriber saw the identical interleaving.
		if reference == nil {
			reference = got
			continue
		}
		for i := range got {
			if got[i] != reference[i] {
				t.Fatalf("subscriber %d diverges from subscriber 0 at event %d", si, i)
			}
		}
	}
	if st := bus.Stats(); st.Published != uint64(total) || st.Dropped != 0 {
		t.Errorf("Stats = %+v", st)
	}
}

func TestBusPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	slow := bus.Subscribe(context.Background(), 2)
	fast := bus.Subscribe(context.Background(), 200)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			bus.Publish(seqEvent("a", i))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}

	if got := slow.Dropped(); got != 98 {
		t.Errorf("slow.Dropped() = %d, want 98", got)
	}
	if got := fast.Dropped(); got != 0 {
		t.Errorf("fast.Dropped() = %d, want 0", got)
	}
	st := bus.Stats()
	if st.Published != 100 || st.Dropped != 98 || st.Subscribers != 2 {
		t.Errorf("Stats = %+v", st)
	}
	// The slow subscriber still has the first two events, in order.
	for i := 0; i < 2; i++ {
		select {
		case ev := <-slow.Events():
			if line := output(t, ev).Line; atoi(line) != i {
				t.Errorf("slow event %d = %q", i, line)
			}
		default:
			t.Fatalf("slow subscriber missing buffered event %d", i)
		}
	}
	// Once it drains, it receives again.
	bus.Publish(seqEvent("a", 100))
	select {
	case ev := <-slow.Events():
		if line := output(t, ev).Line; line != "100" {
			t.Errorf("after drain got %q", line)
		}
	default:
		t.Error("slow subscriber did not receive after draining")
	}
	if fast.Dropped() != 0 || slow.Dropped() != 98 {
		t.Errorf("drop counters moved: fast=%d slow=%d", fast.Dropped(), slow.Dropped())
	}
}

func TestBusSubscriberEndsWithContext(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx, 4)
	bus.Publish(seqEvent("a", 0))
	cancel()
	waitFor(t, func() bool { return sub.Done() })
	waitFor(t, func() bool { return bus.Stats().Subscribers == 0 })

	// The buffered event is still readable, then the channel is closed.
	got := collect(sub)
	if len(got) != 1 {
		t.Errorf("got %d events after cancel, want the 1 buffered", len(got))
	}
	// Publishing afterwards does not panic and is not counted as dropped for
	// a subscriber that is gone.
	bus.Publish(seqEvent("a", 1))
	if sub.Dropped() != 0 {
		t.Errorf("Dropped after unsubscribe = %d", sub.Dropped())
	}

	// A context that is already done yields a subscription that closes
	// promptly.
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	sub2 := bus.Subscribe(done, 1)
	select {
	case _, ok := <-sub2.Events():
		if ok {
			t.Error("received an event on a subscription with a done context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription with a done context never closed")
	}
}

func TestBusUnsubscribeIsIdempotent(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := bus.Subscribe(ctx, 1)
	other := bus.Subscribe(context.Background(), 1)

	sub.Unsubscribe()
	sub.Unsubscribe()
	if !sub.Done() {
		t.Error("Done() false after Unsubscribe")
	}
	if _, ok := <-sub.Events(); ok {
		t.Error("channel not closed after Unsubscribe")
	}
	if bus.Stats().Subscribers != 1 {
		t.Errorf("Subscribers = %d, want 1", bus.Stats().Subscribers)
	}
	cancel() // the context hook must be inert now
	bus.Publish(seqEvent("a", 0))
	if other.Dropped() != 0 {
		t.Error("other subscriber affected")
	}
	if len(other.Events()) != 1 {
		t.Error("other subscriber did not receive")
	}

	bus.Close()
	sub.Unsubscribe()   // after Close: no panic
	other.Unsubscribe() // after Close: no panic
	var nilSub *Subscription
	nilSub.Unsubscribe() // nil receiver: no panic
	(&Subscription{}).Unsubscribe()
}

func TestBusClose(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := bus.Subscribe(ctx, 4)
	b := bus.Subscribe(context.Background(), 4)
	bus.Publish(seqEvent("a", 0))

	bus.Close()
	bus.Close() // idempotent

	for name, sub := range map[string]*Subscription{"a": a, "b": b} {
		if !sub.Done() {
			t.Errorf("%s: Done() false after Close", name)
		}
		got := collect(sub)
		if len(got) != 1 {
			t.Errorf("%s: %d events after Close, want the 1 buffered then closed", name, len(got))
		}
	}
	if st := bus.Stats(); st.Subscribers != 0 || st.Published != 1 {
		t.Errorf("Stats after Close = %+v", st)
	}

	// Publish after Close is a silent no-op.
	bus.Publish(seqEvent("a", 1))
	if bus.Stats().Published != 1 {
		t.Error("Publish after Close was counted")
	}

	// Subscribe after Close returns a closed subscription.
	late := bus.Subscribe(context.Background(), 1)
	if !late.Done() {
		t.Error("late subscription not Done")
	}
	if _, ok := <-late.Events(); ok {
		t.Error("late subscription channel open")
	}
	late.Unsubscribe() // no panic
}

func TestBusIgnoresNilAndClampsBuffer(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	sub := bus.Subscribe(context.Background(), 0)
	bus.Publish(nil)
	if bus.Stats().Published != 0 {
		t.Error("nil event was counted")
	}
	bus.Publish(seqEvent("a", 0))
	select {
	case ev := <-sub.Events():
		if line := output(t, ev).Line; line != "0" {
			t.Errorf("got %q", line)
		}
	default:
		t.Error("buffer 0 was not clamped to 1: event dropped")
	}
	if cap(sub.ch) != 1 {
		t.Errorf("cap = %d, want 1", cap(sub.ch))
	}
}

func TestBusStress(t *testing.T) {
	// Publishers, subscribers coming and going, and a late Close, all at
	// once: the race detector is the assertion here.
	bus := NewBus()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					bus.Publish(seqEvent(string(rune('a'+p)), i))
				}
			}
		}(p)
	}
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				sub := bus.Subscribe(ctx, 1+s)
			read:
				for j := 0; j < 10; j++ {
					select {
					case _, ok := <-sub.Events():
						if !ok {
							break read
						}
					case <-time.After(time.Millisecond):
					}
				}
				if i%2 == 0 {
					cancel()
				} else {
					sub.Unsubscribe()
					cancel()
				}
				_ = sub.Dropped()
			}
		}(s)
	}
	time.Sleep(50 * time.Millisecond)
	bus.Close()
	close(stop)
	wg.Wait()
	if bus.Stats().Subscribers != 0 {
		t.Errorf("subscribers left after Close: %d", bus.Stats().Subscribers)
	}
}

func BenchmarkBusPublish(b *testing.B) {
	bus := NewBus()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 4; i++ {
		sub := bus.Subscribe(ctx, 1024)
		go func() {
			for range sub.Events() {
			}
		}()
	}
	ev := seqEvent("a", 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Publish(ev)
	}
}

func BenchmarkMarshal(b *testing.B) {
	ev := sampleEvents()[8] // BeadStateChanged
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(ev); err != nil {
			b.Fatal(err)
		}
	}
}
