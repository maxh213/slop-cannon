package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// rootGoldenDir is where help goldens live, relative to this package.
const rootGoldenDir = "../../testdata/cli"

// rootGolden compares got with testdata/cli/<name>.golden. Set UPDATE_GOLDEN=1
// (make update-golden) to rewrite the file instead.
func rootGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(rootGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// rootRun executes the command tree with args and returns combined output.
func rootRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRootCommand(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestRootHelpGolden(t *testing.T) {
	out, err := rootRun(t, "--help")
	require.NoError(t, err)
	rootGolden(t, "help", out)
}

func TestVersionHelpGolden(t *testing.T) {
	out, err := rootRun(t, "version", "--help")
	require.NoError(t, err)
	rootGolden(t, "version-help", out)
}

func TestRootNoArgsPrintsHelp(t *testing.T) {
	out, err := rootRun(t)
	require.NoError(t, err)
	help, err := rootRun(t, "--help")
	require.NoError(t, err)
	require.Equal(t, help, out)
}

func TestRootHelpExamplesMirrorREADME(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	_, rest, found := strings.Cut(string(readme), "```\n")
	require.True(t, found, "README.md has no fenced command block")
	block, _, found := strings.Cut(rest, "```")
	require.True(t, found, "README.md fenced block is not closed")

	// Trailing whitespace on a line is not meaningful; everything else is.
	normalise := func(s string) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			lines[i] = strings.TrimRight(l, " \t")
		}
		return strings.Join(lines, "\n")
	}
	require.Equal(t, normalise(block), normalise(rootExamples))

	// And the examples really are what --help prints.
	help, err := rootRun(t, "--help")
	require.NoError(t, err)
	for _, line := range strings.Split(normalise(rootExamples), "\n") {
		if line != "" {
			require.Contains(t, help, "  "+line+"\n")
		}
	}
}

func TestVersionCommand(t *testing.T) {
	out, err := rootRun(t, "version")
	require.NoError(t, err)
	require.Equal(t, "sc dev (commit none, built unknown)\n", out)
}

func TestVersionPrintsInjectedValues(t *testing.T) {
	v, c, d := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = v, c, d })
	Version, Commit, Date = "1.2.3", "abc1234", "2026-09-02T00:00:00Z"

	out, err := rootRun(t, "version")
	require.NoError(t, err)
	require.Equal(t, "sc 1.2.3 (commit abc1234, built 2026-09-02T00:00:00Z)\n", out)

	out, err = rootRun(t, "--json", "version")
	require.NoError(t, err)
	var got VersionInfo
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, VersionInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-09-02T00:00:00Z"}, got)
	require.True(t, strings.HasSuffix(out, "}\n"))
}

// TestVersionLdflagsMatchMakefile guards P1-10: the Makefile injects
// Version, Commit and Date with -X, so renaming any of the three variables
// (or the Makefile's LDFLAGS) must fail here, not silently print "dev".
func TestVersionLdflagsMatchMakefile(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)
	for _, name := range []string{"Version", "Commit", "Date"} {
		flag := "-X $(MODULE)/internal/cli." + name + "="
		require.Contains(t, string(mk), flag, "Makefile LDFLAGS no longer injects cli.%s", name)
	}
	// The variables the flags name must exist with the type -X can set.
	var _ = []*string{&Version, &Commit, &Date}
}

func TestVersionRejectsArgs(t *testing.T) {
	_, err := rootRun(t, "version", "extra")
	require.Error(t, err)
}

func TestUnknownSubcommandSuggestsNearest(t *testing.T) {
	_, err := rootRun(t, "verison")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown command "verison" for "sc"`)
	require.Contains(t, err.Error(), "Did you mean this?")
	require.Contains(t, err.Error(), "\tversion\n")
	require.Contains(t, err.Error(), "Run 'sc --help' for usage.")
}

func TestUnknownSubcommandWithoutSuggestion(t *testing.T) {
	_, err := rootRun(t, "frobnicate")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown command "frobnicate" for "sc"`)
	require.NotContains(t, err.Error(), "Did you mean")
}

func TestRootRepoPathIsNotAnUnknownCommand(t *testing.T) {
	// None of these is a git repository, so the run form refuses them, but
	// they must reach it rather than be reported as mistyped subcommands.
	for _, arg := range []string{"./repo-path", "/abs/path", "~/repo", "sub/dir", t.TempDir()} {
		_, err := rootRun(t, arg)
		require.Error(t, err, arg)
		require.NotContains(t, err.Error(), "unknown command", arg)
	}
	_, err := rootRun(t, "./a", "./b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepts 1 arg(s)")
}

// rootProbe adds a throwaway subcommand that records the resolved global
// options, so the test does not have to touch the package registry.
func rootProbe(t *testing.T, args ...string) (GlobalOptions, error) {
	t.Helper()
	var out bytes.Buffer
	var got GlobalOptions
	root := NewRootCommand(&out)
	root.AddCommand(&cobra.Command{
		Use:  "probe",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			got, err = GlobalOpts(cmd)
			return err
		},
	})
	root.SetArgs(append(args, "probe"))
	return got, root.Execute()
}

func TestGlobalFlagsDefaults(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	require.NoError(t, os.Unsetenv("NO_COLOR"))
	got, err := rootProbe(t)
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, GlobalOptions{Repo: cwd}, got)
}

func TestGlobalFlagsAllSet(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	require.NoError(t, os.Unsetenv("NO_COLOR"))
	dir := t.TempDir()
	got, err := rootProbe(t, "--repo", dir, "--global", "-v", "--json", "--no-color")
	require.NoError(t, err)
	require.Equal(t, GlobalOptions{Repo: dir, Global: true, Verbose: true, JSON: true, NoColor: true}, got)
}

func TestGlobalFlagsAfterSubcommand(t *testing.T) {
	// Persistent flags may follow the subcommand name too.
	var out bytes.Buffer
	var got GlobalOptions
	root := NewRootCommand(&out)
	root.AddCommand(&cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			got, err = GlobalOpts(cmd)
			return err
		},
	})
	root.SetArgs([]string{"probe", "--verbose", "--global"})
	require.NoError(t, root.Execute())
	require.True(t, got.Verbose)
	require.True(t, got.Global)
}

func TestGlobalFlagsRepoRelativeIsMadeAbsolute(t *testing.T) {
	got, err := rootProbe(t, "--repo", "../cli")
	require.NoError(t, err)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, cwd, got.Repo)
}

func TestGlobalFlagsRepoMustBeDirectory(t *testing.T) {
	_, err := rootProbe(t, "--repo", filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Contains(t, err.Error(), `--repo "`)

	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, err = rootProbe(t, "--repo", file)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func TestGlobalFlagsNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	got, err := rootProbe(t)
	require.NoError(t, err)
	require.True(t, got.NoColor)
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	saved := registry
	t.Cleanup(func() { registry = saved })
	register(func() *cobra.Command { return &cobra.Command{Use: "version"} })
	require.PanicsWithValue(t,
		`cli: subcommand "version" registered twice ("version" and "version")`,
		func() { NewRootCommand(&bytes.Buffer{}) })
}

func TestRegistryCommandsAreSortedAndFresh(t *testing.T) {
	a := NewRootCommand(&bytes.Buffer{})
	b := NewRootCommand(&bytes.Buffer{})
	require.NotEmpty(t, a.Commands())
	for i := range a.Commands() {
		require.NotSame(t, a.Commands()[i], b.Commands()[i], "factories must build fresh commands per root")
	}
	names := make([]string, 0, len(a.Commands()))
	for _, c := range a.Commands() {
		names = append(names, c.Name())
	}
	require.IsIncreasing(t, names)
}

func TestExecuteContextExitCodes(t *testing.T) {
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := ExecuteContext(context.Background(), args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, stdout, stderr := run("version")
	require.Equal(t, 0, code)
	require.Equal(t, "sc dev (commit none, built unknown)\n", stdout)
	require.Empty(t, stderr)

	code, stdout, stderr = run("verison")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.True(t, strings.HasPrefix(stderr, `sc: unknown command "verison"`), stderr)
}

func TestExecuteContextHonoursExitError(t *testing.T) {
	saved := registry
	t.Cleanup(func() { registry = saved })
	register(func() *cobra.Command {
		return &cobra.Command{
			Use:  "silent-fail",
			RunE: func(*cobra.Command, []string) error { return &ExitError{Code: 3} },
		}
	})
	register(func() *cobra.Command {
		return &cobra.Command{
			Use:  "loud-fail",
			RunE: func(*cobra.Command, []string) error { return &ExitError{Code: 4, Err: errors.New("boom")} },
		}
	})

	var stdout, stderr bytes.Buffer
	require.Equal(t, 3, ExecuteContext(context.Background(), []string{"silent-fail"}, &stdout, &stderr))
	require.Empty(t, stderr.String())

	stderr.Reset()
	require.Equal(t, 4, ExecuteContext(context.Background(), []string{"loud-fail"}, &stdout, &stderr))
	require.Equal(t, "sc: boom\n", stderr.String())

	err := &ExitError{Code: 3}
	require.Equal(t, "exit status 3", err.Error())
	wrapped := &ExitError{Code: 4, Err: ErrNotImplemented}
	require.ErrorIs(t, wrapped, ErrNotImplemented)
}
