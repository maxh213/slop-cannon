package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// ammoTestEnv points HOME and the XDG directories at a fresh temp dir so the
// command never touches the real config, and returns a directory that is
// not inside any git repository (writes land in the global file) plus one
// that is (writes land in <repo>/.slop-cannon/config.toml).
func ammoTestEnv(t *testing.T) (outside, repo string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	outside = filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	repo = filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	return outside, repo
}

// ammoStored reads the ammo value stored in the config file at path.
func ammoStored(t *testing.T, path string) int {
	t.Helper()
	f, exists, err := config.ReadFile(path)
	require.NoError(t, err)
	require.True(t, exists, "config file %s was not written", path)
	require.NotNil(t, f.Ammo, "ammo not set in %s", path)
	return *f.Ammo
}

func TestAmmoHelpGolden(t *testing.T) {
	out, err := rootRun(t, "ammo", "--help")
	require.NoError(t, err)
	rootGolden(t, "ammo-help", out)
}

func TestAmmoAllocationTable(t *testing.T) {
	outside, _ := ammoTestEnv(t)
	global := config.GlobalPath()

	tests := []struct {
		ammo string
		want string
	}{
		{"3", "ammo 3 → 1 PO, up to 1 engineer, up to 1 tester"},
		{"10", "ammo 10 → 1 PO, up to 2 engineers, up to 7 testers"},
		{"12", "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers"},
		{"13", "ammo 13 → 1 PO, up to 3 engineers, up to 9 testers"},
	}
	for _, tc := range tests {
		t.Run(tc.ammo, func(t *testing.T) {
			out, err := rootRun(t, "--repo", outside, "ammo", tc.ammo)
			require.NoError(t, err)
			require.Equal(t, tc.want+"\nsaved to "+global+"\n", out)
			n, err := strconv.Atoi(tc.ammo)
			require.NoError(t, err)
			require.Equal(t, n, ammoStored(t, global))
		})
	}
}

func TestAmmoTooLow(t *testing.T) {
	outside, _ := ammoTestEnv(t)
	for _, v := range []string{"2", "0", "-1"} {
		t.Run(v, func(t *testing.T) {
			out, err := rootRun(t, "--repo", outside, "ammo", "--", v)
			require.ErrorIs(t, err, config.ErrAmmoTooLow)
			require.EqualError(t, err, "ammo must be at least 3: one engineer, one tester, one product owner")
			require.Empty(t, out)
			_, exists, rerr := config.ReadFile(config.GlobalPath())
			require.NoError(t, rerr)
			require.False(t, exists, "a rejected value must not be written")
		})
	}
}

func TestAmmoNonInteger(t *testing.T) {
	outside, _ := ammoTestEnv(t)
	for _, v := range []string{"twelve", "12.5", "", "1e3", "99999999999999999999"} {
		t.Run(v, func(t *testing.T) {
			out, err := rootRun(t, "--repo", outside, "ammo", v)
			require.Error(t, err)
			require.Contains(t, err.Error(), "is not a whole number")
			require.Contains(t, err.Error(), "example: sc ammo 12")
			require.Empty(t, out)
			_, exists, rerr := config.ReadFile(config.GlobalPath())
			require.NoError(t, rerr)
			require.False(t, exists, "a rejected value must not be written")
		})
	}
}

func TestAmmoWritesRepoConfigInsideGitRepo(t *testing.T) {
	_, repo := ammoTestEnv(t)
	repoCfg := config.RepoConfigPath(repo)

	// From a subdirectory the repo root is still found.
	out, err := rootRun(t, "--repo", filepath.Join(repo, "sub"), "ammo", "12")
	require.NoError(t, err)
	require.Equal(t, "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers\nsaved to "+repoCfg+"\n", out)
	require.Equal(t, 12, ammoStored(t, repoCfg))
	_, exists, err := config.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	require.False(t, exists, "global config must stay untouched")

	// A second write updates the value in place.
	_, err = rootRun(t, "--repo", repo, "ammo", "7")
	require.NoError(t, err)
	require.Equal(t, 7, ammoStored(t, repoCfg))
}

func TestAmmoGlobalFlagForcesGlobalConfig(t *testing.T) {
	_, repo := ammoTestEnv(t)
	out, err := rootRun(t, "--global", "--repo", repo, "ammo", "4")
	require.NoError(t, err)
	require.Equal(t, "ammo 4 → 1 PO, up to 1 engineer, up to 2 testers\nsaved to "+config.GlobalPath()+"\n", out)
	require.Equal(t, 4, ammoStored(t, config.GlobalPath()))
	_, exists, err := config.ReadFile(config.RepoConfigPath(repo))
	require.NoError(t, err)
	require.False(t, exists, "repo config must stay untouched with --global")
}

func TestAmmoPreservesOtherKeys(t *testing.T) {
	_, repo := ammoTestEnv(t)
	doc, err := config.OpenRepo(repo)
	require.NoError(t, err)
	doc.SetSafety(false)
	require.NoError(t, doc.Save())

	_, err = rootRun(t, "--repo", repo, "ammo", "5")
	require.NoError(t, err)
	f, _, err := config.ReadFile(config.RepoConfigPath(repo))
	require.NoError(t, err)
	require.NotNil(t, f.Safety)
	require.False(t, *f.Safety)
	require.Equal(t, 5, ammoStored(t, config.RepoConfigPath(repo)))
}

func TestAmmoJSON(t *testing.T) {
	_, repo := ammoTestEnv(t)
	out, err := rootRun(t, "--json", "--repo", repo, "ammo", "12")
	require.NoError(t, err)
	var got AmmoResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, AmmoResult{
		Allocation: config.Allocation{Ammo: 12, PO: 1, MaxEngineers: 2, MaxTesters: 9},
		Scope:      config.ScopeRepo,
		Path:       config.RepoConfigPath(repo),
	}, got)
	require.True(t, strings.HasSuffix(out, "}\n"))
	require.NotContains(t, out, `"source"`, "source is omitted when a value was written")
}

func TestAmmoShowEffective(t *testing.T) {
	outside, repo := ammoTestEnv(t)

	// Nothing configured: the default with its source.
	out, err := rootRun(t, "--repo", outside, "ammo")
	require.NoError(t, err)
	require.Equal(t, "ammo 10 → 1 PO, up to 2 engineers, up to 7 testers (from default)\n", out)

	// Global value, seen from outside any repo and from inside one.
	_, err = rootRun(t, "--global", "ammo", "6")
	require.NoError(t, err)
	out, err = rootRun(t, "--repo", outside, "ammo")
	require.NoError(t, err)
	require.Equal(t, "ammo 6 → 1 PO, up to 1 engineer, up to 4 testers (from global)\n", out)
	out, err = rootRun(t, "--repo", repo, "ammo")
	require.NoError(t, err)
	require.Equal(t, "ammo 6 → 1 PO, up to 1 engineer, up to 4 testers (from global)\n", out)

	// Repo value wins inside the repo (D-03).
	_, err = rootRun(t, "--repo", repo, "ammo", "13")
	require.NoError(t, err)
	out, err = rootRun(t, "--repo", filepath.Join(repo, "sub"), "ammo")
	require.NoError(t, err)
	require.Equal(t, "ammo 13 → 1 PO, up to 3 engineers, up to 9 testers (from repo)\n", out)

	out, err = rootRun(t, "--json", "--repo", repo, "ammo")
	require.NoError(t, err)
	var got AmmoResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, AmmoResult{
		Allocation: config.Allocation{Ammo: 13, PO: 1, MaxEngineers: 3, MaxTesters: 9},
		Scope:      config.ScopeRepo,
		Source:     config.SourceRepo,
	}, got)
	require.NotContains(t, out, `"path"`, "path is omitted when nothing was written")
}

func TestAmmoShowReportsBadConfig(t *testing.T) {
	_, repo := ammoTestEnv(t)
	require.NoError(t, config.WriteAtomic(config.RepoConfigPath(repo), []byte("ammo = 2\n"), 0o644))
	_, err := rootRun(t, "--repo", repo, "ammo")
	require.Error(t, err)
	var verr *config.ValidationError
	require.ErrorAs(t, err, &verr)
}

func TestAmmoRejectsExtraArgs(t *testing.T) {
	outside, _ := ammoTestEnv(t)
	_, err := rootRun(t, "--repo", outside, "ammo", "12", "13")
	require.Error(t, err)
}
