package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// cartridgeTestEnv points every XDG/HOME path at a fresh temp dir so the
// command never touches the real global config, and returns that dir.
func cartridgeTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

// cartridgeRun executes `sc cartridge <args>` and returns combined output.
func cartridgeRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return rootRun(t, append([]string{"cartridge"}, args...)...)
}

// cartridgeFile reads the global config file written by the command.
func cartridgeFile(t *testing.T) (*config.File, bool) {
	t.Helper()
	f, exists, err := config.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	return f, exists
}

// cartridgeAllowMock temporarily makes "mock" a known cartridge name so
// promotion and switching can be tested while only "claude" ships (D-19).
func cartridgeAllowMock(t *testing.T) {
	t.Helper()
	saved := config.KnownCartridges
	config.KnownCartridges = append([]string{}, saved...)
	config.KnownCartridges = append(config.KnownCartridges, "mock")
	t.Cleanup(func() { config.KnownCartridges = saved })
}

func TestCartridgeHelpGolden(t *testing.T) {
	out, err := cartridgeRun(t, "--help")
	require.NoError(t, err)
	rootGolden(t, "cartridge-help", out)
}

func TestCartridgeREADMEAddAndRemove(t *testing.T) {
	cartridgeTestEnv(t)

	// sc cartridge --add claude --model fable
	out, err := cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	require.Equal(t, "added cartridge \"claude\" (model fable); it is now the active cartridge\n", out)

	f, exists := cartridgeFile(t)
	require.True(t, exists)
	require.NotNil(t, f.ActiveCartridge)
	require.Equal(t, "claude", *f.ActiveCartridge)
	require.Equal(t, config.Cartridge{Model: "fable"}, f.Cartridges["claude"])
	info, err := os.Stat(config.GlobalPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The written file loads as an effective config with claude active.
	eff, err := config.Load("")
	require.NoError(t, err)
	name, cart, ok := eff.Active()
	require.True(t, ok)
	require.Equal(t, "claude", name)
	require.Equal(t, "fable", cart.Model)

	// sc cartridge --remove claude
	out, err = cartridgeRun(t, "--remove", "claude")
	require.NoError(t, err)
	require.Equal(t, "removed cartridge \"claude\"\n"+
		"warning: no cartridge is active; add one with: "+CartridgeAddHint+"\n", out)

	f, exists = cartridgeFile(t)
	require.True(t, exists)
	require.Empty(t, f.Cartridges)
	require.NotNil(t, f.ActiveCartridge)
	require.Equal(t, "", *f.ActiveCartridge)

	eff, err = config.Load("")
	require.NoError(t, err)
	_, _, ok = eff.Active()
	require.False(t, ok)
}

func TestCartridgeRejectsUnsupportedNames(t *testing.T) {
	cartridgeTestEnv(t)
	cases := []struct {
		args []string
		name string
	}{
		{[]string{"--add", "kimi", "--model", "3"}, "kimi"},
		{[]string{"--add", "animus"}, "animus"},
		{[]string{"--use", "kimi"}, "kimi"},
	}
	for _, tc := range cases {
		out, err := cartridgeRun(t, tc.args...)
		require.Error(t, err, tc.args)
		require.Equal(t, `cartridge "`+tc.name+`" is not supported yet (only "claude", "cursor" for now)`, err.Error(), tc.args)
		require.Empty(t, out, tc.args)
		_, exists := cartridgeFile(t)
		require.False(t, exists, "a rejected command must not write the config")
	}
}

func TestCartridgeUnsupportedNameExitCode(t *testing.T) {
	cartridgeTestEnv(t)
	var stdout, stderr bytes.Buffer
	code := ExecuteContext(context.Background(), []string{"cartridge", "--add", "kimi", "--model", "3"}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "sc: cartridge \"kimi\" is not supported yet (only \"claude\", \"cursor\" for now)\n", stderr.String())
}

func TestCartridgeAddWithoutModelAndReAdd(t *testing.T) {
	cartridgeTestEnv(t)

	out, err := cartridgeRun(t, "--add", "claude")
	require.NoError(t, err)
	require.Equal(t, "added cartridge \"claude\" (default model); it is now the active cartridge\n", out)

	out, err = cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	require.Equal(t, "updated cartridge \"claude\" (model fable)\n", out)
	f, _ := cartridgeFile(t)
	require.Equal(t, "fable", f.Cartridges["claude"].Model)

	// Re-adding without --model keeps the configured model.
	out, err = cartridgeRun(t, "--add", "claude")
	require.NoError(t, err)
	require.Equal(t, "updated cartridge \"claude\" (model fable)\n", out)
	f, _ = cartridgeFile(t)
	require.Equal(t, "fable", f.Cartridges["claude"].Model)
	require.Equal(t, "claude", *f.ActiveCartridge)
}

func TestCartridgeAddKeepsOtherSettings(t *testing.T) {
	cartridgeTestEnv(t)
	ammo := 12
	require.NoError(t, config.SaveGlobal(config.File{Ammo: &ammo}))

	_, err := cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	f, _ := cartridgeFile(t)
	require.NotNil(t, f.Ammo)
	require.Equal(t, 12, *f.Ammo)
	require.Equal(t, config.SchemaVersion, f.SchemaVersion)
}

func TestCartridgeRemoveErrors(t *testing.T) {
	cartridgeTestEnv(t)
	_, err := cartridgeRun(t, "--remove", "claude")
	require.Error(t, err)
	require.Equal(t, `cartridge "claude" is not configured`, err.Error())
	_, exists := cartridgeFile(t)
	require.False(t, exists)

	_, err = cartridgeRun(t, "--remove", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be empty")
}

func TestCartridgeRemovePromotesNext(t *testing.T) {
	cartridgeTestEnv(t)
	cartridgeAllowMock(t)

	_, err := cartridgeRun(t, "--add", "mock", "--model", "m1")
	require.NoError(t, err)
	out, err := cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	require.Equal(t, "added cartridge \"claude\" (model fable)\n", out, "second cartridge must not steal active")

	out, err = cartridgeRun(t, "--remove", "mock")
	require.NoError(t, err)
	require.Equal(t, "removed cartridge \"mock\"; \"claude\" is now the active cartridge\n", out)
	f, _ := cartridgeFile(t)
	require.Equal(t, "claude", *f.ActiveCartridge)
	require.NotContains(t, f.Cartridges, "mock")

	// Removing a non-active cartridge says nothing about activation.
	_, err = cartridgeRun(t, "--add", "mock")
	require.NoError(t, err)
	out, err = cartridgeRun(t, "--remove", "mock")
	require.NoError(t, err)
	require.Equal(t, "removed cartridge \"mock\"\n", out)
}

func TestCartridgeUse(t *testing.T) {
	cartridgeTestEnv(t)
	cartridgeAllowMock(t)

	_, err := cartridgeRun(t, "--use", "claude")
	require.Error(t, err)
	require.Equal(t, `cartridge "claude" is not configured; add it with: sc cartridge --add claude`, err.Error())

	_, err = cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	_, err = cartridgeRun(t, "--add", "mock")
	require.NoError(t, err)

	out, err := cartridgeRun(t, "--use", "mock")
	require.NoError(t, err)
	require.Equal(t, "active cartridge is now \"mock\"\n", out)
	f, _ := cartridgeFile(t)
	require.Equal(t, "mock", *f.ActiveCartridge)

	out, err = cartridgeRun(t, "--use", "claude")
	require.NoError(t, err)
	require.Equal(t, "active cartridge is now \"claude\"\n", out)
}

func TestCartridgeList(t *testing.T) {
	cartridgeTestEnv(t)
	cartridgeAllowMock(t)

	for _, args := range [][]string{{"--list"}, {}} {
		out, err := cartridgeRun(t, args...)
		require.NoError(t, err)
		require.Equal(t, "no cartridges configured; add one with: "+CartridgeAddHint+"\n", out)
	}

	_, err := cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	_, err = cartridgeRun(t, "--add", "mock")
	require.NoError(t, err)

	out, err := cartridgeRun(t, "--list")
	require.NoError(t, err)
	require.Equal(t, "NAME    MODEL  ACTIVE\nclaude  fable  yes\nmock           \n", out)

	// With nothing active the list ends with a hint.
	_, err = cartridgeRun(t, "--remove", "mock")
	require.NoError(t, err)
	_, err = cartridgeRun(t, "--remove", "claude")
	require.NoError(t, err)
	_, err = cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	f, _ := cartridgeFile(t)
	empty := ""
	f.ActiveCartridge = &empty
	require.NoError(t, config.SaveGlobal(*f))
	out, err = cartridgeRun(t)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(out, "no cartridge is active; pick one with: sc cartridge --use NAME\n"), out)
}

func TestCartridgeJSON(t *testing.T) {
	cartridgeTestEnv(t)

	decode := func(out string) CartridgeStatus {
		var st CartridgeStatus
		require.NoError(t, json.Unmarshal([]byte(out), &st))
		return st
	}

	out, err := rootRun(t, "--json", "cartridge")
	require.NoError(t, err)
	require.Equal(t, CartridgeStatus{Path: config.GlobalPath(), Cartridges: []CartridgeInfo{}}, decode(out))
	require.Contains(t, out, `"cartridges": []`)
	_, exists := cartridgeFile(t)
	require.False(t, exists, "listing must not create the config")

	out, err = rootRun(t, "--json", "cartridge", "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	require.Equal(t, CartridgeStatus{
		Path:       config.GlobalPath(),
		Active:     "claude",
		Cartridges: []CartridgeInfo{{Name: "claude", Model: "fable", Active: true}},
	}, decode(out))
	require.True(t, strings.HasSuffix(out, "}\n"))

	out, err = rootRun(t, "--json", "cartridge", "--remove", "claude")
	require.NoError(t, err)
	require.Equal(t, CartridgeStatus{
		Path:       config.GlobalPath(),
		Cartridges: []CartridgeInfo{},
		Warning:    "no cartridge is active; add one with: " + CartridgeAddHint,
	}, decode(out))
	require.NotContains(t, out, "warning:", "JSON output carries the warning as a field only")
}

func TestCartridgeFlagConflicts(t *testing.T) {
	cartridgeTestEnv(t)

	_, err := cartridgeRun(t, "--add", "claude", "--remove", "claude")
	require.Error(t, err)
	require.Contains(t, err.Error(), "none of the others can be")

	_, err = cartridgeRun(t, "--list", "--use", "claude")
	require.Error(t, err)

	_, err = cartridgeRun(t, "--model", "fable")
	require.Error(t, err)
	require.Equal(t, "--model requires --add NAME", err.Error())

	_, err = cartridgeRun(t, "claude")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown command "claude"`)

	_, exists := cartridgeFile(t)
	require.False(t, exists)
}

func TestCartridgeIgnoresRepoScope(t *testing.T) {
	dir := cartridgeTestEnv(t)
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(repo+"/.git", 0o755))

	// Inside a git repo the cartridge still goes to the global file (D-03).
	_, err := rootRun(t, "--repo", repo, "cartridge", "--add", "claude", "--model", "fable")
	require.NoError(t, err)
	_, exists := cartridgeFile(t)
	require.True(t, exists)
	_, err = os.Stat(config.RepoConfigPath(repo))
	require.True(t, os.IsNotExist(err), "repo config must not be created")
	require.True(t, strings.HasPrefix(config.GlobalPath(), dir))
}

func TestCartridgeMalformedGlobalConfig(t *testing.T) {
	cartridgeTestEnv(t)
	path := config.GlobalPath()
	require.NoError(t, config.WriteAtomic(path, []byte("schema_version = 1\nammo = [\n"), 0o600))

	_, err := cartridgeRun(t, "--add", "claude", "--model", "fable")
	require.Error(t, err)
	var perr *config.ParseError
	require.ErrorAs(t, err, &perr)
	require.Equal(t, path, perr.Path)
}
