package junie

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func largeTestPrompt() string {
	prompt := "BEGIN-EXACT-PROMPT\n" + strings.Repeat("0123456789abcdef\n", 8192) + "END-EXACT-PROMPT\x00"
	if len(prompt) <= 128*1024 {
		panic("large prompt fixture is too small")
	}
	return prompt
}

func TestTaskForSpecUsesLiteralPromptAtThreshold(t *testing.T) {
	t.Parallel()
	prompt := strings.Repeat("x", promptFileThreshold)
	task, path, err := taskForSpec(prompt, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, prompt, task)
	assert.Empty(t, path)
}

func TestTaskForSpecWritesExactLargePrompt(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	prompt := largeTestPrompt()
	task, path, err := taskForSpec(prompt, workdir)
	require.NoError(t, err)
	t.Cleanup(func() { removePromptFile(path) })

	assert.Less(t, len(task), promptFileThreshold)
	assert.Equal(t, path, promptPathFromInstruction(task))
	assert.Equal(t, workdir, filepath.Dir(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, prompt, string(data), "file bytes must be the exact user prompt, including NUL")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
