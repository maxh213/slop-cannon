package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/logging"
)

// Tests for the one-"run"-key-per-record handler (P8-05). `sc logs` prints
// the run log verbatim, so a record with the key repeated two or three
// times is what a user — and their jq — actually meets.

// logAttrLines decodes the JSON lines written to buf.
func logAttrLines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	require.NotEmpty(t, out)
	return out
}

// TestLogAttrOnceDropsRepeatedKeys: the wrapped handler keeps the value its
// base already carries and drops every later copy, whether it arrives
// through With or on the record itself.
func TestLogAttrOnceDropsRepeatedKeys(t *testing.T) {
	var buf bytes.Buffer
	base := logging.NewFileHandler(&buf, logging.Options{}).
		WithAttrs([]slog.Attr{slog.String("run", "20260904-120000-abcd")})
	l := slog.New(logAttrOnce(base, "run")).
		With(slog.String("run", "20260904-120000-abcd")).
		With(slog.String("run", "20260904-120000-abcd"), slog.String("component", "scheduler"))
	l.Info("fired", slog.String("run", "20260904-120000-abcd"), slog.Int("agents", 2))

	line := logAttrLines(t, &buf)[0]
	require.Equal(t, 1, strings.Count(line, `"run":`), "one run key per record: %s", line)
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &rec))
	require.Equal(t, "20260904-120000-abcd", rec["run"])
	require.Equal(t, "scheduler", rec["component"], "other attributes are untouched")
	require.EqualValues(t, 2, rec["agents"])
	require.Equal(t, "fired", rec["msg"])
}

// TestLogAttrOncePassesEverythingElseThrough: a record without the key is
// handed on unchanged, and a group makes later attributes nested rather
// than top level, so the filter steps out of the way.
func TestLogAttrOncePassesEverythingElseThrough(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(logAttrOnce(logging.NewFileHandler(&buf, logging.Options{}), "run"))
	l.Info("plain", slog.String("bead", "sc-1"))
	l.WithGroup("agent").With(slog.String("run", "r1")).Info("grouped")

	lines := logAttrLines(t, &buf)
	require.Contains(t, lines[0], `"bead":"sc-1"`)
	require.NotContains(t, lines[0], `"run":`)
	require.Contains(t, lines[1], `"agent":{"run":"r1"}`, "a grouped key cannot collide with the top-level one")
}

// TestLogAttrOnceIsInertWithoutAHandlerOrKey covers the guard clauses.
func TestLogAttrOnceIsInertWithoutAHandlerOrKey(t *testing.T) {
	require.Nil(t, logAttrOnce(nil, "run"))
	h := logging.NewFileHandler(&bytes.Buffer{}, logging.Options{})
	require.Equal(t, h, logAttrOnce(h, ""))
}

// TestRunLogHasOneRunKeyPerLine is the production proof: a real headless
// run's sc.log — the file `sc logs` prints — carries the run id exactly
// once per record, although three layers stamp it (logging.OpenRunLog, the
// headless logger here, and orchestrator.New).
func TestRunLogHasOneRunKeyPerLine(t *testing.T) {
	env := runFireSetup(t, false)

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	id := runLatestID(t, env.Repo.Dir)
	dir := config.RunDir(env.Repo.Dir, string(id))
	data, err := os.ReadFile(filepath.Join(dir, logging.RunLogName)) //nolint:gosec // a temp run directory
	require.NoError(t, err)

	withRun := 0
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := strings.Count(line, `"run":`)
		require.LessOrEqual(t, n, 1, "the run id is written %d times in one record: %s", n, line)
		if n == 1 {
			withRun++
			require.Contains(t, line, `"run":"`+string(id)+`"`)
		}
	}
	require.NotZero(t, withRun, "no record carries the run id at all")
}
