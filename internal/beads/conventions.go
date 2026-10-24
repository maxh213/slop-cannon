package beads

import (
	"context"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Labels slop-cannon attaches to beads (D-06). Labels are only used for
// cheap filtering (`bd list -l`); everything that matters lives in the
// status and the sc_* metadata.
const (
	// LabelPrefix starts every slop-cannon label.
	LabelPrefix = "sc:"
	// LabelExternalPR marks a review bead the cannon created for a pull
	// request it did not open (D-11).
	LabelExternalPR = "sc:external-pr"

	labelRunPrefix  = "sc:run:"
	labelRolePrefix = "sc:role:"
)

// RunLabel returns the `sc:run:<run-id>` label every bead of a run carries.
func RunLabel(run domain.RunID) string { return labelRunPrefix + string(run) }

// RoleLabel returns the `sc:role:<role>` label recording which role last
// worked a bead.
func RoleLabel(role domain.Role) string { return labelRolePrefix + string(role) }

// ParseRunLabel returns the run id of an `sc:run:<run-id>` label.
func ParseRunLabel(label string) (domain.RunID, bool) {
	rest, ok := strings.CutPrefix(label, labelRunPrefix)
	if !ok || rest == "" {
		return "", false
	}
	return domain.RunID(rest), true
}

// ParseRoleLabel returns the role of an `sc:role:<role>` label; a label
// naming an unknown role is not one.
func ParseRoleLabel(label string) (domain.Role, bool) {
	rest, ok := strings.CutPrefix(label, labelRolePrefix)
	if !ok {
		return "", false
	}
	role, err := domain.ParseRole(rest)
	if err != nil {
		return "", false
	}
	return role, true
}

// IsSCLabel reports whether label belongs to slop-cannon (`sc:` prefix).
func IsSCLabel(label string) bool { return strings.HasPrefix(label, LabelPrefix) }

// ActorFor returns the bd actor an agent writes as: `sc:<agent-id>`
// (D-06). The orchestrator's own plumbing uses DefaultActor.
func ActorFor(agent domain.AgentID) string { return LabelPrefix + string(agent) }

// AgentOf returns the agent id behind an `sc:<agent-id>` actor. The bare
// DefaultActor and actors of other tools yield false.
func AgentOf(actor string) (domain.AgentID, bool) {
	rest, ok := strings.CutPrefix(actor, LabelPrefix)
	if !ok || rest == "" {
		return "", false
	}
	return domain.AgentID(rest), true
}

// Values of the sc_kind metadata key (D-06).
const (
	// KindEpic marks the one epic bead that owns a run (D-22).
	KindEpic = "epic"
	// KindWork marks a bead the cannon implements and reviews.
	KindWork = "work"
)

// RunEpicPriority is the priority of a run epic: one notch above the
// default so `bd list` shows the epic ahead of its children.
const RunEpicPriority = domain.Priority(1)

// RunMetadata returns the metadata every bead of a run starts with:
// sc_run and sc_kind.
func RunMetadata(run domain.RunID, kind string) map[string]any {
	return map[string]any{MetaRun: string(run), MetaKind: kind}
}

// EpicMetadata returns the metadata of a run epic.
func EpicMetadata(run domain.RunID) map[string]any { return RunMetadata(run, KindEpic) }

// WorkMetadata returns the metadata a work bead is created with: sc_run,
// sc_kind, sc_round 0 and, when known, the PO's sc_size.
func WorkMetadata(run domain.RunID, size domain.Size) map[string]any {
	m := RunMetadata(run, KindWork)
	m[MetaRound] = 0
	if size != "" {
		m[MetaSize] = string(size)
	}
	return m
}

// RoundMetadata returns the metadata recording round review rounds done.
func RoundMetadata(round int) map[string]any { return map[string]any{MetaRound: round} }

// PRMetadata returns the metadata recording the pull request opened for a
// bead (D-10). A zero number is left out.
func PRMetadata(url string, number int) map[string]any {
	m := map[string]any{MetaPRURL: url}
	if number != 0 {
		m[MetaPRNumber] = number
	}
	return m
}

// MergeMetadata returns the metadata recording the commit that landed on
// the default branch.
func MergeMetadata(sha string) map[string]any { return map[string]any{MetaMergeSHA: sha} }

// LeaseMetadata returns the metadata a claim writes (P3-05): the agent
// holding the bead and the RFC 3339 UTC time the lease was last renewed.
func LeaseMetadata(agent domain.AgentID, at time.Time) map[string]any {
	return map[string]any{MetaAgent: string(agent), MetaLease: at.UTC().Format(time.RFC3339)}
}

// LeaseKeys are the metadata keys a release removes (Client.UnsetMetadata).
var LeaseKeys = []string{MetaAgent, MetaLease}

// MergeMeta combines metadata maps left to right; later maps win.
func MergeMeta(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// RunID returns the run the issue belongs to: the sc_run metadata, else
// the sc:run label, else "".
func (i Issue) RunID() domain.RunID {
	if s, ok := i.Metadata.String(MetaRun); ok && s != "" {
		return domain.RunID(s)
	}
	for _, l := range i.Labels {
		if run, ok := ParseRunLabel(l); ok {
			return run
		}
	}
	return ""
}

// InRun reports whether the issue belongs to run.
func (i Issue) InRun(run domain.RunID) bool { return run != "" && i.RunID() == run }

// Kind returns the sc_kind metadata (KindEpic, KindWork), or "" for a bead
// the cannon did not create.
func (i Issue) Kind() string {
	s, _ := i.Metadata.String(MetaKind)
	return s
}

// IsRunEpic reports whether the issue is a run's epic: an epic bead
// carrying sc_kind "epic".
func (i Issue) IsRunEpic() bool { return i.IsEpic() && i.Kind() == KindEpic }

// IsExternalPR reports whether the issue reviews an external pull request.
func (i Issue) IsExternalPR() bool { return i.HasLabel(LabelExternalPR) }

// Round returns the sc_round metadata (review rounds done), 0 when absent.
func (i Issue) Round() int {
	n, _ := i.Metadata.Int(MetaRound)
	return n
}

// PRURL returns the sc_pr_url metadata, or "".
func (i Issue) PRURL() string {
	s, _ := i.Metadata.String(MetaPRURL)
	return s
}

// PRNumber returns the sc_pr_number metadata, or 0.
func (i Issue) PRNumber() int {
	n, _ := i.Metadata.Int(MetaPRNumber)
	return n
}

// PRHead returns the sc_pr_head commit, or "" when none is recorded.
func (i Issue) PRHead() string {
	s, _ := i.Metadata.String(MetaPRHead)
	return s
}

// MergeSHA returns the sc_merge_sha metadata, or "".
func (i Issue) MergeSHA() string {
	s, _ := i.Metadata.String(MetaMergeSHA)
	return s
}

// Agent returns the agent recorded in sc_agent (the claim holder), or "".
func (i Issue) Agent() domain.AgentID {
	s, _ := i.Metadata.String(MetaAgent)
	return domain.AgentID(s)
}

// Timeout returns the sc_timeout the product owner gave the bead; false
// when absent, unparsable or not positive.
func (i Issue) Timeout() (time.Duration, bool) {
	s, ok := i.Metadata.String(MetaTimeout)
	if !ok || s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Lease returns the sc_lease time; false when absent or unparsable.
func (i Issue) Lease() (time.Time, bool) {
	s, ok := i.Metadata.String(MetaLease)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse(time.RFC3339Nano, s); err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}

// Size returns the sc_size metadata parsed leniently; false when absent
// or not an S/M/L value.
func (i Issue) Size() (domain.Size, bool) {
	s, ok := i.Metadata.String(MetaSize)
	if !ok {
		return "", false
	}
	size, err := domain.ParseSize(s)
	if err != nil {
		return "", false
	}
	return size, true
}

// Roles returns the roles named by the issue's sc:role labels, in label
// order.
func (i Issue) Roles() []domain.Role {
	var out []domain.Role
	for _, l := range i.Labels {
		if role, ok := ParseRoleLabel(l); ok {
			out = append(out, role)
		}
	}
	return out
}

// SetMetadata merges meta into the issue's metadata through `bd update
// --metadata @file`, so values keep their JSON types (numbers stay
// numbers, P3-13).
func (c *Client) SetMetadata(ctx context.Context, id domain.BeadID, meta map[string]any) (Issue, error) {
	if len(meta) == 0 {
		return Issue{}, fmt.Errorf("beads: set metadata %s: %w: no keys", id, ErrInvalid)
	}
	return c.Update(ctx, id, Patch{Metadata: meta})
}

// UnsetMetadata removes keys from the issue's metadata.
func (c *Client) UnsetMetadata(ctx context.Context, id domain.BeadID, keys ...string) (Issue, error) {
	if len(keys) == 0 {
		return Issue{}, fmt.Errorf("beads: unset metadata %s: %w: no keys", id, ErrInvalid)
	}
	return c.Update(ctx, id, Patch{UnsetMetadata: keys})
}

// AddLabels attaches labels to the issue.
func (c *Client) AddLabels(ctx context.Context, id domain.BeadID, labels ...string) (Issue, error) {
	if len(labels) == 0 {
		return Issue{}, fmt.Errorf("beads: add labels %s: %w: no labels", id, ErrInvalid)
	}
	return c.Update(ctx, id, Patch{AddLabels: labels})
}

// RemoveLabels detaches labels from the issue.
func (c *Client) RemoveLabels(ctx context.Context, id domain.BeadID, labels ...string) (Issue, error) {
	if len(labels) == 0 {
		return Issue{}, fmt.Errorf("beads: remove labels %s: %w: no labels", id, ErrInvalid)
	}
	return c.Update(ctx, id, Patch{RemoveLabels: labels})
}

// ListRun lists the beads of one run (`bd list -l sc:run:<id>`), the
// filter P3-14 recommends so foreign beads never reach the converter.
// With all, closed beads are included. The epic is part of the result.
func (c *Client) ListRun(ctx context.Context, run domain.RunID, all bool) ([]Issue, error) {
	if run == "" {
		return nil, fmt.Errorf("beads: list run: %w: empty run id", ErrInvalid)
	}
	issues, err := c.List(ctx, ListFilter{All: all, Labels: []string{RunLabel(run)}})
	if err != nil {
		return nil, err
	}
	out := issues[:0]
	for _, is := range issues {
		if is.InRun(run) {
			out = append(out, is)
		}
	}
	return out, nil
}

// RunEpicInput describes the epic bead a run owns (D-22).
type RunEpicInput struct {
	// Run is required; it becomes the sc_run metadata and the sc:run label.
	Run domain.RunID
	// Title defaults to RunEpicTitle(run).
	Title string
	// Description is the human's brief or the autopilot's summary.
	Description string
}

// RunEpicTitle returns the default title of a run's epic.
func RunEpicTitle(run domain.RunID) string { return "Run " + string(run) }

// CreateRunEpic creates the epic bead that owns every work bead of a run
// (D-22): type epic, RunEpicPriority, the sc:run label and the
// EpicMetadata. Work beads are created as its children (GraphNode.ParentKey
// or BeadInput.Parent) so `bd ready --parent <epic>` shows the run.
func (c *Client) CreateRunEpic(ctx context.Context, in RunEpicInput) (Issue, error) {
	if in.Run == "" {
		return Issue{}, fmt.Errorf("beads: create run epic: %w: empty run id", ErrInvalid)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = RunEpicTitle(in.Run)
	}
	return c.Create(ctx, BeadInput{
		Title:       title,
		Type:        domain.BeadTypeEpic,
		Priority:    RunEpicPriority,
		Description: in.Description,
		Labels:      []string{RunLabel(in.Run)},
		Metadata:    EpicMetadata(in.Run),
	})
}

// FindRunEpic returns the epic of run (for resume): the bead labelled
// sc:run:<id> with sc_kind "epic", closed or not. A run without one is
// ErrNotFound; should a repository somehow hold several, the oldest wins.
func (c *Client) FindRunEpic(ctx context.Context, run domain.RunID) (Issue, error) {
	if run == "" {
		return Issue{}, fmt.Errorf("beads: find run epic: %w: empty run id", ErrInvalid)
	}
	issues, err := c.List(ctx, ListFilter{All: true, Labels: []string{RunLabel(run)}, Metadata: map[string]string{MetaKind: KindEpic}})
	if err != nil {
		return Issue{}, err
	}
	var found *Issue
	for k := range issues {
		is := &issues[k]
		if !is.IsRunEpic() || !is.InRun(run) {
			continue
		}
		if found == nil || is.CreatedAt.Before(found.CreatedAt) {
			found = is
		}
	}
	if found == nil {
		return Issue{}, fmt.Errorf("beads: run %s has no epic: %w", run, ErrNotFound)
	}
	return *found, nil
}
