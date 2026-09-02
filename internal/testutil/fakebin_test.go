package testutil

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// runShim runs the named command through PATH lookup and returns stdout,
// stderr, and the exit code.
func runShim(t *testing.T, dir, name string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "running %s: %v", name, err)
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func TestFakeBinRecordsInvocationsInOrder(t *testing.T) {
	shim := FakeBin(t, "recorder", "echo ok")
	work := t.TempDir()

	for i := 0; i < 3; i++ {
		out, _, code := runShim(t, work, "recorder", "", "call", strconv.Itoa(i), "--json")
		require.Equal(t, 0, code)
		require.Equal(t, "ok\n", out)
	}

	calls := shim.Invocations(t)
	require.Len(t, calls, 3)
	wantDir, err := filepath.EvalSymlinks(work)
	require.NoError(t, err)
	for i, c := range calls {
		require.Equal(t, i+1, c.Seq)
		require.Equal(t, "recorder", c.Name)
		require.Equal(t, []string{"call", strconv.Itoa(i), "--json"}, c.Args)
		gotDir, err := filepath.EvalSymlinks(c.Dir)
		require.NoError(t, err)
		require.Equal(t, wantDir, gotDir)
		require.Empty(t, c.Stdin)
	}
	require.Equal(t, [][]string{{"call", "0", "--json"}, {"call", "1", "--json"}, {"call", "2", "--json"}}, shim.ArgLists(t))
}

func TestFakeBinPreservesAwkwardArguments(t *testing.T) {
	shim := FakeBin(t, "argv", "exit 0")
	args := []string{"", "two words", "new\nline", `quo"te'd`, "$HOME", "--flag=a b", "-", "tab\there", "unicodé"}
	_, _, code := runShim(t, t.TempDir(), "argv", "", args...)
	require.Equal(t, 0, code)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	require.Equal(t, args, calls[0].Args)
}

func TestFakeBinNoArguments(t *testing.T) {
	shim := FakeBin(t, "noargs", "exit 0")
	_, _, code := runShim(t, t.TempDir(), "noargs", "")
	require.Equal(t, 0, code)
	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	require.Empty(t, calls[0].Args)
}

func TestFakeBinScriptControlsOutputAndExitStatus(t *testing.T) {
	FakeBin(t, "fails", `echo "out $1"; echo "err $2" >&2; exit 7`)
	out, errOut, code := runShim(t, t.TempDir(), "fails", "", "a", "b")
	require.Equal(t, "out a\n", out)
	require.Equal(t, "err b\n", errOut)
	require.Equal(t, 7, code)
}

func TestFakeBinExposesEnvironment(t *testing.T) {
	realGit, err := RealBinary("git")
	if err != nil {
		t.Skip("git not available")
	}
	script := `printf '%s\n' "$SC_FAKEBIN_NAME" "$SC_FAKEBIN_SEQ" "$SC_FAKEBIN_REAL" "$SC_FAKEBIN_DIR" "$SC_FAKEBIN_LOG"`
	shim := FakeBin(t, "git", script)

	out, _, code := runShim(t, t.TempDir(), "git", "", "status")
	require.Equal(t, 0, code)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Equal(t, []string{"git", "1", realGit, shim.Dir, shim.LogPath}, lines)

	out, _, _ = runShim(t, t.TempDir(), "git", "", "status")
	require.Equal(t, "2", strings.Split(out, "\n")[1], "sequence number increments per call")

	// A name that exists nowhere else gets an empty SC_FAKEBIN_REAL.
	FakeBin(t, "sc-no-such-tool", `printf '[%s]' "$SC_FAKEBIN_REAL"`)
	out, _, _ = runShim(t, t.TempDir(), "sc-no-such-tool", "")
	require.Equal(t, "[]", out)
}

func TestFakeBinCapturesStdin(t *testing.T) {
	shim := FakeBin(t, "reader", `cat "$SC_FAKEBIN_STDIN"`, WithStdinCapture())
	body := "line one\nline two\n"
	out, _, code := runShim(t, t.TempDir(), "reader", body, "--stdin")
	require.Equal(t, 0, code)
	require.Equal(t, body, out, "the script can re-read the captured stdin")

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	require.Equal(t, body, calls[0].Stdin)
	require.Equal(t, []string{"--stdin"}, calls[0].Args)
}

func TestFakeBinConcurrentCallsAreSerialisedAndComplete(t *testing.T) {
	shim := FakeBin(t, "busy", `echo "$SC_FAKEBIN_SEQ"`)
	const n = 24
	work := t.TempDir()
	seqs := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command("busy", "worker", strconv.Itoa(i))
			cmd.Dir = work
			out, err := cmd.Output()
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			seq, err := strconv.Atoi(strings.TrimSpace(string(out)))
			if err != nil {
				t.Errorf("worker %d: bad seq %q", i, out)
				return
			}
			seqs[i] = seq
		}(i)
	}
	wg.Wait()

	sort.Ints(seqs)
	for i, s := range seqs {
		require.Equal(t, i+1, s, "sequence numbers must be unique and dense")
	}

	calls := shim.Invocations(t)
	require.Len(t, calls, n)
	var workers []int
	for i, c := range calls {
		require.Equal(t, i+1, c.Seq)
		require.Len(t, c.Args, 2, "record %d must not be interleaved: %v", i+1, c.Args)
		require.Equal(t, "worker", c.Args[0])
		w, err := strconv.Atoi(c.Args[1])
		require.NoError(t, err)
		workers = append(workers, w)
	}
	sort.Ints(workers)
	for i, w := range workers {
		require.Equal(t, i, w)
	}
}

func TestFakeBinReset(t *testing.T) {
	shim := FakeBin(t, "resettable", `echo "$SC_FAKEBIN_SEQ"`)
	runShim(t, t.TempDir(), "resettable", "", "one")
	require.Len(t, shim.Invocations(t), 1)

	shim.Reset(t)
	require.Empty(t, shim.Invocations(t))
	out, _, _ := runShim(t, t.TempDir(), "resettable", "", "two")
	require.Equal(t, "1\n", out, "sequence restarts after Reset")
	require.Equal(t, [][]string{{"two"}}, shim.ArgLists(t))
}

func TestFakeBinShadowsRealToolAndRealBinarySkipsIt(t *testing.T) {
	realGit, err := RealBinary("git")
	if err != nil {
		t.Skip("git not available")
	}
	shim := FakeBin(t, "git", "exit 0")

	found, err := exec.LookPath("git")
	require.NoError(t, err)
	require.Equal(t, shim.Path, found, "PATH must resolve to the shim")

	stillReal, err := RealBinary("git")
	require.NoError(t, err)
	require.Equal(t, realGit, stillReal)

	cmd := shim.Command(t, "--version")
	require.Equal(t, shim.Path, cmd.Path)
	require.NoError(t, cmd.Run())
	require.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t))
}

func TestRealBinaryNotFound(t *testing.T) {
	_, err := RealBinary("sc-definitely-missing-tool")
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestMultipleShimsRecordSeparately(t *testing.T) {
	gh := FakeBin(t, "gh", "echo gh")
	bd := FakeBin(t, "bd", "echo bd")
	work := t.TempDir()
	runShim(t, work, "gh", "", "pr", "create")
	runShim(t, work, "bd", "", "ready")
	runShim(t, work, "gh", "", "pr", "view")

	require.Equal(t, [][]string{{"pr", "create"}, {"pr", "view"}}, gh.ArgLists(t))
	require.Equal(t, [][]string{{"ready"}}, bd.ArgLists(t))
	require.Equal(t, "gh", gh.Invocations(t)[0].Name)
	require.Equal(t, "bd", bd.Invocations(t)[0].Name)
}

func TestFakeBinRejectsPathLikeNames(t *testing.T) {
	// A fatal call would abort this test, so exercise the validation through
	// a nested testing.T that we can observe.
	sub := &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		FakeBin(sub, "dir/name", "exit 0")
	}()
	require.True(t, sub.failed, "a name containing a path separator must fail")
}

// recordingTB captures Fatalf instead of aborting so validation paths can be
// tested; it panics to unwind the way t.FailNow would.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Fatalf(string, ...any) {
	r.failed = true
	panic("recordingTB: fatal")
}

func (r *recordingTB) Helper() {}
