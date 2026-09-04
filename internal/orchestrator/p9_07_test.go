package orchestrator

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-07's two guards, kept out of bench_test.go because neither is a
// measurement: one pins docs/performance.md to the benchmarks it describes,
// the other pins the invariant every reader of a finished run relies on.

// perfDocPath is the write-up the benchmarks in this package, in
// internal/tui and in internal/logging produce, relative to this package.
const perfDocPath = "../../docs/performance.md"

// perfRepoRoot is the module root, relative to this package: the paths
// docs/performance.md names are written from there.
const perfRepoRoot = "../.."

// perfDocFile matches a backticked benchmark file the document names.
var perfDocFile = regexp.MustCompile("`(internal/[a-z0-9_/]+_test\\.go)`")

// perfDocFunc finds the benchmark and budget-test functions in a bench file.
var perfDocFunc = regexp.MustCompile(`(?m)^func (Benchmark[A-Za-z0-9_]*)\(`)

// perfDocCode finds every backticked span in the document.
var perfDocCode = regexp.MustCompile("`([^`\n]+)`")

// perfDocCommand finds the `go test ./<pkg> -run '^$' -bench '<pattern>'`
// lines the document tells a reader to run.
var perfDocCommand = regexp.MustCompile(`go test \./(\S+) -run '\^\$' -bench '([^']+)'`)

// perfDocBenchmarks reads docs/performance.md and every benchmark file it
// names, and returns the document's text with the benchmark functions of
// each file, keyed by the path the document uses.
func perfDocBenchmarks(t *testing.T) (string, map[string][]string) {
	t.Helper()
	raw, err := os.ReadFile(perfDocPath)
	require.NoError(t, err, "docs/performance.md is the write-up P9-07's benchmarks feed")
	doc := string(raw)

	files := perfDocFile.FindAllStringSubmatch(doc, -1)
	require.NotEmpty(t, files, "docs/performance.md should name the files its benchmarks live in")

	out := make(map[string][]string, len(files))
	for _, m := range files {
		name := m[1]
		if _, seen := out[name]; seen {
			continue
		}
		b, err := os.ReadFile(filepath.Join(perfRepoRoot, name))
		require.NoErrorf(t, err, "docs/performance.md names %s", name)
		var funcs []string
		for _, f := range perfDocFunc.FindAllStringSubmatch(string(b), -1) {
			funcs = append(funcs, f[1])
		}
		sort.Strings(funcs)
		require.NotEmptyf(t, funcs, "%s declares no benchmarks", name)
		out[name] = funcs
	}
	return doc, out
}

// TestPerformanceDocNamesBenchmarksThatExist is the cheap guard P9-07's
// write-up needs and did not have: docs/performance.md names the files its
// benchmarks live in, every benchmark in them and the commands that run
// them, and none of that is compiled, so renaming a benchmark or moving a
// file rots the document silently. This test fails instead. It works from
// the document outwards, so a lane that adds a benchmark file to the write-up
// is covered by it without touching this test.
func TestPerformanceDocNamesBenchmarksThatExist(t *testing.T) {
	doc, files := perfDocBenchmarks(t)

	t.Run("every benchmark that exists is in the doc", func(t *testing.T) {
		for file, funcs := range files {
			for _, name := range funcs {
				// The results tables name a benchmark without its
				// Benchmark prefix, and the prose with it; either counts.
				assert.Containsf(t, doc, strings.TrimPrefix(name, "Benchmark"),
					"%s declares %s; docs/performance.md should report it", file, name)
			}
		}
	})

	t.Run("every benchmark the doc names exists", func(t *testing.T) {
		named := 0
		for _, m := range perfDocCode.FindAllStringSubmatch(doc, -1) {
			name, _, _ := strings.Cut(m[1], "/")
			if !strings.HasPrefix(name, "Benchmark") {
				continue
			}
			named++
			found := false
			for _, funcs := range files {
				found = found || slices.Contains(funcs, name)
			}
			assert.Truef(t, found, "docs/performance.md names %q, which no benchmark file declares", m[1])
		}
		assert.Positive(t, named, "the doc should still name benchmarks by their function name")
	})

	t.Run("the doc's budget tests exist", func(t *testing.T) {
		for _, name := range []string{"TestGraphPerformanceBudgets", "TestOrchestratorPerformanceBudgets"} {
			assert.Containsf(t, doc, name, "docs/performance.md should still name %s", name)
		}
		assert.NotNil(t, TestOrchestratorPerformanceBudgets)
	})

	t.Run("the doc's commands run every benchmark", func(t *testing.T) {
		cmds := perfDocCommand.FindAllStringSubmatch(doc, -1)
		require.NotEmpty(t, cmds, "docs/performance.md should still say how to run the benchmarks")
		for _, cmd := range cmds {
			pkg, pattern := cmd[1], cmd[2]
			re, err := regexp.Compile(pattern)
			require.NoErrorf(t, err, "the -bench pattern for %s does not compile", pkg)
			for file, funcs := range files {
				if path.Dir(file) != pkg {
					continue
				}
				for _, name := range funcs {
					assert.Truef(t, re.MatchString(name),
						"`-bench '%s'` in docs/performance.md does not select %s from %s",
						pattern, name, file)
				}
			}
		}
	})
}

// TestRunPublishesNothingAfterTheTerminator pins the invariant `sc status`,
// `sc logs` and resume all read a finished run with: [events.RunFinished] is
// the last line of the run log, and state.json's LastSeq counts the whole of
// it. Before P9-07 a conductor pass that was still in flight when the run
// finished could publish an [events.GraphChanged] after the terminator,
// which left the log one line longer than the frozen state.
func TestRunPublishesNothingAfterTheTerminator(t *testing.T) {
	var mu sync.Mutex
	var got []events.Kind
	run := perfRun(t, config.MinAmmo, PublisherFunc(func(ev events.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev.Kind())
	}))

	run.Publish(events.AgentOutput{Meta: run.Meta(), Agent: domain.AgentID("eng-sc-1-r1"),
		Stream: events.StreamStdout, Line: "still working"})
	run.Publish(events.RunFinished{Meta: run.Meta(), Reason: events.FinishCompleted, Summary: "done"})
	// Exactly the shape of the leak: a conductor pass that had already
	// started announcing when the run finished.
	run.Publish(events.GraphChanged{Meta: run.Meta(), Epic: "sc-epic"})
	run.Warn(SourceRun, "a warning raised after the run finished", nil)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2, "nothing may be published after the terminator")
	assert.Equal(t, events.KindRunFinished, got[len(got)-1], "the log closes with RunFinished")

	st := run.State()
	assert.Equal(t, uint64(2), st.LastSeq, "state.json counts the whole log and no more")
	assert.Equal(t, events.KindRunFinished, st.LastEventKind)
}
