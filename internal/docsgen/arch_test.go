package docsgen

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
)

// archRepoFile is docs/architecture.md relative to this package.
var archRepoFile = filepath.Join("..", "..", filepath.FromSlash(ArchDocPath))

func readArch(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(archRepoFile)
	require.NoError(t, err, "missing %s", ArchDocPath)
	return string(b)
}

// TestArchitectureTablesAreCurrent is P10-07: the D-05 allocation and the
// D-07 transition tables in docs/architecture.md are rendered from the code,
// so moving an edge or changing the allocation formula without updating the
// document fails here. UPDATE_GOLDEN=1 (make update-golden) rewrites the
// file, exactly like docs/cli.md.
func TestArchitectureTablesAreCurrent(t *testing.T) {
	doc := readArch(t)
	want, err := Render(doc)
	require.NoError(t, err)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if want != doc {
			require.NoError(t, os.WriteFile(archRepoFile, []byte(want), 0o644))
		}
		return
	}
	require.Equal(t, want, doc,
		"%s is stale; run `make update-golden` (or UPDATE_GOLDEN=1 go test ./internal/docsgen/)", ArchDocPath)
}

// TestRenderIsIdempotent: rendering the rendered document changes nothing,
// which is what makes the check above a fixed point rather than a diff that
// never settles.
func TestRenderIsIdempotent(t *testing.T) {
	once, err := Render(readArch(t))
	require.NoError(t, err)
	twice, err := Render(once)
	require.NoError(t, err)
	require.Equal(t, once, twice)
}

// TestChangingTheCodeChangesTheDocument is the acceptance criterion stated
// as a test: a document holding a table the code no longer agrees with is
// rejected. It edits a copy in memory, never the file.
func TestChangingTheCodeChangesTheDocument(t *testing.T) {
	doc, err := Render(readArch(t))
	require.NoError(t, err)

	for _, tc := range []struct{ name, old, new string }{
		// An edge that moved: delivering's conflict edge now points
		// somewhere else.
		{"an edge", "| `delivering` | `conflict` | `in_progress` |", "| `delivering` | `conflict` | `in_review` |"},
		// An edge that was dropped.
		{"a missing edge", "| `escalated` | `scope_narrowed` | `in_review` |\n", ""},
		// An allocation row that no longer matches config.Allocate.
		{"an allocation row", "| 10 | 1 | 2 | 7 | 3 |", "| 10 | 1 | 3 | 6 | 3 |"},
		// A demand that lost its place in the priority order.
		{"a priority row", "| 1 | `arbitrate` | po | `arbitrate` |", "| 1 | `review` | tester | `review` |"},
		// A status whose bd category was mistyped.
		{"a status row", "| `escalated` | `frozen` |", "| `escalated` | `wip` |"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, doc, tc.old, "the document no longer contains the line this case edits")
			stale := strings.Replace(doc, tc.old, tc.new, 1)
			require.NotEqual(t, doc, stale)
			got, err := Render(stale)
			require.NoError(t, err)
			require.Equal(t, doc, got, "Render must put back what the code says")
			require.NotEqual(t, stale, got, "a stale table must not survive Render")
		})
	}
}

// TestEveryBlockIsInTheDocument guards the markers themselves: a block that
// is generated but never placed would silently document nothing.
func TestEveryBlockIsInTheDocument(t *testing.T) {
	doc := readArch(t)
	require.Equal(t, Blocks(), markerNames(doc), "the document's generated blocks, in order")
	for _, name := range Blocks() {
		require.Contains(t, doc, beginMarker(name))
		require.Contains(t, doc, endMarker(name))
		body, err := Body(name)
		require.NoError(t, err)
		require.NotEmpty(t, body)
		require.True(t, strings.HasPrefix(body, "| "), "%s renders a markdown table", name)
	}
	_, err := Body("nope")
	require.Error(t, err)
}

// TestRenderRejectsABrokenDocument: the failure modes of the markers are
// errors, not silently skipped blocks.
func TestRenderRejectsABrokenDocument(t *testing.T) {
	doc := readArch(t)
	for _, tc := range []struct{ name, doc, want string }{
		{"missing", strings.Replace(doc, beginMarker(BlockAllocation), "", 1), "missing"},
		{"unterminated", strings.Replace(doc, endMarker(BlockAllocation), "", 1), "never closes"},
		{"duplicated", doc + "\n" + beginMarker(BlockStates) + "\n" + endMarker(BlockStates) + "\n", "more than one"},
		{"unknown", doc + "\n<!-- BEGIN GENERATED: d99-nope (internal/docsgen) -->\n", "unknown generated block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.doc)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestTheDocumentNamesEveryStateAndDemand keeps the hand-drawn state diagram
// and the prose honest about the vocabulary: a state or a demand kind added
// to the code has to be drawn as well as tabulated.
func TestTheDocumentNamesEveryStateAndDemand(t *testing.T) {
	doc := readArch(t)
	prose := outsideBlocks(doc)
	for _, s := range domain.WorkflowBeadStates() {
		require.Contains(t, prose, s.String(), "the diagram and prose must name the %s state", s)
	}
	for _, k := range orchestrator.DemandKinds() {
		require.Contains(t, prose, k.String(), "the prose must name the %s demand", k)
	}
	// The events of the flow proper; the two side exits are described as a
	// rule rather than drawn edge by edge.
	for _, ev := range orchestrator.BeadEvents() {
		require.Contains(t, doc, ev.String(), "the machine's %s event must appear", ev)
	}
}

// TestTheAllocationRowsCoverTheFormula: the sampled ammo values include the
// D-05 minimum, the D-04 default, and both sides of every step in
// max_engineers_in_flight up to the largest sample, so a change to the
// formula cannot hide between two rows.
func TestTheAllocationRowsCoverTheFormula(t *testing.T) {
	require.Contains(t, AllocationAmmo, config.MinAmmo)
	require.Contains(t, AllocationAmmo, config.DefaultAmmo)
	require.IsIncreasing(t, AllocationAmmo)

	sampled := make(map[int]bool, len(AllocationAmmo))
	for _, a := range AllocationAmmo {
		sampled[a] = true
	}
	last, err := config.Allocate(config.MinAmmo)
	require.NoError(t, err)
	for ammo := config.MinAmmo + 1; ammo <= AllocationAmmo[len(AllocationAmmo)-1]; ammo++ {
		a, err := config.Allocate(ammo)
		require.NoError(t, err)
		if a.MaxEngineers != last.MaxEngineers {
			require.True(t, sampled[ammo] && sampled[ammo-1],
				"ammo %d steps to %d engineers; the table must sample %d and %d",
				ammo, a.MaxEngineers, ammo-1, ammo)
		}
		last = a
	}
}

// TestTheTablesMatchTheCodeRowByRow reads the rendered tables back and
// compares them with the code, so a bug in the renderer itself (a dropped
// row, a column in the wrong place) is caught rather than baked into the
// document.
func TestTheTablesMatchTheCodeRowByRow(t *testing.T) {
	trs := orchestrator.BeadTransitions()
	rows := dataRows(t, BlockTransitions)
	require.Len(t, rows, len(trs))
	for i, tr := range trs {
		require.Equal(t, []string{"`" + string(tr.From) + "`", "`" + tr.Event.String() + "`", "`" + string(tr.To) + "`"}, rows[i])
	}

	rows = dataRows(t, BlockAllocation)
	require.Len(t, rows, len(AllocationAmmo))
	for i, ammo := range AllocationAmmo {
		a, err := config.Allocate(ammo)
		require.NoError(t, err)
		caps := orchestrator.CapsFor(a)
		require.Equal(t, []string{
			strconv.Itoa(a.Ammo), strconv.Itoa(a.PO), strconv.Itoa(a.MaxEngineers), strconv.Itoa(a.MaxTesters), strconv.Itoa(caps.TestersPerBead()),
		}, rows[i])
	}

	rows = dataRows(t, BlockStates)
	require.Len(t, rows, len(domain.WorkflowBeadStates()))
	for i, s := range domain.WorkflowBeadStates() {
		require.Equal(t, "`"+s.String()+"`", rows[i][0])
		require.Equal(t, "`"+string(s.Category())+"`", rows[i][1])
	}

	rows = dataRows(t, BlockPriority)
	require.Len(t, rows, len(orchestrator.DemandKinds()))
	for i, k := range orchestrator.DemandKinds() {
		require.Equal(t, []string{strconv.Itoa(k.Priority() + 1), "`" + k.String() + "`", string(k.Role()), "`" + string(k.Task()) + "`"}, rows[i])
	}
}

// dataRows parses a generated block back into its cells, without the header
// and the alignment rule.
func dataRows(t *testing.T, name string) [][]string {
	t.Helper()
	body, err := Body(name)
	require.NoError(t, err)
	lines := strings.Split(body, "\n")
	require.Greater(t, len(lines), 2, "%s has a header, a rule and at least one row", name)
	var out [][]string
	for _, l := range lines[2:] {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "|")
		l = strings.TrimSuffix(l, "|")
		cells := strings.Split(l, "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		out = append(out, cells)
	}
	return out
}

// outsideBlocks returns the document with every generated block removed, so
// a test can ask what the hand-written parts say.
func outsideBlocks(doc string) string {
	for _, name := range Blocks() {
		begin, end := beginMarker(name), endMarker(name)
		bi := strings.Index(doc, begin)
		if bi < 0 {
			continue
		}
		ei := strings.Index(doc[bi:], end)
		if ei < 0 {
			continue
		}
		doc = doc[:bi] + doc[bi+ei+len(end):]
	}
	return doc
}
