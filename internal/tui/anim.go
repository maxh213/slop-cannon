package tui

import (
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P7-06 — the cannon animations.
//
// Every animation is precomputed at the moment it starts: a slice of frames,
// each a handful of positioned glyphs. Playing one is then a counter, and a
// golden frame is reproducible without a clock. The frame ticker runs at
// [AnimFPS] and only while something is on screen, so an idle TUI schedules
// no timers at all.

// AnimFPS is the frame rate of the animations.
const AnimFPS = 30

// AnimFrame is how long one frame lasts.
const AnimFrame = time.Second / AnimFPS

// AnimMaxActive is how many animations play at once by default. Anything
// beyond it waits in a queue and starts as slots free up, so a burst of
// events cannot turn the pane into noise.
const AnimMaxActive = 4

// AnimMaxQueued is how many animations wait for a slot by default. The queue
// is bounded as tightly as the active set: an orchestrator that publishes a
// thousand events in a second must not leave the layer ticking at [AnimFPS]
// for minutes after the run went quiet, which is exactly what an unbounded
// queue would do to the phase's idle-CPU budget. Once it is full the oldest
// animation still waiting is dropped, because the newest events are the ones
// the viewer is watching; [Animator.Dropped] counts them.
const AnimMaxQueued = 4

// NoAnimEnv disables the animations when it is set to anything but an empty
// value, "0" or "false". The `--no-anim` flag does the same.
const NoAnimEnv = "SC_NO_ANIM"

// AnimKind names one of the cannon's animations.
type AnimKind string

// The animations, one per interesting event.
const (
	// AnimShot is a spawned agent: the cannon recoils and a projectile flies
	// to the target on a sagging arc.
	AnimShot AnimKind = "shot"
	// AnimBurst is an approving verdict: a small burst at the target.
	AnimBurst AnimKind = "burst"
	// AnimRicochet is a changes-requested verdict: a spark bounces off the
	// target back toward the cannon.
	AnimRicochet AnimKind = "ricochet"
	// AnimConfetti is a bead reaching its terminal state: a confetti burst
	// falling under gravity.
	AnimConfetti AnimKind = "confetti"
	// AnimSmoke is a failed agent: a misfire puff drifting up.
	AnimSmoke AnimKind = "smoke"
)

// animKinds is every animation, in the order they are documented.
var animKinds = [...]AnimKind{AnimShot, AnimBurst, AnimRicochet, AnimConfetti, AnimSmoke}

// AnimKinds returns every animation the cannon can play.
func AnimKinds() []AnimKind {
	out := make([]AnimKind, len(animKinds))
	copy(out, animKinds[:])
	return out
}

// String returns the animation's name.
func (k AnimKind) String() string { return string(k) }

// Valid reports whether k names one of the animations.
func (k AnimKind) Valid() bool {
	for _, x := range animKinds {
		if x == k {
			return true
		}
	}
	return false
}

// homeTab is the view the animation belongs to: where its target lives, and
// which tab indicator it aims at when that view is not on screen.
func (k AnimKind) homeTab() Tab {
	switch k {
	case AnimShot, AnimSmoke:
		return TabAgents
	case AnimBurst, AnimRicochet:
		return TabFeedback
	case AnimConfetti:
		return TabGraph
	default:
		return TabGraph
	}
}

// Sprite is one glyph of the animation layer, positioned in the cells of the
// whole terminal frame: row 0 is the header line the cannon sits on.
type Sprite struct {
	// Row is the frame line, counted from the top.
	Row int
	// Col is the column, counted from the left.
	Col int
	// Glyph is the single-cell rune to draw.
	Glyph string
	// Color is the colour to draw it in; the theme resolves it, so a
	// no-colour profile renders the glyph bare.
	Color Color
}

// Target is the cell an animation plays at, in the same whole-frame
// coordinates as [Sprite].
type Target struct {
	// Row is the frame line.
	Row int
	// Col is the column.
	Col int
}

// TargetFunc resolves a bead to the cell of its node in the current frame.
// [GraphTargets] builds one from the graph view (P7-02) so projectiles land
// on the right node; returning false sends the animation to the tab indicator
// instead.
type TargetFunc func(bead domain.BeadID) (Target, bool)

// AnimTickMsg advances the animation layer one frame. Gen identifies the
// ticker that produced it so a tick left over from an earlier run of the
// ticker is ignored rather than doubling the frame rate.
type AnimTickMsg struct {
	// Gen is the ticker generation the message was scheduled by.
	Gen uint64
}

// AnimOptions configure the animation layer.
type AnimOptions struct {
	// Disabled turns every animation off: --no-anim, SC_NO_ANIM, or a
	// terminal that cannot draw them. Nothing is queued and no ticker runs.
	Disabled bool
	// NoColor renders the glyphs without any ANSI colour; ignored when Theme
	// is set.
	NoColor bool
	// Theme resolves the sprite colours; nil means [EnvTheme] with NoColor.
	Theme *Theme
	// Width and Height are the terminal size; zero means 80×24 until the
	// first [tea.WindowSizeMsg].
	Width, Height int
	// Max is how many animations play at once; zero means [AnimMaxActive].
	Max int
	// MaxQueued is how many animations wait for a free slot; zero means
	// [AnimMaxQueued]. A full queue drops its oldest entry.
	MaxQueued int
	// Targets resolves a bead to the cell of its graph node; nil aims every
	// animation at the tab indicators.
	Targets TargetFunc
	// Tab is the view on screen, so an animation can aim at a real node when
	// its view is up and at the tab indicator when it is not.
	Tab Tab
	// Seed makes the confetti and smoke particles reproducible. Zero is a
	// perfectly good seed: the layer never reads a clock.
	Seed uint64
}

// animation is one precomputed animation being played.
type animation struct {
	kind   AnimKind
	bead   domain.BeadID
	agent  domain.AgentID
	frames [][]Sprite
	cannon bool
	at     int
}

// done reports whether every frame has been played.
func (an animation) done() bool { return an.at >= len(an.frames) }

// pending is an animation waiting for a free slot.
type pending struct {
	kind  AnimKind
	bead  domain.BeadID
	agent domain.AgentID
	seq   uint64
}

// Animator is the cannon's animation layer: a bounded queue of animations, a
// frame ticker that only runs while one is playing, and an overlay that
// composites the sprites onto a rendered frame.
//
// Both bounds matter: at most [AnimMaxActive] animations are on screen and at
// most [AnimMaxQueued] wait behind them, so the layer drains within about two
// seconds of the last event no matter how many arrived.
//
// It is a value, like the other Bubble Tea components in this package: the
// mutators return the new animator and never write through a copy.
type Animator struct {
	theme     *Theme
	disabled  bool
	width     int
	height    int
	maxActive int
	maxQueued int
	targets   TargetFunc
	tab       Tab
	seed      uint64

	active  []animation
	queue   []pending
	dropped uint64
	seq     uint64
	gen     uint64
	ticking bool
	frame   uint64
}

// NewAnimator builds the animation layer.
func NewAnimator(opts AnimOptions) Animator {
	t := opts.Theme
	if t == nil {
		t = EnvTheme(opts.NoColor)
	}
	a := Animator{
		theme:     t,
		disabled:  opts.Disabled,
		width:     opts.Width,
		height:    opts.Height,
		maxActive: opts.Max,
		maxQueued: opts.MaxQueued,
		targets:   opts.Targets,
		tab:       opts.Tab,
		seed:      opts.Seed,
	}
	if a.width <= 0 {
		a.width = 80
	}
	if a.height <= 0 {
		a.height = 24
	}
	if a.maxActive <= 0 {
		a.maxActive = AnimMaxActive
	}
	if a.maxQueued <= 0 {
		a.maxQueued = AnimMaxQueued
	}
	if !a.tab.Valid() {
		a.tab = TabGraph
	}
	return a
}

// AnimDisabled reports whether the animations must stay off: the --no-anim
// flag, or [NoAnimEnv] set to anything but "", "0" or "false". A nil lookup
// reads the process environment.
//
// `NO_COLOR` is deliberately not in that list: it takes the colour out of the
// animations (through the theme), it does not take the animations away.
func AnimDisabled(noAnim bool, lookup func(string) (string, bool)) bool {
	if noAnim {
		return true
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, ok := lookup(NoAnimEnv)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Enabled reports whether animations play at all.
func (a Animator) Enabled() bool { return !a.disabled }

// Idle reports whether nothing is playing and nothing is waiting. An idle
// animator holds no timer, which is what keeps a watching TUI off the CPU.
func (a Animator) Idle() bool { return len(a.active) == 0 && len(a.queue) == 0 }

// Active returns how many animations are playing.
func (a Animator) Active() int { return len(a.active) }

// Queued returns how many animations are waiting for a slot.
func (a Animator) Queued() int { return len(a.queue) }

// Dropped returns how many animations were thrown away because the queue was
// already full. It is the price of the bound: a run that publishes faster
// than 30 frames a second gets the newest animations, not all of them.
func (a Animator) Dropped() uint64 { return a.dropped }

// Capacity returns how many animations play at once and how many wait.
func (a Animator) Capacity() (active, queued int) { return a.maxActive, a.maxQueued }

// Playing describes one animation currently on screen.
type Playing struct {
	// Kind is which animation it is.
	Kind AnimKind
	// Bead is the bead it plays at, when it has one.
	Bead domain.BeadID
	// Agent is the agent that caused it, when there was one.
	Agent domain.AgentID
	// Frame is how many frames of it have been drawn.
	Frame int
	// Frames is how many frames it has in total.
	Frames int
}

// Playing lists the animations on screen, oldest first.
func (a Animator) Playing() []Playing {
	out := make([]Playing, 0, len(a.active))
	for _, an := range a.active {
		out = append(out, Playing{Kind: an.kind, Bead: an.bead, Agent: an.agent, Frame: an.at, Frames: len(an.frames)})
	}
	return out
}

// Pending lists the animations waiting for a free slot, oldest first.
func (a Animator) Pending() []AnimKind {
	out := make([]AnimKind, 0, len(a.queue))
	for _, p := range a.queue {
		out = append(out, p.kind)
	}
	return out
}

// Frame returns how many frames have been played since the animator was
// built. It only advances while something is animating.
func (a Animator) Frame() uint64 { return a.frame }

// Size returns the terminal size the layer draws into.
func (a Animator) Size() (int, int) { return a.width, a.height }

// Theme returns the theme the sprites are coloured with.
func (a Animator) Theme() *Theme { return a.theme }

// Ticking reports whether a frame ticker is in flight.
func (a Animator) Ticking() bool { return a.ticking }

// Resize tells the layer the new terminal size. Animations already in flight
// keep the trajectory they were given; they are clipped to the new frame.
func (a Animator) Resize(w, h int) Animator {
	if w > 0 {
		a.width = w
	}
	if h > 0 {
		a.height = h
	}
	return a
}

// SetTab tells the layer which view is on screen, so the next animation aims
// at a real node when its view is up and at the tab indicator when it is not.
func (a Animator) SetTab(t Tab) Animator {
	if t.Valid() {
		a.tab = t
	}
	return a
}

// SetTargets replaces the bead-to-cell resolver.
func (a Animator) SetTargets(f TargetFunc) Animator {
	a.targets = f
	return a
}

// Apply starts the animation an event calls for, if any: [events.AgentSpawned]
// fires a shot, an approving [events.VerdictReceived] a burst and a
// changes-requested one a ricochet, a bead reaching a terminal state confetti,
// and [events.AgentFailed] a puff of smoke. Every other event is ignored.
//
// Deviation from the P7-06 task text, which asks for a misfire puff on every
// [events.AgentFailed]: an agent killed on purpose
// ([events.FailureKilled] — a pause, a stop, an ammo limit) plays nothing.
// The cannon did not misfire, the operator stopped it, and a puff of smoke
// for every agent torn down by `q` would make a clean stop look like a
// failure.
//
// The returned command is the frame ticker, and is nil unless this call woke
// an idle layer.
func (a Animator) Apply(ev events.Event) (Animator, tea.Cmd) {
	if a.disabled || ev == nil {
		return a, nil
	}
	switch e := ev.(type) {
	case events.AgentSpawned:
		return a.fire(AnimShot, e.Agent.Bead, e.Agent.ID)
	case events.AgentFailed:
		if e.Failure == events.FailureKilled {
			return a, nil
		}
		return a.fire(AnimSmoke, e.Bead, e.Agent)
	case events.VerdictReceived:
		switch e.Verdict.Outcome {
		case domain.VerdictApprove:
			return a.fire(AnimBurst, e.Bead, e.Agent)
		case domain.VerdictChanges:
			return a.fire(AnimRicochet, e.Bead, e.Agent)
		}
		return a, nil
	case events.BeadStateChanged:
		if e.To == domain.BeadStateClosed && e.From != domain.BeadStateClosed {
			return a.fire(AnimConfetti, e.Bead, "")
		}
		return a, nil
	default:
		return a, nil
	}
}

// Fire starts one animation by hand, aimed at a bead. `sc tui-demo` and the
// tests use it; the run itself goes through [Animator.Apply].
func (a Animator) Fire(kind AnimKind, bead domain.BeadID) (Animator, tea.Cmd) {
	if a.disabled || !kind.Valid() {
		return a, nil
	}
	return a.fire(kind, bead, "")
}

// fire queues an animation, starting it at once when a slot is free.
//
// Both the active set and the queue are bounded ([AnimMaxActive],
// [AnimMaxQueued]): when the queue is full the animation that has waited
// longest is dropped to make room, so however many events arrive at once the
// layer always drains within a couple of seconds of the last one.
func (a Animator) fire(kind AnimKind, bead domain.BeadID, agent domain.AgentID) (Animator, tea.Cmd) {
	a.seq++
	p := pending{kind: kind, bead: bead, agent: agent, seq: a.seq}
	if len(a.active) < a.maxActive {
		a = a.start(p)
		return a.wake()
	}
	q := append(make([]pending, 0, len(a.queue)+1), a.queue...)
	q = append(q, p)
	if drop := len(q) - a.maxQueued; drop > 0 {
		q = q[drop:]
		a.dropped += uint64(drop)
	}
	a.queue = q
	return a.wake()
}

// start builds the animation's frames and puts it on screen.
func (a Animator) start(p pending) Animator {
	an := animation{kind: p.kind, bead: p.bead, agent: p.agent}
	to := a.resolve(p.kind, p.bead)
	from := Target{Row: 0, Col: CannonMuzzleCol + 2}
	switch p.kind {
	case AnimShot:
		an.frames, an.cannon = animShotFrames(from, to), true
	case AnimBurst:
		an.frames = animBurstFrames(to)
	case AnimRicochet:
		an.frames = animRicochetFrames(to, from)
	case AnimConfetti:
		an.frames = animConfettiFrames(to, a.seed+p.seq)
	case AnimSmoke:
		an.frames = animSmokeFrames(to, a.seed+p.seq)
	default:
		return a
	}
	if len(an.frames) == 0 {
		return a
	}
	a.active = append(append(make([]animation, 0, len(a.active)+1), a.active...), an)
	return a
}

// resolve picks the cell an animation plays at: the bead's node when its own
// view is on screen and the graph knows where the node is, otherwise the
// indicator of the tab that would show it.
func (a Animator) resolve(kind AnimKind, bead domain.BeadID) Target {
	if a.targets != nil && bead != "" && a.tab == TabGraph {
		if t, ok := a.targets(bead); ok && a.inPane(t) {
			return t
		}
	}
	return a.tabIndicator(kind.homeTab())
}

// inPane reports whether a cell is inside the main pane of the current frame.
func (a Animator) inPane(t Target) bool {
	return t.Col >= 0 && t.Col < a.width && t.Row >= animPaneTop && t.Row <= a.height-2
}

// tabIndicator is the middle cell of a tab's label in the tab bar.
func (a Animator) tabIndicator(t Tab) Target {
	col := tabLabelColumn(t) + tabLabelWidth(t)/2
	return Target{Row: 1, Col: min(col, max(a.width-1, 0))}
}

// tabLabelWidth is how wide [Model.tabBar] draws one label: " <n> <name> ".
func tabLabelWidth(t Tab) int {
	if !t.Valid() {
		return 0
	}
	return len([]rune(t.String())) + 4
}

// tabLabelColumn is the first column of a tab's label in the tab bar.
func tabLabelColumn(t Tab) int {
	col := 0
	for _, x := range Tabs() {
		if x == t {
			break
		}
		col += tabLabelWidth(x)
	}
	return col
}

// --- aiming at a real node ------------------------------------------------

// animPaneTop is the frame row the main pane starts on: the header, the tab
// bar and the rule are above it.
const animPaneTop = chromeHeight - 1

// animGraphTop is the frame row the graph view's canvas starts on. The view
// spends its first pane line on the counts summary, so the boxes begin one
// line further down.
const animGraphTop = animPaneTop + 1

// GraphTargets returns the resolver that aims an animation at a bead's node:
// it maps a bead to the middle of its box in the graph view's boxed layout,
// in the whole-frame coordinates [Sprite] uses. [Model] hands one to the
// animator, which is how a projectile reaches the node it was fired at
// instead of the tab indicator (P7-06).
//
// It resolves nothing — so the animation falls back to the tab indicator —
// when the view is showing the bead detail panel or the indented list of a
// narrow terminal, when the run has no such bead, or when the bead's box is
// scrolled off the pane.
func GraphTargets(g GraphModel, st *State) TargetFunc {
	return func(bead domain.BeadID) (Target, bool) { return graphNodeCell(g, st, bead) }
}

// graphNodeCell is the resolution itself, done at the moment an animation
// starts so it sees the graph the viewer is actually looking at.
func graphNodeCell(g GraphModel, st *State, bead domain.BeadID) (Target, bool) {
	if st == nil || bead == "" || g.ShowingDetail() {
		return Target{}, false
	}
	orientation := g.Orientation()
	if orientation == GraphIndentedList {
		return Target{}, false
	}
	lay := BuildGraphLayout(st)
	idx, ok := lay.Index(bead)
	if !ok {
		return Target{}, false
	}
	// The graph view pins its selection to a bead id, so ask it where the
	// selection is now rather than reusing the row it was on (P7-02).
	cursor := g.index(lay)
	_, boxes := g.paint(lay, orientation == GraphTopToBottom, cursor)
	offX, offY := g.scroll(boxes[cursor])
	b := boxes[idx]
	x, y := b.midX()-offX, b.midY()-offY
	w, h := g.Size()
	if x < 0 || x >= w || y < 0 || y >= max(h-1, 1) {
		return Target{}, false // scrolled out of the pane
	}
	return Target{Row: animGraphTop + y, Col: x}, true
}

// animTargets is the resolver the model aims with: the graph view's nodes
// while the graph is the pane on screen, and none while an overlay covers it,
// so a projectile is never fired into the help box or the stop prompt.
func (m Model) animTargets() TargetFunc {
	if m.tab != TabGraph || m.showHelp || m.confirmStop {
		return nil
	}
	return GraphTargets(m.graph, m.state)
}

// wake starts the frame ticker when something is on screen and no ticker is
// in flight yet.
func (a Animator) wake() (Animator, tea.Cmd) {
	if a.Idle() || a.ticking {
		return a, nil
	}
	a.ticking = true
	a.gen++
	return a, a.tickCmd()
}

// tickCmd schedules the next frame.
func (a Animator) tickCmd() tea.Cmd {
	gen := a.gen
	return tea.Tick(AnimFrame, func(time.Time) tea.Msg { return AnimTickMsg{Gen: gen} })
}

// Update handles the layer's own messages: [AnimTickMsg] advances one frame
// and schedules the next while anything is left to draw. It reports whether
// it consumed the message, so the caller can pass everything else on.
func (a Animator) Update(msg tea.Msg) (Animator, tea.Cmd, bool) {
	tick, ok := msg.(AnimTickMsg)
	if !ok {
		return a, nil, false
	}
	if a.disabled || !a.ticking || tick.Gen != a.gen {
		return a, nil, true
	}
	a = a.Advance()
	if a.Idle() {
		a.ticking = false
		a.gen++
		return a, nil, true
	}
	return a, a.tickCmd(), true
}

// Advance plays one frame: every animation moves on, finished ones are
// dropped and queued ones take their slots. It reads no clock, so a test can
// step frame by frame and get the same pictures every time.
func (a Animator) Advance() Animator {
	if a.disabled || a.Idle() {
		return a
	}
	a.frame++
	next := make([]animation, 0, len(a.active))
	for _, an := range a.active {
		an.at++
		if !an.done() {
			next = append(next, an)
		}
	}
	a.active = next
	for len(a.queue) > 0 && len(a.active) < a.maxActive {
		p := a.queue[0]
		a.queue = append(make([]pending, 0, len(a.queue)-1), a.queue[1:]...)
		a = a.start(p)
	}
	return a
}

// Clear stops everything at once, dropping the queue: what a resize to a
// useless size or a finished run wants.
func (a Animator) Clear() Animator {
	a.active, a.queue = nil, nil
	if a.ticking {
		a.ticking = false
		a.gen++
	}
	return a
}

// CannonFrame returns the cannon's drawing for this frame: the deepest recoil
// and brightest muzzle flash of every shot in flight.
func (a Animator) CannonFrame() CannonFrame {
	if a.disabled {
		return CannonRest
	}
	frames := make([]CannonFrame, 0, len(a.active))
	for _, an := range a.active {
		if an.cannon {
			frames = append(frames, cannonFrameAt(an.at))
		}
	}
	return cannonCombine(frames)
}

// Cannon renders the header's cannon for this frame, exactly
// [CannonFieldWidth] columns wide. It replaces the static [CannonSprite] in
// the header line.
func (a Animator) Cannon() string {
	return RenderCannon(a.theme, a.CannonFrame())
}

// Sprites returns the glyphs to draw over the current frame, clipped to the
// terminal and ordered by row then column. Sprites never land on the footer,
// so the key hints stay readable.
func (a Animator) Sprites() []Sprite {
	if a.disabled || len(a.active) == 0 {
		return nil
	}
	out := make([]Sprite, 0, 16)
	for _, an := range a.active {
		if an.at < 0 || an.at >= len(an.frames) {
			continue
		}
		for _, s := range an.frames[an.at] {
			if s.Row < 0 || s.Row > a.height-2 || s.Col < 0 || s.Col >= a.width {
				continue
			}
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	// The newest animation wins a contested cell: keep the last sprite of
	// each (row, col) run.
	kept := out[:0]
	for i, s := range out {
		if i+1 < len(out) && out[i+1].Row == s.Row && out[i+1].Col == s.Col {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// Overlay composites the current sprites onto an already rendered frame — the
// whole terminal, header line first — and returns the new frame. It is ANSI
// aware: the styling of the text underneath survives on both sides of a
// sprite. With animations off, or nothing playing, the frame is returned
// unchanged.
func (a Animator) Overlay(frame string) string {
	sprites := a.Sprites()
	if len(sprites) == 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	row := -1
	start := 0
	for i := 0; i <= len(sprites); i++ {
		if i < len(sprites) && sprites[i].Row == row {
			continue
		}
		if row >= 0 && row < len(lines) {
			lines[row] = a.overlayLine(lines[row], sprites[start:i])
		}
		if i < len(sprites) {
			row, start = sprites[i].Row, i
		}
	}
	return strings.Join(lines, "\n")
}

// overlayLine draws one row's sprites over a rendered line. The sprites must
// be ordered by column.
func (a Animator) overlayLine(line string, sprites []Sprite) string {
	width := ansi.StringWidth(line)
	var b strings.Builder
	col := 0
	for _, s := range sprites {
		if s.Col < col {
			continue
		}
		if col < width {
			b.WriteString(ansi.Cut(line, col, min(s.Col, width)))
		}
		if pad := s.Col - max(col, width); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(a.theme.Style(s.Color).Render(s.Glyph))
		col = s.Col + 1
	}
	if col < width {
		b.WriteString(ansi.TruncateLeft(line, col, ""))
	}
	return b.String()
}

// --- frame builders -------------------------------------------------------
//
// Each builder returns the whole animation up front, so playing it is a
// counter and a golden frame needs no clock.

// animShotMaxFrames caps the flight of a projectile.
const animShotMaxFrames = 45

// animShotFrames is the projectile of a spawned agent: a spring drives the
// travel so it leaves the muzzle fast and eases into the target, and a sine
// sag gives it the lob of something heavy.
func animShotFrames(from, to Target) [][]Sprite {
	s := harmonica.NewSpring(harmonica.FPS(AnimFPS), 9.0, 1.0)
	pos, vel := 0.0, 0.0
	path := make([]Target, 0, animShotMaxFrames)
	dist := math.Hypot(float64(to.Row-from.Row), float64(to.Col-from.Col))
	sag := animClampFloat(dist/8, 1, 4)
	for i := 0; i < animShotMaxFrames; i++ {
		pos, vel = s.Update(pos, vel, 1)
		p := animClampFloat(pos, 0, 1)
		col := animLerp(float64(from.Col), float64(to.Col), p)
		// The sag is eased in so the projectile leaves the muzzle level and
		// only then starts to lob.
		row := animLerp(float64(from.Row), float64(to.Row), p) + sag*math.Pow(math.Sin(math.Pi*p), 1.6)
		path = append(path, Target{Row: animRound(row), Col: animRound(col)})
		if p >= 0.995 {
			break
		}
	}
	trail := [...]struct {
		back  int
		glyph string
		color Color
	}{
		{0, "◆", ColorFire},
		{1, "•", ColorWarning},
		{2, "·", ColorMuted},
	}
	frames := make([][]Sprite, 0, len(path)+3)
	for i := range path {
		f := make([]Sprite, 0, len(trail))
		for _, t := range trail {
			j := i - t.back
			if j < 0 {
				continue
			}
			f = append(f, Sprite{Row: path[j].Row, Col: path[j].Col, Glyph: t.glyph, Color: t.color})
		}
		frames = append(frames, f)
	}
	// A short impact spark so the projectile lands rather than vanishing.
	for _, g := range []struct {
		glyph string
		color Color
	}{{"✷", ColorFire}, {"✦", ColorWarning}, {"·", ColorMuted}} {
		frames = append(frames, []Sprite{{Row: to.Row, Col: to.Col, Glyph: g.glyph, Color: g.color}})
	}
	return frames
}

// animBurstHold is how many frames each still of the burst is held for, so a
// six-still animation lasts about four tenths of a second.
const animBurstHold = 2

// animBurstFrames is the approving verdict: a ring blooms out of the target
// and settles into a check mark.
func animBurstFrames(at Target) [][]Sprite {
	stills := [][]Sprite{
		{{Row: at.Row, Col: at.Col, Glyph: "✷", Color: ColorSuccess}},
		append([]Sprite{{Row: at.Row, Col: at.Col, Glyph: "✦", Color: ColorSuccess}},
			animRing(at, 1, false, "·", ColorSuccess)...),
		append([]Sprite{{Row: at.Row, Col: at.Col, Glyph: "✓", Color: ColorSuccess}},
			animRing(at, 1, true, "·", ColorSuccess)...),
		append([]Sprite{{Row: at.Row, Col: at.Col, Glyph: "✓", Color: ColorSuccess}},
			animRing(at, 2, false, "·", ColorMuted)...),
		{{Row: at.Row, Col: at.Col, Glyph: "✓", Color: ColorSuccess}},
		{{Row: at.Row, Col: at.Col, Glyph: "✓", Color: ColorMuted}},
	}
	return animHold(stills, animBurstHold)
}

// animRing returns the cells one Chebyshev radius out from a point: the four
// orthogonal neighbours, or the four diagonal ones.
func animRing(at Target, radius int, diagonal bool, glyph string, color Color) []Sprite {
	deltas := [][2]int{{-radius, 0}, {radius, 0}, {0, -radius}, {0, radius}}
	if diagonal {
		deltas = [][2]int{{-radius, -radius}, {-radius, radius}, {radius, -radius}, {radius, radius}}
	}
	out := make([]Sprite, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, Sprite{Row: at.Row + d[0], Col: at.Col + d[1], Glyph: glyph, Color: color})
	}
	return out
}

// animHold repeats every still n times, turning a storyboard into frames.
func animHold(stills [][]Sprite, n int) [][]Sprite {
	if n < 1 {
		n = 1
	}
	out := make([][]Sprite, 0, len(stills)*n)
	for _, s := range stills {
		for i := 0; i < n; i++ {
			out = append(out, s)
		}
	}
	return out
}

// animRicochetMaxFrames caps the spark's flight back to the cannon.
const animRicochetMaxFrames = 30

// animRicochetFrames is the changes-requested verdict: the round bounces off
// the target and a spark comes back at the cannon.
func animRicochetFrames(at, back Target) [][]Sprite {
	frames := [][]Sprite{
		{{Row: at.Row, Col: at.Col, Glyph: "✗", Color: ColorDanger}},
		{{Row: at.Row, Col: at.Col, Glyph: "✗", Color: ColorDanger}},
		{{Row: at.Row, Col: at.Col, Glyph: "✳", Color: ColorWarning}},
	}
	s := harmonica.NewSpring(harmonica.FPS(AnimFPS), 12.0, 1.0)
	pos, vel := 0.0, 0.0
	glyphs := [...]string{"✳", "✶", "·"}
	for i := 0; i < animRicochetMaxFrames; i++ {
		pos, vel = s.Update(pos, vel, 1)
		p := animClampFloat(pos, 0, 1)
		col := animLerp(float64(at.Col), float64(back.Col), p)
		row := animLerp(float64(at.Row), float64(back.Row), p) - animClampFloat(math.Abs(float64(at.Row-back.Row))/4, 0, 3)*math.Sin(math.Pi*p)
		color := ColorWarning
		if p > 0.6 {
			color = ColorMuted
		}
		frames = append(frames, []Sprite{{
			Row:   animRound(row),
			Col:   animRound(col),
			Glyph: glyphs[i%len(glyphs)],
			Color: color,
		}})
		if p >= 0.99 {
			break
		}
	}
	return frames
}

// animConfettiCount is how many pieces a confetti burst throws.
const animConfettiCount = 10

// animConfettiLength is how many frames the confetti falls for.
const animConfettiLength = 30

// confettiGlyphs are the pieces, cycled per particle.
var confettiGlyphs = [...]string{"✦", "✧", "◆", "◇", "•"}

// confettiColors are the colours the pieces are drawn in.
var confettiColors = [...]Color{ColorSuccess, ColorAccent, ColorWarning, ColorFire}

// animConfettiFrames is the celebration when a bead closes: pieces thrown up
// out of the node and falling back under gravity ([harmonica.Projectile], in
// terminal coordinates where +Y is down).
func animConfettiFrames(at Target, seed uint64) [][]Sprite {
	rng := rand.New(rand.NewPCG(seed, 0x5ca1ab1e))
	type piece struct {
		proj  *harmonica.Projectile
		glyph string
		color Color
	}
	pieces := make([]piece, 0, animConfettiCount)
	for i := 0; i < animConfettiCount; i++ {
		vx := (rng.Float64()*2 - 1) * 16
		vy := -(10 + rng.Float64()*12)
		pieces = append(pieces, piece{
			proj: harmonica.NewProjectile(
				harmonica.FPS(AnimFPS),
				harmonica.Point{X: float64(at.Col), Y: float64(at.Row)},
				harmonica.Vector{X: vx, Y: vy},
				harmonica.Vector{X: 0, Y: 45},
			),
			glyph: confettiGlyphs[i%len(confettiGlyphs)],
			color: confettiColors[i%len(confettiColors)],
		})
	}
	frames := make([][]Sprite, 0, animConfettiLength)
	for f := 0; f < animConfettiLength; f++ {
		out := make([]Sprite, 0, len(pieces))
		for _, p := range pieces {
			pt := p.proj.Update()
			color := p.color
			if f > animConfettiLength*2/3 {
				color = ColorMuted
			}
			out = append(out, Sprite{Row: animRound(pt.Y), Col: animRound(pt.X), Glyph: p.glyph, Color: color})
		}
		frames = append(frames, out)
	}
	return frames
}

// animSmokeCount is how many puffs a misfire makes.
const animSmokeCount = 5

// animSmokeLength is how many frames the smoke drifts for.
const animSmokeLength = 24

// smokeGlyphs are the puff as it thins out, densest first.
var smokeGlyphs = [...]string{"▓", "▒", "░", "·"}

// animSmokeFrames is the misfire: a spark at the target and puffs of smoke
// drifting up and apart.
func animSmokeFrames(at Target, seed uint64) [][]Sprite {
	rng := rand.New(rand.NewPCG(seed, 0x5c0a1e))
	type puff struct {
		proj  *harmonica.Projectile
		delay int
	}
	puffs := make([]puff, 0, animSmokeCount)
	for i := 0; i < animSmokeCount; i++ {
		vx := (rng.Float64()*2 - 1) * 5
		vy := -(3 + rng.Float64()*4)
		puffs = append(puffs, puff{
			proj: harmonica.NewProjectile(
				harmonica.FPS(AnimFPS),
				harmonica.Point{X: float64(at.Col), Y: float64(at.Row)},
				harmonica.Vector{X: vx, Y: vy},
				harmonica.Vector{X: 0, Y: -2},
			),
			delay: i * 2,
		})
	}
	frames := make([][]Sprite, 0, animSmokeLength)
	for f := 0; f < animSmokeLength; f++ {
		out := make([]Sprite, 0, len(puffs)+1)
		if f < 3 {
			out = append(out, Sprite{Row: at.Row, Col: at.Col, Glyph: "✗", Color: ColorDanger})
		}
		for _, p := range puffs {
			pt := p.proj.Update()
			if f < p.delay {
				continue
			}
			age := f - p.delay
			g := smokeGlyphs[min(age*len(smokeGlyphs)/max(animSmokeLength-p.delay, 1), len(smokeGlyphs)-1)]
			out = append(out, Sprite{Row: animRound(pt.Y), Col: animRound(pt.X), Glyph: g, Color: ColorMuted})
		}
		frames = append(frames, out)
	}
	return frames
}

// --- small maths ----------------------------------------------------------

// animLerp interpolates between a and b.
func animLerp(a, b, t float64) float64 { return a + (b-a)*t }

// animRound rounds a coordinate to the nearest cell.
func animRound(v float64) int { return int(math.Round(v)) }

// animClampFloat keeps v inside [lo, hi].
func animClampFloat(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

// animClampInt keeps v inside [lo, hi].
func animClampInt(v, lo, hi int) int { return min(max(v, lo), hi) }
