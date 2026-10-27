package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/dash"
	"slop-cannon/internal/domain"
)

// dashCapture records the command's resolved read-only server options.
type dashCapture struct {
	opts dash.Options
}

func dashStub(t *testing.T) *dashCapture {
	t.Helper()
	c := &dashCapture{}
	saved := dashServe
	t.Cleanup(func() { dashServe = saved })
	dashServe = func(_ context.Context, opts dash.Options) error {
		c.opts = opts
		return nil
	}
	return c
}

func TestDashHelpGolden(t *testing.T) {
	out, err := rootRun(t, "dash", "--help")
	require.NoError(t, err)
	rootGolden(t, "dash-help", out)
}

func TestDashDefaultsToLatestRunOnLoopback(t *testing.T) {
	_, repo := runTestEnv(t)
	older, newer := domain.RunID("20260924-100000-aaaa"), domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, older, watchStarted(older), watchFinished(older))
	watchWrite(t, repo, newer, watchStarted(newer), watchFinished(newer))
	c := dashStub(t)

	stdout, stderr, code := runExec(t, "dash", repo)
	require.Equal(t, 0, code, stderr)
	require.Empty(t, stdout, "the test server does not print its listening address")
	require.Equal(t, newer, c.opts.Run.ID)
	require.Equal(t, repo, c.opts.Run.RepoPath)
	require.Equal(t, config.RunDir(repo, string(newer)), c.opts.RunDir)
	require.Equal(t, dash.DefaultHost, c.opts.Host)
	require.Equal(t, dash.DefaultPort, c.opts.Port)
	require.Equal(t, dash.DefaultBasePath, c.opts.BasePath)
	require.NotNil(t, c.opts.Out)
	require.False(t, c.opts.Dandelion)
	require.Equal(t, dash.DefaultDandelionRefresh, c.opts.DandelionRefresh)
}

func TestDashNamesARunAndTakesServerFlags(t *testing.T) {
	_, repo := runTestEnv(t)
	older, newer := domain.RunID("20260924-100000-aaaa"), domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, older, watchStarted(older), watchFinished(older))
	watchWrite(t, repo, newer, watchStarted(newer), watchFinished(newer))
	c := dashStub(t)

	_, stderr, code := runExec(t, "dash", "--repo", repo, "--run", string(older),
		"--host", "::1", "--port", "0", "--base-path", "/sc/",
		"--allowed-host", "DASH.DEMO.OTWARTE.XYZ.", "--allowed-host", "other.example",
		"--dandelion", "--dandelion-refresh", "90s")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, older, c.opts.Run.ID)
	require.Equal(t, "::1", c.opts.Host)
	require.Equal(t, 0, c.opts.Port)
	require.Equal(t, "/sc", c.opts.BasePath)
	require.Equal(t, []string{"dash.demo.otwarte.xyz", "other.example"}, c.opts.AllowedHosts)
	require.True(t, c.opts.Dandelion)
	require.Equal(t, 90*time.Second, c.opts.DandelionRefresh)
}

func TestDashDandelionRefreshMinimumDoesNotEnableCollection(t *testing.T) {
	_, repo := runTestEnv(t)
	run := domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, run, watchStarted(run), watchFinished(run))
	c := dashStub(t)

	_, stderr, code := runExec(t, "dash", repo, "--dandelion-refresh", "1m", "--dandelion=false")
	require.Equal(t, 0, code, stderr)
	require.False(t, c.opts.Dandelion)
	require.Equal(t, dash.MinDandelionRefresh, c.opts.DandelionRefresh)
}

func TestDashRejectsInvalidDandelionRefresh(t *testing.T) {
	_, repo := runTestEnv(t)
	run := domain.RunID("20260924-200000-bbbb")
	watchWrite(t, repo, run, watchStarted(run), watchFinished(run))
	c := dashStub(t)

	for _, refresh := range []string{"59s", "0s", "-1m", "1ms", "forever"} {
		_, stderr, code := runExec(t, "dash", repo, "--dandelion", "--dandelion-refresh", refresh)
		require.Equal(t, 1, code, refresh)
		require.NotEmpty(t, stderr, refresh)
		if refresh != "forever" {
			require.Contains(t, stderr, "dandelion refresh must be at least 1m0s", refresh)
		}
	}
	require.Zero(t, c.opts.RunDir, "invalid Dandelion options must not start a server")
}

func TestDashRefusals(t *testing.T) {
	_, repo := runTestEnv(t)

	_, stderr, code := runExec(t, "dash", repo)
	require.Equal(t, 1, code)
	require.Equal(t, "sc: no runs recorded for "+repo+"\n", stderr)

	_, stderr, code = runExec(t, "dash", repo, "--run", "20260101-000000-none")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "run 20260101-000000-none: no run directory at ")

	c := dashStub(t)
	for _, args := range [][]string{
		{"dash", repo, "--host="},
		{"dash", repo, "--port", "70000"},
		{"dash", repo, "--base-path", "relative"},
		{"dash", repo, "--base-path", "/sc/../admin"},
		{"dash", repo, "--base-path", "/sc?run=other"},
		{"dash", repo, "--allowed-host", "https://dash.demo.otwarte.xyz"},
		{"dash", repo, "--allowed-host", "*.example"},
	} {
		_, stderr, code = runExec(t, args...)
		require.Equal(t, 1, code, args)
		require.NotEmpty(t, stderr, args)
	}
	require.Zero(t, c.opts.RunDir, "invalid listener options must not start a server")
}
