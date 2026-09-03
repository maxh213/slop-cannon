package prompt

import (
	"fmt"
	"strconv"
	"strings"

	"slop-cannon/internal/domain"
)

// BeadContext is the bead an agent owns for this firing, plus where its
// code lives. The product owner planning a run has none.
type BeadContext struct {
	// Bead is the bead itself, as the beads client read it.
	Bead domain.Bead
	// Epic is the run's epic bead, the parent of every bead the run creates
	// (D-22); empty when the caller has none to show.
	Epic domain.BeadID
	// Worktree is the absolute path of the agent's git worktree, when it has
	// one of its own (D-23: <run-dir>/wt/<bead-id>).
	Worktree string
	// Branch is the branch the work lives on.
	Branch string
	// Round is the review round this firing belongs to, 1-based; 0 while the
	// bead has not been reviewed yet.
	Round int
	// Notes are extra lines the orchestrator wants the agent to see about
	// this bead, for example "the previous engineer left this mid-rebase".
	Notes []string
}

// Round is one completed pass of the feedback loop, oldest first. The
// builder drops whole rounds from the front when the prompt does not fit.
type Round struct {
	// Number is the round number, 1-based.
	Number int
	// Report is the engineer's impl_report for the round, when there was one.
	Report *domain.ImplReport
	// Verdicts are the testers' verdicts, one per reviewing lens.
	Verdicts []domain.Verdict
	// Responses are the engineer's answers to the items raised.
	Responses []domain.Response
	// Arbitrations are the product owner's rulings on items that stayed
	// disputed (D-08).
	Arbitrations []domain.Arbitration
	// Escalation is the product owner's ruling when the round cap was
	// reached (D-08).
	Escalation *domain.Escalation
	// Diff is a summary of the diff the round reviewed (a diffstat, or the
	// patch itself for a small change); rendered verbatim in a fenced block.
	Diff string
	// Notes are extra lines about the round, for example a failed check.
	Notes []string
}

// renderRepo renders the repository context. treeLimit caps the number of
// tree entries shown (negative means all of RepoContext.Tree) and
// readmeChars caps the README head (negative means all of it, zero omits
// it).
func renderRepo(rc RepoContext, treeLimit, readmeChars int) string {
	if rc.empty() {
		return ""
	}
	var b strings.Builder
	writeBullet(&b, "Repository root", rc.Root)
	writeBullet(&b, "Default branch", rc.DefaultBranch)
	if len(rc.Stack) > 0 {
		writeBullet(&b, "Detected stack", strings.Join(rc.Stack, ", "))
	}
	switch {
	case rc.CheckCommand != "" && rc.CheckCommandDetected:
		writeBullet(&b, "Check command", "`"+rc.CheckCommand+"` (autodetected)")
	case rc.CheckCommand != "":
		writeBullet(&b, "Check command", "`"+rc.CheckCommand+"` (configured)")
	default:
		writeBullet(&b, "Check command", "none detected; use your judgement about how to verify the work")
	}
	if tree := limitTree(rc, treeLimit); len(tree) > 0 {
		shown, total := len(tree), rc.TreeTotal
		if total < shown {
			total = shown
		}
		depth := "unlimited depth"
		if rc.TreeDepth >= 0 {
			depth = "depth " + strconv.Itoa(rc.TreeDepth)
		}
		count := strconv.Itoa(shown) + " entries"
		if shown < total {
			count = strconv.Itoa(shown) + " of " + strconv.Itoa(total) + " entries"
		}
		fmt.Fprintf(&b, "\nTree (%s, %s):\n\n```\n%s\n```\n", depth, count, strings.Join(tree, "\n"))
	}

	if head := limitReadme(rc.ReadmeHead, readmeChars); head != "" {
		title := rc.ReadmeName
		if title == "" {
			title = "README"
		}
		suffix := ""
		if rc.ReadmeTruncated || len(head) < len(rc.ReadmeHead) {
			suffix = " (truncated)"
		}
		fmt.Fprintf(&b, "\n%s%s:\n\n```\n%s\n```\n", title, suffix, head)
	}
	return strings.TrimRight(b.String(), "\n")
}

// limitTree returns the first limit entries of the tree; a negative limit
// means all of them.
func limitTree(rc RepoContext, limit int) []string {
	if limit < 0 || limit >= len(rc.Tree) {
		return rc.Tree
	}
	if limit == 0 {
		return nil
	}
	return rc.Tree[:limit]
}

// limitReadme cuts the README head to limit characters at a line boundary; a
// negative limit means all of it and zero omits it.
func limitReadme(head string, limit int) string {
	switch {
	case head == "" || limit == 0:
		return ""
	case limit < 0 || limit >= len(head):
		return head
	}
	return strings.TrimRight(cutAtLine(head[:limit]), "\n")
}

// renderOpenBeads renders the beads that already exist, so the product owner
// does not propose them again (P5-02). A negative limit means all of them.
func renderOpenBeads(beads []domain.Bead, limit int) string {
	if len(beads) == 0 || limit == 0 {
		return ""
	}
	shown := beads
	if limit > 0 && limit < len(shown) {
		shown = shown[:limit]
	}
	var b strings.Builder
	b.WriteString("Do not propose work that duplicates any of these:\n")
	for _, bead := range shown {
		fmt.Fprintf(&b, "\n- `%s` [%s] %s", bead.ID, bead.State, oneLine(bead.Title))
	}
	if len(shown) < len(beads) {
		fmt.Fprintf(&b, "\n- … and %d more not listed here", len(beads)-len(shown))
	}
	return b.String()
}

// renderBead renders the bead an agent owns.
func renderBead(bc *BeadContext) string {
	if bc == nil {
		return ""
	}
	bead := bc.Bead
	var b strings.Builder
	writeBullet(&b, "Bead", string(bead.ID)+" — "+oneLine(bead.Title))
	if bead.Type != "" {
		writeBullet(&b, "Type", bead.Type.String())
	}
	writeBullet(&b, "Priority", strconv.Itoa(int(bead.Priority))+" (0 highest, 4 lowest)")
	if bead.Size != "" {
		writeBullet(&b, "Size", strings.ToUpper(bead.Size.String()))
	}
	if bead.State != "" {
		writeBullet(&b, "State", bead.State.String())
	}
	if len(bead.Deps) > 0 {
		deps := make([]string, len(bead.Deps))
		for i, d := range bead.Deps {
			deps[i] = string(d)
		}
		writeBullet(&b, "Depends on", strings.Join(deps, ", "))
	}
	if bc.Epic != "" {
		writeBullet(&b, "Run epic", string(bc.Epic))
	}
	if bc.Worktree != "" {
		writeBullet(&b, "Worktree", bc.Worktree)
	}
	if bc.Branch != "" {
		writeBullet(&b, "Branch", bc.Branch)
	}
	if bc.Round > 0 {
		writeBullet(&b, "Review round", strconv.Itoa(bc.Round))
	}
	for _, n := range bc.Notes {
		if s := strings.TrimSpace(n); s != "" {
			b.WriteString("- " + s + "\n")
		}
	}
	if d := strings.TrimSpace(bead.Description); d != "" {
		b.WriteString("\nDescription:\n\n" + d + "\n")
	}
	if a := strings.TrimSpace(bead.Acceptance); a != "" {
		b.WriteString("\nAcceptance criteria (every one of these must hold):\n\n" + a + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderHistory renders the feedback loop so far. keep is how many of the
// most recent rounds to show; a negative value means all of them.
func renderHistory(rounds []Round, keep int) string {
	if len(rounds) == 0 || keep == 0 {
		return ""
	}
	shown := rounds
	if keep > 0 && keep < len(shown) {
		shown = shown[len(shown)-keep:]
	}
	var b strings.Builder
	if dropped := len(rounds) - len(shown); dropped > 0 {
		fmt.Fprintf(&b, "%d earlier round(s) omitted to fit the prompt budget; ask if you need them.\n\n", dropped)
	}
	for i, r := range shown {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "### Round %d\n", r.Number)
		if r.Report != nil {
			b.WriteString("\n" + renderReport(*r.Report))
		}
		if d := strings.TrimSpace(r.Diff); d != "" {
			fmt.Fprintf(&b, "\nDiff reviewed:\n\n```diff\n%s\n```\n", d)
		}
		for _, v := range r.Verdicts {
			b.WriteString("\n" + renderVerdict(v))
		}
		if len(r.Responses) > 0 {
			b.WriteString("\nEngineer responses:\n")
			for _, resp := range r.Responses {
				fmt.Fprintf(&b, "\n- `%s` — %s%s", resp.ItemID, resp.Action, suffix(": ", oneLine(resp.Rationale)))
			}
			b.WriteString("\n")
		}
		for _, a := range r.Arbitrations {
			fmt.Fprintf(&b, "\nProduct owner ruling on `%s`: %s%s%s\n", a.ItemID, a.Ruling,
				suffix(" — ", oneLine(a.Instruction)), suffix(" (", closeParen(oneLine(a.Rationale))))
		}
		if e := r.Escalation; e != nil {
			fmt.Fprintf(&b, "\nProduct owner escalation ruling: %s%s%s%s\n", e.Outcome,
				suffix(" — ", oneLine(e.Notes)), suffix(" New acceptance: ", oneLine(e.NewAcceptance)),
				suffix(" For the human: ", oneLine(e.Summary)))
		}
		for _, n := range r.Notes {
			if s := strings.TrimSpace(n); s != "" {
				b.WriteString("\nNote: " + s + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderReport renders one engineer impl_report.
func renderReport(rep domain.ImplReport) string {
	var b strings.Builder
	b.WriteString("Engineer report:\n")
	if s := strings.TrimSpace(rep.Summary); s != "" {
		b.WriteString("\n" + s + "\n")
	}
	if len(rep.FilesChanged) > 0 {
		fmt.Fprintf(&b, "\n- Files changed: %s\n", strings.Join(rep.FilesChanged, ", "))
	}
	if rep.Commit != "" {
		fmt.Fprintf(&b, "- Commit: %s\n", rep.Commit)
	}
	if s := strings.TrimSpace(rep.HowToVerify); s != "" {
		fmt.Fprintf(&b, "- How to verify: %s\n", oneLine(s))
	}
	for _, r := range rep.Risks {
		fmt.Fprintf(&b, "- Risk: %s\n", oneLine(r))
	}
	for _, q := range rep.Questions {
		fmt.Fprintf(&b, "- Question: %s\n", oneLine(q))
	}
	return b.String()
}

// renderVerdict renders one tester verdict with its items.
func renderVerdict(v domain.Verdict) string {
	var b strings.Builder
	lens := "tester"
	if v.Lens != "" {
		lens = v.Lens.String() + " tester"
	}
	fmt.Fprintf(&b, "Verdict (%s): %s%s\n", lens, v.Outcome, suffix(" — ", oneLine(v.Summary)))
	for _, it := range v.Items {
		fmt.Fprintf(&b, "\n- `%s` [%s", it.ID, it.Severity)
		if it.Category != "" {
			b.WriteString("/" + it.Category)
		}
		b.WriteString("] " + oneLine(it.Title))
		if it.File != "" {
			b.WriteString(" (" + it.File + location(it.Line) + ")")
		}
		if it.Status != "" {
			b.WriteString(" — status: " + it.Status.String())
		}
		if d := oneLine(it.Detail); d != "" {
			b.WriteString("\n  " + d)
		}
		if s := oneLine(it.Suggestion); s != "" {
			b.WriteString("\n  Suggestion: " + s)
		}
	}
	if len(v.Items) > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// writeBullet writes "- Label: value" when value is not empty.
func writeBullet(b *strings.Builder, label, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fmt.Fprintf(b, "- %s: %s\n", label, value)
}

// oneLine collapses a value onto a single line so list items stay readable.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

// suffix returns prefix+value when value is not empty.
func suffix(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

// closeParen closes the parenthesis suffix opens, so an empty rationale
// leaves no stray bracket behind.
func closeParen(s string) string {
	if s == "" {
		return ""
	}
	return s + ")"
}

// location renders ":<line>" for a positive line number.
func location(line int) string {
	if line <= 0 {
		return ""
	}
	return ":" + strconv.Itoa(line)
}
