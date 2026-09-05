package testutil

import (
	"flag"
	"time"
)

// PatienceReference is the `go test` default timeout. A budget passed to
// [Patience] is written for a test binary given exactly this much time;
// one given more is assumed to be running somewhere slower or busier, and
// every budget grows in proportion.
const PatienceReference = 10 * time.Minute

// PatienceMax bounds the factor, so a `-timeout 24h` debugging session
// does not turn a five-second wait into a five-minute hang before the
// failure is reported.
const PatienceMax = 20

// PatienceUnbounded is the factor used when the binary was given no
// timeout at all (`-timeout 0`): the caller has said they do not want the
// clock deciding anything, so waits are stretched rather than left at
// their nominal length.
const PatienceUnbounded = 10

// Patience scales a wall-clock budget by the timeout the test binary was
// given (P9-27, P6-56).
//
// A test that waits for a condition needs some bound, or a deadlock hangs
// the suite instead of failing it. Written as a bare duration, that bound
// is a claim about how fast the machine is: five seconds is generous on an
// idle laptop and short on a loaded CI box running six race-instrumented
// test binaries at once, where the failure that follows says "timed out
// waiting for …" and names nothing the code did. Both known flakes of that
// shape in this repository — one scheduler arbitration test in 4000
// iterations, and one unreproduced whole-tree failure — are that claim
// being wrong.
//
// The signal already available is `-timeout`: whoever runs a suite on a
// slow or busy machine raises it, and `make check` can raise it once for
// everyone. So a budget is multiplied by timeout/[PatienceReference],
// never shrunk below the budget itself and never grown beyond
// [PatienceMax] times it. Use it wherever a test waits on something the
// code does asynchronously:
//
//	require.Eventually(t, ready, testutil.Patience(5*time.Second), 10*time.Millisecond, "…")
//
// It is not a substitute for waiting on a condition rather than sleeping:
// poll for what you need and let this decide only how long you are willing
// to wait for it. A fake clock, where the component under test takes one,
// is better still.
func Patience(budget time.Duration) time.Duration {
	if budget <= 0 {
		return budget
	}
	return time.Duration(float64(budget) * PatienceFactor())
}

// PatienceFactor is the multiplier [Patience] applies, exposed so a test
// can scale something that is not a duration (a poll count, say). It is at
// least 1.
func PatienceFactor() float64 {
	timeout, known := testTimeout()
	switch {
	case !known:
		// No testing flags registered — this is not a test binary, or
		// flag parsing has not happened yet. Take the budget as written.
		return 1
	case timeout <= 0:
		return PatienceUnbounded
	}
	factor := float64(timeout) / float64(PatienceReference)
	if factor < 1 {
		return 1
	}
	if factor > PatienceMax {
		return PatienceMax
	}
	return factor
}

// testTimeout reads -test.timeout. known is false when the flag is not
// registered, which is every binary that is not a test.
func testTimeout() (d time.Duration, known bool) {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0, false
	}
	getter, ok := f.Value.(flag.Getter)
	if !ok {
		return 0, false
	}
	timeout, ok := getter.Get().(time.Duration)
	if !ok {
		return 0, false
	}
	return timeout, true
}
