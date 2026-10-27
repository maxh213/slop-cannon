package dash

import (
	"math"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFaintTextContrast(t *testing.T) {
	colors := dashboardColors()
	for _, name := range []string{"bg", "surface", "surface-raised", "faint"} {
		require.NotEmpty(t, colors[name], name)
	}
	for _, background := range []string{"bg", "surface", "surface-raised"} {
		ratio := contrastRatio(colors["faint"], colors[background])
		require.GreaterOrEqual(t, ratio, 4.5, "--faint on --%s", background)
	}
}

func TestRecentCardBottomContrast(t *testing.T) {
	recentRule := cssRule(".card-recent")
	require.NotEmpty(t, recentRule)
	require.NotContains(t, recentRule, "opacity", "recent-card opacity also dims essential text")
	bottomRule := cssRule(".card-bottom")
	require.Contains(t, bottomRule, "color: var(--faint)")
	require.Contains(t, bottomRule, "font: 9px")
	colors := dashboardColors()
	for _, background := range []string{"bg", "surface", "surface-raised"} {
		ratio := contrastRatio(colors["faint"], colors[background])
		require.GreaterOrEqual(t, ratio, 4.5, ".card-bottom on --%s", background)
	}
}

func dashboardColors() map[string]string {
	colors := make(map[string]string)
	for _, match := range regexp.MustCompile(`--(bg|surface|surface-raised|faint):\s*(#[0-9a-f]{6})`).FindAllStringSubmatch(styleCSS, -1) {
		colors[match[1]] = match[2]
	}
	return colors
}

func cssRule(selector string) string {
	matches := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + ` \{[^}]*\}`).FindString(styleCSS)
	return matches
}

func contrastRatio(foreground, background string) float64 {
	a, b := relativeLuminance(foreground), relativeLuminance(background)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

func relativeLuminance(hex string) float64 {
	channels := [3]float64{}
	for i := range channels {
		value, err := strconv.ParseUint(hex[1+i*2:3+i*2], 16, 8)
		if err != nil {
			return 0
		}
		channel := float64(value) / 255
		if channel <= 0.04045 {
			channels[i] = channel / 12.92
		} else {
			channels[i] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2]
}
