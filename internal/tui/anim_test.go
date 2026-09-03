package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// animGoldenDir is where the animation golden frame sequences live.
const animGoldenDir = "../../testdata/tui"

// animGoldenWidth and animGoldenHeight are the terminal the golden sequences
// are rendered at: small enough to read a diff, big enough for an arc.
const (
	animGoldenWidth  = 64
	animGoldenHeight = 10
)

// animGoldenStep is how many frames pass between the stills a golden keeps.
// Recording all 30 frames a second would make the files unreadable.
const animGoldenStep = 3

// animTheme is the theme the tests render with: no colour, so a frame is
// exactly its glyphs and the goldens carry no ANSI.
func animTheme() *Theme { return ThemeFor(ProfileNoColor) }

// animTargetBead is the bead the golden sequences aim at.
const animTargetBead = domain.BeadID("sc-3c4d")

// animTargetCell is where the fake graph view says that bead's node is.
var animTargetCell = Target{Row: 5, Col: 40}

// animTargets is the graph view's node resolver, faked: one bead has a node,
// everything else falls back to a tab indicator.
func animTargets(bead domain.BeadID) (Target, bool) {
	if bead == animTargetBead {
		return animTargetCell, true
	}
	return Target{}, false
}

// animNewAnimator builds the animator the tests drive: deterministic theme,
// deterministic seed, a fixed size and the fake graph resolver.
func animNewAnimator(opts AnimOptions) Animator {
	if opts.Theme == nil {
		opts.Theme = animTheme()
	}
	if opts.Width == 0 {
		opts.Width = animGoldenWidth
	}
	if opts.Height == 0 {
		opts.Height = animGoldenHeight
	}
	if opts.Targets == nil {
		opts.Targets = animTargets
	}
	return NewAnimator(opts)
}

// animPaneRows is the fake main pane the sequences animate over.
var animPaneRows = []string{
	"  sc-1a2b  in_progress  Add retry to the HTTP client",
	"  sc-3c4d  in_review    Cache DNS lookups",
	"  sc-5e6f  approved     Drop the dead flag",
	"  sc-7g8h  open         Document the retry policy",
}

// animTabBar renders the tab bar exactly the way [Model.tabBar] does, so the
// tab indicators the animations aim at line up with the labels.
func animTabBar() string {
	var b strings.Builder
	for i, t := range Tabs() {
		fmt.Fprintf(&b, " %d %s ", i+1, t)
	}
	return b.String()
}

// animBaseFrame renders the terminal the sequences animate over: the header
// with the animated cannon in it, the tab bar, the rule, a small graph and
// the footer.
func animBaseFrame(a Animator, w, h int) string {
	lines := make([]string, h)
	lines[0] = animClipLine(a.Cannon()+"slop-cannon · run sc-anim-0001 · brief · ammo 2/4", w)
	lines[1] = animClipLine(animTabBar(), w)
	lines[2] = strings.Repeat("─", w)
	for i := 3; i < h-1; i++ {
		row := ""
		if j := i - 3; j < len(animPaneRows) {
			row = animPaneRows[j]
		}
		lines[i] = animClipLine(row, w)
	}
	lines[h-1] = animClipLine("tab next view · ↑/k up · ↓/j down · ? help · q stop", w)
	return strings.Join(lines, "\n")
}

// animClipLine truncates a line to w columns without padding it, so the
// overlay's own padding is exercised by the goldens.
func animClipLine(s string, w int) string { return ansi.Truncate(s, w, "") }

// animSequence plays an animator to the end and returns every animGoldenStep
// frame as text.
func animSequence(t *testing.T, a Animator, w, h int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; ; i++ {
		require.Less(t, i, 400, "the animation never finished")
		if i%animGoldenStep == 0 {
			c := a.CannonFrame()
			fmt.Fprintf(&b, "frame %03d  active %d  queued %d  recoil %d  flash %d\n", i, a.Active(), a.Queued(), c.Recoil, c.Flash)
			b.WriteString(a.Overlay(animBaseFrame(a, w, h)))
			b.WriteString("\n\n")
		}
		if a.Idle() {
			return b.String()
		}
		a = a.Advance()
	}
}

// injectSprites replaces whatever is playing with a single still frame, so an
// overlay test can pin exact cells.
func (a Animator) injectSprites(sprites []Sprite) Animator {
	a.active = []animation{{kind: AnimBurst, frames: [][]Sprite{sprites}}}
	return a
}

// animGolden compares got with testdata/tui/<name>.golden; UPDATE_GOLDEN=1
// (make update-golden) rewrites it instead.
func animGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(animGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "missing golden %s; run UPDATE_GOLDEN=1 go test ./internal/tui/", path)
	require.Equal(t, string(want), got, "golden %s is stale; run make update-golden", path)
}

func TestAnimGoldenSequences(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind AnimKind
	}{
		{"anim-shot", AnimShot},
		{"anim-burst", AnimBurst},
		{"anim-ricochet", AnimRicochet},
		{"anim-confetti", AnimConfetti},
		{"anim-smoke", AnimSmoke},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := animNewAnimator(AnimOptions{Tab: TabGraph})
			a, cmd := a.Fire(tc.kind, animTargetBead)
			require.NotNil(t, cmd, "firing an idle animator starts the ticker")
			animGolden(t, tc.name, animSequence(t, a, animGoldenWidth, animGoldenHeight))
		})
	}
}

func TestAnimGoldenQueue(t *testing.T) {
	// Five shots with two slots: three of them wait their turn.
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Max: 2})
	for i := 0; i < 5; i++ {
		a, _ = a.Fire(AnimShot, domain.BeadID(fmt.Sprintf("sc-%04d", i)))
	}
	require.Equal(t, 2, a.Active())
	require.Equal(t, 3, a.Queued())
	animGolden(t, "anim-queue", animSequence(t, a, animGoldenWidth, animGoldenHeight))
}

func TestAnimGoldenTabIndicatorFallback(t *testing.T) {
	// The graph is not on screen, so the shot flies at the Agents tab.
	a := animNewAnimator(AnimOptions{Tab: TabLog})
	a, _ = a.Fire(AnimShot, animTargetBead)
	animGolden(t, "anim-shot-tab", animSequence(t, a, animGoldenWidth, animGoldenHeight))
}

func TestAnimGoldenDisabled(t *testing.T) {
	a := animNewAnimator(AnimOptions{Disabled: true, Tab: TabGraph})
	a, cmd := a.Fire(AnimShot, animTargetBead)
	require.Nil(t, cmd)
	require.True(t, a.Idle())
	animGolden(t, "anim-disabled", animSequence(t, a, animGoldenWidth, animGoldenHeight))
}

func TestAnimGoldensCarryNoANSI(t *testing.T) {
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	a, _ = a.Fire(AnimConfetti, animTargetBead)
	require.NotContains(t, animSequence(t, a, animGoldenWidth, animGoldenHeight), "\x1b")
}

func TestAnimDisabledReadsFlagAndEnv(t *testing.T) {
	t.Parallel()
	env := func(v string, ok bool) func(string) (string, bool) {
		return func(k string) (string, bool) {
			require.Equal(t, NoAnimEnv, k)
			return v, ok
		}
	}
	require.True(t, AnimDisabled(true, env("", false)), "--no-anim wins")
	require.False(t, AnimDisabled(false, env("", false)), "unset means animate")
	require.False(t, AnimDisabled(false, env("", true)))
	require.False(t, AnimDisabled(false, env("0", true)))
	require.False(t, AnimDisabled(false, env("false", true)))
	require.False(t, AnimDisabled(false, env("OFF", true)))
	require.True(t, AnimDisabled(false, env("1", true)))
	require.True(t, AnimDisabled(false, env("yes", true)))
	require.True(t, AnimDisabled(false, env("anything", true)))
}

func TestAnimDisabledDefaultsToTheEnvironment(t *testing.T) {
	t.Setenv(NoAnimEnv, "1")
	require.True(t, AnimDisabled(false, nil))
	t.Setenv(NoAnimEnv, "0")
	require.False(t, AnimDisabled(false, nil))
}

func TestAnimKindsAreClosed(t *testing.T) {
	t.Parallel()
	kinds := AnimKinds()
	require.Len(t, kinds, 5)
	for _, k := range kinds {
		require.True(t, k.Valid(), k)
		require.Equal(t, string(k), k.String())
		require.True(t, k.homeTab().Valid())
	}
	require.False(t, AnimKind("").Valid())
	require.False(t, AnimKind("fireworks").Valid())
	require.Equal(t, TabGraph, AnimKind("fireworks").homeTab(), "an unknown animation still has a home")
}

func TestAnimatorApplyStartsOneAnimationPerEvent(t *testing.T) {
	t.Parallel()
	meta := events.MetaAt("sc-anim-0001", time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	agent := domain.Agent{ID: "eng-1", Role: domain.RoleEngineer, Bead: animTargetBead, Cartridge: "mock", State: domain.AgentStateRunning}
	for _, tc := range []struct {
		name string
		ev   events.Event
		want AnimKind
	}{
		{"spawn", events.AgentSpawned{Meta: meta, Agent: agent, Task: events.TaskImplement}, AnimShot},
		{"approve", events.VerdictReceived{Meta: meta, Bead: animTargetBead, Round: 1, Agent: "qa-1", Verdict: domain.Verdict{Lens: domain.LensQA, Outcome: domain.VerdictApprove}}, AnimBurst},
		{"changes", events.VerdictReceived{Meta: meta, Bead: animTargetBead, Round: 1, Agent: "qa-1", Verdict: domain.Verdict{Lens: domain.LensQA, Outcome: domain.VerdictChanges}}, AnimRicochet},
		{"closed", events.BeadStateChanged{Meta: meta, Bead: animTargetBead, From: domain.BeadStateDelivering, To: domain.BeadStateClosed}, AnimConfetti},
		{"failed", events.AgentFailed{Meta: meta, Agent: "eng-1", Role: domain.RoleEngineer, Bead: animTargetBead, Failure: events.FailureCrash}, AnimSmoke},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := animNewAnimator(AnimOptions{Tab: TabGraph})
			a, cmd := a.Apply(tc.ev)
			require.NotNil(t, cmd)
			require.Equal(t, 1, a.Active())
			playing := a.Playing()
			require.Len(t, playing, 1)
			require.Equal(t, tc.want, playing[0].Kind)
			require.Equal(t, animTargetBead, playing[0].Bead)
			require.Positive(t, playing[0].Frames)
		})
	}
}

func TestAnimatorApplyIgnoresEverythingElse(t *testing.T) {
	t.Parallel()
	meta := events.MetaAt("sc-anim-0001", time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	for _, ev := range []events.Event{
		nil,
		events.AgentOutput{Meta: meta, Agent: "eng-1", Stream: events.StreamStdout, Line: "hello"},
		events.RoundStarted{Meta: meta, Bead: animTargetBead, Round: 1},
		events.BeadStateChanged{Meta: meta, Bead: animTargetBead, From: domain.BeadStateOpen, To: domain.BeadStateInProgress},
		events.BeadStateChanged{Meta: meta, Bead: animTargetBead, From: domain.BeadStateClosed, To: domain.BeadStateClosed},
		events.AgentFailed{Meta: meta, Agent: "eng-1", Bead: animTargetBead, Failure: events.FailureKilled},
		events.VerdictReceived{Meta: meta, Bead: animTargetBead, Round: 1, Verdict: domain.Verdict{}},
		events.Warning{Meta: meta, Source: "beads", Message: "slow"},
	} {
		a := animNewAnimator(AnimOptions{Tab: TabGraph})
		a, cmd := a.Apply(ev)
		require.Nil(t, cmd, "%T started a ticker", ev)
		require.True(t, a.Idle(), "%T started an animation", ev)
	}
}

func TestAnimatorDisabledStaysSilent(t *testing.T) {
	t.Parallel()
	meta := events.MetaAt("sc-anim-0001", time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC))
	a := animNewAnimator(AnimOptions{Disabled: true, Tab: TabGraph})
	require.False(t, a.Enabled())

	a, cmd := a.Apply(events.AgentSpawned{Meta: meta, Agent: domain.Agent{ID: "eng-1", Bead: animTargetBead}})
	require.Nil(t, cmd)
	require.True(t, a.Idle())
	require.Empty(t, a.Sprites())
	require.Equal(t, CannonRest, a.CannonFrame())
	require.Equal(t, RenderCannon(animTheme(), CannonRest), a.Cannon())

	base := animBaseFrame(a, animGoldenWidth, animGoldenHeight)
	require.Equal(t, base, a.Overlay(base))

	a = a.Advance()
	require.Equal(t, uint64(0), a.Frame())

	a, cmd = a.Fire(AnimConfetti, animTargetBead)
	require.Nil(t, cmd)
	require.True(t, a.Idle())
}

func TestAnimatorFireRejectsUnknownKinds(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{})
	a, cmd := a.Fire(AnimKind("fireworks"), animTargetBead)
	require.Nil(t, cmd)
	require.True(t, a.Idle())
}

func TestAnimatorQueuesOverlappingAnimations(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Max: 2})
	for i := 0; i < 5; i++ {
		a, _ = a.Fire(AnimBurst, animTargetBead)
	}
	require.Equal(t, 2, a.Active())
	require.Equal(t, []AnimKind{AnimBurst, AnimBurst, AnimBurst}, a.Pending())

	// Playing the first two out lets the queue in, and every queued
	// animation eventually runs.
	starts, frames := 0, 0
	for !a.Idle() {
		require.LessOrEqual(t, a.Active(), 2, "the cap holds")
		queued := a.Queued()
		a = a.Advance()
		starts += queued - a.Queued()
		frames++
		require.Less(t, frames, 400)
	}
	require.Equal(t, 3, starts, "every queued animation started")
	require.Equal(t, 0, a.Queued())
	require.Equal(t, uint64(len(animBurstFrames(Target{}))*3), a.Frame(), "three batches of bursts")
}

func TestAnimatorTickerRunsOnlyWhileSomethingIsAnimating(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	require.True(t, a.Idle())
	require.False(t, a.Ticking())

	// Nothing is playing, so a stray tick is ignored and schedules nothing.
	a, cmd, handled := a.Update(AnimTickMsg{})
	require.True(t, handled, "the layer owns its tick message")
	require.Nil(t, cmd)

	a, cmd = a.Fire(AnimBurst, animTargetBead)
	require.NotNil(t, cmd, "the first animation starts the ticker")
	require.True(t, a.Ticking())
	gen := a.gen

	// A second animation does not start a second ticker.
	a, cmd = a.Fire(AnimBurst, animTargetBead)
	require.Nil(t, cmd)
	require.Equal(t, gen, a.gen)

	// A tick from an older generation is dropped rather than doubling the
	// frame rate.
	before := a.Frame()
	a, cmd, _ = a.Update(AnimTickMsg{Gen: gen - 1})
	require.Nil(t, cmd)
	require.Equal(t, before, a.Frame())

	ticks := 0
	for {
		var c tea.Cmd
		a, c, _ = a.Update(AnimTickMsg{Gen: a.gen})
		ticks++
		require.Less(t, ticks, 400)
		if c == nil {
			break
		}
	}
	require.True(t, a.Idle())
	require.False(t, a.Ticking(), "the ticker stops when the last frame is drawn")
	require.Equal(t, uint64(ticks), a.Frame())

	// And it starts again for the next animation.
	a, cmd = a.Fire(AnimSmoke, animTargetBead)
	require.NotNil(t, cmd)
	require.True(t, a.Ticking())
}

func TestAnimatorUpdateIgnoresForeignMessages(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{})
	a, cmd, handled := a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	require.False(t, handled)
	require.Nil(t, cmd)
	w, h := a.Size()
	require.Equal(t, animGoldenWidth, w)
	require.Equal(t, animGoldenHeight, h)
}

func TestAnimatorTickCmdCarriesTheGeneration(t *testing.T) {
	a := animNewAnimator(AnimOptions{})
	a, cmd := a.Fire(AnimBurst, animTargetBead)
	require.NotNil(t, cmd)
	msg := cmd()
	require.Equal(t, AnimTickMsg{Gen: a.gen}, msg)
}

func TestAnimatorClearStopsEverything(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Max: 1})
	a, _ = a.Fire(AnimShot, animTargetBead)
	a, _ = a.Fire(AnimShot, animTargetBead)
	require.False(t, a.Idle())

	a = a.Clear()
	require.True(t, a.Idle())
	require.False(t, a.Ticking())
	require.Empty(t, a.Sprites())
	require.Empty(t, a.Playing())
	require.Empty(t, a.Pending())

	a, cmd := a.Fire(AnimShot, animTargetBead)
	require.NotNil(t, cmd, "the ticker can be started again")
	require.Equal(t, AnimTickMsg{Gen: a.gen}, cmd())
}

func TestAnimatorAimsAtTheNodeThenTheTabIndicator(t *testing.T) {
	t.Parallel()
	graph := animNewAnimator(AnimOptions{Tab: TabGraph})
	require.Equal(t, animTargetCell, graph.resolve(AnimShot, animTargetBead))

	// Another tab is up: the shot flies at the Agents indicator instead.
	log := animNewAnimator(AnimOptions{Tab: TabLog})
	agents := log.resolve(AnimShot, animTargetBead)
	require.Equal(t, 1, agents.Row, "tab indicators live on the tab bar")
	require.Equal(t, tabLabelColumn(TabAgents)+tabLabelWidth(TabAgents)/2, agents.Col)

	// A bead the graph does not know about falls back too.
	require.Equal(t, agents, graph.resolve(AnimShot, "sc-nope"))
	require.Equal(t, agents, graph.resolve(AnimShot, ""))

	// Each animation has its own home tab.
	require.Equal(t, graph.tabIndicator(TabFeedback), log.resolve(AnimBurst, animTargetBead))
	require.Equal(t, graph.tabIndicator(TabFeedback), log.resolve(AnimRicochet, animTargetBead))
	require.Equal(t, graph.tabIndicator(TabAgents), log.resolve(AnimSmoke, animTargetBead))
	require.Equal(t, graph.tabIndicator(TabGraph), log.resolve(AnimConfetti, "sc-nope"))

	// A node the graph puts outside the pane is not aimed at.
	off := animNewAnimator(AnimOptions{Tab: TabGraph, Targets: func(domain.BeadID) (Target, bool) {
		return Target{Row: 0, Col: 0}, true
	}})
	require.Equal(t, off.tabIndicator(TabAgents), off.resolve(AnimShot, animTargetBead))
}

func TestAnimTabLabelGeometryMatchesTheTabBar(t *testing.T) {
	t.Parallel()
	bar := animTabBar()
	for i, tab := range Tabs() {
		label := fmt.Sprintf(" %d %s ", i+1, tab)
		require.Equal(t, len([]rune(label)), tabLabelWidth(tab))
		col := tabLabelColumn(tab)
		require.Equal(t, label, string([]rune(bar)[col:col+tabLabelWidth(tab)]))
	}
	require.Equal(t, 0, tabLabelWidth(Tab(99)))
}

func TestAnimatorSetTabAndTargetsAndResize(t *testing.T) {
	t.Parallel()
	a := NewAnimator(AnimOptions{Theme: animTheme()})
	w, h := a.Size()
	require.Equal(t, 80, w)
	require.Equal(t, 24, h)
	require.Equal(t, TabGraph, a.tab, "an invalid tab falls back to the graph")

	a = a.Resize(0, 0)
	w, h = a.Size()
	require.Equal(t, 80, w, "a zero size is ignored")
	require.Equal(t, 24, h)

	a = a.Resize(120, 40).SetTab(TabPRs).SetTargets(animTargets)
	w, h = a.Size()
	require.Equal(t, 120, w)
	require.Equal(t, 40, h)
	require.Equal(t, TabPRs, a.tab)

	a = a.SetTab(Tab(99))
	require.Equal(t, TabPRs, a.tab, "an invalid tab is ignored")
	require.Same(t, animTheme(), a.Theme())
}

func TestAnimatorSpritesStayInsideTheFrame(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Width: 30, Height: 8})
	a, _ = a.Fire(AnimConfetti, animTargetBead)
	for !a.Idle() {
		for _, s := range a.Sprites() {
			require.GreaterOrEqual(t, s.Row, 0)
			require.LessOrEqual(t, s.Row, 8-2, "nothing is drawn over the footer")
			require.GreaterOrEqual(t, s.Col, 0)
			require.Less(t, s.Col, 30)
			require.Equal(t, 1, ansi.StringWidth(s.Glyph), "sprites are one cell")
		}
		a = a.Advance()
	}
}

func TestAnimatorSpritesAreOrderedAndDeduplicated(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	// Two identical bursts land on the same cells; only one sprite per cell
	// reaches the overlay.
	a, _ = a.Fire(AnimBurst, animTargetBead)
	a, _ = a.Fire(AnimBurst, animTargetBead)
	a = a.Advance()

	sprites := a.Sprites()
	require.NotEmpty(t, sprites)
	seen := map[[2]int]bool{}
	prev := [2]int{-1, -1}
	for _, s := range sprites {
		cell := [2]int{s.Row, s.Col}
		require.False(t, seen[cell], "cell %v drawn twice", cell)
		seen[cell] = true
		require.True(t, cell[0] > prev[0] || (cell[0] == prev[0] && cell[1] > prev[1]), "sprites are ordered")
		prev = cell
	}
}

func TestAnimatorOverlayKeepsTheTextAroundASprite(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	a = a.injectSprites([]Sprite{
		{Row: 1, Col: 0, Glyph: "◆", Color: ColorFire},
		{Row: 1, Col: 5, Glyph: "•", Color: ColorFire},
		{Row: 2, Col: 12, Glyph: "·", Color: ColorMuted},
	})
	got := a.Overlay("0123456789\nabcdefghij\nshort")
	require.Equal(t, "0123456789\n◆bcde•ghij\nshort       ·", got)
}

func TestAnimatorOverlayIsANSIAware(t *testing.T) {
	t.Parallel()
	theme := ThemeFor(ProfileTrueColor)
	a := NewAnimator(AnimOptions{Theme: theme, Width: 40, Height: 10})
	a = a.injectSprites([]Sprite{{Row: 0, Col: 4, Glyph: "◆", Color: ColorFire}})

	line := theme.Accent().Render("abcdefghij")
	got := a.Overlay(line)
	require.Equal(t, 10, ansi.StringWidth(got), "the sprite replaced one cell, it did not insert one")
	require.Contains(t, got, "◆")
	require.NotContains(t, ansi.Strip(got), "e", "the sprite covered the fifth column")
	require.Equal(t, "abcd◆fghij", ansi.Strip(got))
}

func TestAnimatorOverlayLeavesUntouchedFramesAlone(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	frame := "one\ntwo\nthree"
	require.Equal(t, frame, a.Overlay(frame), "an idle layer draws nothing")

	// Sprites off the bottom of the frame do not extend it.
	a = a.injectSprites([]Sprite{{Row: 9, Col: 0, Glyph: "◆", Color: ColorFire}})
	require.Equal(t, frame, a.Overlay(frame))
}

func TestAnimatorIsDeterministic(t *testing.T) {
	t.Parallel()
	play := func() string {
		a := animNewAnimator(AnimOptions{Tab: TabGraph, Seed: 7})
		a, _ = a.Fire(AnimConfetti, animTargetBead)
		a, _ = a.Fire(AnimSmoke, animTargetBead)
		var b strings.Builder
		for !a.Idle() {
			fmt.Fprintf(&b, "%v|%+v\n", a.Sprites(), a.CannonFrame())
			a = a.Advance()
		}
		return b.String()
	}
	require.Equal(t, play(), play(), "the same seed draws the same frames")
}

func TestAnimatorSeedChangesTheParticles(t *testing.T) {
	t.Parallel()
	play := func(seed uint64) string {
		a := animNewAnimator(AnimOptions{Tab: TabGraph, Seed: seed})
		a, _ = a.Fire(AnimConfetti, animTargetBead)
		a = a.Advance().Advance().Advance()
		return fmt.Sprint(a.Sprites())
	}
	require.NotEqual(t, play(1), play(2))
}

func TestAnimatorCopiesDoNotShareState(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Max: 1})
	a, _ = a.Fire(AnimShot, animTargetBead)
	a, _ = a.Fire(AnimShot, animTargetBead)

	before := a
	next := before.Advance().Advance()
	require.Equal(t, uint64(0), before.Frame(), "advancing a copy left the original alone")
	require.Equal(t, uint64(2), next.Frame())
	require.Equal(t, 1, before.Queued())

	fired, _ := before.Fire(AnimBurst, animTargetBead)
	require.Equal(t, 2, fired.Queued())
	require.Equal(t, 1, before.Queued())
}

func TestAnimatorCannonRecoilsOnlyForShots(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	a, _ = a.Fire(AnimBurst, animTargetBead)
	require.Equal(t, CannonRest, a.CannonFrame(), "only a shot kicks the cannon")

	a = animNewAnimator(AnimOptions{Tab: TabGraph})
	a, _ = a.Fire(AnimShot, animTargetBead)
	require.Equal(t, CannonMaxRecoil, a.CannonFrame().Recoil)
	require.True(t, a.CannonFrame().Firing())

	// The barrel is back at rest well before the projectile lands.
	settled := false
	for i := 0; !a.Idle(); i++ {
		if a.CannonFrame() == CannonRest {
			settled = true
			require.Less(t, i, len(CannonRecoilFrames())+1)
			break
		}
		a = a.Advance()
	}
	require.True(t, settled)
}

func TestAnimatorShotArcsFromTheMuzzleToTheTarget(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	a, _ = a.Fire(AnimShot, animTargetBead)

	first := a.Sprites()
	require.Len(t, first, 1, "the projectile leaves the muzzle alone")
	require.Equal(t, 0, first[0].Row, "it starts on the header line")
	require.Greater(t, first[0].Col, CannonMuzzleCol, "in front of the muzzle")

	var last Sprite
	sagged := false
	for !a.Idle() {
		for _, s := range a.Sprites() {
			last = s
			if s.Row > animTargetCell.Row {
				sagged = true
			}
		}
		a = a.Advance()
	}
	require.True(t, sagged, "the shot lobs below the target on its way")
	require.Equal(t, animTargetCell.Row, last.Row, "it lands on the node")
	require.Equal(t, animTargetCell.Col, last.Col)
}

func TestAnimatorRicochetComesBackAtTheCannon(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph})
	a, _ = a.Fire(AnimRicochet, animTargetBead)

	first := a.Sprites()
	require.Len(t, first, 1)
	require.Equal(t, animTargetCell, Target{Row: first[0].Row, Col: first[0].Col})

	var last Sprite
	for !a.Idle() {
		if s := a.Sprites(); len(s) > 0 {
			last = s[len(s)-1]
		}
		a = a.Advance()
	}
	require.Less(t, last.Row, animTargetCell.Row, "the spark ends up back at the cannon")
	require.Less(t, last.Col, animTargetCell.Col)
}

func TestAnimatorConfettiFallsBackDown(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Width: 200, Height: 40, Targets: func(domain.BeadID) (Target, bool) {
		return Target{Row: 20, Col: 100}, true
	}})
	a, _ = a.Fire(AnimConfetti, animTargetBead)

	highest, lowest := 100, -1
	for !a.Idle() {
		for _, s := range a.Sprites() {
			highest = min(highest, s.Row)
			lowest = max(lowest, s.Row)
		}
		a = a.Advance()
	}
	require.Less(t, highest, 20, "the pieces are thrown up")
	require.Greater(t, lowest, 20, "and fall back past where they started")
}

func TestAnimatorSmokeRises(t *testing.T) {
	t.Parallel()
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Width: 200, Height: 40, Targets: func(domain.BeadID) (Target, bool) {
		return Target{Row: 20, Col: 100}, true
	}})
	a, _ = a.Fire(AnimSmoke, animTargetBead)

	lowest := -1
	highest := 100
	for !a.Idle() {
		for _, s := range a.Sprites() {
			lowest = max(lowest, s.Row)
			highest = min(highest, s.Row)
		}
		a = a.Advance()
	}
	require.Equal(t, 20, lowest, "the puff never sinks below the misfire")
	require.Less(t, highest, 20, "it drifts up")
}

func TestAnimatorNoColorDrawsBareGlyphs(t *testing.T) {
	t.Parallel()
	color := NewAnimator(AnimOptions{Theme: ThemeFor(ProfileTrueColor), Width: 40, Height: 12, Targets: animTargets, Tab: TabGraph})
	plain := NewAnimator(AnimOptions{NoColor: true, Width: 40, Height: 12, Targets: animTargets, Tab: TabGraph})
	require.Equal(t, ProfileNoColor, plain.Theme().Profile())

	color, _ = color.Fire(AnimConfetti, animTargetBead)
	plain, _ = plain.Fire(AnimConfetti, animTargetBead)
	color, plain = color.Advance(), plain.Advance()

	base := strings.Repeat("x", 40) + "\n" + strings.Repeat("y", 40)
	require.Contains(t, color.Overlay(base), "\x1b[", "a colour profile styles the sprites")
	got := plain.Overlay(base)
	require.NotContains(t, got, "\x1b", "NO_COLOR draws the glyphs bare")
	require.Equal(t, ansi.Strip(color.Overlay(base)), got, "the pictures are the same either way")
}

// TestAnimatorIdleCostsAlmostNothing is the P7-06 idle-CPU criterion: with an
// empty queue the layer schedules no timer at all, and the work a frame would
// still do costs a small fraction of the 30fps frame budget.
func TestAnimatorIdleCostsAlmostNothing(t *testing.T) {
	a := animNewAnimator(AnimOptions{Tab: TabGraph, Width: 200, Height: 50})
	require.True(t, a.Idle())
	require.False(t, a.Ticking())

	a, cmd, _ := a.Update(AnimTickMsg{Gen: a.gen})
	require.Nil(t, cmd, "an idle layer never reschedules the ticker")
	require.Equal(t, uint64(0), a.Frame())

	frame := animBaseFrame(a, 200, 50)
	res := testing.Benchmark(func(b *testing.B) {
		idle := a
		for i := 0; i < b.N; i++ {
			idle, _, _ = idle.Update(AnimTickMsg{Gen: idle.gen})
			idle = idle.Advance()
			_ = idle.Cannon()
			_ = idle.Overlay(frame)
		}
	})
	require.Positive(t, res.N)
	budget := int64(AnimFrame) / 20 // 5% of one frame
	require.Less(t, res.NsPerOp(), budget,
		"idle frame costs %d ns, more than 5%% of the %s frame budget", res.NsPerOp(), AnimFrame)
}

// BenchmarkAnimatorIdle measures the animation layer with an empty queue: the
// state a watching TUI spends nearly all its time in.
func BenchmarkAnimatorIdle(b *testing.B) {
	a := NewAnimator(AnimOptions{Theme: animTheme(), Width: 200, Height: 50, Targets: animTargets})
	frame := animBaseFrame(a, 200, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, _, _ = a.Update(AnimTickMsg{Gen: a.gen})
		a = a.Advance()
		_ = a.Cannon()
		_ = a.Overlay(frame)
	}
}

// BenchmarkAnimatorBusy measures a full queue of animations, for comparison
// with the idle case.
func BenchmarkAnimatorBusy(b *testing.B) {
	base := NewAnimator(AnimOptions{Theme: animTheme(), Width: 200, Height: 50, Targets: animTargets, Tab: TabGraph})
	frame := animBaseFrame(base, 200, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a := base
		for _, k := range AnimKinds() {
			a, _ = a.Fire(k, animTargetBead)
		}
		b.StartTimer()
		for !a.Idle() {
			a, _, _ = a.Update(AnimTickMsg{Gen: a.gen})
			_ = a.Cannon()
			_ = a.Overlay(frame)
		}
	}
}
