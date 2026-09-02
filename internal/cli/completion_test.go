package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompletionHelpGolden(t *testing.T) {
	out, err := rootRun(t, "completion", "--help")
	require.NoError(t, err)
	rootGolden(t, "completion-help", out)
}

func TestCompletionScripts(t *testing.T) {
	// Each script must be the real cobra generator's output for "sc": the
	// markers below are the function/registration names those scripts
	// define, so a wrong shell or a wrong root name would fail.
	cases := []struct {
		shell   string
		markers []string
	}{
		{"bash", []string{"__start_sc()", "complete -o default -F __start_sc sc", "__complete"}},
		{"zsh", []string{"#compdef sc", "_sc()", "compdef _sc sc"}},
		{"fish", []string{"complete -c sc", "__sc_perform_completion"}},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			out, err := rootRun(t, "completion", tc.shell)
			require.NoError(t, err)
			for _, m := range tc.markers {
				require.Contains(t, out, m)
			}
		})
	}
}

func TestCompletionNoDescriptions(t *testing.T) {
	withDesc, err := rootRun(t, "completion", "bash")
	require.NoError(t, err)
	noDesc, err := rootRun(t, "completion", "bash", "--"+FlagCompletionNoDescriptions)
	require.NoError(t, err)
	require.NotEqual(t, withDesc, noDesc)
	require.Contains(t, withDesc, "__complete")
	require.Contains(t, noDesc, "__completeNoDesc")

	zsh, err := rootRun(t, "completion", "zsh", "--"+FlagCompletionNoDescriptions)
	require.NoError(t, err)
	require.Contains(t, zsh, "__completeNoDesc")

	fish, err := rootRun(t, "completion", "fish", "--"+FlagCompletionNoDescriptions)
	require.NoError(t, err)
	require.Contains(t, fish, "__completeNoDesc")
}

func TestCompletionRejectsBadArgs(t *testing.T) {
	_, err := rootRun(t, "completion")
	require.Error(t, err)
	require.Equal(t, "sc completion needs a shell argument: one of bash, zsh, fish", err.Error())

	_, err = rootRun(t, "completion", "powershell")
	require.Error(t, err)
	require.Equal(t, `unsupported shell "powershell": choose one of bash, zsh, fish`, err.Error())

	_, err = rootRun(t, "completion", "bash", "zsh")
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepts exactly one shell argument")

	// WriteCompletion is exported; it rejects unknown shells the same way.
	err = WriteCompletion(&bytes.Buffer{}, NewRootCommand(&bytes.Buffer{}), "tcsh", true)
	require.Error(t, err)
	require.Equal(t, `unsupported shell "tcsh": choose one of bash, zsh, fish`, err.Error())
}

func TestCompletionReplacesCobraDefault(t *testing.T) {
	// Executing the tree lets cobra add its default commands; ours must be
	// the only "completion" and it must not grow cobra's shell subcommands.
	var out bytes.Buffer
	root := NewRootCommand(&out)
	root.SetArgs([]string{"completion", "--help"})
	require.NoError(t, root.Execute())

	var found int
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			found++
			require.Empty(t, c.Commands(), "cobra's default completion subcommands must not be attached")
			require.Equal(t, completionLong, c.Long)
		}
	}
	require.Equal(t, 1, found)
	require.NotContains(t, out.String(), "Generate the autocompletion script for the specified shell")
}

func TestCompletionShellCompletesShells(t *testing.T) {
	// The hidden __complete command that the scripts call must offer the
	// supported shells for `sc completion <TAB>`.
	var stdout, stderr bytes.Buffer
	code := ExecuteContext(context.Background(), []string{"__complete", "completion", ""}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	for _, shell := range CompletionShells {
		require.Contains(t, stdout.String(), shell+"\n")
	}
	require.NotContains(t, stdout.String(), "powershell")

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.GreaterOrEqual(t, len(lines), len(CompletionShells)+1, "cobra ends the list with a :<directive> line")
}
