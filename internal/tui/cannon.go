package tui

import (
	"math"
	"strings"

	"github.com/charmbracelet/harmonica"
)

// The cannon lives in the top-left corner of the header and points right, at
// the main pane. Firing an agent kicks the barrel back into the breech and
// lights a muzzle flash; a spring pushes it out again (P7-06).
//
// Everything here is a pure function of a frame number, so a golden frame is
// reproducible without a clock.

// CannonFieldWidth is how many columns the header reserves for the cannon:
// the five of [CannonSprite] plus the two-column gap before the run fields.
// Every frame [RenderCannon] returns is exactly this wide, so the header does
// not jitter while the cannon recoils.
const CannonFieldWidth = 7

// CannonMuzzleCol is the column of the muzzle (`▶`) when the cannon is at
// rest, counted from the left edge of the header. A projectile leaves from
// the cell after it.
const CannonMuzzleCol = 4

// CannonMaxRecoil is the deepest the barrel is driven into the breech, in
// columns.
const CannonMaxRecoil = 2

// cannonBarrels are the barrel sprites indexed by recoil depth: at rest the
// full [CannonSprite], then one and two columns shorter as the barrel is
// driven back.
var cannonBarrels = [CannonMaxRecoil + 1]string{CannonSprite, "▄█▓▶", "▄█▶"}

// cannonFlashGlyphs are the muzzle-flash frames, brightest first.
var cannonFlashGlyphs = [...]string{"✸", "✷", "·"}

// CannonFrame is one drawing of the cannon: how far the barrel is recoiled
// and which muzzle-flash glyph, if any, sits in front of it.
type CannonFrame struct {
	// Recoil is how many columns the barrel is driven back, 0…[CannonMaxRecoil].
	Recoil int
	// Flash indexes the muzzle-flash glyph, or is negative when the muzzle is
	// dark.
	Flash int
}

// CannonRest is the cannon standing by: no recoil, no flash.
var CannonRest = CannonFrame{Flash: -1}

// Firing reports whether the frame shows a muzzle flash.
func (f CannonFrame) Firing() bool { return f.Flash >= 0 && f.Flash < len(cannonFlashGlyphs) }

// barrel returns the barrel sprite for the frame's recoil depth.
func (f CannonFrame) barrel() string {
	return cannonBarrels[animClampInt(f.Recoil, 0, CannonMaxRecoil)]
}

// flash returns the muzzle-flash glyph, or "" when the muzzle is dark.
func (f CannonFrame) flash() string {
	if !f.Firing() {
		return ""
	}
	return cannonFlashGlyphs[f.Flash]
}

// String renders the frame as plain text, exactly [CannonFieldWidth] columns
// wide and with no ANSI. [RenderCannon] is the coloured version.
func (f CannonFrame) String() string {
	barrel := f.barrel()
	out := barrel + f.flash()
	if pad := CannonFieldWidth - len([]rune(out)); pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

// RenderCannon draws one cannon frame with the theme's colours: the barrel in
// the chrome's text colour and the muzzle flash in the fire colour. The
// result is exactly [CannonFieldWidth] columns wide. A nil theme renders the
// no-colour profile, which is what `NO_COLOR` and the golden frames want.
func RenderCannon(t *Theme, f CannonFrame) string {
	if t == nil {
		t = ThemeFor(ProfileNoColor)
	}
	barrel := f.barrel()
	out := t.Fire().Render(barrel)
	width := len([]rune(barrel))
	if g := f.flash(); g != "" {
		out += t.Warning().Render(g)
		width++
	}
	if pad := CannonFieldWidth - width; pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

// cannonRecoilSpring is the spring that pushes the barrel back out after a
// shot: stiff, and damped just short of critically so it settles without a
// visible bounce.
func cannonRecoilSpring() harmonica.Spring {
	return harmonica.NewSpring(harmonica.FPS(AnimFPS), 16.0, 0.6)
}

// cannonRecoilMaxFrames caps the recoil sequence so a spring that never quite
// settles cannot animate forever.
const cannonRecoilMaxFrames = 24

// cannonRecoil is the recoil sequence of one shot, computed once: the barrel
// starts fully driven back with the muzzle lit and springs out to rest.
var cannonRecoil = buildCannonRecoil()

// buildCannonRecoil integrates the recoil spring frame by frame.
func buildCannonRecoil() []CannonFrame {
	s := cannonRecoilSpring()
	pos, vel := float64(CannonMaxRecoil), 0.0
	out := make([]CannonFrame, 0, cannonRecoilMaxFrames)
	for i := 0; i < cannonRecoilMaxFrames; i++ {
		f := CannonFrame{Recoil: animClampInt(int(math.Round(pos)), 0, CannonMaxRecoil), Flash: -1}
		if i < len(cannonFlashGlyphs) {
			f.Flash = i
		}
		out = append(out, f)
		if f.Recoil == 0 && !f.Firing() && math.Abs(vel) < 0.05 {
			break
		}
		pos, vel = s.Update(pos, vel, 0)
	}
	return out
}

// CannonRecoilFrames returns a copy of the recoil sequence a single shot
// plays, oldest frame first. It is deterministic: the same slice every run.
func CannonRecoilFrames() []CannonFrame {
	out := make([]CannonFrame, len(cannonRecoil))
	copy(out, cannonRecoil)
	return out
}

// cannonFrameAt returns the recoil frame i of a shot, or [CannonRest] once
// the sequence has played out.
func cannonFrameAt(i int) CannonFrame {
	if i < 0 || i >= len(cannonRecoil) {
		return CannonRest
	}
	return cannonRecoil[i]
}

// cannonCombine merges the cannon frames of every shot in flight: the deepest
// recoil and the brightest flash win, so overlapping shots read as one
// hard-working gun rather than a flicker.
func cannonCombine(frames []CannonFrame) CannonFrame {
	out := CannonRest
	for _, f := range frames {
		if f.Recoil > out.Recoil {
			out.Recoil = f.Recoil
		}
		if f.Firing() && (!out.Firing() || f.Flash < out.Flash) {
			out.Flash = f.Flash
		}
	}
	return out
}
