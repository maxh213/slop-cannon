package proc

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// reapTagPrefix starts the name of the environment variable that marks a
// child started with [Spec.ReapDescendants]. Every child gets its own
// variable name rather than its own value of one shared name, so an sc
// running under another sc keeps both marks and each sweep finds its own.
const reapTagPrefix = "SC_REAP_"

// reapGraceCap bounds how long a sweep waits for tagged stragglers to
// honour SIGTERM before it sends SIGKILL, however long the spec's grace
// period is. The child has already exited, so whatever is left is debris.
const reapGraceCap = 5 * time.Second

// reapPoll is how often a sweep re-reads /proc while it waits.
const reapPoll = 50 * time.Millisecond

func newReapTag() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return reapTagPrefix + hex.EncodeToString(b)
}

func reapGrace(grace time.Duration) time.Duration {
	return min(grace, reapGraceCap)
}
