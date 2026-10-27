package junie

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

const largePromptShimScript = `
task=''
previous=''
for arg in "$@"; do
  if [ "$previous" = "--task" ]; then task="$arg"; fi
  previous="$arg"
done
case "$task" in
  "Read the file at this exact path: "*)
    prompt_path=${task#Read the file at this exact path: }
    prompt_path=${prompt_path%. Its complete contents are the task instructions; follow them as the task.}
    if [ -n "$SC_JUNIE_PROMPT_COPY" ]; then cp "$prompt_path" "$SC_JUNIE_PROMPT_COPY"; fi
    if [ -n "$SC_JUNIE_PROMPT_MODE" ]; then stat -c '%a' "$prompt_path" > "$SC_JUNIE_PROMPT_MODE"; fi
    if [ -n "$SC_JUNIE_PROMPT_PATH" ]; then printf '%s' "$prompt_path" > "$SC_JUNIE_PROMPT_PATH"; fi
    ;;
esac
if [ -n "$SC_JUNIE_SLEEP" ]; then sleep "$SC_JUNIE_SLEEP"; fi
if [ -n "$SC_JUNIE_STDERR_FILE" ]; then cat "$SC_JUNIE_STDERR_FILE" >&2; fi
if [ -n "$SC_JUNIE_STDOUT_FILE" ]; then cat "$SC_JUNIE_STDOUT_FILE"; fi
exit "${SC_JUNIE_EXIT:-0}"
`

type promptCapture struct {
	copyPath string
	modePath string
	pathPath string
}

func newPromptCapture(t *testing.T) (promptCapture, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	capture := promptCapture{
		copyPath: filepath.Join(dir, "prompt.copy"),
		modePath: filepath.Join(dir, "prompt.mode"),
		pathPath: filepath.Join(dir, "prompt.path"),
	}
	return capture, map[string]string{
		"SC_JUNIE_PROMPT_COPY": capture.copyPath,
		"SC_JUNIE_PROMPT_MODE": capture.modePath,
		"SC_JUNIE_PROMPT_PATH": capture.pathPath,
	}
}

func largePromptSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	spec := junieSpec(t, env)
	spec.Prompt = largeTestPrompt()
	return spec
}

func (c promptCapture) assertExactAndRemoved(t *testing.T, shim *testutil.Shim, spec cartridge.RunSpec) {
	t.Helper()
	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	task := calls[0].Args[len(calls[0].Args)-1]
	assert.Less(t, len(task), promptFileThreshold, "the full prompt must not appear on argv")
	path := promptPathFromInstruction(task)
	assert.NotEmpty(t, path)
	assert.Equal(t, spec.Workdir, filepath.Dir(path))

	pathData, err := os.ReadFile(c.pathPath)
	require.NoError(t, err)
	assert.Equal(t, path, string(pathData))
	content, err := os.ReadFile(c.copyPath)
	require.NoError(t, err, "the fake CLI must be able to read the project-local prompt")
	assert.Equal(t, spec.Prompt, string(content))
	mode, err := os.ReadFile(c.modePath)
	require.NoError(t, err)
	assert.Equal(t, "600", strings.TrimSpace(string(mode)))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "prompt file must be removed after the session")
}

func argumentValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func TestRunLargePromptSuccessPreservesControlsAndCleansUp(t *testing.T) {
	shim := testutil.FakeBin(t, DefaultBinary, largePromptShimScript)
	capture, env := newPromptCapture(t)
	env["SC_JUNIE_STDOUT_FILE"] = junieFixture(t, "success.json")
	a := junieAdapter(t, config.Cartridge{Args: []string{"--effort", "high"}})
	spec := largePromptSpec(t, env)
	_, res, err := runJunie(t, a, spec)
	require.NoError(t, err)
	assert.Equal(t, "OK", res.Output)
	capture.assertExactAndRemoved(t, shim, spec)

	args := shim.Invocations(t)[0].Args
	assert.Equal(t, spec.Workdir, argumentValue(args, flagProject))
	assert.Equal(t, spec.Model, argumentValue(args, flagModel))
	assert.Equal(t, spec.SystemPrompt, argumentValue(args, flagSystemPrompt))
	assert.Equal(t, "high", argumentValue(args, flagEffort))
	assert.Equal(t, outputFormatJSON, argumentValue(args, flagOutputFormat))
	for _, name := range []string{
		flagConfigDefaults, flagMCPDefaults, flagModelDefaults, flagSkillDefaults,
		flagCommandDefaults, flagAgentDefaults,
	} {
		assert.Equal(t, "false", argumentValue(args, name), name)
	}
	assert.NotEmpty(t, argumentValue(args, flagExtensionsDefault))
	assert.NotContains(t, args, "--gateway-stop")
}

func TestRunLargePromptProcessErrorCleansUp(t *testing.T) {
	shim := testutil.FakeBin(t, DefaultBinary, largePromptShimScript)
	capture, env := newPromptCapture(t)
	env["SC_JUNIE_STDERR_FILE"] = junieFixture(t, "auth.stderr")
	env["SC_JUNIE_EXIT"] = "1"
	spec := largePromptSpec(t, env)
	_, _, err := runJunie(t, junieAdapter(t, config.Cartridge{}), spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(err))
	capture.assertExactAndRemoved(t, shim, spec)
}

func TestRunLargePromptTimeoutCleansUp(t *testing.T) {
	shim := testutil.FakeBin(t, DefaultBinary, largePromptShimScript)
	capture, env := newPromptCapture(t)
	env["SC_JUNIE_SLEEP"] = "5"
	spec := largePromptSpec(t, env)
	spec.Timeout = 250 * time.Millisecond
	_, _, err := runJunie(t, junieAdapter(t, config.Cartridge{}), spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
	capture.assertExactAndRemoved(t, shim, spec)
}

func TestRunLargePromptKillCleansUp(t *testing.T) {
	shim := testutil.FakeBin(t, DefaultBinary, largePromptShimScript)
	capture, env := newPromptCapture(t)
	env["SC_JUNIE_SLEEP"] = "30"
	spec := largePromptSpec(t, env)
	s, err := junieAdapter(t, config.Cartridge{}).Run(context.Background(), spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(capture.pathPath)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)
	s.Kill()
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled))
	capture.assertExactAndRemoved(t, shim, spec)
}

func TestRunLargePromptStartFailureCleansUp(t *testing.T) {
	spec := largePromptSpec(t, nil)
	a := junieAdapter(t, config.Cartridge{Command: "sc-junie-definitely-missing"})
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
	leftovers, globErr := filepath.Glob(filepath.Join(spec.Workdir, promptFilePattern))
	require.NoError(t, globErr)
	assert.Empty(t, leftovers, "a process that never starts must not leave its prompt file")
}
