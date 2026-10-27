//go:build !linux

package dash

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnsupportedHostSamplerIsSafe(t *testing.T) {
	sample, err := newHostSampler()()
	require.Error(t, err)
	require.Empty(t, sample)
	o := NewObserver(Options{Run: observerTestRun("run-host-unsupported"), RunDir: t.TempDir()})
	o.applyHostEvent(hostEvent{success: err == nil, sample: sample})
	o.publish()
	require.Nil(t, o.Snapshot().Host)
}
