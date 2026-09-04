package logging

import (
	"regexp"
	"strings"
	"testing"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// TestSafeTextScrubsThenRedacts: the order matters. A cartridge that writes
// an escape sequence into the middle of a token would defeat a redactor that
// ran first, so the scrubber goes first and the rules see the token whole.
func TestSafeTextScrubsThenRedacts(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text is untouched", "nothing to see", "nothing to see"},
		{"escape sequences go", "\x1b[2J\x1b[31mred\x1b[0m", "red"},
		{"a token is masked", "key=sk-abcdefghijklmnop123456", "key=sk-[REDACTED]"},
		{"a token split by an escape is still masked",
			"key=sk-\x1b[0mabcdefghijklmnop123456", "key=sk-[REDACTED]"},
		{"a carriage return is honoured", "10%\r100%", "100%"},
		{"a tab becomes four columns", "a\tb", "a    b"},
		{"the line structure survives", "one\ntwo", "one\ntwo"},
		{"c0, del and c1 controls go", "\a bell \x7f del \u009b31m", " bell  del 31m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SafeText(tc.in)
			if got != tc.want {
				t.Fatalf("SafeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := SafeText(got); again != got {
				t.Fatalf("SafeText is not idempotent: %q then %q", got, again)
			}
		})
	}
}

// TestSafeEventCleansWhatAPublisherBuilds covers the hook P7-17 and P9-09
// put at a run's single publication point: `sc run` wires
// [Redactor.SafeEvent] into the publisher every event goes through, and
// internal/cli's TestRunPublisherRedactsBeforeEverySink pins that wiring.
// This test is the unit half — what the hook does to one event a publisher
// hands it.
func TestSafeEventCleansWhatAPublisherBuilds(t *testing.T) {
	const run domain.RunID = "run-hook"
	dirty := events.AgentOutput{
		Meta: events.NewMeta(run), Agent: "eng-1", Stream: events.StreamStdout,
		Line: "\x1b[2Jexport ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnop\a",
	}
	out, ok := SafeEvent(dirty).(events.AgentOutput)
	if !ok {
		t.Fatalf("SafeEvent changed the event type: %T", SafeEvent(dirty))
	}
	if want := "export ANTHROPIC_API_KEY=sk-[REDACTED]"; out.Line != want {
		t.Fatalf("line = %q, want %q", out.Line, want)
	}
	if out.Agent != dirty.Agent || out.RunID() != run || !out.At().Equal(dirty.At()) {
		t.Fatalf("SafeEvent changed a field it does not own: %+v", out)
	}

	// The notice fields a cartridge and bd write reach a terminal too.
	warn, ok := SafeEvent(events.Warning{
		Meta: events.NewMeta(run), Source: "cartridge",
		Message: "\x1b[1;41m spoofed \x1b[0m", Err: "Bearer abcdefghijklmnop",
	}).(events.Warning)
	if !ok {
		t.Fatal("SafeEvent changed the warning's type")
	}
	if warn.Message != " spoofed " || warn.Err != "Bearer [REDACTED]" {
		t.Fatalf("warning = %+v", warn)
	}
}

// TestRedactEventIsTheRulesWithoutTheScrubber, and Redactor.Event is the
// same for a redactor carrying extra rules.
func TestRedactEventIsTheRulesWithoutTheScrubber(t *testing.T) {
	const run domain.RunID = "run-hook"
	dirty := events.AgentOutput{
		Meta: events.NewMeta(run), Agent: "eng-1", Stream: events.StreamStdout,
		Line: "\x1b[2J echo sk-ant-api03-abcdefghijklmnop",
	}
	kept, ok := RedactEvent(dirty).(events.AgentOutput)
	if !ok {
		t.Fatal("RedactEvent changed the event type")
	}
	if !strings.Contains(kept.Line, "\x1b[2J") {
		t.Fatalf("RedactEvent is not supposed to scrub escapes: %q", kept.Line)
	}
	if strings.Contains(kept.Line, "sk-ant-api03-abcdefghijklmnop") {
		t.Fatalf("RedactEvent left the token in: %q", kept.Line)
	}

	extra := NewRedactor(Pattern{Name: "internal", Regexp: regexp.MustCompile(`\bxk-([a-z0-9]{8,})`)})
	custom, ok := extra.Event(events.Warning{Meta: events.NewMeta(run), Message: "xk-abcdefgh"}).(events.Warning)
	if !ok {
		t.Fatal("Redactor.Event changed the event type")
	}
	if custom.Message != "xk-[REDACTED]" {
		t.Fatalf("message = %q", custom.Message)
	}
}
