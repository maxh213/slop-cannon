package beads

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// statusesFullValue is the D-07 list as bd stores it.
const statusesFullValue = "in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen"

func TestEnsureStatusesAlreadySet(t *testing.T) {
	// config-get.json holds the full D-07 list: nothing to write.
	c, bd := clientFake(t, nil)
	res, err := c.EnsureStatuses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, WorkflowCustomStatuses(), res.Existing)
	assert.Nil(t, res.Added)
	assert.Equal(t, statusesFullValue, res.Value)
	assert.True(t, res.Statuses.Has("in_review"))
	calls := bd.ArgLists(t)
	require.Len(t, calls, 2, "get + statuses, no set")
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "get", StatusCustomKey, "--json"}, calls[0])
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "statuses", "--json"}, calls[1])
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestEnsureStatusesFreshRepository(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"config-get": "config-get-missing"})
	res, err := c.EnsureStatuses(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Existing)
	assert.Equal(t, WorkflowCustomStatuses(), res.Added)
	assert.Equal(t, statusesFullValue, res.Value)
	calls := bd.ArgLists(t)
	require.Len(t, calls, 3, "get + set + statuses")
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "set", StatusCustomKey, statusesFullValue, "--json"}, calls[1])
}

func TestEnsureStatusesUnionsWithForeignStatuses(t *testing.T) {
	// The repository already has triage:active and parked:frozen (D-07:
	// merge, never replace): they stay first and keep their categories.
	c, bd := clientFake(t, map[string]string{"config-get": "config-get-foreign", "statuses": "statuses-foreign"})
	res, err := c.EnsureStatuses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []CustomStatus{{"triage", domain.CategoryActive}, {"parked", domain.CategoryFrozen}}, res.Existing)
	assert.Equal(t, WorkflowCustomStatuses(), res.Added)
	want := "triage:active,parked:frozen," + statusesFullValue
	assert.Equal(t, want, res.Value)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "set", StatusCustomKey, want, "--json"}, bd.ArgLists(t)[1])
	assert.True(t, res.Statuses.Has("triage"), "bd still reports the foreign status")
	for _, s := range domain.WorkflowBeadStates() {
		assert.True(t, res.Statuses.Has(string(s)), s)
	}
}

func TestEnsureStatusesNeverReplacesAMalformedValue(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"config-get": "config-get-malformed"})
	_, err := c.EnsureStatuses(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
	assert.Contains(t, err.Error(), "triage,parked:frozen")
	assert.Len(t, bd.ArgLists(t), 1, "no write after a value sc cannot parse")
}

func TestEnsureStatusesVerifiesWithBD(t *testing.T) {
	// bd answered the set but its statuses listing lacks the custom ones.
	c, _ := clientFake(t, map[string]string{"config-get": "config-get-missing", "statuses": "statuses-builtin"})
	res, err := c.EnsureStatuses(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in_review")
	assert.Contains(t, err.Error(), "failed")
	assert.NotContains(t, err.Error(), "open", "built-ins are present")
	assert.Equal(t, WorkflowCustomStatuses(), res.Added, "the partial result is still reported")

	failing, _ := clientFake(t, map[string]string{"config-get": "error-no-project"})
	_, err = failing.EnsureStatuses(context.Background())
	assert.ErrorIs(t, err, ErrNoProject)
}

func TestSetStatus(t *testing.T) {
	c, bd := clientFake(t, nil)
	ctx := context.Background()
	for _, s := range domain.WorkflowBeadStates() {
		if s == domain.BeadStateClosed {
			continue
		}
		is, err := c.SetStatus(ctx, "fx-ctt", s)
		require.NoError(t, err, s)
		assert.Equal(t, domain.BeadID("fx-ctt"), is.ID)
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "-s", string(s), "--json"}, clientLastArgs(t, bd))
	}
	calls := len(bd.ArgLists(t))
	for _, s := range []domain.BeadState{domain.BeadStateClosed, domain.BeadStateBlocked, domain.BeadStateHooked, domain.BeadStateDeferred, domain.BeadStatePinned, "triage", ""} {
		_, err := c.SetStatus(ctx, "fx-ctt", s)
		assert.ErrorIs(t, err, ErrInvalid, string(s))
	}
	assert.Len(t, bd.ArgLists(t), calls, "refused states never reach bd")

	rejected, _ := clientFake(t, map[string]string{"update": "error-bad-status"})
	_, err := rejected.SetStatus(ctx, "fx-ctt", domain.BeadStateInReview)
	assert.ErrorIs(t, err, ErrInvalid, "bd's own refusal (status not registered) is ErrInvalid")
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestReadyNeverListsACustomStatus(t *testing.T) {
	// D-07 relies on categories: every custom status is wip or frozen, so
	// bd's own readiness rule (category active) keeps them out of `bd
	// ready`, and the captured ready sets contain only open beads.
	for _, s := range WorkflowCustomStatuses() {
		assert.NotEqual(t, domain.CategoryActive, s.Category, "%s must not be claimable", s.Name)
		assert.NotEqual(t, domain.CategoryDone, s.Category, "%s must stay visible in default listings", s.Name)
	}
	var list StatusList
	require.NoError(t, Decode(envSample(t, "statuses.json"), &list))
	m := NewStatusMap(list)
	for _, s := range domain.CustomBeadStates() {
		assert.Equal(t, s.Category(), m[string(s)], "bd stores %s with the category sc registered", s)
	}
	for _, sample := range []string{"ready.json", "ready-parent.json", "ready-after-close.json"} {
		var issues []Issue
		require.NoError(t, Decode(envSample(t, sample), &issues))
		require.NotEmpty(t, issues, sample)
		for _, is := range issues {
			assert.Equal(t, string(domain.BeadStateOpen), is.Status, "%s: %s", sample, is.ID)
			assert.Equal(t, domain.CategoryActive, m[is.Status])
		}
	}
	c, _ := clientFake(t, nil)
	ready, err := c.Ready(context.Background(), "")
	require.NoError(t, err)
	for _, is := range ready {
		assert.Equal(t, string(domain.BeadStateOpen), is.Status, is.ID)
	}
}

func TestSafeState(t *testing.T) {
	cases := []struct {
		status   string
		category domain.StateCategory
		want     domain.BeadState
		origin   StatusOrigin
	}{
		{"open", domain.CategoryActive, domain.BeadStateOpen, OriginWorkflow},
		{"in_review", domain.CategoryWIP, domain.BeadStateInReview, OriginWorkflow},
		{"closed", domain.CategoryDone, domain.BeadStateClosed, OriginWorkflow},
		{"needs_human", "", domain.BeadStateNeedsHuman, OriginWorkflow},
		{"Changes-Requested", "", domain.BeadStateChangesRequested, OriginWorkflow},
		{"blocked", domain.CategoryWIP, domain.BeadStateBlocked, OriginBuiltIn},
		{"hooked", domain.CategoryWIP, domain.BeadStateHooked, OriginBuiltIn},
		{"deferred", domain.CategoryFrozen, domain.BeadStateDeferred, OriginBuiltIn},
		{"pinned", domain.CategoryFrozen, domain.BeadStatePinned, OriginBuiltIn},
		{"triage", domain.CategoryActive, domain.BeadStatePinned, OriginForeign},
		{"reviewing", domain.CategoryWIP, domain.BeadStateBlocked, OriginForeign},
		{"parked", domain.CategoryFrozen, domain.BeadStateDeferred, OriginForeign},
		{"shipped", domain.CategoryDone, domain.BeadStateClosed, OriginForeign},
		{"mystery", "", domain.BeadStateDeferred, OriginForeign},
		{"", "", domain.BeadStateDeferred, OriginForeign},
	}
	for _, tc := range cases {
		got, origin := SafeState(tc.status, tc.category)
		assert.Equal(t, tc.want, got, tc.status)
		assert.Equal(t, tc.origin, origin, tc.status)
		if origin == OriginForeign {
			assert.True(t, !got.Workflow() || got.Terminal(), "%s maps to a state the state machine never transitions", tc.status)
		}
		assert.True(t, got.Valid(), tc.status)
	}
	assert.Equal(t, "workflow", OriginWorkflow.String())
	assert.Equal(t, "built-in", OriginBuiltIn.String())
	assert.Equal(t, "foreign", OriginForeign.String())
	assert.Equal(t, "StatusOrigin(7)", StatusOrigin(7).String())
}

func TestStatusMap(t *testing.T) {
	var list StatusList
	require.NoError(t, Decode(envSample(t, "statuses-foreign.json"), &list))
	m := NewStatusMap(list)
	assert.Equal(t, domain.CategoryActive, m["triage"])
	assert.Equal(t, domain.CategoryDone, m["shipped"])
	assert.Equal(t, domain.CategoryWIP, m["in_progress"])
	state, origin := m.Resolve("triage")
	assert.Equal(t, domain.BeadStatePinned, state)
	assert.Equal(t, OriginForeign, origin)
	state, origin = m.Resolve("shipped")
	assert.Equal(t, domain.BeadStateClosed, state)
	assert.Equal(t, OriginForeign, origin)

	var nilMap StatusMap
	state, origin = nilMap.Resolve("triage")
	assert.Equal(t, domain.BeadStateDeferred, state, "without categories a foreign status is parked")
	assert.Equal(t, OriginForeign, origin)
	state, origin = nilMap.Resolve("approved")
	assert.Equal(t, domain.BeadStateApproved, state)
	assert.Equal(t, OriginWorkflow, origin)

	c, bd := clientFake(t, map[string]string{"statuses": "statuses-foreign"})
	got, err := c.StatusMap(context.Background())
	require.NoError(t, err)
	assert.Equal(t, m, got)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "statuses", "--json"}, clientLastArgs(t, bd))
}

func TestForeignStatusesRoundTripThroughList(t *testing.T) {
	// P3-14: a listing with the repository's own custom statuses (triage,
	// parked, shipped), bd's blocked and hooked built-ins, an issue type
	// sc does not know and a status nobody registered decodes without
	// error, and every issue converts to a safe domain state.
	c, bd := clientFake(t, map[string]string{"list": "list-foreign", "statuses": "statuses-foreign"})
	ctx := context.Background()
	issues, err := c.List(ctx, ListFilter{All: true})
	require.NoError(t, err)
	require.Len(t, issues, 8)
	assert.Empty(t, testutil.MissingSamples(t, bd))

	m, err := c.StatusMap(ctx)
	require.NoError(t, err)

	want := map[domain.BeadID]struct {
		state  domain.BeadState
		origin StatusOrigin
		typ    domain.BeadType
	}{
		"fx-nvi": {domain.BeadStateOpen, OriginWorkflow, domain.BeadTypeEpic},
		"fx-ctt": {domain.BeadStateInReview, OriginWorkflow, domain.BeadTypeBug},
		"fx-t1a": {domain.BeadStatePinned, OriginForeign, domain.BeadTypeBug},
		"fx-p2b": {domain.BeadStateDeferred, OriginForeign, domain.BeadTypeFeature},
		"fx-s3c": {domain.BeadStateClosed, OriginForeign, domain.BeadTypeMilestone},
		"fx-b4d": {domain.BeadStateBlocked, OriginBuiltIn, domain.BeadTypeTask},
		"fx-h5e": {domain.BeadStateHooked, OriginBuiltIn, domain.BeadTypeTask},
		"fx-u6f": {domain.BeadStateDeferred, OriginForeign, domain.BeadTypeTask},
	}
	for _, is := range issues {
		w, ok := want[is.ID]
		require.True(t, ok, is.ID)
		b, origin := is.SafeBead(m)
		assert.Equal(t, w.state, b.State, is.ID)
		assert.Equal(t, w.origin, origin, is.ID)
		assert.Equal(t, w.typ, b.Type, is.ID)
		assert.Equal(t, is.ID, b.ID)
		assert.Equal(t, is.Title, b.Title)
		if origin == OriginForeign {
			assert.True(t, is.Foreign(), is.ID)
			assert.True(t, !b.State.Workflow() || b.State.Terminal(), "%s never gets transitioned", is.ID)
			_, err := is.Bead()
			assert.ErrorIs(t, err, domain.ErrUnknownValue, "%s: the strict converter still refuses", is.ID)
		} else {
			assert.False(t, is.Foreign(), is.ID)
		}
	}
	// The strict converter accepts the built-ins and rejects only the
	// unknown type.
	_, err = conventionsIssue(t, "list-foreign.json", "fx-b4d").Bead()
	assert.NoError(t, err)
	_, err = conventionsIssue(t, "list-foreign.json", "fx-h5e").Bead()
	assert.ErrorIs(t, err, domain.ErrUnknownValue, "issue type convoy")

	// The sc bead keeps its metadata through the safe path.
	b, _ := conventionsIssue(t, "list-foreign.json", "fx-ctt").SafeBead(m)
	assert.Equal(t, 1, b.Round)
	assert.Equal(t, domain.SizeM, b.Size)
	assert.Equal(t, []domain.BeadID{"fx-so0"}, b.Deps)

	beads := SafeBeads(issues, m)
	require.Len(t, beads, len(issues))
	assert.Equal(t, IssueIDs(issues)[2], beads[2].ID)

	// Filtering by run first (ListRun) is the cheap way to never see them.
	run, err := c.ListRun(ctx, "r1", true)
	require.NoError(t, err)
	for _, is := range run {
		assert.False(t, is.Foreign(), is.ID)
	}
}
