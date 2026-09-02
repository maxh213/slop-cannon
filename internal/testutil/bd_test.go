package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// bdKeyCases pairs bd argument lists with the sample key they must resolve
// to. Both BDKey (Go) and the shim (shell) are checked against it.
var bdKeyCases = []struct {
	args []string
	key  string
}{
	{[]string{"ready", "-n", "0", "--json"}, "ready"},
	{[]string{"-C", "/repo", "--actor", "sc:eng-1", "show", "fx-1", "--json", "--include-comments"}, "show"},
	{[]string{"show", "fx-1", "fx-2", "--json"}, "show"},
	{[]string{"list", "-n", "0", "--flat", "--all", "-s", "open,closed", "--json"}, "list"},
	{[]string{"dep", "add", "fx-a0u.1", "fx-0uu", "--json"}, "dep-add"},
	{[]string{"dep", "list", "fx-a0u.1", "--json"}, "dep-list"},
	{[]string{"comments", "fx-a0u.1", "--json"}, "comments"},
	{[]string{"comment", "fx-z7f", "--stdin", "--json"}, "comment"},
	{[]string{"gate", "create", "--type=gh:pr", "--blocks", "fx-0uu", "--await-id", "1", "--json"}, "gate-create"},
	{[]string{"gate", "check", "--type=gh:pr", "--json"}, "gate-check"},
	{[]string{"create", "--graph", "/tmp/plan.json", "--dry-run", "--json"}, "create-graph-dry-run"},
	{[]string{"create", "--graph=/tmp/plan.json", "--json"}, "create-graph"},
	{[]string{"create", "Add Median", "-t", "task", "-p", "2", "--parent", "fx-a0u", "--json"}, "create"},
	{[]string{"config", "set", "status.custom", "in_review:wip", "--json"}, "config-set"},
	{[]string{"--readonly", "update", "fx-1", "-s", "open", "--assignee", "", "--json"}, "update"},
	{[]string{"update", "fx-1", "--claim", "--json"}, "update"},
	{[]string{"close", "fx-1", "--reason", "done", "--json"}, "close"},
	{[]string{"epic", "status", "--json"}, "epic-status"},
	{[]string{"init", "--non-interactive", "--init-if-missing", "-p", "fx", "--json"}, "init"},
	{[]string{"export"}, "export"},
	{[]string{"version", "--json"}, "version"},
}

func TestBDKey(t *testing.T) {
	for _, tc := range bdKeyCases {
		require.Equal(t, tc.key, BDKey(tc.args), "args %q", tc.args)
	}
	require.Equal(t, "", BDKey(nil))
	require.Equal(t, "", BDKey([]string{"--json"}))
}

func TestFakeBDShimDerivesSameKeysAsBDKey(t *testing.T) {
	// An empty samples directory makes every call a miss, and the miss log
	// records the key the shell logic derived.
	shim := FakeBD(t, t.TempDir())
	var want []string
	for _, tc := range bdKeyCases {
		_, errOut, code := runShim(t, t.TempDir(), "bd", "", tc.args...)
		require.Equal(t, 3, code, "args %q", tc.args)
		require.Contains(t, errOut, "fakebd: no sample for key \""+tc.key+"\"")
		want = append(want, tc.key)
	}
	require.Equal(t, want, MissingSamples(t, shim))
}

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data"`
}

func decodeEnvelope(t *testing.T, out string) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal([]byte(out), &env), "not an envelope: %q", out)
	require.Equal(t, 1, env.SchemaVersion)
	return env
}

func issueIDs(t *testing.T, data json.RawMessage) []string {
	t.Helper()
	var issues []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(data, &issues))
	ids := make([]string, len(issues))
	for i, is := range issues {
		ids[i] = is.ID
	}
	return ids
}

func TestFakeBDReplaysCapturedSamples(t *testing.T) {
	shim := FakeBD(t, BDSamplesDir(t))
	work := t.TempDir()

	out, errOut, code := runShim(t, work, "bd", "", "-C", work, "version", "--json")
	require.Equal(t, 0, code)
	require.Contains(t, errOut, "anonymous usage metrics", "first call prints bd's one-time notice on stderr")
	var version struct {
		Version string `json:"version"`
		Build   string `json:"build"`
	}
	require.NoError(t, json.Unmarshal(decodeEnvelope(t, out).Data, &version))
	require.Equal(t, "1.2.2", version.Version)

	out, errOut, code = runShim(t, work, "bd", "", "-C", work, "ready", "-n", "0", "--json")
	require.Equal(t, 0, code)
	require.Empty(t, errOut, "the notice is printed once only")
	require.Equal(t, []string{"fx-a0u", "fx-z7f", "fx-0uu"}, issueIDs(t, decodeEnvelope(t, out).Data))

	out, _, code = runShim(t, work, "bd", "", "show", "fx-z7f", "--json", "--include-comments")
	require.Equal(t, 0, code)
	var shown []struct {
		ID       string         `json:"id"`
		Status   string         `json:"status"`
		Metadata map[string]any `json:"metadata"`
		Comments []struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(decodeEnvelope(t, out).Data, &shown))
	require.Len(t, shown, 1, "bd show returns an array even for one id")
	require.Equal(t, "fx-z7f", shown[0].ID)
	require.Equal(t, "in_review", shown[0].Status)
	require.Equal(t, float64(1), shown[0].Metadata["sc_round"], "bd stores numeric --set-metadata values as JSON numbers")
	require.Len(t, shown[0].Comments, 1)
	require.Equal(t, "sc:test", shown[0].Comments[0].Author)
	require.Contains(t, shown[0].Comments[0].Text, `"type":"impl_report"`)

	out, _, code = runShim(t, work, "bd", "", "create", "--graph", "/tmp/plan.json", "--json")
	require.Equal(t, 0, code)
	var graph struct {
		IDs map[string]string `json:"ids"`
	}
	require.NoError(t, json.Unmarshal(decodeEnvelope(t, out).Data, &graph))
	require.Equal(t, map[string]string{"epic": "fx-a0u", "mean": "fx-z7f", "max": "fx-0uu"}, graph.IDs)

	out, errOut, code = runShim(t, work, "bd", "", "create", "--graph", "/tmp/plan.json", "--dry-run", "--json")
	require.Equal(t, 0, code)
	require.Contains(t, errOut, "unknown field(s)", "dry-run stderr warnings are replayed")
	require.Contains(t, out, `"dry_run": true`)

	out, _, code = runShim(t, work, "bd", "", "export")
	require.Equal(t, 0, code)
	first, _, _ := strings.Cut(out, "\n")
	require.True(t, strings.HasPrefix(first, `{"_type":"issue"`), "export is JSONL without an envelope: %q", first)

	require.Equal(t, [][]string{
		{"-C", work, "version", "--json"},
		{"-C", work, "ready", "-n", "0", "--json"},
		{"show", "fx-z7f", "--json", "--include-comments"},
		{"create", "--graph", "/tmp/plan.json", "--json"},
		{"create", "--graph", "/tmp/plan.json", "--dry-run", "--json"},
		{"export"},
	}, shim.ArgLists(t))
	require.Empty(t, MissingSamples(t, shim))
}

func TestFakeBDReplaysErrorSamples(t *testing.T) {
	dir := StageBDSamples(t, map[string]string{
		"close":  "error-close-blocked",
		"show":   "error-not-found",
		"update": "error-readonly",
	})
	shim := FakeBD(t, dir)
	work := t.TempDir()

	out, errOut, code := runShim(t, work, "bd", "", "close", "fx-a0u.1", "--reason", "done", "--json")
	require.Equal(t, 1, code)
	require.Empty(t, out, "bd prints nothing on stdout for this refusal")
	require.Contains(t, errOut, "blocked by open issues [fx-a0u.2]")

	out, errOut, code = runShim(t, work, "bd", "", "show", "nope-1", "--json")
	require.Equal(t, 1, code)
	var data struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(decodeEnvelope(t, out).Data, &data))
	require.Equal(t, "no issues found matching the provided IDs", data.Error)
	require.Contains(t, errOut, "no issue found matching")

	out, errOut, code = runShim(t, work, "bd", "", "--readonly", "update", "fx-0uu", "-s", "open", "--json")
	require.Equal(t, 1, code)
	require.Empty(t, out)
	require.Contains(t, errOut, "operation 'update' is not allowed in read-only mode")

	// Keys that were not overridden still replay the defaults.
	out, _, code = runShim(t, work, "bd", "", "ready", "-n", "0", "--json")
	require.Equal(t, 0, code)
	require.Len(t, issueIDs(t, decodeEnvelope(t, out).Data), 3)
	require.Empty(t, MissingSamples(t, shim))
}

func TestFakeBDSequencedSamples(t *testing.T) {
	dir := StageBDSamples(t, map[string]string{"ready.2": "ready-after-close"})
	FakeBD(t, dir)
	work := t.TempDir()

	ids := func() []string {
		out, _, code := runShim(t, work, "bd", "", "ready", "-n", "0", "--json")
		require.Equal(t, 0, code)
		return issueIDs(t, decodeEnvelope(t, out).Data)
	}
	require.Equal(t, []string{"fx-a0u", "fx-z7f", "fx-0uu"}, ids(), "first call: default sample")
	require.Equal(t, []string{"fx-a0u", "fx-a0u.1"}, ids(), "second call: sequenced sample")
	require.Equal(t, []string{"fx-a0u", "fx-z7f", "fx-0uu"}, ids(), "third call: back to the default")

	// Other keys keep their own counters.
	out, _, code := runShim(t, work, "bd", "", "blocked", "--json")
	require.Equal(t, 0, code)
	require.Equal(t, []string{"fx-a0u.1"}, issueIDs(t, decodeEnvelope(t, out).Data))
}

func TestStageBDSamplesRejectsUnknownSample(t *testing.T) {
	sub := &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		StageBDSamples(sub, map[string]string{"ready": "no-such-sample"})
	}()
	require.True(t, sub.failed)
}

func TestStageBDSamplesDropsStaleCompanions(t *testing.T) {
	// error-not-found has .json, .stderr and .exit; overriding it with the
	// plain "show" sample must remove the exit and stderr files so the call
	// succeeds again.
	dir := StageBDSamples(t, map[string]string{"error-not-found": "show"})
	for _, ext := range []string{".stderr", ".exit"} {
		_, err := os.Stat(filepath.Join(dir, "error-not-found"+ext))
		require.True(t, os.IsNotExist(err), "%s should be gone", ext)
	}
	_, err := os.Stat(filepath.Join(dir, "error-not-found.json"))
	require.NoError(t, err)
}

func TestFakeBDCapturesCommentBody(t *testing.T) {
	shim := FakeBD(t, BDSamplesDir(t))
	body := "Verdict\n\n```json\n{\"type\":\"verdict\",\"decision\":\"approve\"}\n```\n"
	out, _, code := runShim(t, t.TempDir(), "bd", body, "comment", "fx-z7f", "--stdin", "--json")
	require.Equal(t, 0, code)
	require.Contains(t, out, `"schema_version": 1`)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	require.Equal(t, body, calls[0].Stdin)
	require.Equal(t, []string{"comment", "fx-z7f", "--stdin", "--json"}, calls[0].Args)
}

func TestFakeBDMissingSampleFailsLoudly(t *testing.T) {
	shim := FakeBD(t, BDSamplesDir(t))
	out, errOut, code := runShim(t, t.TempDir(), "bd", "", "frobnicate", "--json")
	require.Equal(t, 3, code)
	require.Empty(t, out)
	require.Contains(t, errOut, `fakebd: no sample for key "frobnicate"`)
	require.Equal(t, []string{"frobnicate"}, MissingSamples(t, shim))
}

func TestBDSamplesAreValidEnvelopes(t *testing.T) {
	dir := BDSamplesDir(t)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		var env envelope
		require.NoError(t, json.Unmarshal(data, &env), e.Name())
		require.Equal(t, 1, env.SchemaVersion, e.Name())
		require.NotEmpty(t, env.Data, e.Name())
		require.NotContains(t, string(data), "/home/", "%s leaks a machine path", e.Name())
		checked++
	}
	require.GreaterOrEqual(t, checked, 30, "expected the full captured sample set")
}
