package beads

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// typesIssues decodes a captured array sample into issues.
func typesIssues(t *testing.T, sample string) []Issue {
	t.Helper()
	var issues []Issue
	require.NoError(t, Decode(envSample(t, sample), &issues))
	return issues
}

// typesIssue returns the issue with the given id from a captured sample.
func typesIssue(t *testing.T, sample string, id domain.BeadID) Issue {
	t.Helper()
	for _, is := range typesIssues(t, sample) {
		if is.ID == id {
			return is
		}
	}
	t.Fatalf("no issue %s in %s", id, sample)
	return Issue{}
}

func TestMetadataAcceptsNumbersAndStrings(t *testing.T) {
	// P3-13 (c): --set-metadata sc_round=1 lands as a JSON number, other
	// writers store strings; both must read back.
	is := typesIssue(t, "update-set-metadata-types.json", "fx-ctt")
	m := is.Metadata
	round, ok := m.Int(MetaRound)
	assert.True(t, ok)
	assert.Equal(t, 1, round)
	s, ok := m.String(MetaRound)
	assert.True(t, ok)
	assert.Equal(t, "1", s, "a whole number renders without a fraction")

	n, ok := m.Int(MetaPRNumber)
	assert.True(t, ok)
	assert.Equal(t, 7, n)
	url, ok := m.String(MetaPRURL)
	assert.True(t, ok)
	assert.Equal(t, "https://example.com/pr/7", url)

	b, ok := m.Bool("sc_flag")
	assert.True(t, ok)
	assert.True(t, b)
	s, ok = m.String("sc_flag")
	assert.True(t, ok)
	assert.Equal(t, "true", s)

	s, ok = m.String("sc_float")
	assert.True(t, ok)
	assert.Equal(t, "1.5", s)
	_, ok = m.Int("sc_float")
	assert.False(t, ok, "a fraction is not an int")

	s, ok = m.String("sc_obj")
	assert.True(t, ok)
	assert.Equal(t, `{"a":1}`, s, "bd stores an object-looking k=v value as a string")

	_, ok = m.Int("sc_run")
	assert.False(t, ok, "r1 is not a number")
	_, ok = m.String("sc_missing")
	assert.False(t, ok)
	_, ok = m.Int("sc_missing")
	assert.False(t, ok)
	_, ok = m.Bool("sc_missing")
	assert.False(t, ok)
	_, ok = m.Bool("sc_run")
	assert.False(t, ok)

	// Values written by other tools or decoded differently.
	m = Metadata{"a": "12", "b": json.Number("3"), "c": nil, "d": []any{1.0}, "e": "yes", "f": int64(4), "g": 5, "h": 1e18, "i": "x"}
	n, ok = m.Int("a")
	assert.True(t, ok)
	assert.Equal(t, 12, n)
	n, ok = m.Int("b")
	assert.True(t, ok)
	assert.Equal(t, 3, n)
	_, ok = m.Int("c")
	assert.False(t, ok, "null counts as absent")
	_, ok = m.String("c")
	assert.False(t, ok)
	s, ok = m.String("d")
	assert.True(t, ok)
	assert.Equal(t, "[1]", s)
	b, ok = m.Bool("e")
	assert.False(t, ok)
	assert.False(t, b)
	n, ok = m.Int("f")
	assert.True(t, ok)
	assert.Equal(t, 4, n)
	n, ok = m.Int("g")
	assert.True(t, ok)
	assert.Equal(t, 5, n)
	s, ok = m.String("h")
	assert.True(t, ok)
	assert.Equal(t, "1000000000000000000", s)
	_, ok = m.Int("i")
	assert.False(t, ok)
	_, ok = m.Int("d")
	assert.False(t, ok)
}

func TestIssueDecodesEveryShape(t *testing.T) {
	// show: the issue form of dependencies, comments, parent.
	is := typesIssue(t, "show.json", "fx-so0")
	assert.Equal(t, "Fix Mean skipping the first value", is.Title)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, domain.Priority(1), is.Priority)
	assert.Equal(t, "bug", is.Type)
	assert.Equal(t, "sc:engineer-1", is.Assignee)
	assert.Equal(t, domain.BeadID("fx-nvi"), is.Parent)
	assert.Equal(t, domain.BeadID("fx-nvi"), is.ParentID())
	assert.False(t, is.CreatedAt.IsZero())
	assert.False(t, is.StartedAt.IsZero())
	assert.True(t, is.ClosedAt.IsZero())
	assert.Equal(t, 2, is.CommentCount)
	require.Len(t, is.Dependencies, 1)
	assert.Equal(t, Dependency{IssueID: "fx-so0", DependsOnID: "fx-nvi", Type: DepParentChild, Title: "Run r1: fix the two bugs", Status: "open", CreatedAt: is.Dependencies[0].CreatedAt}, is.Dependencies[0])
	assert.Empty(t, is.Blockers(), "a parent-child edge is not a blocker")
	assert.True(t, is.HasLabel("sc:run:r1"))
	assert.False(t, is.HasLabel("sc:role:tester"))
	assert.False(t, is.IsEpic())

	// show --include-dependents: the dependents' issue form.
	is = typesIssue(t, "show-dependents.json", "fx-so0")
	require.Len(t, is.Dependents, 1)
	assert.Equal(t, domain.BeadID("fx-ctt"), is.Dependents[0].IssueID)
	assert.Equal(t, domain.BeadID("fx-so0"), is.Dependents[0].DependsOnID)
	assert.Equal(t, DepBlocks, is.Dependents[0].Type)
	assert.Equal(t, "Fix Max for all-negative input", is.Dependents[0].Title)

	// list: the edge form.
	is = typesIssue(t, "list.json", "fx-ctt")
	require.Len(t, is.Dependencies, 2)
	var blocks Dependency
	for _, d := range is.Dependencies {
		assert.Equal(t, domain.BeadID("fx-ctt"), d.IssueID)
		if d.Type == DepBlocks {
			blocks = d
		}
	}
	assert.Equal(t, Dependency{IssueID: "fx-ctt", DependsOnID: "fx-so0", Type: DepBlocks, CreatedBy: "sc", CreatedAt: blocks.CreatedAt}, blocks)
	assert.Equal(t, []domain.BeadID{"fx-so0"}, is.Blockers())
	assert.Equal(t, domain.BeadID("fx-nvi"), is.ParentID())
	assert.Equal(t, 1, is.DependencyCount)

	// The epic has no dependencies at all.
	epic := typesIssue(t, "list.json", "fx-nvi")
	assert.True(t, epic.IsEpic())
	assert.Empty(t, epic.ParentID())
	assert.Empty(t, epic.Blockers())

	// blocked: blocked_by ids without edges.
	is = typesIssue(t, "blocked.json", "fx-nvi.1")
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, is.BlockedBy)
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, is.Blockers())
	assert.Equal(t, 1, is.BlockedByCount)

	// A blocker listed both as an edge and in blocked_by is reported once.
	is.Dependencies = []Dependency{{IssueID: "fx-nvi.1", DependsOnID: "fx-nvi.2", Type: DepBlocks}}
	assert.Equal(t, []domain.BeadID{"fx-nvi.2"}, is.Blockers())

	// A missing priority decodes as the default.
	var bare Issue
	require.NoError(t, json.Unmarshal([]byte(`{"id":"x-1","title":"t"}`), &bare))
	assert.Equal(t, domain.PriorityDefault, bare.Priority)
	assert.Empty(t, bare.Type)
}

func TestReadyListsTheEpic(t *testing.T) {
	// P3-13 (a): the raw `bd ready` output includes the run epic itself.
	issues := typesIssues(t, "ready.json")
	var epics []domain.BeadID
	for _, is := range issues {
		if is.IsEpic() {
			epics = append(epics, is.ID)
		}
	}
	assert.Equal(t, []domain.BeadID{"fx-nvi"}, epics)
	issues = typesIssues(t, "ready-after-close.json")
	assert.True(t, issues[0].IsEpic())
}

func TestPreambleTolerance(t *testing.T) {
	// P3-13 (b): a text line before the envelope, and text-only outputs.
	shared := testutil.BDSamplesDir(t)
	gate, err := os.ReadFile(filepath.Join(shared, "gate-check.txt"))
	require.NoError(t, err)
	var checked struct {
		Checked int `json:"checked"`
		Errors  int `json:"errors"`
	}
	require.NoError(t, Decode(string(gate), &checked))
	assert.Equal(t, 1, checked.Checked)
	assert.Equal(t, 1, checked.Errors)

	var anything json.RawMessage
	for _, name := range []string{"init.txt", "info.txt", "comment-no-json.txt", "create-silent.txt"} {
		err := Decode(envSample(t, name), &anything)
		assert.True(t, errors.Is(err, ErrNoJSON), "%s should hold no JSON: %v", name, err)
	}

	// The metrics preamble followed by a legacy array still decodes.
	var issues []Issue
	require.NoError(t, Decode(envSample(t, "preamble.txt")+`[{"id":"x-1","title":"t"}]`, &issues))
	assert.Len(t, issues, 1)
}

func TestDependencyJSONRoundTrip(t *testing.T) {
	d := Dependency{IssueID: "a", DependsOnID: "b", Type: DepBlocks, CreatedBy: "sc", CreatedAt: time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)}
	data, err := json.Marshal(d)
	require.NoError(t, err)
	assert.JSONEq(t, `{"issue_id":"a","depends_on_id":"b","type":"blocks","created_by":"sc","created_at":"2026-09-02T01:02:03Z"}`, string(data))
	var back Dependency
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, d, back)

	data, err = json.Marshal(Dependency{IssueID: "a", DependsOnID: "b"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"issue_id":"a","depends_on_id":"b","type":""}`, string(data))

	var issueForm Dependency
	require.NoError(t, json.Unmarshal([]byte(`{"id":"z","title":"Z","status":"open","dependency_type":"blocks"}`), &issueForm))
	assert.Empty(t, issueForm.IssueID, "the issue form leaves the sides for the enclosing issue")
	issueForm.normalise("me", false)
	assert.Equal(t, domain.BeadID("me"), issueForm.IssueID)
	assert.Equal(t, domain.BeadID("z"), issueForm.DependsOnID)

	var untyped Dependency
	require.NoError(t, json.Unmarshal([]byte(`{"issue_id":"a","depends_on_id":"b"}`), &untyped))
	assert.Equal(t, DepBlocks, untyped.Type, "a missing type means blocks")

	assert.Error(t, json.Unmarshal([]byte(`[1]`), &untyped))
}

func TestIssueBead(t *testing.T) {
	is := typesIssue(t, "update-set-metadata-types.json", "fx-ctt")
	is.Metadata[MetaSize] = "M"
	is.Dependencies = []Dependency{{IssueID: "fx-ctt", DependsOnID: "fx-so0", Type: DepBlocks}, {IssueID: "fx-ctt", DependsOnID: "fx-nvi", Type: DepParentChild}}
	b, err := is.Bead()
	require.NoError(t, err)
	assert.Equal(t, domain.Bead{
		ID: "fx-ctt", Title: "Fix Max for all-negative input", Description: "stats.Max starts from 0.",
		Type: domain.BeadTypeBug, Priority: 2, Size: domain.SizeM, Deps: []domain.BeadID{"fx-so0"},
		State: domain.BeadStateInReview, Labels: []string{"sc:role:engineer", "sc:run:r1"},
		Round: 1, PRURL: "https://example.com/pr/7", PRNumber: 7,
	}, b)

	closed := typesIssue(t, "close.json", "fx-so0")
	b, err = closed.Bead()
	require.NoError(t, err)
	assert.Equal(t, domain.BeadStateClosed, b.State)
	assert.Equal(t, "sc:engineer-1", b.Assignee)
	assert.Zero(t, b.Round)

	// A type-less issue is a task; a foreign status or type is an error.
	b, err = Issue{ID: "x", Status: "open", Priority: 2}.Bead()
	require.NoError(t, err)
	assert.Equal(t, domain.BeadTypeTask, b.Type)

	_, err = Issue{ID: "x", Status: "triage"}.Bead()
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
	assert.Contains(t, err.Error(), "bead x")
	_, err = Issue{ID: "x", Status: "open", Type: "convoy"}.Bead()
	assert.ErrorIs(t, err, domain.ErrUnknownValue)

	// Every status in the captured statuses list that sc knows converts.
	var list StatusList
	require.NoError(t, Decode(envSample(t, "statuses.json"), &list))
	for _, s := range append(list.BuiltIn, list.Custom...) {
		_, err := Issue{ID: "x", Status: s.Name, Priority: 2}.Bead()
		assert.NoError(t, err, s.Name)
	}
}

func TestCustomStatusHelpers(t *testing.T) {
	want := "in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen"
	assert.Equal(t, want, FormatCustomStatuses(WorkflowCustomStatuses()), "D-07 in flow order")

	var got ConfigValue
	require.NoError(t, Decode(envSample(t, "config-get.json"), &got))
	parsed, err := ParseCustomStatuses(got.Value)
	require.NoError(t, err)
	assert.Equal(t, WorkflowCustomStatuses(), parsed)
	assert.Equal(t, got.Value, FormatCustomStatuses(parsed), "round trip")

	parsed, err = ParseCustomStatuses(" triage : active ,, ")
	require.NoError(t, err)
	assert.Equal(t, []CustomStatus{{Name: "triage", Category: domain.CategoryActive}}, parsed)
	parsed, err = ParseCustomStatuses("")
	require.NoError(t, err)
	assert.Empty(t, parsed)
	_, err = ParseCustomStatuses("nocategory")
	assert.Error(t, err)
	_, err = ParseCustomStatuses("x:bogus")
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
	_, err = ParseCustomStatuses(":wip")
	assert.Error(t, err)

	// Merging keeps the repository's own statuses first and never
	// rewrites their categories.
	existing := []CustomStatus{{Name: "triage", Category: domain.CategoryActive}, {Name: "in_review", Category: domain.CategoryFrozen}}
	merged := MergeCustomStatuses(existing, WorkflowCustomStatuses())
	assert.Equal(t, existing, merged[:2])
	assert.Len(t, merged, 2+len(WorkflowCustomStatuses())-1)
	assert.Equal(t, WorkflowCustomStatuses(), MergeCustomStatuses(nil, WorkflowCustomStatuses()))
	assert.Equal(t, CustomStatus{Name: "a", Category: "wip"}.String(), "a:wip")
}

func TestStatusListDecodes(t *testing.T) {
	var list StatusList
	require.NoError(t, Decode(envSample(t, "statuses.json"), &list))
	assert.Equal(t, StatusInfo{Name: "open", Category: "active", Icon: "○", Description: "Available to work (default)"}, list.BuiltIn[0])
	assert.Equal(t, StatusInfo{Name: "approved", Category: "wip"}, list.Custom[0])
	assert.Len(t, list.CustomStatuses(), 7)
	for _, s := range domain.WorkflowBeadStates() {
		assert.True(t, list.Has(string(s)), s)
	}
}

func TestIssueIDs(t *testing.T) {
	assert.Equal(t, []domain.BeadID{"a", "b"}, IssueIDs([]Issue{{ID: "a"}, {ID: "b"}}))
	assert.Empty(t, IssueIDs(nil))
	assert.Equal(t, []string{"a", "b", "c"}, sortedKeys(map[string]int{"c": 1, "a": 2, "b": 3}))
}

func TestGraphResultDecodes(t *testing.T) {
	var dry GraphResult
	require.NoError(t, Decode(envSample(t, "create-graph-dry-run.json"), &dry))
	assert.True(t, dry.DryRun)
	assert.Equal(t, 1, dry.EdgeCount)
	require.Len(t, dry.Nodes, 3)
	assert.Equal(t, GraphNode{Key: "mean", Title: "Fix Mean skipping the first value", Type: domain.BeadTypeBug, Priority: 1, ParentKey: "epic"}, dry.Nodes[1])

	var real GraphResult
	require.NoError(t, Decode(envSample(t, "create-graph.json"), &real))
	assert.Equal(t, domain.BeadID("fx-nvi"), real.IDs["epic"])
	assert.False(t, real.DryRun)
}
