// Command fakebd is the stateful `bd` stand-in the orchestrator's
// end-to-end test (P6-09) puts on PATH.
//
// It speaks the part of bd 1.2.2's command line and JSON envelope that
// internal/beads uses (D-21), against a tiny JSON database in the
// directory named by SC_E2E_FAKEBD_DIR, so a whole run can plan, claim,
// review, gate and close beads with the real beads.Client, the real
// argument contract and the real decoding — on a machine with no bd and no
// embedded Dolt, and fast enough for `go test -race`.
//
// It lives under testdata/ so the go tool never builds it as part of the
// module; internal/orchestrator's e2e test builds it on demand. It is a
// test fixture, not a bd replacement: it implements what the cannon calls
// and nothing else, and any other command exits non-zero and says so.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// dirEnv names the database directory. The shim exports it.
const dirEnv = "SC_E2E_FAKEBD_DIR"

func main() {
	dir := os.Getenv(dirEnv)
	if dir == "" {
		fmt.Fprintf(os.Stderr, "fakebd: %s is not set\n", dirEnv)
		os.Exit(2)
	}
	os.Exit(helper(dir, os.Args[1:]))
}

// epoch is the fixed instant the whole scenario runs at; nothing in the
// test reads the wall clock for anything that ends up in an assertion.
var epoch = time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

// dep is one dependency edge: From is blocked by To (D-20).
type dep struct {
	From domain.BeadID `json:"from"`
	To   domain.BeadID `json:"to"`
}

// gateRecord is a gate plus the bead it blocks, which bd itself only
// records in the description.
type gateRecord struct {
	// Gate is a named field rather than an embedded one: beads.Gate has a
	// custom UnmarshalJSON, and embedding would promote it to the record
	// and swallow BlocksID on the way back in.
	Gate     beads.Gate    `json:"gate"`
	BlocksID domain.BeadID `json:"blocks_id"`
}

// db is the fake tracker's whole database, rewritten on every call.
type db struct {
	Prefix   string            `json:"prefix"`
	Seq      int               `json:"seq"`
	Calls    int               `json:"calls"`
	Config   map[string]string `json:"config"`
	Issues   []beads.Issue     `json:"issues"`
	Comments []beads.Comment   `json:"comments"`
	Deps     []dep             `json:"deps"`
	Gates    []gateRecord      `json:"gates"`
}

// valueFlags are the bd flags that consume the next argument, so a
// value is never mistaken for a positional.
var valueFlags = map[string]bool{
	"-C": true, "--directory": true, "--actor": true, "--db": true,
	"-n": true, "--limit": true, "-l": true, "--labels": true,
	"-p": true, "--priority": true, "-t": true, "--type": true,
	"-s": true, "--status": true, "-a": true, "--assignee": true,
	"-d": true, "--description": true, "--acceptance": true,
	"--reason": true, "--parent": true, "--metadata": true,
	"--set-metadata": true, "--unset-metadata": true, "--add-label": true,
	"--remove-label": true, "--set-labels": true, "--metadata-field": true,
	"--title": true, "--await-id": true, "--blocks": true, "--graph": true,
	"-f": true, "--file": true, "--body-file": true, "--deps": true, "--id": true,
}

// cmdArgs is one parsed bd command line.
type cmdArgs struct {
	words []string            // positionals, in order
	flags map[string][]string // flag name (without "=") to its values
}

// has reports whether the flag was given.
func (a cmdArgs) has(name string) bool { return len(a.flags[name]) > 0 }

// one returns the first value of a flag, or "".
func (a cmdArgs) one(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// word returns the nth positional, or "".
func (a cmdArgs) word(n int) string {
	if n < len(a.words) {
		return a.words[n]
	}
	return ""
}

// parseArgs splits a bd argument list into positionals and flags,
// accepting both "--flag value" and "--flag=value".
func parseArgs(argv []string) cmdArgs {
	out := cmdArgs{flags: map[string][]string{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			name, value, _ := strings.Cut(a, "=")
			out.flags[name] = append(out.flags[name], value)
		case valueFlags[a]:
			value := ""
			if i+1 < len(argv) {
				value = argv[i+1]
				i++
			}
			out.flags[a] = append(out.flags[a], value)
		case strings.HasPrefix(a, "-"):
			out.flags[a] = append(out.flags[a], "")
		default:
			out.words = append(out.words, a)
		}
	}
	return out
}

// fail prints a bd-shaped error and returns its exit code.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
	return 1
}

// reply writes v inside bd's JSON envelope (BD_JSON_ENVELOPE=1, D-21).
func reply(v any) int {
	data, err := json.Marshal(map[string]any{"schema_version": 1, "data": v})
	if err != nil {
		return fail("encoding the reply: %v", err)
	}
	fmt.Println(string(data))
	return 0
}

// load reads the database, creating an empty one on first use.
func load(dir string) (*db, error) {
	db := &db{Prefix: "e2e", Config: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, "db.json"))
	if errors.Is(err, os.ErrNotExist) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, db); err != nil {
		return nil, err
	}
	if db.Config == nil {
		db.Config = map[string]string{}
	}
	return db, nil
}

// save writes the database back.
func save(dir string, db *db) error {
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "db.json"), data, 0o600)
}

// now is the tracker's deterministic clock: one second per call.
func (db *db) now() time.Time {
	db.Seq++
	return epoch.Add(time.Duration(db.Seq) * time.Second)
}

// mint returns the next bead id.
func (db *db) mint() domain.BeadID {
	db.Seq++
	return domain.BeadID(fmt.Sprintf("%s-%d", db.Prefix, db.Seq))
}

// find returns a pointer to the issue with the given id.
func (db *db) find(id domain.BeadID) *beads.Issue {
	for i := range db.Issues {
		if db.Issues[i].ID == id {
			return &db.Issues[i]
		}
	}
	return nil
}

// blockers returns the ids the bead depends on.
func (db *db) blockers(id domain.BeadID) []domain.BeadID {
	var out []domain.BeadID
	for _, d := range db.Deps {
		if d.From == id {
			out = append(out, d.To)
		}
	}
	return out
}

// openBlockers returns the blockers of the bead that are not closed, plus
// the ids of the open gates blocking it (D-20: both keep a bead out of the
// ready set and refuse its close).
func (db *db) openBlockers(id domain.BeadID) []string {
	var out []string
	for _, b := range db.blockers(id) {
		if is := db.find(b); is != nil && is.Status != string(domain.BeadStateClosed) {
			out = append(out, string(b))
		}
	}
	for _, g := range db.Gates {
		if g.BlocksID == id && g.Gate.Status != string(domain.BeadStateClosed) {
			out = append(out, string(g.Gate.ID))
		}
	}
	return out
}

// helper is the fake bd: it interprets one command line against the
// database in dir and returns the exit status.
func helper(dir string, argv []string) int {
	db, err := load(dir)
	if err != nil {
		return fail("reading the fake tracker: %v", err)
	}
	db.Calls++
	a := parseArgs(argv)
	code := dispatch(db, a)
	if err := save(dir, db); err != nil {
		return fail("writing the fake tracker: %v", err)
	}
	return code
}

// dispatch routes one parsed command line.
func dispatch(db *db, a cmdArgs) int {
	switch a.word(0) {
	case "ping":
		return reply(map[string]any{"ok": true, "mode": "embedded"})
	case "statuses":
		return cmdStatuses(db)
	case "config":
		return cmdConfig(db, a)
	case "create":
		return cmdCreate(db, a)
	case "update":
		return cmdUpdate(db, a)
	case "close":
		return cmdClose(db, a)
	case "list":
		return cmdList(db, a)
	case "ready":
		return cmdReady(db, a)
	case "show":
		return cmdShow(db, a)
	case "comment":
		return cmdComment(db, a)
	case "comments":
		return cmdComments(db, a)
	case "dep":
		return cmdDep(db, a)
	case "gate":
		return cmdGate(db, a)
	case "blocked":
		return reply([]beads.Issue{})
	default:
		return fail("unknown command %q (fake bd)", a.word(0))
	}
}

// cmdStatuses answers `bd statuses --json` from the registered custom
// statuses (D-07).
func cmdStatuses(db *db) int {
	list := beads.StatusList{}
	for _, s := range domain.BeadStates() {
		if s.Custom() {
			continue
		}
		list.BuiltIn = append(list.BuiltIn, beads.StatusInfo{Name: string(s), Category: string(s.Category())})
	}
	custom, err := beads.ParseCustomStatuses(db.Config[beads.StatusCustomKey])
	if err != nil {
		return fail("malformed %s: %v", beads.StatusCustomKey, err)
	}
	for _, s := range custom {
		list.Custom = append(list.Custom, beads.StatusInfo{Name: s.Name, Category: string(s.Category)})
	}
	return reply(list)
}

// cmdConfig answers `bd config get|set`.
func cmdConfig(db *db, a cmdArgs) int {
	key := a.word(2)
	switch a.word(1) {
	case "get":
		return reply(beads.ConfigValue{Key: key, Value: db.Config[key]})
	case "set":
		db.Config[key] = a.word(3)
		return reply(beads.ConfigValue{Key: key, Value: db.Config[key]})
	default:
		return fail("unknown config subcommand %q", a.word(1))
	}
}

// readMetadata loads a `--metadata @file` argument.
func readMetadata(arg string) (map[string]any, error) {
	if !strings.HasPrefix(arg, "@") {
		var m map[string]any
		return m, json.Unmarshal([]byte(arg), &m)
	}
	data, err := os.ReadFile(strings.TrimPrefix(arg, "@"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

// cmdCreate answers `bd create`, including the `--graph` form (D-22).
func cmdCreate(db *db, a cmdArgs) int {
	if path := a.one("--graph"); path != "" {
		return cmdCreateGraph(db, a, path)
	}
	priority := domain.PriorityDefault
	if p := a.one("-p"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fail("bad priority %q", p)
		}
		priority = domain.Priority(n)
	}
	is := beads.Issue{
		ID: db.mint(), Title: a.word(1), Status: string(domain.BeadStateOpen),
		Priority: priority, Type: a.one("-t"), Description: a.one("--description"),
		Acceptance: a.one("--acceptance"), Parent: domain.BeadID(a.one("--parent")),
		CreatedAt: db.now(), UpdatedAt: db.now(),
	}
	if is.Type == "" {
		is.Type = string(domain.BeadTypeTask)
	}
	if l := a.one("-l"); l != "" {
		is.Labels = strings.Split(l, ",")
	}
	if m := a.one("--metadata"); m != "" {
		meta, err := readMetadata(m)
		if err != nil {
			return fail("reading metadata: %v", err)
		}
		is.Metadata = meta
	}
	db.Issues = append(db.Issues, is)
	return reply(is)
}

// cmdCreateGraph applies a graph plan atomically, or reports the dry run.
func cmdCreateGraph(db *db, a cmdArgs, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("reading the graph plan: %v", err)
	}
	var plan beads.GraphPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return fail("parsing the graph plan: %v", err)
	}
	if a.has("--dry-run") {
		res := beads.GraphResult{DryRun: true, NodeCount: len(plan.Nodes), EdgeCount: len(plan.Edges), Nodes: plan.Nodes}
		for _, n := range plan.Nodes {
			if n.ParentKey != "" {
				res.ParentDeps++
			}
		}
		return reply(res)
	}
	ids := make(map[string]domain.BeadID, len(plan.Nodes))
	for _, n := range plan.Nodes {
		is := beads.Issue{
			ID: db.mint(), Title: n.Title, Status: string(domain.BeadStateOpen),
			Priority: n.Priority, Type: string(n.Type), Description: n.Description,
			Labels: append([]string(nil), n.Labels...), Metadata: beads.Metadata(n.Metadata),
			CreatedAt: db.now(), UpdatedAt: db.now(),
		}
		if is.Type == "" {
			is.Type = string(domain.BeadTypeTask)
		}
		ids[n.Key] = is.ID
		db.Issues = append(db.Issues, is)
	}
	for _, n := range plan.Nodes {
		if n.ParentKey == "" {
			continue
		}
		if is := db.find(ids[n.Key]); is != nil {
			is.Parent = ids[n.ParentKey]
		}
	}
	for _, e := range plan.Edges {
		db.Deps = append(db.Deps, dep{From: ids[e.FromKey], To: ids[e.ToKey]})
	}
	return reply(beads.GraphResult{IDs: ids})
}

// cmdUpdate answers `bd update`, including the atomic `--claim`.
func cmdUpdate(db *db, a cmdArgs) int {
	id := domain.BeadID(a.word(1))
	is := db.find(id)
	if is == nil {
		return fail("no issue found matching %q", id)
	}
	if a.has("--claim") {
		actor := os.Getenv("BEADS_ACTOR")
		switch {
		case is.Status == string(domain.BeadStateInProgress) && is.Assignee == actor:
			// Idempotent re-claim by the same actor: renews the lease.
		case is.Assignee != "" && is.Assignee != actor:
			return fail("issue %s is already claimed by %s", id, is.Assignee)
		case is.Status != string(domain.BeadStateOpen):
			return fail("issue not claimable: status %s", is.Status)
		}
		is.Assignee = actor
		is.Status = string(domain.BeadStateInProgress)
	}
	if s := a.one("-s"); s != "" {
		is.Status = s
	}
	if v := a.flags["--assignee"]; len(v) > 0 {
		is.Assignee = v[0]
	}
	if p := a.one("-p"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fail("bad priority %q", p)
		}
		is.Priority = domain.Priority(n)
	}
	if v := a.flags["--acceptance"]; len(v) > 0 {
		is.Acceptance = v[0]
	}
	if v := a.flags["--parent"]; len(v) > 0 {
		is.Parent = domain.BeadID(v[0])
	}
	if is.Metadata == nil {
		is.Metadata = beads.Metadata{}
	}
	for _, kv := range a.flags["--set-metadata"] {
		k, v, _ := strings.Cut(kv, "=")
		is.Metadata[k] = v
	}
	if m := a.one("--metadata"); m != "" {
		meta, err := readMetadata(m)
		if err != nil {
			return fail("reading metadata: %v", err)
		}
		for k, v := range meta {
			is.Metadata[k] = v
		}
	}
	for _, k := range a.flags["--unset-metadata"] {
		delete(is.Metadata, k)
	}
	for _, l := range a.flags["--add-label"] {
		if !hasString(is.Labels, l) {
			is.Labels = append(is.Labels, l)
		}
	}
	for _, l := range a.flags["--remove-label"] {
		is.Labels = withoutString(is.Labels, l)
	}
	is.UpdatedAt = db.now()
	return reply([]beads.Issue{*is})
}

// cmdClose answers `bd close`, refusing a bead whose blockers or gates
// are still open (D-20).
func cmdClose(db *db, a cmdArgs) int {
	id := domain.BeadID(a.word(1))
	is := db.find(id)
	if is == nil {
		return fail("no issue found matching %q", id)
	}
	if is.Status == string(domain.BeadStateClosed) {
		return reply([]beads.Issue{*is})
	}
	if open := db.openBlockers(id); len(open) > 0 {
		return fail("cannot close %s: blocked by open dependencies: %s", id, strings.Join(open, ", "))
	}
	is.Status = string(domain.BeadStateClosed)
	is.CloseReason = a.one("--reason")
	is.ClosedAt = db.now()
	is.UpdatedAt = is.ClosedAt
	return reply([]beads.Issue{*is})
}

// cmdList answers `bd list` with the filters the client passes.
func cmdList(db *db, a cmdArgs) int {
	var statuses []string
	if s := a.one("-s"); s != "" {
		statuses = strings.Split(s, ",")
	}
	var labels []string
	if l := a.one("-l"); l != "" {
		labels = strings.Split(l, ",")
	}
	parent := domain.BeadID(a.one("--parent"))
	out := []beads.Issue{}
	for _, is := range db.Issues {
		switch {
		case !a.has("--all") && len(statuses) == 0 && is.Status == string(domain.BeadStateClosed):
			continue
		case len(statuses) > 0 && !hasString(statuses, is.Status):
			continue
		case parent != "" && is.Parent != parent:
			continue
		}
		missing := false
		for _, l := range labels {
			if !hasString(is.Labels, l) {
				missing = true
			}
		}
		if missing {
			continue
		}
		out = append(out, db.withComments(is))
	}
	return reply(out)
}

// cmdReady answers `bd ready`: open beads whose blockers and gates are
// all resolved (D-20). The run epic is listed alongside its children, as
// bd 1.2.2 does.
func cmdReady(db *db, a cmdArgs) int {
	parent := domain.BeadID(a.one("--parent"))
	out := []beads.Issue{}
	for _, is := range db.Issues {
		switch {
		case is.Status != string(domain.BeadStateOpen):
			continue
		case len(db.openBlockers(is.ID)) > 0:
			continue
		case parent != "" && is.Parent != parent && is.ID != parent:
			continue
		}
		out = append(out, is)
	}
	return reply(out)
}

// cmdShow answers `bd show <ids…>`; unknown ids are reported on stderr
// the way bd does, with a zero exit status.
func cmdShow(db *db, a cmdArgs) int {
	out := []beads.Issue{}
	var missing []string
	for _, w := range a.words[1:] {
		is := db.find(domain.BeadID(w))
		if is == nil {
			missing = append(missing, w)
			continue
		}
		out = append(out, db.withComments(*is))
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Error resolving %s: no issue found matching %q\n",
			strings.Join(missing, ", "), missing[0])
	}
	return reply(out)
}

// withComments returns the issue with its comments and dependency edges
// attached, the way `bd show --include-comments` prints them.
func (db *db) withComments(is beads.Issue) beads.Issue {
	out := is
	out.Comments = nil
	for _, c := range db.Comments {
		if c.IssueID == is.ID {
			out.Comments = append(out.Comments, c)
		}
	}
	out.CommentCount = len(out.Comments)
	out.Dependencies = nil
	for _, b := range db.blockers(is.ID) {
		out.Dependencies = append(out.Dependencies, beads.Dependency{IssueID: is.ID, DependsOnID: b, Type: beads.DepBlocks})
	}
	out.DependencyCount = len(out.Dependencies)
	return out
}

// cmdComment answers `bd comment <id> --stdin`.
func cmdComment(db *db, a cmdArgs) int {
	id := domain.BeadID(a.word(1))
	if db.find(id) == nil {
		return fail("no issue found matching %q", id)
	}
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("reading the comment body: %v", err)
	}
	db.Seq++
	c := beads.Comment{
		ID: fmt.Sprintf("comment-%d", db.Seq), IssueID: id,
		Author: os.Getenv("BEADS_ACTOR"), Text: string(body), CreatedAt: db.now(),
	}
	db.Comments = append(db.Comments, c)
	return reply(c)
}

// cmdComments answers `bd comments <id>`.
func cmdComments(db *db, a cmdArgs) int {
	id := domain.BeadID(a.word(1))
	out := []beads.Comment{}
	for _, c := range db.Comments {
		if c.IssueID == id {
			out = append(out, c)
		}
	}
	return reply(out)
}

// cmdDep answers `bd dep add` and `bd dep list`.
func cmdDep(db *db, a cmdArgs) int {
	switch a.word(1) {
	case "add":
		from, to := domain.BeadID(a.word(2)), domain.BeadID(a.word(3))
		db.Deps = append(db.Deps, dep{From: from, To: to})
		return reply(beads.DepResult{IssueID: from, DependsOnID: to, Status: "added", Type: beads.DepBlocks})
	case "list":
		out := []beads.Dependency{}
		for _, w := range a.words[2:] {
			for _, b := range db.blockers(domain.BeadID(w)) {
				out = append(out, beads.Dependency{IssueID: domain.BeadID(w), DependsOnID: b, Type: beads.DepBlocks})
			}
		}
		return reply(out)
	default:
		return fail("unknown dep subcommand %q", a.word(1))
	}
}

// cmdGate answers the `bd gate` family (D-20).
func cmdGate(db *db, a cmdArgs) int {
	switch a.word(1) {
	case "create":
		blocks := domain.BeadID(a.one("--blocks"))
		if db.find(blocks) == nil {
			return fail("no issue found matching %q", blocks)
		}
		db.Seq++
		g := gateRecord{
			Gate: beads.Gate{
				ID:        domain.BeadID(fmt.Sprintf("gate-%d", db.Seq)),
				Title:     "Gate: " + a.one("--type") + " " + a.one("--await-id"),
				Status:    "open",
				Type:      "gate",
				AwaitType: a.one("--type"),
				AwaitID:   a.one("--await-id"),
				CreatedAt: db.now(),
				Description: fmt.Sprintf("Ad-hoc gate blocking %s\n\nReason: %s",
					blocks, a.one("--reason")),
			},
			BlocksID: blocks,
		}
		db.Gates = append(db.Gates, g)
		return reply(g.Gate)
	case "list":
		out := []beads.Gate{}
		for _, g := range db.Gates {
			if !a.has("--all") && g.Gate.Status == string(domain.BeadStateClosed) {
				continue
			}
			out = append(out, g.Gate)
		}
		return reply(out)
	case "show":
		for _, g := range db.Gates {
			if g.Gate.ID == domain.BeadID(a.word(2)) {
				return reply(g.Gate)
			}
		}
		return fail("no gate found matching %q", a.word(2))
	case "resolve":
		for i := range db.Gates {
			if db.Gates[i].Gate.ID != domain.BeadID(a.word(2)) {
				continue
			}
			db.Gates[i].Gate.Status = string(domain.BeadStateClosed)
			db.Gates[i].Gate.CloseReason = a.one("--reason")
			db.Gates[i].Gate.ClosedAt = db.now()
			fmt.Printf("Resolved gate %s\n", db.Gates[i].Gate.ID)
			return 0
		}
		return fail("no gate found matching %q", a.word(2))
	case "check":
		return cmdGateCheck(db, a)
	default:
		return fail("unknown gate subcommand %q", a.word(1))
	}
}

// cmdGateCheck shells out to `gh pr view` for every open gh:pr gate, the
// way bd 1.2.2 does, and closes the gates whose pull request merged.
func cmdGateCheck(db *db, a cmdArgs) int {
	check := beads.GateCheck{DryRun: a.has("--dry-run")}
	var lines []string
	for i := range db.Gates {
		g := &db.Gates[i].Gate
		if g.Status == string(domain.BeadStateClosed) || g.AwaitType != beads.GateTypePR {
			continue
		}
		check.Checked++
		state, err := ghPRState(g.AwaitID)
		switch {
		case err != nil:
			check.Errors++
			lines = append(lines, fmt.Sprintf("✗ %s: error checking - %v", g.ID, err))
		case state == "MERGED":
			check.Resolved++
			if !check.DryRun {
				g.Status = string(domain.BeadStateClosed)
				g.CloseReason = "PR was merged"
				g.ClosedAt = db.now()
			}
			lines = append(lines, fmt.Sprintf("✓ %s: resolved - PR '#%s' was merged", g.ID, g.AwaitID))
		case state == "CLOSED":
			check.Escalated++
			lines = append(lines, fmt.Sprintf("⚠ %s: ESCALATE - PR '#%s' was closed without merging", g.ID, g.AwaitID))
		default:
			lines = append(lines, fmt.Sprintf("· %s: pending - PR '#%s' is still open", g.ID, g.AwaitID))
		}
	}
	if check.Checked == 0 {
		fmt.Println("No open gates of type gh:pr")
		return 0
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return reply(check)
}

// ghPRState asks the `gh` on PATH for a pull request's state, which is
// how bd resolves a gh:pr gate.
func ghPRState(number string) (string, error) {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("gh CLI not installed")
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(bin, "pr", "view", number, "--json", "state")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view failed: %s", strings.TrimSpace(errb.String()))
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &payload); err != nil {
		return "", fmt.Errorf("gh pr view printed something unreadable: %v", err)
	}
	return strings.ToUpper(payload.State), nil
}

// hasString reports whether list contains s.
func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// withoutString returns list without s.
func withoutString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
