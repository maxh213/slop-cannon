package dandelion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDocs_SectionMatchesCode fails when the Dandelion section of
// docs/cartridges.md drifts from the code: every provider id and how it is
// launched, the router flags, the efforts passed on and the timings.
func TestDocs_SectionMatchesCode(t *testing.T) {
	t.Parallel()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "docs", "cartridges.md"))
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	doc := string(data)
	start := strings.Index(doc, "## Dandelion (`dandelion`)")
	require.GreaterOrEqual(t, start, 0, "docs/cartridges.md has no Dandelion section")
	end := strings.Index(doc[start:], "\n## Conformance suite")
	require.Positive(t, end)
	section := doc[start : start+end]

	mentions := func(what, needle string) {
		t.Helper()
		assert.True(t, strings.Contains(section, needle), "%s %q is not mentioned in the Dandelion section of docs/cartridges.md", what, needle)
	}
	for _, pr := range Providers {
		mentions("provider", "`"+pr.ID+"`")
		if pr.ConfigDirEnv != "" {
			mentions("config dir variable", "`"+pr.ConfigDirEnv+"`")
			mentions("config dir fallback", "`~/"+pr.ConfigDirFallback+"`")
		}
	}
	for _, e := range Efforts {
		mentions("effort", "`"+e+"`")
	}
	for _, flag := range []string{routeSubcommand, flagHigh, flagMaxAge + " 120", disableEnv, string(ModeHigh), string(ModeStandard)} {
		mentions("router flag or mode", flag)
	}
	assert.Equal(t, 2*time.Minute, CacheTTL)
	mentions("cache TTL", "**2 minutes**")
	assert.Equal(t, 5*time.Minute, NoRouteRetryAfter)
	mentions("no-route pause", "`RetryAfter` 5 min")
	assert.Equal(t, time.Minute, AccountRetryAfter)
	mentions("account pause", "`RetryAfter` 1 min")
	assert.Equal(t, 90*time.Second, RouteTimeout)
	mentions("router timeout", "(90 s)")
}
