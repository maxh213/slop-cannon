package agy

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

const largeStructuredSuffix = "\n<sc-result>{\"contract\":\"impl_report\",\"summary\":\"exact suffix\"}</sc-result>"

func largeAgySpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	spec := agySpec(t, env)
	spec.Prompt = strings.Repeat("λ", 70*1024) + largeStructuredSuffix
	return spec
}

func promptFiles(t *testing.T, workdir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(workdir, promptFilePattern))
	require.NoError(t, err)
	return paths
}

func promptPathFromInstruction(t *testing.T, instruction string) string {
	t.Helper()
	const prefix = "Use the Read tool to read the exact file at "
	const suffix = ". The complete contents"
	require.True(t, strings.HasPrefix(instruction, prefix), instruction)
	rest := strings.TrimPrefix(instruction, prefix)
	quoted, _, found := strings.Cut(rest, suffix)
	require.True(t, found, instruction)
	path, err := strconv.Unquote(quoted)
	require.NoError(t, err)
	return path
}

func TestRunLargePromptUsesPrivateReadableFile(t *testing.T) {
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadWrite, cartridge.ToolReadOnly} {
		t.Run(policy.String(), func(t *testing.T) {
			shim := agyShim(t)
			capture := filepath.Join(t.TempDir(), "captured-prompt")
			mode := filepath.Join(t.TempDir(), "captured-mode")
			a := newAdapter(t, config.Cartridge{})
			spec := largeAgySpec(t, map[string]string{
				"SC_AGY_FIXTURE":        agyFixture(t, "success.json"),
				"SC_AGY_PROMPT_CAPTURE": capture,
				"SC_AGY_PROMPT_MODE":    mode,
			})
			spec.ToolPolicy = policy
			fullPrompt := Prompt(spec)
			require.Greater(t, len(fullPrompt), 128*1024)

			_, res, err := collect(t, a, spec)
			require.NoError(t, err)
			assert.Equal(t, "PONG", res.Output)
			captured, err := os.ReadFile(capture)
			require.NoError(t, err)
			assert.Equal(t, fullPrompt, string(captured), "the file is the exact combined prompt and structured suffix")
			modeBytes, err := os.ReadFile(mode)
			require.NoError(t, err)
			assert.Equal(t, "600", strings.TrimSpace(string(modeBytes)))

			calls := shim.Invocations(t)
			require.Len(t, calls, 1)
			args := calls[0].Args
			instruction := args[len(args)-1]
			assert.Equal(t, flagPrint, args[len(args)-2])
			assert.Less(t, len(instruction), 2048, "the full prompt is not in argv")
			assert.NotContains(t, strings.Join(args, "\x00"), largeStructuredSuffix)
			path := promptPathFromInstruction(t, instruction)
			assert.Equal(t, spec.Workdir, filepath.Dir(path), "the Read target stays inside --add-dir")
			assert.Contains(t, args, flagAddDir)
			assert.Contains(t, args, spec.Workdir)
			assert.Empty(t, promptFiles(t, spec.Workdir), "success cleans the prompt file")
			if policy == cartridge.ToolReadOnly {
				assert.Contains(t, args, modePlan)
				assert.NotContains(t, args, flagSkipPermissions)
			} else {
				assert.Contains(t, args, modeAcceptEdits)
				assert.Contains(t, args, flagSkipPermissions)
			}
		})
	}
}

func TestRunSmallPromptKeepsDirectArgv(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	spec := agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "success.json")})
	_, _, err := collect(t, a, spec)
	require.NoError(t, err)
	args := shim.Invocations(t)[0].Args
	assert.Equal(t, Prompt(spec), args[len(args)-1])
	assert.Empty(t, promptFiles(t, spec.Workdir))
}

func TestRunLargePromptCleanupOnEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     func(*testing.T) map[string]string
		timeout time.Duration
		want    error
	}{
		{
			name: "success",
			env: func(t *testing.T) map[string]string {
				return map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "success.json")}
			},
		},
		{
			name: "process error",
			env: func(*testing.T) map[string]string {
				return map[string]string{"SC_AGY_STDERR": "boom", "SC_AGY_EXIT": "1"}
			},
			want: cartridge.ErrCrash,
		},
		{
			name: "malformed result",
			env: func(t *testing.T) map[string]string {
				return map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "malformed.json")}
			},
			want: cartridge.ErrCrash,
		},
		{
			name: "timeout",
			env: func(*testing.T) map[string]string {
				return map[string]string{"SC_AGY_SLEEP": "5"}
			},
			timeout: 250 * time.Millisecond,
			want:    cartridge.ErrTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agyShim(t)
			a := newAdapter(t, config.Cartridge{})
			spec := largeAgySpec(t, tc.env(t))
			if tc.timeout > 0 {
				spec.Timeout = tc.timeout
			}
			_, _, err := collect(t, a, spec)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			assert.Empty(t, promptFiles(t, spec.Workdir))
		})
	}
}

func TestRunLargePromptCleanupOnKillAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "kill"},
		{name: "context cancellation", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agyShim(t)
			a := newAdapter(t, config.Cartridge{})
			spec := largeAgySpec(t, map[string]string{"SC_AGY_SLEEP": "5"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := a.Run(ctx, spec)
			require.NoError(t, err)
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
			} else {
				time.AfterFunc(100*time.Millisecond, s.Kill)
			}
			_, err = cartridge.Collect(s, nil)
			require.ErrorIs(t, err, cartridge.ErrKilled)
			assert.Empty(t, promptFiles(t, spec.Workdir))
		})
	}
}

func TestRunLargePromptCleanupOnStartFailure(t *testing.T) {
	a := newAdapter(t, config.Cartridge{Command: filepath.Join(t.TempDir(), "missing-agy")})
	spec := largeAgySpec(t, nil)
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Empty(t, promptFiles(t, spec.Workdir))
}

func TestRunLargePromptReadExecStillRefusedWithoutFile(t *testing.T) {
	a := newAdapter(t, config.Cartridge{})
	spec := largeAgySpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadExec
	_, err := a.Run(context.Background(), spec)
	require.ErrorIs(t, err, ErrReadExecUnsupported)
	assert.Empty(t, promptFiles(t, spec.Workdir))
}
