package testutil

import (
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withTimeout sets -test.timeout for the duration of one subtest.
func withTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	f := flag.Lookup("test.timeout")
	require.NotNil(t, f, "the testing package must have registered -test.timeout")
	saved := f.Value.String()
	require.NoError(t, f.Value.Set(d.String()))
	t.Cleanup(func() { _ = f.Value.Set(saved) })
}

func TestPatienceScalesWithTheTimeoutTheBinaryWasGiven(t *testing.T) {
	t.Run("the default timeout leaves a budget as written", func(t *testing.T) {
		withTimeout(t, PatienceReference)
		assert.Equal(t, 5*time.Second, Patience(5*time.Second))
	})
	t.Run("a shorter timeout never shrinks a budget", func(t *testing.T) {
		withTimeout(t, time.Minute)
		assert.Equal(t, 5*time.Second, Patience(5*time.Second))
		assert.InDelta(t, 1.0, PatienceFactor(), 0)
	})
	t.Run("a longer timeout stretches it in proportion", func(t *testing.T) {
		withTimeout(t, 30*time.Minute)
		assert.Equal(t, 15*time.Second, Patience(5*time.Second))
	})
	t.Run("a huge timeout is capped", func(t *testing.T) {
		withTimeout(t, 24*time.Hour)
		assert.Equal(t, PatienceMax*5*time.Second, Patience(5*time.Second))
	})
	t.Run("no timeout at all stretches rather than leaves alone", func(t *testing.T) {
		withTimeout(t, 0)
		assert.Equal(t, PatienceUnbounded*5*time.Second, Patience(5*time.Second))
	})
	t.Run("a budget of zero or less is left alone", func(t *testing.T) {
		withTimeout(t, 30*time.Minute)
		assert.Zero(t, Patience(0))
		assert.Equal(t, -time.Second, Patience(-time.Second))
	})
}
