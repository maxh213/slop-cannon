package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// poRequest returns a usable autopilot planning request rooted at dir.
func poRequest(dir string) agents.PlanRequest {
	return agents.PlanRequest{
		Run:     "run-42",
		AgentID: "po-1",
		Mode:    domain.RunModeAutopilot,
		Repo:    prompt.RepoContext{Root: dir, DefaultBranch: "main"},
	}
}

// poPlan is a small valid plan: three beads with one dependency.
func poPlan() domain.Plan {
	return domain.Plan{
		Brief: "Make the retry path testable.",
		Beads: []domain.PlanBead{
			{Key: "retry-backoff", Title: "Add exponential backoff to the HTTP client",
				Description: "The client retries immediately, which hammers the server.",
				Acceptance:  "Retries back off exponentially; a unit test proves the delays.",
				Type:        domain.BeadTypeBug, Priority: domain.PriorityDefault, Size: domain.SizeM},
			{Key: "retry-metrics", Title: "Count retries",
				Acceptance: "Each retry increments a counter covered by a test.",
				Type:       domain.BeadTypeFeature, Priority: domain.PriorityDefault, Size: domain.SizeS,
				Deps: []string{"retry-backoff"}},
			{Key: "retry-docs", Title: "Document the retry policy",
				Acceptance: "README describes the backoff and the counter.",
				Priority:   domain.PriorityDefault, Size: domain.SizeS},
		},
	}
}

// poPlanJSON renders a plan the way an agent would put it in its sc-result
// block.
func poPlanJSON(t *testing.T, p domain.Plan) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, agents.ValidateJSON(domain.MessagePlan, raw), "the test plan must satisfy the published schema")
	return raw
}

// poOutput wraps a plan in the sc-result block a session would produce.
func poOutput(t *testing.T, p domain.Plan) string {
	t.Helper()
	block, err := mock.ResultBlock(poPlanJSON(t, p))
	require.NoError(t, err)
	return "Here is the plan.\n\n" + block
}

// poApplier is a PlanApplier that converts the request with the real
// beads.BuildGraphPlan (so anything bd would reject fails here too) and
// mints predictable ids.
type poApplier struct {
	requests []beads.PlanRequest
	dryRuns  []bool
	err      error
}

func (a *poApplier) ApplyPlan(_ context.Context, req beads.PlanRequest, dryRun bool) (beads.PlanResult, error) {
	a.requests = append(a.requests, req)
	a.dryRuns = append(a.dryRuns, dryRun)
	if a.err != nil {
		return beads.PlanResult{}, a.err
	}
	graph, err := beads.BuildGraphPlan(req)
	if err != nil {
		return beads.PlanResult{}, err
	}
	res := beads.PlanResult{
		Run:         req.Run,
		Epic:        req.Epic,
		EpicCreated: req.Epic == "",
		Graph:       graph,
		DryRun: beads.GraphResult{
			DryRun: true, NodeCount: len(graph.Nodes), EdgeCount: len(graph.Edges),
		},
		IDs: map[string]domain.BeadID{},
	}
	for i, b := range req.Plan.Beads {
		id := domain.BeadID(fmt.Sprintf("fx-%d", i+1))
		res.IDs[b.Key] = id
		res.Beads = append(res.Beads, id)
	}
	return res, nil
}

func TestPOPlanRequestValidate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, poRequest(dir).Validate())

	brief := poRequest(dir)
	brief.Mode = domain.RunModeBrief
	brief.Brief = "Add a --json flag to every command."
	require.NoError(t, brief.Validate())

	cases := []struct {
		name string
		fn   func(r *agents.PlanRequest)
		want string
	}{
		{"no run", func(r *agents.PlanRequest) { r.Run = "" }, "empty run id"},
		{"unknown mode", func(r *agents.PlanRequest) { r.Mode = "vibes" }, "run mode \"vibes\""},
		{"brief without text", func(r *agents.PlanRequest) { r.Mode = domain.RunModeBrief }, "brief mode needs a brief"},
		{"autopilot with brief", func(r *agents.PlanRequest) { r.Brief = "do it" }, "autopilot has no brief"},
		{"negative cap", func(r *agents.PlanRequest) { r.MaxBeads = -1 }, "max beads per plan -1 is negative"},
		{"no root", func(r *agents.PlanRequest) { r.Repo.Root = "" }, "no repository root"},
		{"relative workdir", func(r *agents.PlanRequest) { r.Workdir = "repo" }, "is not absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := poRequest(dir)
			tc.fn(&req)
			err := req.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, agents.ErrInvalidPlanRequest)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPOPlanRequestDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)
	assert.Equal(t, agents.DefaultMaxBeadsPerPlan, req.BeadLimit())
	assert.Equal(t, 8, agents.DefaultMaxBeadsPerPlan)
	assert.Equal(t, dir, req.Dir())

	req.MaxBeads = 3
	req.Workdir = dir + "/sub"
	assert.Equal(t, 3, req.BeadLimit())
	assert.Equal(t, dir+"/sub", req.Dir())
}

func TestPOPlanPromptSingleBeadCap(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	req.MaxBeads = 1
	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "at most 1 bead,")
	assert.Contains(t, p.Text, "more than 1 bead,")
	assert.NotContains(t, p.Text, "at most 1 beads")
}

func TestPOPlanPromptAutopilot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)
	req.Rules = []prompt.Doc{{Name: "rules.md", Content: "MUST NOT touch generated files."}}
	req.Vibes = []prompt.Doc{{Name: "vibes.md", Content: "Prefer small functions."}}
	req.OpenBeads = []domain.Bead{{ID: "bd-1", Title: "Add exponential backoff to the HTTP client", State: domain.BeadStateOpen}}

	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.Equal(t, domain.RolePO, p.Role)
	assert.Equal(t, events.TaskPlan, p.Task)
	assert.Equal(t, domain.AgentID("po-1"), p.AgentID)

	text := p.Text
	assert.Contains(t, text, "# Product owner — plan")
	assert.Contains(t, text, "You are the product owner")
	assert.Contains(t, text, "read_only")
	assert.Contains(t, text, "MUST NOT touch generated files.")
	assert.Contains(t, text, "Prefer small functions.")
	assert.Contains(t, text, "Add exponential backoff to the HTTP client")
	assert.Contains(t, text, "You are in autopilot")
	assert.Contains(t, text, "at most 8 beads")
	assert.Contains(t, text, "empty `beads` list")
	// Autopilot's product owner pitches where the project goes next, not
	// only what is broken, so a mature repository does not run it dry.
	assert.Contains(t, text, "Maintenance first")
	assert.Contains(t, text, "not only its janitor: pitch new features")
	assert.Contains(t, text, "smallest slice that ships something a user would notice")
	assert.Contains(t, text, "name the directions you considered")
	assert.Contains(t, text, "## How to shape each bead")
	assert.Contains(t, text, "`0` most urgent to `4` least")
	assert.Contains(t, text, "`task`, `bug`, `feature`, `chore`")
	assert.Contains(t, text, "`sc_size`")
	assert.Contains(t, text, "cycle")
	assert.NotContains(t, text, "## Brief from the human")

	// The output contract is the last layer and is the plan schema.
	contract := agents.MustContractFor(domain.MessagePlan)
	assert.True(t, contract.HasPromptSuffix(text))
	assert.Contains(t, text, "sc-result")
	assert.Contains(t, text, "\"acceptance\"")
}

func TestPOPlanPromptBrief(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)
	req.Mode = domain.RunModeBrief
	req.Brief = "Add a --json flag to every command."
	req.MaxBeads = 3
	req.Notes = []string{"The ready queue is empty.", "  "}

	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	text := p.Text
	assert.Contains(t, text, "## Brief from the human")
	assert.Contains(t, text, "Add a --json flag to every command.")
	assert.Contains(t, text, "You are in brief mode")
	assert.Contains(t, text, "Ask no questions")
	assert.Contains(t, text, "at most 3 beads")
	assert.Contains(t, text, "more than 3 beads")
	assert.Contains(t, text, "at least one bead")
	assert.Contains(t, text, "## Notes from the orchestrator")
	assert.Contains(t, text, "- The ready queue is empty.")
	assert.NotContains(t, text, "You are in autopilot")
	assert.NotContains(t, text, "pitch new features", "a brief is the whole scope; nothing is pitched beside it")

	// Deterministic: the same request assembles the same bytes.
	again, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.Equal(t, text, again.Text)
}

func TestPOPlanPromptRejectsBadRequest(t *testing.T) {
	t.Parallel()
	_, err := agents.PlanPrompt(agents.PlanRequest{})
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrInvalidPlanRequest)
}

func TestPOPlanRunSpec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)
	req.Model = "opus"
	req.MaxTurns = 12
	req.Timeout = 90 * time.Second

	spec, p, err := agents.PlanRunSpec(req)
	require.NoError(t, err)
	require.NoError(t, spec.Validate())
	assert.Equal(t, cartridge.ToolReadOnly, spec.ToolPolicy)
	assert.Equal(t, dir, spec.Workdir)
	assert.Equal(t, "opus", spec.Model)
	assert.Equal(t, 12, spec.MaxTurns)
	assert.Equal(t, 90*time.Second, spec.Timeout)
	assert.Equal(t, p.Text, spec.Prompt)
	assert.Equal(t, "po", spec.Labels.Get(cartridge.LabelRole))
	assert.Equal(t, "plan", spec.Labels.Get(cartridge.LabelTask))
	assert.Equal(t, "run-42", spec.Labels.Get(cartridge.LabelRun))
	assert.Equal(t, "po-1", spec.Labels.Get(cartridge.LabelAgent))
	assert.Empty(t, spec.Labels.Get(cartridge.LabelBead))

	// The policy is the one the enforcement mapping expects for this firing.
	want, err := agents.Policy(domain.RolePO, events.TaskPlan)
	require.NoError(t, err)
	assert.Equal(t, want, spec.ToolPolicy)
}

func TestPOValidatePlanAccepts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)
	require.NoError(t, agents.ValidatePlan(req, poPlan()))

	// An empty plan is autopilot's way of saying "nothing worth adding".
	require.NoError(t, agents.ValidatePlan(req, domain.Plan{Brief: "Nothing to add."}))

	// A closed bead with the same title is not a duplicate.
	req.OpenBeads = []domain.Bead{{ID: "bd-9", Title: "Count retries", State: domain.BeadStateClosed}}
	require.NoError(t, agents.ValidatePlan(req, poPlan()))
}

func TestPOValidatePlanRejects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cyclic := domain.Plan{Beads: []domain.PlanBead{
		{Key: "a", Title: "A", Acceptance: "a works", Deps: []string{"b"}},
		{Key: "b", Title: "B", Acceptance: "b works", Deps: []string{"a"}},
	}}
	dangling := domain.Plan{Beads: []domain.PlanBead{
		{Key: "a", Title: "A", Acceptance: "a works", Deps: []string{"ghost"}},
	}}
	noAcceptance := domain.Plan{Beads: []domain.PlanBead{{Key: "a", Title: "A"}}}
	sameTitle := domain.Plan{Beads: []domain.PlanBead{
		{Key: "a", Title: "Count retries", Acceptance: "done"},
		{Key: "b", Title: "count   Retries.", Acceptance: "done"},
	}}
	tooMany := domain.Plan{}
	for i := 0; i < agents.DefaultMaxBeadsPerPlan+1; i++ {
		tooMany.Beads = append(tooMany.Beads, domain.PlanBead{
			Key: fmt.Sprintf("k%d", i), Title: fmt.Sprintf("Bead %d", i), Acceptance: "done",
		})
	}

	cases := []struct {
		name    string
		mutate  func(r *agents.PlanRequest)
		plan    domain.Plan
		want    string
		alsoIs  error
		notWant string
	}{
		{name: "cycle", plan: cyclic, want: "dependency cycle: a -> b -> a", alsoIs: domain.ErrInvalidPlan},
		{name: "dangling dep", plan: dangling, want: "dependency \"ghost\" is not a key in the plan", alsoIs: domain.ErrInvalidPlan},
		{name: "empty acceptance", plan: noAcceptance, want: "empty acceptance criteria", alsoIs: domain.ErrInvalidPlan},
		{
			name: "empty title",
			plan: domain.Plan{Beads: []domain.PlanBead{{Key: "a", Title: "  ", Acceptance: "ok"}}},
			want: "empty title", alsoIs: domain.ErrInvalidPlan,
		},
		{name: "duplicate titles", plan: sameTitle, want: "bead \"b\" repeats the title of bead \"a\""},
		{name: "over the cap", plan: tooMany, want: "plan proposes 9 beads, the limit is 8"},
		{
			name:   "over a configured cap",
			mutate: func(r *agents.PlanRequest) { r.MaxBeads = 2 },
			plan:   poPlan(),
			want:   "plan proposes 3 beads, the limit is 2",
		},
		{
			name: "duplicate of an open bead",
			mutate: func(r *agents.PlanRequest) {
				r.OpenBeads = []domain.Bead{{ID: "bd-7", Title: "  count retries  ", State: domain.BeadStateInProgress}}
			},
			plan: poPlan(),
			want: "bead \"retry-metrics\" duplicates open bead bd-7",
		},
		{
			name:   "brief mode with no beads",
			mutate: func(r *agents.PlanRequest) { r.Mode = domain.RunModeBrief; r.Brief = "do the thing" },
			plan:   domain.Plan{},
			want:   "brief mode plan proposes no beads",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := poRequest(dir)
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			err := agents.ValidatePlan(req, tc.plan)
			require.Error(t, err)
			assert.ErrorIs(t, err, agents.ErrPlanRejected)
			if tc.alsoIs != nil {
				assert.ErrorIs(t, err, tc.alsoIs)
			}
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPODuplicateOpenBeads(t *testing.T) {
	t.Parallel()
	open := []domain.Bead{
		{ID: "bd-1", Title: "Count retries", State: domain.BeadStateOpen},
		{ID: "bd-2", Title: "Document the retry policy!", State: domain.BeadStateChangesRequested},
		{ID: "bd-3", Title: "Add exponential backoff to the HTTP client", State: domain.BeadStateClosed},
		{ID: "bd-4", Title: "   ", State: domain.BeadStateOpen},
	}
	dups := agents.DuplicateOpenBeads(poPlan(), open)
	require.Len(t, dups, 2)
	assert.Equal(t, "retry-metrics", dups[0].Key)
	assert.Equal(t, domain.BeadID("bd-1"), dups[0].Bead)
	assert.Equal(t, "retry-docs", dups[1].Key)
	assert.Equal(t, domain.BeadID("bd-2"), dups[1].Bead)

	assert.Nil(t, agents.DuplicateOpenBeads(domain.Plan{}, open))
	assert.Nil(t, agents.DuplicateOpenBeads(poPlan(), nil))
}

func TestPOParsePlanOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := poRequest(dir)

	plan, parsed, err := agents.ParsePlanOutput(req, poOutput(t, poPlan()))
	require.NoError(t, err)
	assert.Equal(t, "plan", parsed.Contract)
	require.Len(t, plan.Beads, 3)
	assert.Equal(t, "retry-backoff", plan.Beads[0].Key)
	assert.Equal(t, domain.SizeM, plan.Beads[0].Size)
	assert.Equal(t, []string{"retry-backoff"}, plan.Beads[1].Deps)
}

func TestPOParsePlanOutputMissingBlock(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	_, _, err := agents.ParsePlanOutput(req, "I had a think and gave up.")
	require.Error(t, err)
	assert.ErrorIs(t, err, structured.ErrMissing)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.Contains(t, pe.Feedback(), "sc-result")
}

func TestPOParsePlanOutputSchemaFailure(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	block, err := mock.ResultBlock(json.RawMessage(`{"beads":[{"key":"a","title":"A"}]}`))
	require.NoError(t, err)
	_, _, err = agents.ParsePlanOutput(req, block)
	require.Error(t, err)
	assert.ErrorIs(t, err, structured.ErrSchema)
	assert.NotErrorIs(t, err, agents.ErrPlanRejected)
}

func TestPOParsePlanOutputRuleFailure(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	cyclic := domain.Plan{Beads: []domain.PlanBead{
		{Key: "a", Title: "A", Acceptance: "a works", Priority: domain.PriorityDefault, Deps: []string{"b"}},
		{Key: "b", Title: "B", Acceptance: "b works", Priority: domain.PriorityDefault, Deps: []string{"a"}},
	}}
	plan, parsed, err := agents.ParsePlanOutput(req, poOutput(t, cyclic))
	require.Error(t, err)
	// The block itself is returned so the caller can log what the agent said.
	assert.Len(t, plan.Beads, 2)
	assert.NotEmpty(t, parsed.JSON)

	assert.ErrorIs(t, err, agents.ErrPlanRejected)
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
	assert.ErrorIs(t, err, structured.ErrSchema, "a rule failure is still a contract failure P5-08 can retry")
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.Contains(t, pe.Feedback(), "dependency cycle: a -> b -> a")
}

func TestPOParsePlanOutputRejectsBadRequest(t *testing.T) {
	t.Parallel()
	_, _, err := agents.ParsePlanOutput(agents.PlanRequest{}, poOutput(t, poPlan()))
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrInvalidPlanRequest)
}

func TestPOBeadsPlanRequest(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	req.Labels = []string{"sc:wave:1"}
	plan := poPlan()

	got := agents.BeadsPlanRequest(req, plan)
	assert.Equal(t, domain.RunID("run-42"), got.Run)
	assert.Equal(t, plan, got.Plan)
	assert.Empty(t, got.Epic)
	assert.Equal(t, plan.Brief, got.EpicDescription)
	assert.Equal(t, []domain.Role{domain.RolePO}, got.Roles)
	assert.Equal(t, []string{"sc:wave:1"}, got.Labels)
	assert.Contains(t, got.CommitMessage, "run-42")

	req.Epic = "bd-epic"
	assert.Equal(t, domain.BeadID("bd-epic"), agents.BeadsPlanRequest(req, plan).Epic)
}

func TestPOCreatePlanBeads(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	plan := poPlan()
	ap := &poApplier{}

	res, err := agents.CreatePlanBeads(context.Background(), ap, req, plan)
	require.NoError(t, err)
	require.Len(t, ap.requests, 1)
	assert.Equal(t, []bool{false}, ap.dryRuns, "ApplyPlan dry-runs the graph itself (D-22)")
	assert.Len(t, res.Beads, 3)
	assert.Equal(t, domain.BeadID("fx-1"), res.IDs["retry-backoff"])

	// The graph bd would receive: an epic plus the beads, one blocks edge,
	// the origin label and the sc_size metadata.
	graph := res.Graph
	require.Len(t, graph.Nodes, 4)
	assert.Equal(t, domain.BeadTypeEpic, graph.Nodes[0].Type)
	assert.Equal(t, "retry-backoff", graph.Nodes[1].Key)
	assert.Equal(t, graph.Nodes[0].Key, graph.Nodes[1].ParentKey)
	assert.Contains(t, graph.Nodes[1].Labels, "sc:role:po")
	assert.Contains(t, graph.Nodes[1].Labels, "sc:run:run-42")
	assert.Equal(t, "m", graph.Nodes[1].Metadata["sc_size"])
	require.Len(t, graph.Edges, plan.DependencyCount())
	assert.Equal(t, beads.GraphEdge{FromKey: "retry-metrics", ToKey: "retry-backoff", Type: beads.DepBlocks}, graph.Edges[0])
}

func TestPOCreatePlanBeadsGuards(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())

	ap := &poApplier{}
	_, err := agents.CreatePlanBeads(context.Background(), ap, req, domain.Plan{Brief: "Nothing to add."})
	require.ErrorIs(t, err, agents.ErrEmptyPlan)
	assert.Empty(t, ap.requests, "an empty plan creates nothing")

	bad := domain.Plan{Beads: []domain.PlanBead{{Key: "a", Title: "A", Acceptance: "ok", Deps: []string{"a"}}}}
	_, err = agents.CreatePlanBeads(context.Background(), ap, req, bad)
	require.ErrorIs(t, err, agents.ErrPlanRejected)
	assert.Empty(t, ap.requests, "a rejected plan creates nothing")

	_, err = agents.CreatePlanBeads(context.Background(), nil, req, poPlan())
	require.ErrorIs(t, err, agents.ErrInvalidPlanRequest)

	_, err = agents.CreatePlanBeads(context.Background(), ap, agents.PlanRequest{}, poPlan())
	require.ErrorIs(t, err, agents.ErrInvalidPlanRequest)

	boom := &poApplier{err: errors.New("bd exploded")}
	_, err = agents.CreatePlanBeads(context.Background(), boom, req, poPlan())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bd exploded")
}

// TestPOPlansTheFixtureRepoThroughTheMock is the P5-02 exit criterion: the
// product owner fires against the fixture repository through the mock
// cartridge, and the plan it returns becomes beads.
func TestPOPlansTheFixtureRepoThroughTheMock(t *testing.T) {
	t.Parallel()
	repo := testutil.NewFixtureRepo(t)
	rc, err := prompt.ScanRepo(repo.Dir, prompt.ScanOptions{DefaultBranch: repo.DefaultBranch})
	require.NoError(t, err)

	req := poRequest(repo.Dir)
	req.Repo = rc
	req.OpenBeads = []domain.Bead{{ID: "bd-1", Title: "Set up CI", State: domain.BeadStateOpen}}

	spec, p, err := agents.PlanRunSpec(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "Repository root")
	assert.Contains(t, p.Text, "Set up CI")
	assert.True(t, p.Fits(), "the fixture repo must fit the default budget: %v", p.Truncations)

	// The debug copy lands outside the target repository (D-23).
	runDir := t.TempDir()
	require.NoError(t, p.Write(runDir))
	assert.Equal(t, prompt.PathFor(runDir, req.AgentID), p.Path)

	scenario := mock.NewScenario("po-plan")
	scenario.Rule("plan", mock.Match{Role: "po", Task: "plan", ToolPolicy: "read_only", PromptContains: "How to shape each bead"}).
		Say("Reading the repository.\n").
		Structured(poPlanJSON(t, poPlan()))
	scenario.Otherwise("unexpected").Fail(mock.FailCrash, "no rule matched the firing")
	adapter, err := mock.NewAdapter("mock", scenario)
	require.NoError(t, err)

	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(session, nil)
	require.NoError(t, err)

	plan, parsed, err := agents.ParsePlanOutput(req, res.Output)
	require.NoError(t, err)
	assert.Equal(t, "plan", parsed.Contract)
	require.Len(t, plan.Beads, 3)

	ap := &poApplier{}
	created, err := agents.CreatePlanBeads(context.Background(), ap, req, plan)
	require.NoError(t, err)
	assert.Len(t, created.Beads, 3)
	assert.True(t, created.EpicCreated)
	require.Len(t, created.Graph.Edges, 1)

	// The firing was recorded as a read-only product-owner planning call.
	call, ok := adapter.LastCall()
	require.True(t, ok)
	assert.Equal(t, "plan", call.Rule)
	assert.Equal(t, cartridge.ToolReadOnly, call.Spec.ToolPolicy)
	assert.Equal(t, repo.Dir, call.Spec.Workdir)
	assert.True(t, strings.HasSuffix(strings.TrimSpace(res.Output), "```"), "the output ends with the sc-result block")
}

// poGoldenDir holds the two planning-prompt goldens, relative to this
// package.
const poGoldenDir = "testdata/plan"

// poGolden compares got with testdata/plan/<name>.golden.md. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func poGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(poGoldenDir, name+".golden.md")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644)) //nolint:gosec // test fixture
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got,
		"planning prompt differs from %s; run `make update-golden` if the change is intended", path)
}

// poGoldenRequest is the planning firing both goldens are built from: fixed
// values only, so the files never depend on a temporary directory.
func poGoldenRequest(mode domain.RunMode) agents.PlanRequest {
	return agents.PlanRequest{
		Run:     "r-2026-09-03-01",
		AgentID: "po-1",
		Mode:    mode,
		Repo: prompt.RepoContext{
			Root:                 "/work/fixture",
			DefaultBranch:        "main",
			Stack:                []string{"Go", "Make"},
			CheckCommand:         "make test",
			CheckCommandDetected: true,
			Tree:                 []string{"Makefile", "README.md", "go.mod", "stats/", "stats/stats.go", "stats/stats_test.go"},
			TreeTotal:            6,
			TreeDepth:            prompt.DefaultTreeDepth,
			ReadmeName:           "README.md",
			ReadmeHead:           "# fixture\n\nA tiny Go command-line tool that prints statistics for a list of numbers.",
		},
		Rules: []prompt.Doc{{
			Name:    "rules.md",
			Path:    "/work/fixture/rules.md",
			Content: "- Never disable or delete a test to make the suite pass.\n- Every behaviour change lands with a test.",
		}},
		Vibes: []prompt.Doc{{
			Name:    "vibes.md",
			Path:    "/work/fixture/vibes.md",
			Content: "- Prefer short functions with obvious names.",
		}},
		OpenBeads: []domain.Bead{{
			ID:       "sc-7a1",
			Title:    "Mean must not panic on an empty slice",
			Type:     domain.BeadTypeBug,
			Priority: domain.PriorityDefault,
			Size:     domain.SizeS,
			State:    domain.BeadStateOpen,
		}},
	}
}

// poPlanBody returns everything before the output contract. The contract is
// pinned separately (poRequireContract) so that an edit to plan.schema.json,
// which the schemas own, does not rewrite both planning goldens.
func poPlanBody(t *testing.T, p prompt.Prompt) string {
	t.Helper()
	marker := "## Output contract (" + domain.MessagePlan.String() + ")"
	i := strings.Index(p.Text, marker)
	require.GreaterOrEqual(t, i, 0, "prompt has no output contract section")
	return strings.TrimRight(p.Text[:i], "\n") + "\n"
}

// poRequireContract asserts the plan contract is appended verbatim.
func poRequireContract(t *testing.T, p prompt.Prompt) {
	t.Helper()
	assert.True(t, agents.MustContractFor(domain.MessagePlan).HasPromptSuffix(p.Text),
		"the plan output contract is missing")
}

// TestPOPlanPromptGolden pins the whole planning prompt in both modes: the
// body of these prompts is generated rather than templated, so the bead cap
// wording, the priority scale, the plannable types and the dependency and
// rejection rules are only really covered by a golden.
func TestPOPlanPromptGolden(t *testing.T) {
	t.Parallel()

	t.Run("autopilot", func(t *testing.T) {
		t.Parallel()
		p, err := agents.PlanPrompt(poGoldenRequest(domain.RunModeAutopilot))
		require.NoError(t, err)
		assert.True(t, p.Fits(), "the fixture prompt should fit the default budget")
		assert.Empty(t, p.Truncations)
		poGolden(t, "po-plan-autopilot", poPlanBody(t, p))
		poRequireContract(t, p)
	})

	t.Run("brief", func(t *testing.T) {
		t.Parallel()
		req := poGoldenRequest(domain.RunModeBrief)
		req.Brief = "Make the stats command trustworthy: no panics on empty input."
		req.MaxBeads = 3
		req.Notes = []string{"The ready queue is empty; plan the whole brief in one go."}
		p, err := agents.PlanPrompt(req)
		require.NoError(t, err)
		assert.True(t, p.Fits(), "the fixture prompt should fit the default budget")
		assert.Empty(t, p.Truncations)
		poGolden(t, "po-plan-brief", poPlanBody(t, p))
		poRequireContract(t, p)
	})
}

// TestPOPlanRunSpecCarriesTheBeadsShim covers D-21 for a planning firing:
// the product owner runs in the repository itself, so the orchestrator has
// to hand it a read-only `bd`, and the spec must carry that PATH through to
// the cartridge for the promise the prompt makes to be true.
func TestPOPlanRunSpecCarriesTheBeadsShim(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	realBD := filepath.Join(state, "real", "bd")
	require.NoError(t, os.MkdirAll(filepath.Dir(realBD), 0o755))
	require.NoError(t, os.WriteFile(realBD, []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // fake binary
	shim, err := beads.WriteShim(filepath.Join(state, "bin"), beads.ShimOptions{Real: realBD})
	require.NoError(t, err)

	req := poRequest(t.TempDir())
	req.Env = map[string]string{"PATH": "/usr/bin", "SC_RUN": "r-1"}
	req.InheritEnv = []string{"HOME"}
	req.UseShim(shim)
	require.Equal(t, shim.Dir+string(os.PathListSeparator)+"/usr/bin", req.Env["PATH"],
		"UseShim prepends the shim to the PATH already in the map")

	spec, p, err := agents.PlanRunSpec(req)
	require.NoError(t, err)
	assert.Equal(t, req.Env["PATH"], spec.Env["PATH"], "the shim reaches the cartridge")
	assert.Equal(t, "r-1", spec.Env["SC_RUN"])
	assert.Equal(t, []string{"HOME"}, spec.InheritEnv)
	assert.Contains(t, p.Text, beads.PromptNote,
		"the prompt promises a read-only bd, so the spec must ship one")

	// The spec holds a copy: mutating the request afterwards cannot change
	// the environment a firing already built.
	req.Env["PATH"] = "/tmp"
	assert.NotEqual(t, "/tmp", spec.Env["PATH"])

	// UseShim also works on a request with no environment at all.
	fresh := poRequest(t.TempDir())
	fresh.UseShim(shim)
	assert.Equal(t, shim.PathEnv(), fresh.Env["PATH"])
}

// TestPOPlanRunSpecWithoutAShimInheritsPath documents what the doc comment
// on PlanRunSpec warns about: with no shim in Env the child inherits the
// parent's PATH, so installing one is the caller's job.
func TestPOPlanRunSpecWithoutAShimInheritsPath(t *testing.T) {
	t.Parallel()
	spec, _, err := agents.PlanRunSpec(poRequest(t.TempDir()))
	require.NoError(t, err)
	assert.Empty(t, spec.Env)
	assert.Nil(t, spec.InheritEnv, "nil means cartridge.DefaultInheritEnv()")
}

// TestPOParsePlanOutputRuleFailureProblems pins the retry feedback P5-08
// sends back: one bullet per broken rule, with the wrapper sentinels
// dropped and nothing glued together.
func TestPOParsePlanOutputRuleFailureProblems(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	// Two rules at once, one from each layer: domain.Plan.Validate finds the
	// cycle, ValidatePlan finds the repeated title.
	broken := domain.Plan{Brief: "Two problems.", Beads: []domain.PlanBead{
		{Key: "a", Title: "Same title", Acceptance: "a works", Priority: domain.PriorityDefault, Deps: []string{"b"}},
		{Key: "b", Title: "Same title", Acceptance: "b works", Priority: domain.PriorityDefault, Deps: []string{"a"}},
	}}
	_, _, err := agents.ParsePlanOutput(req, poOutput(t, broken))
	require.Error(t, err)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.Equal(t, []string{
		"dependency cycle: a -> b -> a",
		`bead "b" repeats the title of bead "a"`,
	}, pe.Problems)
	for _, p := range pe.Problems {
		assert.NotContains(t, p, "\n", "each problem is one line")
		assert.NotContains(t, p, "agents: plan rejected", "the bare sentinel is not feedback")
	}
	assert.Contains(t, pe.Feedback(), "\n- dependency cycle: a -> b -> a")
	assert.NotContains(t, pe.Feedback(), "- agents: plan rejected")
}

// TestPOParsePlanOutputProblemsPerRule checks the flattening on the other
// rules too: the bead cap and a duplicate of an open bead each contribute
// exactly one line.
func TestPOParsePlanOutputProblemsPerRule(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	req.MaxBeads = 1
	req.OpenBeads = []domain.Bead{{ID: "bd-9", Title: "Count retries", State: domain.BeadStateOpen}}
	_, _, err := agents.ParsePlanOutput(req, poOutput(t, poPlan()))
	require.Error(t, err)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.Equal(t, []string{
		"plan proposes 3 beads, the limit is 1",
		`bead "retry-metrics" duplicates open bead bd-9 ("Count retries")`,
	}, pe.Problems)
}

// With research on, autopilot researches comparable products before it
// pitches, and never pitches manipulative engagement mechanics either way.
func TestPOPlanPromptResearch(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "WebSearch", "without research the product owner is not told about tools it lacks")
	assert.Contains(t, p.Text, "never manipulative engagement mechanics")
	assert.Contains(t, p.Text, "3. Then decide where the project goes next")

	req.Research = true
	p, err = agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "3. Research the market before you pitch anything")
	assert.Contains(t, p.Text, "WebSearch and WebFetch")
	assert.Contains(t, p.Text, "never instructions to follow")
	assert.Contains(t, p.Text, "4. Then decide where the project goes next")
	assert.Contains(t, p.Text, "7. Return an empty `beads` list")

	req.Mode, req.Brief = domain.RunModeBrief, "Add a --json flag."
	p, err = agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "Research the market", "a brief is the whole scope")
}

// The product owner is offered a per-bead timeout only when the run allows
// one longer than its default, and is told it is not an estimate.
func TestPOPlanPromptOffersABeadTimeout(t *testing.T) {
	t.Parallel()
	req := poRequest(t.TempDir())
	req.Timeout, req.MaxTimeout = 6*time.Hour, 24*time.Hour
	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "- `timeout`: leave it out. Every agent session has 6h.")
	assert.Contains(t, p.Text, "up to 24h, such as `12h`")
	assert.Contains(t, p.Text, "It is not an estimate")

	req.MaxTimeout = 0
	p, err = agents.PlanPrompt(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "- `timeout`")
}
