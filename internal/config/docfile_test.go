package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenSaveRoundTrip(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)

	g, err := OpenGlobal()
	require.NoError(t, err)
	assert.Equal(t, ScopeGlobal, g.Scope)
	assert.Equal(t, globalPath, g.Path)
	assert.False(t, g.Exists)
	assert.Equal(t, SchemaVersion, g.File.SchemaVersion)

	activated, err := g.AddCartridge("claude", "fable")
	require.NoError(t, err)
	assert.True(t, activated)
	_, err = g.SetAmmo(12)
	require.NoError(t, err)
	g.SetSafety(false)
	require.NoError(t, g.Save())
	assert.True(t, g.Exists)

	info, err := os.Stat(globalPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "global file holds subscriptions")
	dirInfo, err := os.Stat(filepath.Dir(globalPath))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), "# slop-cannon configuration"), string(data))
	assert.Contains(t, string(data), "schema_version = 1\n")
	assert.Contains(t, string(data), "active_cartridge = 'claude'\n")
	assert.Contains(t, string(data), "[cartridges.claude]\nmodel = 'fable'\n")
	assert.NotContains(t, string(data), "vibes", "unset keys are not written")
	assert.NotContains(t, string(data), "[limits]")

	r, err := OpenRepo(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, repoPath, r.Path)
	_, err = r.SetAmmo(4)
	require.NoError(t, err)
	require.NoError(t, r.Save())
	info, err = os.Stat(repoPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "repo file is committable")

	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, 4, eff.Ammo)
	assert.False(t, eff.Safety)
	assert.Equal(t, "claude", eff.ActiveCartridge)
	assert.Equal(t, "fable", eff.Cartridges["claude"].Model)

	again, err := OpenGlobal()
	require.NoError(t, err)
	assert.True(t, again.Exists)
	assert.Equal(t, g.File, again.File)

	_, err = OpenRepo("")
	assert.Error(t, err)
	assert.Error(t, SaveRepo("", File{}))
}

func TestSaveGlobalAndSaveRepo(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	ammo := 5
	require.NoError(t, SaveGlobal(File{Ammo: &ammo}))
	require.NoError(t, SaveRepo(repoRoot, File{Rules: []string{"r.md"}}))
	for _, p := range []string{globalPath, repoPath} {
		f, exists, err := ReadFile(p)
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, SchemaVersion, f.SchemaVersion, "Save stamps the schema version")
	}
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, 5, eff.Ammo)
	assert.Equal(t, []string{filepath.Join(filepath.Dir(repoPath), "r.md")}, eff.Rules)
}

func TestSaveKeepsUnknownFreeRoundTrip(t *testing.T) {
	// Every key the loader understands survives a save/load cycle unchanged.
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, configGlobalTOML)
	before, _, err := ReadFile(globalPath)
	require.NoError(t, err)
	require.NoError(t, SaveGlobal(*before))
	after, _, err := ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = Load(repoRoot)
	require.NoError(t, err)
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.toml")
	big := bytes.Repeat([]byte("ammo = 10\n"), 20_000)
	require.NoError(t, WriteAtomic(path, big, 0o644))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, big, got)

	require.NoError(t, WriteAtomic(path, []byte("ammo = 3\n"), 0o644))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "ammo = 3\n", string(got), "replaces the whole file")

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp files left behind: %v", entries)
	assert.Equal(t, "config.toml", entries[0].Name())

	// When the write cannot complete the old content is untouched and no
	// temp file remains.
	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o755) })
		err = WriteAtomic(path, []byte("ammo = 4\n"), 0o644)
		require.Error(t, err)
		got, err = os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "ammo = 3\n", string(got))
		entries, err = os.ReadDir(filepath.Dir(path))
		require.NoError(t, err)
		assert.Len(t, entries, 1)
	}

	// A directory that cannot be created is an error, not a panic.
	blocker := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	assert.Error(t, WriteAtomic(filepath.Join(blocker, "x", "config.toml"), []byte("x"), 0o644))
}

func TestDocCartridges(t *testing.T) {
	configTestEnv(t)
	g, err := OpenGlobal()
	require.NoError(t, err)

	_, err = g.AddCartridge("kimi", "3")
	assert.EqualError(t, err, `cartridge "kimi" is not supported yet (only "claude" for now)`)
	_, err = g.AddCartridge("animus", "")
	assert.EqualError(t, err, `cartridge "animus" is not supported yet (only "claude" for now)`)
	assert.Nil(t, g.File.Cartridges)

	activated, err := g.AddCartridge("claude", "fable")
	require.NoError(t, err)
	assert.True(t, activated, "first cartridge becomes active")
	require.NotNil(t, g.File.ActiveCartridge)
	assert.Equal(t, "claude", *g.File.ActiveCartridge)

	activated, err = g.AddCartridge("claude", "opus")
	require.NoError(t, err)
	assert.False(t, activated, "re-adding updates the model")
	assert.Equal(t, "opus", g.File.Cartridges["claude"].Model)

	require.NoError(t, g.SetActive("claude"))
	assert.EqualError(t, g.SetActive("kimi"), `cartridge "kimi" is not supported yet (only "claude" for now)`)

	_, _, err = g.RemoveCartridge("kimi")
	assert.EqualError(t, err, `cartridge "kimi" is not configured`)

	promoted, wasActive, err := g.RemoveCartridge("claude")
	require.NoError(t, err)
	assert.True(t, wasActive)
	assert.Equal(t, "", promoted, "nothing left to promote")
	require.NotNil(t, g.File.ActiveCartridge)
	assert.Equal(t, "", *g.File.ActiveCartridge)
	assert.Empty(t, g.File.Cartridges)

	assert.EqualError(t, g.SetActive("claude"), `cartridge "claude" is not configured; add it with: sc cartridge --add claude`)

	activated, err = g.AddCartridge("claude", "fable")
	require.NoError(t, err)
	assert.True(t, activated, "a blank active_cartridge counts as none")
	require.NoError(t, g.Save())
	eff, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "claude", eff.ActiveCartridge)
}

func TestDocRemoveCartridgePromotesNext(t *testing.T) {
	// Promotion picks the next configured name in sorted order. Only one
	// name is valid today, so exercise the rule through the File directly.
	configTestEnv(t)
	g, err := OpenGlobal()
	require.NoError(t, err)
	_, err = g.AddCartridge("claude", "fable")
	require.NoError(t, err)
	g.File.Cartridges["zeta"] = Cartridge{} // not addable via the D-19 check, but a stored file could carry it
	promoted, wasActive, err := g.RemoveCartridge("claude")
	require.NoError(t, err)
	assert.True(t, wasActive)
	assert.Equal(t, "zeta", promoted)
	assert.Equal(t, "zeta", *g.File.ActiveCartridge)

	promoted, wasActive, err = g.RemoveCartridge("zeta")
	require.NoError(t, err)
	assert.True(t, wasActive)
	assert.Equal(t, "", promoted)
}

func TestDocCartridgeHelpersRefuseRepoScope(t *testing.T) {
	repoRoot, _, _ := configTestEnv(t)
	r, err := OpenRepo(repoRoot)
	require.NoError(t, err)
	_, err = r.AddCartridge("claude", "fable")
	assert.ErrorIs(t, err, ErrNotGlobal)
	_, _, err = r.RemoveCartridge("claude")
	assert.ErrorIs(t, err, ErrNotGlobal)
	assert.ErrorIs(t, r.SetActive("claude"), ErrNotGlobal)
}

func TestDocSetAmmoAndSafety(t *testing.T) {
	repoRoot, _, _ := configTestEnv(t)
	r, err := OpenRepo(repoRoot)
	require.NoError(t, err)

	_, err = r.SetAmmo(2)
	assert.ErrorIs(t, err, ErrAmmoTooLow)
	assert.Nil(t, r.File.Ammo, "rejected values are not stored")

	alloc, err := r.SetAmmo(12)
	require.NoError(t, err)
	assert.Equal(t, "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers", alloc.String())
	assert.Equal(t, 12, *r.File.Ammo)

	r.SetSafety(false)
	assert.False(t, *r.File.Safety)
	r.SetSafety(true)
	assert.True(t, *r.File.Safety)
}

func TestDocVibesAndRules(t *testing.T) {
	repoRoot, _, repoPath := configTestEnv(t)
	r, err := OpenRepo(repoRoot)
	require.NoError(t, err)

	inside := filepath.Join(repoRoot, "vibes.md")
	require.NoError(t, os.WriteFile(inside, []byte("# be kind\n"), 0o644))
	outside := filepath.Join(t.TempDir(), "rules.md")
	require.NoError(t, os.WriteFile(outside, []byte("MUST test\n"), 0o644))

	stored, err := r.AddVibes(inside)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("..", "vibes.md"), stored, "inside the repo: relative to the config dir")
	stored, err = r.AddVibes(inside)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("..", "vibes.md"), stored)
	assert.Equal(t, []string{filepath.Join("..", "vibes.md")}, r.File.Vibes, "deduplicated")

	stored, err = r.AddRules(outside)
	require.NoError(t, err)
	assert.Equal(t, outside, stored, "outside the config dir: absolute")
	assert.Equal(t, []string{inside}, r.VibesPaths())
	assert.Equal(t, []string{outside}, r.RulesPaths())

	require.NoError(t, r.Save())
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{inside}, eff.Vibes)
	assert.Equal(t, []string{outside}, eff.Rules)

	err = r.RemoveVibes(filepath.Join(repoRoot, "nope.md"))
	assert.EqualError(t, err, filepath.Join(repoRoot, "nope.md")+" is not in vibes ("+repoPath+"); nothing removed")
	assert.Error(t, r.RemoveRules(inside), "listed under vibes, not rules")

	require.NoError(t, r.RemoveVibes(inside))
	assert.Nil(t, r.File.Vibes)
	require.NoError(t, r.RemoveRules(outside))
	assert.Nil(t, r.File.Rules)
	require.NoError(t, r.Save())
	data, err := os.ReadFile(repoPath)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "vibes")
	assert.NotContains(t, string(data), "rules")

	// Global scope stores paths relative to the global config directory when
	// inside it, which is rare; typical global entries are absolute.
	g, err := OpenGlobal()
	require.NoError(t, err)
	stored, err = g.AddVibes(inside)
	require.NoError(t, err)
	assert.Equal(t, inside, stored)
}

func TestValidatePromptFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.md")
	require.NoError(t, os.WriteFile(ok, []byte("héllo ✓\n"), 0o644))
	assert.NoError(t, ValidatePromptFile(ok))

	exact := filepath.Join(dir, "exact.md")
	require.NoError(t, os.WriteFile(exact, bytes.Repeat([]byte("a"), MaxPromptFileSize), 0o644))
	assert.NoError(t, ValidatePromptFile(exact), "64 KiB exactly is allowed")

	big := filepath.Join(dir, "big.md")
	require.NoError(t, os.WriteFile(big, bytes.Repeat([]byte("a"), MaxPromptFileSize+1), 0o644))
	err := ValidatePromptFile(big)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the 64 KiB limit")

	bad := filepath.Join(dir, "bad.md")
	require.NoError(t, os.WriteFile(bad, []byte{0xff, 0xfe, 'x'}, 0o644))
	err = ValidatePromptFile(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid UTF-8")

	nul := filepath.Join(dir, "nul.md")
	require.NoError(t, os.WriteFile(nul, []byte("a\x00b"), 0o644))
	err = ValidatePromptFile(nul)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NUL")

	assert.Error(t, ValidatePromptFile(dir), "directories are rejected")
	assert.ErrorIs(t, ValidatePromptFile(filepath.Join(dir, "missing.md")), os.ErrNotExist)

	r := &Doc{Scope: ScopeRepo, Path: filepath.Join(dir, RepoDirName, FileName)}
	_, err = r.AddVibes(big)
	assert.Error(t, err)
	assert.Nil(t, r.File.Vibes)
}

func TestStoredPath(t *testing.T) {
	dir, root := "/repo/.slop-cannon", "/repo"
	assert.Equal(t, "x.md", StoredPath(dir, root, "/repo/.slop-cannon/x.md"))
	assert.Equal(t, filepath.Join("..", "x.md"), StoredPath(dir, root, "/repo/x.md"))
	assert.Equal(t, filepath.Join("..", "docs", "x.md"), StoredPath(dir, root, "/repo/docs/x.md"))
	assert.Equal(t, "/elsewhere/x.md", StoredPath(dir, root, "/elsewhere/x.md"))
	assert.Equal(t, "/x.md", StoredPath(dir, root, "/x.md"))
	assert.Equal(t, "/repo-2/x.md", StoredPath(dir, root, "/repo-2/x.md"), "sibling with a shared prefix is outside")
	gdir := "/home/u/.config/slop-cannon"
	assert.Equal(t, "x.md", StoredPath(gdir, gdir, gdir+"/x.md"))
	assert.Equal(t, "/home/u/x.md", StoredPath(gdir, gdir, "/home/u/x.md"))
}
