package beads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// envSamplesDir returns internal/beads/testdata/bd, the bd 1.2.2 outputs
// captured for this package.
func envSamplesDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "bd"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "version.json"))
	require.NoError(t, err, "captured bd samples missing")
	return dir
}

// envSample reads one captured sample file.
func envSample(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(envSamplesDir(t), name))
	require.NoError(t, err)
	return string(data)
}

// envIsolate points HOME and the XDG directories at temp dirs and clears
// BEADS_DIR so no test can touch the machine's own beads state.
func envIsolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("BEADS_DIR", "")
	require.NoError(t, os.Unsetenv("BEADS_DIR"))
}

func TestSpec(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		s := Spec(CallOptions{}, "ready", "-n", "0", "--json")
		assert.Equal(t, "bd", s.Command)
		assert.Equal(t, []string{"ready", "-n", "0", "--json"}, s.Args)
		assert.Empty(t, s.Dir)
		assert.Equal(t, map[string]string{
			"BD_JSON_ENVELOPE":   "1",
			"BD_NON_INTERACTIVE": "1",
			"BEADS_ACTOR":        "sc",
		}, s.Env)
		assert.Equal(t, InheritEnv, s.InheritEnv)
		assert.Equal(t, DefaultTimeout, s.Timeout)
		assert.Nil(t, s.Stdin)
	})
	t.Run("repo, actor, readonly, timeout, stdin", func(t *testing.T) {
		in := strings.NewReader("body")
		s := Spec(CallOptions{Repo: "/r", Actor: "sc:engineer-1", ReadOnly: true, Timeout: time.Second, Stdin: in, Dir: "/d"}, "show", "x-1", "--json")
		assert.Equal(t, []string{"--readonly", "-C", "/r", "show", "x-1", "--json"}, s.Args)
		assert.Equal(t, "sc:engineer-1", s.Env["BEADS_ACTOR"])
		assert.Equal(t, time.Second, s.Timeout)
		assert.Equal(t, "/d", s.Dir)
		assert.Same(t, in, s.Stdin)
	})
	t.Run("inherit list is a copy", func(t *testing.T) {
		s := Spec(CallOptions{})
		s.InheritEnv[0] = "MUTATED"
		assert.Equal(t, "HOME", InheritEnv[0])
	})
	t.Run("only the allowlist reaches bd", func(t *testing.T) {
		t.Setenv("HOME", "/fake/home")
		t.Setenv("SC_BEADS_SECRET", "leak")
		env := Spec(CallOptions{}).Environ()
		v, ok := proc.EnvValue(env, "HOME")
		assert.True(t, ok)
		assert.Equal(t, "/fake/home", v)
		_, leaked := proc.EnvValue(env, "SC_BEADS_SECRET")
		assert.False(t, leaked)
		v, _ = proc.EnvValue(env, "BD_JSON_ENVELOPE")
		assert.Equal(t, "1", v)
	})
}

func TestRunUsesTheContract(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBin(t, "bd", `printf '%s\n' "$BD_JSON_ENVELOPE" "$BD_NON_INTERACTIVE" "$BEADS_ACTOR" "$PWD"`)
	dir := t.TempDir()
	res, err := Run(context.Background(), CallOptions{Repo: "/repo", Actor: "sc:po", Dir: dir}, "ready", "--json")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	require.Len(t, lines, 4)
	assert.Equal(t, []string{"1", "1", "sc:po"}, lines[:3])
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(lines[3])
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, [][]string{{"-C", "/repo", "ready", "--json"}}, bd.ArgLists(t))
}

func TestFindJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"envelope", `{"data":{"version":"1.2.2"},"schema_version":1}`, `{"data":{"version":"1.2.2"},"schema_version":1}`, false},
		{"pretty printed", "{\n  \"build\": \"dev\",\n  \"schema_version\": 1\n}\n", "{\n  \"build\": \"dev\",\n  \"schema_version\": 1\n}", false},
		{"preamble with braces", "notice {not json}\nmore text\n{\"data\":1,\"schema_version\":1}\ntrailing\n", `{"data":1,"schema_version":1}`, false},
		{"gate check text line first", "Checked 1 gates: 0 resolved, 1 pending\n{\"data\":[],\"schema_version\":1}\n", `{"data":[],"schema_version":1}`, false},
		{"envelope preferred over earlier value", "[1] step done\n{\"data\":{\"a\":1},\"schema_version\":1}\n", `{"data":{"a":1},"schema_version":1}`, false},
		{"legacy array", "[{\"id\":\"x-1\"}]\n", `[{"id":"x-1"}]`, false},
		{"legacy object", "{\"id\":\"x-1\"}", `{"id":"x-1"}`, false},
		{"no json", "Error: no beads database found\n", "", true},
		{"empty", "", "", true},
		{"unterminated", "{\"data\": ", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindJSON(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrNoJSON), "got %v", err)
				assert.Contains(t, err.Error(), "no JSON object in output")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

func TestParseEnvelope(t *testing.T) {
	t.Parallel()
	env, err := ParseEnvelope(`{"schema_version":1,"data":{"status":"ok"},"pagination":{"limit":100}}`)
	require.NoError(t, err)
	assert.Equal(t, 1, env.SchemaVersion)
	assert.JSONEq(t, `{"status":"ok"}`, string(env.Data))
	assert.JSONEq(t, `{"limit":100}`, string(env.Pagination))

	env, err = ParseEnvelope(`{"error":"nope","code":"not_found","hint":"check the id","schema_version":1}`)
	require.NoError(t, err)
	assert.Equal(t, "nope", env.Error)
	assert.Equal(t, "not_found", env.Code)
	assert.Equal(t, "check the id", env.Hint)
	assert.Empty(t, env.Data)

	env, err = ParseEnvelope(`[{"id":"x-1"}]`)
	require.NoError(t, err)
	assert.Zero(t, env.SchemaVersion)
	assert.JSONEq(t, `[{"id":"x-1"}]`, string(env.Data))

	_, err = ParseEnvelope("nothing here")
	assert.True(t, errors.Is(err, ErrNoJSON))
}

func TestDecode(t *testing.T) {
	t.Parallel()
	var v struct {
		Version string `json:"version"`
	}
	require.NoError(t, Decode(`{"data":{"version":"1.2.2"},"schema_version":1}`, &v))
	assert.Equal(t, "1.2.2", v.Version)

	v.Version = ""
	require.NoError(t, Decode("{\n  \"build\": \"dev\",\n  \"schema_version\": 1,\n  \"version\": \"1.2.3\"\n}\n", &v))
	assert.Equal(t, "1.2.3", v.Version, "flat shape with schema_version but no data member is the legacy create shape")

	v.Version = ""
	require.NoError(t, Decode("notice {not json}\nmore text\n{\"data\":{\"version\":\"1.2.4\"},\"schema_version\":1}\ntrailing\n", &v))
	assert.Equal(t, "1.2.4", v.Version, "preamble containing braces is skipped")

	v.Version = ""
	require.NoError(t, Decode(`{"version":"1.2.5"}`, &v))
	assert.Equal(t, "1.2.5", v.Version, "plain object without envelope")

	var list []struct {
		ID string `json:"id"`
	}
	require.NoError(t, Decode(`{"schema_version":1,"data":[{"id":"x-1"},{"id":"x-2"}]}`, &list))
	require.Len(t, list, 2)
	assert.Equal(t, "x-2", list[1].ID)

	err := Decode("Error: no beads database found\n", &v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON object in output")
	require.Error(t, Decode("", &v))

	err = Decode(`{"schema_version":1,"data":{"version":5}}`, &v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding bd output")

	v.Version = "stale"
	require.NoError(t, Decode(`{"schema_version":1,"error":"boom"}`, &v), "an envelope without data decodes as a whole")
	assert.Equal(t, "stale", v.Version)
}

func TestDecodeCapturedSamples(t *testing.T) {
	t.Parallel()
	var version struct {
		Version string `json:"version"`
		Build   string `json:"build"`
	}
	require.NoError(t, Decode(envSample(t, "version.json"), &version))
	assert.Equal(t, "1.2.2", version.Version)
	assert.Equal(t, "dev", version.Build)

	var ping PingResult
	require.NoError(t, Decode(envSample(t, "ping.json"), &ping))
	assert.Equal(t, "ok", ping.Status)
	assert.Positive(t, ping.TotalMS)

	// Text outputs hold no JSON at all.
	var anything json.RawMessage
	for _, name := range []string{"init.txt", "version-text.txt", "ping-no-db.stderr"} {
		err := Decode(envSample(t, name), &anything)
		assert.True(t, errors.Is(err, ErrNoJSON), "%s: %v", name, err)
	}
}

func TestErrorSummary(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "timed out", ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: -1}, Cause: proc.ErrTimeout}))
	assert.Equal(t, "exit status 1: Error: no beads database found",
		ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: 1, Stderr: "\nError: no beads database found\nHint: run bd init\n"}}))
	assert.Equal(t, "exit status 2: from stdout",
		ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: 2, Stdout: "from stdout\n"}}))
	preamble, err := os.ReadFile(filepath.Join(testutil.BDSamplesDir(t), "preamble.txt"))
	require.NoError(t, err)
	assert.Equal(t, "exit status 1: Error: cannot use -C directory \"/r\": no beads project found",
		ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: 1, Stderr: string(preamble) + "Error: cannot use -C directory \"/r\": no beads project found\nHint: run bd init\n"}}),
		"the first-run metrics notice on stderr is skipped in favour of the Error: line")
	assert.Equal(t, "exit status 1: warning: something",
		ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: 1, Stderr: "\nwarning: something\ndetail\n"}}))
	assert.Equal(t, "exit status 3", ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: 3}}))
	assert.Equal(t, "proc: bd: killed by signal", ErrorSummary(&proc.ExitError{Command: "bd", Result: proc.Result{ExitCode: -1}}))
	assert.Equal(t, "plain", ErrorSummary(errors.New("plain")))
}
