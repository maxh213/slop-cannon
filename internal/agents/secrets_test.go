package agents_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
)

// The secrets planted by every test in this file. They are the shapes a real
// repository leaks: an API token, a password in an environment file, and a
// PEM private key.
const (
	plantedToken    = "sk-ant-api03-plantedsecrettoken0123456789"
	plantedPassword = "DATABASE_PASSWORD=hunter2-hunter2-hunter2"
	plantedKey      = "-----BEGIN RSA PRIVATE KEY-----\nMIIEplantedprivatekeymaterial\n-----END RSA PRIVATE KEY-----"
)

// plantSecrets fills dir with a small project that keeps its secrets in the
// usual places, and returns the paths that must never reach a prompt.
func plantSecrets(t *testing.T, dir string) []string {
	t.Helper()
	files := map[string]string{
		"README.md":               "# Demo\n\nExport " + plantedToken + " before running.\n",
		"go.mod":                  "module demo\n",
		"main.go":                 "package main\n\nfunc main() {}\n",
		".env":                    plantedPassword + "\n",
		".env.production":         "API_TOKEN=" + plantedToken + "\n",
		"id_rsa":                  plantedKey + "\n",
		"certs/server.pem":        plantedKey + "\n",
		".ssh/id_ed25519":         plantedKey + "\n",
		"deploy/secrets/prod.key": "OTHER_SECRET=abcdefghijkl\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	return []string{".env", ".env.production", "id_rsa", "certs/server.pem", ".ssh/id_ed25519", "deploy/secrets/prod.key"}
}

// mustFilter builds a filter with the configured excludes of these tests.
func mustFilter(t *testing.T, exclude ...string) *agents.SecretFilter {
	t.Helper()
	f, err := agents.NewSecretFilter(exclude...)
	require.NoError(t, err)
	return f
}

func TestSecretFilterMatchesTheBuiltInPatterns(t *testing.T) {
	t.Parallel()
	f := mustFilter(t, "deploy/secrets/*", "*.key")
	for _, tc := range []struct {
		path string
		want string
	}{
		{".env", ".env"},
		{"config/.env", ".env"},
		{".env.production", ".env.*"},
		{"app.env", "*.env"},
		{"certs/server.pem", "*.pem"},
		{"id_rsa", "id_*"},
		{".ssh/id_ed25519", "id_*"},
		{"deploy/secrets/prod.key", "deploy/secrets/*"},
		{"keys/private.key", "*.key"},
	} {
		pattern, excluded := f.Match(tc.path)
		assert.True(t, excluded, "%s must be excluded", tc.path)
		assert.Equal(t, tc.want, pattern, tc.path)
	}
	for _, keep := range []string{"main.go", "README.md", "internal/env/env.go", "docs/environment.md", "cmd/", ""} {
		_, excluded := f.Match(keep)
		assert.False(t, excluded, "%s must not be excluded", keep)
	}
	assert.Equal(t, agents.DefaultSecretPatterns(), f.Patterns()[:len(agents.DefaultSecretPatterns())],
		"the built-in patterns come first and are always present")
}

// A nil filter is still a filter: the built-in patterns apply, so a caller
// that forgot to build one does not silently leak.
func TestNilSecretFilterStillExcludesTheBuiltIns(t *testing.T) {
	t.Parallel()
	var f *agents.SecretFilter
	assert.True(t, f.Excluded(".env"))
	assert.True(t, f.Excluded("certs/server.pem"))
	assert.False(t, f.Excluded("main.go"))
	assert.NotContains(t, f.Text("token "+plantedToken), plantedToken)
}

func TestNewSecretFilterRejectsAMalformedPattern(t *testing.T) {
	t.Parallel()
	_, err := agents.NewSecretFilter("secrets/[")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `exclude pattern "secrets/[" is malformed`)

	// Blank entries are ignored rather than rejected.
	f, err := agents.NewSecretFilter("  ", "", "/vault/")
	require.NoError(t, err)
	assert.True(t, f.Excluded("vault/key"))
}

// TestPlantedSecretsNeverReachAPrompt is the P9-05 acceptance criterion for
// prompts at the level this package can reach: with secrets planted in the
// repository, no prompt any role is fired with names them or quotes what
// they hold.
//
// It calls the filter itself, because a prompt builder is a pure function
// and nothing in this package knows how a run is wired. That is exactly why
// it is not the whole acceptance test: it would pass just as well if no
// production code ever called [agents.SecretFilter.GitDiff], which is how
// the diff half of P9-05 shipped unwired the first time.
// [TestTheDiffHalfOfTheFilterHasAProductionCaller] below is the guard
// against that, and the wiring itself is pinned end to end by
// TestFeedbackNeverPutsAWithheldPathInAPrompt (internal/orchestrator) and
// TestRunNeverPutsAPlantedSecretInAPrompt (internal/cli), which drive the
// real loop and the real binary.
func TestPlantedSecretsNeverReachAPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	planted := plantSecrets(t, dir)
	f := mustFilter(t, "deploy/secrets/*")

	rc, err := f.Scan(dir, prompt.ScanOptions{DefaultBranch: "main"})
	require.NoError(t, err)
	require.Contains(t, rc.Tree, "main.go", "the scan must still describe the repository")
	for _, p := range planted {
		assert.NotContains(t, rc.Tree, p)
	}

	bead := prompt.BeadContext{
		Bead: domain.Bead{
			ID: "sc-7a1", Title: "Rotate the credentials", State: domain.BeadStateInProgress,
			Acceptance: "The old key is gone.", Priority: domain.PriorityDefault,
		},
		Worktree: dir,
		Branch:   "sc/sc-7a1",
		Round:    1,
	}
	diff := &gitx.Diff{
		Stats: gitx.DiffStats{Base: "aaaa", Head: "bbbb", Files: 2, Insertions: 3, Deletions: 1, Changed: []gitx.FileStat{
			{Path: "main.go", Insertions: 2, Deletions: 1},
			{Path: ".env", Insertions: 1},
		}},
		Files: []gitx.DiffFile{
			{FileStat: gitx.FileStat{Path: "main.go", Insertions: 2, Deletions: 1}, Status: gitx.DiffModified},
			{FileStat: gitx.FileStat{Path: ".env", Insertions: 1}, Status: gitx.DiffAdded},
		},
		Patch: "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n+func main() {}\n" +
			"diff --git a/.env b/.env\n--- /dev/null\n+++ b/.env\n@@ -0,0 +1 @@\n+" + plantedPassword + "\n",
	}

	po, err := agents.PlanPrompt(agents.PlanRequest{
		Run: "run-42", AgentID: "po-1", Mode: domain.RunModeAutopilot, Repo: rc,
	})
	require.NoError(t, err)

	eng, err := agents.EngineerRequest{
		Mode: agents.EngineerImplement, RunID: "run-42", AgentID: "eng-1",
		Bead: bead, Repo: rc,
	}.Prompt()
	require.NoError(t, err)

	tester, err := agents.BuildTesterPrompt(agents.TesterRequest{
		Mode: agents.TesterModeReview, Lens: domain.LensQA, RunID: "run-42", AgentID: "tst-1",
		Workdir: dir, Round: 1, Bead: bead, Repo: rc, Diff: f.GitDiff(diff),
	})
	require.NoError(t, err)

	for name, p := range map[string]prompt.Prompt{"po": po, "engineer": eng, "tester": tester} {
		for _, secret := range append(planted, plantedToken, plantedPassword, "PRIVATE KEY") {
			assert.NotContains(t, p.Text, secret, "%s prompt leaks %q", name, secret)
		}
		assert.Contains(t, p.Text, "main.go", "%s prompt must still carry the repository", name)
	}
	assert.Contains(t, tester.Text, "withheld", "the tester is told the excluded change exists")
}

// The README is quoted in every prompt, so a token pasted into its
// quickstart is masked rather than repeated.
func TestSecretFilterRedactsTheReadme(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plantSecrets(t, dir)
	rc, err := mustFilter(t).Scan(dir, prompt.ScanOptions{})
	require.NoError(t, err)
	assert.Contains(t, rc.ReadmeHead, "# Demo")
	assert.NotContains(t, rc.ReadmeHead, plantedToken)
	assert.Contains(t, rc.ReadmeHead, logging.Redacted)
}

func TestSecretFilterDiffWithholdsExcludedSections(t *testing.T) {
	t.Parallel()
	f := mustFilter(t)
	patch := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"index 111..222 100644",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1,2 +1,3 @@",
		"+// kept",
		"diff --git a/.env b/.env",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/.env",
		"@@ -0,0 +1 @@",
		"+" + plantedPassword,
		"diff --git a/notes.md b/notes.md",
		"--- a/notes.md",
		"+++ b/notes.md",
		"@@ -1 +1 @@",
		"+the token is " + plantedToken,
		"",
	}, "\n")

	got := f.Diff(patch)
	assert.Contains(t, got, "+// kept")
	assert.NotContains(t, got, plantedPassword)
	assert.NotContains(t, got, plantedToken)
	assert.Contains(t, got, agents.SecretPlaceholder)
	assert.NotContains(t, got, ".env", "the withheld path is not named: an agent could open it")
	assert.Equal(t, 1, strings.Count(got, agents.SecretPlaceholder), "one placeholder per withheld section")
	assert.Contains(t, got, "the token is sk-"+logging.Redacted, "prose around a token survives")
	assert.Equal(t, 1, strings.Count(got, "diff --git a/notes.md"), "sections after the withheld one come back")

	// A plain unified diff with no git header is understood too.
	plain := "--- old/id_rsa\n+++ new/id_rsa\n@@ -0,0 +1 @@\n+" + plantedKey + "\n--- a/ok.txt\n+++ b/ok.txt\n@@ -1 +1 @@\n+fine\n"
	got = f.Diff(plain)
	assert.NotContains(t, got, "plantedprivatekeymaterial")
	assert.Contains(t, got, "+fine")

	// A path with a space in it is still recognised, quoted or not.
	spaced := "diff --git a/deploy keys/id_rsa b/deploy keys/id_rsa\n+" + plantedKey + "\n"
	assert.Contains(t, f.Diff(spaced), agents.SecretPlaceholder)
	quoted := "diff --git \"a/certs/caf\\303\\251.pem\" \"b/certs/caf\\303\\251.pem\"\n+" + plantedKey + "\n"
	assert.NotContains(t, f.Diff(quoted), "plantedprivatekeymaterial")

	// Nothing to withhold: the diff comes back as it went in.
	clean := "diff --git a/main.go b/main.go\n+ok\n"
	assert.Equal(t, clean, f.Diff(clean))
	assert.Equal(t, "", f.Diff(""))
}

func TestSecretFilterGitDiffDropsExcludedFilesAndStats(t *testing.T) {
	t.Parallel()
	f := mustFilter(t)
	in := &gitx.Diff{
		Stats: gitx.DiffStats{Base: "aaa", Head: "bbb", Files: 2, Insertions: 5, Deletions: 2, Changed: []gitx.FileStat{
			{Path: "main.go", Insertions: 4, Deletions: 2},
			{Path: "certs/server.pem", Insertions: 1},
		}},
		Files: []gitx.DiffFile{
			{FileStat: gitx.FileStat{Path: "main.go", Insertions: 4, Deletions: 2}, Status: gitx.DiffModified},
			{FileStat: gitx.FileStat{Path: "certs/server.pem", Insertions: 1}, Status: gitx.DiffAdded},
		},
		Patch: "diff --git a/certs/server.pem b/certs/server.pem\n+" + plantedKey + "\n",
	}
	out := f.GitDiff(in)
	require.Len(t, out.Files, 1)
	assert.Equal(t, "main.go", out.Files[0].Path)
	assert.Equal(t, 1, out.Stats.Files)
	assert.Equal(t, 4, out.Stats.Insertions)
	assert.Equal(t, 2, out.Stats.Deletions)
	assert.NotContains(t, out.Stat(), "server.pem")
	assert.NotContains(t, out.Patch, "plantedprivatekeymaterial")

	// The caller's diff is untouched: the D-09 change ratio still uses it.
	assert.Len(t, in.Files, 2)
	assert.Equal(t, 7, in.ChangedLines())
	assert.Nil(t, f.GitDiff(nil))
}

func TestSecretFilterTextMasksValues(t *testing.T) {
	t.Parallel()
	f := mustFilter(t)
	for _, tc := range []struct{ name, in string }{
		{"api token", "run with " + plantedToken},
		{"assignment", plantedPassword},
		{"pem body", plantedKey},
		{"aws key", "id AKIAIOSFODNN7EXAMPLE"},
		{"bearer", "Authorization: Bearer abcdefghijklmnop"},
	} {
		got := f.Text(tc.in)
		assert.Contains(t, got, logging.Redacted, tc.name)
		assert.NotContains(t, got, "hunter2-hunter2-hunter2", tc.name)
		assert.Equal(t, got, f.Text(got), "%s: redaction must be idempotent", tc.name)
	}
	assert.Contains(t, f.Text(plantedPassword), "DATABASE_PASSWORD=", "the name survives so the line still reads")
	assert.Contains(t, f.Text("+API_TOKEN=abcdefghijkl"), "+API_TOKEN=", "a diff line is masked, prefix and all")

	// The rule must not fire inside an expression: a reviewed diff of code
	// that reads a credential from the environment is not itself a leak.
	for _, code := range []string{
		`apiKey := os.Getenv("API_KEY")`,
		"\tif token := cfg.APIKey; token != \"\" {",
		"# set DATABASE_PASSWORD in your shell before running",
	} {
		assert.Equal(t, code, f.Text(code), "code must survive redaction unchanged")
	}
	clean := "nothing secret here\nsecond line\twith a tab"
	assert.Equal(t, clean, f.Text(clean), "clean text is returned unchanged, tabs included")
}

// TestPlantedSecretsNeverReachTheLogOrTheEventsFile is the other half of the
// P9-05 acceptance criterion: the same secrets, written through the run's
// logger and into the events file, are masked on disk.
func TestPlantedSecretsNeverReachTheLogOrTheEventsFile(t *testing.T) {
	t.Parallel()
	f := mustFilter(t)

	var log bytes.Buffer
	logger := logging.NewFileLogger(&log, logging.Options{Redactor: f.Redactor()})
	logger.Info("cartridge said "+plantedToken, "env", plantedPassword)
	assert.NotContains(t, log.String(), plantedToken)
	assert.NotContains(t, log.String(), "hunter2")
	assert.Contains(t, log.String(), logging.Redacted)

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	file, err := os.Create(path) //nolint:gosec // a temp file the test owns
	require.NoError(t, err)
	w := events.NewJSONLWriter(file)
	require.NoError(t, w.Write(events.AgentOutput{
		Meta:   events.Meta{Run: "run-42"},
		Agent:  "eng-1",
		Stream: events.StreamStdout,
		Line:   f.Text("exporting " + plantedToken),
	}))
	require.NoError(t, w.Write(events.AgentOutput{
		Meta:   events.Meta{Run: "run-42"},
		Agent:  "eng-1",
		Stream: events.StreamStderr,
		Line:   f.Text(plantedPassword),
	}))
	require.NoError(t, w.Err())
	require.NoError(t, file.Close())

	raw, err := os.ReadFile(path) //nolint:gosec // the file this test just wrote
	require.NoError(t, err)
	assert.NotContains(t, string(raw), plantedToken)
	assert.NotContains(t, string(raw), "hunter2")
	assert.Contains(t, string(raw), logging.Redacted)
}

// The `exclude` config key is what a user reaches for when the built-in
// patterns are not enough; it must survive the round trip into a filter.
func TestExcludeConfigKeyReachesTheFilter(t *testing.T) {
	t.Parallel()
	file, err := config.Parse("config.toml", []byte("schema_version = 1\nexclude = [\"deploy/secrets/*\", \"*.key\"]\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"deploy/secrets/*", "*.key"}, file.Exclude)

	f := mustFilter(t, file.Exclude...)
	assert.True(t, f.Excluded("deploy/secrets/prod.key"))
	assert.True(t, f.Excluded("anything.key"))
	assert.True(t, f.Excluded(".env"), "the built-ins still apply")

	// The list is unioned global-then-repo, exactly like vibes and rules.
	assert.Equal(t, []string{"*.key", "vault/*"}, config.MergeExcludes([]string{"*.key"}, []string{" *.key ", "vault/*", ""}))
}

// The other half of secrets hygiene: a file kept out of the prompts is still
// leaked if an agent commits it, so the run's doctor row asks whether the
// repository ignores .env at all (P9-05).
func TestGitignoreCoversEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{"exact", "bin/\n.env\n", true},
		{"glob", "# secrets\n.env*\n", true},
		{"anchored", "/.env\n", true},
		{"star env", "*.env\n", true},
		{"crlf", "bin/\r\n.env\r\n", true},
		{"negated", ".env*\n!.env\n", false},
		{"directory only", ".env/\n", false},
		{"commented out", "#.env\n", false},
		{"unrelated", "node_modules/\n*.log\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(tc.content), 0o600))
			got, err := agents.GitignoreCoversEnv(dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// No .gitignore at all is the same answer, and not an error.
	covered, err := agents.GitignoreCoversEnv(t.TempDir())
	require.NoError(t, err)
	assert.False(t, covered)
	assert.Contains(t, agents.GitignoreHint, ".gitignore")
}

// A prompt quotes the earlier rounds too, so their diffs and notes go
// through the same filter as this round's.
func TestSecretFilterHistory(t *testing.T) {
	t.Parallel()
	f := mustFilter(t)
	in := []prompt.Round{{
		Number: 1,
		Diff:   "diff --git a/.env b/.env\n+" + plantedPassword + "\n",
		Notes:  []string{"the check failed: " + plantedToken, "retrying"},
	}}
	out := f.History(in)
	require.Len(t, out, 1)
	assert.Contains(t, out[0].Diff, agents.SecretPlaceholder)
	assert.NotContains(t, out[0].Diff, "hunter2")
	assert.NotContains(t, out[0].Notes[0], plantedToken)
	assert.Equal(t, "retrying", out[0].Notes[1])

	// The caller's rounds are untouched.
	assert.Contains(t, in[0].Diff, "hunter2")
	assert.Contains(t, in[0].Notes[0], plantedToken)
	assert.Nil(t, f.History(nil))
}

// The key is unioned global-then-repo through the loader, so a repository
// can hide one more file without losing what the global config hides.
func TestExcludeConfigKeyMergesAcrossLayers(t *testing.T) {
	t.Parallel()
	global, err := config.Parse("global.toml", []byte("schema_version = 1\nexclude = [\"vault/*\"]\n"))
	require.NoError(t, err)
	repo, err := config.Parse("repo.toml", []byte("schema_version = 1\nexclude = [\"vault/*\", \"*.key\"]\n"))
	require.NoError(t, err)

	merged := config.MergeExcludes(config.MergeExcludes(nil, global.Exclude), repo.Exclude)
	assert.Equal(t, []string{"vault/*", "*.key"}, merged)

	f := mustFilter(t, merged...)
	assert.True(t, f.Excluded("vault/token"))
	assert.True(t, f.Excluded("deploy/prod.key"))
}

// TestTheDiffHalfOfTheFilterHasAProductionCaller is a regression guard on
// P9-05's own failure mode, not on the filter's behaviour.
//
// [agents.SecretFilter] shipped with the tree half wired into `sc run` and
// the diff half wired into nothing: SecretFilter.GitDiff, SecretFilter.Diff
// and SecretFilter.History had no caller anywhere in the module outside
// this package's tests, so every reviewed change reached the tester prompt
// with its .env, its keys and its excluded paths quoted line by line — and
// the acceptance test passed, because the test did the filtering itself.
//
// A method of the filter with no production caller is a claim the product
// does not keep, so each of these must be called from a non-test file
// outside internal/agents.
func TestTheDiffHalfOfTheFilterHasAProductionCaller(t *testing.T) {
	t.Parallel()
	sources := secretsProductionSources(t)
	// The receiver is spelled differently at each call site (an option
	// field, a struct field, a parameter), so the guard matches the method
	// on any receiver whose name ends in "filter".
	for _, method := range []string{"filter.GitDiff(", "filter.History(", "filter.Texts(", "filter.Message("} {
		found := ""
		for path, src := range sources {
			if strings.Contains(strings.ToLower(src), strings.ToLower(method)) {
				found = path
				break
			}
		}
		assert.NotEmpty(t, found,
			"no production code calls %s: the filter is built and then not used, "+
				"which is how the diff half of P9-05 shipped unwired", method)
	}
}

// secretsProductionSources reads every non-test .go file of the module
// outside internal/agents, keyed by path.
func secretsProductionSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir():
				// internal/agents is where the filter lives; a call inside
				// it is not a production caller for this guard's purpose.
				if d.Name() == "testdata" || d.Name() == "agents" {
					return filepath.SkipDir
				}
				return nil
			case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // test reading the module's own source
			if err != nil {
				return err
			}
			out[path] = string(b)
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, out)
	return out
}
