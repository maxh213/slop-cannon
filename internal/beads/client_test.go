package beads

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// clientStage copies this package's captured samples into a temp directory
// and installs each override sample under the given key, the way
// testutil.StageBDSamples does for the shared samples.
func clientStage(t *testing.T, overrides map[string]string) string {
	t.Helper()
	src := envSamplesDir(t)
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644))
	}
	exts := []string{".json", ".jsonl", ".txt", ".stderr", ".exit"}
	for key, sample := range overrides {
		for _, ext := range exts {
			_ = os.Remove(filepath.Join(dir, key+ext))
		}
		found := false
		for _, ext := range exts {
			data, err := os.ReadFile(filepath.Join(src, sample+ext))
			if err != nil {
				continue
			}
			found = true
			require.NoError(t, os.WriteFile(filepath.Join(dir, key+ext), data, 0o644))
		}
		require.True(t, found, "no sample named %q", sample)
	}
	return dir
}

// clientFake installs a FakeBD shim replaying the captured samples (with
// overrides) and returns a client pointing at a fake repository path.
func clientFake(t *testing.T, overrides map[string]string) (*Client, *testutil.Shim) {
	t.Helper()
	envIsolate(t)
	bd := testutil.FakeBD(t, clientStage(t, overrides))
	c := New("/tmp/fixture-repo", Options{Actor: "sc:test", Timeout: 20 * time.Second})
	return c, bd
}

// clientLastArgs returns the arguments of the last shim call.
func clientLastArgs(t *testing.T, bd *testutil.Shim) []string {
	t.Helper()
	calls := bd.ArgLists(t)
	require.NotEmpty(t, calls)
	return calls[len(calls)-1]
}

func TestNewDefaults(t *testing.T) {
	c := New("/repo", Options{})
	assert.Equal(t, "/repo", c.Repo())
	assert.Equal(t, DefaultActor, c.Actor())
	assert.Equal(t, DefaultTimeout, c.timeout)
	assert.NotNil(t, c.logger)

	spec := Spec(c.options(nil), "ready", "--json")
	assert.Equal(t, []string{"-C", "/repo", "ready", "--json"}, spec.Args)
	assert.Equal(t, "1", spec.Env["BD_JSON_ENVELOPE"])
	assert.Equal(t, "1", spec.Env["BD_NON_INTERACTIVE"])
	assert.Equal(t, DefaultActor, spec.Env["BEADS_ACTOR"])

	ro := New("/repo", Options{ReadOnly: true, Actor: "sc:agent"})
	assert.Equal(t, []string{"--readonly", "-C", "/repo", "show", "x"}, Spec(ro.options(nil), "show", "x").Args)
}

func TestWithActorSharesTheQueue(t *testing.T) {
	c := New("/repo", Options{Actor: "sc:orch"})
	a := c.WithActor("sc:engineer-1")
	assert.Equal(t, "sc:engineer-1", a.Actor())
	assert.Equal(t, "sc:orch", c.Actor(), "the original is unchanged")
	assert.Same(t, c.mu, a.mu, "derived clients share the mutex")
	assert.Same(t, c, c.WithActor(""), "empty actor returns the same client")
	assert.Same(t, c, c.WithActor("sc:orch"))
}

func TestCallsAreSerialised(t *testing.T) {
	c := New("/repo", Options{Actor: "sc:test"})
	var active, peak, calls atomic.Int32
	c.exec = func(_ context.Context, _ proc.Spec) (proc.Result, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		calls.Add(1)
		return proc.Result{Stdout: `{"data":[],"schema_version":1}`}, nil
	}
	agent := c.WithActor("sc:engineer-1")
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 4 {
			case 0:
				_, err = c.List(context.Background(), ListFilter{})
			case 1:
				_, err = c.Ready(context.Background(), "")
			case 2:
				_, err = agent.Comments(context.Background(), "fx-1")
			default:
				_, err = c.Blocked(context.Background())
			}
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int32(30), calls.Load())
	assert.Equal(t, int32(1), peak.Load(), "two bd calls overlapped")
}

func TestCallsAreSerialisedThroughRealProcesses(t *testing.T) {
	envIsolate(t)
	// The shim notes any invocation that starts while another is running.
	bd := testutil.FakeBin(t, "bd", `if ! mkdir "$SC_FAKEBIN_DIR/.busy" 2>/dev/null; then echo overlap >> "$SC_FAKEBIN_DIR/overlap"; fi
sleep 0.02
rmdir "$SC_FAKEBIN_DIR/.busy"
echo '{"data":[],"schema_version":1}'`)
	c := New("/repo", Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.List(context.Background(), ListFilter{All: true})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Len(t, bd.ArgLists(t), 8)
	_, err := os.Stat(filepath.Join(bd.Dir, "overlap"))
	assert.True(t, os.IsNotExist(err), "bd processes overlapped")
}

func TestRunHonoursContext(t *testing.T) {
	c := New("/repo", Options{})
	c.exec = func(context.Context, proc.Spec) (proc.Result, error) {
		t.Fatal("exec must not run with a cancelled context")
		return proc.Result{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Ping(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCreate(t *testing.T) {
	c, bd := clientFake(t, nil)
	is, err := c.Create(context.Background(), BeadInput{
		Title: "Add Median", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault,
		Description: "README promises Median", Acceptance: "TestMedian passes",
		Labels:   []string{"sc:run:r1", "sc:role:engineer"},
		Metadata: map[string]any{"sc_run": "r1", "sc_round": 0, "sc_kind": "work"},
		Parent:   "fx-nvi",
	})
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-nvi.1"), is.ID)
	assert.Equal(t, "open", is.Status)
	assert.Equal(t, []string{"sc:run:r1", "sc:role:engineer"}, is.Labels)
	round, ok := is.Metadata.Int(MetaRound)
	assert.True(t, ok)
	assert.Zero(t, round)

	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "create", "Add Median", "-t", "task", "-p", "2",
		"--description", "README promises Median", "--acceptance", "TestMedian passes",
		"-l", "sc:run:r1,sc:role:engineer"}, args[:14])
	assert.Equal(t, "--metadata", args[14])
	assert.True(t, strings.HasPrefix(args[15], "@"), "metadata goes through @file: %q", args[15])
	assert.Equal(t, []string{"--parent", "fx-nvi", "--json"}, args[16:])
	_, statErr := os.Stat(strings.TrimPrefix(args[15], "@"))
	assert.True(t, os.IsNotExist(statErr), "the metadata temp file is removed")
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

// TestCreateExternalRef covers P3-19: a review bead of an external pull
// request (D-11) names the pull request with bd's own `--external-ref`
// beside the sc_external_ref metadata, and bd echoes it back on the issue.
// The sample is the real bd 1.2.2 answer (create-external-ref.json).
func TestCreateExternalRef(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"create": "create-external-ref"})
	is, err := c.Create(context.Background(), BeadInput{
		Title: "Review PR #9: Add Median", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault,
		Description: "External pull request review",
		ExternalRef: "gh-9",
		Labels:      []string{"sc:run:r1", "sc:external-pr"},
		Metadata:    map[string]any{MetaRun: "r1", MetaKind: "review", "sc_external_ref": "gh-9"},
	})
	require.NoError(t, err)
	assert.Equal(t, "gh-9", is.ExternalRef, "bd echoes external_ref on the created issue")
	ref, ok := is.Metadata.String("sc_external_ref")
	assert.True(t, ok)
	assert.Equal(t, "gh-9", ref, "the metadata sc already wrote is unchanged")

	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "create", "Review PR #9: Add Median", "-t", "task", "-p", "2",
		"--description", "External pull request review", "--external-ref", "gh-9",
		"-l", "sc:run:r1,sc:external-pr"}, args[:14])
	assert.Equal(t, "--json", args[len(args)-1])

	// No reference, no flag.
	c2, bd2 := clientFake(t, nil)
	_, err = c2.Create(context.Background(), BeadInput{Title: "X", Priority: domain.PriorityDefault})
	require.NoError(t, err)
	assert.NotContains(t, clientLastArgs(t, bd2), "--external-ref")
}

func TestCreateDefaultsAndValidation(t *testing.T) {
	c, bd := clientFake(t, nil)
	_, err := c.Create(context.Background(), BeadInput{Title: "  "})
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, bd.ArgLists(t), "nothing is run for an empty title")

	_, err = c.Create(context.Background(), BeadInput{Title: "X", Priority: domain.PriorityDefault})
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "create", "X", "-t", "task", "-p", "2", "--json"}, clientLastArgs(t, bd))
}

func TestCreateGraph(t *testing.T) {
	c, bd := clientFake(t, nil)
	plan := GraphPlan{
		CommitMessage: "sc run r1",
		Nodes: []GraphNode{
			{Key: "epic", Title: "Run r1", Type: domain.BeadTypeEpic, Priority: 1, Labels: []string{"sc:run:r1"}, Metadata: map[string]any{"sc_run": "r1"}},
			{Key: "mean", Title: "Fix Mean", Type: domain.BeadTypeBug, Priority: 1, ParentKey: "epic"},
			{Key: "max", Title: "Fix Max", Type: domain.BeadTypeBug, Priority: 2, ParentKey: "epic"},
		},
		Edges: []GraphEdge{{FromKey: "max", ToKey: "mean", Type: DepBlocks}},
	}
	dry, err := c.CreateGraph(context.Background(), plan, true)
	require.NoError(t, err)
	assert.True(t, dry.DryRun)
	assert.Equal(t, 3, dry.NodeCount)
	assert.Equal(t, 1, dry.EdgeCount)
	assert.Equal(t, 2, dry.ParentDeps)
	assert.Len(t, dry.Nodes, 3)
	assert.Empty(t, dry.IDs)
	assert.Empty(t, dry.Warnings)
	assert.NotEmpty(t, dry.ValidationNotes)
	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "create", "--graph"}, args[:4])
	assert.Equal(t, []string{"--dry-run", "--json"}, args[5:])

	real, err := c.CreateGraph(context.Background(), plan, false)
	require.NoError(t, err)
	assert.False(t, real.DryRun)
	assert.Equal(t, map[string]domain.BeadID{"epic": "fx-nvi", "mean": "fx-so0", "max": "fx-ctt"}, real.IDs)
	args = clientLastArgs(t, bd)
	assert.Equal(t, "--json", args[len(args)-1])
	assert.NotContains(t, args, "--dry-run")

	// The plan file bd was given holds the edges (D-24) and is removed after.
	inv := bd.Invocations(t)
	planPath := inv[len(inv)-1].Args[4]
	_, statErr := os.Stat(planPath)
	assert.True(t, os.IsNotExist(statErr))

	_, err = c.CreateGraph(context.Background(), GraphPlan{}, true)
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestCreateGraphPlanFileShape(t *testing.T) {
	envIsolate(t)
	// A shim that copies the plan file it was handed, so the JSON bd sees
	// can be checked.
	bd := testutil.FakeBin(t, "bd", `while [ $# -gt 0 ]; do if [ "$1" = --graph ]; then cp "$2" "$SC_FAKEBIN_DIR/plan.json"; fi; shift; done
echo '{"data":{"ids":{"a":"fx-1"}},"schema_version":1}'`)
	c := New("/repo", Options{})
	_, err := c.CreateGraph(context.Background(), GraphPlan{
		Nodes: []GraphNode{{Key: "a", Title: "A", Priority: domain.PriorityDefault}, {Key: "b", Title: "B", Priority: 0, ParentKey: "a"}},
		Edges: []GraphEdge{{FromKey: "b", ToKey: "a"}},
	}, false)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(bd.Dir, "plan.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"nodes":[{"key":"a","title":"A","priority":2},{"key":"b","title":"B","priority":0,"parent_key":"a"}],"edges":[{"from_key":"b","to_key":"a"}]}`, string(data))
}

func TestCreateGraphWarnings(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"create-graph-dry-run": "create-graph-dry-run-warning"})
	res, err := c.CreateGraph(context.Background(), GraphPlan{Nodes: []GraphNode{{Key: "a", Title: "A"}}}, true)
	require.NoError(t, err)
	assert.Equal(t, 0, res.EdgeCount)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "unknown field(s): [bogus_field deps]")

	assert.Empty(t, GraphWarnings(envSample(t, "create.stderr")), "the beads.role notice is not a plan warning")
	assert.Equal(t, []string{"warning: x"}, GraphWarnings("warning: beads.role not configured\n  Fix: y\nwarning: x\n"))
}

func TestShow(t *testing.T) {
	c, bd := clientFake(t, nil)
	issues, err := c.Show(context.Background(), "fx-so0")
	require.NoError(t, err)
	require.Len(t, issues, 1, "bd show answers with an array even for one id")
	is := issues[0]
	assert.Equal(t, domain.BeadID("fx-so0"), is.ID)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:engineer-1", is.Assignee)
	assert.Equal(t, domain.BeadID("fx-nvi"), is.ParentID())
	require.Len(t, is.Comments, 2)
	assert.Equal(t, "sc:engineer-1", is.Comments[0].Author)
	assert.Contains(t, is.Comments[0].Text, `"type":"impl_report"`)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "show", "fx-so0", "--json", "--include-comments"}, clientLastArgs(t, bd))

	_, err = c.Show(context.Background())
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestShowMulti(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"show": "show-multi"})
	issues, err := c.Show(context.Background(), "fx-so0", "fx-ctt")
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-so0", "fx-ctt"}, IssueIDs(issues))
	assert.Equal(t, []domain.BeadID{"fx-so0"}, issues[1].Blockers())
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "show", "fx-so0", "fx-ctt", "--json", "--include-comments"}, clientLastArgs(t, bd))
}

func TestShowNotFound(t *testing.T) {
	t.Run("single unknown id", func(t *testing.T) {
		c, _ := clientFake(t, map[string]string{"show": "error-not-found"})
		issues, err := c.Show(context.Background(), "nope-1")
		assert.ErrorIs(t, err, ErrNotFound)
		assert.Empty(t, issues)
		var be *Error
		require.ErrorAs(t, err, &be)
		assert.Equal(t, "no issues found matching the provided IDs", be.Message)
		assert.Equal(t, 1, be.ExitCode)
		assert.Equal(t, "bd show: bead not found: no issues found matching the provided IDs", err.Error())
	})
	t.Run("some ids found", func(t *testing.T) {
		c, _ := clientFake(t, map[string]string{"show": "error-not-found-mixed"})
		issues, err := c.Show(context.Background(), "fx-so0", "nope-1")
		assert.ErrorIs(t, err, ErrNotFound, "bd exits 0 but reports the missing id on stderr")
		assert.Equal(t, []domain.BeadID{"fx-so0"}, IssueIDs(issues), "the found issues are still returned")
		var be *Error
		require.ErrorAs(t, err, &be)
		assert.Equal(t, `Error fetching nope-1: no issue found matching "nope-1"`, be.Message)
		assert.Equal(t, 0, be.ExitCode)
	})
}

func TestList(t *testing.T) {
	c, bd := clientFake(t, nil)
	issues, err := c.List(context.Background(), ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-nvi", "fx-so0", "fx-ctt", "fx-nvi.1", "fx-nvi.2"}, IssueIDs(issues))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "list", "--json", "-n", "0", "--flat"}, clientLastArgs(t, bd), "listings always disable the limit and the tree")

	_, err = c.List(context.Background(), ListFilter{
		All:      true,
		Statuses: []domain.BeadState{domain.BeadStateOpen, domain.BeadStateInReview},
		Labels:   []string{"sc:run:r1"},
		Parent:   "fx-nvi",
		Metadata: map[string]string{"sc_run": "r1", "sc_kind": "work"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "list", "--json", "-n", "0", "--flat", "--all",
		"-s", "open,in_review", "-l", "sc:run:r1", "--parent", "fx-nvi",
		"--metadata-field", "sc_kind=work", "--metadata-field", "sc_run=r1"}, clientLastArgs(t, bd))
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestListAllIncludesClosed(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"list": "list-all"})
	issues, err := c.List(context.Background(), ListFilter{All: true})
	require.NoError(t, err)
	var closed []domain.BeadID
	for _, is := range issues {
		if is.Status == "closed" {
			closed = append(closed, is.ID)
		}
	}
	assert.Equal(t, []domain.BeadID{"fx-so0"}, closed)
}

func TestReadyFiltersTheEpic(t *testing.T) {
	c, bd := clientFake(t, nil)
	issues, err := c.Ready(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, IssueIDs(issues), "bd ready lists the epic fx-nvi itself; Ready drops it")
	for _, is := range issues {
		assert.Equal(t, "open", is.Status)
	}
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "ready", "--json", "-n", "0"}, clientLastArgs(t, bd))

	c2, bd2 := clientFake(t, map[string]string{"ready": "ready-parent"})
	issues, err = c2.Ready(context.Background(), "fx-nvi")
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, IssueIDs(issues))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "ready", "--json", "-n", "0", "--parent", "fx-nvi"}, clientLastArgs(t, bd2))

	c3, _ := clientFake(t, map[string]string{"ready": "ready-after-close"})
	issues, err = c3.Ready(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-ctt", "fx-nvi.2"}, IssueIDs(issues), "closing the blocker made fx-ctt ready")
}

func TestBlocked(t *testing.T) {
	c, bd := clientFake(t, nil)
	issues, err := c.Blocked(context.Background())
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, domain.BeadID("fx-ctt"), issues[0].ID)
	assert.Equal(t, []domain.BeadID{"fx-so0"}, issues[0].BlockedBy)
	assert.Equal(t, []domain.BeadID{"fx-so0"}, issues[0].Blockers())
	assert.Equal(t, 1, issues[0].BlockedByCount)
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, issues[1].BlockedBy)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "blocked", "--json"}, clientLastArgs(t, bd))
}

func TestUpdate(t *testing.T) {
	c, bd := clientFake(t, nil)
	is, err := c.Update(context.Background(), "fx-ctt", Patch{
		Status:      domain.BeadStateInReview,
		SetMetadata: map[string]string{"sc_round": "1", "sc_agent": "engineer-1"},
		AddLabels:   []string{"sc:role:engineer"},
	})
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-ctt"), is.ID)
	assert.Equal(t, "in_review", is.Status)
	round, ok := is.Metadata.Int(MetaRound)
	assert.True(t, ok)
	assert.Equal(t, 1, round, "bd stores --set-metadata sc_round=1 as a JSON number")
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "-s", "in_review",
		"--set-metadata", "sc_agent=engineer-1", "--set-metadata", "sc_round=1",
		"--add-label", "sc:role:engineer", "--json"}, clientLastArgs(t, bd))
}

func TestUpdateEveryFlag(t *testing.T) {
	c, bd := clientFake(t, nil)
	empty := ""
	prio := domain.PriorityLowest
	acc := "TestMax passes for negatives"
	parent := domain.BeadID("fx-nvi")
	_, err := c.Update(context.Background(), "fx-ctt", Patch{
		Status:        domain.BeadStateOpen,
		Assignee:      &empty,
		Priority:      &prio,
		Acceptance:    &acc,
		Parent:        &parent,
		Metadata:      map[string]any{"sc_pr_number": 7},
		UnsetMetadata: []string{"sc_lease"},
		RemoveLabels:  []string{"sc:role:engineer"},
	})
	require.NoError(t, err)
	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "-s", "open", "--assignee", "", "-p", "4",
		"--acceptance", acc, "--parent", "fx-nvi", "--metadata"}, args[:15])
	assert.True(t, strings.HasPrefix(args[15], "@"))
	assert.Equal(t, []string{"--unset-metadata", "sc_lease", "--remove-label", "sc:role:engineer", "--json"}, args[16:])

	// A pointer to the empty string is how bd 1.2.2 spells "no parent".
	orphan := domain.BeadID("")
	_, err = c.Update(context.Background(), "fx-ctt", Patch{Parent: &orphan})
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "--parent", "", "--json"}, clientLastArgs(t, bd))

	_, err = c.Update(context.Background(), "fx-ctt", Patch{})
	assert.ErrorIs(t, err, ErrInvalid)
	assert.True(t, Patch{}.IsEmpty())
	assert.False(t, Patch{AddLabels: []string{"x"}}.IsEmpty())
	assert.False(t, Patch{Parent: &orphan}.IsEmpty(), "reparenting is a change")
}

func TestUpdateReleaseAndAcceptance(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"update": "update-release"})
	empty := ""
	is, err := c.Update(context.Background(), "fx-ctt", Patch{Status: domain.BeadStateOpen, Assignee: &empty})
	require.NoError(t, err)
	assert.Equal(t, "open", is.Status)
	assert.Empty(t, is.Assignee)

	c2, _ := clientFake(t, map[string]string{"update": "update-acceptance"})
	acc := "TestMax passes for negatives"
	is, err = c2.Update(context.Background(), "fx-ctt", Patch{Acceptance: &acc})
	require.NoError(t, err)
	assert.Equal(t, acc, is.Acceptance)
}

func TestUpdateErrors(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"update": "error-update-not-found"})
	_, err := c.Update(context.Background(), "nope-1", Patch{Status: domain.BeadStateOpen})
	assert.ErrorIs(t, err, ErrNotFound)

	c2, _ := clientFake(t, map[string]string{"update": "error-bad-status"})
	_, err = c2.Update(context.Background(), "fx-ctt", Patch{Status: "not_a_status"})
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), `invalid status "not_a_status"`)

	c3, _ := clientFake(t, map[string]string{"update": "error-readonly"})
	_, err = c3.Update(context.Background(), "fx-ctt", Patch{Status: domain.BeadStateOpen})
	assert.ErrorIs(t, err, ErrReadOnly)
	assert.Equal(t, "bd update: bd is in read-only mode: operation 'update' is not allowed in read-only mode", err.Error())
}

func TestClaim(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"update": "update-claim"})
	is, err := c.Claim(context.Background(), "fx-so0", "sc:engineer-1")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:engineer-1", is.Assignee)
	assert.False(t, is.StartedAt.IsZero())
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-so0", "--claim", "--json"}, clientLastArgs(t, bd))

	// The claim runs as the agent, not the orchestrator.
	c2 := c.WithActor("sc:engineer-1")
	assert.Equal(t, "sc:engineer-1", Spec(c2.options(nil), "update").Env["BEADS_ACTOR"])
	assert.Equal(t, "sc:test", Spec(c.options(nil), "update").Env["BEADS_ACTOR"])

	c3, _ := clientFake(t, map[string]string{"update": "update-claim-again"})
	_, err = c3.Claim(context.Background(), "fx-so0", "sc:engineer-1")
	assert.NoError(t, err, "a repeated claim by the same actor is idempotent")

	c4, _ := clientFake(t, map[string]string{"update": "error-claim-taken"})
	_, err = c4.Claim(context.Background(), "fx-so0", "sc:engineer-2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed)
	assert.Contains(t, err.Error(), "already claimed by sc:engineer-1")
}

func TestClaimActorReachesTheProcess(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBin(t, "bd", `echo "$BEADS_ACTOR" > "$SC_FAKEBIN_DIR/actor"
echo '{"data":[{"id":"fx-1","status":"in_progress","assignee":"'"$BEADS_ACTOR"'"}],"schema_version":1}'`)
	c := New("/repo", Options{Actor: "sc:orch"})
	is, err := c.Claim(context.Background(), "fx-1", "sc:engineer-3")
	require.NoError(t, err)
	assert.Equal(t, "sc:engineer-3", is.Assignee)
	actor, err := os.ReadFile(filepath.Join(bd.Dir, "actor"))
	require.NoError(t, err)
	assert.Equal(t, "sc:engineer-3", strings.TrimSpace(string(actor)))
}

func TestClose(t *testing.T) {
	c, bd := clientFake(t, nil)
	is, err := c.Close(context.Background(), "fx-so0", "fixed in round 1")
	require.NoError(t, err)
	assert.Equal(t, "closed", is.Status)
	assert.Equal(t, "fixed in round 1", is.CloseReason)
	assert.False(t, is.ClosedAt.IsZero())
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "close", "fx-so0", "--reason", "fixed in round 1", "--json"}, clientLastArgs(t, bd))

	c2, _ := clientFake(t, map[string]string{"close": "close-again"})
	_, err = c2.Close(context.Background(), "fx-so0", "")
	assert.NoError(t, err, "closing a closed bead is a no-op")
}

func TestCloseRefusedWhileBlockersAreOpen(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"close": "error-close-blocked"})
	_, err := c.Close(context.Background(), "fx-nvi.1", "done")
	require.ErrorIs(t, err, ErrBlockedOpen)
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Equal(t, "cannot close fx-nvi.1: blocked by open issues [fx-nvi.2] (use --force to override)", be.Message,
		"the message survives the metrics preamble the shim prints on its first call")
	assert.Equal(t, "close", be.Command)
	assert.Equal(t, 1, be.ExitCode)
	assert.Empty(t, be.Stdout, "bd prints nothing on stdout for this refusal")
	assert.Contains(t, be.Stderr, "Thanks for using bd", "the first call carried the preamble")
	var exit *proc.ExitError
	assert.ErrorAs(t, err, &exit, "the proc error is reachable")
	assert.Len(t, bd.ArgLists(t), 1)

	c2, _ := clientFake(t, map[string]string{"close": "error-close-not-found"})
	_, err = c2.Close(context.Background(), "nope-1", "x")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestComment(t *testing.T) {
	c, bd := clientFake(t, nil)
	body := "Implementation report\n\n```json\n{\"type\":\"impl_report\"}\n```\n"
	cm, err := c.WithActor("sc:engineer-1").Comment(context.Background(), "fx-so0", body)
	require.NoError(t, err)
	assert.Equal(t, "01a0636a-ad0d-7bd0-8fe1-9ddb12501edd", cm.ID)
	assert.Equal(t, domain.BeadID("fx-so0"), cm.IssueID)
	assert.Equal(t, "sc:engineer-1", cm.Author)
	assert.Contains(t, cm.Text, "impl_report")
	assert.False(t, cm.CreatedAt.IsZero())
	inv := bd.Invocations(t)
	require.Len(t, inv, 1)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "comment", "fx-so0", "--stdin", "--json"}, inv[0].Args)
	assert.Equal(t, body, inv[0].Stdin, "the body travels on stdin")

	_, err = c.Comment(context.Background(), "fx-so0", " \n")
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, bd.ArgLists(t), 1, "an empty body is rejected before running bd")
}

func TestCommentErrors(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"comment": "error-comment-not-found"})
	_, err := c.Comment(context.Background(), "nope-1", "x")
	assert.ErrorIs(t, err, ErrNotFound)

	c2, _ := clientFake(t, map[string]string{"comment": "error-readonly-comment"})
	_, err = c2.Comment(context.Background(), "fx-so0", "x")
	assert.ErrorIs(t, err, ErrReadOnly)

	c3, _ := clientFake(t, map[string]string{"comment": "error-comment-empty"})
	_, err = c3.Comment(context.Background(), "fx-so0", "x")
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestComments(t *testing.T) {
	c, bd := clientFake(t, nil)
	cms, err := c.Comments(context.Background(), "fx-so0")
	require.NoError(t, err)
	require.Len(t, cms, 2)
	assert.Equal(t, "sc:engineer-1", cms[0].Author)
	assert.Equal(t, "sc", cms[1].Author)
	assert.True(t, cms[0].CreatedAt.Before(cms[1].CreatedAt) || cms[0].CreatedAt.Equal(cms[1].CreatedAt))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "comments", "fx-so0", "--json"}, clientLastArgs(t, bd))

	c2, _ := clientFake(t, map[string]string{"comments": "comments-empty"})
	cms, err = c2.Comments(context.Background(), "fx-ctt")
	require.NoError(t, err)
	assert.Empty(t, cms)
}

func TestAddDep(t *testing.T) {
	c, bd := clientFake(t, nil)
	res, err := c.AddDep(context.Background(), "fx-nvi.1", "fx-nvi.2")
	require.NoError(t, err)
	assert.Equal(t, DepResult{IssueID: "fx-nvi.1", DependsOnID: "fx-nvi.2", Status: "added", Type: DepBlocks}, res)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "dep", "add", "fx-nvi.1", "fx-nvi.2", "--json"}, clientLastArgs(t, bd))

	_, err = c.AddDep(context.Background(), "fx-nvi.1", "")
	assert.ErrorIs(t, err, ErrInvalid)

	c2, _ := clientFake(t, map[string]string{"dep-add": "error-dep-cycle"})
	_, err = c2.AddDep(context.Background(), "fx-nvi.2", "fx-nvi.1")
	assert.ErrorIs(t, err, ErrCycle)
	assert.Equal(t, "bd dep add: dependency would create a cycle: adding dependency would create a cycle", err.Error())

	c3, _ := clientFake(t, map[string]string{"dep-add": "error-dep-not-found"})
	res, err = c3.AddDep(context.Background(), "fx-nvi.1", "nope-1")
	assert.NoError(t, err, "bd 1.2.2 accepts an unknown depends-on id (documented quirk)")
	assert.Equal(t, domain.BeadID("nope-1"), res.DependsOnID)
}

func TestDepList(t *testing.T) {
	c, bd := clientFake(t, nil)
	deps, err := c.DepList(context.Background(), "fx-nvi.1")
	require.NoError(t, err)
	require.Len(t, deps, 2)
	assert.Equal(t, domain.BeadID("fx-nvi.1"), deps[0].IssueID, "the issue form is normalised to the asked id")
	assert.Equal(t, domain.BeadID("fx-nvi.2"), deps[0].DependsOnID)
	assert.Equal(t, DepBlocks, deps[0].Type)
	assert.Equal(t, "Write Median tests", deps[0].Title)
	assert.Equal(t, domain.BeadID("fx-nvi"), deps[1].DependsOnID)
	assert.Equal(t, DepParentChild, deps[1].Type)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "dep", "list", "fx-nvi.1", "--json"}, clientLastArgs(t, bd))

	c2, bd2 := clientFake(t, map[string]string{"dep-list": "dep-list-multi"})
	deps, err = c2.DepList(context.Background(), "fx-nvi.1", "fx-ctt")
	require.NoError(t, err)
	require.Len(t, deps, 4)
	assert.Equal(t, Dependency{IssueID: "fx-nvi.1", DependsOnID: "fx-nvi.2", Type: DepBlocks, CreatedAt: deps[0].CreatedAt, CreatedBy: "sc"}, deps[0])
	assert.Equal(t, domain.BeadID("fx-ctt"), deps[3].IssueID)
	assert.Equal(t, domain.BeadID("fx-so0"), deps[3].DependsOnID)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "dep", "list", "fx-nvi.1", "fx-ctt", "--json"}, clientLastArgs(t, bd2))

	_, err = c.DepList(context.Background())
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestExport(t *testing.T) {
	c, bd := clientFake(t, nil)
	issues, err := c.Export(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-nvi", "fx-so0", "fx-ctt", "fx-nvi.1", "fx-nvi.2"}, IssueIDs(issues))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "export"}, clientLastArgs(t, bd))
	byID := map[domain.BeadID]Issue{}
	for _, is := range issues {
		byID[is.ID] = is
	}
	assert.Equal(t, "closed", byID["fx-so0"].Status, "exports include closed issues")
	assert.Len(t, byID["fx-so0"].Comments, 2, "exports carry comments")
	assert.Equal(t, []domain.BeadID{"fx-so0"}, byID["fx-ctt"].Blockers(), "exports carry dependencies in edge form")
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, byID["fx-nvi.1"].Blockers())
	assert.Equal(t, []string{"sc:run:r1"}, byID["fx-nvi"].Labels)
}

func TestParseExport(t *testing.T) {
	issues, err := ParseExport(strings.NewReader("Thanks for using bd!\n\n{\"_type\":\"agent\",\"id\":\"a\"}\n{\"id\":\"x-1\",\"title\":\"no type field\"}\n{\"_type\":\"issue\",\"id\":\"x-2\",\"priority\":1}\n"))
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, domain.BeadID("x-1"), issues[0].ID)
	assert.Equal(t, domain.PriorityDefault, issues[0].Priority)
	assert.Equal(t, domain.Priority(1), issues[1].Priority)

	_, err = ParseExport(strings.NewReader("{not json\n"))
	assert.Error(t, err)
}

func TestStatuses(t *testing.T) {
	c, bd := clientFake(t, nil)
	list, err := c.Statuses(context.Background())
	require.NoError(t, err)
	assert.Len(t, list.BuiltIn, 7)
	assert.Len(t, list.Custom, 7)
	assert.True(t, list.Has("open"))
	assert.True(t, list.Has("in_review"))
	assert.False(t, list.Has("nonsense"))
	custom := list.CustomStatuses()
	assert.Contains(t, custom, CustomStatus{Name: "escalated", Category: domain.CategoryFrozen})
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "statuses", "--json"}, clientLastArgs(t, bd))

	c2, _ := clientFake(t, map[string]string{"statuses": "statuses-builtin"})
	list, err = c2.Statuses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list.Custom)
	for _, s := range domain.BeadStates() {
		if !s.Custom() {
			assert.True(t, list.Has(string(s)), "%s is a bd built-in", s)
		}
	}
}

func TestSetCustomStatusesAndConfig(t *testing.T) {
	c, bd := clientFake(t, nil)
	require.NoError(t, c.SetCustomStatuses(context.Background(), WorkflowCustomStatuses()))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "set", "status.custom",
		"in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen", "--json"},
		clientLastArgs(t, bd), "the D-07 list, in flow order")

	value, err := c.ConfigGet(context.Background(), "status.custom")
	require.NoError(t, err)
	assert.Equal(t, "in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen", value)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "get", "status.custom", "--json"}, clientLastArgs(t, bd))

	require.NoError(t, c.ConfigSet(context.Background(), "status.custom", "x:wip"))

	assert.Error(t, c.SetCustomStatuses(context.Background(), nil))
	assert.Error(t, c.SetCustomStatuses(context.Background(), []CustomStatus{{Name: "x", Category: "bogus"}}))
	assert.Len(t, bd.ArgLists(t), 3, "invalid input never reaches bd")

	c2, _ := clientFake(t, map[string]string{"config-get": "config-get-missing"})
	value, err = c2.ConfigGet(context.Background(), "sc.nothing")
	require.NoError(t, err)
	assert.Empty(t, value, "an unset key is an empty value, not an error")
}

func TestConfigSetChecksTheKey(t *testing.T) {
	envIsolate(t)
	testutil.FakeBin(t, "bd", `echo '{"data":{"key":"other","value":"v"},"schema_version":1}'`)
	c := New("/repo", Options{})
	err := c.ConfigSet(context.Background(), "status.custom", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `bd reported "other"`)
}

func TestClientPing(t *testing.T) {
	c, bd := clientFake(t, nil)
	ping, err := c.Ping(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", ping.Status)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "ping", "--json"}, clientLastArgs(t, bd))

	c2, _ := clientFake(t, map[string]string{"ping": "error-no-project"})
	_, err = c2.Ping(context.Background())
	assert.ErrorIs(t, err, ErrNoProject)
	assert.Equal(t, `bd ping: no beads project found: cannot use -C directory "/tmp": no beads project found`, err.Error())
}

func TestClientPingUnhealthy(t *testing.T) {
	envIsolate(t)
	testutil.FakeBin(t, "bd", `echo '{"data":{"status":"degraded"},"schema_version":1}'`)
	c := New("/repo", Options{})
	_, err := c.Ping(context.Background())
	var ce *CheckError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ReasonUnhealthy, ce.Reason)
}

func TestClientBDMissing(t *testing.T) {
	envIsolate(t)
	t.Setenv("PATH", t.TempDir())
	c := New("/repo", Options{})
	_, err := c.List(context.Background(), ListFilter{})
	require.Error(t, err)
	assert.True(t, proc.IsNotFound(err), "%v", err)
	assert.Equal(t, "bd list: not found on PATH", err.Error())
}

func TestClientTimeout(t *testing.T) {
	envIsolate(t)
	testutil.FakeBin(t, "bd", `sleep 5`)
	c := New("/repo", Options{Timeout: 50 * time.Millisecond})
	_, err := c.List(context.Background(), ListFilter{})
	require.Error(t, err)
	assert.ErrorIs(t, err, proc.ErrTimeout)
	assert.Equal(t, "bd list: timed out", err.Error())
	assert.Equal(t, "timed out", ErrorSummary(err))
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Nil(t, be.Kind)
}

func TestClientBadOutput(t *testing.T) {
	envIsolate(t)
	testutil.FakeBin(t, "bd", `echo "not json at all"`)
	c := New("/repo", Options{})
	_, err := c.Ready(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoJSON)
	assert.True(t, strings.HasPrefix(err.Error(), "bd ready: "), err.Error())

	testutil.FakeBin(t, "bd", `echo '{"data":[],"schema_version":1}'`)
	_, err = c.Close(context.Background(), "fx-1", "x")
	assert.ErrorIs(t, err, errNoIssue)
}

func TestClientLogsEveryCall(t *testing.T) {
	envIsolate(t)
	var buf strings.Builder
	bd := testutil.FakeBD(t, clientStage(t, map[string]string{"close": "error-close-blocked"}))
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := New("/tmp/fixture-repo", Options{Logger: logger})
	_, err := c.Ready(context.Background(), "")
	require.NoError(t, err)
	_, err = c.Close(context.Background(), "fx-nvi.1", "done")
	require.Error(t, err)
	log := buf.String()
	assert.Contains(t, log, "bd call")
	assert.Contains(t, log, "command=ready")
	assert.Contains(t, log, "bd call failed")
	assert.Contains(t, log, "blocked by open issues")
	assert.Len(t, bd.ArgLists(t), 2)
}

// TestEveryCapturedCommandDecodes replays each command sample the shim can
// serve and checks the client decodes it; a sample added to testdata/bd
// without a client path shows up here.
func TestEveryCapturedCommandDecodes(t *testing.T) {
	c, bd := clientFake(t, nil)
	ctx := context.Background()
	calls := []struct {
		name string
		fn   func() error
	}{
		{"create", func() error { _, err := c.Create(ctx, BeadInput{Title: "x", Priority: 2}); return err }},
		{"create-graph", func() error {
			_, err := c.CreateGraph(ctx, GraphPlan{Nodes: []GraphNode{{Key: "a", Title: "A"}}}, false)
			return err
		}},
		{"create-graph-dry-run", func() error {
			_, err := c.CreateGraph(ctx, GraphPlan{Nodes: []GraphNode{{Key: "a", Title: "A"}}}, true)
			return err
		}},
		{"show", func() error { _, err := c.Show(ctx, "fx-so0"); return err }},
		{"list", func() error { _, err := c.List(ctx, ListFilter{}); return err }},
		{"ready", func() error { _, err := c.Ready(ctx, ""); return err }},
		{"blocked", func() error { _, err := c.Blocked(ctx); return err }},
		{"update", func() error { _, err := c.Update(ctx, "fx-ctt", Patch{Status: domain.BeadStateInReview}); return err }},
		{"close", func() error { _, err := c.Close(ctx, "fx-so0", "done"); return err }},
		{"comment", func() error { _, err := c.Comment(ctx, "fx-so0", "hi"); return err }},
		{"comments", func() error { _, err := c.Comments(ctx, "fx-so0"); return err }},
		{"dep add", func() error { _, err := c.AddDep(ctx, "fx-nvi.1", "fx-nvi.2"); return err }},
		{"dep list", func() error { _, err := c.DepList(ctx, "fx-nvi.1"); return err }},
		{"export", func() error { _, err := c.Export(ctx); return err }},
		{"statuses", func() error { _, err := c.Statuses(ctx); return err }},
		{"config set", func() error { return c.ConfigSet(ctx, StatusCustomKey, "v") }},
		{"config get", func() error { _, err := c.ConfigGet(ctx, "k"); return err }},
		{"ping", func() error { _, err := c.Ping(ctx); return err }},
	}
	for _, call := range calls {
		assert.NoError(t, call.fn(), call.name)
	}
	assert.Empty(t, testutil.MissingSamples(t, bd))
	assert.Len(t, bd.ArgLists(t), len(calls))
	for _, args := range bd.ArgLists(t) {
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo"}, args[:2], "every call names the repo: %v", args)
	}
}
