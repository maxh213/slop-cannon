package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/testutil"
)

// Tests for the P8-15 reconciliation of `sc status` against bd, and for
// P8-05's "sc status works while another sc holds the run lock". Both are
// about the same property: the command reports a run it does not own, so it
// must read everything it needs and write nothing at all.
//
// The recorded run is status_test.go's fixture (statusEnv, statusRecord):
// three beads, of which the log leaves sc-1 closed, sc-2 in_review and sc-3
// open. Every helper here is prefixed "live" so it cannot collide.

// liveFakeBeads answers the reconciliation from a fixture.
type liveFakeBeads struct {
	issues    []beads.Issue
	statuses  beads.StatusList
	listErr   error
	statusErr error
	// lists records the (run, all) pairs ListRun was asked for.
	lists []string
}

func (f *liveFakeBeads) ListRun(_ context.Context, run domain.RunID, all bool) ([]beads.Issue, error) {
	f.lists = append(f.lists, fmt.Sprintf("%s all=%t", run, all))
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.issues, nil
}

func (f *liveFakeBeads) StatusMap(context.Context) (beads.StatusMap, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return beads.NewStatusMap(f.statuses), nil
}

// liveInstallBeads gives the repository a beads database and points the
// reconciliation at a fake reader.
func liveInstallBeads(t *testing.T, repo string, fake *liveFakeBeads) *liveFakeBeads {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, statusBeadsDirName), 0o755))
	saved := statusLiveBeads
	t.Cleanup(func() { statusLiveBeads = saved })
	statusLiveBeads = func(string) (statusLiveReader, error) { return fake, nil }
	return fake
}

// liveIssue is one bd issue of the recorded run.
func liveIssue(id domain.BeadID, title, status string) beads.Issue {
	return beads.Issue{
		ID: id, Title: title, Status: status,
		Type: string(domain.BeadTypeTask), Priority: domain.PriorityDefault,
		Metadata: beads.Metadata{beads.MetaRun: statusRunID, beads.MetaKind: beads.KindWork},
		Labels:   []string{beads.RunLabel(statusRunID)},
	}
}

// liveIssues is what bd holds after a human closed sc-2 by hand: the run
// log's last word on it was in_review.
func liveIssues() []beads.Issue {
	return []beads.Issue{
		{ID: "sc-0", Title: "Run " + statusRunID, Status: string(domain.BeadStateOpen),
			Type:     string(domain.BeadTypeEpic),
			Metadata: beads.Metadata{beads.MetaRun: statusRunID, beads.MetaKind: beads.KindEpic}},
		liveIssue("sc-1", "Add retry", string(domain.BeadStateClosed)),
		liveIssue("sc-2", "Document retries", string(domain.BeadStateClosed)),
		liveIssue("sc-3", "Retire the old client", string(domain.BeadStateOpen)),
	}
}

// TestStatusLiveReportsABeadClosedByHand is P8-15's Done-when: the run log
// says sc-2 is in review, bd says it is closed, and `sc status` says
// closed — and names the bead that moved, because a count that changed
// without a reason is worse than no reconciliation at all.
func TestStatusLiveReportsABeadClosedByHand(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	fake := liveInstallBeads(t, repo, &liveFakeBeads{issues: liveIssues()})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "beads      3 (1 changed in bd)")
	require.Equal(t, "2", liveCount(t, out, "closed"))
	require.Equal(t, "1", liveCount(t, out, "open"))
	require.NotContains(t, out, "\n  in_review ",
		"the state the run last published is no longer a row of the counts")
	require.Contains(t, out, "sc-2  in_review -> closed (changed outside the run)")
	require.Equal(t, []string{statusRunID + " all=true"}, fake.lists,
		"closed beads are what a hand-closed bead needs, so the listing includes them")
}

// TestStatusLiveJSON: a machine consumer gets the corrected counts and the
// reconciliation itself, so it need not diff two reports to see what moved.
func TestStatusLiveJSON(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	liveInstallBeads(t, repo, &liveFakeBeads{issues: liveIssues()})

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.NotNil(t, rep.Live)
	require.Equal(t, 3, rep.Live.Checked)
	require.Empty(t, rep.Live.Added)
	require.Len(t, rep.Live.Changed, 1)
	require.Equal(t, StatusLiveChange{
		Bead: "sc-2", Title: "Document retries",
		From: domain.BeadStateInReview, To: domain.BeadStateClosed,
		Status: string(domain.BeadStateClosed),
	}, rep.Live.Changed[0])
	require.Equal(t, map[domain.BeadState]int{
		domain.BeadStateClosed: 2, domain.BeadStateOpen: 1,
	}, rep.Summary.Counts)
	require.Equal(t, 2, rep.Summary.Settled())
}

// TestStatusLiveOffKeepsTheRunLogAlone: --live=false is the log-only path,
// and it is byte for byte the report `sc status` gave before P8-15.
func TestStatusLiveOffKeepsTheRunLogAlone(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	fake := liveInstallBeads(t, repo, &liveFakeBeads{issues: liveIssues()})

	out, err := rootRun(t, "--repo", repo, "status", "--live=false")
	require.NoError(t, err)
	require.Contains(t, out, "beads      3\n")
	require.Equal(t, "1", liveCount(t, out, "in_review"))
	require.NotContains(t, out, "changed outside the run")
	require.Empty(t, fake.lists, "bd is not consulted at all")
}

// TestStatusLiveNeedsABeadsDatabase: a run that never got as far as
// creating one (or a run directory copied to another machine) reports the
// fold, with no note and no bd call.
func TestStatusLiveNeedsABeadsDatabase(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	fake := &liveFakeBeads{issues: liveIssues()}
	saved := statusLiveBeads
	t.Cleanup(func() { statusLiveBeads = saved })
	statusLiveBeads = func(string) (statusLiveReader, error) { return fake, nil }

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Equal(t, "1", liveCount(t, out, "in_review"))
	require.NotContains(t, out, "note:")
	require.Empty(t, fake.lists)
}

// TestStatusLiveSurvivesAFailingBd: a bd that is not installed is silent
// (the run directory is being read on a machine that has no bd), and a bd
// that fails for any other reason is a note, never a refusal to report.
func TestStatusLiveSurvivesAFailingBd(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)

	liveInstallBeads(t, repo, &liveFakeBeads{listErr: fmt.Errorf("bd list: %w", exec.ErrNotFound)})
	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Equal(t, "1", liveCount(t, out, "in_review"))
	require.NotContains(t, out, "note:")

	liveInstallBeads(t, repo, &liveFakeBeads{listErr: errors.New("dolt is wedged")})
	out, err = rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Equal(t, "1", liveCount(t, out, "in_review"), "the fold still stands")
	require.Contains(t, out, "note: bead states not reconciled with bd: dolt is wedged")
}

// TestStatusLiveShowsABeadTheRunLogNeverSaw: nothing in production
// publishes BeadCreated (P6-32), so a bead created outside a plan reaches
// `sc status` only through bd.
func TestStatusLiveShowsABeadTheRunLogNeverSaw(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	issues := append(liveIssues(), liveIssue("sc-9", "Review PR 12", string(domain.BeadStateOpen)))
	liveInstallBeads(t, repo, &liveFakeBeads{issues: issues})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "beads      4 (1 changed in bd, 1 not in the run log)")
	require.Contains(t, out, "sc-9  open (created outside the run)")
	require.Equal(t, "2", liveCount(t, out, "open"))
}

// TestStatusLiveMapsAForeignStatus: a repository's own status is mapped by
// its category (P3-14) and bd's word for it is kept, so a reader can tell
// the D-07 state from the custom status that maps onto it.
func TestStatusLiveMapsAForeignStatus(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	issues := liveIssues()
	issues[3] = liveIssue("sc-3", "Retire the old client", "in-qa")
	liveInstallBeads(t, repo, &liveFakeBeads{
		issues: issues,
		statuses: beads.StatusList{Custom: []beads.StatusInfo{
			{Name: "in-qa", Category: string(domain.CategoryWIP)},
		}},
	})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "sc-3  open -> blocked (changed outside the run, bd status in-qa)")
	require.Equal(t, "1", liveCount(t, out, "blocked"))
}

// TestStatusLiveScrubsWhatBdSays: a bead id, a title and a status name all
// come out of a database sc does not own, so they are scrubbed like every
// other borrowed string before they reach the terminal.
func TestStatusLiveScrubsWhatBdSays(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	issues := append(liveIssues(),
		liveIssue("sc-\x1b[1;41mX\x1b[0m", "Spoof\x1b[2J", "\x1b[31mopen\x1b[0m"))
	liveInstallBeads(t, repo, &liveFakeBeads{issues: issues})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "sc-X")
	for _, line := range strings.Split(out, "\n") {
		require.False(t, strings.ContainsFunc(line, func(r rune) bool {
			return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
		}), "control character in %q", line)
	}
}

// TestStatusLiveAgainstARealBd runs the production client — no fake — over
// a `bd` shim, which is the only way to assert what `sc status` actually
// asks bd for. Two things matter: the hand-closed bead is reported closed,
// and every call carries --readonly, because `sc status` may be reading a
// run another process is still firing.
func TestStatusLiveAgainstARealBd(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, statusBeadsDirName), 0o755))
	list, err := json.Marshal(liveIssues())
	require.NoError(t, err)
	statuses, err := json.Marshal(beads.StatusList{BuiltIn: []beads.StatusInfo{
		{Name: "open", Category: string(domain.CategoryActive)},
		{Name: "closed", Category: string(domain.CategoryDone)},
	}})
	require.NoError(t, err)
	shim := testutil.FakeBin(t, "bd", fmt.Sprintf(`
for a in "$@"; do
  case "$a" in
    list) cat <<'JSON'
%s
JSON
      exit 0 ;;
    statuses) cat <<'JSON'
%s
JSON
      exit 0 ;;
  esac
done
echo 'unexpected bd call' >&2
exit 1
`, list, statuses))

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "sc-2  in_review -> closed (changed outside the run)")
	require.Equal(t, "2", liveCount(t, out, "closed"))

	calls := shim.ArgLists(t)
	require.NotEmpty(t, calls)
	for _, args := range calls {
		require.Equal(t, "--readonly", args[0], "sc status must never be able to write: %v", args)
	}
	require.Contains(t, calls[0], "list")
}

// TestStatusGoldensHoldWithoutBd is P8-15's second Done-when: with bd
// hidden from PATH the reconciliation is silently skipped and the report is
// the recorded one, unchanged — which is what keeps the goldens
// deterministic on a machine that has bd installed.
func TestStatusGoldensHoldWithoutBd(t *testing.T) {
	base, repo := statusEnv(t)
	statusRecord(t, repo, true)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, statusBeadsDirName), 0o755))
	t.Setenv("PATH", t.TempDir()) // no bd anywhere

	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	rootGolden(t, "status-running", statusNormalise(out, base, repo))

	logOnly, err := rootRun(t, "--repo", repo, "status", "--live=false")
	require.NoError(t, err)
	require.Equal(t, logOnly, out, "no bd means the log-only report, note for note")
}

// TestStatusIsReadOnlyWhileAnotherProcessHoldsTheRunLock is P8-05's third
// clause. `sc status` is what a user reaches for while the cannon is
// firing, so it must report a locked run rather than refuse it, and must
// leave every byte of the run directory as it found it — including when it
// goes to bd for P8-15.
func TestStatusIsReadOnlyWhileAnotherProcessHoldsTheRunLock(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	liveInstallBeads(t, repo, &liveFakeBeads{issues: liveIssues()})

	// A real, live process holds the lock: another sc, as far as the run
	// directory can tell.
	child := exec.Command("/bin/sh", "-c", "sleep 60")
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	statusLock(t, dir, child.Process.Pid)

	before := liveSnapshot(t, config.StateDir(repo))
	out, err := rootRun(t, "--repo", repo, "status")
	require.NoError(t, err)
	require.Contains(t, out, fmt.Sprintf("process    pid %d holds the run lock", child.Process.Pid))
	require.Contains(t, out, "phase      running\n")
	require.NotContains(t, out, "no live process")
	require.Contains(t, out, "sc-2  in_review -> closed", "and the live reconciliation still runs")
	require.Equal(t, before, liveSnapshot(t, config.StateDir(repo)),
		"sc status wrote to the run of another process")

	// The lock itself is intact: the run that holds it is unaffected.
	rep, err := RunStatus(repo, statusRunID, 0)
	require.NoError(t, err)
	require.True(t, rep.Running())
	require.Equal(t, child.Process.Pid, rep.Liveness.Holder.PID)
}

// liveCount reads one row of the per-state bead counts. The rows are
// padded to the longest state name present, which changes with the states
// the report holds, so the count is read by field rather than by column.
func liveCount(t *testing.T, out, state string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == state {
			return f[1]
		}
	}
	t.Fatalf("no %q row in the bead counts:\n%s", state, out)
	return ""
}

// liveSnapshot hashes every file under dir with its path and mode, so a
// test can assert that a command changed nothing at all.
func liveSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out = append(out, fmt.Sprintf("%s dir %v", path, info.Mode()))
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // a temp state directory
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out = append(out, fmt.Sprintf("%s %v %s", path, info.Mode(), hex.EncodeToString(sum[:8])))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(out)
	return out
}

// TestStatusCountAfterReconciliation covers the recount directly: a bead
// the log named but never gave a state to is still counted, exactly as the
// fold counts it, so the rows keep adding up to the total.
func TestStatusCountAfterReconciliation(t *testing.T) {
	counts := statusCount([]orchestrator.BeadSummary{
		{ID: "sc-1", State: domain.BeadStateClosed},
		{ID: "sc-2", State: domain.BeadStateClosed},
		{ID: "sc-3"},
	})
	require.Equal(t, map[domain.BeadState]int{
		domain.BeadStateClosed:           2,
		orchestrator.BeadStateUnrecorded: 1,
	}, counts)
	require.Nil(t, statusCount(nil))
}

// TestRunStatusLiveIsTheProgrammaticForm: RunStatus stays the log-only
// fold every existing caller expects, and RunStatusLive is the same report
// with the reconciliation applied.
func TestRunStatusLiveIsTheProgrammaticForm(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	liveInstallBeads(t, repo, &liveFakeBeads{issues: liveIssues()})

	plain, err := RunStatus(repo, "", 0)
	require.NoError(t, err)
	require.Nil(t, plain.Live)
	b, ok := plain.Summary.Bead("sc-2")
	require.True(t, ok)
	require.Equal(t, domain.BeadStateInReview, b.State)

	rep, err := RunStatusLive(context.Background(), repo, "", 0)
	require.NoError(t, err)
	require.NotNil(t, rep.Live)
	require.Len(t, rep.Live.Changed, 1)
	b, ok = rep.Summary.Bead("sc-2")
	require.True(t, ok)
	require.Equal(t, domain.BeadStateClosed, b.State)
}
