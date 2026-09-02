package beads

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// conventionsIssues decodes a captured listing sample.
func conventionsIssues(t *testing.T, sample string) []Issue {
	t.Helper()
	var issues []Issue
	require.NoError(t, Decode(envSample(t, sample), &issues))
	return issues
}

// conventionsIssue picks one issue out of a listing sample.
func conventionsIssue(t *testing.T, sample string, id domain.BeadID) Issue {
	t.Helper()
	for _, is := range conventionsIssues(t, sample) {
		if is.ID == id {
			return is
		}
	}
	t.Fatalf("no issue %s in %s", id, sample)
	return Issue{}
}

// conventionsMetadataFile reads the JSON object the client wrote for a
// `--metadata @file` argument of the last shim call.
func conventionsMetadataFile(t *testing.T, args []string) map[string]any {
	t.Helper()
	for k, a := range args {
		if a == "--metadata" && k+1 < len(args) {
			path := strings.TrimPrefix(args[k+1], "@")
			data, err := os.ReadFile(path)
			if err != nil {
				// The client removes the file after the call; the shim
				// captured nothing, so the caller must have kept it.
				t.Fatalf("metadata file %s: %v", path, err)
			}
			var m map[string]any
			require.NoError(t, json.Unmarshal(data, &m))
			return m
		}
	}
	t.Fatal("no --metadata argument")
	return nil
}

func TestConventionsLabels(t *testing.T) {
	assert.Equal(t, "sc:run:r1", RunLabel("r1"))
	assert.Equal(t, "sc:role:engineer", RoleLabel(domain.RoleEngineer))
	assert.Equal(t, "sc:external-pr", LabelExternalPR)
	assert.True(t, IsSCLabel(LabelExternalPR))
	assert.False(t, IsSCLabel("bug"))

	run, ok := ParseRunLabel("sc:run:r1")
	assert.True(t, ok)
	assert.Equal(t, domain.RunID("r1"), run)
	_, ok = ParseRunLabel("sc:run:")
	assert.False(t, ok, "an empty run id is not a run label")
	_, ok = ParseRunLabel("sc:role:engineer")
	assert.False(t, ok)

	role, ok := ParseRoleLabel("sc:role:tester")
	assert.True(t, ok)
	assert.Equal(t, domain.RoleTester, role)
	_, ok = ParseRoleLabel("sc:role:janitor")
	assert.False(t, ok, "unknown roles are not role labels")
	_, ok = ParseRoleLabel("sc:run:r1")
	assert.False(t, ok)
}

func TestConventionsActors(t *testing.T) {
	assert.Equal(t, "sc:eng-sc-68s-r2", ActorFor("eng-sc-68s-r2"))
	agent, ok := AgentOf("sc:eng-sc-68s-r2")
	assert.True(t, ok)
	assert.Equal(t, domain.AgentID("eng-sc-68s-r2"), agent)
	_, ok = AgentOf(DefaultActor)
	assert.False(t, ok, "the plumbing actor is not an agent")
	_, ok = AgentOf("maintainer")
	assert.False(t, ok)
	_, ok = AgentOf("sc:")
	assert.False(t, ok)
}

func TestConventionsMetadataBuilders(t *testing.T) {
	assert.Equal(t, map[string]any{MetaRun: "r1", MetaKind: KindEpic}, EpicMetadata("r1"))
	assert.Equal(t, map[string]any{MetaRun: "r1", MetaKind: KindWork, MetaRound: 0, MetaSize: "m"}, WorkMetadata("r1", domain.SizeM))
	assert.Equal(t, map[string]any{MetaRun: "r1", MetaKind: KindWork, MetaRound: 0}, WorkMetadata("r1", ""), "no size, no key")
	assert.Equal(t, map[string]any{MetaRound: 3}, RoundMetadata(3))
	assert.Equal(t, map[string]any{MetaPRURL: "https://example.com/pr/7", MetaPRNumber: 7}, PRMetadata("https://example.com/pr/7", 7))
	assert.Equal(t, map[string]any{MetaPRURL: "https://example.com/pr/7"}, PRMetadata("https://example.com/pr/7", 0))
	assert.Equal(t, map[string]any{MetaMergeSHA: "abc123"}, MergeMetadata("abc123"))

	at := time.Date(2026, 9, 2, 14, 9, 32, 500, time.FixedZone("CEST", 2*3600))
	assert.Equal(t, map[string]any{MetaAgent: "engineer-1", MetaLease: "2026-09-02T12:09:32Z"}, LeaseMetadata("engineer-1", at), "leases are RFC 3339 UTC")
	assert.Equal(t, []string{MetaAgent, MetaLease}, LeaseKeys)

	merged := MergeMeta(WorkMetadata("r1", domain.SizeS), RoundMetadata(2), map[string]any{MetaRun: "r2"})
	assert.Equal(t, map[string]any{MetaRun: "r2", MetaKind: KindWork, MetaRound: 2, MetaSize: "s"}, merged, "later maps win")
	assert.Equal(t, map[string]any{}, MergeMeta(), "no maps, empty result")

	// Every sc_ key the decisions name has a builder or an accessor, and
	// none uses a reserved prefix.
	for _, key := range []string{MetaRun, MetaRound, MetaPRURL, MetaPRNumber, MetaMergeSHA, MetaAgent, MetaLease, MetaKind, MetaSize} {
		assert.True(t, strings.HasPrefix(key, "sc_"), key)
		assert.False(t, strings.HasPrefix(key, "bd:") || strings.HasPrefix(key, "_"), key)
	}
}

func TestConventionsIssueAccessors(t *testing.T) {
	is := conventionsIssue(t, "list-filtered.json", "fx-ctt")
	assert.Equal(t, domain.RunID("r1"), is.RunID())
	assert.True(t, is.InRun("r1"))
	assert.False(t, is.InRun("r2"))
	assert.False(t, is.InRun(""), "the empty run matches nothing")
	assert.Equal(t, 1, is.Round())
	assert.Equal(t, "https://example.com/pr/7", is.PRURL())
	assert.Equal(t, 7, is.PRNumber())
	assert.Equal(t, domain.AgentID("engineer-1"), is.Agent())
	lease, ok := is.Lease()
	assert.True(t, ok)
	assert.Equal(t, time.Date(2026, 9, 2, 14, 9, 32, 0, time.UTC), lease.UTC())
	assert.Equal(t, []domain.Role{domain.RoleEngineer}, is.Roles())
	assert.Empty(t, is.Kind(), "a graph-plan bead without sc_kind")
	assert.False(t, is.IsRunEpic())
	assert.False(t, is.IsExternalPR())
	assert.Empty(t, is.MergeSHA())
	_, ok = is.Size()
	assert.False(t, ok)

	work := conventionsIssue(t, "list-filtered.json", "fx-nvi.1")
	assert.Equal(t, KindWork, work.Kind())
	assert.Zero(t, work.Round(), "sc_round 0 decodes as 0")

	epic := conventionsIssue(t, "list-all.json", "fx-nvi")
	assert.True(t, epic.IsRunEpic())
	assert.Equal(t, KindEpic, epic.Kind())
	assert.Equal(t, domain.RunID("r1"), epic.RunID())

	// Labels stand in when the metadata is missing; a foreign bead has
	// neither.
	labelled := Issue{ID: "x", Labels: []string{"bug", "sc:run:r9"}}
	assert.Equal(t, domain.RunID("r9"), labelled.RunID())
	assert.Empty(t, Issue{ID: "y"}.RunID())
	assert.Empty(t, Issue{ID: "y"}.Roles())
	_, ok = Issue{ID: "y"}.Lease()
	assert.False(t, ok)
	_, ok = Issue{ID: "y", Metadata: Metadata{MetaLease: "yesterday"}}.Lease()
	assert.False(t, ok, "an unparsable lease is reported as absent")
	sized := Issue{ID: "z", Metadata: Metadata{MetaSize: "L"}}
	size, ok := sized.Size()
	assert.True(t, ok)
	assert.Equal(t, domain.SizeL, size)
	assert.True(t, Issue{ID: "z", Labels: []string{LabelExternalPR}}.IsExternalPR())
	assert.Equal(t, "https://example.com/pr/7", Issue{Metadata: Metadata{MetaMergeSHA: "abc", MetaPRURL: "https://example.com/pr/7"}}.PRURL())
	assert.Equal(t, "abc", Issue{Metadata: Metadata{MetaMergeSHA: "abc"}}.MergeSHA())
}

func TestConventionsMetadataAndLabelCalls(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"update": "update-metadata-file"})
	ctx := context.Background()

	is, err := c.SetMetadata(ctx, "fx-ctt", PRMetadata("https://example.com/pr/7", 7))
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-ctt"), is.ID)
	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "--metadata"}, args[:5])
	assert.True(t, strings.HasPrefix(args[5], "@"), "metadata travels through @file so numbers stay numbers")
	assert.Equal(t, "--json", args[6])

	_, err = c.UnsetMetadata(ctx, "fx-ctt", LeaseKeys...)
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "--unset-metadata", MetaAgent, "--unset-metadata", MetaLease, "--json"}, clientLastArgs(t, bd))

	_, err = c.AddLabels(ctx, "fx-ctt", RoleLabel(domain.RoleTester), LabelExternalPR)
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "--add-label", "sc:role:tester", "--add-label", "sc:external-pr", "--json"}, clientLastArgs(t, bd))

	_, err = c.RemoveLabels(ctx, "fx-ctt", RoleLabel(domain.RoleEngineer))
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "--remove-label", "sc:role:engineer", "--json"}, clientLastArgs(t, bd))

	calls := len(bd.ArgLists(t))
	for _, call := range []func() error{
		func() error { _, err := c.SetMetadata(ctx, "fx-ctt", nil); return err },
		func() error { _, err := c.UnsetMetadata(ctx, "fx-ctt"); return err },
		func() error { _, err := c.AddLabels(ctx, "fx-ctt"); return err },
		func() error { _, err := c.RemoveLabels(ctx, "fx-ctt"); return err },
	} {
		assert.ErrorIs(t, call(), ErrInvalid)
	}
	assert.Len(t, bd.ArgLists(t), calls, "empty requests never reach bd")
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestConventionsListRun(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"list": "list-foreign"})
	issues, err := c.ListRun(context.Background(), "r1", true)
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-nvi", "fx-ctt"}, IssueIDs(issues), "foreign beads are dropped even when bd's label filter let them through")
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "list", "--json", "-n", "0", "--flat", "--all", "-l", "sc:run:r1"}, clientLastArgs(t, bd))

	_, err = c.ListRun(context.Background(), "r1", false)
	require.NoError(t, err)
	assert.NotContains(t, clientLastArgs(t, bd), "--all")

	_, err = c.ListRun(context.Background(), "", false)
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestConventionsCreateRunEpic(t *testing.T) {
	c, bd := clientFake(t, nil)
	var seen map[string]any
	// Keep the metadata file: the client deletes it after the call.
	c.exec = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		seen = conventionsMetadataFile(t, spec.Args)
		return proc.Run(ctx, spec)
	}
	is, err := c.CreateRunEpic(context.Background(), RunEpicInput{Run: "r1", Description: "fix the two bugs"})
	require.NoError(t, err)
	assert.NotEmpty(t, is.ID)
	args := clientLastArgs(t, bd)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "create", "Run r1", "-t", "epic", "-p", "1", "--description", "fix the two bugs", "-l", "sc:run:r1", "--metadata"}, args[:13])
	assert.Equal(t, map[string]any{MetaRun: "r1", MetaKind: KindEpic}, seen)
	assert.Equal(t, "--json", args[len(args)-1])
	assert.NotContains(t, args, "--parent", "the epic has no parent")

	_, err = c.CreateRunEpic(context.Background(), RunEpicInput{Run: "r1", Title: "  Custom  "})
	require.NoError(t, err)
	assert.Equal(t, "Custom", clientLastArgs(t, bd)[3], "an explicit title is trimmed and used")

	_, err = c.CreateRunEpic(context.Background(), RunEpicInput{})
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, "Run r1", RunEpicTitle("r1"))
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestConventionsFindRunEpic(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"list": "list-all"})
	is, err := c.FindRunEpic(context.Background(), "r1")
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-nvi"), is.ID)
	assert.True(t, is.IsRunEpic())
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "list", "--json", "-n", "0", "--flat", "--all", "-l", "sc:run:r1", "--metadata-field", "sc_kind=epic"}, clientLastArgs(t, bd))

	_, err = c.FindRunEpic(context.Background(), "r2")
	assert.ErrorIs(t, err, ErrNotFound, "the listing is filtered by run again on the client side")

	_, err = c.FindRunEpic(context.Background(), "")
	assert.ErrorIs(t, err, ErrInvalid)

	empty, _ := clientFake(t, map[string]string{"list": "comments-empty"})
	_, err = empty.FindRunEpic(context.Background(), "r1")
	assert.ErrorIs(t, err, ErrNotFound)
}
