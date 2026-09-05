package logging_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
)

// The cost of the publication-time hook (P9-09, P7-17).
//
// P9-07 budgets the event wiring at 10 000 events a second and measures
// `Run.Publish` in front of the teed run log. A real run publishes through
// one more layer than that: `sc run` wraps its bus with
// [logging.Redactor.SafeEvent], so every event pays an escape scan plus the
// secret rules on every untrusted field before the run log ever sees it
// (internal/cli/run.go). These benchmarks measure that layer on its own, so
// the number in docs/performance.md is the whole path a run pays and not
// only the part below the hook.
//
// The shapes are the two ends of the range: [events.AgentOutput], which is
// one line and by far the most frequent event, and
// [events.VerdictReceived], the fattest event the loop publishes — a
// tester's prose plus four items of it.

// benchLine is what an agent writes: a log line with an escape sequence in
// it and a credential-shaped token, so both halves of SafeText have work.
const benchLine = "\x1b[32mok\x1b[0m  exporting ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwx build 12/40"

// benchMeta is a fixed envelope: the clock is not part of what is measured.
func benchMeta() events.Meta {
	return events.MetaAt("run-bench", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
}

// benchOutput is one agent output event, the most frequent kind by far.
func benchOutput() events.Event {
	return events.AgentOutput{
		Meta: benchMeta(), Agent: "eng-sc-1a2b-r1", Stream: events.StreamStdout, Line: benchLine,
	}
}

// benchVerdict is the fattest event the feedback loop publishes: a summary
// and four items, each with five pieces of prose.
func benchVerdict() events.Event {
	items := make([]domain.FeedbackItem, 4)
	for i := range items {
		items[i] = domain.FeedbackItem{
			ID: "qa-r1-" + string(rune('1'+i)), Severity: domain.SeverityBlocker,
			Category: domain.CategoryRules, Title: "the retry path has no test",
			Detail:     strings.Repeat("the backoff loop is not covered by any test. ", 4),
			Suggestion: "add a table test over the three retryable failures",
			File:       "internal/proc/retry.go", Line: 42, Status: domain.ItemOpen,
		}
	}
	return events.VerdictReceived{
		Meta: benchMeta(), Bead: "sc-1a2b", Round: 1, Agent: "tester-qa-sc-1a2b-r1",
		Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges,
			Summary: "one blocker and three notes; " + benchLine, Items: items,
		},
	}
}

// BenchmarkSafeText measures the hook on one string: the escape scrub and
// the secret rules over a line of agent output.
func BenchmarkSafeText(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = logging.SafeText(benchLine)
	}
}

// BenchmarkSafeEvent measures what `sc run` puts in front of its bus, for
// both ends of the event size range. The redactor is one built the way a
// run builds it — with extra rules on top of the defaults — because that is
// the path production takes ([logging.Redactor.SafeEvent]), not the
// package-level shortcut.
func BenchmarkSafeEvent(b *testing.B) {
	r := logging.NewRedactor(logging.Pattern{
		Name:   "bench-extra",
		Regexp: regexp.MustCompile(`(?i)\binternal-token-([a-z0-9]{8,})`),
	})
	cases := []struct {
		name string
		ev   events.Event
	}{
		{"agent_output", benchOutput()},
		{"verdict_received", benchVerdict()},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = r.SafeEvent(c.ev)
			}
		})
	}
}

// benchCleanOutput is an output line with nothing to mask, which is what
// nearly every event carries.
func benchCleanOutput() events.Event {
	return events.AgentOutput{
		Meta: benchMeta(), Agent: "eng-sc-1a2b-r1", Stream: events.StreamStdout,
		Line: "ok   slop-cannon/internal/proc  0.42s",
	}
}

// BenchmarkSafeEventClean is the same hook over an event with nothing to
// mask, which is the common case: it is the floor a run pays per event just
// for looking. Since P9-26 that floor is a byte scan rather than a regular
// expression per rule, and the prefilter is what this benchmark watches.
func BenchmarkSafeEventClean(b *testing.B) {
	ev := benchCleanOutput()
	b.ReportAllocs()
	for b.Loop() {
		_ = logging.SafeEvent(ev)
	}
}

// The publication hook's timing budget (P9-26).
//
// P9-07 budgets a publish at 10 000 events a second and asserts it over
// `Run.Publish` in front of the teed run log — the part *below* the hook.
// A real run publishes through one more layer, `(*logging.Redactor).SafeEvent`
// (internal/cli, runPublisher.Publish), and that layer was the larger half
// of a publish until the prefilter landed. These assertions cover it, so the
// two together cover the whole path an event travels.
//
// Like the orchestrator's, they are opt-in: a wall-clock threshold is not
// deterministic on a loaded machine, so they run only under
// SC_PERF_BUDGETS=1, which is how the numbers in docs/performance.md are
// re-checked.
const (
	// redactCleanBudget is the longest the hook may take over an event with
	// nothing to mask, which is nearly every event. It is the cost of the
	// publish beneath it (`RunPublishTeed`, 2.6 µs): P9-26's Done-when is
	// that looking costs no more than writing. The measured figure is an
	// order of magnitude inside it, so what this catches is the fast path
	// being lost — a rule whose words cannot be derived, or the scan being
	// taken out — rather than ordinary drift.
	redactCleanBudget = 2600 * time.Nanosecond
	// redactEventBudget is the 10 000 events a second the whole wiring is
	// budgeted at (orchestrator's perfBusBudget), asserted over the fattest
	// event the loop publishes: a verdict with four items, twenty pieces of
	// prose, one of which carries a credential.
	redactEventBudget = 100 * time.Microsecond
)

// TestRedactionPerformanceBudgets asserts P9-26's budgets over the
// publication hook, with the rules a real run carries.
func TestRedactionPerformanceBudgets(t *testing.T) {
	if os.Getenv("SC_PERF_BUDGETS") != "1" {
		t.Skip("set SC_PERF_BUDGETS=1 to check the P9-26 timing budgets")
	}
	// The redactor a run builds: the defaults plus the rules the secret
	// filter adds (agents.SecretPatterns, P9-05), which is the six the
	// prefilter has to answer for. They are repeated rather than imported
	// because internal/agents imports this package.
	r := logging.NewRedactor(
		logging.Pattern{
			Name:   "assignment",
			Regexp: regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_])(?:export\s+)?[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWD|PASSWORD|KEY|CREDENTIAL)[A-Z0-9_]*\s*[=:]\s*["']?([^\s"']{6,})`),
		},
		logging.Pattern{Name: "pem", Regexp: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(.+?)-----END`)},
		logging.Pattern{Name: "aws", Regexp: regexp.MustCompile(`\b(?:AKIA|ASIA)([0-9A-Z]{12,})`)},
	)

	t.Run("a clean agent output line", func(t *testing.T) {
		ev := benchCleanOutput()
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				_ = r.SafeEvent(ev)
			}
		})
		redactAssert(t, "the hook over a clean output line", res, redactCleanBudget)
	})

	t.Run("the fattest event the loop publishes", func(t *testing.T) {
		ev := benchVerdict()
		res := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				_ = r.SafeEvent(ev)
			}
		})
		redactAssert(t, "the hook over a verdict of four items", res, redactEventBudget)
	})
}

// redactAssert fails when a benchmark's time per operation is over its
// budget, and records the measurement either way.
func redactAssert(t *testing.T, what string, res testing.BenchmarkResult, budget time.Duration) {
	t.Helper()
	per := res.T / time.Duration(max(res.N, 1))
	if per > budget {
		t.Errorf("%s took %s, over the %s budget", what, per, budget)
		return
	}
	t.Logf("%s: %s (budget %s)", what, per, budget)
}
