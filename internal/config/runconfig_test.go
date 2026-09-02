package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// runConfigEnv isolates HOME and the XDG directories and returns a fake
// repository root.
func runConfigEnv(t *testing.T) (repo string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "home", ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "home", ".local", "state"))
	repo = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	return repo
}

// runConfigEffective builds an effective config with one active cartridge.
func runConfigEffective(t *testing.T, repo string) *Effective {
	t.Helper()
	eff, err := Load(repo)
	require.NoError(t, err)
	eff.Cartridges["claude"] = Cartridge{Model: "fable", Args: []string{"-p"}}
	eff.ActiveCartridge = "claude"
	eff.Sources[KeyActiveCartridge] = SourceGlobal
	eff.Sources[KeyCartridges+".claude"] = SourceGlobal
	return eff
}

func TestNewRunIDFormatAndOrder(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC)
	id := NewRunID(now)
	require.Regexp(t, regexp.MustCompile(`^20260902-123456-[0-9a-f]{4}$`), id.String())
	require.Equal(t, id.String(), filepath.Base(RunDir("/repo", id.String())), "id must be a single path element")

	later := NewRunID(now.Add(time.Second))
	require.Less(t, id.String(), later.String(), "ids sort chronologically")

	// Local times are rendered in UTC.
	local := now.In(time.FixedZone("plus2", 2*3600))
	require.Equal(t, id.String()[:15], NewRunID(local).String()[:15])
}

func TestNewRunConfigNewRun(t *testing.T) {
	repo := runConfigEnv(t)
	eff := runConfigEffective(t, repo)
	eff.Vibes = []string{"/v.md"}
	started := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)

	rc, err := NewRunConfig(repo, eff, RunSettings{
		Brief: "fix it", Headless: true, ReviewPRs: true, NoAnim: true, StartedAt: started,
	})
	require.NoError(t, err)
	require.Equal(t, "20260902-010203", rc.Run.ID.String()[:15])
	require.Equal(t, repo, rc.Run.RepoPath)
	require.Equal(t, RunDir(repo, rc.Run.ID.String()), rc.Run.StateDir)
	require.Equal(t, domain.RunModeBrief, rc.Run.Mode)
	require.Equal(t, "fix it", rc.Run.Brief)
	require.Equal(t, started, rc.Run.StartedAt)
	require.False(t, rc.Resume)
	require.True(t, rc.NoAnim)
	require.Equal(t, "claude", rc.CartridgeName)
	require.Equal(t, Cartridge{Model: "fable", Args: []string{"-p"}}, rc.Cartridge)
	require.Equal(t, Allocation{Ammo: 10, PO: 1, MaxEngineers: 2, MaxTesters: 7}, rc.Allocation)
	require.Same(t, eff, rc.Effective)

	want := domain.ConfigSnapshot{
		Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable",
		Vibes: []string{"/v.md"}, ReviewPRs: true, Headless: true,
	}
	require.Equal(t, want, rc.Run.Config)

	// The cartridge is a copy: mutating the config does not leak into the run.
	eff.Cartridges["claude"].Args[0] = "changed"
	require.Equal(t, "-p", rc.Cartridge.Args[0])
}

func TestNewRunConfigAutopilotDefaultsAndExplicitID(t *testing.T) {
	repo := runConfigEnv(t)
	eff := runConfigEffective(t, repo)
	before := time.Now()
	rc, err := NewRunConfig(repo, eff, RunSettings{ID: "custom-id"})
	require.NoError(t, err)
	require.Equal(t, domain.RunID("custom-id"), rc.Run.ID)
	require.Equal(t, domain.RunModeAutopilot, rc.Run.Mode)
	require.Empty(t, rc.Run.Brief)
	require.False(t, rc.Run.Config.Headless)
	require.False(t, rc.Run.Config.ReviewPRs)
	require.False(t, rc.Run.StartedAt.Before(before))
}

func TestNewRunConfigRefusesWithoutCartridge(t *testing.T) {
	repo := runConfigEnv(t)
	eff, err := Load(repo)
	require.NoError(t, err)
	_, err = NewRunConfig(repo, eff, RunSettings{})
	require.ErrorIs(t, err, ErrNoCartridge)

	_, err = NewRunConfig(repo, nil, RunSettings{})
	require.Error(t, err)
}

func TestNewRunConfigResume(t *testing.T) {
	repo := runConfigEnv(t)
	eff := runConfigEffective(t, repo)

	_, err := NewRunConfig(repo, eff, RunSettings{Resume: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing to resume")
	require.Contains(t, err.Error(), filepath.Join(StateDir(repo), RunsDirName))

	for _, id := range []string{"20260901-000000-aaaa", "20260902-000000-bbbb", "20260831-235959-ffff"} {
		require.NoError(t, os.MkdirAll(RunDir(repo, id), 0o755))
	}
	// A stray file in the runs directory is not a run.
	require.NoError(t, os.WriteFile(filepath.Join(StateDir(repo), RunsDirName, "99999999-zzz"), nil, 0o644))

	rc, err := NewRunConfig(repo, eff, RunSettings{Resume: true, Brief: ""})
	require.NoError(t, err)
	require.True(t, rc.Resume)
	require.Equal(t, domain.RunID("20260902-000000-bbbb"), rc.Run.ID)
	require.Equal(t, RunDir(repo, "20260902-000000-bbbb"), rc.Run.StateDir)

	// An explicit id wins over the lookup.
	rc, err = NewRunConfig(repo, eff, RunSettings{Resume: true, ID: "20260901-000000-aaaa"})
	require.NoError(t, err)
	require.Equal(t, domain.RunID("20260901-000000-aaaa"), rc.Run.ID)
}

func TestLatestRunID(t *testing.T) {
	repo := runConfigEnv(t)
	id, found, err := LatestRunID(repo)
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, id)

	runs := filepath.Join(StateDir(repo), RunsDirName)
	require.NoError(t, os.MkdirAll(runs, 0o755))
	id, found, err = LatestRunID(repo)
	require.NoError(t, err)
	require.False(t, found, "empty runs dir")
	require.Empty(t, id)

	require.NoError(t, os.MkdirAll(filepath.Join(runs, "b"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(runs, "a"), 0o755))
	id, found, err = LatestRunID(repo)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, domain.RunID("b"), id)
}

func TestRunConfigJSONRoundTrip(t *testing.T) {
	repo := runConfigEnv(t)
	eff := runConfigEffective(t, repo)
	rc, err := NewRunConfig(repo, eff, RunSettings{ID: "r1", Brief: "b", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)

	data, err := json.Marshal(rc)
	require.NoError(t, err)
	var back RunConfig
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, rc.Run, back.Run)
	require.Equal(t, rc.CartridgeName, back.CartridgeName)
	require.Equal(t, rc.Cartridge, back.Cartridge)
	require.Equal(t, rc.Allocation, back.Allocation)
	require.NotNil(t, back.Effective)
	require.Equal(t, rc.Effective.Ammo, back.Effective.Ammo)
	require.Equal(t, rc.Effective.Sources, back.Effective.Sources)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	for _, key := range []string{"run", "resume", "no_anim", "cartridge_name", "cartridge", "allocation", "effective"} {
		require.Contains(t, raw, key)
	}
}
