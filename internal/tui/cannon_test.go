package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// cannonPlainTheme is the theme the cannon tests render with: no colour, so a
// frame is exactly its glyphs.
func cannonPlainTheme() *Theme { return ThemeFor(ProfileNoColor) }

func TestCannonFrameStringIsAlwaysTheSameWidth(t *testing.T) {
	t.Parallel()
	for _, f := range []CannonFrame{
		CannonRest,
		{Recoil: 1, Flash: -1},
		{Recoil: 2, Flash: -1},
		{Recoil: 2, Flash: 0},
		{Recoil: 0, Flash: 2},
		{Recoil: 9, Flash: 9},
		{Recoil: -3, Flash: -9},
	} {
		got := f.String()
		require.Equal(t, CannonFieldWidth, len([]rune(got)), "frame %+v rendered %q", f, got)
		require.NotContains(t, got, "\x1b", "plain frames carry no ANSI")
	}
}

func TestCannonFrameDrawsBarrelAndFlash(t *testing.T) {
	t.Parallel()
	require.Equal(t, CannonSprite+"  ", CannonRest.String())
	require.Equal(t, "▄█▓▶   ", CannonFrame{Recoil: 1, Flash: -1}.String())
	require.Equal(t, "▄█▶    ", CannonFrame{Recoil: 2, Flash: -1}.String())
	require.Equal(t, "▄█▓═▶✸ ", CannonFrame{Recoil: 0, Flash: 0}.String())
	require.Equal(t, "▄█▶✸   ", CannonFrame{Recoil: 2, Flash: 0}.String())

	require.True(t, CannonFrame{Flash: 0}.Firing())
	require.False(t, CannonRest.Firing())
	require.False(t, CannonFrame{Flash: 99}.Firing())
}

func TestCannonSpriteIsTheRestingBarrel(t *testing.T) {
	t.Parallel()
	// The header's static sprite and the animation's resting frame must be
	// the same picture, or wiring the animator would move the cannon.
	require.Equal(t, CannonSprite, cannonBarrels[0])
	require.Equal(t, CannonMuzzleCol, len([]rune(CannonSprite))-1)
	require.Equal(t, CannonMaxRecoil, len(cannonBarrels)-1)
}

func TestCannonRecoilFramesSpringBackToRest(t *testing.T) {
	t.Parallel()
	frames := CannonRecoilFrames()
	require.NotEmpty(t, frames)
	require.LessOrEqual(t, len(frames), cannonRecoilMaxFrames)

	require.Equal(t, CannonMaxRecoil, frames[0].Recoil, "the shot drives the barrel all the way back")
	require.True(t, frames[0].Firing(), "the first frame lights the muzzle")
	require.Equal(t, 0, frames[len(frames)-1].Recoil, "the barrel comes back out")
	require.False(t, frames[len(frames)-1].Firing(), "the flash dies before the barrel settles")

	// The flash is short and the recoil never deepens after the shot.
	flashes := 0
	for i, f := range frames {
		require.LessOrEqual(t, f.Recoil, CannonMaxRecoil)
		require.GreaterOrEqual(t, f.Recoil, 0)
		if f.Firing() {
			flashes++
			require.Less(t, i, len(cannonFlashGlyphs), "the flash is over quickly")
		}
	}
	require.Equal(t, len(cannonFlashGlyphs), flashes)
}

func TestCannonRecoilFramesAreDeterministicAndCopied(t *testing.T) {
	t.Parallel()
	a, b := CannonRecoilFrames(), CannonRecoilFrames()
	require.Equal(t, a, b)

	a[0] = CannonRest
	require.NotEqual(t, a[0], CannonRecoilFrames()[0], "the caller gets a copy")
}

func TestCannonFrameAtClampsToRest(t *testing.T) {
	t.Parallel()
	require.Equal(t, cannonRecoil[0], cannonFrameAt(0))
	require.Equal(t, CannonRest, cannonFrameAt(-1))
	require.Equal(t, CannonRest, cannonFrameAt(len(cannonRecoil)))
	require.Equal(t, CannonRest, cannonFrameAt(1000))
}

func TestCannonCombineTakesTheDeepestRecoilAndBrightestFlash(t *testing.T) {
	t.Parallel()
	require.Equal(t, CannonRest, cannonCombine(nil))
	got := cannonCombine([]CannonFrame{
		{Recoil: 0, Flash: 2},
		{Recoil: 2, Flash: -1},
		{Recoil: 1, Flash: 1},
	})
	require.Equal(t, CannonFrame{Recoil: 2, Flash: 1}, got)
}

func TestRenderCannonRespectsTheProfile(t *testing.T) {
	t.Parallel()
	plain := RenderCannon(cannonPlainTheme(), CannonFrame{Recoil: 1, Flash: 0})
	require.Equal(t, "▄█▓▶✸  ", plain)
	require.NotContains(t, plain, "\x1b")

	colored := RenderCannon(ThemeFor(ProfileTrueColor), CannonFrame{Recoil: 1, Flash: 0})
	require.Contains(t, colored, "\x1b[", "a colour profile styles the barrel")
	require.Equal(t, CannonFieldWidth, ansi.StringWidth(colored))

	require.Equal(t, plain, RenderCannon(nil, CannonFrame{Recoil: 1, Flash: 0}), "a nil theme renders plain")
}

func TestRenderCannonKeepsTheHeaderStill(t *testing.T) {
	t.Parallel()
	for i := range CannonRecoilFrames() {
		got := RenderCannon(cannonPlainTheme(), cannonFrameAt(i))
		require.Equal(t, CannonFieldWidth, len([]rune(got)), "frame %d is %q", i, got)
	}
}
