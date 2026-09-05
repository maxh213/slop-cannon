package gitguard

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheParserReadsGitsOwnCommandLine covers the pieces [Inspect] is built
// from directly, because a misread command line is the one way this package
// can pass a command it should have reported (P9-02, P9-23).
func TestTheParserReadsGitsOwnCommandLine(t *testing.T) {
	sub, rest := Split([]string{"-c", "commit.gpgsign=false", "-c", "rebase.autoStash=false", "rebase", "--quiet", "main"})
	assert.Equal(t, "rebase", sub)
	assert.Equal(t, []string{"--quiet", "main"}, rest)

	sub, rest = Split([]string{"--git-dir=/tmp/x.git", "log", "--merges"})
	assert.Equal(t, "log", sub)
	assert.Equal(t, []string{"--merges"}, rest)

	assert.Empty(t, operands([]string{"--quiet", "--prune"}, false))
	assert.Equal(t, []string{"origin", "refs/heads/a:refs/heads/b"},
		operands([]string{"--quiet", "--", "origin", "refs/heads/a:refs/heads/b"}, false))
	assert.Equal(t, []string{"main"}, operands([]string{"main", "--", "stats/mean.go"}, true))
	assert.Equal(t, []string{"add", "/wt/fx-1", "main"},
		operands([]string{"add", "--quiet", "/wt/fx-1", "-b", "sc/fx-1", "main"}, false))

	specs := refspecs([]string{"--quiet", "origin", "+refs/heads/a:refs/heads/main", "sc/b"})
	require.Len(t, specs, 2)
	assert.Equal(t, refspec{src: "refs/heads/a", dst: "refs/heads/main", force: true}, specs[0])
	assert.Equal(t, refspec{src: "sc/b", dst: "sc/b"}, specs[1])

	env := Env{Default: "main", Remote: "origin"}
	for _, ref := range []string{"main", "refs/heads/main", "origin/main", "refs/remotes/origin/main", "+main", "main^{commit}"} {
		assert.True(t, isDefaultRef(ref, env), "%s names the default branch", ref)
	}
	for _, ref := range []string{"", "sc/fx-1", "refs/heads/mainline", "upstream/main", "HEAD"} {
		assert.False(t, isDefaultRef(ref, env), "%s does not name the default branch", ref)
	}
}
