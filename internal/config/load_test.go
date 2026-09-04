package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// configTestEnv points XDG_CONFIG_HOME, XDG_STATE_HOME and HOME at fresh
// temp dirs so nothing touches the real home, and returns a fake repo root
// (a directory with a .git entry) plus the two config paths.
func configTestEnv(t *testing.T) (repoRoot, globalPath, repoPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	repoRoot = filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755))
	return repoRoot, GlobalPath(), RepoConfigPath(repoRoot)
}

// configWrite writes raw TOML at path, creating directories.
func configWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

const configGlobalTOML = `
schema_version = 1
active_cartridge = "claude"
ammo = 8
safety = false
vibes = ["global-vibes.md"]
rules = ["/abs/rules.md"]
exclude = ["secrets/*.key"]
check_command = "make check"
idle_replan_interval = "5m"
gate_poll_interval = "30s"
pr_poll_interval = "45s"
max_beads_per_plan = 6

[limits]
max_review_rounds = 4
max_pushback_rounds = 1
agent_timeout = "20m"
max_tokens = 1000
max_cost_usd = 2.5
max_plan_rejections = 3
max_plan_failures = 7

[cartridges.claude]
model = "fable"
command = "/opt/claude"
args = ["--verbose"]
env = { FOO = "bar" }

[roles.tester]
cartridge = "claude"
`

const configRepoTOML = `
ammo = 12
safety = true
vibes = ["repo-vibes.md", "global-vibes.md"]
rules = ["docs/rules.md"]
exclude = ["private/**", "secrets/*.key"]
check_command = "go test ./..."
idle_replan_interval = "1h"
gate_poll_interval = "2m"
pr_poll_interval = "3m"
max_beads_per_plan = 4

[limits]
max_review_rounds = 6
max_pushback_rounds = 3
agent_timeout = "45m"
max_tokens = 2000
max_cost_usd = 9
max_plan_rejections = 4
max_plan_failures = 9

[roles.engineer]
cartridge = "claude"
`

func TestLoadDefaultsWhenNoFiles(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, Defaults(), eff.Config)
	assert.Equal(t, globalPath, eff.GlobalPath)
	assert.Equal(t, repoPath, eff.RepoPath)
	assert.Equal(t, repoRoot, eff.RepoRoot)
	assert.False(t, eff.GlobalExists)
	assert.False(t, eff.RepoExists)
	assert.Empty(t, eff.Warnings)
	for _, key := range scalarKeys {
		assert.Equal(t, SourceDefault, eff.Sources[key], key)
	}
	assert.Equal(t, SourceDefault, eff.Sources[KeyCartridges])
}

func TestLoadNoRepo(t *testing.T) {
	_, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, "ammo = 5\n")
	eff, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 5, eff.Ammo)
	assert.Empty(t, eff.RepoRoot)
	assert.Empty(t, eff.RepoPath)
	assert.True(t, eff.GlobalExists)
}

func TestLoadFindsRepoRootFromSubdir(t *testing.T) {
	repoRoot, _, repoPath := configTestEnv(t)
	configWrite(t, repoPath, "ammo = 7\n")
	sub := filepath.Join(repoRoot, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	eff, err := Load(sub)
	require.NoError(t, err)
	assert.Equal(t, repoRoot, eff.RepoRoot)
	assert.Equal(t, 7, eff.Ammo)
	assert.Equal(t, SourceRepo, eff.Sources[KeyAmmo])
}

func TestLoadNonRepoPathUsedAsRoot(t *testing.T) {
	configTestEnv(t)
	dir := t.TempDir()
	if _, inRepo := FindRepoRoot(dir); inRepo {
		t.Skip("temp dir is inside a git repository")
	}
	configWrite(t, RepoConfigPath(dir), "ammo = 9\n")
	eff, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, eff.RepoRoot, "a path outside any repo is used as the root itself")
	assert.Equal(t, 9, eff.Ammo)
	assert.Equal(t, SourceRepo, eff.Sources[KeyAmmo])
}

func TestLoadGlobalOnly(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, configGlobalTOML)
	eff, err := Load(repoRoot)
	require.NoError(t, err)

	assert.Equal(t, "claude", eff.ActiveCartridge)
	assert.Equal(t, Cartridge{Model: "fable", Command: "/opt/claude", Args: []string{"--verbose"}, Env: map[string]string{"FOO": "bar"}}, eff.Cartridges["claude"])
	assert.Equal(t, 8, eff.Ammo)
	assert.False(t, eff.Safety)
	assert.Equal(t, []string{filepath.Join(filepath.Dir(globalPath), "global-vibes.md")}, eff.Vibes)
	assert.Equal(t, []string{"/abs/rules.md"}, eff.Rules)
	assert.Equal(t, "make check", eff.CheckCommand)
	assert.Equal(t, 5*time.Minute, eff.IdleReplanInterval.Duration())
	assert.Equal(t, 30*time.Second, eff.GatePollInterval.Duration())
	assert.Equal(t, 45*time.Second, eff.PRPollInterval.Duration())
	assert.Equal(t, 6, eff.MaxBeadsPerPlan)
	assert.Equal(t, Limits{MaxReviewRounds: 4, MaxPushbackRounds: 1, AgentTimeout: NewDuration(20 * time.Minute),
		MaxTokens: 1000, MaxCostUSD: 2.5, MaxPlanRejections: 3, MaxPlanFailures: 7}, eff.Limits)
	assert.Equal(t, map[domain.Role]string{domain.RoleTester: "claude"}, eff.Roles)

	for _, key := range scalarKeys {
		assert.Equal(t, SourceGlobal, eff.Sources[key], key)
	}
	assert.Equal(t, SourceGlobal, eff.Sources[KeyCartridges])
	assert.Equal(t, SourceGlobal, eff.Sources[KeyCartridges+".claude"])
	assert.Equal(t, SourceGlobal, eff.Sources[KeyRoles+".tester"])
	assert.True(t, eff.GlobalExists)
	assert.False(t, eff.RepoExists)
}

func TestLoadRepoOverridesGlobalForEveryKey(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	configWrite(t, globalPath, configGlobalTOML)
	configWrite(t, repoPath, configRepoTOML)
	eff, err := Load(repoRoot)
	require.NoError(t, err)

	assert.Equal(t, 12, eff.Ammo)
	assert.True(t, eff.Safety)
	assert.Equal(t, "go test ./...", eff.CheckCommand)
	assert.Equal(t, time.Hour, eff.IdleReplanInterval.Duration())
	assert.Equal(t, 2*time.Minute, eff.GatePollInterval.Duration(), "the repo's gate poll interval wins")
	assert.Equal(t, 3*time.Minute, eff.PRPollInterval.Duration(), "the repo's pull request poll interval wins")
	assert.Equal(t, Limits{MaxReviewRounds: 6, MaxPushbackRounds: 3, AgentTimeout: NewDuration(45 * time.Minute),
		MaxTokens: 2000, MaxCostUSD: 9, MaxPlanRejections: 4, MaxPlanFailures: 9}, eff.Limits)
	// Lists union across layers, global first, deduplicated by absolute path.
	globalDir, repoDir := filepath.Dir(globalPath), filepath.Dir(repoPath)
	assert.Equal(t, []string{
		filepath.Join(globalDir, "global-vibes.md"),
		filepath.Join(repoDir, "repo-vibes.md"),
		filepath.Join(repoDir, "global-vibes.md"),
	}, eff.Vibes)
	assert.Equal(t, []string{"/abs/rules.md", filepath.Join(repoDir, "docs", "rules.md")}, eff.Rules)
	// exclude patterns are globs, so they union across layers as written,
	// deduplicated rather than resolved against the config file's directory.
	assert.Equal(t, []string{"secrets/*.key", "private/**"}, eff.Exclude)
	// Cartridges stay global.
	assert.Equal(t, "claude", eff.ActiveCartridge)
	assert.Equal(t, "fable", eff.Cartridges["claude"].Model)
	// Roles merge per role.
	assert.Equal(t, map[domain.Role]string{domain.RoleTester: "claude", domain.RoleEngineer: "claude"}, eff.Roles)

	want := map[string]Source{
		KeyActiveCartridge: SourceGlobal, KeyCartridges: SourceGlobal,
		KeyAmmo: SourceRepo, KeySafety: SourceRepo, KeyVibes: SourceMerged, KeyRules: SourceMerged, KeyExclude: SourceMerged,
		KeyCheckCommand: SourceRepo, KeyIdleReplanInterval: SourceRepo, KeyGatePollInterval: SourceRepo,
		KeyPRPollInterval:  SourceRepo,
		KeyMaxReviewRounds: SourceRepo, KeyMaxPushbackRounds: SourceRepo, KeyAgentTimeout: SourceRepo,
		KeyMaxTokens: SourceRepo, KeyMaxCostUSD: SourceRepo,
		KeyRoles + ".tester": SourceGlobal, KeyRoles + ".engineer": SourceRepo,
	}
	for key, src := range want {
		assert.Equal(t, src, eff.Sources[key], key)
	}
	assert.True(t, eff.RepoExists)
}

func TestLoadRepoOnlyKeepsDefaultsForUnsetKeys(t *testing.T) {
	repoRoot, _, repoPath := configTestEnv(t)
	configWrite(t, repoPath, "safety = false\n[limits]\nmax_tokens = 5\n")
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.False(t, eff.Safety)
	assert.Equal(t, SourceRepo, eff.Sources[KeySafety])
	assert.Equal(t, SourceRepo, eff.Sources[KeyMaxTokens])
	assert.Equal(t, DefaultAmmo, eff.Ammo)
	assert.Equal(t, SourceDefault, eff.Sources[KeyAmmo])
	assert.Equal(t, SourceDefault, eff.Sources[KeyMaxReviewRounds])
	assert.Equal(t, DefaultMaxReviewRounds, eff.Limits.MaxReviewRounds)
}

func TestLoadRepoCartridgesAreIgnoredWithWarning(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	configWrite(t, globalPath, "active_cartridge = \"claude\"\n[cartridges.claude]\nmodel = \"fable\"\n")
	configWrite(t, repoPath, "active_cartridge = \"claude\"\n[cartridges.claude]\nmodel = \"other\"\n")
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "fable", eff.Cartridges["claude"].Model)
	assert.Equal(t, SourceGlobal, eff.Sources[KeyActiveCartridge])
	require.Len(t, eff.Warnings, 1)
	assert.Contains(t, eff.Warnings[0], repoPath)
	assert.Contains(t, eff.Warnings[0], "global config")
}

func TestLoadApplyFlags(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, configGlobalTOML)
	eff, err := Load(repoRoot)
	require.NoError(t, err)

	ammo, safety, cart := 4, true, "claude"
	require.NoError(t, eff.Apply(Overrides{Ammo: &ammo, Safety: &safety, Cartridge: &cart}))
	assert.Equal(t, 4, eff.Ammo)
	assert.True(t, eff.Safety)
	assert.Equal(t, "claude", eff.ActiveCartridge)
	assert.Equal(t, SourceFlag, eff.Sources[KeyAmmo])
	assert.Equal(t, SourceFlag, eff.Sources[KeySafety])
	assert.Equal(t, SourceFlag, eff.Sources[KeyActiveCartridge])
	assert.Equal(t, SourceGlobal, eff.Sources[KeyCheckCommand], "untouched keys keep their source")

	require.NoError(t, eff.Apply(Overrides{}), "empty overrides are a no-op")
	assert.Equal(t, 4, eff.Ammo)

	low := 2
	assert.ErrorIs(t, eff.Apply(Overrides{Ammo: &low}), ErrAmmoTooLow)
	kimi := "kimi"
	assert.EqualError(t, eff.Apply(Overrides{Cartridge: &kimi}), `cartridge "kimi" is not supported yet (only "claude" for now)`)

	fresh, err := load(filepath.Join(t.TempDir(), "none.toml"), "")
	require.NoError(t, err)
	assert.EqualError(t, fresh.Apply(Overrides{Cartridge: &cart}), `cartridge "claude" is not configured; add it with: sc cartridge --add claude`)
}

func TestLoadMalformedTOMLReportsLine(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	configWrite(t, globalPath, "ammo = 10\n\nsafety = tru\n")
	_, err := Load(repoRoot)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, globalPath, pe.Path)
	assert.Equal(t, 3, pe.Line)
	assert.Equal(t, 10, pe.Column)
	assert.Equal(t, globalPath+`:3:10: expected keyword "true"`, err.Error())

	configWrite(t, globalPath, "ammo = 10\n")
	configWrite(t, repoPath, "[limits]\nmax_review_rounds = \"five\"\n")
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, repoPath, pe.Path)
	assert.Equal(t, 2, pe.Line)
	assert.Contains(t, pe.Msg, "limits.max_review_rounds")
	assert.Contains(t, pe.Msg, "cannot decode TOML string")
}

func TestLoadUnknownKeyIsAnError(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, "ammo = 10\namo = 12\n")
	_, err := Load(repoRoot)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 2, pe.Line)
	assert.Equal(t, `unknown key "amo"`, pe.Msg)

	configWrite(t, globalPath, "[limits]\nfoo = 1\nbar = 2\n")
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, `unknown key "limits.foo" (and 1 more)`, pe.Msg)
}

func TestLoadDurationMustBeString(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, "idle_replan_interval = 10\n")
	_, err := Load(repoRoot)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, `invalid duration "10"`, "a bare integer is not read as nanoseconds")

	configWrite(t, globalPath, "idle_replan_interval = \"soon\"\n")
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, `invalid duration "soon"`)
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name, global, repo, wantKey, wantMsg string
	}{
		{"ammo too low global", "ammo = 2\n", "", KeyAmmo, ErrAmmoTooLow.Error()},
		{"ammo too low repo", "ammo = 10\n", "ammo = 1\n", KeyAmmo, ErrAmmoTooLow.Error()},
		{"unknown cartridge name", "[cartridges.kimi]\nmodel = \"3\"\n", "", "cartridges.kimi", `cartridge "kimi" is not supported yet (only "claude" for now)`},
		{"active not configured", "active_cartridge = \"claude\"\n", "", KeyActiveCartridge, `cartridge "claude" is not configured; add it with: sc cartridge --add claude`},
		{"active unknown", "active_cartridge = \"animus\"\n", "", KeyActiveCartridge, `cartridge "animus" is not supported yet (only "claude" for now)`},
		{"bad role", "[roles.janitor]\ncartridge = \"claude\"\n", "", "roles.janitor", `role "janitor"`},
		{"bad role cartridge", "", "[roles.tester]\ncartridge = \"kimi\"\n", "roles.tester.cartridge", `cartridge "kimi" is not supported yet (only "claude" for now)`},
		{"zero interval", "idle_replan_interval = \"0s\"\n", "", KeyIdleReplanInterval, "must be a positive duration"},
		{"zero gate poll interval", "gate_poll_interval = \"0s\"\n", "", KeyGatePollInterval, "must be a positive duration"},
		{"zero pr poll interval", "pr_poll_interval = \"0s\"\n", "", KeyPRPollInterval, "must be a positive duration"},
		{"zero beads per plan", "max_beads_per_plan = 0\n", "", KeyMaxBeadsPerPlan, "must be at least 1"},
		{"negative beads per plan", "", "max_beads_per_plan = -3\n", KeyMaxBeadsPerPlan, "must be at least 1"},
		{"zero rounds", "", "[limits]\nmax_review_rounds = 0\n", KeyMaxReviewRounds, "must be at least 1"},
		{"negative pushback", "", "[limits]\nmax_pushback_rounds = -1\n", KeyMaxPushbackRounds, "must be at least 1"},
		// Zero is refused rather than accepted and then read as "unset" by
		// the feedback loop, which is the default two exchanges (P9-01).
		{"zero pushback", "[limits]\nmax_pushback_rounds = 0\n", "", KeyMaxPushbackRounds, "must be at least 1"},
		{"zero timeout", "[limits]\nagent_timeout = \"0s\"\n", "", KeyAgentTimeout, "must be a positive duration"},
		{"negative tokens", "[limits]\nmax_tokens = -5\n", "", KeyMaxTokens, "must not be negative"},
		{"negative cost", "", "[limits]\nmax_cost_usd = -0.5\n", KeyMaxCostUSD, "must not be negative"},
		{"no plan rejections", "[limits]\nmax_plan_rejections = 0\n", "", KeyMaxPlanRejections, "must be at least 1"},
		{"negative plan failures", "", "[limits]\nmax_plan_failures = -1\n", KeyMaxPlanFailures, "must be at least 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, globalPath, repoPath := configTestEnv(t)
			if tc.global != "" {
				configWrite(t, globalPath, tc.global)
			}
			if tc.repo != "" {
				configWrite(t, repoPath, tc.repo)
			}
			_, err := Load(repoRoot)
			var ve *ValidationError
			require.ErrorAs(t, err, &ve, "got %v", err)
			assert.Equal(t, tc.wantKey, ve.Key)
			assert.Contains(t, ve.Msg, tc.wantMsg)
			assert.Contains(t, err.Error(), ve.Path)
			if tc.repo != "" && tc.global == "" {
				assert.Equal(t, repoPath, ve.Path)
			}
		})
	}
}

func TestLoadRolesWithoutCartridgeAreIgnored(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, "[roles.tester]\n")
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Empty(t, eff.Roles)
}

func TestLoadSchemaVersion(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)

	configWrite(t, globalPath, "schema_version = 99\n")
	_, err := Load(repoRoot)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, "schema_version 99 was written by a newer sc")

	configWrite(t, globalPath, "schema_version = \"one\"\n")
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, "schema_version must be an integer")

	configWrite(t, globalPath, "schema_version = 0\n")
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, "positive integer")

	configWrite(t, globalPath, "ammo = 4\n")
	f, exists, err := ReadFile(globalPath)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, SchemaVersion, f.SchemaVersion, "missing schema_version means version 1")
}

func TestLoadMigrationHook(t *testing.T) {
	repoRoot, globalPath, _ := configTestEnv(t)
	// Pretend the current schema is 2 and version 2 renamed `bullets` to `ammo`.
	prevVersion, prevMigrations := schemaVersion, migrations
	t.Cleanup(func() { schemaVersion, migrations = prevVersion, prevMigrations })
	schemaVersion = 2
	migrations = []migration{{To: 2, Apply: func(doc map[string]any) error {
		if v, ok := doc["bullets"]; ok {
			doc["ammo"] = v
			delete(doc, "bullets")
		}
		return nil
	}}}

	configWrite(t, globalPath, "schema_version = 1\nbullets = 6\n")
	eff, err := Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, 6, eff.Ammo)
	assert.Equal(t, SourceGlobal, eff.Sources[KeyAmmo])
	f, _, err := ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, 2, f.SchemaVersion, "migrated in memory to the current version")

	// A file with no version is version 1 and is migrated too.
	configWrite(t, globalPath, "bullets = 7\n")
	eff, err = Load(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, 7, eff.Ammo)

	// The hook does not touch the file on disk; Save does.
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, "bullets = 7\n", string(data))

	// A version with no migration path fails loudly.
	migrations = nil
	configWrite(t, globalPath, "schema_version = 1\nammo = 6\n")
	_, err = Load(repoRoot)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, pe.Msg, "no migration to version 2")

	// A failing step reports which step failed.
	migrations = []migration{{To: 2, Apply: func(map[string]any) error { return errors.New("boom") }}}
	_, err = Load(repoRoot)
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "migrate config from schema 1 to 2: boom", pe.Msg)
}

func TestLoadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	repoRoot, globalPath, _ := configTestEnv(t)
	configWrite(t, globalPath, "ammo = 10\n")
	require.NoError(t, os.Chmod(globalPath, 0o000))
	_, err := Load(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config")
}

func TestEntries(t *testing.T) {
	repoRoot, globalPath, repoPath := configTestEnv(t)
	configWrite(t, globalPath, configGlobalTOML)
	configWrite(t, repoPath, "ammo = 12\n")
	eff, err := Load(repoRoot)
	require.NoError(t, err)

	entries := eff.Entries()
	byKey := map[string]Entry{}
	var order []string
	for _, e := range entries {
		byKey[e.Key] = e
		order = append(order, e.Key)
	}
	assert.Equal(t, Entry{Key: KeyAmmo, Value: "12", Source: SourceRepo}, byKey[KeyAmmo])
	assert.Equal(t, Entry{Key: KeySafety, Value: "false", Source: SourceGlobal}, byKey[KeySafety])
	assert.Equal(t, Entry{Key: KeyActiveCartridge, Value: "claude", Source: SourceGlobal}, byKey[KeyActiveCartridge])
	assert.Equal(t, Entry{Key: "cartridges.claude.model", Value: "fable", Source: SourceGlobal}, byKey["cartridges.claude.model"])
	assert.Equal(t, "/opt/claude", byKey["cartridges.claude.command"].Value)
	assert.Equal(t, "[--verbose]", byKey["cartridges.claude.args"].Value)
	assert.Equal(t, "[FOO=bar]", byKey["cartridges.claude.env"].Value)
	assert.Equal(t, "5m", byKey[KeyIdleReplanInterval].Value)
	assert.Equal(t, "30s", byKey[KeyGatePollInterval].Value)
	assert.Equal(t, "45s", byKey[KeyPRPollInterval].Value)
	assert.Equal(t, Entry{Key: KeyMaxBeadsPerPlan, Value: "6", Source: SourceGlobal}, byKey[KeyMaxBeadsPerPlan])
	assert.Equal(t, "20m", byKey[KeyAgentTimeout].Value)
	assert.Equal(t, "2.5", byKey[KeyMaxCostUSD].Value)
	assert.Equal(t, "1000", byKey[KeyMaxTokens].Value)
	assert.Equal(t, Entry{Key: KeyMaxPlanRejections, Value: "3", Source: SourceGlobal}, byKey[KeyMaxPlanRejections])
	assert.Equal(t, Entry{Key: KeyMaxPlanFailures, Value: "7", Source: SourceGlobal}, byKey[KeyMaxPlanFailures])
	assert.Equal(t, Entry{Key: "roles.tester.cartridge", Value: "claude", Source: SourceGlobal}, byKey["roles.tester.cartridge"])
	assert.Equal(t, Entry{Key: "roles.po.cartridge", Value: "", Source: SourceDefault}, byKey["roles.po.cartridge"])
	assert.Equal(t, "[/abs/rules.md]", byKey[KeyRules].Value)
	assert.Equal(t, KeyActiveCartridge, order[0])
	assert.Equal(t, "roles.tester.cartridge", order[len(order)-1])

	defaults, err := load(filepath.Join(t.TempDir(), "none.toml"), "")
	require.NoError(t, err)
	for _, e := range defaults.Entries() {
		assert.Equal(t, SourceDefault, e.Source, e.Key)
	}
	assert.Equal(t, "[]", defaults.Entries()[3].Value, "empty lists render as []")
}

func TestResolvePath(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	assert.Equal(t, "/base/x.md", ResolvePath("/base", "x.md"))
	assert.Equal(t, "/base/sub/x.md", ResolvePath("/base", "sub/x.md"))
	assert.Equal(t, "/abs/x.md", ResolvePath("/base", "/abs/x.md"))
	assert.Equal(t, "/home/tester/x.md", ResolvePath("/base", "~/x.md"))
	assert.Equal(t, "/home/tester", ResolvePath("/base", "~"))
}
