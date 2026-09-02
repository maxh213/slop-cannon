package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var mockEpoch = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func TestFakeClockAfterFiresOnAdvance(t *testing.T) {
	t.Parallel()
	c := NewFakeClock(mockEpoch)
	short := c.After(time.Second)
	long := c.After(time.Minute)
	assert.Equal(t, 2, c.Waiters())

	c.Advance(500 * time.Millisecond)
	select {
	case <-short:
		t.Fatal("fired before its deadline")
	default:
	}

	c.Advance(500 * time.Millisecond)
	select {
	case at := <-short:
		assert.Equal(t, mockEpoch.Add(time.Second), at)
	default:
		t.Fatal("did not fire at its deadline")
	}
	assert.Equal(t, 1, c.Waiters())

	c.Set(mockEpoch.Add(time.Hour))
	select {
	case <-long:
	default:
		t.Fatal("Set did not fire the later waiter")
	}
	assert.Equal(t, 0, c.Waiters())
	assert.Equal(t, mockEpoch.Add(time.Hour), c.Now())
}

func TestFakeClockNonPositiveAfterFiresImmediately(t *testing.T) {
	t.Parallel()
	c := NewFakeClock(mockEpoch)
	select {
	case <-c.After(0):
	default:
		t.Fatal("After(0) did not fire")
	}
	select {
	case <-c.After(-time.Second):
	default:
		t.Fatal("After(-1s) did not fire")
	}
	assert.Equal(t, 0, c.Waiters())
}

func TestFakeClockNeverMovesBackwards(t *testing.T) {
	t.Parallel()
	c := NewFakeClock(mockEpoch)
	c.Advance(-time.Hour)
	c.Set(mockEpoch.Add(-time.Hour))
	assert.Equal(t, mockEpoch, c.Now())
}

func TestFakeClockBlockUntil(t *testing.T) {
	t.Parallel()
	c := NewFakeClock(mockEpoch)
	go func() { c.After(time.Second) }()
	c.BlockUntil(1)
	assert.Equal(t, 1, c.Waiters())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.BlockUntilContext(ctx, 2), context.DeadlineExceeded)
	require.NoError(t, c.BlockUntilContext(context.Background(), 1))
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	c := SystemClock()
	before := time.Now()
	assert.False(t, c.Now().Before(before))
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("system clock After never fired")
	}
}
