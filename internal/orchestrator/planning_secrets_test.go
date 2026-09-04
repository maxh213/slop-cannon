package orchestrator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
)

// The planning prompt is the first prompt of every run and the one prompt
// no caller has to prepare: [PlannerOptions.Repo] is optional, and when it
// is empty the planner scans the repository itself. That fallback used to
// call prompt.ScanRepo directly — the one call agents.SecretFilter.Scan's
// doc comment forbids — so a Planner built without a pre-scanned context
// would have listed .env, the keys and every configured exclude to the
// product owner. `sc run` happened to pass a filtered scan, which made the
// hole invisible rather than absent (P9-05).

// planningSecretFiles are the files a careless repository keeps beside its
// source, with the paths that must never reach a planning prompt.
func planningSecretFiles(t *testing.T, root string) []string {
	t.Helper()
	files := map[string]string{
		"main.go":            "package main\n\nfunc main() {}\n",
		".env":               "DATABASE_PASSWORD=hunter2-hunter2\n",
		"certs/server.pem":   "-----BEGIN RSA PRIVATE KEY-----\nplanted\n-----END RSA PRIVATE KEY-----\n",
		".ssh/id_ed25519":    "planted\n",
		"secrets/deploy.txt": "deployment-token-planted\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	return []string{".env", "certs/server.pem", ".ssh/id_ed25519", "secrets/deploy.txt"}
}

// TestPlannerFallbackScanIsFiltered is the guard on that fallback: a planner
// with no pre-scanned context still shows the product owner a filtered tree,
// with the built-in patterns and the run's configured `exclude` list both
// applied.
func TestPlannerFallbackScanIsFiltered(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	secrets := planningSecretFiles(t, rc.Run.RepoPath)

	f, err := agents.NewSecretFilter("secrets/*")
	require.NoError(t, err)
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
		Filter: f,
	})
	require.NoError(t, err)

	rcx, err := p.repoContext()
	require.NoError(t, err)
	require.NotEmpty(t, rcx.Tree, "the fallback scan still reads the repository")
	assert.Contains(t, rcx.Tree, "main.go", "and still shows the code")
	for _, secret := range secrets {
		assert.NotContains(t, rcx.Tree, secret, "the planning prompt would name %q", secret)
	}
}

// TestPlannerWithoutAFilterStillExcludesTheBuiltIns pins the default: a
// Planner assembled without a filter — the next caller, or a test — gets the
// built-in patterns rather than no filtering at all. Only the configured
// `exclude` list is lost, because there is nothing to read it from.
func TestPlannerWithoutAFilterStillExcludesTheBuiltIns(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	planningSecretFiles(t, rc.Run.RepoPath)

	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
	})
	require.NoError(t, err)

	rcx, err := p.repoContext()
	require.NoError(t, err)
	for _, secret := range []string{".env", "certs/server.pem", ".ssh/id_ed25519"} {
		assert.NotContains(t, rcx.Tree, secret)
	}
}

// TestPlannerFiltersAPreScannedContextToo closes the other half: a caller
// that scanned the repository itself, without the filter, must not be able
// to hand the product owner what the run hides everywhere else.
func TestPlannerFiltersAPreScannedContextToo(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	secrets := planningSecretFiles(t, rc.Run.RepoPath)

	raw, err := prompt.ScanRepo(rc.Run.RepoPath, prompt.ScanOptions{})
	require.NoError(t, err)
	require.Subset(t, raw.Tree, []string{".env"}, "the unfiltered scan really does list the secrets")

	f, err := agents.NewSecretFilter("secrets/*")
	require.NoError(t, err)
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
		Repo: raw, Filter: f,
	})
	require.NoError(t, err)

	rcx, err := p.repoContext()
	require.NoError(t, err)
	for _, secret := range secrets {
		assert.NotContains(t, rcx.Tree, secret)
	}
}
