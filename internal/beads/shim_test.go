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

// shimOverridableFakeScript is a fake bd that behaves the way bd 1.2.2
// really does with its --readonly flag: pflag lets the last occurrence
// win, so a `--readonly=false` (or =0, =F) anywhere in the arguments
// switches read-only mode back off and the write succeeds.
const shimOverridableFakeScript = `
ro=0
for a in "$@"; do
  case "$a" in
    --readonly|--readonly=true|--readonly=1) ro=1 ;;
    --readonly=*) ro=0 ;;
    version) printf '{"schema_version":1,"data":{"version":"1.2.2","build":"dev"}}\n'; exit 0 ;;
  esac
done
for a in "$@"; do
  case "$a" in
    update|create|comment|close)
      if [ "$ro" = 1 ]; then echo "Error: operation '$a' is not allowed in read-only mode" >&2; exit 1; fi
      echo "wrote $a"; exit 0 ;;
  esac
done
printf '%s\n' "$@"
`

// shimWeakMode names the flawed shims TestShimVerifyCatchesWeakerShims
// builds, one per gap the P3-09 reviews found.
type shimWeakMode int

const (
	// shimWeakReadonlyOnly leaves everything to bd's --readonly.
	shimWeakReadonlyOnly shimWeakMode = iota
	// shimWeakDenyList is the round-2 shim: the deny-list that missed
	// `config set-many`, `backup`, `federation` and `audit`.
	shimWeakDenyList
	// shimWeakNoOverride is today's allow-list shim with the --readonly
	// override guard cut out.
	shimWeakNoOverride
	// shimWeakRefusesReads denies every command except `version`, the
	// shape an over-broad allow-list edit would produce.
	shimWeakRefusesReads
)

// shimWeakScript writes a flawed shim of the given mode in front of real,
// so Shim.Verify can be shown to reject it.
func shimWeakScript(t *testing.T, real string, mode shimWeakMode) Shim {
	t.Helper()
	body := "#!/bin/sh\n" + ShimHeader + "\n"
	switch mode {
	case shimWeakReadonlyOnly:
	case shimWeakDenyList:
		body += "case \"$1\" in branch|vc|dolt|hooks|import|init|metrics|rename|restore|setup)" +
			" echo \"Error: operation '$1' is not allowed in read-only mode\" >&2; exit 1 ;; esac\n" +
			"case \"$1 $2\" in 'config set'|'config unset')" +
			" echo \"Error: operation '$1 $2' is not allowed in read-only mode\" >&2; exit 1 ;; esac\n"
	case shimWeakRefusesReads:
		body += "case \"$1\" in version) ;; *)" +
			" echo \"Error: operation '$1' is not allowed in read-only mode\" >&2; exit 1 ;; esac\n"
	case shimWeakNoOverride:
		body = shimScriptWithoutOverrideGuard(t, real)
	}
	if mode != shimWeakNoOverride {
		body += "exec '" + real + "' --readonly \"$@\"\n"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bd")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return Shim{Dir: dir, Path: path, Real: real}
}

// shimScriptWithoutOverrideGuard returns the generated shim with the
// --readonly refusal block removed, leaving the allow-list intact.
func shimScriptWithoutOverrideGuard(t *testing.T, real string) string {
	t.Helper()
	script := ShimScript(real)
	start := strings.Index(script, "if [ -n \"$_sc_bad\"")
	require.GreaterOrEqual(t, start, 0, "the override guard moved")
	end := strings.Index(script[start:], "\nfi\n")
	require.GreaterOrEqual(t, end, 0, "the override guard moved")
	return script[:start] + script[start+end+len("\nfi\n"):]
}

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

// TestShimRefusesTheWriteItself pins the message an agent sees for the
// write it is most likely to try: identical to bd's own refusal (the
// conformance check and beads.KindOf both key on it), but produced by the
// shim, so bd never runs. bd's --readonly stays the second line of defence,
// checked by Shim.Verify.
func TestShimRefusesTheWriteItself(t *testing.T) {
	s, fake := shimInstall(t)
	out, errOut, code := shimRun(t, s, t.TempDir(), nil, "update", "sc-1", "-s", "closed")
	assert.Equal(t, 1, code)
	assert.Empty(t, out)
	assert.Equal(t, "Error: "+ReadOnlyMessage("update"), strings.SplitN(errOut, "\n", 2)[0])
	assert.Contains(t, errOut, ShimDeniedHint)
	assert.Empty(t, fake.Invocations(t), "bd never runs")
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
		// bd 1.2.2 runs these under --readonly (P3-09 review, verified
		// live): `bd branch <name>` creates a Dolt branch and `bd vc
		// commit`/`bd vc merge` commit and merge one, all exiting 0.
		{"branch create", []string{"branch", "sc-evil"}, "branch"},
		{"branch list", []string{"branch"}, "branch"},
		{"branch after global flags", []string{"-C", "/repo", "--json", "branch", "sc-evil"}, "branch"},
		{"vc commit", []string{"vc", "commit", "-m", "sneaky"}, "vc"},
		{"vc merge", []string{"vc", "merge", "sc-evil"}, "vc"},
		{"vc status", []string{"vc", "status"}, "vc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.Reset(t)
			out, errOut, code := shimRun(t, s, t.TempDir(), nil, tc.args...)
			assert.Equal(t, 1, code)
			assert.Empty(t, out)
			lines := strings.Split(strings.TrimRight(errOut, "\n"), "\n")
			require.Len(t, lines, 2)
			assert.Equal(t, "Error: "+ReadOnlyMessage(tc.op), lines[0])
			assert.Equal(t, ShimDeniedHint, lines[1])
			assert.Equal(t, ReadOnlyMessage(tc.op), StderrMessage(errOut), "the hint does not confuse the parser")
			assert.Empty(t, fake.Invocations(t), "bd never runs")
			assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
		})
	}
}

// TestShimAllowsOnlyReadCommands covers the P3-09 review's major gap: a
// deny-list only closed the bd families someone had checked, so
// `bd config set-many status.custom=pwned:wip` still rewrote the D-07
// status registry through the shim, `bd backup init`/`bd backup sync`
// pushed the whole database off-machine, `bd federation add-peer`
// persisted a peer and `bd audit record` appended to
// `.beads/interactions.jsonl` — all reproduced live. ShimAllowed is an
// allow-list, so every command that is not a read is refused, whether or
// not anyone thought of it.
func TestShimAllowsOnlyReadCommands(t *testing.T) {
	s, fake := shimInstall(t)
	cases := []struct {
		args []string
		op   string
	}{
		// The commands the review drove through the old deny-list.
		{[]string{"config", "set-many", "status.custom=pwned:wip"}, "config set-many"},
		{[]string{"config", "apply"}, "config apply"},
		{[]string{"backup", "init", "file:///tmp/x"}, "backup"},
		{[]string{"backup", "sync"}, "backup"},
		{[]string{"federation", "add-peer", "evil", "http://evil.example"}, "federation"},
		{[]string{"audit", "record", "--kind", "label"}, "audit"},
		// Issue-level writes bd blocks itself; the shim gets there first.
		{[]string{"update", "sc-1", "-s", "closed"}, "update"},
		{[]string{"close", "sc-1"}, "close"},
		{[]string{"create", "sneaky", "-t", "task"}, "create"},
		{[]string{"comment", "sc-1", "hi"}, "comment"},
		{[]string{"comments", "add", "sc-1", "hi"}, "comments add"},
		{[]string{"label", "add", "sc-1", "x"}, "label"},
		{[]string{"dep", "add", "sc-1", "sc-2"}, "dep add"},
		{[]string{"dep", "remove", "sc-1", "sc-2"}, "dep remove"},
		{[]string{"kv", "set", "k", "v"}, "kv"},
		{[]string{"note", "sc-1", "text"}, "note"},
		{[]string{"batch"}, "batch"},
		// Maintenance and integrations an agent has no business running.
		{[]string{"sql", "select 1"}, "sql"},
		{[]string{"admin", "reset"}, "admin"},
		{[]string{"gc"}, "gc"},
		{[]string{"prune"}, "prune"},
		{[]string{"purge"}, "purge"},
		{[]string{"compact"}, "compact"},
		{[]string{"flatten"}, "flatten"},
		{[]string{"migrate", "schema"}, "migrate"},
		{[]string{"worktree", "create", "sc-1"}, "worktree"},
		{[]string{"merge-slot", "acquire"}, "merge-slot"},
		{[]string{"swarm", "create"}, "swarm"},
		{[]string{"rename-prefix", "sc"}, "rename-prefix"},
		{[]string{"doctor", "--fix"}, "doctor"},
		{[]string{"bootstrap"}, "bootstrap"},
		{[]string{"upgrade"}, "upgrade"},
		{[]string{"remember", "x"}, "remember"},
		{[]string{"forget", "x"}, "forget"},
		{[]string{"jira", "sync"}, "jira"},
		{[]string{"github", "sync"}, "github"},
		{[]string{"ship", "x"}, "ship"},
		{[]string{"mol", "wisp"}, "mol"},
		{[]string{"rules", "compact"}, "rules"},
		// A verb no one has heard of yet is refused by default.
		{[]string{"brand-new-verb", "--force"}, "brand-new-verb"},
		{[]string{"-C", "/repo", "--json", "brand-new-verb"}, "brand-new-verb"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			fake.Reset(t)
			out, errOut, code := shimRun(t, s, t.TempDir(), nil, tc.args...)
			assert.Equal(t, 1, code)
			assert.Empty(t, out)
			assert.Equal(t, ReadOnlyMessage(tc.op), StderrMessage(errOut))
			assert.Contains(t, errOut, ShimDeniedHint)
			assert.Empty(t, fake.Invocations(t), "bd never runs")
			assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
		})
	}
}

// TestShimRefusesReadonlyOverride covers the P3-09 review's first gap:
// bd's --readonly is an ordinary pflag bool, so a later `--readonly=false`
// (or =0, =F), anywhere in the argument list, wins over the flag the shim
// passes and re-enables every write (verified live against bd 1.2.2:
// update, comment and create all succeeded). The shim must refuse such an
// argument before bd runs at all.
func TestShimRefusesReadonlyOverride(t *testing.T) {
	s, fake := shimInstall(t)
	cases := []struct {
		name string
		args []string
		bad  string
	}{
		{"before the command", []string{"--readonly=false", "update", "sc-1", "-s", "closed"}, "--readonly=false"},
		{"after the command", []string{"update", "sc-1", "--readonly=false", "-s", "closed"}, "--readonly=false"},
		{"last argument", []string{"comment", "sc-1", "hi", "--readonly=0"}, "--readonly=0"},
		{"single letter", []string{"create", "sneaky", "--readonly=F"}, "--readonly=F"},
		{"turning it on again", []string{"ready", "--readonly=true"}, "--readonly=true"},
		{"bare flag", []string{"--readonly", "ready"}, "--readonly"},
		{"bare flag with a value", []string{"update", "sc-1", "--readonly", "false"}, "--readonly"},
		{"first of several", []string{"--readonly=false", "update", "--readonly=0", "sc-1"}, "--readonly=false"},
		{"read command too", []string{"list", "--readonly=false", "--json"}, "--readonly=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.Reset(t)
			out, errOut, code := shimRun(t, s, t.TempDir(), nil, tc.args...)
			assert.Equal(t, 1, code)
			assert.Empty(t, out)
			assert.Empty(t, fake.Invocations(t), "bd never runs")
			lines := strings.Split(strings.TrimRight(errOut, "\n"), "\n")
			require.Len(t, lines, 2)
			assert.Equal(t, "Error: "+ReadOnlyMessage(tc.bad), lines[0])
			assert.Equal(t, ShimOverrideHint, lines[1])
			assert.Equal(t, ReadOnlyMessage(tc.bad), StderrMessage(errOut), "the hint does not confuse the parser")
			assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
		})
	}
}

// TestShimPassesInertReadonlyLookalikes proves the guard does not fire on
// arguments bd itself would not read as the flag: everything after a bare
// "--" is positional for cobra/pflag, and the value of a global
// value-taking flag belongs to that flag (verified live: `bd -C
// --readonly=false update …` fails with `cannot use -C directory
// "--readonly=false"` and writes nothing).
func TestShimPassesInertReadonlyLookalikes(t *testing.T) {
	s, fake := shimInstall(t)
	cases := [][]string{
		{"search", "--", "--readonly=false"},
		{"show", "sc-1", "--", "--readonly"},
		{"-C", "--readonly=false", "ready"},
		{"--actor", "--readonly=false", "list", "--json"},
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

// TestShimIgnoresCommandsAfterDoubleDash mirrors cobra's own stripFlags:
// a word after "--" never names a command, so it must not be denied
// (verified live: `bd -- config set custom.x 1` printed the root help and
// wrote nothing).
func TestShimIgnoresCommandsAfterDoubleDash(t *testing.T) {
	s, fake := shimInstall(t)
	_, errOut, code := shimRun(t, s, t.TempDir(), nil, "--", "config", "set", "a", "b")
	assert.Equal(t, 0, code, errOut)
	calls := fake.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"--readonly", "--", "config", "set", "a", "b"}, calls[0].Args)
}

func TestShimAllowsReadCommands(t *testing.T) {
	s, fake := shimInstall(t)
	cases := [][]string{
		{"ready", "--json", "-n", "0"},
		{"show", "sc-1", "--json", "--include-comments"},
		{"list", "--json", "-n", "0", "--flat", "--all"},
		{"blocked", "--json"},
		{"dep", "list", "sc-1", "--json"},
		{"dep", "tree", "sc-1"},
		{"dep", "cycles"},
		{"comments", "sc-1", "--json"},
		{"children", "sc-1"},
		{"count", "--status", "open"},
		{"export", "--json"},
		{"graph", "--all"},
		{"history", "sc-1"},
		{"query", "status=open"},
		{"search", "mean"},
		{"stale"},
		{"statuses"},
		{"types"},
		{"config", "get", "status.custom"},
		{"config", "list"},
		{"config", "show"},
		{"-C", "/repo", "config", "get", "x"},
		{"--actor", "sc:engineer-1", "config", "get", "x"},
		{"version", "--json"},
		{"help", "ready"},
		{"--version"},
		{"--help"},
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
	for _, a := range ShimAllowed {
		assert.Contains(t, script, "'"+a+"'")
	}
	// The message is what bd prints itself.
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
		require.Len(t, calls, 4)
		assert.Equal(t, []string{"--readonly", "version", "--json"}, calls[0].Args)
		assert.Equal(t, []string{"--readonly", "ready", "--json", "-n", "0"}, calls[1].Args, "reads must work")
		assert.Equal(t, []string{"--readonly", "show", "sc-shim-probe", "--json"}, calls[2].Args)
		assert.Equal(t, []string{"--readonly", "update", "sc-shim-probe", "-s", "open"}, calls[3].Args,
			"bd itself is checked directly, because the shim refuses the write first")
		for _, c := range calls {
			assert.Equal(t, dir, c.Dir, "runs where the agent would")
		}
	})
	t.Run("fails when bd accepts the write", func(t *testing.T) {
		testutil.FakeBin(t, "bd", `printf '{"schema_version":1,"data":{"version":"1.2.2"}}\n'`)
		s, err := WriteShim(t.TempDir(), ShimOptions{})
		require.NoError(t, err)
		err = s.Verify(context.Background(), t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "was not refused")
		assert.Contains(t, err.Error(), "does not enforce --readonly itself")
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
	t.Run("accepts a workdir with no beads database", func(t *testing.T) {
		// The read probes must not turn bd's own "no beads database found"
		// into a shim failure: Verify checks the shim, not the project.
		testutil.FakeBin(t, "bd", `case "$*" in
  *version*) printf '{"schema_version":1,"data":{"version":"1.2.2"}}\n'; exit 0 ;;
  *update*) echo "Error: operation 'update' is not allowed in read-only mode" >&2; exit 1 ;;
esac
echo "Error: no beads database found" >&2; exit 1`)
		s, err := WriteShim(t.TempDir(), ShimOptions{})
		require.NoError(t, err)
		assert.NoError(t, s.Verify(context.Background(), t.TempDir()))
	})
}

func TestShimPromptNote(t *testing.T) {
	assert.Contains(t, PromptNote, "never write")
	assert.Contains(t, PromptNote, "read-only")
	assert.Contains(t, PromptNote, "bd ready")
	assert.Contains(t, PromptNote, ReadOnlyFlag, "agents are told the override is refused too")
	assert.False(t, strings.Contains(PromptNote, "\n"), "one line")
}

// TestShimBlocksOverrideAgainstAnOverridableBD is the end-to-end version of
// the P3-09 review's first gap: with a bd that honours `--readonly=false`
// the way the real one does, the shim still refuses the write, and
// Shim.Verify says so.
func TestShimBlocksOverrideAgainstAnOverridableBD(t *testing.T) {
	fake := testutil.FakeBin(t, "bd", shimOverridableFakeScript)
	s, err := WriteShim(filepath.Join(t.TempDir(), "bin"), ShimOptions{})
	require.NoError(t, err)

	// The fake really would write without the guard.
	out, err := exec.Command(fake.Path, "--readonly", "--readonly=false", "update", "sc-1").Output()
	require.NoError(t, err)
	require.Equal(t, "wrote update\n", string(out))
	fake.Reset(t)

	_, errOut, code := shimRun(t, s, t.TempDir(), nil, "--readonly=false", "update", "sc-1", "-s", "closed")
	assert.Equal(t, 1, code)
	assert.Equal(t, ReadOnlyMessage("--readonly=false"), StderrMessage(errOut))
	assert.Empty(t, fake.Invocations(t), "bd never runs")

	require.NoError(t, s.Verify(context.Background(), t.TempDir()))
}

// TestShimVerifyCatchesWeakerShims proves the extended self-check rejects
// every shim shape the P3-09 reviews found wanting: one that trusts bd's
// --readonly alone, the round-2 deny-list that let `config set-many`
// through, one without the --readonly override guard, and one so strict it
// refuses the reads agents need.
func TestShimVerifyCatchesWeakerShims(t *testing.T) {
	cases := []struct {
		name string
		mode shimWeakMode
		want []string
	}{
		{"only bd's --readonly", shimWeakReadonlyOnly, []string{"bd config set", "was not refused"}},
		{"an out-of-date deny list", shimWeakDenyList, []string{"bd config set-many", "was not refused"}},
		{"no override guard", shimWeakNoOverride, []string{"--readonly=false", "was refused as"}},
		{"refuses the reads too", shimWeakRefusesReads, []string{"refuses the read", "bd ready"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := testutil.FakeBin(t, "bd", shimOverridableFakeScript)
			s := shimWeakScript(t, fake.Path, tc.mode)
			err := s.Verify(context.Background(), t.TempDir())
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestShimVerifyProbesEveryMechanism pins what Verify checks: the reads an
// agent needs, a refusal for every mechanism (the allow-list, its
// exceptions, and a --readonly override), and bd's own --readonly. Only the
// reads and the bd-side probe start the binary, so the check is cheap and
// cannot change a database.
func TestShimVerifyProbesEveryMechanism(t *testing.T) {
	s, fake := shimInstall(t)
	require.NoError(t, s.Verify(context.Background(), t.TempDir()))
	require.Len(t, fake.Invocations(t), 4, "only the reads and bd's own gate run the binary")

	var ops []string
	for _, p := range shimProbes {
		ops = append(ops, p.op)
	}
	assert.Equal(t, []string{
		"update", "config set", "config set-many", "backup", "federation",
		"audit", "import", "branch", "vc", "comments add", "--readonly=false",
	}, ops)
	for _, p := range shimProbes {
		fake.Reset(t)
		_, errOut, code := shimRun(t, s, t.TempDir(), nil, p.args...)
		assert.Equal(t, 1, code, strings.Join(p.args, " "))
		assert.Equal(t, ReadOnlyMessage(p.op), StderrMessage(errOut), strings.Join(p.args, " "))
		assert.Empty(t, fake.Invocations(t), "the shim refuses without running bd")
	}
	for _, args := range shimReadProbes {
		fake.Reset(t)
		_, errOut, code := shimRun(t, s, t.TempDir(), nil, args...)
		assert.Equal(t, 0, code, errOut)
		assert.Len(t, fake.Invocations(t), 1, strings.Join(args, " "))
	}
}

// TestShimDirFor pins the per-worktree layout: the shim sits beside the
// run's worktrees, never inside one and never outside the run (D-23).
func TestShimDirFor(t *testing.T) {
	wt := filepath.Join("/state", "runs", "r1", WorktreesDirName, "sc-1")
	want := filepath.Join("/state", "runs", "r1", ShimDirName, "sc-1")
	got, err := ShimDirFor(wt)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	got, err = ShimDirFor(wt + string(filepath.Separator))
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Anything but <run-dir>/wt/<bead-id> would put the shim outside the
	// run directory, so it is refused instead: climbing two levels from
	// /home/me/proj lands on /home/bin/proj.
	for _, bad := range []string{
		"",
		filepath.Join("relative", WorktreesDirName, "sc-1"),
		"/home/me/proj",
		filepath.Join("/state", "runs", "r1", "worktrees", "sc-1"),
		"/",
		filepath.Join("/", WorktreesDirName),
	} {
		t.Run("rejects "+bad, func(t *testing.T) {
			dir, err := ShimDirFor(bad)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrShimLayout)
			assert.Empty(t, dir)
		})
	}
}

// TestInstallForWorktree covers the production entry point an orchestrator
// calls once per agent worktree: write the shim beside the worktree, check
// it, and hand back something Apply puts first on the agent's PATH.
func TestInstallForWorktree(t *testing.T) {
	ctx := context.Background()
	sep := string(os.PathListSeparator)

	t.Run("installs, verifies and leaves the worktree clean", func(t *testing.T) {
		fake := testutil.FakeBin(t, "bd", shimFakeScript)
		run := t.TempDir()
		wt := filepath.Join(run, WorktreesDirName, "sc-1")
		require.NoError(t, os.MkdirAll(wt, 0o755))

		s, err := InstallForWorktree(ctx, wt, ShimOptions{})
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(run, ShimDirName, "sc-1"), s.Dir)
		assert.Equal(t, filepath.Join(run, ShimDirName, "sc-1", "bd"), s.Path)
		assert.Equal(t, fake.Path, s.Real)
		assert.True(t, IsShim(s.Path))
		assert.Len(t, fake.Invocations(t), 4, "Verify ran")

		entries, err := os.ReadDir(wt)
		require.NoError(t, err)
		assert.Empty(t, entries, "nothing is written inside the worktree")

		env := map[string]string{"PATH": "/usr/bin"}
		s.Apply(env)
		assert.Equal(t, s.Dir+sep+"/usr/bin", env["PATH"])
	})
	t.Run("honours Dir and SkipVerify", func(t *testing.T) {
		fake := testutil.FakeBin(t, "bd", shimFakeScript)
		dir := filepath.Join(t.TempDir(), "custom")
		s, err := InstallForWorktree(ctx, filepath.Join(t.TempDir(), "elsewhere"), ShimOptions{Dir: dir, SkipVerify: true})
		require.NoError(t, err)
		assert.Equal(t, dir, s.Dir)
		assert.Empty(t, fake.Invocations(t), "no self-check")
	})
	t.Run("rejects a relative or empty worktree", func(t *testing.T) {
		testutil.FakeBin(t, "bd", shimFakeScript)
		_, err := InstallForWorktree(ctx, "", ShimOptions{})
		assert.Error(t, err)
		_, err = InstallForWorktree(ctx, filepath.Join("relative", WorktreesDirName, "sc-1"), ShimOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not absolute")
	})
	t.Run("rejects a path that is not a run worktree", func(t *testing.T) {
		testutil.FakeBin(t, "bd", shimFakeScript)
		home := filepath.Join(t.TempDir(), "me", "proj")
		require.NoError(t, os.MkdirAll(home, 0o755))
		_, err := InstallForWorktree(ctx, home, ShimOptions{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrShimLayout)
		// Nothing was created two levels up, where ShimDirFor's arithmetic
		// would have put the directory.
		assert.NoDirExists(t, filepath.Join(filepath.Dir(filepath.Dir(home)), ShimDirName))
	})
	t.Run("propagates a failed self-check", func(t *testing.T) {
		testutil.FakeBin(t, "bd", `printf '{"schema_version":1,"data":{"version":"1.2.2"}}\n'`)
		run := t.TempDir()
		wt := filepath.Join(run, WorktreesDirName, "sc-1")
		require.NoError(t, os.MkdirAll(wt, 0o755))
		_, err := InstallForWorktree(ctx, wt, ShimOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "was not refused")
	})
}
