package beads

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Metadata keys slop-cannon writes on beads (D-06). The `bd:` and `_`
// prefixes are reserved by beads, so every key carries the `sc_` prefix.
const (
	// MetaRun is the run id that created the bead.
	MetaRun = "sc_run"
	// MetaRound is the number of review rounds completed so far.
	MetaRound = "sc_round"
	// MetaPRURL is the pull request opened for the bead.
	MetaPRURL = "sc_pr_url"
	// MetaPRNumber is the pull request number.
	MetaPRNumber = "sc_pr_number"
	// MetaPRHead is the bead branch's tip the run delivered to its pull
	// request: the commit the testers approved (D-31).
	MetaPRHead = "sc_pr_head"
	// MetaMergeSHA is the commit that landed on the default branch.
	MetaMergeSHA = "sc_merge_sha"
	// MetaAgent is the agent id holding the claim.
	MetaAgent = "sc_agent"
	// MetaLease is the RFC 3339 time the claim was last renewed.
	MetaLease = "sc_lease"
	// MetaKind distinguishes the run epic ("epic") from work beads ("work").
	MetaKind = "sc_kind"
	// MetaSize is the PO's S/M/L estimate (domain.Size).
	MetaSize = "sc_size"
	// MetaTimeout is the longer agent session limit the PO gave the bead
	// in its plan (a Go duration such as "10h").
	MetaTimeout = "sc_timeout"
)

// StatusCustomKey is the bd config key holding the custom statuses (D-07).
const StatusCustomKey = "status.custom"

// Metadata is a bead's metadata object as bd returns it. Values keep their
// JSON types: `--set-metadata sc_round=1` is stored as the number 1, while
// the same key written through a JSON file may be a string, so the accessors
// accept both (P3-13).
type Metadata map[string]any

// String returns the value under key rendered as a string: strings as they
// are, numbers without a trailing ".0", booleans as "true"/"false". The
// second result is false when the key is absent or null.
func (m Metadata) String(key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x), true
		}
		return string(b), true
	}
}

// Int returns the value under key as an integer, accepting a JSON number
// (whole) or a numeric string. The second result is false when the key is
// absent, null or not a whole number.
func (m Metadata) Int(key string) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) {
			return 0, false
		}
		return int(x), true
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, false
		}
		return n, true
	case int:
		return x, true
	case int64:
		return int(x), true
	default:
		return 0, false
	}
}

// Bool returns the value under key as a boolean, accepting a JSON bool or
// the strings "true"/"false" (and what strconv.ParseBool accepts).
func (m Metadata) Bool(key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(x))
		if err != nil {
			return false, false
		}
		return b, true
	default:
		return false, false
	}
}

// Issue is a bd issue as `bd show/list/ready/blocked/export --json` print
// it. It is the raw shape: Status and Type stay strings so a repository's
// own custom statuses and bd's extra issue types decode without error; Bead
// converts to the domain model.
type Issue struct {
	// ID is the bd-minted identifier ("fx-nvi", hierarchical "fx-nvi.1").
	ID domain.BeadID `json:"id"`
	// Title is the one-line summary.
	Title string `json:"title"`
	// Description is the body text.
	Description string `json:"description,omitempty"`
	// Acceptance holds the acceptance criteria.
	Acceptance string `json:"acceptance_criteria,omitempty"`
	// Status is the bd status name (built-in or custom).
	Status string `json:"status"`
	// Priority is on bd's 0 (highest) to 4 scale; absent means 2.
	Priority domain.Priority `json:"priority"`
	// Type is the bd issue type ("task", "epic", "gate", …).
	Type string `json:"issue_type,omitempty"`
	// Owner and CreatedBy are bd's bookkeeping of who made the issue.
	Owner     string `json:"owner,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	// Assignee is the actor holding the claim ("sc:engineer-1").
	Assignee string `json:"assignee,omitempty"`
	// CreatedAt, UpdatedAt, StartedAt and ClosedAt are bd's timestamps; the
	// zero time means bd did not print the field.
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	// CloseReason is the `bd close --reason` text.
	CloseReason string `json:"close_reason,omitempty"`
	// ExternalRef is the `--external-ref` value: where the bead came from
	// outside beads ("gh-9" for a GitHub pull request). bd echoes it on
	// create and show (P3-19).
	ExternalRef string `json:"external_ref,omitempty"`
	// Metadata is the custom metadata object (sc_* keys, D-06).
	Metadata Metadata `json:"metadata,omitempty"`
	// Labels are the bd labels.
	Labels []string `json:"labels,omitempty"`
	// Parent is the parent issue when bd printed one.
	Parent domain.BeadID `json:"parent,omitempty"`
	// Dependencies are the edges pointing from this issue at what it depends
	// on (blockers and its parent), normalised so DependsOnID is always the
	// other issue whichever shape bd printed.
	Dependencies []Dependency `json:"dependencies,omitempty"`
	// Dependents are the edges of issues depending on this one
	// (`show --include-dependents`), normalised so IssueID is the other issue.
	Dependents []Dependency `json:"dependents,omitempty"`
	// Comments are present with `show --include-comments` and in exports.
	Comments []Comment `json:"comments,omitempty"`
	// BlockedBy lists the open blockers reported by `bd blocked`.
	BlockedBy []domain.BeadID `json:"blocked_by,omitempty"`
	// Counts bd prints alongside listings.
	DependencyCount int `json:"dependency_count,omitempty"`
	DependentCount  int `json:"dependent_count,omitempty"`
	CommentCount    int `json:"comment_count,omitempty"`
	BlockedByCount  int `json:"blocked_by_count,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler: a missing priority decodes as
// domain.PriorityDefault and the dependency edges are normalised to the
// issue's id (see Dependency).
func (i *Issue) UnmarshalJSON(data []byte) error {
	type plain Issue
	v := plain{Priority: domain.PriorityDefault}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	for k := range v.Dependencies {
		v.Dependencies[k].normalise(v.ID, false)
	}
	for k := range v.Dependents {
		v.Dependents[k].normalise(v.ID, true)
	}
	*i = Issue(v)
	return nil
}

// IsEpic reports whether the issue is an epic. `bd ready` lists the run
// epic itself alongside its children (P3-13), so readiness callers skip it.
func (i Issue) IsEpic() bool { return i.Type == string(domain.BeadTypeEpic) }

// ParentID returns the parent issue: the `parent` field when bd printed it,
// else the target of the parent-child dependency, else "".
func (i Issue) ParentID() domain.BeadID {
	if i.Parent != "" {
		return i.Parent
	}
	for _, d := range i.Dependencies {
		if d.Type == DepParentChild {
			return d.DependsOnID
		}
	}
	return ""
}

// Blockers returns the issues this one depends on through `blocks` edges,
// in the order bd printed them: the readiness dependencies (D-20). The
// blocked_by list of `bd blocked` is included when the edges are absent.
func (i Issue) Blockers() []domain.BeadID {
	var out []domain.BeadID
	seen := map[domain.BeadID]bool{}
	for _, d := range i.Dependencies {
		if d.Type == DepBlocks && d.DependsOnID != "" && !seen[d.DependsOnID] {
			seen[d.DependsOnID] = true
			out = append(out, d.DependsOnID)
		}
	}
	for _, id := range i.BlockedBy {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// HasLabel reports whether the issue carries label.
func (i Issue) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// Bead converts the issue to the domain model: status and type are parsed
// with the domain enums, the blockers become Deps, and the sc_* metadata is
// surfaced as Round, PRURL, PRNumber and Size. A status or type outside the
// domain vocabulary (a foreign custom status, P3-14) is an error wrapping
// domain.ErrUnknownValue; callers listing a whole repository should filter
// by run label first or skip such issues.
//
// The prose fields are normalised on the way out (domain.Bead.Sanitized,
// P3-21): control characters are stripped from the title, description and
// acceptance criteria, and invalid UTF-8 is dropped. bd holds whatever was
// written into it — by another tool, by a person pasting an escape
// sequence into `bd create` — and from here the text travels into prompts,
// TUI frames and the event log, so this is the boundary that normalises
// it. Refusing the bead is not an option: a run must not be stranded by a
// bead someone else wrote (P0-11 rejects only text an agent just invented).
func (i Issue) Bead() (domain.Bead, error) {
	state, err := domain.ParseBeadState(i.Status)
	if err != nil {
		return domain.Bead{}, fmt.Errorf("bead %s: %w", i.ID, err)
	}
	typ := domain.BeadTypeTask
	if i.Type != "" {
		typ, err = domain.ParseBeadType(i.Type)
		if err != nil {
			return domain.Bead{}, fmt.Errorf("bead %s: %w", i.ID, err)
		}
	}
	b := domain.Bead{
		ID:          i.ID,
		Title:       i.Title,
		Description: i.Description,
		Acceptance:  i.Acceptance,
		Type:        typ,
		Priority:    i.Priority,
		Deps:        i.Blockers(),
		State:       state,
		Assignee:    i.Assignee,
		Labels:      append([]string(nil), i.Labels...),
	}
	if s, ok := i.Metadata.String(MetaSize); ok {
		if size, err := domain.ParseSize(s); err == nil {
			b.Size = size
		}
	}
	if n, ok := i.Metadata.Int(MetaRound); ok {
		b.Round = n
	}
	if s, ok := i.Metadata.String(MetaPRURL); ok {
		b.PRURL = s
	}
	if n, ok := i.Metadata.Int(MetaPRNumber); ok {
		b.PRNumber = n
	}
	return b.Sanitized(), nil
}

// Dependency types bd uses (Appendix A). Only DepBlocks affects readiness.
const (
	DepBlocks      = "blocks"
	DepParentChild = "parent-child"
	DepWaitsFor    = "waits-for"
	DepRelated     = "related"
	DepTracks      = "tracks"
	DepDiscovered  = "discovered-from"
)

// Dependency is one edge of the graph: IssueID depends on DependsOnID.
//
// bd prints edges in two shapes (P3-13): the edge form
// `{"issue_id","depends_on_id","type",…}` (list, ready, export, `dep list`
// with several ids) and the issue form `{"id","title","status",…,
// "dependency_type"}` where the id is the other end (show, `dep list <id>`).
// Decoding accepts both; the enclosing Issue (or Client.DepList) fills in
// the side the issue form leaves implicit, so IssueID and DependsOnID are
// always set on values the client returns.
type Dependency struct {
	// IssueID is the dependent issue.
	IssueID domain.BeadID `json:"issue_id"`
	// DependsOnID is the issue it depends on (its blocker or parent).
	DependsOnID domain.BeadID `json:"depends_on_id"`
	// Type is the dependency type (DepBlocks, DepParentChild, …).
	Type string `json:"type"`
	// Title and Status describe the other end when bd printed the issue form.
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
	// CreatedAt and CreatedBy are bd's bookkeeping when it printed them.
	CreatedAt time.Time `json:"created_at,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`

	// other is the id from the issue form, before normalisation.
	other domain.BeadID
}

// UnmarshalJSON implements json.Unmarshaler for both shapes bd prints.
func (d *Dependency) UnmarshalJSON(data []byte) error {
	var probe struct {
		ID             domain.BeadID `json:"id"`
		IssueID        domain.BeadID `json:"issue_id"`
		DependsOnID    domain.BeadID `json:"depends_on_id"`
		Type           string        `json:"type"`
		DependencyType string        `json:"dependency_type"`
		Title          string        `json:"title"`
		Status         string        `json:"status"`
		CreatedAt      time.Time     `json:"created_at"`
		CreatedBy      string        `json:"created_by"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	*d = Dependency{
		IssueID:     probe.IssueID,
		DependsOnID: probe.DependsOnID,
		Type:        probe.Type,
		Title:       probe.Title,
		Status:      probe.Status,
		CreatedAt:   probe.CreatedAt,
		CreatedBy:   probe.CreatedBy,
		other:       probe.ID,
	}
	if probe.DependencyType != "" {
		d.Type = probe.DependencyType
	}
	if d.Type == "" {
		d.Type = DepBlocks
	}
	return nil
}

// MarshalJSON implements json.Marshaler with the edge form.
func (d Dependency) MarshalJSON() ([]byte, error) {
	type plain struct {
		IssueID     domain.BeadID `json:"issue_id"`
		DependsOnID domain.BeadID `json:"depends_on_id"`
		Type        string        `json:"type"`
		Title       string        `json:"title,omitempty"`
		Status      string        `json:"status,omitempty"`
		CreatedAt   *time.Time    `json:"created_at,omitempty"`
		CreatedBy   string        `json:"created_by,omitempty"`
	}
	p := plain{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type, Title: d.Title, Status: d.Status, CreatedBy: d.CreatedBy}
	if !d.CreatedAt.IsZero() {
		t := d.CreatedAt
		p.CreatedAt = &t
	}
	return json.Marshal(p)
}

// normalise fills in the implicit side of an issue-form edge: for an
// issue's dependencies the other id is what it depends on; for its
// dependents the other id is the dependent.
func (d *Dependency) normalise(self domain.BeadID, dependents bool) {
	other := d.other
	d.other = ""
	if other == "" {
		return
	}
	if dependents {
		if d.IssueID == "" {
			d.IssueID = other
		}
		if d.DependsOnID == "" {
			d.DependsOnID = self
		}
	} else {
		if d.DependsOnID == "" {
			d.DependsOnID = other
		}
		if d.IssueID == "" {
			d.IssueID = self
		}
	}
}

// Comment is one bd comment on an issue.
type Comment struct {
	// ID is bd's comment id (a UUID).
	ID string `json:"id"`
	// IssueID is the issue the comment belongs to.
	IssueID domain.BeadID `json:"issue_id"`
	// Author is the actor that wrote it (BEADS_ACTOR at the time).
	Author string `json:"author"`
	// Text is the comment body.
	Text string `json:"text"`
	// CreatedAt is when bd stored it.
	CreatedAt time.Time `json:"created_at"`
}

// BeadInput describes a bead to create with Client.Create. Priority must be
// set explicitly (the zero value is the highest priority, as in
// domain.Bead); use domain.PriorityDefault when there is no opinion.
type BeadInput struct {
	// Title is required.
	Title string
	// Type is the issue type; empty means task.
	Type domain.BeadType
	// Priority is on bd's 0 (highest) to 4 scale and is always passed.
	Priority domain.Priority
	// Description and Acceptance are the body texts.
	Description string
	Acceptance  string
	// Labels are attached at creation (`-l a,b`).
	Labels []string
	// Metadata is written through `--metadata @file` so values keep their
	// JSON types.
	Metadata map[string]any
	// Parent makes the bead a hierarchical child of that issue (`--parent`).
	Parent domain.BeadID
	// ExternalRef records where the bead came from outside beads
	// (`--external-ref`): "gh-9" for a GitHub pull request, "jira-ABC", a
	// Linear URL. sc sets it on the review bead of an external pull
	// request (D-11) beside the sc_external_ref metadata, so bd's own
	// tooling can find the bead from the reference too (P3-19). Empty
	// means the flag is not passed.
	ExternalRef string
}

// Patch describes an update for Client.Update. Only set fields are sent;
// IsEmpty reports whether there is anything to send.
type Patch struct {
	// Status sets the bd status (`-s`); empty means unchanged.
	Status domain.BeadState
	// Assignee sets the assignee (`--assignee`); nil means unchanged and a
	// pointer to "" clears it (the D-07 release).
	Assignee *string
	// Priority sets the priority (`-p`); nil means unchanged.
	Priority *domain.Priority
	// Acceptance replaces the acceptance criteria (`--acceptance`); nil
	// means unchanged.
	Acceptance *string
	// Parent reparents the bead (`--parent`); nil means unchanged and a
	// pointer to "" removes the parent, which is how bd 1.2.2 spells it.
	// A plan built by BuildGraphPlan sets the parent in the graph plan
	// instead (GraphNode.ParentID), so this is for the beads sc adopts
	// later — a review bead joining a run epic, a repair after a partial
	// plan.
	Parent *domain.BeadID
	// SetMetadata sets keys with `--set-metadata k=v`. bd types the values
	// itself: "1" is stored as the number 1, "true" as a boolean, anything
	// else as a string (P3-13). Use Metadata to control the type.
	SetMetadata map[string]string
	// Metadata merges a JSON object into the metadata through
	// `--metadata @file`, keeping the values' JSON types.
	Metadata map[string]any
	// UnsetMetadata removes keys (`--unset-metadata`).
	UnsetMetadata []string
	// AddLabels and RemoveLabels adjust the labels (`--add-label`,
	// `--remove-label`).
	AddLabels    []string
	RemoveLabels []string
}

// IsEmpty reports whether the patch changes nothing.
func (p Patch) IsEmpty() bool {
	return p.Status == "" && p.Assignee == nil && p.Priority == nil && p.Acceptance == nil &&
		p.Parent == nil && len(p.SetMetadata) == 0 && len(p.Metadata) == 0 && len(p.UnsetMetadata) == 0 &&
		len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
}

// ListFilter narrows Client.List. The zero value lists every non-closed
// issue in the repository.
type ListFilter struct {
	// All includes closed issues (`--all`).
	All bool
	// Statuses restricts to these statuses (`-s a,b`, comma-joined because
	// bd overwrites a repeated -s).
	Statuses []domain.BeadState
	// Labels requires every listed label (`-l`, AND semantics).
	Labels []string
	// Parent restricts to children of that issue (`--parent`).
	Parent domain.BeadID
	// Metadata requires each key to have the given value
	// (`--metadata-field k=v`, repeatable).
	Metadata map[string]string
}

// GraphPlan is the JSON `bd create --graph` accepts (D-22, D-24): nodes
// with plan-local keys and dependencies as top-level edges.
type GraphPlan struct {
	// CommitMessage is the optional Dolt commit message.
	CommitMessage string `json:"commit_message,omitempty"`
	// Nodes are the beads to create.
	Nodes []GraphNode `json:"nodes"`
	// Edges are the dependencies: FromKey depends on ToKey.
	Edges []GraphEdge `json:"edges,omitempty"`
}

// GraphNode is one bead of a graph plan. bd 1.2.2 drops node-level
// `acceptance_criteria` and `deps` with a warning (D-24), so neither is
// here; set acceptance criteria with Client.Update afterwards.
type GraphNode struct {
	// Key is the plan-local identifier used by edges and ParentKey.
	Key string `json:"key"`
	// Title is required.
	Title string `json:"title"`
	// Type is the issue type; empty means task.
	Type domain.BeadType `json:"type,omitempty"`
	// Priority is on bd's scale and is always emitted.
	Priority domain.Priority `json:"priority"`
	// Description is the body text.
	Description string `json:"description,omitempty"`
	// Labels and Metadata are attached at creation. Every metadata value
	// must be a JSON string: bd 1.2.2 fails the whole `bd create --graph`
	// call on a number ("cannot unmarshal number into Go struct field
	// GraphApplyPlan.nodes.N.metadata.<key> of type string"), so build the
	// map with GraphMetadata (P3-11).
	Labels   []string       `json:"labels,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// ParentKey makes the node a child of another node of the same plan
	// (the run epic when the plan mints it).
	ParentKey string `json:"parent_key,omitempty"`
	// ParentID makes the node a child of a bead that already exists — the
	// run epic of an earlier plan — so joining an existing epic is part of
	// the one atomic create rather than a `bd update --parent` afterwards
	// (P3-18). A node carrying either parent is counted in the dry run's
	// ParentDeps.
	ParentID domain.BeadID `json:"parent_id,omitempty"`
	// Assignee sets the bead's assignee at creation. sc leaves it empty:
	// beads are claimed with `bd update --claim` (D-07), which is the
	// atomic path.
	Assignee string `json:"assignee,omitempty"`
}

// GraphEdge is one dependency of a graph plan: FromKey depends on ToKey,
// matching `bd dep add <issue> <depends-on>`.
type GraphEdge struct {
	FromKey string `json:"from_key"`
	ToKey   string `json:"to_key"`
	// Type is the dependency type; empty means DepBlocks.
	Type string `json:"type,omitempty"`
}

// GraphResult is what `bd create --graph` reported. A dry run fills the
// counts, notes and echoed nodes; a real run fills IDs.
type GraphResult struct {
	// IDs maps plan keys to the minted ids (real run only).
	IDs map[string]domain.BeadID `json:"ids,omitempty"`
	// DryRun is true for `--dry-run` output.
	DryRun bool `json:"dry_run,omitempty"`
	// NodeCount, EdgeCount and ParentDeps are the dry-run counts; P3-10
	// compares EdgeCount with the plan's dependency count (D-24).
	NodeCount  int `json:"node_count,omitempty"`
	EdgeCount  int `json:"edge_count,omitempty"`
	ParentDeps int `json:"parent_deps,omitempty"`
	// ValidationNotes are bd's dry-run caveats.
	ValidationNotes []string `json:"validation_notes,omitempty"`
	// Nodes echoes the nodes as bd understood them (dry run).
	Nodes []GraphNode `json:"nodes,omitempty"`
	// Warnings are the `warning:` lines bd printed about the plan (unknown
	// fields silently dropped); D-24 says any of them fails the plan build.
	Warnings []string `json:"-"`
}

// DepResult is what `bd dep add` reported.
type DepResult struct {
	IssueID     domain.BeadID `json:"issue_id"`
	DependsOnID domain.BeadID `json:"depends_on_id"`
	// Status is "added".
	Status string `json:"status"`
	Type   string `json:"type"`
}

// StatusInfo describes one status from `bd statuses --json`.
type StatusInfo struct {
	Name        string `json:"name"`
	Category    string `json:"category"`
	Icon        string `json:"icon,omitempty"`
	Description string `json:"description,omitempty"`
}

// StatusList is the payload of `bd statuses --json`.
type StatusList struct {
	BuiltIn []StatusInfo `json:"built_in_statuses"`
	Custom  []StatusInfo `json:"custom_statuses"`
}

// Has reports whether name is a built-in or custom status.
func (l StatusList) Has(name string) bool {
	for _, s := range l.BuiltIn {
		if s.Name == name {
			return true
		}
	}
	for _, s := range l.Custom {
		if s.Name == name {
			return true
		}
	}
	return false
}

// CustomStatuses returns the custom statuses as CustomStatus values.
func (l StatusList) CustomStatuses() []CustomStatus {
	out := make([]CustomStatus, 0, len(l.Custom))
	for _, s := range l.Custom {
		out = append(out, CustomStatus{Name: s.Name, Category: domain.StateCategory(s.Category)})
	}
	return out
}

// CustomStatus is one entry of the `status.custom` config value.
type CustomStatus struct {
	Name     string
	Category domain.StateCategory
}

// String renders the entry as bd stores it: "name:category".
func (s CustomStatus) String() string { return s.Name + ":" + string(s.Category) }

// FormatCustomStatuses renders statuses as the `status.custom` value:
// "name:category,…" in the given order.
func FormatCustomStatuses(statuses []CustomStatus) string {
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		parts[i] = s.String()
	}
	return strings.Join(parts, ",")
}

// ParseCustomStatuses parses a `status.custom` value ("name:category,…").
// An entry without a category is an error; whitespace is tolerated.
func ParseCustomStatuses(value string) ([]CustomStatus, error) {
	var out []CustomStatus
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, cat, ok := strings.Cut(part, ":")
		name, cat = strings.TrimSpace(name), strings.TrimSpace(cat)
		if !ok || name == "" || cat == "" {
			return nil, fmt.Errorf("beads: malformed custom status %q (want name:category)", part)
		}
		category, err := domain.ParseStateCategory(cat)
		if err != nil {
			return nil, fmt.Errorf("beads: custom status %q: %w", part, err)
		}
		out = append(out, CustomStatus{Name: name, Category: category})
	}
	return out, nil
}

// MergeCustomStatuses unions existing with wanted: entries already present
// keep their position and category (D-07 never replaces a repository's own
// statuses), missing ones are appended in wanted's order.
func MergeCustomStatuses(existing, wanted []CustomStatus) []CustomStatus {
	out := append([]CustomStatus(nil), existing...)
	seen := make(map[string]bool, len(out))
	for _, s := range out {
		seen[s.Name] = true
	}
	for _, s := range wanted {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	return out
}

// WorkflowCustomStatuses returns the D-07 custom statuses with their
// categories, in flow order, ready for SetCustomStatuses.
func WorkflowCustomStatuses() []CustomStatus {
	states := domain.CustomBeadStates()
	out := make([]CustomStatus, 0, len(states))
	for _, s := range states {
		out = append(out, CustomStatus{Name: string(s), Category: s.Category()})
	}
	return out
}

// ConfigValue is the payload of `bd config get/set --json`. An unset key
// comes back with an empty Value and no error.
type ConfigValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// IssueIDs returns the ids of issues in order.
func IssueIDs(issues []Issue) []domain.BeadID {
	out := make([]domain.BeadID, len(issues))
	for i, is := range issues {
		out[i] = is.ID
	}
	return out
}

// sortedKeys returns the keys of m in order, for deterministic argument
// lists.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
