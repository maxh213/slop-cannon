package agents_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// TestTesterRunSpecCarriesTheBeadsShim covers D-21 for a tester firing: the
// tester prompt carries beads.PromptNote, so the spec has to ship the
// read-only `bd` the prompt promises.
func TestTesterRunSpecCarriesTheBeadsShim(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	realBD := filepath.Join(state, "real", "bd")
	require.NoError(t, os.MkdirAll(filepath.Dir(realBD), 0o755))
	require.NoError(t, os.WriteFile(realBD, []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // fake binary
	shim, err := beads.WriteShim(filepath.Join(state, "bin"), beads.ShimOptions{Real: realBD})
	require.NoError(t, err)

	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Env = map[string]string{"PATH": "/usr/bin", "SC_RUN": "r-1"}
	req.InheritEnv = []string{"HOME"}
	req.UseShim(shim)
	require.Equal(t, shim.Dir+string(os.PathListSeparator)+"/usr/bin", req.Env["PATH"],
		"UseShim prepends the shim to the PATH already in the map")

	spec, p, err := agents.TesterRunSpec(req)
	require.NoError(t, err)
	assert.Equal(t, req.Env["PATH"], spec.Env["PATH"], "the shim reaches the cartridge")
	assert.Equal(t, "r-1", spec.Env["SC_RUN"])
	assert.Equal(t, []string{"HOME"}, spec.InheritEnv)
	assert.Contains(t, p.Text, beads.PromptNote,
		"the prompt promises a read-only bd, so the spec must ship one")

	// The spec holds a copy of the environment.
	req.Env["PATH"] = "/tmp"
	assert.NotEqual(t, "/tmp", spec.Env["PATH"])

	// UseShim also works on a request with no environment at all.
	fresh := tsRequest(domain.LensUX, agents.TesterModeReview)
	fresh.UseShim(shim)
	assert.Equal(t, shim.PathEnv(), fresh.Env["PATH"])

	// Without a shim the child inherits the parent's PATH: installing one
	// is the caller's job, as the doc comment says.
	bare, _, err := agents.TesterRunSpec(tsRequest(domain.LensTest, agents.TesterModeReview))
	require.NoError(t, err)
	assert.Empty(t, bare.Env)
	assert.Nil(t, bare.InheritEnv, "nil means cartridge.DefaultInheritEnv()")
}
