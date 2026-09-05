// Package docsgen renders the parts of docs/architecture.md that are
// derived from code rather than written by hand (P10-07).
//
// The D-05 allocation and the D-07 state machine live in Go — in
// [config.Allocate], [orchestrator.CapsFor] and
// [orchestrator.BeadTransitions] — and a table copied out of them by hand
// goes stale the first time an edge moves. So the document carries marked
// blocks:
//
//	<!-- BEGIN GENERATED: <name> (internal/docsgen) -->
//	…
//	<!-- END GENERATED: <name> -->
//
// [Render] rewrites the body of every block from the code, leaving the rest
// of the document alone, and TestArchitectureTablesAreCurrent fails when the
// file on disk is not what Render would produce. UPDATE_GOLDEN=1 (i.e.
// `make update-golden`) rewrites it in place, like docs/cli.md.
package docsgen

import (
	"fmt"
	"slices"
	"strings"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
)

// ArchDocPath is the document these blocks live in, relative to the
// repository root.
const ArchDocPath = "docs/architecture.md"

// Block names, in the order they appear in the document.
const (
	// BlockAllocation is the D-05 ammo allocation table.
	BlockAllocation = "d05-allocation"
	// BlockPriority is the D-05 scheduler priority order.
	BlockPriority = "d05-priority"
	// BlockStates is the D-07 status table.
	BlockStates = "d07-states"
	// BlockTransitions is the D-07 transition table.
	BlockTransitions = "d07-transitions"
)

// AllocationAmmo is the ammo the allocation table is rendered for: the
// minimum (D-05), the default (D-04), and the values on either side of each
// step in max_engineers_in_flight, so a change to the formula moves a row.
var AllocationAmmo = []int{3, 4, 5, 8, 9, 10, 12, 13, 16, 17}

// Blocks returns every block name, in document order.
func Blocks() []string {
	return []string{BlockAllocation, BlockPriority, BlockStates, BlockTransitions}
}

// beginMarker and endMarker are the HTML comments that delimit a block.
func beginMarker(name string) string {
	return "<!-- BEGIN GENERATED: " + name + " (internal/docsgen) -->"
}

func endMarker(name string) string { return "<!-- END GENERATED: " + name + " -->" }

// Body returns the generated markdown of one block, without its markers and
// without a trailing newline. An unknown name is an error.
func Body(name string) (string, error) {
	switch name {
	case BlockAllocation:
		return allocationTable()
	case BlockPriority:
		return priorityTable(), nil
	case BlockStates:
		return stateTable(), nil
	case BlockTransitions:
		return transitionTable(), nil
	}
	return "", fmt.Errorf("docsgen: unknown block %q (want one of %s)", name, strings.Join(Blocks(), ", "))
}

// Render returns doc with the body of every marked block replaced by what
// the code says it is. Everything outside the markers is untouched. It is an
// error for the document to miss a block, to repeat one, to leave one
// unterminated, or to name one that does not exist.
func Render(doc string) (string, error) {
	out := doc
	for _, name := range Blocks() {
		var err error
		if out, err = renderBlock(out, name); err != nil {
			return "", err
		}
	}
	// A marker naming a block we do not generate is a typo in the
	// document, not something to pass over in silence.
	for _, name := range markerNames(out) {
		if !slices.Contains(Blocks(), name) {
			return "", fmt.Errorf("docsgen: %s has an unknown generated block %q (want one of %s)",
				ArchDocPath, name, strings.Join(Blocks(), ", "))
		}
	}
	return out, nil
}

// markerNames returns the block name of every BEGIN marker in doc, in the
// order they appear.
func markerNames(doc string) []string {
	const open = "<!-- BEGIN GENERATED: "
	var out []string
	for rest := doc; ; {
		i := strings.Index(rest, open)
		if i < 0 {
			return out
		}
		rest = rest[i+len(open):]
		name, _, ok := strings.Cut(rest, " ")
		if !ok {
			return out
		}
		out = append(out, name)
	}
}

// renderBlock replaces one block's body, leaving its markers in place.
func renderBlock(doc, name string) (string, error) {
	begin, end := beginMarker(name), endMarker(name)
	bi := strings.Index(doc, begin)
	if bi < 0 {
		return "", fmt.Errorf("docsgen: %s is missing the %q block (%s)", ArchDocPath, name, begin)
	}
	if strings.Contains(doc[bi+len(begin):], begin) {
		return "", fmt.Errorf("docsgen: %s has more than one %q block", ArchDocPath, name)
	}
	ei := strings.Index(doc[bi:], end)
	if ei < 0 {
		return "", fmt.Errorf("docsgen: %s never closes the %q block (%s)", ArchDocPath, name, end)
	}
	ei += bi
	body, err := Body(name)
	if err != nil {
		return "", err
	}
	return doc[:bi] + begin + "\n\n" + body + "\n\n" + doc[ei:], nil
}

// table renders a markdown table. align is one character per column: 'l' or
// 'r'.
func table(align string, header []string, rows [][]string) string {
	var b strings.Builder
	row := func(cells []string) {
		b.WriteString("| ")
		b.WriteString(strings.Join(cells, " | "))
		b.WriteString(" |\n")
	}
	row(header)
	rule := make([]string, len(header))
	for i := range header {
		if i < len(align) && align[i] == 'r' {
			rule[i] = "---:"
			continue
		}
		rule[i] = "---"
	}
	row(rule)
	for _, r := range rows {
		row(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// allocationTable renders [config.Allocate] and the review width
// [orchestrator.SlotCaps.TestersPerBead] derives from it.
func allocationTable() (string, error) {
	rows := make([][]string, 0, len(AllocationAmmo))
	for _, ammo := range AllocationAmmo {
		a, err := config.Allocate(ammo)
		if err != nil {
			return "", fmt.Errorf("docsgen: allocation for ammo %d: %w", ammo, err)
		}
		caps := orchestrator.CapsFor(a)
		rows = append(rows, []string{
			fmt.Sprint(a.Ammo), fmt.Sprint(a.PO), fmt.Sprint(a.MaxEngineers),
			fmt.Sprint(a.MaxTesters), fmt.Sprint(caps.TestersPerBead()),
		})
	}
	return table("rrrrr",
		[]string{"ammo", "PO", "engineers in flight", "testers", "testers per bead"},
		rows), nil
}

// priorityTable renders the D-05 order the scheduler fills free slots in.
func priorityTable() string {
	kinds := orchestrator.DemandKinds()
	rows := make([][]string, 0, len(kinds))
	for _, k := range kinds {
		rows = append(rows, []string{
			fmt.Sprint(k.Priority() + 1), "`" + k.String() + "`",
			string(k.Role()), "`" + string(k.Task()) + "`",
		})
	}
	return table("rlll", []string{"order", "demand", "role", "event task"}, rows)
}

// stateTable renders the D-07 statuses with the bd categories they are
// registered under.
func stateTable() string {
	states := domain.WorkflowBeadStates()
	rows := make([][]string, 0, len(states))
	for _, s := range states {
		origin := "bd built-in"
		if s.Custom() {
			origin = "registered by `sc`"
		}
		rows = append(rows, []string{
			"`" + s.String() + "`", "`" + string(s.Category()) + "`", origin, yesNo(s.Terminal()),
		})
	}
	return table("llll", []string{"status", "bd category", "origin", "terminal"}, rows)
}

// transitionTable renders every edge of the D-07 machine.
func transitionTable() string {
	trs := orchestrator.BeadTransitions()
	rows := make([][]string, 0, len(trs))
	for _, t := range trs {
		rows = append(rows, []string{
			"`" + string(t.From) + "`", "`" + t.Event.String() + "`", "`" + string(t.To) + "`",
		})
	}
	return table("lll", []string{"from", "event", "to"}, rows)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
