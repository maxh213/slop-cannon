package beads

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// The gate-* samples in testdata/bd/ were captured on 2026-09-02 from bd
// 1.2.2 in a scratch repository (prefix fx) with a `gh` shim answering
// `gh pr view <n> --json state,title` with the state named by the sample:
//
//	gate-create            gate create --type=gh:pr --blocks fx-byi --await-id 1 --reason "PR #1" --json
//	gate-create-noawait    the same without --await-id (bd accepts it; the gate then never resolves)
//	gate-create-not-found  --blocks nope-1: exit 1, data.error "issue not found: nope-1"
//	gate-create-readonly   under --readonly: exit 1, stderr "operation 'gate create' is not allowed in read-only mode"
//	gate-list              gate list --json with one open gate
//	gate-list-empty        gate list --json after the gate resolved (data null)
//	gate-list-all          gate list --all --json: the resolved gate with close_reason
//	gate-list-two          gate list --all --json: one open, one closed
//	gate-show              gate show fx-qba --json (an object, not an array)
//	gate-show-not-found    gate show nope-1 --json: exit 1, data.error "gate not found: nope-1"
//	gate-check[-open]      gate check --type=gh:pr --json, PR OPEN
//	gate-check-merged      PR MERGED: bd closed the gate
//	gate-check-closed      PR CLOSED: ESCALATE line, gate left open
//	gate-check-closed-escalate  the same with --escalate and no `gt` on PATH: stderr Warning
//	gate-check-missing-gh  no `gh` on PATH: stderr "error checking - gh CLI not installed"
//	gate-check-unauth      `gh` not logged in: two-line stderr error
//	gate-check-bad-gh      `gh` printed garbage: "failed to parse gh output"
//	gate-check-pr-missing  `gh` cannot find the PR: ESCALATE "pull request not found"
//	gate-check-none        no open gh:pr gate: a text line, no JSON
//	gate-check-dry-run     --dry-run: dry_run true
//	gate-check-two         gate check --json over two gates (one without --await-id)
//	gate-resolve           gate resolve fx-96y --reason done --json: text only

// gatesFake is clientFake with the gate samples staged under the keys the
// shim derives ("gate-check" for `bd gate check`, and so on).
func gatesFake(t *testing.T, overrides map[string]string) (*Client, *testutil.Shim) {
	t.Helper()
	return clientFake(t, overrides)
}

func TestGateUnmarshal(t *testing.T) {
	var g Gate
	require.NoError(t, Decode(envSample(t, "gate-create.json"), &g))
	assert.Equal(t, domain.BeadID("fx-qba"), g.ID)
	assert.Equal(t, "Gate: gh:pr 1", g.Title)
	assert.Equal(t, "open", g.Status)
	assert.Equal(t, "gate", g.Type)
	assert.Equal(t, domain.Priority(2), g.Priority)
	assert.Equal(t, GateTypePR, g.AwaitType)
	assert.Equal(t, "1", g.AwaitID)
	assert.Equal(t, domain.BeadID("fx-byi"), g.Blocks, "blocked issue recovered from the description")
	assert.Equal(t, "sc:test", g.CreatedBy)
	assert.False(t, g.CreatedAt.IsZero())
	assert.True(t, g.IsOpen())
	assert.True(t, g.IsPR())
	n, ok := g.PRNumber()
	assert.True(t, ok)
	assert.Equal(t, 1, n)

	t.Run("closed", func(t *testing.T) {
		var gates []Gate
		require.NoError(t, Decode(envSample(t, "gate-list-all.json"), &gates))
		require.Len(t, gates, 1)
		assert.False(t, gates[0].IsOpen())
		assert.Equal(t, "PR 'Fix Max' was merged", gates[0].CloseReason)
		assert.False(t, gates[0].ClosedAt.IsZero())
	})
	t.Run("no await id", func(t *testing.T) {
		var g Gate
		require.NoError(t, Decode(envSample(t, "gate-create-noawait.json"), &g))
		assert.True(t, g.IsPR())
		_, ok := g.PRNumber()
		assert.False(t, ok)
		assert.Equal(t, domain.BeadID("fx-8b8"), g.Blocks)
	})
	t.Run("other types", func(t *testing.T) {
		var g Gate
		require.NoError(t, json.Unmarshal([]byte(`{"id":"fx-1","title":"Gate: human","status":"open","await_type":"human","description":"nothing"}`), &g))
		assert.False(t, g.IsPR())
		_, ok := g.PRNumber()
		assert.False(t, ok)
		assert.Empty(t, g.Blocks)
		assert.Equal(t, domain.PriorityDefault, g.Priority, "missing priority defaults")
	})
	t.Run("null list", func(t *testing.T) {
		var gates []Gate
		require.NoError(t, Decode(envSample(t, "gate-list-empty.json"), &gates))
		assert.Empty(t, gates)
	})
}

func TestCreatePRGate(t *testing.T) {
	c, bd := gatesFake(t, nil)
	ctx := context.Background()

	g, err := c.CreatePRGate(ctx, "fx-byi", 1, "PR #1")
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-qba"), g.ID)
	assert.Equal(t, domain.BeadID("fx-byi"), g.Blocks)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "gate", "create", "--type=gh:pr", "--blocks", "fx-byi", "--await-id", "1", "--reason", "PR #1", "--json"}, clientLastArgs(t, bd))

	_, err = c.CreatePRGate(ctx, "fx-byi", 7, "")
	require.NoError(t, err)
	assert.Contains(t, clientLastArgs(t, bd), "PR #7", "empty reason defaults to the PR number")

	_, err = c.CreatePRGate(ctx, "fx-byi", 0, "x")
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = c.CreatePRGate(ctx, "", 1, "x")
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, bd.ArgLists(t), 2, "invalid input never reaches bd")
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestCreatePRGateErrors(t *testing.T) {
	t.Run("unknown bead", func(t *testing.T) {
		c, _ := gatesFake(t, map[string]string{"gate-create": "gate-create-not-found"})
		_, err := c.CreatePRGate(context.Background(), "nope-1", 9, "")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotFound)
		var e *Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "gate create", e.Command)
		assert.Equal(t, "issue not found: nope-1", e.Message)
	})
	t.Run("read-only", func(t *testing.T) {
		c, _ := gatesFake(t, map[string]string{"gate-create": "gate-create-readonly"})
		_, err := c.CreatePRGate(context.Background(), "fx-8b8", 9, "")
		assert.ErrorIs(t, err, ErrReadOnly)
	})
}

func TestGatesAndGateFor(t *testing.T) {
	ctx := context.Background()
	t.Run("open gates", func(t *testing.T) {
		c, bd := gatesFake(t, nil)
		gates, err := c.Gates(ctx, false)
		require.NoError(t, err)
		require.Len(t, gates, 1)
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "gate", "list", "-n", "0", "--json"}, clientLastArgs(t, bd))

		g, err := c.GateFor(ctx, "fx-byi")
		require.NoError(t, err)
		assert.Equal(t, domain.BeadID("fx-qba"), g.ID)

		_, err = c.GateFor(ctx, "fx-other")
		assert.ErrorIs(t, err, ErrNoGate)
		_, err = c.GateFor(ctx, "")
		assert.ErrorIs(t, err, ErrInvalid)
	})
	t.Run("all includes closed", func(t *testing.T) {
		c, bd := gatesFake(t, map[string]string{"gate-list": "gate-list-two"})
		gates, err := c.Gates(ctx, true)
		require.NoError(t, err)
		require.Len(t, gates, 2)
		assert.Contains(t, clientLastArgs(t, bd), "--all")
		assert.True(t, gates[0].IsOpen())
		assert.False(t, gates[1].IsOpen())

		// GateFor ignores the closed gate even when the listing carries it.
		_, err = c.GateFor(ctx, "fx-byi")
		assert.ErrorIs(t, err, ErrNoGate)
		g, err := c.GateFor(ctx, "fx-6z4")
		require.NoError(t, err)
		assert.Equal(t, domain.BeadID("fx-8za"), g.ID)
	})
	t.Run("none", func(t *testing.T) {
		c, _ := gatesFake(t, map[string]string{"gate-list": "gate-list-empty"})
		gates, err := c.Gates(ctx, false)
		require.NoError(t, err)
		assert.Empty(t, gates)
		_, err = c.GateFor(ctx, "fx-byi")
		assert.ErrorIs(t, err, ErrNoGate)
	})
	t.Run("prefers the newest pr gate", func(t *testing.T) {
		c := New("/repo", Options{Actor: "sc:test"})
		c.exec = func(context.Context, proc.Spec) (proc.Result, error) {
			return proc.Result{Stdout: `{"data":[
				{"id":"g-human","status":"open","await_type":"human","description":"Ad-hoc gate blocking fx-1","created_at":"2026-09-02T12:00:00Z"},
				{"id":"g-old","status":"open","await_type":"gh:pr","await_id":"1","description":"Ad-hoc gate blocking fx-1","created_at":"2026-09-02T10:00:00Z"},
				{"id":"g-new","status":"open","await_type":"gh:pr","await_id":"2","description":"Ad-hoc gate blocking fx-1","created_at":"2026-09-02T11:00:00Z"}
			],"schema_version":1}`}, nil
		}
		g, err := c.GateFor(ctx, "fx-1")
		require.NoError(t, err)
		assert.Equal(t, domain.BeadID("g-new"), g.ID)
	})
}

func TestShowGateAndResolveGate(t *testing.T) {
	ctx := context.Background()
	c, bd := gatesFake(t, nil)
	g, err := c.ShowGate(ctx, "fx-qba")
	require.NoError(t, err)
	assert.Equal(t, domain.BeadID("fx-qba"), g.ID)
	assert.Equal(t, domain.BeadID("fx-byi"), g.Blocks)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "gate", "show", "fx-qba", "--json"}, clientLastArgs(t, bd))

	require.NoError(t, c.ResolveGate(ctx, "fx-96y", "done"))
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "gate", "resolve", "fx-96y", "--reason", "done", "--json"}, clientLastArgs(t, bd))
	require.NoError(t, c.ResolveGate(ctx, "fx-96y", ""))
	assert.NotContains(t, clientLastArgs(t, bd), "--reason")

	assert.ErrorIs(t, c.ResolveGate(ctx, "", ""), ErrInvalid)
	_, err = c.ShowGate(ctx, "")
	assert.ErrorIs(t, err, ErrInvalid)

	c2, _ := gatesFake(t, map[string]string{"gate-show": "gate-show-not-found"})
	_, err = c2.ShowGate(ctx, "nope-1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestCheckGatesReplaysCapturedOutput(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		sample    string
		opts      GateCheckOptions
		want      GateCheck
		results   []GateResult
		ghMissing string
	}{
		{
			name: "still open", sample: "gate-check-open",
			want:    GateCheck{Checked: 1},
			results: []GateResult{{GateID: "fx-qba", Outcome: GatePending, Detail: "PR 'Fix Max' is still open"}},
		},
		{
			name: "resolved", sample: "gate-check-merged",
			want:    GateCheck{Checked: 1, Resolved: 1},
			results: []GateResult{{GateID: "fx-qba", Outcome: GateResolved, Detail: "PR 'Fix Max' was merged"}},
		},
		{
			name: "closed without merge", sample: "gate-check-closed",
			want:    GateCheck{Checked: 1, Escalated: 1},
			results: []GateResult{{GateID: "fx-8za", Outcome: GateEscalated, Detail: "PR 'Fix Max' was closed without merging"}},
		},
		{
			name: "closed without merge with --escalate", sample: "gate-check-closed-escalate", opts: GateCheckOptions{Escalate: true},
			want:    GateCheck{Checked: 1, Escalated: 1, Warnings: []string{`escalation failed for fx-8p9: exec: "gt": executable file not found in $PATH`}},
			results: []GateResult{{GateID: "fx-8p9", Outcome: GateEscalated, Detail: "PR 'Fix Max' was closed without merging"}},
		},
		{
			name: "pr deleted", sample: "gate-check-pr-missing",
			want:    GateCheck{Checked: 1, Escalated: 1},
			results: []GateResult{{GateID: "fx-8p9", Outcome: GateEscalated, Detail: "pull request not found"}},
		},
		{
			name: "gh missing", sample: "gate-check-missing-gh",
			want:      GateCheck{Checked: 1, Errors: 1, Warnings: []string{"fx-8p9: gh CLI not installed"}},
			results:   []GateResult{{GateID: "fx-8p9", Outcome: GateError, Detail: "gh CLI not installed"}},
			ghMissing: "gh CLI not installed",
		},
		{
			name: "gh not authenticated", sample: "gate-check-unauth",
			want: GateCheck{Checked: 1, Errors: 1, Warnings: []string{"fx-qba: gh pr view failed: To get started with GitHub CLI, please run:  gh auth login Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token."}},
			results: []GateResult{{GateID: "fx-qba", Outcome: GateError,
				Detail: "gh pr view failed: To get started with GitHub CLI, please run:  gh auth login Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token."}},
			ghMissing: "gh pr view failed: To get started with GitHub CLI, please run:  gh auth login Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token.",
		},
		{
			name: "gh printed garbage", sample: "gate-check-bad-gh",
			want:    GateCheck{Checked: 1, Errors: 1, Warnings: []string{`fx-8p9: failed to parse gh output: invalid character '"' after object key:value pair`}},
			results: []GateResult{{GateID: "fx-8p9", Outcome: GateError, Detail: `failed to parse gh output: invalid character '"' after object key:value pair`}},
		},
		{
			name: "no open gates", sample: "gate-check-none",
			want: GateCheck{},
		},
		{
			name: "dry run", sample: "gate-check-dry-run", opts: GateCheckOptions{DryRun: true},
			want:    GateCheck{Checked: 1, DryRun: true},
			results: []GateResult{{GateID: "fx-qba", Outcome: GatePending, Detail: "PR 'Fix Max' is still open"}},
		},
		{
			name: "two gates", sample: "gate-check-two",
			want: GateCheck{Checked: 2},
			results: []GateResult{
				{GateID: "fx-96y", Outcome: GatePending, Detail: "no PR number specified"},
				{GateID: "fx-8p9", Outcome: GatePending, Detail: "PR 'Fix Max' is still open"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, bd := gatesFake(t, map[string]string{"gate-check": tc.sample})
			got, err := c.CheckGates(ctx, tc.opts)
			require.NoError(t, err)
			want := tc.want
			want.Results = tc.results
			assert.Equal(t, want, got)

			args := clientLastArgs(t, bd)
			assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "gate", "check", "--type=gh:pr"}, args[:5])
			assert.Equal(t, "--json", args[len(args)-1])
			assert.Equal(t, tc.opts.Escalate, contains(args, "--escalate"))
			assert.Equal(t, tc.opts.DryRun, contains(args, "--dry-run"))

			reason, unavailable := got.GHUnavailable()
			assert.Equal(t, tc.ghMissing != "", unavailable)
			assert.Equal(t, tc.ghMissing, reason)
			assert.Empty(t, testutil.MissingSamples(t, bd))
		})
	}
}

func TestGateCheckHelpers(t *testing.T) {
	check := GateCheck{Checked: 4, Resolved: 1, Escalated: 1, Errors: 1, Results: []GateResult{
		{GateID: "g-1", Outcome: GateResolved},
		{GateID: "g-2", Outcome: GatePending},
		{GateID: "g-3", Outcome: GateEscalated},
		{GateID: "g-4", Outcome: GateError, Detail: "gh CLI not installed"},
	}}
	assert.Equal(t, []domain.BeadID{"g-1"}, check.ByOutcome(GateResolved))
	assert.Equal(t, []domain.BeadID{"g-3"}, check.ByOutcome(GateEscalated))
	assert.Nil(t, check.ByOutcome("nothing"))
	_, unavailable := check.GHUnavailable()
	assert.False(t, unavailable, "gh answered for three of the four gates")

	_, unavailable = GateCheck{}.GHUnavailable()
	assert.False(t, unavailable)
	_, unavailable = GateCheck{Checked: 1, Errors: 1, Results: []GateResult{{GateID: "g", Outcome: GateError, Detail: "failed to parse gh output"}}}.GHUnavailable()
	assert.False(t, unavailable, "a parse failure is not a missing gh")
}

func TestParseGateCheck(t *testing.T) {
	t.Run("tolerates the metrics preamble and notices", func(t *testing.T) {
		stdout := "○ fx-1: pending - PR 'A' is still open\n✓ fx-2: resolved - PR 'B' was merged\n\nChecked 2 gates: 1 resolved, 0 escalated, 0 errors\n{\"data\":{\"checked\":2,\"resolved\":1,\"escalated\":0,\"errors\":0,\"dry_run\":false},\"schema_version\":1}\n"
		stderr := envSample(t, "preamble.txt") + "warning: beads.role not configured (GH#2950).\n  Fix: git config beads.role maintainer\n✗ fx-3: error checking - gh CLI not installed\n"
		got, err := ParseGateCheck(stdout, stderr)
		require.NoError(t, err)
		assert.Equal(t, 2, got.Checked)
		assert.Equal(t, []GateResult{
			{GateID: "fx-1", Outcome: GatePending, Detail: "PR 'A' is still open"},
			{GateID: "fx-2", Outcome: GateResolved, Detail: "PR 'B' was merged"},
			{GateID: "fx-3", Outcome: GateError, Detail: "gh CLI not installed"},
		}, got.Results)
		assert.Equal(t, []string{"fx-3: gh CLI not installed"}, got.Warnings, "the beads.role notice is not a warning")
	})
	t.Run("no json", func(t *testing.T) {
		_, err := ParseGateCheck("something else entirely\n", "")
		assert.ErrorIs(t, err, ErrNoJSON)
	})
	t.Run("summary line only", func(t *testing.T) {
		got, err := ParseGateCheck("Checked 0 gates: 0 resolved, 0 escalated, 0 errors\n{\"data\":{\"checked\":0},\"schema_version\":1}\n", "")
		require.NoError(t, err)
		assert.Equal(t, GateCheck{}, got)
	})
}

func TestCheckGatesFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("bd missing", func(t *testing.T) {
		envIsolate(t)
		t.Setenv("PATH", t.TempDir())
		c := New("/repo", Options{Actor: "sc:test"})
		_, err := c.CheckGates(ctx, GateCheckOptions{})
		require.Error(t, err)
		assert.True(t, proc.IsNotFound(err), err.Error())
	})
	t.Run("no project", func(t *testing.T) {
		c, _ := gatesFake(t, map[string]string{"gate-check": "error-no-project"})
		_, err := c.CheckGates(ctx, GateCheckOptions{})
		assert.ErrorIs(t, err, ErrNoProject)
	})
	t.Run("cancelled context", func(t *testing.T) {
		c, bd := gatesFake(t, nil)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := c.CheckGates(cancelled, GateCheckOptions{})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, bd.ArgLists(t))
	})
}

// gatesChainBD installs a bd shim that behaves like bd 1.2.2's gate
// commands over a small state file and, for `gate check`, really runs
// `gh pr view <n> --json state,title` on PATH the way bd does, formatting
// the verdict lines from what gh answered. It exercises the whole chain
// (gh answer → bd output → parser) without the real binary. A file named
// no-gh in the shim directory makes it report gh as not installed.
func gatesChainBD(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "bd", `
gates="$SC_FAKEBIN_DIR/gates"
touch "$gates"
key=''; sub=''; blocks=''; await=''; escalate=0; skip=0
for a in "$@"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case "$a" in
    -C) skip=1 ;;
    --blocks) want=blocks ;;
    --await-id) want=await ;;
    --escalate) escalate=1 ;;
    -n|--reason) skip=1 ;;
    -*) ;;
    *) if [ -n "${want:-}" ]; then eval "$want=\"\$a\""; want=''; elif [ -z "$key" ]; then key="$a"; elif [ -z "$sub" ]; then sub="$a"; fi ;;
  esac
done
case "$key $sub" in
  "gate create")
    id="fx-g$(( $(wc -l < "$gates") + 1 ))"
    echo "$id $await $blocks open" >> "$gates"
    printf '{"data":{"id":"%s","title":"Gate: gh:pr %s","description":"Ad-hoc gate blocking %s\\n\\nReason: PR #%s","status":"open","priority":2,"issue_type":"gate","created_by":"sc:test","created_at":"2026-09-02T14:09:49Z","await_type":"gh:pr","await_id":"%s"},"schema_version":1}\n' "$id" "$await" "$blocks" "$await" "$await"
    ;;
  "gate list")
    printf '{"data":['
    first=1
    while read -r id pr bead status; do
      [ "$status" = open ] || continue
      [ "$first" = 1 ] || printf ','
      first=0
      printf '{"id":"%s","title":"Gate: gh:pr %s","description":"Ad-hoc gate blocking %s\\n\\nReason: PR #%s","status":"open","priority":2,"issue_type":"gate","await_type":"gh:pr","await_id":"%s"}' "$id" "$pr" "$bead" "$pr" "$pr"
    done < "$gates"
    printf '],"schema_version":1}\n'
    ;;
  "gate check")
    n=0; r=0; e=0; x=0
    tmp="$gates.next"; : > "$tmp"
    while read -r id pr bead status; do
      if [ "$status" != open ]; then echo "$id $pr $bead $status" >> "$tmp"; continue; fi
      n=$((n + 1))
      if [ -f "$SC_FAKEBIN_DIR/no-gh" ] || ! command -v gh >/dev/null 2>&1; then
        echo "✗ $id: error checking - gh CLI not installed" >&2; x=$((x + 1))
        echo "$id $pr $bead open" >> "$tmp"; continue
      fi
      if out=$(gh pr view "$pr" --json state,title 2>&1); then
        state=$(printf '%s' "$out" | sed -n 's/.*"state":"\([A-Z]*\)".*/\1/p')
        case "$state" in
          MERGED) echo "✓ $id: resolved - PR 'Fix Max' was merged"; r=$((r + 1)); echo "$id $pr $bead closed" >> "$tmp" ;;
          CLOSED) echo "⚠ $id: ESCALATE - PR 'Fix Max' was closed without merging"; e=$((e + 1)); echo "$id $pr $bead open" >> "$tmp"
                  [ "$escalate" = 1 ] && echo "Warning: escalation failed for $id: exec: \"gt\": executable file not found in \$PATH" >&2 ;;
          *) echo "○ $id: pending - PR 'Fix Max' is still open"; echo "$id $pr $bead open" >> "$tmp" ;;
        esac
      else
        echo "✗ $id: error checking - gh pr view failed: $out" >&2; x=$((x + 1))
        echo "$id $pr $bead open" >> "$tmp"
      fi
    done < "$gates"
    mv "$tmp" "$gates"
    if [ "$n" = 0 ]; then echo "No open gates of type 'gh:pr' found."; exit 0; fi
    echo
    echo "Checked $n gates: $r resolved, $e escalated, $x errors"
    printf '{"data":{"checked":%d,"dry_run":false,"errors":%d,"escalated":%d,"resolved":%d},"schema_version":1}\n' "$n" "$x" "$e" "$r"
    ;;
  *) echo "fakebd: unexpected command $*" >&2; exit 3 ;;
esac
exit 0
`)
}

// gatesChainGH installs a gh shim answering `gh pr view` from a state file
// in its directory: a state name (OPEN, MERGED, CLOSED) prints the JSON bd
// expects; "FAIL <message>" fails the way an unauthenticated gh does.
func gatesChainGH(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "gh", `
state=$(cat "$SC_FAKEBIN_DIR/state" 2>/dev/null || echo OPEN)
case "$state" in
  FAIL*) echo "${state#FAIL }" >&2; exit 1 ;;
esac
printf '{"state":"%s","title":"Fix Max"}\n' "$state"
`)
}

func TestCheckGatesThroughShims(t *testing.T) {
	envIsolate(t)
	bd := gatesChainBD(t)
	gh := gatesChainGH(t)
	c := New("/tmp/fixture-repo", Options{Actor: "sc:test"})
	ctx := context.Background()
	setGH := func(state string) {
		require.NoError(t, os.WriteFile(filepath.Join(gh.Dir, "state"), []byte(state), 0o644))
	}

	// No gate yet.
	check, err := c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, GateCheck{}, check)

	g1, err := c.CreatePRGate(ctx, "fx-byi", 41, "")
	require.NoError(t, err)
	g2, err := c.CreatePRGate(ctx, "fx-6z4", 42, "")
	require.NoError(t, err)
	got, err := c.GateFor(ctx, "fx-6z4")
	require.NoError(t, err)
	assert.Equal(t, g2.ID, got.ID)
	n, ok := got.PRNumber()
	require.True(t, ok)
	assert.Equal(t, 42, n)

	// Still open: nothing changes.
	setGH("OPEN")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, check.Checked)
	assert.Equal(t, []domain.BeadID{g1.ID, g2.ID}, check.ByOutcome(GatePending))
	assert.Empty(t, check.Warnings)

	// gh asked exactly the way bd does.
	ghCalls := gh.ArgLists(t)
	require.Len(t, ghCalls, 2)
	assert.Equal(t, []string{"pr", "view", "41", "--json", "state,title"}, ghCalls[0])
	assert.Equal(t, []string{"pr", "view", "42", "--json", "state,title"}, ghCalls[1])

	// Merged: both gates resolve and disappear from the open list.
	setGH("MERGED")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, check.Resolved)
	assert.Equal(t, []domain.BeadID{g1.ID, g2.ID}, check.ByOutcome(GateResolved))
	_, err = c.GateFor(ctx, "fx-byi")
	assert.ErrorIs(t, err, ErrNoGate)

	// Closed without merge: escalated, the gate stays open, --escalate
	// only adds bd's warning about the missing gt.
	g3, err := c.CreatePRGate(ctx, "fx-8b8", 43, "")
	require.NoError(t, err)
	setGH("CLOSED")
	check, err = c.CheckGates(ctx, GateCheckOptions{Escalate: true})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Escalated)
	assert.Equal(t, []domain.BeadID{g3.ID}, check.ByOutcome(GateEscalated))
	assert.Equal(t, []string{`escalation failed for fx-g3: exec: "gt": executable file not found in $PATH`}, check.Warnings)
	assert.True(t, contains(clientLastArgs(t, bd), "--escalate"))
	still, err := c.GateFor(ctx, "fx-8b8")
	require.NoError(t, err)
	assert.Equal(t, g3.ID, still.ID)

	// gh not authenticated: an error per gate, surfaced as a warning.
	setGH("FAIL To get started with GitHub CLI, please run:  gh auth login")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Errors)
	reason, unavailable := check.GHUnavailable()
	assert.True(t, unavailable)
	assert.Equal(t, "gh pr view failed: To get started with GitHub CLI, please run:  gh auth login", reason)
	assert.Equal(t, []string{"fx-g3: gh pr view failed: To get started with GitHub CLI, please run:  gh auth login"}, check.Warnings)

	// gh not installed.
	require.NoError(t, os.WriteFile(filepath.Join(bd.Dir, "no-gh"), nil, 0o644))
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	reason, unavailable = check.GHUnavailable()
	assert.True(t, unavailable)
	assert.Equal(t, "gh CLI not installed", reason)
	assert.Equal(t, []GateResult{{GateID: g3.ID, Outcome: GateError, Detail: "gh CLI not installed"}}, check.Results)
	assert.Len(t, gh.ArgLists(t), 6, "gh was not asked while missing")

	// Every bd call carried the D-21 contract.
	for _, inv := range bd.Invocations(t) {
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo"}, inv.Args[:2])
	}
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
