package beads

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// shimFakeScript is the FakeBin body standing in for the real bd: it prints
// a version envelope for `version`, refuses `update` the way bd's
// --readonly does, and echoes its arguments otherwise.
const shimFakeScript = `
for a in "$@"; do
  case "$a" in
    version) printf '{"schema_version":1,"data":{"version":"1.2.2","build":"dev"}}\n'; exit 0 ;;
    update) echo "Error: operation 'update' is not allowed in read-only mode" >&2; exit 1 ;;
  esac
done
printf '%s\n' "$@"
`

// shimRun executes the shim script by path from dir with the given
// arguments and the shim directory first on PATH, returning stdout, stderr
// and the exit status.
func shimRun(t *testing.T, s Shim, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(s.Path, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + s.PathEnv()}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		require.ErrorAs(t, err, &ee, "running shim: %v", err)
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// shimInstall installs a FakeBin bd and writes a shim in front of it.
func shimInstall(t *testing.T) (Shim, *testutil.Shim) {
	t.Helper()
	fake := testutil.FakeBin(t, "bd", shimFakeScript)
	s, err := WriteShim(filepath.Join(t.TempDir(), "bin"), ShimOptions{})
	require.NoError(t, err)
	return s, fake
}

func TestWriteShimWritesExecutableScript(t *testing.T) {
	s, fake := shimInstall(t)
	assert.Equal(t, filepath.Join(s.Dir, "bd"), s.Path)
	assert.Equal(t, fake.Path, s.Real, "resolves the first bd on PATH")
	assert.True(t, filepath.IsAbs(s.Dir))

	info, err := os.Stat(s.Path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111, "shim is executable")
	data, err := os.ReadFile(s.Path)
	require.NoError(t, err)
	text := string(data)
	assert.True(t, strings.HasPrefix(text, "#!/bin/sh\n"+ShimHeader+"\n"))
	assert.Contains(t, text, "exec '"+fake.Path+"' --readonly \"$@\"")
	assert.Equal(t, ShimScript(fake.Path), text)
	assert.True(t, IsShim(s.Path))
	assert.False(t, IsShim(fake.Path), "a FakeBin script is not our shim")
	assert.False(t, IsShim(filepath.Join(s.Dir, "missing")))

	entries, err := os.ReadDir(s.Dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file left behind")

	// Rewriting is idempotent.
	again, err := WriteShim(s.Dir, ShimOptions{Real: fake.Path})
	require.NoError(t, err)
	assert.Equal(t, s, again)
}

func TestWriteShimErrors(t *testing.T) {
	t.Run("empty dir", func(t *testing.T) {
		_, err := WriteShim("", ShimOptions{Real: "/usr/bin/true"})
		assert.Error(t, err)
	})
	t.Run("no real bd on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := WriteShim(t.TempDir(), ShimOptions{})
		require.Error(t, err)
		assert.True(t, proc.IsNotFound(err), err)
	})
	t.Run("refuses to chain shims", func(t *testing.T) {
		s, _ := shimInstall(t)
		_, err := WriteShim(t.TempDir(), ShimOptions{Real: s.Path})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "itself a read-only shim")
	})
	t.Run("directory is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o644))
		_, err := WriteShim(file, ShimOptions{Real: "/usr/bin/true"})
		assert.Error(t, err)
	})
}

func TestRealBDSkipsShims(t *testing.T) {
	s, fake := shimInstall(t)
	// A shim already first on PATH (a nested sc, or this test) must not
	// shadow the real binary for the orchestrator itself.
	t.Setenv("PATH", s.PathEnv())
	real, err := RealBD()
	require.NoError(t, err)
	assert.Equal(t, fake.Path, real)

	// And proc.LookPath (what agents effectively do) finds the shim first.
	found, err := proc.LookPath("bd")
	require.NoError(t, err)
	assert.Equal(t, s.Path, found)

	t.Setenv("PATH", s.Dir)
	_, err = RealBD()
	require.Error(t, err)
	assert.True(t, proc.IsNotFound(err), err)
}

func TestShimPrependsReadonlyAndPassesEverythingThrough(t *testing.T) {
	s, fake := shimInstall(t)
	dir := t.TempDir()
	env := []string{"BEADS_ACTOR=sc:engineer-1", "BD_JSON_ENVELOPE=1"}

	out, errOut, code := shimRun(t, s, dir, env, "-C", "/repo", "ready", "--json", "-n", "0", "--parent", "sc-1", "it's odd")
	assert.Equal(t, 0, code, errOut)
	assert.Equal(t, "--readonly\n-C\n/repo\nready\n--json\n-n\n0\n--parent\nsc-1\nit's odd\n", out)

	calls := fake.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"--readonly", "-C", "/repo", "ready", "--json", "-n", "0", "--parent", "sc-1", "it's odd"}, calls[0].Args)
	assert.Equal(t, dir, calls[0].Dir, "runs in the agent's cwd")

	// No arguments at all still reaches bd.
	out, _, code = shimRun(t, s, dir, env)
	assert.Equal(t, 0, code)
	assert.Equal(t, "--readonly\n", out)
}

func TestShimRelaysBDRefusal(t *testing.T) {
	s, fake := shimInstall(t)
	_, errOut, code := shimRun(t, s, t.TempDir(), nil, "update", "sc-1", "-s", "closed")
	assert.Equal(t, 1, code)
	assert.Equal(t, "Error: "+ReadOnlyMessage("update")+"\n", errOut)
	assert.Len(t, fake.Invocations(t), 1, "bd itself refused")
	assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
}

func TestShimDeniesCommandsBDDoesNotBlock(t *testing.T) {
	s, fake := shimInstall(t)
	cases := []struct {
		name string
		args []string
		op   string
	}{
		{"config set", []string{"config", "set", "custom.x", "y"}, "config set"},
		{"config unset", []string{"config", "unset", "custom.x"}, "config unset"},
		{"config set after global flags", []string{"-C", "/repo", "--actor", "bob", "--json", "config", "set", "a", "b"}, "config set"},
		{"config set with --directory", []string{"--directory", "/repo", "config", "set", "a", "b"}, "config set"},
		{"import", []string{"--json", "import", "-i", "issues.jsonl"}, "import"},
		{"init", []string{"init", "-p", "x", "--non-interactive"}, "init"},
		{"metrics", []string{"metrics", "off"}, "metrics"},
		{"dolt", []string{"--db", "/db", "dolt", "commit", "-m", "x"}, "dolt"},
		{"hooks", []string{"hooks", "install"}, "hooks"},
		{"setup", []string{"setup", "claude"}, "setup"},
		{"rename", []string{"rename", "sc-1", "sc-2"}, "rename"},
		{"restore", []string{"restore", "sc-1"}, "restore"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.Reset(t)
			out, errOut, code := shimRun(t, s, t.TempDir(), nil, tc.args...)
			assert.Equal(t, 1, code)
			assert.Empty(t, out)
			assert.Equal(t, "Error: "+ReadOnlyMessage(tc.op)+"\n", errOut)
			assert.Empty(t, fake.Invocations(t), "bd never runs")
			assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
		})
	}
}

func TestShimAllowsReadsOfDeniedCommandFamilies(t *testing.T) {
	s, fake := shimInstall(t)
	cases := [][]string{
		{"config", "get", "status.custom"},
		{"config", "list"},
		{"-C", "/repo", "config", "get", "x"},
		{"--actor", "config", "get", "x"}, // the flag value is skipped, not read as the command
		{"show", "sc-1", "--json", "--include-comments"},
		{"list", "--json", "-n", "0", "--flat", "--all"},
		{"blocked", "--json"},
		{"dep", "list", "sc-1", "--json"},
		{"comments", "sc-1", "--json"},
		{"version", "--json"},
		{"--version"},
		{"-C", "init"}, // "init" is the -C value, not a command
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fake.Reset(t)
			_, errOut, code := shimRun(t, s, t.TempDir(), nil, args...)
			assert.Equal(t, 0, code, errOut)
			calls := fake.Invocations(t)
			require.Len(t, calls, 1)
			assert.Equal(t, append([]string{"--readonly"}, args...), calls[0].Args)
		})
	}
}

func TestShimScriptQuotesRealPath(t *testing.T) {
	real := "/opt/it's here/bd"
	script := ShimScript(real)
	assert.Contains(t, script, `exec '/opt/it'\''s here/bd' --readonly "$@"`)
	for _, d := range ShimDenied {
		assert.Contains(t, script, "'"+d+"'")
	}
	// The denied list and the message are what bd prints itself.
	assert.Equal(t, "operation 'config set' is not allowed in read-only mode", ReadOnlyMessage("config set"))
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	assert.Equal(t, "/a", PrependPath("/a", ""))
	assert.Equal(t, "/a"+sep+"/b"+sep+"/c", PrependPath("/a", "/b"+sep+"/c"))
	assert.Equal(t, "/a"+sep+"/b", PrependPath("/a", "/b"+sep+"/a"), "earlier occurrence removed")
	assert.Equal(t, "/a"+sep+"/b", PrependPath("/a", "/a"+sep+"/b"), "idempotent")
	assert.Equal(t, "/a"+sep+"/b", PrependPath("/a", sep+"/b"+sep), "empty entries dropped")
}

func TestShimPathEnvAndApply(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/bin")
	s := Shim{Dir: "/state/wt/sc-1/bin", Path: "/state/wt/sc-1/bin/bd", Real: "/usr/bin/bd"}
	assert.Equal(t, s.Dir+sep+"/usr/bin"+sep+"/bin", s.PathEnv())

	env := map[string]string{"HOME": "/h"}
	s.Apply(env)
	assert.Equal(t, s.PathEnv(), env["PATH"])
	assert.Equal(t, "/h", env["HOME"])

	env = map[string]string{"PATH": "/custom"}
	s.Apply(env)
	assert.Equal(t, s.Dir+sep+"/custom", env["PATH"], "prepends to a PATH already in the map")
	s.Apply(env)
	assert.Equal(t, s.Dir+sep+"/custom", env["PATH"], "idempotent")

	s.Apply(nil) // no panic
}

func TestShimVerify(t *testing.T) {
	t.Run("passes with a refusing bd", func(t *testing.T) {
		s, fake := shimInstall(t)
		dir := t.TempDir()
		require.NoError(t, s.Verify(context.Background(), dir))
		calls := fake.Invocations(t)
		require.Len(t, calls, 2)
		assert.Equal(t, []string{"--readonly", "version", "--json"}, calls[0].Args)
		assert.Equal(t, []string{"--readonly", "update", "sc-shim-probe", "-s", "open"}, calls[1].Args)
		assert.Equal(t, dir, calls[1].Dir)
	})
	t.Run("fails when bd accepts the write", func(t *testing.T) {
		testutil.FakeBin(t, "bd", `printf '{"schema_version":1,"data":{"version":"1.2.2"}}\n'`)
		s, err := WriteShim(t.TempDir(), ShimOptions{})
		require.NoError(t, err)
		err = s.Verify(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "was not refused")
	})
	t.Run("fails when the refusal is something else", func(t *testing.T) {
		testutil.FakeBin(t, "bd", `case "$*" in *version*) printf '{"schema_version":1,"data":{"version":"1.2.2"}}\n';; *) echo "Error: no beads project found" >&2; exit 1;; esac`)
		s, err := WriteShim(t.TempDir(), ShimOptions{})
		require.NoError(t, err)
		err = s.Verify(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "another reason")
		assert.Contains(t, err.Error(), "no beads project found")
	})
	t.Run("fails when bd prints no version", func(t *testing.T) {
		testutil.FakeBin(t, "bd", `echo nope`)
		s, err := WriteShim(t.TempDir(), ShimOptions{})
		require.NoError(t, err)
		err = s.Verify(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no version")
	})
	t.Run("fails when the real binary is gone", func(t *testing.T) {
		s, fake := shimInstall(t)
		require.NoError(t, os.Remove(fake.Path))
		err := s.Verify(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot run bd")
	})
	t.Run("honours context cancellation", func(t *testing.T) {
		s, _ := shimInstall(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.Verify(ctx, t.TempDir())
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "cannot run bd"), err)
	})
}

func TestShimPromptNote(t *testing.T) {
	assert.Contains(t, PromptNote, "never write")
	assert.Contains(t, PromptNote, "read-only")
	assert.Contains(t, PromptNote, "bd ready")
	assert.False(t, strings.Contains(PromptNote, "\n"), "one line")
}
