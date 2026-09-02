package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCommand(&out)
	root.SetArgs([]string{"version"})
	require.NoError(t, root.Execute())
	require.Equal(t, "sc dev (commit none, built unknown)\n", out.String())
}

func TestVersionRejectsArgs(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCommand(&out)
	root.SetArgs([]string{"version", "extra"})
	require.Error(t, root.Execute())
}
