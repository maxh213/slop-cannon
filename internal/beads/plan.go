package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"slop-cannon/internal/domain"
)

// Plan application (P3-10, D-22, D-24): a validated PO plan becomes beads
// through one `bd create --graph` call, dry-run first.
//
// Facts verified against bd 1.2.2 (P3-11, 2026-09-02):
//   - Accepted node fields are key, title, type, priority, description,
//     labels, metadata, parent_key, parent_id and assignee. Every spelling
//     of acceptance criteria (acceptance, acceptance_criteria, …), notes,
//     design, owner, id and node-level deps are "unknown field(s)" that bd
//     drops with a stderr warning, so acceptance criteria are written with
//     `bd update --acceptance` right after creation.
//   - Dependencies go in top-level edges[{from_key,to_key,type:"blocks"}]
//     (from depends on to); a plan with a diamond of five dependencies dry
//     runs with edge_count 5 and lands with the matching blocked_by lists
//     (testdata/bd/graph-*.json). to_id references an existing bead.
//   - Graph-plan metadata values must be JSON strings: a number fails the
//     whole call with "cannot unmarshal number into Go struct field
//     GraphApplyPlan.nodes.N.metadata.<key> of type string". GraphMetadata
//     converts, and Issue metadata accessors read "0" and 0 alike (P3-13).
//   - parent_deps counts the nodes carrying a parent, through either
//     parent_key (an epic minted by the same plan) or parent_id (a run epic
//     that already exists, P3-18). Both go in the plan, so a bead is never
//     created outside its epic and no `bd update --parent` follows.

// Sentinel errors of plan application. Every failure of BuildGraphPlan,
// VerifyDryRun and ApplyPlan wraps one of these, domain.ErrInvalidPlan, or
// a *Error from the bd call that failed.
var (
	// ErrPlanWarning means bd printed a `warning:` line about the plan on
	// the dry run (typically a field it silently dropped); D-24 says the
	// build fails rather than losing data.
	ErrPlanWarning = errors.New("bd warned about the graph plan")
	// ErrPlanEdgeCount means the dry run's edge_count differs from the
	// number of dependencies in the plan (D-24, P3-15).
	ErrPlanEdgeCount = errors.New("dry-run edge count does not match the plan")
	// ErrPlanNodeCount means the dry run's node_count differs from the
	// number of nodes sent.
	ErrPlanNodeCount = errors.New("dry-run node count does not match the plan")
	// ErrPlanIDs means the real run did not return an id for every key.
	ErrPlanIDs = errors.New("bd did not return an id for every plan key")
)

// PlanEpicKey is the graph-plan key of the run epic when ApplyPlan creates
// it together with the work beads. Should a plan bead use the same key, the
// epic key is prefixed with "run-" until it is unique (PlanResult.EpicKey
// says which key was used).
const PlanEpicKey = "epic"

// PlanRequest describes how a PO plan becomes beads of a run.
type PlanRequest struct {
	// Run is the run the beads belong to (required): it becomes the sc_run
	// metadata and the sc:run:<id> label of every bead.
	Run domain.RunID
	// Plan is the PO's proposal; it must pass domain.Plan.Validate and hold
	// at least one bead.
	Plan domain.Plan
	// Epic is the run's existing epic (from CreateRunEpic or FindRunEpic).
	// When set, the beads are created as its children; when empty, an epic
	// node is created in the same graph plan (EpicTitle, EpicDescription).
	Epic domain.BeadID
	// EpicTitle names the epic created for the run; empty means
	// RunEpicTitle(Run). Ignored when Epic is set.
	EpicTitle string
	// EpicDescription is the epic's body; empty means Plan.Brief. Ignored
	// when Epic is set.
	EpicDescription string
	// Roles are the origin roles labelled on every work bead
	// (sc:role:<role>); P5-02 passes domain.RolePO.
	Roles []domain.Role
	// Labels are extra labels for every work bead.
	Labels []string
	// CommitMessage is the Dolt commit message; empty means a generated one.
	CommitMessage string
}

// PlanResult is what ApplyPlan reports.
type PlanResult struct {
	// Run is the run the beads belong to.
	Run domain.RunID
	// Epic is the run epic the beads are children of: the requested one or
	// the one minted by the plan (empty on a dry run that would create it).
	Epic domain.BeadID
	// EpicCreated is true when the epic was part of the graph plan.
	EpicCreated bool
	// EpicKey is the graph-plan key of the created epic, "" otherwise.
	EpicKey string
	// Graph is the plan exactly as bd received it.
	Graph GraphPlan
	// DryRun is bd's dry-run report, already verified (VerifyDryRun).
	DryRun GraphResult
	// IDs maps every plan key to its minted bead id (nil on a dry run).
	IDs map[string]domain.BeadID
	// Beads are the minted ids in plan order (nil on a dry run).
	Beads []domain.BeadID
	// Unfinished lists beads that were created but whose acceptance
	// criteria could not be written before an error; the returned error
	// says why. Empty on success.
	Unfinished []domain.BeadID
	// Warnings are `warning:` lines bd printed on the real create call.
	// The dry run of the very same plan is verified first (any warning
	// there fails the build, D-24), so this stays empty in practice; when
	// it does not, bd dropped something and the caller should say so.
	// ApplyPlan also logs them at Warn.
	Warnings []string
}

// planBuild is the converted plan plus what the graph cannot carry.
type planBuild struct {
	graph      GraphPlan
	epicKey    string
	keys       []string
	acceptance map[string]string
}

// BuildGraphPlan converts a validated plan into the graph-plan JSON bd
// accepts (D-22, D-24): one node per bead with the run label, the role
// labels, WorkMetadata (string-valued, see GraphMetadata) and, when the
// request creates the epic, parent_key pointing at the epic node; one
// blocks edge per dependency, so len(Edges) == req.Plan.DependencyCount().
// When the request names an existing epic, every work node carries it as
// parent_id instead (P3-18), so joining an epic is part of the same atomic
// create. Acceptance criteria are not part of the result (bd 1.2.2 drops
// them); ApplyPlan writes them afterwards. Errors wrap ErrInvalid (missing run,
// empty plan) or domain.ErrInvalidPlan.
func BuildGraphPlan(req PlanRequest) (GraphPlan, error) {
	b, err := buildPlan(req)
	if err != nil {
		return GraphPlan{}, err
	}
	return b.graph, nil
}

// buildPlan is BuildGraphPlan keeping the epic key, the plan order and the
// acceptance criteria for ApplyPlan.
func buildPlan(req PlanRequest) (planBuild, error) {
	if strings.TrimSpace(string(req.Run)) == "" {
		return planBuild{}, fmt.Errorf("beads: build plan: %w: empty run id", ErrInvalid)
	}
	if err := req.Plan.Validate(); err != nil {
		return planBuild{}, fmt.Errorf("beads: build plan: %w", err)
	}
	if len(req.Plan.Beads) == 0 {
		return planBuild{}, fmt.Errorf("beads: build plan: %w: plan has no beads", ErrInvalid)
	}
	b := planBuild{
		keys:       make([]string, 0, len(req.Plan.Beads)),
		acceptance: make(map[string]string, len(req.Plan.Beads)),
	}
	b.graph.CommitMessage = strings.TrimSpace(req.CommitMessage)
	if b.graph.CommitMessage == "" {
		b.graph.CommitMessage = fmt.Sprintf("sc: plan %d beads for run %s", len(req.Plan.Beads), req.Run)
	}
	epic := strings.TrimSpace(string(req.Epic))
	if epic == "" {
		b.epicKey = planEpicKey(req.Plan)
		title := strings.TrimSpace(req.EpicTitle)
		if title == "" {
			title = RunEpicTitle(req.Run)
		}
		desc := req.EpicDescription
		if strings.TrimSpace(desc) == "" {
			desc = req.Plan.Brief
		}
		b.graph.Nodes = append(b.graph.Nodes, GraphNode{
			Key:         b.epicKey,
			Title:       title,
			Type:        domain.BeadTypeEpic,
			Priority:    RunEpicPriority,
			Description: desc,
			Labels:      []string{RunLabel(req.Run)},
			Metadata:    GraphMetadata(EpicMetadata(req.Run)),
		})
	}
	labels := planLabels(req)
	for _, pb := range req.Plan.Beads {
		typ := pb.Type
		if typ == "" {
			typ = domain.BeadTypeTask
		}
		b.graph.Nodes = append(b.graph.Nodes, GraphNode{
			Key:         pb.Key,
			Title:       pb.Title,
			Type:        typ,
			Priority:    pb.Priority,
			Description: pb.Description,
			Labels:      append([]string(nil), labels...),
			Metadata:    GraphMetadata(planBeadMetadata(req.Run, pb)),
			ParentKey:   b.epicKey,
			ParentID:    domain.BeadID(epic),
		})
		for _, dep := range pb.Deps {
			b.graph.Edges = append(b.graph.Edges, GraphEdge{FromKey: pb.Key, ToKey: dep, Type: DepBlocks})
		}
		b.keys = append(b.keys, pb.Key)
		b.acceptance[pb.Key] = pb.Acceptance
	}
	return b, nil
}

// planEpicKey returns PlanEpicKey, prefixed with "run-" as often as needed
// to avoid the plan's own keys.
func planEpicKey(p domain.Plan) string {
	used := make(map[string]bool, len(p.Beads))
	for _, b := range p.Beads {
		used[b.Key] = true
	}
	key := PlanEpicKey
	for used[key] {
		key = "run-" + key
	}
	return key
}

// planLabels returns the labels of every work bead: the run label, one
// role label per role, then the extra labels, deduplicated in that order
// with blanks dropped.
func planLabels(req PlanRequest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(l string) {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}
	add(RunLabel(req.Run))
	for _, r := range req.Roles {
		if r != "" {
			add(RoleLabel(r))
		}
	}
	for _, l := range req.Labels {
		add(l)
	}
	return out
}

// GraphMetadata returns a copy of m whose values are all strings, the only
// value type bd 1.2.2's graph-plan parser accepts for node metadata (a
// number fails the whole `bd create --graph` call). Values are rendered as
// Metadata.String does: numbers without a trailing ".0", booleans as
// "true"/"false"; nil values are dropped. Use it on every GraphNode.Metadata.
func GraphMetadata(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k := range m {
		if s, ok := Metadata(m).String(k); ok {
			out[k] = s
		}
	}
	return out
}

// VerifyDryRun checks bd's `--dry-run` report for plan against D-24: any
// `warning:` line is ErrPlanWarning (bd dropped something), node_count must
// equal len(plan.Nodes) (ErrPlanNodeCount) and edge_count must equal
// len(plan.Edges) (ErrPlanEdgeCount), which for a plan built by
// BuildGraphPlan is Plan.DependencyCount() (P3-15).
func VerifyDryRun(plan GraphPlan, res GraphResult) error {
	if len(res.Warnings) > 0 {
		return fmt.Errorf("beads: plan dry run: %w: %s", ErrPlanWarning, strings.Join(res.Warnings, "; "))
	}
	if res.NodeCount != len(plan.Nodes) {
		return fmt.Errorf("beads: plan dry run: %w: bd counted %d nodes, plan has %d", ErrPlanNodeCount, res.NodeCount, len(plan.Nodes))
	}
	if res.EdgeCount != len(plan.Edges) {
		return fmt.Errorf("beads: plan dry run: %w: bd counted %d edges, plan has %d dependencies", ErrPlanEdgeCount, res.EdgeCount, len(plan.Edges))
	}
	return nil
}

// ApplyPlan turns a validated PO plan into beads (D-22): it builds the
// graph plan (BuildGraphPlan), runs `bd create --graph --dry-run` and
// verifies the report (VerifyDryRun), then, unless dryRun is set, creates
// every bead in one atomic call and finally writes each bead's acceptance
// criteria with `bd update --acceptance` (bd 1.2.2 drops them from the
// plan). The epic is always part of the create — as a node of the plan, or
// as every work node's parent_id when it already existed (P3-18) — so no
// bead is ever left parentless by a failure here.
//
// With dryRun the result carries Graph and DryRun only and nothing is
// written. A failure before the real call leaves beads untouched. A
// failure while finalising returns the result with IDs, Beads and
// Unfinished filled in together with the error, so the caller can report
// or retry; the beads already carry the run label and metadata.
func (c *Client) ApplyPlan(ctx context.Context, req PlanRequest, dryRun bool) (PlanResult, error) {
	b, err := buildPlan(req)
	if err != nil {
		return PlanResult{}, err
	}
	out := PlanResult{
		Run:         req.Run,
		Epic:        domain.BeadID(strings.TrimSpace(string(req.Epic))),
		EpicCreated: b.epicKey != "",
		EpicKey:     b.epicKey,
		Graph:       b.graph,
	}
	if !out.EpicCreated {
		// Fail before anything is created rather than leave orphans behind
		// when the reparenting update would reject the epic.
		issues, err := c.Show(ctx, out.Epic)
		if err != nil {
			return out, fmt.Errorf("beads: apply plan: epic %s: %w", out.Epic, err)
		}
		if len(issues) == 0 || !issues[0].IsEpic() {
			return out, fmt.Errorf("beads: apply plan: %w: %s is not an epic", ErrInvalid, out.Epic)
		}
	}
	dry, err := c.CreateGraph(ctx, b.graph, true)
	if err != nil {
		return out, err
	}
	out.DryRun = dry
	if err := VerifyDryRun(b.graph, dry); err != nil {
		return out, err
	}
	if dryRun {
		return out, nil
	}
	real, err := c.CreateGraph(ctx, b.graph, false)
	if err != nil {
		return out, err
	}
	if len(real.Warnings) > 0 {
		// The identical plan dry ran without a warning, so this means bd
		// dropped something the dry run accepted: never silent (D-24).
		out.Warnings = append([]string(nil), real.Warnings...)
		c.logger.WarnContext(ctx, "bd warned about the applied graph plan",
			"repo", c.repo, "run", string(req.Run), "warnings", strings.Join(real.Warnings, "; "))
	}
	out.IDs = make(map[string]domain.BeadID, len(b.keys))
	var missing []string
	for _, n := range b.graph.Nodes {
		id, ok := real.IDs[n.Key]
		if !ok || id == "" {
			missing = append(missing, n.Key)
			continue
		}
		if n.Key == b.epicKey {
			out.Epic = id
			continue
		}
		out.IDs[n.Key] = id
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("beads: create --graph: %w: missing %s", ErrPlanIDs, strings.Join(missing, ", "))
	}
	out.Beads = make([]domain.BeadID, 0, len(b.keys))
	for _, k := range b.keys {
		out.Beads = append(out.Beads, out.IDs[k])
	}
	for i, k := range b.keys {
		id := out.IDs[k]
		acceptance := b.acceptance[k]
		if _, err := c.Update(ctx, id, Patch{Acceptance: &acceptance}); err != nil {
			out.Unfinished = append([]domain.BeadID(nil), out.Beads[i:]...)
			return out, fmt.Errorf("beads: plan created but finalising %s (key %q) failed: %w", id, k, err)
		}
	}
	return out, nil
}

// planBeadMetadata is WorkMetadata plus the sc_timeout the plan asked for.
func planBeadMetadata(run domain.RunID, pb domain.PlanBead) map[string]any {
	m := WorkMetadata(run, pb.Size)
	if t := strings.TrimSpace(pb.Timeout); t != "" {
		m[MetaTimeout] = t
	}
	return m
}
