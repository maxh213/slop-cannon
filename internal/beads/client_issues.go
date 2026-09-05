package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"slop-cannon/internal/domain"
)

// Create runs `bd create <title> -t <type> -p <priority> [--description]
// [--acceptance] [--external-ref <ref>] [-l a,b] [--metadata @file]
// [--parent <id>] --json` and returns the new issue. Metadata is written
// through a temporary file so its values keep their JSON types.
func (c *Client) Create(ctx context.Context, in BeadInput) (Issue, error) {
	if strings.TrimSpace(in.Title) == "" {
		return Issue{}, fmt.Errorf("beads: create: %w: empty title", ErrInvalid)
	}
	typ := in.Type
	if typ == "" {
		typ = domain.BeadTypeTask
	}
	args := []string{"create", in.Title, "-t", string(typ), "-p", strconv.Itoa(int(in.Priority))}
	if in.Description != "" {
		args = append(args, "--description", in.Description)
	}
	if in.Acceptance != "" {
		args = append(args, "--acceptance", in.Acceptance)
	}
	if in.ExternalRef != "" {
		args = append(args, "--external-ref", in.ExternalRef)
	}
	if len(in.Labels) > 0 {
		args = append(args, "-l", strings.Join(in.Labels, ","))
	}
	if len(in.Metadata) > 0 {
		path, cleanup, err := writeJSONFile("sc-metadata-*.json", in.Metadata)
		if err != nil {
			return Issue{}, err
		}
		defer cleanup()
		args = append(args, "--metadata", "@"+path)
	}
	if in.Parent != "" {
		args = append(args, "--parent", string(in.Parent))
	}
	args = append(args, "--json")
	var is Issue
	if err := c.call(ctx, &is, args...); err != nil {
		return Issue{}, err
	}
	return is, nil
}

// CreateGraph applies a graph plan with `bd create --graph <file> [--dry-run]
// --json` (D-22, D-24). With dryRun the result carries bd's counts and
// notes and nothing is created; otherwise IDs maps plan keys to minted
// ids. Warnings holds the `warning:` lines bd printed about the plan
// (unknown node fields are silently dropped); P3-10 fails the build on any.
func (c *Client) CreateGraph(ctx context.Context, plan GraphPlan, dryRun bool) (GraphResult, error) {
	if len(plan.Nodes) == 0 {
		return GraphResult{}, fmt.Errorf("beads: create --graph: %w: plan has no nodes", ErrInvalid)
	}
	path, cleanup, err := writeJSONFile("sc-plan-*.json", plan)
	if err != nil {
		return GraphResult{}, err
	}
	defer cleanup()
	args := []string{"create", "--graph", path}
	if dryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, "--json")
	res, err := c.run(ctx, args...)
	if err != nil {
		return GraphResult{}, err
	}
	var out GraphResult
	if err := Decode(res.Stdout, &out); err != nil {
		return GraphResult{}, fmt.Errorf("bd create --graph: %w", err)
	}
	out.Warnings = GraphWarnings(res.Stderr)
	return out, nil
}

// GraphWarnings returns the `warning:` lines of a `bd create --graph`
// stderr that concern the plan, without the unrelated beads.role notice bd
// prints on every write in some repositories.
func GraphWarnings(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "warning:") || strings.Contains(line, "beads.role") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Show runs `bd show <ids…> --json --include-comments` and returns the
// issues (bd answers with an array even for one id). When bd found only
// some of the ids (it exits 0 and reports the rest on stderr), the found
// issues are returned together with an ErrNotFound error naming the
// missing ids; an unknown single id is ErrNotFound with no issues.
func (c *Client) Show(ctx context.Context, ids ...domain.BeadID) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("beads: show: %w: no ids", ErrInvalid)
	}
	args := []string{"show"}
	for _, id := range ids {
		args = append(args, string(id))
	}
	args = append(args, "--json", "--include-comments")
	res, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := Decode(res.Stdout, &issues); err != nil {
		return nil, fmt.Errorf("bd show: %w", err)
	}
	found := make(map[domain.BeadID]bool, len(issues))
	for _, is := range issues {
		found[is.ID] = true
	}
	var missing []string
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, string(id))
		}
	}
	if len(missing) > 0 {
		msg := StderrMessage(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("no issue found matching %s", strings.Join(missing, ", "))
		}
		return issues, &Error{Command: "show", Args: args, Kind: ErrNotFound, Message: msg, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return issues, nil
}

// List runs `bd list --json -n 0 --flat [--all] [-s a,b] [-l …] [--parent]
// [--metadata-field k=v]…`. The limit and flat flags are always passed
// because bd defaults to 50 issues in tree form (Appendix A).
func (c *Client) List(ctx context.Context, f ListFilter) ([]Issue, error) {
	args := []string{"list", "--json", "-n", "0", "--flat"}
	if f.All {
		args = append(args, "--all")
	}
	if len(f.Statuses) > 0 {
		names := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			names[i] = string(s)
		}
		args = append(args, "-s", strings.Join(names, ","))
	}
	if len(f.Labels) > 0 {
		args = append(args, "-l", strings.Join(f.Labels, ","))
	}
	if f.Parent != "" {
		args = append(args, "--parent", string(f.Parent))
	}
	for _, k := range sortedKeys(f.Metadata) {
		args = append(args, "--metadata-field", k+"="+f.Metadata[k])
	}
	var issues []Issue
	if err := c.call(ctx, &issues, args...); err != nil {
		return nil, err
	}
	return issues, nil
}

// Ready runs `bd ready --json -n 0 [--parent <id>]` and returns the
// claimable issues: status open with every blocker closed. bd lists the
// run epic itself alongside its children (P3-13), so epics are filtered
// out; parent narrows the set to descendants of that issue.
func (c *Client) Ready(ctx context.Context, parent domain.BeadID) ([]Issue, error) {
	args := []string{"ready", "--json", "-n", "0"}
	if parent != "" {
		args = append(args, "--parent", string(parent))
	}
	var issues []Issue
	if err := c.call(ctx, &issues, args...); err != nil {
		return nil, err
	}
	out := issues[:0]
	for _, is := range issues {
		if !is.IsEpic() {
			out = append(out, is)
		}
	}
	return out, nil
}

// Blocked runs `bd blocked --json` and returns the issues that cannot
// proceed, each with BlockedBy naming its open blockers.
func (c *Client) Blocked(ctx context.Context) ([]Issue, error) {
	var issues []Issue
	if err := c.call(ctx, &issues, "blocked", "--json"); err != nil {
		return nil, err
	}
	return issues, nil
}

// Update runs `bd update <id> … --json` with the flags the patch sets and
// returns the updated issue. An empty patch is an error.
func (c *Client) Update(ctx context.Context, id domain.BeadID, p Patch) (Issue, error) {
	if p.IsEmpty() {
		return Issue{}, fmt.Errorf("beads: update %s: %w: empty patch", id, ErrInvalid)
	}
	args := []string{"update", string(id)}
	if p.Status != "" {
		args = append(args, "-s", string(p.Status))
	}
	if p.Assignee != nil {
		args = append(args, "--assignee", *p.Assignee)
	}
	if p.Priority != nil {
		args = append(args, "-p", strconv.Itoa(int(*p.Priority)))
	}
	if p.Acceptance != nil {
		args = append(args, "--acceptance", *p.Acceptance)
	}
	if p.Parent != nil {
		args = append(args, "--parent", string(*p.Parent))
	}
	for _, k := range sortedKeys(p.SetMetadata) {
		args = append(args, "--set-metadata", k+"="+p.SetMetadata[k])
	}
	if len(p.Metadata) > 0 {
		path, cleanup, err := writeJSONFile("sc-metadata-*.json", p.Metadata)
		if err != nil {
			return Issue{}, err
		}
		defer cleanup()
		args = append(args, "--metadata", "@"+path)
	}
	for _, k := range p.UnsetMetadata {
		args = append(args, "--unset-metadata", k)
	}
	for _, l := range p.AddLabels {
		args = append(args, "--add-label", l)
	}
	for _, l := range p.RemoveLabels {
		args = append(args, "--remove-label", l)
	}
	args = append(args, "--json")
	return c.callIssue(ctx, args...)
}

// Claim runs `bd update <id> --claim --json` as actor (an agent id; empty
// means the client's own actor): bd atomically sets the assignee and the
// in_progress status, is idempotent for the same actor, and fails with
// ErrAlreadyClaimed when someone else holds the bead.
func (c *Client) Claim(ctx context.Context, id domain.BeadID, actor string) (Issue, error) {
	return c.WithActor(actor).callIssue(ctx, "update", string(id), "--claim", "--json")
}

// Close runs `bd close <id> --reason <reason> --json`. bd refuses to close
// a bead whose blockers are open (D-20); that surfaces as ErrBlockedOpen
// with the blockers named in the message. Closing a closed bead is a
// no-op that returns the issue.
func (c *Client) Close(ctx context.Context, id domain.BeadID, reason string) (Issue, error) {
	args := []string{"close", string(id)}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	args = append(args, "--json")
	return c.callIssue(ctx, args...)
}

// callIssue runs a write that answers with an array holding the affected
// issue and returns that issue.
func (c *Client) callIssue(ctx context.Context, args ...string) (Issue, error) {
	var issues []Issue
	if err := c.call(ctx, &issues, args...); err != nil {
		return Issue{}, err
	}
	if len(issues) == 0 {
		return Issue{}, fmt.Errorf("bd %s: %w", commandName(args), errNoIssue)
	}
	return issues[0], nil
}

// Comment runs `bd comment <id> --stdin --json` with body on standard
// input (the only form that takes arbitrary text; `bd comments add -f -`
// does not read stdin) and returns the stored comment. The author is the
// client's actor, so agents comment through WithActor. An empty body is
// ErrInvalid.
func (c *Client) Comment(ctx context.Context, id domain.BeadID, body string) (Comment, error) {
	if strings.TrimSpace(body) == "" {
		return Comment{}, fmt.Errorf("beads: comment %s: %w: empty body", id, ErrInvalid)
	}
	res, err := c.runStdin(ctx, strings.NewReader(body), "comment", string(id), "--stdin", "--json")
	if err != nil {
		return Comment{}, err
	}
	var cm Comment
	if err := Decode(res.Stdout, &cm); err != nil {
		return Comment{}, fmt.Errorf("bd comment: %w", err)
	}
	return cm, nil
}

// Comments runs `bd comments <id> --json` and returns the comments in
// creation order.
func (c *Client) Comments(ctx context.Context, id domain.BeadID) ([]Comment, error) {
	var out []Comment
	if err := c.call(ctx, &out, "comments", string(id), "--json"); err != nil {
		return nil, err
	}
	return out, nil
}

// AddDep runs `bd dep add <issue> <depends-on> --json`: issue depends on
// (is blocked by) dependsOn, the only direction sc ever writes (D-20).
// A cycle is ErrCycle. Note that bd 1.2.2 accepts an unknown depends-on id
// without complaint, so callers creating edges to ids they did not mint
// should Show them first.
func (c *Client) AddDep(ctx context.Context, issue, dependsOn domain.BeadID) (DepResult, error) {
	if issue == "" || dependsOn == "" {
		return DepResult{}, fmt.Errorf("beads: dep add: %w: empty id", ErrInvalid)
	}
	var out DepResult
	if err := c.call(ctx, &out, "dep", "add", string(issue), string(dependsOn), "--json"); err != nil {
		return DepResult{}, err
	}
	return out, nil
}

// DepList runs `bd dep list <ids…> --json` and returns the edges of the
// given issues with IssueID and DependsOnID always set: bd prints the
// issue form for one id and the edge form for several (P3-13).
func (c *Client) DepList(ctx context.Context, ids ...domain.BeadID) ([]Dependency, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("beads: dep list: %w: no ids", ErrInvalid)
	}
	args := []string{"dep", "list"}
	for _, id := range ids {
		args = append(args, string(id))
	}
	args = append(args, "--json")
	var deps []Dependency
	if err := c.call(ctx, &deps, args...); err != nil {
		return nil, err
	}
	for i := range deps {
		deps[i].normalise(ids[0], false)
	}
	return deps, nil
}

// writeJSONFile marshals v into a temporary file and returns its path and
// a cleanup function, for bd's `@file` arguments.
func writeJSONFile(pattern string, v any) (string, func(), error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", nil, fmt.Errorf("beads: encoding %s: %w", pattern, err)
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("beads: creating temp file: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("beads: writing %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("beads: writing %s: %w", filepath.Base(path), err)
	}
	return path, cleanup, nil
}
