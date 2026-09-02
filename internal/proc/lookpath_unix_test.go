//go:build unix

package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTool writes an executable script named name into a fresh directory and
// prepends that directory to PATH for the rest of the test.
func fakeTool(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestLookPath(t *testing.T) {
	path := fakeTool(t, "sc-fake-tool", "exit 0")
	got, err := LookPath("sc-fake-tool")
	require.NoError(t, err)
	assert.Equal(t, path, got)

	_, err = LookPath("sc-missing-tool")
	require.Error(t, err)
	assert.True(t, IsNotFound(err), "got %v", err)
	assert.Contains(t, err.Error(), "sc-missing-tool")
}

func TestProbe(t *testing.T) {
	t.Run("version on stdout", func(t *testing.T) {
		path := fakeTool(t, "sc-fake-bd", `[ "$1" = "version" ] || exit 2; echo "bd version 1.2.2 (build deadbeef)"`)
		tool, err := Probe(context.Background(), Spec{Command: "sc-fake-bd", Args: []string{"version"}})
		require.NoError(t, err)
		assert.Equal(t, Tool{
			Name:       "sc-fake-bd",
			Path:       path,
			Output:     "bd version 1.2.2 (build deadbeef)",
			Version:    V(1, 2, 2),
			HasVersion: true,
		}, tool)
		assert.True(t, tool.Version.AtLeast(V(1, 2, 2)))
	})

	t.Run("version on stderr", func(t *testing.T) {
		fakeTool(t, "sc-fake-old", `echo "old-tool v0.9" >&2`)
		tool, err := Probe(context.Background(), Spec{Command: "sc-fake-old", Args: []string{"--version"}})
		require.NoError(t, err)
		assert.Equal(t, "old-tool v0.9", tool.Output)
		assert.Equal(t, V(0, 9, 0), tool.Version)
		assert.True(t, tool.HasVersion)
	})

	t.Run("no version number", func(t *testing.T) {
		fakeTool(t, "sc-fake-silent", `echo "no version info"`)
		tool, err := Probe(context.Background(), Spec{Command: "sc-fake-silent"})
		require.NoError(t, err)
		assert.False(t, tool.HasVersion)
		assert.Equal(t, Version{}, tool.Version)
	})

	t.Run("probe command fails", func(t *testing.T) {
		path := fakeTool(t, "sc-fake-broken", `echo "unknown flag" >&2; exit 2`)
		tool, err := Probe(context.Background(), Spec{Command: "sc-fake-broken", Args: []string{"--version"}})
		var ee *ExitError
		require.ErrorAs(t, err, &ee)
		assert.Equal(t, 2, ee.Result.ExitCode)
		assert.Equal(t, path, tool.Path, "the path is still reported so doctor can show where the broken tool lives")
	})

	t.Run("missing tool", func(t *testing.T) {
		_, err := Probe(context.Background(), Spec{Command: "sc-missing-tool"})
		assert.True(t, IsNotFound(err), "got %v", err)
	})

	t.Run("env is limited to PATH and HOME", func(t *testing.T) {
		t.Setenv("SC_PROBE_SECRET", "x")
		fakeTool(t, "sc-fake-env", `env`)
		tool, err := Probe(context.Background(), Spec{Command: "sc-fake-env"})
		require.NoError(t, err)
		assert.NotContains(t, tool.Output, "SC_PROBE_SECRET")
	})

	t.Run("hung probe times out", func(t *testing.T) {
		fakeTool(t, "sc-fake-hang", `sleep 30`)
		_, err := Probe(context.Background(), Spec{Command: "sc-fake-hang", Timeout: 100_000_000})
		assert.True(t, errors.Is(err, ErrTimeout), "got %v", err)
	})
}
