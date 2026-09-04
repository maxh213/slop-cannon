package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/textsafe"
)

// sanDirty is one line carrying every hazard at once: clear-screen and
// colour sequences, an OSC window-title string, a bell, a DEL, a tab and a
// carriage return that rewrites the line.
const sanDirty = "\x1b[2J\x1b[H\x1b[31mBOOM\x1b[0m\x1b]0;pwned\x07 done\x07\x7f\rrewritten\ttail"

// sanEscape matches an ANSI escape sequence, so a test can assert on the
// text a reader actually sees rather than on the styling around it.
var sanEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]|\x1b\\][^\x07]*\x07")

// sanPlain strips the styling a themed frame draws.
func sanPlain(s string) string { return sanEscape.ReplaceAllString(s, "") }

// sanControl reports whether s carries anything a terminal reads as an
// instruction. Newlines count: a headless line and a frame row are one line.
func sanControl(s string) bool { return strings.ContainsFunc(s, textsafe.IsControl) }

// TestSanitiseHelpersUseTheSharedScanner pins the three policies the package
// asks internal/textsafe for (P9-10): a transcript cell honours a carriage
// return the way a terminal does and pays four columns for a tab; a row
// turns its breaks — the carriage return included — into spaces, so a title
// cannot erase the columns to its left; and a headless line drops the
// carriage return so a cartridge cannot erase the clock stamp in front of it.
func TestSanitiseHelpersUseTheSharedScanner(t *testing.T) {
	require.Equal(t, "rewritten    tail", agentsSanitize(sanDirty))
	require.Equal(t, "BOOM done rewritten    tail", rowText(sanDirty))
	require.Equal(t, "BOOM donerewritten tail", headlessText(sanDirty))

	require.Equal(t, "one two", rowText("one\ntwo"), "a row is one row")
	require.Equal(t, "one two", headlessText("one\ntwo"), "a headless line is one line")
	require.Equal(t, "onetwo", agentsSanitize("one\ntwo"), "a cell has no room for a break")

	for _, f := range []func(string) string{agentsSanitize, rowText, headlessText} {
		require.Equal(t, "plain text", f("plain text"), "clean text is returned unchanged")
		require.False(t, sanControl(f(sanDirty)))
	}
}

// TestHeadlessScrubsEveryUntrustedField is P7-18's acceptance for the
// renderer: `sc run --headless` writes to a terminal, so a control character
// in an agent's output line, in a warning's or an error's message and error,
// in the run's summary or in a verdict's summary must come out as one clean
// line — and the text itself must survive.
func TestHeadlessScrubsEveryUntrustedField(t *testing.T) {
	const run domain.RunID = "sc-san-0001"
	at := time.Date(2026, 5, 6, 12, 1, 3, 0, time.UTC)
	meta := events.MetaAt(run, at)
	dirty := func(s string) string { return "\x1b[2J\x1b[31m" + s + "\x1b[0m\rand\ttabbed\nplus a break\x07" }

	cases := []struct {
		name string
		ev   events.Event
		want string // a fragment that has to survive the scrub
	}{
		{"agent output", events.AgentOutput{Meta: meta, Agent: "eng-1", Stream: events.StreamStdout,
			Line: dirty("spoofed")}, "spoofed"},
		{"warning", events.Warning{Meta: meta, Source: "cartridge", Message: dirty("bad news"),
			Err: dirty("exit 9")}, "bad news"},
		{"error", events.Error{Meta: meta, Source: "bd", Message: dirty("broken"),
			Err: dirty("no such issue")}, "broken"},
		{"run finished summary", events.RunFinished{Meta: meta, Reason: events.FinishFailed,
			Summary: dirty("2 of 3 beads")}, "2 of 3 beads"},
		{"verdict summary", events.VerdictReceived{Meta: meta, Bead: "sc-1", Round: 1, Agent: "qa-1",
			Verdict: domain.Verdict{Lens: domain.LensQA, Outcome: domain.VerdictChanges,
				Summary: dirty("needs work")}}, "needs work"},
		{"agent failed error", events.AgentFailed{Meta: meta, Agent: "eng-1", Role: domain.RoleEngineer,
			Failure: events.FailureCrash, Err: dirty("panic")}, "panic"},
		{"bead title", events.AgentSpawned{Meta: meta, Agent: domain.Agent{ID: "eng-1", Role: domain.RoleEngineer,
			Bead: "sc-1"}, Task: events.TaskImplement, BeadTitle: dirty("Add retry")}, "Add retry"},
		{"pr url", events.PROpened{Meta: meta, Bead: "sc-1", Number: 42,
			URL: dirty("https://x.test/pull/42"), Branch: dirty("sc/branch")}, "https://x.test/pull/42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := FormatHeadless(tc.ev, time.UTC)
			require.False(t, sanControl(line), "a control character reached the terminal: %q", line)
			require.Equal(t, 1, len(strings.Split(line, "\n")), "the one-event-per-line contract broke: %q", line)
			require.True(t, strings.HasPrefix(line, "12:01:03 "), "the clock stamp was overwritten: %q", line)
			require.Contains(t, line, tc.want, "the text itself has to survive")
		})
	}
}

// TestPRsViewScrubsUntrustedText is P7-18's acceptance for the PRs view: the
// bead title comes out of a model through bd and the URL, branch and merge
// sha out of gh and git, and all of them are drawn into the frame.
func TestPRsViewScrubsUntrustedText(t *testing.T) {
	const w, h = 100, 24
	dirty := "\x1b[2J\x1b[31mBOOM\x1b[0m\rrewritten\ttail"
	st := prsState(true, []events.Event{
		events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
		events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{
			ID: "sc-1a2b", Title: dirty, Type: domain.BeadTypeTask,
			Priority: domain.PriorityDefault, State: domain.BeadStateDelivering,
		}},
		events.PROpened{Meta: prsMeta(2), Bead: "sc-1a2b", Number: 42,
			URL: "https://example.invalid/pull/42" + dirty, Branch: dirty, Base: dirty},
		events.Merged{Meta: prsMeta(3), Bead: "sc-1a2b", SHA: dirty, Branch: dirty, PRNumber: 42},
	})

	rows := PRRows(st)
	require.Len(t, rows, 1)
	require.False(t, sanControl(rows[0].Title), "the row kept a control character in the title")
	require.False(t, sanControl(rows[0].URL), "the row kept a control character in the URL")
	require.False(t, sanControl(rows[0].Ref()), "the REF column is drawn straight from the row")

	// The state itself still holds what the events said: scrubbing is the
	// row's job, so nothing else has to know about it.
	require.Len(t, st.Deliveries, 1)
	require.Equal(t, dirty, st.Deliveries[0].Branch, "the state is expected to hold the delivery verbatim")

	frame := prsFrame(t, prsView(w, h, PRsOptions{}), st, w, h)
	for i, line := range strings.Split(frame, "\n") {
		plain := sanPlain(line)
		require.False(t, sanControl(plain), "control character in frame line %d: %q", i, plain)
		require.LessOrEqual(t, lipgloss.Width(line), w, "line %d is wider than the terminal: %q", i, line)
	}
	require.Contains(t, sanPlain(frame), "rewritten", "the title's text survives the scrub")
}

// TestPublishedOutputIsCleanEverywhere is P7-17's acceptance. The scrub that
// matters happens once, where the event is built out of a subprocess's bytes
// (logging.SafeEvent), so every consumer of the bus — the JSONL run log, the
// headless renderer and the TUI's own state — sees text that is already
// safe. The views keep their scrubbing as an idempotent second pass for
// events built elsewhere, which is why the frame is clean either way.
func TestPublishedOutputIsCleanEverywhere(t *testing.T) {
	const run domain.RunID = "sc-san-0002"
	at := time.Date(2026, 5, 6, 12, 1, 3, 0, time.UTC)
	raw := events.AgentOutput{
		Meta: events.MetaAt(run, at), Agent: "eng-1", Stream: events.StreamStdout,
		Line: "\x1b[2J\x1b[31mstep 1\x1b[0m\x07 done\ttail",
	}

	// What a publisher does before it hands the event to the bus.
	published := logging.SafeEvent(raw)
	out, ok := published.(events.AgentOutput)
	require.True(t, ok, "the hook must not change the event type")
	line := out.Line
	require.Equal(t, "step 1 done    tail", line, "the publisher's copy is the clean one")

	// The run log stores exactly that.
	var file strings.Builder
	log := events.NewJSONLWriter(&file)
	require.NoError(t, log.Write(published))
	record := strings.TrimSuffix(file.String(), "\n") // the record's own line break
	require.False(t, sanControl(record), "the run log kept a control character: %q", record)
	require.Contains(t, record, "step 1 done")

	// So does the headless renderer.
	headless := FormatHeadless(published, time.UTC)
	require.False(t, sanControl(headless), "the headless line kept a control character: %q", headless)
	require.Contains(t, headless, "step 1 done")

	// And the TUI's state, which no longer has to hold raw bytes for the
	// view to clean up at draw time.
	st := NewState(domain.Run{ID: run, Mode: domain.RunModeBrief})
	st.Apply(events.AgentSpawned{
		Meta: events.MetaAt(run, at), Task: events.TaskImplement, Round: 1, Attempt: 1,
		Agent: domain.Agent{ID: "eng-1", Role: domain.RoleEngineer, Bead: "sc-1",
			Cartridge: "mock", State: domain.AgentStateRunning, StartedAt: at},
	})
	st.Apply(published)
	a, ok := st.Agent("eng-1")
	require.True(t, ok)
	require.Equal(t, line, a.LastLine, "the state holds what was published, and it is already clean")
	require.False(t, sanControl(a.LastLine))
}
