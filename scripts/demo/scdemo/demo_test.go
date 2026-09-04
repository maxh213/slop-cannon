package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge/mock"
)

// The demo's own tests (P8-07). `make demo DEMO_ARGS=--check` proves the
// scripted run still raises the event behind every cannon animation, but it
// is a shell script nobody runs by accident: nothing in `go test ./...`
// touched scripts/demo/scenario.json or demo.tape, so both could rot
// silently. These tests are the cheap half of that guard — they load the
// scenario through the real decoder and read the recording script the way
// VHS will — and they run wherever `go test ./...` does.

// demoRoot is the repository root from this package's directory.
const demoRoot = "../../.."

// demoTUIKeyBudget is how long a key press in demo.tape may be deferred.
// `sc run` owns the terminal only while the run is going: renderTUI cancels
// the Bubble Tea program as soon as the orchestrator finishes, so a key
// typed after that lands on the shell prompt and is recorded as a stray
// character rather than a view switch. The scripted run takes about 13.6s
// end to end (measured; it is the sum of the scenario's own delays), and
// this leaves a safety margin of about a third of that.
const demoTUIKeyBudget = 10 * time.Second

// demoMinTail is how long the tape must keep recording after the last key,
// so the GIF covers the end of the run: the beads closing, the summary sc
// prints and the first-run notice under it.
const demoMinTail = 8 * time.Second

// demoScenarioPath and demoTapePath are what the tests read.
var (
	demoScenarioPath = filepath.Join(demoRoot, "scripts", "demo", "scenario.json")
	demoTapePath     = filepath.Join(demoRoot, "demo.tape")
	demoGIFPath      = filepath.Join(demoRoot, "docs", "demo.gif")
	demoREADMEPath   = filepath.Join(demoRoot, "README.md")
)

// demoStructured is the part of a scripted result these tests read back: a
// plan's beads and a verdict's outcome.
type demoStructured struct {
	Beads   []json.RawMessage `json:"beads"`
	Outcome string            `json:"outcome"`
}

// TestDemoScenarioExercisesEveryAnimation loads scripts/demo/scenario.json
// through the mock cartridge's own decoder — so a typo, an unknown field or
// an invalid step fails here rather than in the middle of a recording — and
// then checks it still scripts the reply behind each of the five cannon
// animations (P7-06), which is what demo.sh --check asserts from the run's
// event log.
func TestDemoScenarioExercisesEveryAnimation(t *testing.T) {
	s, err := mock.Load(demoScenarioPath)
	require.NoError(t, err, "the demo scenario must load and validate")
	require.NoError(t, s.Validate())
	require.NotEmpty(t, s.Rules)

	var (
		planBeads int    // shots: an agent per bead, and confetti when they close
		malformed string // smoke: the engineer whose first reply is broken
		changes   string // ricochet: the QA lens asking for changes
		approves  int    // burst: an approving verdict
	)
	for _, r := range s.Rules {
		for _, st := range r.Steps {
			if st.Kind == mock.StepFail && st.Failure != nil && st.Failure.Kind == mock.FailMalformed {
				malformed = r.Name
			}
		}
		if len(r.Result.Structured) == 0 {
			continue
		}
		var out demoStructured
		require.NoError(t, json.Unmarshal(r.Result.Structured, &out), "rule %q", r.Name)
		if n := len(out.Beads); n > planBeads {
			planBeads = n
		}
		switch out.Outcome {
		case "approve":
			approves++
		case "changes":
			changes = r.Name
		}
	}

	assert.GreaterOrEqual(t, planBeads, 2, "the plan must create at least two beads, or the graph has nothing to show")
	assert.NotEmpty(t, malformed, "no rule fails with a malformed reply: the demo would show no smoke")
	assert.NotEmpty(t, changes, "no rule asks for changes: the demo would show no ricochet")
	assert.Positive(t, approves, "no rule approves: the demo would show no burst and never deliver")

	// A firing no rule answers fails the run, which would end the demo
	// half-way through with no explanation on the recording.
	require.NotNil(t, s.Fallback, "the scenario needs a fallback so an unscripted firing is a clear failure")
}

// demoTapeKey matches a single-character Type line: the view switches.
var (
	demoTapeType  = regexp.MustCompile(`^Type\s+"(.*)"\s*$`)
	demoTapeSleep = regexp.MustCompile(`^Sleep\s+(\S+)\s*$`)
	demoTapeOut   = regexp.MustCompile(`^Output\s+(\S+)\s*$`)
)

// TestDemoTapeRecordsALiveRun reads demo.tape the way VHS will and pins the
// two things a wrong tape gets wrong. First, that it records what it says it
// records: `make demo`, into docs/demo.gif, after an off-camera build of
// packages that exist. Second, and this is the defect the tape shipped with:
// that every key it presses is pressed while the TUI is still up. The TUI is
// torn down the moment the run finishes, so a late key press is typed into
// the shell instead and the GIF ends on a prompt full of stray characters.
func TestDemoTapeRecordsALiveRun(t *testing.T) {
	data, err := os.ReadFile(demoTapePath)
	require.NoError(t, err)
	lines := strings.Split(string(data), "\n")

	var (
		output   string
		hidden   []string // Type lines before Show
		visible  bool
		started  bool // the recorded command has been entered
		elapsed  time.Duration
		lastKey  time.Duration
		keys     []string
		commands []string
	)
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case line == "Show":
			visible = true
			continue
		case line == "Enter":
			if len(commands) == 1 && !started {
				// The recorded command has been submitted; the clock the
				// run is measured against starts here.
				started, elapsed = true, 0
			}
			continue
		}
		if m := demoTapeOut.FindStringSubmatch(line); m != nil {
			require.Empty(t, output, "line %d: the tape writes more than one output", i+1)
			output = m[1]
			continue
		}
		if m := demoTapeSleep.FindStringSubmatch(line); m != nil {
			d, err := time.ParseDuration(m[1])
			require.NoError(t, err, "line %d: %q is not a duration", i+1, m[1])
			elapsed += d
			continue
		}
		if m := demoTapeType.FindStringSubmatch(line); m != nil {
			switch {
			case !visible:
				hidden = append(hidden, m[1])
			case len([]rune(m[1])) == 1 && started:
				keys = append(keys, m[1])
				lastKey = elapsed
			default:
				commands = append(commands, m[1])
			}
		}
	}

	assert.Equal(t, "docs/demo.gif", output, "the tape must write the GIF the README embeds")
	require.Equal(t, []string{"make demo"}, commands, "the tape records `make demo` and nothing else")

	// The off-camera warm-up builds packages that exist.
	require.Len(t, hidden, 1, "one hidden line, the build")
	for _, pkg := range regexp.MustCompile(`\./\S+`).FindAllString(hidden[0], -1) {
		_, err := os.Stat(filepath.Join(demoRoot, pkg))
		assert.NoError(t, err, "the tape builds %s, which does not exist", pkg)
	}

	// Every key is a TUI tab key, pressed while the run is still live.
	require.NotEmpty(t, keys, "the tape presses no keys: the GIF shows one view")
	for _, k := range keys {
		assert.Contains(t, []string{"1", "2", "3", "4", "5"}, k,
			"%q is not a tab key; `q` and `y` in particular would be typed into the shell "+
				"if the run has already finished, and are what the finished TUI does not wait for", k)
	}
	assert.Subset(t, keys, []string{"2", "3"}, "the tape must show the Agents and Feedback views")
	assert.Less(t, lastKey, demoTUIKeyBudget,
		"the last key is pressed %s into the run; the TUI is gone by then and the key lands on the shell prompt", lastKey)
	assert.GreaterOrEqual(t, elapsed-lastKey, demoMinTail,
		"the tape stops recording %s after the last key: too early to catch the end of the run and the summary", elapsed-lastKey)
}

// TestREADMEEmbedsTheDemoGIF is P8-07's Done-when: the recording exists and
// the README shows it. A GIF nobody can find is not a demo.
func TestREADMEEmbedsTheDemoGIF(t *testing.T) {
	info, err := os.Stat(demoGIFPath)
	require.NoError(t, err, "docs/demo.gif is missing: record it with `vhs demo.tape`")
	assert.Greater(t, info.Size(), int64(10_000), "docs/demo.gif looks empty")

	head, err := os.ReadFile(demoGIFPath)
	require.NoError(t, err)
	require.Greater(t, len(head), 6)
	assert.True(t, strings.HasPrefix(string(head), "GIF8"), "docs/demo.gif is not a GIF")

	readme, err := os.ReadFile(demoREADMEPath)
	require.NoError(t, err)
	text := string(readme)
	assert.Contains(t, text, "docs/demo.gif", "the README does not embed the recording")
	assert.Contains(t, text, "make demo", "the README does not say how to run the demo")
}
