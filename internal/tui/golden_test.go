package tui_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// P7-09 — whole-frame golden tests of the TUI.
//
// Every frame here is rendered from the scripted demo run ([tui.DemoEvents]),
// which carries fixed UTC timestamps, at a fixed terminal size and through
// the no-colour theme, so a frame is byte-for-byte reproducible on any
// machine. "Any machine" is enforced, not hoped for: [TestMain] pins the
// global Lip Gloss renderer and the animation environment variable, and
// [TestGoldenFramesIgnoreTheGlobalRenderer] re-renders every frame under
// every colour profile a terminal can present. The per-view goldens
// (agents-*, graph-*, feedback-*, prs-*, anim-*, headless*) belong to the
// tasks that own those views and are listed in gfViewGoldens so no stale one
// can survive; these are the composed frames: chrome, pane and overlays
// together, exactly as the user sees them.
//
// Updating: `make update-golden-tui` (or UPDATE_GOLDEN=1 go test
// ./internal/tui/...) rewrites the files; `make check-golden` is the check
// that they are current — CI runs it as its own step — and `make check`
// fails on a stale frame like any other test failure.

// gfGoldenDir is where the golden frames live, relative to this file.
const gfGoldenDir = "../../testdata/tui"

// gfPrefix keeps the composed frames apart from the per-view goldens in the
// same directory.
const gfPrefix = "frame-"

// gfWidth and gfHeight are the deterministic terminal every frame is
// rendered in unless a scenario asks for another size.
const (
	gfWidth  = 80
	gfHeight = 24
)

// gfWaitTimeout is how long the teatest program may take to draw a frame. It
// is generous because a loaded CI box renders slowly.
const gfWaitTimeout = 10 * time.Second

// gfDemo is the scripted run every frame is built from: fixed run id, fixed
// start time, no wall-clock pacing.
var gfDemo = tui.DemoOptions{Delay: -1}

// gfBrief is the same script as a brief run with safety on, so the header
// and the delivery it renders differ from the autopilot frames.
var gfBrief = tui.DemoOptions{Delay: -1, Brief: "make the http client retry", Safety: true}

// gfEastAsianEnv is the ambiguous-width switch. Both go-runewidth and
// github.com/charmbracelet/x/ansi — the width measurement under every Lip
// Gloss pad, truncate and join — read it once, in a package initialiser, and
// keep the answer in an unexported condition that no test can reach. A
// process that starts with it set therefore measures '·', '✓' and '▌' as two
// cells for its whole life, which shifts the padding of every frame.
const gfEastAsianEnv = "RUNEWIDTH_EASTASIAN"

// gfWidthChildEnv marks the process [TestMain] re-executed with
// [gfEastAsianEnv] cleared, so that process never re-executes itself again.
const gfWidthChildEnv = "SC_TUI_GOLDEN_NARROW_CHILD"

// gfEastAsianWidth reports whether this process was started in the width mode
// where East-Asian ambiguous characters take two cells.
func gfEastAsianWidth() bool {
	v, err := strconv.ParseBool(os.Getenv(gfEastAsianEnv))
	return err == nil && v
}

// gfRerunWithNarrowWidth re-executes this test binary with [gfEastAsianEnv]
// removed and returns the child's exit code, when this process was started
// with the ambiguous-width mode on and is not one of the two processes that
// want it: the child [TestThemeGlyphColumnHoldsUnderEastAsianWidth] spawns to
// exercise that mode on purpose, and a child this function already spawned.
//
// It has to be a new process. The width mode is latched at package
// initialisation, so clearing the variable from inside a running test is too
// late, and there is no exported knob to override it — see
// [TestGoldenFramesSurviveEastAsianWidth] for the guarantee this buys.
func gfRerunWithNarrowWidth() (code int, rerun bool) {
	if !gfEastAsianWidth() || os.Getenv(gfWidthChildEnv) != "" || os.Getenv(themeEAChildEnv) != "" {
		return 0, false
	}
	cmd := exec.Command(os.Args[0], os.Args[1:]...) //nolint:gosec // re-runs this very test binary
	cmd.Env = append(gfEnvWithout(os.Environ(), gfEastAsianEnv), gfWidthChildEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, true
	case errors.As(err, &exit):
		return exit.ExitCode(), true
	default:
		fmt.Fprintf(os.Stderr, "tui: re-running the test binary without %s: %v\n", gfEastAsianEnv, err)
		return 1, true
	}
}

// gfEnvWithout returns env without any assignment of the named variable.
func gfEnvWithout(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == name {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// TestMain pins the ambient rendering environment before a single frame is
// drawn, so the whole package renders the same way on every machine.
//
// Three things leak in otherwise. The global Lip Gloss renderer detects a
// colour profile from the process's stdout: Ascii under a piped `go test`,
// but ANSI or truecolor under CLICOLOR_FORCE, or whenever the compiled test
// binary is run on a tty. Nothing in this package may build a style from
// that renderer — every style comes from a [tui.Theme], which pins its own
// profile — so the profile is set here to the *most* colourful one: a style
// that slipped through would emit a truecolor escape and fail
// [TestGoldenFramesCarryNoEscapes] loudly, instead of silently rendering
// plain for whoever happens to run the suite through a pipe. The second leak
// is SC_NO_ANIM, which would otherwise switch the animated frame off in a
// shell that exports it. The third is RUNEWIDTH_EASTASIAN, which cannot be
// unset from here at all because the width libraries latch it at start-up:
// the whole run is handed to a child process without it instead
// (gfRerunWithNarrowWidth), so a developer who exports that variable gets
// the same frames as CI rather than 91 failures — and, worse, rather than a
// `make update-golden-tui` that rewrites every frame with two-cell padding
// (P7-09).
func TestMain(m *testing.M) {
	if code, rerun := gfRerunWithNarrowWidth(); rerun {
		os.Exit(code)
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	if err := os.Unsetenv(tui.NoAnimEnv); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// gfScenario is one golden frame: a model built from the scripted run,
// rendered at a fixed size.
type gfScenario struct {
	// name is the golden file, without the frame- prefix and .golden suffix.
	name string
	// w and h are the terminal size; zero means gfWidth × gfHeight.
	w, h int
	// build returns the model to render.
	build func(t *testing.T) tui.Model
}

// gfScenarios is every composed frame the suite pins.
func gfScenarios() []gfScenario {
	return []gfScenario{{
		// Before a single event: the header is already populated from the
		// run, and every view is empty.
		name:  "startup",
		build: func(t *testing.T) tui.Model { return gfNew(t, gfDemo, gfWidth, gfHeight) },
	}, {
		// Mid-run: the plan landed, bead A is in review, agents are live.
		name: "midrun",
		build: func(t *testing.T) tui.Model {
			return gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindVerdictReceived))
		},
	}, {
		name:  "graph",
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, gfWidth, gfHeight) },
	}, {
		name: "agents",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("2"))
		},
	}, {
		name: "feedback",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("3"))
		},
	}, {
		name: "prs",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("4"))
		},
	}, {
		// The log view follows the stream, so it is pinned to the last line.
		name: "log",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("5"))
		},
	}, {
		// …until the cursor is moved, which stops it following.
		name: "log-scrolled",
		build: func(t *testing.T) tui.Model {
			m := gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("5"))
			for range 6 {
				m = gfSend(m, gfKey("up"))
			}
			return m
		},
	}, {
		name: "help",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, gfWidth, gfHeight), gfKey("?"))
		},
	}, {
		// `q` on a live run asks first; the confirmation replaces the pane.
		name: "confirm",
		build: func(t *testing.T) tui.Model {
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindAgentSpawned))
			return gfSend(m, gfKey("q"))
		},
	}, {
		// A subscription that fell behind is reported in the footer.
		name: "dropped",
		build: func(t *testing.T) tui.Model {
			evs := tui.DemoEvents(gfDemo)
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), evs[:len(evs)-1])
			return gfSend(m, tui.EventMsg{Event: evs[len(evs)-1], Dropped: 7})
		},
	}, {
		// A stream that ended before the run did says so in the header.
		name: "disconnected",
		build: func(t *testing.T) tui.Model {
			m := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindRunStopped))
			return gfSend(m, tui.StreamClosedMsg{})
		},
	}, {
		// A brief run with safety on, on the deliveries view.
		name: "brief-prs",
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfBrief, gfWidth, gfHeight), gfKey("4"))
		},
	}, {
		name: "wide",
		w:    120, h: 40,
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, 120, 40) },
	}, {
		name: "narrow",
		w:    60, h: 14,
		build: func(t *testing.T) tui.Model {
			return gfSend(gfWhole(t, gfDemo, 60, 14), gfKey("2"))
		},
	}, {
		// The smallest terminal the model still draws chrome in.
		name: "tiny",
		w:    24, h: 5,
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, 24, 5) },
	}, {
		// Shorter than the chrome itself: the frame must still be exactly
		// the terminal, so the display does not scroll.
		name: "shortest",
		w:    40, h: 2,
		build: func(t *testing.T) tui.Model { return gfWhole(t, gfDemo, 40, 2) },
	}, {
		// The animation layer on: sprites are composited over the finished
		// chrome, so a projectile that drew past the pane or trampled the
		// header would change this frame. The layer reads no clock and seeds
		// its particles from a constant, so stepping it by hand is exact.
		name: "anim",
		build: func(t *testing.T) tui.Model {
			m := gfFeed(gfNewAnim(t, gfDemo, gfWidth, gfHeight), gfUntil(gfDemo, events.KindAgentSpawned))
			return gfAnimTick(t, m)
		},
	}}
}

// TestGoldenFrames is the P7-09 acceptance test: every composed frame of the
// scripted run, at a fixed size, compared with its golden file.
func TestGoldenFrames(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			w, h := sc.size()
			frame := sc.build(t).View()
			gfRequireGeometry(t, frame, w, h)
			gfGolden(t, sc.name, frame)
		})
	}
}

// TestGoldenFramesAreDeterministic renders every frame twice from two
// independent models and requires the two to be identical: a wall clock, a
// map iteration or a random seed leaking into a view would show up here
// rather than as a golden that only fails on someone else's machine.
func TestGoldenFramesAreDeterministic(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			first := sc.build(t).View()
			second := sc.build(t).View()
			require.Equal(t, first, second, "frame %q is not reproducible", sc.name)
		})
	}
}

// gfAttrEscape matches an SGR sequence built only from the attributes a
// no-colour frame is allowed to carry: reset (0), bold (1), underline (4)
// and the two that switch them off again (22, 24). Anything else — a colour
// in any of its forms, a cursor move, an erase — does not match, so the test
// below rejects it.
var gfAttrEscape = regexp.MustCompile(`^\x1b\[(?:0|1|4|22|24)?(?:;(?:0|1|4|22|24))*m$`)

// TestGoldenFramesCarryNoEscapes proves the frames the goldens hold carry no
// colour: with the no-colour theme nothing may emit a colour escape, so a
// golden reads the same on a truecolor terminal, under NO_COLOR, and in a CI
// log. Bold and underline survive, deliberately: they are how the cursor,
// the selected box and the active tab stay visible when colour is gone
// (P7-07).
//
// [TestMain] pins the global Lip Gloss renderer to truecolor, so this is
// also the check that no style escaped the theme: such a style would render
// its colour here and be caught, whatever terminal the suite is run from.
func TestGoldenFramesCarryNoEscapes(t *testing.T) {
	t.Parallel()
	for _, sc := range gfScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			frame := sc.build(t).View()
			for _, esc := range gfAnyEscape.FindAllString(frame, -1) {
				require.Regexp(t, gfAttrEscape, esc,
					"frame %q carries %q: an escape that is not bold, underline or reset", sc.name, esc)
			}
		})
	}
}

// TestGoldenFramesIgnoreTheGlobalRenderer is the direct proof of the promise
// the goldens make. It flips the global Lip Gloss renderer through every
// colour profile a terminal can present — the Ascii a piped `go test` sees,
// the ANSI of CLICOLOR_FORCE, and the truecolor of a modern tty — and
// requires every frame to come out identical. It cannot run in parallel with
// anything, because the renderer it moves is process-wide.
func TestGoldenFramesIgnoreTheGlobalRenderer(t *testing.T) {
	restore := termenv.TrueColor
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	for _, sc := range gfScenarios() {
		lipgloss.SetColorProfile(termenv.Ascii)
		want := sc.build(t).View()
		for _, p := range []termenv.Profile{termenv.ANSI, termenv.ANSI256, termenv.TrueColor} {
			lipgloss.SetColorProfile(p)
			require.Equal(t, want, sc.build(t).View(),
				"frame %q changed with the global renderer profile; build every style from the theme", sc.name)
		}
	}
}

// gfEAChildEnv marks the child process
// [TestGoldenFramesSurviveEastAsianWidth] starts, so that child does not
// start one of its own.
const gfEAChildEnv = "SC_TUI_GOLDEN_EASTASIAN_CHILD"

// TestGoldenFramesSurviveEastAsianWidth runs the golden suite again in a
// child process started with RUNEWIDTH_EASTASIAN=1 — the variable a terminal
// configured for ambiguous-width 2 exports, and the one setting that would
// otherwise silently re-pad every frame. The child must pass: [TestMain]
// hands the run on to a process without the variable, so the frames a
// developer with it exported renders, compares and regenerates are the ones
// CI has.
//
// The proof has to be a second process, because the width libraries latch
// the mode at package initialisation.
func TestGoldenFramesSurviveEastAsianWidth(t *testing.T) {
	t.Parallel()

	if os.Getenv(gfEAChildEnv) != "" || gfEastAsianWidth() {
		t.Skip("already running under the ambiguous-width mode")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // re-runs this very test binary
		"-test.run", "^TestGoldenFrames", "-test.count=1", "-test.timeout=5m")
	// Two variables are cleared from the child's environment. UPDATE_GOLDEN,
	// because a rewrite must never come from a process whose width mode is
	// in question; and gfWidthChildEnv, which this process inherits when it
	// is itself the re-executed child of a hostile shell — leaving it set
	// would tell the child it had already been through [TestMain]'s re-exec,
	// and the test would then measure the very failure it exists to prevent.
	env := gfEnvWithout(gfEnvWithout(os.Environ(), "UPDATE_GOLDEN"), gfWidthChildEnv)
	cmd.Env = append(env, gfEastAsianEnv+"=1", gfEAChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "the golden frames fail with %s=1:\n%s", gfEastAsianEnv, out)
}

// TestGoldenFrameWithAnimationDrawsSprites proves the animated scenario is
// worth pinning: the layer really did composite something over the chrome,
// so the golden would notice a projectile that drew in the wrong place.
func TestGoldenFrameWithAnimationDrawsSprites(t *testing.T) {
	t.Parallel()
	evs := gfUntil(gfDemo, events.KindAgentSpawned)
	still := gfFeed(gfNew(t, gfDemo, gfWidth, gfHeight), evs).View()
	animated := gfAnimTick(t, gfFeed(gfNewAnim(t, gfDemo, gfWidth, gfHeight), evs)).View()
	require.NotEqual(t, still, animated, "the animated frame draws no sprites")
	require.Equal(t, len(strings.Split(still, "\n")), len(strings.Split(animated, "\n")),
		"the animation layer changed the height of the frame")
}

// gfAnyEscape matches any ANSI escape sequence.
var gfAnyEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// gfPlain strips every ANSI escape, so a comparison sees the characters the
// user reads rather than how the renderer chose to group its SGR runs.
func gfPlain(s string) string { return gfAnyEscape.ReplaceAllString(s, "") }

// gfViewGoldens is the inventory of the per-view goldens in the same
// directory: one component's pane or one headless render, each owned by the
// task that wrote it (graph P7-02, agents P7-03, feedback P7-04, PRs P7-05,
// anim P7-06, headless P7-08). The composed frame- files are not listed
// because [gfScenarios] already names them.
//
// It is here so that renaming or dropping a per-view scenario cannot leave a
// stale file behind: [TestGoldenFrameFilesAreReadable] requires the
// directory to hold exactly these plus one frame per scenario. Adding a
// scenario to any view means adding its file here in the same change.
var gfViewGoldens = []string{
	"agents-empty", "agents-history", "agents-killed", "agents-live",
	"agents-narrow", "agents-transcript", "agents-transcript-ended", "agents-transcript-scrolled",
	"agents-wide",
	"anim-burst", "anim-confetti", "anim-disabled", "anim-over-frame",
	"anim-queue", "anim-ricochet", "anim-shot", "anim-shot-tab",
	"anim-smoke", "feedback", "feedback-arbitration", "feedback-changes",
	"feedback-details", "feedback-empty", "feedback-escalation", "feedback-narrow",
	"feedback-pushback", "feedback-rereview", "feedback-rules", "feedback-scrolled",
	"graph-50-200x50", "graph-50-80x24", "graph-chain-200x50", "graph-chain-80x24",
	"graph-cursor", "graph-cycle", "graph-cycle-narrow", "graph-detail",
	"graph-detail-scrolled", "graph-diamond-200x50", "graph-diamond-80x24", "graph-empty",
	"graph-fanout-200x50", "graph-fanout-80x24", "graph-narrow", "graph-skip",
	"graph-skip-vertical", "graph-vertical", "headless", "headless-json",
	"headless-sparse", "prs", "prs-empty", "prs-external",
	"prs-narrow", "prs-safety-off",
}

// TestGoldenFrameFilesAreReadable guards the golden files themselves, every
// one in testdata/tui and not only the composed frames: a golden is text,
// never empty, never carries a carriage return that would make the
// comparison platform-dependent, and is produced by something. A file no
// test writes any more is an orphan, and a scenario with no file has never
// been generated; both fail here.
func TestGoldenFrameFilesAreReadable(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join(gfGoldenDir, "*.golden"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "no goldens in %s; run `make update-golden-tui`", gfGoldenDir)

	want := make(map[string]bool, len(gfScenarios())+len(gfViewGoldens))
	for _, sc := range gfScenarios() {
		want[filepath.Join(gfGoldenDir, gfPrefix+sc.name+".golden")] = true
	}
	for _, name := range gfViewGoldens {
		require.False(t, strings.HasPrefix(name, gfPrefix),
			"%q is a composed frame; gfScenarios already owns it", name)
		path := filepath.Join(gfGoldenDir, name+".golden")
		require.False(t, want[path], "%q is listed twice", name)
		want[path] = true
	}

	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a test-owned path
		require.NoError(t, err)
		require.NotEmpty(t, body, "%s is empty", path)
		require.True(t, utf8.Valid(body), "%s is not valid UTF-8", path)
		require.NotContains(t, string(body), "\r", "%s has a carriage return", path)
		require.True(t, want[path],
			"%s is not produced by any scenario; delete it, or add it to gfScenarios or gfViewGoldens", path)
	}
	require.Len(t, matches, len(want),
		"every scenario needs a golden; run `make update-golden` and check gfViewGoldens")
}

// gfCheckGoldenPathspec is the pathspec `make check-golden` passes to `git
// status` when it decides whether a regenerated golden changed. Every golden
// in the repo has to be covered by it: one that is not is regenerated by the
// target and then silently ignored, so the gate reports a clean tree while
// the file on disk is stale (P7-09).
var gfCheckGoldenPathspec = regexp.MustCompile(`git status --porcelain --(.*)\)`)

// TestCheckGoldenCoversEveryGoldenSuffix reads the Makefile and requires the
// check-golden pathspec to match every golden file in the repository, whatever
// its extension: `*.golden` alone silently skips the 22 `*.golden.md` prompt
// goldens, because a git pathspec is matched against the whole name.
func TestCheckGoldenCoversEveryGoldenSuffix(t *testing.T) {
	t.Parallel()
	mk, err := os.ReadFile("../../Makefile") //nolint:gosec // the repository's own Makefile
	require.NoError(t, err)
	m := gfCheckGoldenPathspec.FindStringSubmatch(string(mk))
	require.Len(t, m, 2, "the check-golden target no longer runs `git status --porcelain --`")
	patterns := gfPathspecPatterns(m[1])
	require.NotEmpty(t, patterns)

	goldens := gfRepoGoldens(t)
	require.NotEmpty(t, goldens, "no golden files found under the repository root")
	for _, name := range goldens {
		require.Truef(t, gfPathspecMatches(patterns, name),
			"`make check-golden` ignores %s: its pathspec (%s) does not match that name, so a stale copy passes the gate",
			name, strings.Join(patterns, " "))
	}
}

// gfPathspecPatterns splits the quoted words of a shell pathspec list.
func gfPathspecPatterns(list string) []string {
	var out []string
	for _, f := range strings.Fields(list) {
		if f = strings.Trim(f, `'"`); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// gfPathspecMatches reports whether any pattern matches the file's base name,
// which is how git matches a leading-`*` pathspec against a tracked path.
func gfPathspecMatches(patterns []string, base string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(strings.TrimPrefix(p, "*/"), base); err == nil && ok {
			return true
		}
	}
	return false
}

// gfRepoGoldens returns the distinct base names of every golden file in the
// repository, one per extension, so the assertion above names a real file.
func gfRepoGoldens(t *testing.T) []string {
	t.Helper()
	seen := map[string]string{}
	skip := map[string]bool{".git": true, "bin": true, "dist": true, "vendor": true}
	require.NoError(t, filepath.WalkDir("../..", func(_ string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			// A file another test is writing right now is not this
			// test's business; the walk carries on.
			return nil
		case d.IsDir():
			if skip[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		base := d.Name()
		if i := strings.Index(base, ".golden"); i >= 0 {
			seen[base[i:]] = base
		}
		return nil
	}))
	out := make([]string, 0, len(seen))
	for _, base := range seen {
		out = append(out, base)
	}
	sort.Strings(out)
	return out
}

// TestGoldenFramesMatchTheProgram runs the real Bubble Tea program over the
// same scripted run and requires it to draw the frame the goldens hold: the
// goldens describe what the user sees, not just what Model.View returns in a
// test.
func TestGoldenFramesMatchTheProgram(t *testing.T) {
	t.Parallel()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	m := tui.New(t.Context(), tui.Options{
		Bus:     bus,
		Run:     tui.DemoRun(gfDemo),
		NoColor: true,
		Theme:   tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:  true,
		Now:     gfClock,
	})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(gfWidth, gfHeight))

	evs := tui.DemoEvents(gfDemo)
	for _, ev := range evs {
		bus.Publish(ev)
	}

	// The graph view is the one the program opens on, so its golden is the
	// frame the program has to end up drawing.
	want := gfLoad(t, "graph")
	out := gfWaitFor(t, tm, gfMeaningfulLines(want)...)

	tm.Send(gfKey("q"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(gfWaitTimeout))
	final, ok := tm.FinalModel(t).(tui.Model)
	require.True(t, ok, "the final model is not a tui.Model")
	require.Equal(t, tui.TabGraph, final.Tab())
	require.Equal(t, len(evs), final.State().Events, "the program missed events; drawn so far:\n%s", out)
	require.Zero(t, final.State().Dropped)
	require.False(t, final.State().Live, "the scripted run finishes, so `q` must not have asked")
}

// TestGoldenHelperRewritesTheFileWhenAsked covers the update path itself, in
// a temporary directory: with UPDATE_GOLDEN set the helper writes what it
// was given, and the file it wrote then compares equal.
func TestGoldenHelperRewritesTheFileWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame-example.golden")

	t.Setenv("UPDATE_GOLDEN", "1")
	gfCompare(t, path, "first\nsecond\n")
	body, err := os.ReadFile(path) //nolint:gosec // a temporary test path
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", string(body))

	t.Setenv("UPDATE_GOLDEN", "")
	gfCompare(t, path, "first\nsecond\n")
}

// size returns the terminal the scenario renders in.
func (sc gfScenario) size() (int, int) {
	w, h := sc.w, sc.h
	if w <= 0 {
		w = gfWidth
	}
	if h <= 0 {
		h = gfHeight
	}
	return w, h
}

// gfNew builds a model with no event stream, sized to w×h, with colour and
// animation off so the frame is reproducible.
func gfNew(t *testing.T, demo tui.DemoOptions, w, h int) tui.Model {
	t.Helper()
	return gfBuild(t, demo, w, h, true)
}

// gfNewAnim is [gfNew] with the animation layer on. The layer reads no clock
// and seeds its particles from a constant, so a frame taken between two
// hand-driven ticks is still exact.
func gfNewAnim(t *testing.T, demo tui.DemoOptions, w, h int) tui.Model {
	t.Helper()
	return gfBuild(t, demo, w, h, false)
}

// gfBuild is the model constructor both of those share. The theme is pinned
// explicitly rather than left to the environment: a frame must not change
// because the machine running the test has NO_COLOR, COLORTERM or a tty.
func gfBuild(t *testing.T, demo tui.DemoOptions, w, h int, noAnim bool) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:     tui.DemoRun(demo),
		NoColor: true,
		Theme:   tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:  noAnim,
		Now:     gfClock,
	})
	return gfSend(m, tea.WindowSizeMsg{Width: w, Height: h})
}

// gfAnimTick advances the animation layer by one frame. The ticker
// generation is not exported, so the helper tries the handful a freshly fed
// model can be on and keeps the tick that changed the frame; a model with
// nothing queued fails the test rather than pinning a still frame that says
// nothing about the overlay.
func gfAnimTick(t *testing.T, m tui.Model) tui.Model {
	t.Helper()
	before := m.View()
	for gen := range uint64(4) {
		if next := gfSend(m, tui.AnimTickMsg{Gen: gen}); next.View() != before {
			return next
		}
	}
	t.Fatal("no animation tick changed the frame: nothing was queued, or the layer is disabled")
	return m
}

// gfClock is the fixed clock the frames are rendered against, so an elapsed
// time in a live agent's card cannot make a frame depend on when it ran.
func gfClock() time.Time { return tui.DemoStart.Add(20 * time.Minute) }

// gfWhole builds a model that has seen the whole scripted run.
func gfWhole(t *testing.T, demo tui.DemoOptions, w, h int) tui.Model {
	t.Helper()
	return gfFeed(gfNew(t, demo, w, h), tui.DemoEvents(demo))
}

// gfFeed applies every event as the update loop would.
func gfFeed(m tui.Model, evs []events.Event) tui.Model {
	for _, ev := range evs {
		m = gfSend(m, tui.EventMsg{Event: ev})
	}
	return m
}

// gfUntil returns the scripted events up to and including the first one of
// the given kind, so a frame can be taken from the middle of the run.
func gfUntil(demo tui.DemoOptions, kind events.Kind) []events.Event {
	evs := tui.DemoEvents(demo)
	for i, ev := range evs {
		if ev.Kind() == kind {
			return evs[:i+1]
		}
	}
	return evs
}

// gfSend applies messages to the model and returns the model they produced,
// dropping the commands: a golden frame is a still.
func gfSend(m tui.Model, msgs ...tea.Msg) tui.Model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		out, ok := next.(tui.Model)
		if !ok {
			panic("tui: Update returned something that is not a tui.Model")
		}
		m = out
	}
	return m
}

// gfKey builds a key press from its printable spelling.
func gfKey(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// gfRequireGeometry proves the frame is exactly the terminal: h lines, none
// of them wider than w columns.
func gfRequireGeometry(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	require.Len(t, lines, h, "the frame is not %d lines", h)
	for i, l := range lines {
		require.LessOrEqual(t, lipgloss.Width(l), w, "line %d is wider than %d columns: %q", i, w, l)
	}
}

// gfGolden compares got with testdata/tui/frame-<name>.golden. Set
// UPDATE_GOLDEN=1 (make update-golden-tui) to rewrite the file instead.
func gfGolden(t *testing.T, name, got string) {
	t.Helper()
	gfCompare(t, filepath.Join(gfGoldenDir, gfPrefix+name+".golden"), got)
}

// gfCompare is the golden comparison itself, on an explicit path.
func gfCompare(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644)) //nolint:gosec // goldens are readable fixtures
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a test-owned path
	require.NoError(t, err, "missing golden %s; run `make update-golden-tui`", path)
	require.Equal(t, string(want), got, "frame differs from %s; run `make update-golden-tui` if the change is intended", path)
}

// gfLoad reads a golden frame, failing the test when it is missing.
func gfLoad(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(gfGoldenDir, gfPrefix+name+".golden")
	body, err := os.ReadFile(path) //nolint:gosec // a test-owned path
	require.NoError(t, err, "missing golden %s; run `make update-golden-tui`", path)
	return string(body)
}

// gfMeaningfulLines returns the lines of a frame worth looking for in a
// program's output: blank and one-glyph lines say nothing, and the renderer
// drops trailing spaces, so they are trimmed off.
func gfMeaningfulLines(frame string) []string {
	out := make([]string, 0, 8)
	for _, l := range strings.Split(frame, "\n") {
		l = strings.TrimRight(l, " ")
		if len([]rune(strings.TrimSpace(l))) < 8 {
			continue
		}
		out = append(out, l)
	}
	return out
}

// gfWaitFor blocks until every string has appeared in the program's output,
// and returns everything drawn so far. teatest's reader is drained by each
// read, so the output is accumulated here and every expectation is checked
// against the whole of it.
func gfWaitFor(t *testing.T, tm *teatest.TestModel, want ...string) string {
	t.Helper()
	var seen strings.Builder
	deadline := time.Now().Add(gfWaitTimeout)
	for {
		chunk, err := io.ReadAll(tm.Output())
		require.NoError(t, err, "reading the program output")
		seen.Write(chunk)
		out := seen.String()

		plain := gfPlain(out)
		missing := ""
		for _, w := range want {
			if !strings.Contains(plain, gfPlain(w)) {
				missing = w
				break
			}
		}
		if missing == "" {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %q; drawn so far:\n%s", gfWaitTimeout, missing, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
