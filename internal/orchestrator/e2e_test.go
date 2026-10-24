package orchestrator

// End-to-end test of the whole orchestrator (P6-09).
//
// The scenario the task asks for runs twice, once with safety on and once
// with safety off: a fixture git repository, a stateful `bd` shim, a `gh`
// shim, and the mock cartridge; brief mode, ammo 4, two beads with a
// dependency between them, one review round each. Everything above
// internal/beads and internal/gitx is the real thing — the planner, the
// scheduler, the supervisor, the feedback loop, the state machine, the
// delivery handoff, the gate poller, the run lifecycle and the run log —
// wired together here the way P8-01 will wire them in cmd/sc.
//
// Every helper in this file is prefixed "e2e" so it cannot collide with
// another Phase 6 task's file in this package.
//
// The `bd` shim runs testdata/orchestrator/fakebd, a small stateful stand-in
// built on demand, so the orchestrator drives the real beads.Client, the
// real D-21 argument contract and the real JSON decoding against a tracker
// that remembers what it was told — without needing bd (and its embedded
// Dolt) on the machine, and fast enough to run under `go test -race` on
// every push. `make check` therefore runs the whole scenario everywhere;
// the real bd 1.2.2 is covered by the tagged beads integration suite.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
	"slop-cannon/internal/testutil/gitguard"
)

// ---------------------------------------------------------------------------
// The stateful bd shim
// ---------------------------------------------------------------------------

// e2eBDDirEnv names the fake tracker's database directory; the shim
// exports it for testdata/orchestrator/fakebd.
const e2eBDDirEnv = "SC_E2E_FAKEBD_DIR"

// e2eEpoch is the instant the scenario's clock starts from. Timestamps are
// not asserted — the sequence of events is — but starting from a fixed
// point keeps a failing run's log readable.
var e2eEpoch = time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

// e2eRunID is the run every scenario fires; a resumed rig finds it again
// through config.LatestRunID.
const e2eRunID = "20260903-080000-e2e0"

// e2eFakeBDSource is the fake tracker's source directory, relative to this
// package.
var e2eFakeBDSource = filepath.Join("..", "..", "testdata", "orchestrator", "fakebd")

// e2eBuildFakeBD compiles testdata/orchestrator/fakebd and returns the
// binary. The result is cached in the system temporary directory under a
// name derived from the source and the toolchain, so a repeated `go test`
// reuses it and a change to the source rebuilds it.
//
// The fake tracker is a separate program rather than a re-exec of this
// test binary on purpose: under `go test -race` the instrumented test
// binary costs the best part of a second to start, and a run makes tens of
// bd calls.
func e2eBuildFakeBD(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(e2eFakeBDSource)
	require.NoError(t, err)
	sum, err := e2eSourceHash(src)
	require.NoError(t, err, "hashing the fake tracker's source")
	bin := filepath.Join(os.TempDir(), "sc-e2e-fakebd-"+sum)
	if info, err := os.Stat(bin); err == nil && info.Mode().Perm()&0o111 != 0 {
		return bin
	}
	tmp := fmt.Sprintf("%s.%d", bin, os.Getpid())
	cmd := exec.Command(e2eGoTool(t), "build", "-o", tmp, src)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "building the fake bd: %s", out)
	require.NoError(t, os.Rename(tmp, bin))
	return bin
}

// e2eGoTool locates the go command; "go" on PATH is what runs the test in
// the first place, so it is there.
func e2eGoTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	require.NoError(t, err, "the go tool is needed to build the fake bd")
	return path
}

// e2eSourceHash digests the fake tracker's Go files and the toolchain, so
// the cached binary is never stale.
func e2eSourceHash(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintln(h, runtime.Version())
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintln(h, name)
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// e2eFakeBD installs the stateful bd shim: `bd` on PATH becomes the fake
// tracker, pointed at a database directory of its own.
func e2eFakeBD(t *testing.T) *testutil.Shim {
	t.Helper()
	bin := e2eBuildFakeBD(t)
	state := t.TempDir()
	script := fmt.Sprintf("%s=%s\nexport %s\nexec %s \"$@\"\n",
		e2eBDDirEnv, e2eShellQuote(state), e2eBDDirEnv, e2eShellQuote(bin))
	return testutil.FakeBin(t, "bd", script)
}

// e2eShellQuote wraps s in single quotes for a POSIX shell.
func e2eShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// The gh shim
// ---------------------------------------------------------------------------

// e2ePRHost is the GitHub repository the gh shim pretends to serve, and
// e2eLogin the login it is authenticated as.
const (
	e2ePRHost = "https://github.com/acme/widgets"
	e2eLogin  = "sc-bot"
)

// e2eFakeGH installs a `gh` shim that opens numbered pull requests and
// remembers them (P8-26). It is the one gh stand-in the package has: the
// end-to-end scenarios and the crash-recovery children all run against it,
// so what one test proves about a pull request the next one may rely on.
//
// Three things make it behave like GitHub rather than like a stub:
//
//   - a head→number registry, so `gh pr list --head <branch>` answers with
//     the pull request that branch already has. Without it every lookup
//     said "no pull request" for ever and a re-delivery of a bead left
//     `delivering` by a crash (PRDeliverer.Deliver → FindPR → CreatePR)
//     would open a second pull request for the same branch — the shim's
//     failure, not the product's, which is exactly what a crash-recovery
//     test must not be able to hide;
//   - a number→head registry, so `gh pr view <n>` answers by number, which
//     is the query [gitx.Resumer] makes for a bead that already carries a
//     pull request and the query `bd gate check` makes for a gh:pr gate;
//   - a pull request is OPEN the first time it is looked at and MERGED
//     from the second look on. A pull request sc has just opened is open,
//     as it is in life, so a resume finds the D-20 state that hangs — a
//     pull request open with no gate watching it — and the merge lands
//     between two polls, which is what the gate poller exists to notice.
//     With the control file "automerge" present (D-31) nothing merges by
//     being looked at: a pull request is OPEN, green and CLEAN until
//     `gh pr merge` merges it, which is the run merging its own;
//   - every pull request it lists carries what D-31's ownership check
//     reads — the label, the body's marker line, the base, the author
//     `gh api user` names and the time it was opened — so a resumed run
//     can reuse the pull request it opened itself.
//
// Every invocation is recorded, so a test can assert the pull requests the
// run created and the order it asked about them in.
func e2eFakeGH(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "gh", `
_head=''
_base=''
_prev=''
for _a in "$@"; do
  if [ "$_prev" = "--head" ]; then _head=$_a; fi
  if [ "$_prev" = "--base" ]; then _base=$_a; fi
  _prev=$_a
done
_key=$(printf '%s' "$_head" | tr '/' '_')
# _state prints the state of pull request $1: OPEN until it has been looked
# at once, MERGED afterwards (or, with auto-merge, until gh pr merge).
_state() {
  if [ -f "$SC_FAKEBIN_DIR/merged.$1" ]; then echo MERGED; else echo OPEN; fi
}
# _pr prints one pull request as JSON: $1 number, $2 head branch, $3 state.
_pr() {
  _m=$(cat "$SC_FAKEBIN_DIR/marker.$1" 2>/dev/null || echo '')
  _b=$(cat "$SC_FAKEBIN_DIR/base.$1" 2>/dev/null || echo main)
  _c=$(cat "$SC_FAKEBIN_DIR/created.$1" 2>/dev/null || echo 2000-01-01T00:00:00Z)
  printf '{"number":%s,"url":"`+e2ePRHost+`/pull/%s","state":"%s","title":"Pull request %s","headRefName":"%s","baseRefName":"%s","labels":[{"name":"slop-cannon"}],"body":"%s","author":{"login":"`+e2eLogin+`"},"createdAt":"%s"}' "$1" "$1" "$3" "$1" "$2" "$_b" "$_m" "$_c"
}
# _view prints everything auto-merge reads about pull request $1: green,
# CLEAN, and at the head its branch has on the remote.
_view() {
  _h=$(cat "$SC_FAKEBIN_DIR/num.$1" 2>/dev/null || echo '')
  _m=$(cat "$SC_FAKEBIN_DIR/marker.$1" 2>/dev/null || echo '')
  _b=$(cat "$SC_FAKEBIN_DIR/base.$1" 2>/dev/null || echo main)
  _c=$(cat "$SC_FAKEBIN_DIR/created.$1" 2>/dev/null || echo 2000-01-01T00:00:00Z)
  _o=$(git ls-remote origin "refs/heads/$_h" 2>/dev/null | cut -f1)
  printf '{"number":%s,"state":"%s","isDraft":false,"headRefName":"%s","headRefOid":"%s","baseRefName":"%s","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://ci.example/test"}],"labels":[{"name":"slop-cannon"}],"body":"%s","createdAt":"%s","author":{"login":"`+e2eLogin+`"}}' "$1" "$(_state "$1")" "$_h" "${_o:-e2e-head-$1}" "$_b" "$_m" "$_c"
}
case "$1 $2" in
  "label create")
    ;;
  "api user")
    echo `+e2eLogin+`
    ;;
  "pr merge")
    echo merged > "$SC_FAKEBIN_DIR/merged.$3"
    echo "Squashed and merged pull request acme/widgets#$3"
    ;;
  "pr list")
    if [ -n "$_head" ] && [ -f "$SC_FAKEBIN_DIR/head.$_key" ]; then
      _n=$(cat "$SC_FAKEBIN_DIR/head.$_key")
      printf '['; _pr "$_n" "$_head" "$(_state "$_n")"; printf ']\n'
    else
      echo '[]'
    fi
    ;;
  "pr create")
    cat > "$SC_FAKEBIN_DIR/body.$SC_FAKEBIN_SEQ"
    w=0
    while ! mkdir "$SC_FAKEBIN_DIR/.prlock" 2>/dev/null; do
      # A shim killed while it held the lock would otherwise hang every
      # later pull request for ever, and the kill-and-resume tests kill a
      # run's process group on purpose; steal a lock nobody released
      # within five seconds. Numbering takes microseconds, so a live
      # holder never reaches that.
      if [ "$w" -ge 500 ]; then rmdir "$SC_FAKEBIN_DIR/.prlock" 2>/dev/null; w=0; continue; fi
      w=$((w + 1))
      sleep 0.01
    done
    n=$(cat "$SC_FAKEBIN_DIR/prcount" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > "$SC_FAKEBIN_DIR/prcount"
    echo "$n" > "$SC_FAKEBIN_DIR/head.$_key"
    echo "$_head" > "$SC_FAKEBIN_DIR/num.$n"
    echo "$_base" > "$SC_FAKEBIN_DIR/base.$n"
    head -n 1 "$SC_FAKEBIN_DIR/body.$SC_FAKEBIN_SEQ" > "$SC_FAKEBIN_DIR/marker.$n"
    date -u +%Y-%m-%dT%H:%M:%SZ > "$SC_FAKEBIN_DIR/created.$n"
    rmdir "$SC_FAKEBIN_DIR/.prlock"
    echo "Creating pull request in acme/widgets"
    echo "`+e2ePRHost+`/pull/$n"
    ;;
  "pr view")
    _n=$3
    if [ -f "$SC_FAKEBIN_DIR/automerge" ]; then
      case "$*" in
        *statusCheckRollup*) _view "$_n"; printf '\n' ;;
        *) _h=$(cat "$SC_FAKEBIN_DIR/num.$_n" 2>/dev/null || echo ''); _pr "$_n" "$_h" "$(_state "$_n")"; printf '\n' ;;
      esac
    else
      _h=$(cat "$SC_FAKEBIN_DIR/num.$_n" 2>/dev/null || echo '')
      _s=$(_state "$_n")
      echo merged > "$SC_FAKEBIN_DIR/merged.$_n"
      _pr "$_n" "$_h" "$_s"; printf '\n'
    fi
    ;;
  *)
    echo "gh shim: unexpected command: $*" >&2
    exit 1
    ;;
esac
`)
}

// e2eGHCommands returns the "<verb> <noun>" of every recorded gh call.
func e2eGHCommands(t *testing.T, gh *testutil.Shim) []string {
	t.Helper()
	var out []string
	for _, inv := range gh.Invocations(t) {
		if len(inv.Args) < 2 {
			out = append(out, strings.Join(inv.Args, " "))
			continue
		}
		out = append(out, inv.Args[0]+" "+inv.Args[1])
	}
	return out
}

// e2ePRCreateHeads returns the --head → --base pair of every `gh pr create`
// the run made.
func e2ePRCreateHeads(t *testing.T, rig *e2eRig) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, inv := range rig.GH.Invocations(t) {
		if len(inv.Args) < 2 || inv.Args[0] != "pr" || inv.Args[1] != "create" {
			continue
		}
		out[e2eFlagValue(inv.Args, "--head")] = e2eFlagValue(inv.Args, "--base")
	}
	return out
}

// e2eFlagValue returns the argument after name, or "".
func e2eFlagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// e2eCountCommand counts how many entries equal name.
func e2eCountCommand(list []string, name string) int {
	n := 0
	for _, s := range list {
		if s == name {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The scripted cartridge
// ---------------------------------------------------------------------------

// e2eWorkFiles are the files the two scripted engineers add, one each. They
// must differ: both branches are rebased onto the same default branch, and
// two engineers adding the same path would either produce an empty commit
// or a conflict, neither of which this scenario is about.
var e2eWorkFiles = []string{"notes/first.md", "notes/second.md"}

// e2eScenario scripts the whole run: one plan, one engineer per bead, and
// three approving testers per bead in round one. Rules match on the role,
// task and round labels rather than on agent ids, because the bead ids the
// agent ids are built from are minted by bd at run time.
func e2eScenario(brief string) *mock.Scenario {
	sc := mock.NewScenario("orchestrator end to end")
	sc.Rule("plan", mock.Match{Role: string(domain.RolePO), Task: string(events.TaskPlan)}).
		Say("Two beads, the second building on the first.").
		Structured(domain.Plan{
			Brief: brief,
			Beads: []domain.PlanBead{
				{
					Key: "notes", Title: "Add a notes file", Acceptance: "notes/first.md exists",
					Description: "Start the notes directory.", Priority: domain.PriorityDefault,
					Type: domain.BeadTypeTask,
				},
				{
					Key: "more", Title: "Extend the notes", Acceptance: "notes/second.md exists",
					Description: "Add the second note.", Priority: domain.PriorityDefault,
					Type: domain.BeadTypeTask, Deps: []string{"notes"},
				},
			},
		})
	// The two engineers are told apart by the bead in front of them, not by
	// the order they fire in: a resumed run may implement only the second
	// bead (the first was already delivered when the process died), and a
	// rule that fired Once() per process would then write the first bead's
	// file onto the second bead's branch, where the rebase drops it as an
	// empty commit (P9-03).
	sc.Rule("first engineer", mock.Match{
		Role: string(domain.RoleEngineer), PromptContains: "Add a notes file",
	}).
		Say("Adding the first note.").
		Write(e2eWorkFiles[0], "the first note\n").
		Structured(domain.ImplReport{
			Summary: "Added " + e2eWorkFiles[0] + ".", FilesChanged: []string{e2eWorkFiles[0]},
			HowToVerify: "ls " + e2eWorkFiles[0],
		})
	sc.Rule("second engineer", mock.Match{
		Role: string(domain.RoleEngineer), PromptContains: "Extend the notes",
	}).
		Say("Adding the second note.").
		Write(e2eWorkFiles[1], "the second note\n").
		Structured(domain.ImplReport{
			Summary: "Added " + e2eWorkFiles[1] + ".", FilesChanged: []string{e2eWorkFiles[1]},
			HowToVerify: "ls " + e2eWorkFiles[1],
		})
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester)}).
		Say("Reading the diff.").
		Structured(domain.Verdict{Outcome: domain.VerdictApprove, Summary: "The note reads well and is covered."})
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent the end-to-end scenario does not script")
	return sc
}

// ---------------------------------------------------------------------------
// The rig: everything P8-01 will wire in cmd/sc
// ---------------------------------------------------------------------------

// e2eBrief is the human's -m text for the scenario.
const e2eBrief = "add a short notes file and then extend it"

// e2eRig is one whole run: the target repository, the two shims, the beads
// client, the git manager and every orchestrator part, wired together.
type e2eRig struct {
	T      *testing.T
	Safety bool
	// Resumed is true for the second rig of a kill-and-resume scenario,
	// and Recovered is what the stale-lease recovery released for it.
	// Env is the machine the run happens on, so a test can reach the
	// recording shims the argv invariant is judged from (P9-23).
	Env       *e2eEnv
	Resumed   bool
	Recovered beads.RecoverResult

	// stopHelpers ends the rig's poller, conductor and ammo sampler; the
	// kill-and-resume scenario calls it so the first rig's goroutines are
	// not still talking to bd while the second rig runs.
	stopHelpers func()

	Repo *testutil.FixtureRepo
	BD   *testutil.Shim
	GH   *testutil.Shim

	RC       *config.RunConfig
	Beads    *beads.Client
	Statuses beads.StatusMap
	Manager  *gitx.Manager
	Git      DeliveryGit
	Adapter  *mock.Adapter
	Selector *cartridge.Selector

	Bus *events.Bus
	Log *EventLog

	Machine   *StateMachine
	Feedback  *Feedback
	Sup       *Supervisor
	Planner   *Planner
	Sched     *Scheduler
	Deliverer *Deliverer
	Poller    *GatePoller
	// prPath is the pull request path of a safety-on rig.
	prPath *gitx.PRDeliverer
	Run    *Run
	// Cond is the production run-level glue (P8-14). The rig drives it
	// rather than a conductor of its own: `sc run` and this test must fire
	// the same code, or the e2e proves the rig instead of the product.
	Cond *Conductor

	RepoCtx prompt.RepoContext
	now     func() time.Time

	mu        sync.Mutex
	delivered map[domain.BeadID]*DeliveryOutcome
	problems  []string
	peak      int
	peakRole  map[domain.Role]int
}

// e2eEnv is the machine a run happens on: a temporary home, the fixture
// repository with its bare remote, the fake tracker and the fake gh. It
// outlives a single run, so a killed run can be resumed by a second rig
// over the same repository, the same bd database and the same run
// directory ([e2eBuild] with resume set).
type e2eEnv struct {
	Home string
	Repo *testutil.FixtureRepo
	BD   *testutil.Shim
	GH   *testutil.Shim
	// Git is the recording git shim every run on this machine goes
	// through (P9-23). It is nil in a crash-recovery child, which
	// inherits the parent's PATH and so records into the parent's log
	// without needing to read it.
	Git *testutil.Shim

	// SafetyOn and built are what the argv invariant needs to know about
	// the runs this machine hosted; e2eBuild fills them in.
	SafetyOn bool
	built    bool
	// AutoMerge runs every rig built on this machine with auto_merge on
	// (D-31): the gh shim merges only through `gh pr merge`, and the argv
	// invariant allows exactly the merge auto-merge makes.
	AutoMerge bool
}

// e2eFakeGit installs a `git` shim that records every invocation and then
// execs the real git, so the whole end-to-end run happens for real while
// the exact argv is kept for judgement (P9-23).
//
// Only the code under test goes through it: everything the tests
// themselves run — testutil.FixtureRepo.Git and the rec* helpers built on
// it — resolves git with testutil.RealBinary, which skips shim
// directories, so what a test does to the fixture is never mistaken for
// what a run did.
func e2eFakeGit(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "git", `
if [ -z "$SC_FAKEBIN_REAL" ]; then
  echo "git shim: no real git on PATH" >&2
  exit 127
fi
exec "$SC_FAKEBIN_REAL" "$@"
`)
}

// assertGitInvariant judges every git and gh command line the runs on this
// machine produced against the P9-02 safety invariant (P9-23). It runs
// when the test ends, so it covers the whole run — the deliverers and the
// worktree manager, but also branch cleanup, the resume, the delivery
// settler and the gate poller, none of which internal/gitx's own scenario
// reaches — and it covers a crash-recovery child too, because the child
// inherits this machine's PATH and appends to these very logs.
//
// A run that made no git call at all fails rather than passing vacuously.
func (e *e2eEnv) assertGitInvariant(t *testing.T) {
	t.Helper()
	if e.Git == nil || !e.built {
		return
	}
	env := gitguard.Env{
		Safety:  gitguard.SafetyFor(e.SafetyOn),
		Default: e.Repo.DefaultBranch,
		Remote:  "origin",
		MainDir: gitguard.ResolvePath(e.Repo.Dir),
	}
	calls := gitguard.Parse(t, e.Git)
	assert.NotEmpty(t, calls, "the run made no git call at all, so the invariant would pass vacuously")
	if bad := gitguard.Scan(calls, env); len(bad) > 0 {
		assert.Failf(t, "git broke the safety invariant",
			"safety %s: git was asked to do something a run may never do:\n%s",
			env.Safety, strings.Join(bad, "\n"))
	}
	if bad := gitguard.ScanGHWith(t, e.GH, gitguard.GHOptions{AutoMerge: e.AutoMerge}); len(bad) > 0 {
		assert.Failf(t, "gh broke the safety invariant",
			"safety %s: gh was asked to do something a run may never do (D-11):\n%s",
			env.Safety, strings.Join(bad, "\n"))
	}
}

// e2eNewEnv sets up that machine. Everything it creates is temporary: the
// run directory, the worktrees and the run log live under XDG_STATE_HOME,
// and the target repository sees nothing but git's own worktree
// registration (D-23).
func e2eNewEnv(t *testing.T) *e2eEnv {
	t.Helper()

	// Build the fake tracker before HOME moves: `go build` would otherwise
	// put its module and build caches inside the test's temporary home and
	// re-download the world.
	e2eBuildFakeBD(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	env := &e2eEnv{
		Home: home,
		Repo: testutil.NewFixtureRepo(t, testutil.WithBareRemote()),
		BD:   e2eFakeBD(t),
		GH:   e2eFakeGH(t),
		Git:  e2eFakeGit(t),
	}
	// P9-23: every git and gh invocation any run on this machine makes is
	// judged by argv when the test ends, in whichever safety mode the run
	// used.
	t.Cleanup(func() { env.assertGitInvariant(t) })
	return env
}

// judgedGit judges every git and gh invocation the run has made so far
// against the safety invariant and returns the git calls, so a test can
// also assert that the scenario really produced the commands the invariant
// is about — a scan of nothing passes every rule (P9-23). The same
// judgement runs again when the test ends, covering whatever happens after
// this call.
func (r *e2eRig) judgedGit(t *testing.T) []gitguard.Call {
	t.Helper()
	r.Env.assertGitInvariant(t)
	return gitguard.Parse(t, r.Env.Git)
}

// e2eSetup builds the rig for one safety setting on a fresh machine.
func e2eSetup(t *testing.T, safety bool) *e2eRig {
	t.Helper()
	return e2eBuild(t, e2eNewEnv(t), safety, false)
}

// e2eBuild assembles the whole stack over env: beads, git, the cartridge
// selector, the state machine, the feedback loop, the supervisor, the
// planner, the scheduler, the run, the deliverer and the gate poller —
// every one of them the production type. With resume set it continues the
// run env already carries: the frozen configuration comes back from
// state.json (P6-11), the leases of the killed process's agents are
// recovered (P3-05) and the run acts on what gitx finds in the worktrees
// (P6-15).
func e2eBuild(t *testing.T, env *e2eEnv, safety, resume bool) *e2eRig {
	t.Helper()
	ctx := context.Background()

	repo, bd, gh := env.Repo, env.BD, env.GH
	env.SafetyOn, env.built = safety, true
	start := time.Now()
	rig := &e2eRig{
		T: t, Safety: safety, Repo: repo, BD: bd, GH: gh, Env: env, Resumed: resume,
		delivered: map[domain.BeadID]*DeliveryOutcome{},
		peakRole:  map[domain.Role]int{},
		now:       func() time.Time { return e2eEpoch.Add(time.Since(start)) },
	}

	cfg := config.Defaults()
	cfg.Ammo = 4
	cfg.Safety = safety
	// A trivial check command: the fixture repository ships two planted
	// bugs on purpose (testutil.PlantedBugTests), so `go test ./...` would
	// fail the safety-off delivery for reasons this test is not about. The
	// crash-recovery child replaces it with one that blocks, which is how a
	// kill point lands inside a delivery (P9-03, recModeMerging); nothing
	// but that child sets the variable.
	cfg.CheckCommand = "true"
	if hold := os.Getenv(recCheckEnv); hold != "" {
		cfg.CheckCommand = hold
	}
	cfg.ActiveCartridge = mock.Name
	cfg.Cartridges = map[string]config.Cartridge{mock.Name: {}}
	if env.AutoMerge {
		cfg.AutoMerge = true
		// Two green pull requests must both merge within the test, so the
		// spacing is short; that it is honoured is a unit test's business.
		cfg.AutoMergeSpacing = config.NewDuration(time.Millisecond)
		cfg.AutoMergeChecks = []string{"test"}
	}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	settings := config.RunSettings{
		ID: e2eRunID, Brief: e2eBrief, StartedAt: e2eEpoch, Headless: true,
	}
	if resume {
		// A resumed invocation names no run: it takes the latest one.
		settings = config.RunSettings{Resume: true, StartedAt: e2eEpoch, Headless: true}
	}
	rc, err := config.NewRunConfig(repo.Dir, eff, settings)
	require.NoError(t, err)
	if resume {
		restored, rerr := RestoreRunConfig(rc)
		require.NoError(t, rerr)
		require.True(t, restored.Restored, "the killed run left a state file to restore")
		require.Equal(t, e2eRunID, string(rc.Run.ID))
	}
	rig.RC = rc

	adapter, err := mock.NewAdapter(mock.Name, e2eScenario(e2eBrief))
	require.NoError(t, err)
	rig.Adapter = adapter
	sel, err := cartridge.NewSelector(cfg, map[string]cartridge.Cartridge{mock.Name: adapter})
	require.NoError(t, err)
	rig.Selector = sel

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// beads: register the D-07 statuses once, then read them back so no
	// foreign status is ever transitioned (P6-12).
	client := beads.New(repo.Dir, beads.Options{Logger: logger})
	rig.Beads = client
	statuses, err := client.EnsureStatuses(ctx)
	require.NoError(t, err, "registering the D-07 custom statuses")
	rig.Statuses = beads.NewStatusMap(statuses.Statuses)

	// git: the worktrees live under the run directory (D-23).
	dirs, err := EnsureRunDir(rc.Run.StateDir)
	require.NoError(t, err)
	gitEnv := map[string]string{}
	for _, kv := range repo.Env() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			gitEnv[k] = v
		}
	}
	gitRepo, err := gitx.Open(ctx, repo.Dir, gitx.WithEnv(gitEnv))
	require.NoError(t, err)
	manager, err := gitx.NewManager(gitRepo, dirs.Dir)
	require.NoError(t, err)
	rig.Manager = manager
	rig.Git = NewDeliveryGit(manager)

	scan, err := prompt.ScanRepo(repo.Dir, prompt.ScanOptions{})
	require.NoError(t, err)
	scan.DefaultBranch = repo.DefaultBranch
	scan.CheckCommand = cfg.CheckCommand
	rig.RepoCtx = scan

	// events: the run log is first in the tee so a line is durable before
	// any subscriber renders it (P6-08).
	bus := events.NewBus()
	rig.Bus = bus
	log, err := OpenEventLog(rc.Run.StateDir, logger)
	require.NoError(t, err)
	rig.Log = log
	t.Cleanup(func() { _ = log.Close(); bus.Close() })

	// Everything publishes through Run.Publish, so the run log and the
	// sequence number in state.json see every event (P6-01, P6-08). The
	// run does not exist yet, hence the indirection.
	sink := PublisherFunc(rig.publish)
	rig.Machine = NewStateMachine(client,
		WithStatePublisher(sink), WithStateRun(rc.Run.ID), WithStateClock(rig.now))

	feedback, err := NewFeedback(FeedbackOptions{
		Context: rig.beadContext, States: rig.Machine, Messages: client, Bus: sink,
		Run: rc.Run.ID, Now: rig.now, Logger: logger, Limits: cfg.Limits,
		Ratio:            rig.changeRatio,
		ConflictResolved: rig.conflictResolved,
	})
	require.NoError(t, err)
	rig.Feedback = feedback

	sup, err := NewSupervisor(SupervisorOptions{
		Cartridges: sel, Bus: bus, Timeout: 30 * time.Second, Now: rig.now, Logger: logger,
		NoShims: true,
		Planner: FiringPlannerFunc(func(ctx context.Context, f Firing) (AgentFiring, error) {
			if feedback.Handles(f.Demand) {
				return feedback.PlanFiring(ctx, f)
			}
			return AgentFiring{}, fmt.Errorf("%w: %s", ErrUnknownFeedbackWork, f.Demand.Kind)
		}),
	})
	require.NoError(t, err)
	rig.Sup = sup
	t.Cleanup(func() { _ = sup.Close() })

	planner, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: SupervisorPlanRunner(sup), Applier: client,
		Beads: rig.beadSource, Credits: sup, Repo: scan, NoShim: true,
		Now: rig.now, Logger: logger, SettleInterval: 10 * time.Millisecond,
		Status: func() SchedulerStatus { return rig.schedulerStatus() },
	})
	require.NoError(t, err)
	rig.Planner = planner

	sched := NewScheduler(SchedulerOptions{
		Allocation: rc.Allocation,
		Work: WorkSourceFunc(func(ctx context.Context) ([]Demand, error) {
			return rig.workSource().Demands(ctx)
		}),
		Fire: FirerFunc(func(ctx context.Context, f Firing) (FireResult, error) {
			res, err := PlanRouter(planner, sup).Fire(ctx, f)
			if err != nil && ctx.Err() == nil {
				// A firing that cannot even be built is re-offered every
				// tick, so without this the run spins in silence: the
				// supervisor never gets as far as publishing AgentFailed.
				rig.problem("firing %s for %s: %v", f.Demand.Kind, f.Demand.Bead, err)
			}
			return res, err
		}),
		Claims: client, Stop: planner,
		Tick: 15 * time.Millisecond, SuppressAgentEvents: true,
	})
	rig.Sched = sched

	runOpts := Options{
		Loop: sched, Bus: Tee(log, bus), Logger: logger, Now: rig.now,
		Out: io.Discard, StopGrace: 2 * time.Second,
		Agents: sup, Leases: client, Cleaner: manager, Beads: rig.beadSource,
	}
	if resume {
		// A killed run leaves its beads claimed and its worktrees where
		// they were: the leases come back first (P3-05), then the run acts
		// on what gitx finds in the worktrees (P6-15).
		rec, rerr := client.RecoverStale(ctx, beads.RecoverOptions{Run: rc.Run.ID, Now: rig.now})
		require.NoError(t, rerr)
		rig.Recovered = rec
		// The tracker gitx writes the reconciled states through is the
		// production P6-37 guard, not the raw client: `sc run` wraps one
		// around it (cli.runResumer) and every bead-status write of a
		// resume is checked against the D-07 table before it reaches bd.
		// Without it these tests would prove a resume path the shipped
		// binary does not have.
		tracker, rerr := NewResumeTracker(ResumeTrackerOptions{
			Store: client, Beads: client, Statuses: rig.Statuses, Logger: logger,
		})
		require.NoError(t, rerr)
		resumer, rerr := gitx.NewResumer(manager, tracker, gitx.WithResumeLogger(logger))
		require.NoError(t, rerr)
		runOpts.Resumer = resumer
		// The production hand-off type as well (P9-03): `sc run` gives the
		// run exactly this, with exactly this late binding.
		runOpts.OnResume = DeliveryResumeHandler{Deliverer: rig.deliverer}
	}
	run, err := New(rc, runOpts)
	require.NoError(t, err)
	rig.Run = run
	sup.Attach(run)
	planner.Attach(run)

	rig.setDeliveryPaths()

	var autoMerge *AutoMergeOptions
	if snap := rc.Run.Config; snap.Safety && snap.AutoMerge {
		// The production parts, as cli.runAutoMerge assembles them; the
		// conductor is built below, so the outcome applier is late-bound.
		autoMerge = &AutoMergeOptions{
			GH: rig.prPath.GH(), Branches: rig.prPath, Ledger: run, Outcomes: e2eOutcomes{rig: rig},
			Checks: snap.AutoMergeChecks, Spacing: snap.AutoMergeSpacing, RunStarted: rc.Run.StartedAt,
		}
	}
	poller, err := NewGatePoller(GatePollerOptions{
		Gates: client, Beads: client, States: rig.Machine, Host: run,
		Scope: rc.Run.ID, Statuses: rig.Statuses, Branch: repo.DefaultBranch,
		// The poller is kicked the moment a pull request is opened, so
		// this interval only bounds how long a merge that happened
		// behind the run's back waits. Keeping it well above the
		// conductor's leaves the serialised bd client free for the work.
		Interval: 100 * time.Millisecond, Logger: logger,
		AutoMerge: autoMerge,
	})
	require.NoError(t, err)
	rig.Poller = poller

	// P8-14: the run-level glue is the production Conductor. The rig only
	// records the delivery outcomes on their way past (e2eDelivery), which
	// the assertions read.
	cond, err := NewConductor(ConductorOptions{
		Run: run, Beads: client, Statuses: rig.Statuses,
		Worktrees: manager, Git: rig.Git, Repo: scan,
		Brief: e2eBrief, CheckCommand: scan.CheckCommand,
		Timeout: 30 * time.Second, Interval: 15 * time.Millisecond,
		Logger: logger,
	})
	require.NoError(t, err)
	cond.Attach(ConductorParts{
		Feedback: feedback, Delivery: e2eDelivery{rig: rig}, Gates: poller,
	})
	rig.Cond = cond

	return rig
}

// e2eOutcomes hands a bead auto-merge sends back to the rig's conductor,
// which is built after the poller.
type e2eOutcomes struct{ rig *e2eRig }

func (o e2eOutcomes) ApplyOutcome(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) error {
	return o.rig.Cond.ApplyOutcome(ctx, id, out)
}

// e2eDelivery is the glue's delivery seam with the rig's own bookkeeping
// around it: every outcome is recorded so the assertions can read what the
// run shipped and how.
type e2eDelivery struct{ rig *e2eRig }

func (d e2eDelivery) Deliver(ctx context.Context, req DeliveryRequest) (*DeliveryOutcome, error) {
	out, err := d.rig.deliverer().Deliver(ctx, req)
	d.rig.record(req.Bead.ID, out, err)
	return out, err
}

func (d e2eDelivery) Resolved(ctx context.Context, id domain.BeadID) (*DeliveryOutcome, error) {
	out, err := d.rig.deliverer().Resolved(ctx, id)
	d.rig.record(id, out, err)
	return out, err
}

// record remembers one delivery outcome (and reports a failure, which is
// far more use than a timeout).
func (r *e2eRig) record(id domain.BeadID, out *DeliveryOutcome, err error) {
	if err != nil {
		r.problem("delivering %s: %v", id, err)
		return
	}
	if out == nil {
		return
	}
	r.mu.Lock()
	r.delivered[id] = out
	r.mu.Unlock()
}

// deliverer reads the rig's deliverer under the lock.
func (r *e2eRig) deliverer() *Deliverer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Deliverer
}

// setDeliveryPaths gives the deliverer the path the run's safety setting
// needs. It is separate from e2eSetup because the pull request deliverer
// needs the beads client as its tracker and the merge path needs none.
func (r *e2eRig) setDeliveryPaths() {
	r.T.Helper()
	opts := DeliveryOptions{
		Host: r.Run, States: r.Machine, Git: r.Git, Beads: r.Beads,
		CheckCommand: r.RC.Effective.CheckCommand, CheckTimeout: time.Minute,
		// D-31: what `sc run` passes, so a resume reuses the pull request
		// the run opened before it was killed.
		RunStarted: r.RC.Run.StartedAt,
	}
	if r.Safety {
		pr, err := gitx.NewPRDeliverer(r.Manager, r.Beads,
			gitx.WithPRLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		require.NoError(r.T, err)
		opts.PR = pr
		r.prPath = pr
	} else {
		opts.Merge = r.Manager
	}
	deliverer, err := NewDeliverer(opts)
	require.NoError(r.T, err)
	r.Deliverer = deliverer
}

// publish sends an event through the run once there is one, so the run
// log and state.json's sequence number see it (P6-01). Before the run
// exists — nothing publishes then — it falls back to the bare bus.
func (r *e2eRig) publish(ev events.Event) {
	if r.Run != nil {
		r.Run.Publish(ev)
		return
	}
	r.Bus.Publish(ev)
}

// workSource concatenates the three sources the scheduler draws from, in
// the order the wiring note prescribes.
func (r *e2eRig) workSource() WorkSource {
	srcs := []WorkSource{r.Planner, r.Feedback}
	r.mu.Lock()
	deliverer := r.Deliverer
	r.mu.Unlock()
	if deliverer != nil {
		srcs = append(srcs, deliverer)
	}
	return WorkSources(srcs...)
}

// schedulerStatus is PlannerOptions.Status, guarded because the planner is
// built before the scheduler it reports on.
func (r *e2eRig) schedulerStatus() SchedulerStatus {
	if r.Sched == nil {
		return SchedulerStatus{}
	}
	return r.Sched.Status()
}

// beadSource lists the run's beads with the states bd holds. It is the
// BeadSource the planning loop's stop condition and the run's finish pass
// both read.
func (r *e2eRig) beadSource(ctx context.Context) ([]domain.Bead, error) {
	issues, err := r.Beads.ListRun(ctx, r.RC.Run.ID, true)
	if err != nil {
		return nil, err
	}
	return beads.SafeBeads(issues, r.Statuses), nil
}

// beadContext, changeRatio and conflictResolved are the three seams the
// feedback loop is given. All three are the production glue's
// ([Conductor.BeadContext], which makes sure the bead has a worktree,
// commits whatever the last engineer left uncommitted through DeliveryGit
// so the testers always review committed code (D-14, P6-16(b), P8-18), and
// describes the change set). They are indirections only because the loop is
// built before the run, and the glue after it.
func (r *e2eRig) beadContext(ctx context.Context, id domain.BeadID) (FeedbackBeadContext, error) {
	return r.Cond.BeadContext(ctx, id)
}

// changeRatio is the D-09 measurement, taken from the real repository.
func (r *e2eRig) changeRatio(ctx context.Context, id domain.BeadID, previous, current *gitx.Diff) (gitx.ChangeRatio, error) {
	return r.Cond.ChangeRatio(ctx, id, previous, current)
}

// conflictResolved hands a resolved delivery conflict back to the delivery
// handoff, which owns what happens to the bead next (P6-06).
func (r *e2eRig) conflictResolved(ctx context.Context, id domain.BeadID) error {
	return r.Cond.ConflictResolved(ctx, id)
}

// ---------------------------------------------------------------------------
// The conductor: the run-level glue P8-01 owns
// ---------------------------------------------------------------------------

// start starts the run, the gate poller and the conductor, and registers
// the shutdown the test needs whatever happens.
func (r *e2eRig) start(ctx context.Context) {
	r.T.Helper()
	require.NoError(r.T, r.Run.Start(ctx))

	loop, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = r.Poller.Run(loop) }()
	go func() { defer wg.Done(); _ = r.Cond.Run(loop) }()
	go func() { defer wg.Done(); r.sampleAmmo(loop) }()
	r.stopHelpers = func() { cancel(); wg.Wait() }
	r.T.Cleanup(func() {
		_ = r.Run.Stop("test cleanup")
		select {
		case <-r.Run.Done():
		case <-time.After(testutil.Patience(20 * time.Second)):
			r.T.Error("the run did not finish")
		}
		cancel()
		wg.Wait()
	})
}

// sampleAmmo watches the scheduler while the run is live and remembers the
// most agents it ever had running, overall and per role. It is how the
// Phase 6 exit criterion "the scheduler never exceeds ammo" is checked
// against a real run rather than against a property test's model.
func (r *e2eRig) sampleAmmo(ctx context.Context) {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		st := r.Sched.Status()
		r.mu.Lock()
		if st.Live > r.peak {
			r.peak = st.Live
		}
		for role, n := range st.ByRole {
			if n > r.peakRole[role] {
				r.peakRole[role] = n
			}
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-r.Run.Done():
			return
		case <-tick.C:
		}
	}
}

// Peak returns the most agents that ever ran at once and the most of each
// role.
func (r *e2eRig) Peak() (int, map[domain.Role]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[domain.Role]int, len(r.peakRole))
	for k, v := range r.peakRole {
		out[k] = v
	}
	return r.peak, out
}

// wait blocks until the run has finished and returns its outcome.
func (r *e2eRig) wait(timeout time.Duration) (Outcome, error) {
	r.T.Helper()
	done := make(chan struct{})
	var (
		out Outcome
		err error
	)
	go func() { defer close(done); out, err = r.Run.Wait() }()
	select {
	case <-done:
		return out, err
	case <-time.After(timeout):
		r.T.Fatalf("the run did not finish in %s\n%s", timeout, r.diagnose())
		return Outcome{}, nil
	}
}

// diagnose describes what the run was doing when it ran out of time. A bare
// "it did not finish" says nothing about whether the run was starved, stuck
// on one bead or spinning, and this test is the only place several of these
// components meet, so the report is worth the twenty lines.
func (r *e2eRig) diagnose() string {
	var b strings.Builder
	st := r.Sched.Status()
	fmt.Fprintf(&b, "scheduler: ticks=%d fired=%d live=%d idle=%v caps=%s\n",
		st.Ticks, st.Fired, st.Live, st.Idle, st.Caps)
	for _, a := range st.Agents {
		fmt.Fprintf(&b, "  agent %s %s/%s %s bead=%s\n", a.ID, a.Role, a.Lens, a.State, a.Bead)
	}
	ps := r.Planner.Status()
	fmt.Fprintf(&b, "planner: attempts=%d plans=%d beads=%d empty=%d rejections=%d gaveUp=%d lastErr=%v\n",
		ps.Attempts, ps.Plans, ps.Beads, ps.Empty, ps.Rejections, ps.GaveUp, ps.LastError)
	fmt.Fprintf(&b, "gates: %+v\n", r.Poller.Stats())
	for _, t := range r.Feedback.Threads() {
		fmt.Fprintf(&b, "thread %s state=%s phase=%s round=%d reviewers=%v approved=%v items=%d\n",
			t.Bead, t.State, t.Phase, t.Round, t.Reviewers, t.Approved, len(t.Items))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testutil.Patience(10*time.Second))
	defer cancel()
	if list, err := r.beadSource(ctx); err == nil {
		for _, bd := range list {
			fmt.Fprintf(&b, "bead %s %s assignee=%q\n", bd.ID, bd.State, bd.Assignee)
		}
	} else {
		fmt.Fprintf(&b, "beads: %v\n", err)
	}
	uses := map[string]int{}
	for _, c := range r.Adapter.Calls() {
		uses[c.Rule]++
	}
	fmt.Fprintf(&b, "cartridge uses: %v\n", uses)
	fmt.Fprintf(&b, "bd calls: %v\n", r.BDCalls())
	if evs, err := ReadEventLog(r.RC.Run.StateDir); err == nil {
		var failures []string
		for _, ev := range evs {
			if f, ok := ev.(events.AgentFailed); ok {
				failures = append(failures, fmt.Sprintf("%s %s: %v", f.Agent, f.Failure, f.Err))
			}
		}
		if n := len(failures); n > 0 {
			fmt.Fprintf(&b, "agent failures (%d, last 3): %v\n", n, failures[max(0, n-3):])
		}
	}
	fmt.Fprintf(&b, "problems: %v", r.Problems())
	return b.String()
}

// problem records something the conductor could not do; a failing test
// prints them all, which is far more use than one timeout.
func (r *e2eRig) problem(format string, a ...any) {
	r.mu.Lock()
	r.problems = append(r.problems, fmt.Sprintf(format, a...))
	r.mu.Unlock()
}

// Problems returns what the conductor could not do.
func (r *e2eRig) Problems() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.problems...)
}

// Outcomes returns the delivery outcome of every bead the run shipped.
func (r *e2eRig) Outcomes() map[domain.BeadID]*DeliveryOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[domain.BeadID]*DeliveryOutcome, len(r.delivered))
	for k, v := range r.delivered {
		out[k] = v
	}
	return out
}

// States returns the final state of every work bead of the run, keyed by
// bead id, read back from bd rather than from anything the run remembers.
func (r *e2eRig) States(ctx context.Context) map[domain.BeadID]domain.BeadState {
	r.T.Helper()
	list, err := r.beadSource(ctx)
	require.NoError(r.T, err)
	out := map[domain.BeadID]domain.BeadState{}
	for _, b := range list {
		if b.Type == domain.BeadTypeEpic {
			continue
		}
		out[b.ID] = b.State
	}
	return out
}

// Titles maps every work bead to its title, so assertions can name beads
// by what the plan called them rather than by the ids bd minted.
func (r *e2eRig) Titles(ctx context.Context) map[string]domain.BeadID {
	r.T.Helper()
	list, err := r.beadSource(ctx)
	require.NoError(r.T, err)
	out := map[string]domain.BeadID{}
	for _, b := range list {
		if b.Type != domain.BeadTypeEpic {
			out[b.Title] = b.ID
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The scenario
// ---------------------------------------------------------------------------

// e2eTimeout bounds one whole run. It is a deadlock bound, not a
// performance budget: the four scenarios together take about 12 s untagged
// and about 30 s under -race, well inside the 30 s per run P6-09 asks for,
// and this only decides how long a genuinely stuck run hangs before the
// test says so. It is deliberately far above the real cost, because the
// scenario spawns a `bd` subprocess per tracker call and several
// race-instrumented copies of this package running at once make each of
// those an order of magnitude slower.
const e2eTimeout = 3 * time.Minute

// TestE2EBriefRunWithSafetyOn runs the whole cannon at the fixture
// repository with safety on: two beads with a dependency, one review round
// each, a pull request per bead and a gh:pr gate that the gate poller
// closes once the shim reports the pull request merged (D-10, D-20).
func TestE2EBriefRunWithSafetyOn(t *testing.T) {
	ctx := context.Background()
	rig := e2eSetup(t, true)
	mainBefore := rig.Repo.Head(t)

	rig.start(ctx)
	out, err := rig.wait(e2eTimeout)
	require.NoError(t, err, "problems: %v", rig.Problems())
	assert.Empty(t, rig.Problems(), "the conductor hit no problem")
	assert.Equal(t, events.FinishCompleted, out.Reason)

	e2eAssertBothBeadsShipped(ctx, t, rig)

	// Safety on never touches the default branch, locally or on the remote.
	assert.Equal(t, mainBefore, rig.Repo.Head(t), "the local default branch must not move (D-10)")
	assert.Equal(t, mainBefore, e2eRemoteHead(t, rig), "the remote default branch must not move (D-10)")

	// One pull request per bead, and every one of them behind a gate the
	// poller resolved.
	gh := e2eGHCommands(t, rig.GH)
	assert.Equal(t, 2, e2eCountCommand(gh, "pr create"), "one pull request per bead (gh calls: %v)", gh)
	assert.GreaterOrEqual(t, e2eCountCommand(gh, "pr view"), 2, "the gate poller asked gh about both pull requests")
	assert.Zero(t, e2eCountCommand(gh, "pr merge"), "auto_merge off: the run never merges anything (D-10, D-31)")
	assert.Zero(t, e2eCountCommand(gh, "pr update-branch"))
	assert.False(t, rig.Poller.Stats().AutoMergeOn)
	heads := e2ePRCreateHeads(t, rig)
	require.Len(t, heads, 2)
	for head, base := range heads {
		assert.Truef(t, strings.HasPrefix(head, gitx.BranchPrefix), "%s is an sc/ branch", head)
		assert.Equalf(t, rig.Repo.DefaultBranch, base, "%s targets the default branch", head)
		assert.NotEmptyf(t, rig.Repo.Git(t, "ls-remote", rig.Repo.RemoteDir, "refs/heads/"+head),
			"%s was pushed before the pull request was opened", head)
	}
	for id, o := range rig.Outcomes() {
		assert.Equalf(t, DeliveryPending, o.Kind, "%s took the pull request path", id)
		require.NotNilf(t, o.PR, "%s has a pull request", id)
		assert.Containsf(t, o.PR.URL, e2ePRHost+"/pull/", "%s: pull request URL", id)
		assert.NotEmptyf(t, o.Gate, "%s is behind a gh:pr gate (D-20)", id)
	}
	gates, err := rig.Beads.Gates(ctx, true)
	require.NoError(t, err)
	require.Len(t, gates, 2, "one gh:pr gate per bead")
	for _, g := range gates {
		assert.Falsef(t, g.IsOpen(), "gate %s was resolved by the poller", g.ID)
	}
	assert.Equal(t, 2, rig.Poller.Stats().Closed, "the poller closed both beads")
	calls := rig.BDCalls()
	assert.Equal(t, 2, calls["gate-create"], "one gh:pr gate per bead (D-20)")
	assert.GreaterOrEqual(t, calls["gate-check"], 1, "the poller ran `bd gate check`")

	// P9-23: the whole run — the deliverer, the worktree manager, branch
	// cleanup and the gate poller alike — is judged by argv and not only
	// by where the branch tips ended up, and the scenario really produced
	// the commands that invariant is about, so it cannot pass vacuously.
	git := rig.judgedGit(t)
	assert.GreaterOrEqual(t, gitguard.Count(git, "worktree"), 2, "a worktree per bead (git calls: %d)", len(git))
	assert.Positive(t, gitguard.Count(git, "push"), "the sc/ branches were pushed")
	assert.Zero(t, gitguard.Count(git, "merge"), "safety on merges nothing (D-10)")

	e2eAssertEvents(t, rig, true)
	e2eAssertStateFile(t, rig, out)
}

// TestE2EBriefRunWithSafetyOnAndAutoMerge runs the safety-on scenario with
// auto_merge on (D-31) against a GitHub on which nothing merges unless the
// run asks: both pull requests are green and CLEAN, the run merges each of
// its own with exactly the squash that names the head it read, and the gate
// poller then closes both beads as it does for a human merge.
func TestE2EBriefRunWithSafetyOnAndAutoMerge(t *testing.T) {
	ctx := context.Background()
	env := e2eNewEnv(t)
	env.AutoMerge = true
	require.NoError(t, os.WriteFile(filepath.Join(env.GH.Dir, "automerge"), nil, 0o644))
	rig := e2eBuild(t, env, true, false)
	mainBefore := rig.Repo.Head(t)

	rig.start(ctx)
	out, err := rig.wait(e2eTimeout)
	require.NoError(t, err, "problems: %v", rig.Problems())
	assert.Empty(t, rig.Problems(), "the conductor hit no problem")
	assert.Equal(t, events.FinishCompleted, out.Reason)
	e2eAssertBothBeadsShipped(ctx, t, rig)

	// The run's git still never touches the default branch: the merge is
	// GitHub's, made through gh.
	assert.Equal(t, mainBefore, rig.Repo.Head(t))
	assert.Equal(t, mainBefore, e2eRemoteHead(t, rig))

	// Exactly one merge per pull request the run opened, each the exact
	// squash at the head the same poll read, and nothing else.
	created := map[string]bool{}
	for _, inv := range rig.GH.Invocations(t) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			created[e2eFlagValue(inv.Args, "--head")] = true
		}
	}
	require.Len(t, created, 2)
	var merged []string
	for _, inv := range rig.GH.Invocations(t) {
		if len(inv.Args) < 2 || inv.Args[0] != "pr" || inv.Args[1] != "merge" {
			continue
		}
		require.Len(t, inv.Args, 6, "gh %s", strings.Join(inv.Args, " "))
		n := inv.Args[2]
		head, err := os.ReadFile(filepath.Join(rig.GH.Dir, "num."+n))
		require.NoError(t, err, "pull request #%s was opened by this run", n)
		branch := strings.TrimSpace(string(head))
		assert.True(t, created[branch], "#%s is one of the run's own", n)
		tip := strings.Fields(rig.Repo.Git(t, "ls-remote", rig.Repo.RemoteDir, "refs/heads/"+branch))
		require.NotEmpty(t, tip)
		assert.Equal(t, []string{"pr", "merge", n, "--squash", "--match-head-commit", tip[0]}, inv.Args, "the tip the run delivered and reviewed")
		merged = append(merged, n)
	}
	assert.Len(t, merged, 2, "both of the run's pull requests were merged, once each")
	gh := e2eGHCommands(t, rig.GH)
	assert.Zero(t, e2eCountCommand(gh, "pr update-branch"), "nothing was behind")
	assert.Equal(t, 1, e2eCountCommand(gh, "api user"), "the login is read once")
	stats := rig.Poller.Stats()
	assert.True(t, stats.AutoMergeOn)
	assert.Equal(t, 2, stats.AutoMerged)
	assert.Equal(t, 2, stats.Closed, "the poller closed both beads once bd saw the merges")
	am := rig.Run.State().AutoMerge
	require.NotNil(t, am, "the merges are in state.json")
	assert.Equal(t, 2, am.Merges)

	// The invariant is meaningful: the same argv judged without the
	// auto-merge opt-in reports both merges as damage.
	bad := gitguard.ScanGH(t, rig.GH)
	require.Len(t, bad, 2, "%v", bad)
	for _, b := range bad {
		assert.Contains(t, b, "gh pr merge")
	}
	git := rig.judgedGit(t)
	assert.Zero(t, gitguard.Count(git, "merge"), "the run's git merges nothing")
	e2eAssertStateFile(t, rig, out)
}

// TestE2EBriefRunWithSafetyOff runs the same scenario with safety off: both
// beads are rebased and fast-forwarded onto the default branch, no pull
// request is opened and no merge commit appears (D-10).
func TestE2EBriefRunWithSafetyOff(t *testing.T) {
	ctx := context.Background()
	rig := e2eSetup(t, false)
	mainBefore := rig.Repo.Head(t)

	rig.start(ctx)
	out, err := rig.wait(e2eTimeout)
	require.NoError(t, err, "problems: %v", rig.Problems())
	assert.Empty(t, rig.Problems(), "the conductor hit no problem")
	assert.Equal(t, events.FinishCompleted, out.Reason)

	e2eAssertBothBeadsShipped(ctx, t, rig)

	// Both engineers' work landed on the default branch, as a
	// fast-forward: no merge commit, and every file the plan promised.
	assert.NotEqual(t, mainBefore, rig.Repo.Head(t), "the default branch moved")
	assert.Empty(t, rig.Repo.Git(t, "log", "--merges", "--format=%H", mainBefore+"..HEAD"),
		"safety off fast-forwards; it never writes a merge commit (D-10)")
	for _, f := range e2eWorkFiles {
		assert.FileExistsf(t, filepath.Join(rig.Repo.Dir, f), "%s landed on the default branch", f)
	}
	assert.Equal(t, rig.Repo.Head(t), e2eRemoteHead(t, rig), "the default branch was pushed")

	assert.Empty(t, e2eGHCommands(t, rig.GH), "safety off opens no pull request (D-10)")
	assert.Zero(t, rig.BDCalls()["gate-create"], "safety off creates no gh:pr gate (D-20)")
	shas := map[string]bool{}
	for id, o := range rig.Outcomes() {
		assert.Equalf(t, DeliveryMerged, o.Kind, "%s took the merge path", id)
		assert.Equalf(t, rig.Repo.DefaultBranch, o.Target, "%s merged into the default branch", id)
		assert.Truef(t, o.Pushed, "%s was pushed", id)
		require.NotEmptyf(t, o.MergeSHA, "%s recorded a merge sha", id)
		shas[o.MergeSHA] = true
	}
	assert.Len(t, shas, 2, "each bead landed its own commit")

	// The merge sha of each bead is on the default branch and is recorded
	// on the bead (D-20).
	for id := range rig.Outcomes() {
		issues, err := rig.Beads.Show(ctx, id)
		require.NoError(t, err)
		sha, ok := issues[0].Metadata.String(beads.MetaMergeSHA)
		require.Truef(t, ok, "%s has no %s metadata", id, beads.MetaMergeSHA)
		assert.Containsf(t, rig.Repo.Git(t, "log", "--format=%H"), sha, "%s: %s is on the default branch", id, sha)
	}

	// P9-23: the same argv judgement as safety on, with the fast-forward
	// rule in force instead — and the run really did merge, so the rule
	// had something to judge.
	git := rig.judgedGit(t)
	assert.GreaterOrEqual(t, gitguard.Count(git, "worktree"), 2, "a worktree per bead (git calls: %d)", len(git))
	assert.Positive(t, gitguard.Count(git, "merge"), "safety off merges into the default branch")
	assert.Positive(t, gitguard.Count(git, "push"), "and pushes it")

	e2eAssertEvents(t, rig, false)
	e2eAssertStateFile(t, rig, out)
}

// e2eAssertBothBeadsClosed checks what is true of every finished scenario,
// interrupted or not: the plan's epic and its two beads, the dependency bd
// holds between them, both beads closed after one approving review round,
// the worktrees pruned and the ammo cap respected. It returns the two
// beads, in plan order.
func e2eAssertBothBeadsClosed(ctx context.Context, t *testing.T, rig *e2eRig) (first, second domain.BeadID) {
	t.Helper()

	// The plan created the run epic and its two work beads (D-22).
	require.NotEmpty(t, rig.Run.Epic(), "the run has an epic")
	titles := rig.Titles(ctx)
	require.Len(t, titles, 2, "the plan created two work beads (got %v)", titles)
	first, ok := titles["Add a notes file"]
	require.True(t, ok, "the first bead exists (%v)", titles)
	second, ok = titles["Extend the notes"]
	require.True(t, ok, "the second bead exists (%v)", titles)

	// The dependency the plan asked for is the one bd holds (D-24).
	deps, err := rig.Beads.DepList(ctx, second)
	require.NoError(t, err)
	require.Len(t, deps, 1, "the second bead has one blocker")
	assert.Equal(t, first, deps[0].DependsOnID, "the second bead is blocked by the first (D-20)")

	// Both beads are closed, whichever path they took.
	assert.Equal(t, map[domain.BeadID]domain.BeadState{
		first:  domain.BeadStateClosed,
		second: domain.BeadStateClosed,
	}, rig.States(ctx))

	// One review round each, all three lenses, all approving.
	for _, id := range []domain.BeadID{first, second} {
		th, ok := rig.Feedback.Thread(id)
		require.Truef(t, ok, "the feedback loop tracked %s", id)
		assert.Equalf(t, 1, th.Round, "%s took one review round", id)
		assert.ElementsMatchf(t, domain.Lenses(), th.Approved, "%s: every lens approved", id)
		assert.Emptyf(t, th.Items, "%s: nothing was raised", id)
	}
	assert.Equal(t, 0, rig.Feedback.Outstanding(), "the feedback loop is finished with every bead")

	// P6-18: the worktrees of the closed beads were pruned when the run
	// finished, and the run directory itself is still there.
	for _, id := range []domain.BeadID{first, second} {
		assert.NoDirExistsf(t, rig.Manager.PathFor(id), "the worktree of the closed bead %s was pruned", id)
	}
	assert.DirExists(t, rig.RC.Run.StateDir)

	// The ammo cap was never exceeded at any point of the run (D-05).
	caps := CapsFor(rig.RC.Allocation)
	assert.Equal(t, 4, caps.Ammo)
	peak, byRole := rig.Peak()
	assert.LessOrEqualf(t, peak, caps.Ammo, "at most %d agents ran at once (peak %d, by role %v)", caps.Ammo, peak, byRole)
	for role, n := range byRole {
		assert.LessOrEqualf(t, n, caps.For(role), "at most %d %s agents ran at once (peak %d)", caps.For(role), role, n)
	}
	assert.Zero(t, rig.Sched.Status().Live, "no agent is left running")
	return first, second
}

// e2eAssertBothBeadsShipped checks the plan, the dependency, the review
// rounds and the final bead states, which are the same whichever delivery
// path the run took.
func e2eAssertBothBeadsShipped(ctx context.Context, t *testing.T, rig *e2eRig) {
	t.Helper()
	first, second := e2eAssertBothBeadsClosed(ctx, t, rig)

	// The agents that ran: one product owner, one engineer per bead and
	// three testers per bead, and nothing else (the mock's fallback rule
	// crashes an unscripted firing, so a wrong prompt would fail here).
	assert.Equal(t, 1, rig.Adapter.Uses("plan"), "one planning firing")
	assert.Equal(t, 1, rig.Adapter.Uses("first engineer"))
	assert.Equal(t, 1, rig.Adapter.Uses("second engineer"))
	assert.Equal(t, 6, rig.Adapter.Uses("tester"), "three lenses per bead")
	assert.Equal(t, 0, rig.Adapter.Uses("unscripted"), "every firing was scripted")

	// D-06: the report and the three verdicts of each bead are on the bead
	// as structured comments.
	for _, id := range []domain.BeadID{first, second} {
		msgs, err := rig.Beads.Messages(ctx, id)
		require.NoError(t, err)
		var kinds []domain.MessageType
		for _, m := range msgs {
			kinds = append(kinds, m.Type)
		}
		assert.Equalf(t, []domain.MessageType{
			domain.MessageImplReport, domain.MessageVerdict, domain.MessageVerdict, domain.MessageVerdict,
		}, kinds, "%s: the bead carries its report and verdicts", id)
	}

	// The prompts were built from the real repository and the real diff:
	// the engineer was told what the plan asked for, and every tester saw
	// the file that engineer added.
	engineers := rig.Adapter.CallsMatching(mock.Match{Role: string(domain.RoleEngineer)})
	require.Len(t, engineers, 2)
	for _, c := range engineers {
		assert.Contains(t, c.Spec.Prompt, "notes", "the engineer prompt carries the bead")
	}
	testers := rig.Adapter.CallsMatching(mock.Match{Role: string(domain.RoleTester)})
	require.Len(t, testers, 6)
	for _, c := range testers {
		bead := c.Spec.Labels.Get(cartridge.LabelBead)
		want := e2eWorkFiles[0]
		if bead == string(second) {
			want = e2eWorkFiles[1]
		}
		assert.Containsf(t, c.Spec.Prompt, want,
			"the %s tester of %s reviews the diff that added %s", c.Spec.Labels.Get(cartridge.LabelLens), bead, want)
	}

	// The bd calls the run made: one dry run and one atomic create for the
	// plan (D-22), and one atomic claim per bead (D-07, P6-13).
	calls := rig.BDCalls()
	assert.Equal(t, 1, calls["create-graph-dry-run"], "the plan was dry run first (D-22)")
	assert.Equal(t, 1, calls["create-graph"], "the plan was created in one atomic call (D-22)")
	assert.Equal(t, 2, rig.BDClaims(), "each bead was claimed once with bd's atomic claim")
}

// e2eAssertEvents checks the run log: the lifecycle brackets, the bead
// flow of D-07 and the delivery events the path implies.
func e2eAssertEvents(t *testing.T, rig *e2eRig, safety bool) {
	t.Helper()
	evs, err := ReadEventLog(rig.RC.Run.StateDir)
	require.NoError(t, err)
	require.NotEmpty(t, evs)

	kinds := make([]events.Kind, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind())
	}
	assert.Equal(t, events.KindRunStarted, kinds[0], "the log opens with RunStarted")
	assert.Equal(t, events.KindRunFinished, kinds[len(kinds)-1], "the log closes with RunFinished")

	summary := Summarise(evs, -1)
	assert.Equal(t, rig.RC.Run.ID, summary.Run)
	assert.Equal(t, rig.Run.Epic(), summary.Epic)
	assert.Equal(t, PhaseFinished, summary.Phase)
	assert.Equal(t, events.FinishCompleted, summary.Finish)
	assert.Equal(t, 1, summary.Plans, "one plan was produced")
	assert.Equal(t, 2, summary.Rounds, "one review round per bead")
	// Nine: the planning product owner goes through the supervisor too
	// (SupervisorPlanRunner), so it is counted like every other agent in
	// `sc status` and on the TUI's Agents tab.
	assert.Equal(t, 9, summary.Fired, "one product owner, two engineers and six testers")
	assert.Equal(t, 9, summary.Completed, "every one of them finished")
	assert.Equal(t, 0, summary.Failed)
	assert.Equal(t, 2, summary.Settled(), "both beads settled")
	assert.Zero(t, summary.Errors, "the run published no Error event")

	if safety {
		assert.Equal(t, 2, summary.PRs, "one PROpened per bead")
	} else {
		assert.Zero(t, summary.PRs, "safety off opens no pull request")
	}
	assert.Equal(t, 2, summary.Merges, "both beads landed")

	// Every bead walked the D-07 flow, in order, and ended closed.
	for id, flow := range e2eBeadFlows(evs) {
		assert.Equalf(t, e2eExpectedFlow(), flow, "%s walked the D-07 flow", id)
	}
}

// e2eExpectedFlow is the D-07 sequence a bead of this scenario walks, as
// the *event stream* records it.
//
// The `open → in_progress` edge is published by the scheduler itself
// (Scheduler.announceClaim): bd's atomic claim writes the status, so the
// move does not go through the state machine (P6-02, P6-13) and the
// scheduler announces it from the demand's own From state. Without it a
// bead appeared to jump from open straight to in_review in the run log,
// `sc status` and the TUI graph.
func e2eExpectedFlow() []string {
	return []string{
		"open→in_progress",
		"in_progress→in_review",
		"in_review→approved",
		"approved→delivering",
		"delivering→closed",
	}
}

// e2eBeadFlows folds the BeadStateChanged events into one "from→to" list
// per bead, in publication order.
func e2eBeadFlows(evs []events.Event) map[domain.BeadID][]string {
	out := map[domain.BeadID][]string{}
	for _, ev := range evs {
		c, ok := ev.(events.BeadStateChanged)
		if !ok {
			continue
		}
		out[c.Bead] = append(out[c.Bead], fmt.Sprintf("%s→%s", c.From, c.To))
	}
	return out
}

// e2eAssertStateFile checks that state.json agrees with the run that just
// finished: the phase, the outcome, the epic and the event count.
func e2eAssertStateFile(t *testing.T, rig *e2eRig, out Outcome) {
	t.Helper()
	st, err := LoadState(rig.RC.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, StateFileVersion, st.Version)
	assert.Equal(t, rig.RC.Run.ID, st.Run.ID)
	assert.Equal(t, rig.RC.Run.RepoPath, st.Run.RepoPath)
	assert.Equal(t, domain.RunModeBrief, st.Run.Mode)
	assert.Equal(t, e2eBrief, st.Run.Brief)
	assert.Equal(t, rig.Run.Epic(), st.Run.Epic, "state.json carries the run's epic")
	assert.Equal(t, rig.Safety, st.Run.Config.Safety, "the frozen snapshot keeps the safety setting")
	assert.Equal(t, 4, st.Run.Config.Ammo)
	assert.Equal(t, PhaseFinished, st.Phase)
	assert.Equal(t, out.Reason, st.Finish)
	assert.Equal(t, out.Summary, st.FinishSummary)
	assert.Empty(t, st.FinishError)
	assert.Empty(t, st.Agents, "no agent is left running")
	assert.False(t, st.FinishedAt.IsZero())
	assert.Zero(t, st.Resumes)

	// The sequence number in state.json is the run's own event count, and
	// the run log holds exactly that many lines.
	evs, err := ReadEventLog(rig.RC.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, uint64(len(evs)), st.LastSeq, "state.json and the run log agree on the event count")
	assert.Equal(t, evs[len(evs)-1].Kind(), st.LastEventKind)
}

// e2eLazy renders a rig's diagnosis only when a failing assertion asks for
// it. Go evaluates a call's arguments before the call, so passing
// rig.Problems() to require.Eventually reports the problems the run had
// *before* the wait, which is always none.
type e2eLazy struct{ rig *e2eRig }

// String implements fmt.Stringer.
func (l e2eLazy) String() string { return l.rig.diagnose() }

// BDCalls counts the bd calls the run made, keyed the way testutil derives
// a bd sample name ("ready", "gate-check", "create-graph-dry-run", …).
func (r *e2eRig) BDCalls() map[string]int {
	r.T.Helper()
	out := map[string]int{}
	for _, args := range r.BD.ArgLists(r.T) {
		out[testutil.BDKey(args)]++
	}
	return out
}

// BDClaims counts the `bd update --claim` calls, which is how many times a
// bead was taken atomically (P6-13).
func (r *e2eRig) BDClaims() int {
	r.T.Helper()
	n := 0
	for _, args := range r.BD.ArgLists(r.T) {
		for _, a := range args {
			if a == "--claim" {
				n++
				break
			}
		}
	}
	return n
}

// e2eRemoteHead returns the default branch tip in the bare origin.
func e2eRemoteHead(t *testing.T, rig *e2eRig) string {
	t.Helper()
	out := rig.Repo.Git(t, "ls-remote", rig.Repo.RemoteDir, "refs/heads/"+rig.Repo.DefaultBranch)
	fields := strings.Fields(out)
	require.NotEmpty(t, fields, "the remote has a default branch")
	return fields[0]
}

// ---------------------------------------------------------------------------
// Harness self-checks
// ---------------------------------------------------------------------------

// TestE2EFakeBDBehavesLikeBD pins the behaviour the scenario relies on the
// stateful bd shim for: the D-07 custom statuses, an atomic graph plan with
// its edges, the atomic claim, structured bead comments, and the D-20 rule
// that an open dependency or an open gate refuses a close and keeps a bead
// out of the ready set. A change here fails loudly instead of turning the
// end-to-end run into a mystery timeout.
func TestE2EFakeBDBehavesLikeBD(t *testing.T) {
	e2eFakeBD(t)
	ctx := context.Background()
	client := beads.New(t.TempDir(), beads.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	statuses, err := client.EnsureStatuses(ctx)
	require.NoError(t, err)
	for _, s := range domain.WorkflowBeadStates() {
		assert.Truef(t, statuses.Statuses.Has(string(s)), "bd knows the %s status (D-07)", s)
	}

	plan, err := client.ApplyPlan(ctx, beads.PlanRequest{
		Run: "run-1",
		Plan: domain.Plan{Brief: "two beads", Beads: []domain.PlanBead{
			{Key: "a", Title: "First", Acceptance: "done", Priority: domain.PriorityDefault},
			{Key: "b", Title: "Second", Acceptance: "done", Priority: domain.PriorityDefault, Deps: []string{"a"}},
		}},
		Roles: []domain.Role{domain.RolePO},
	}, false)
	require.NoError(t, err)
	require.Len(t, plan.Beads, 2)
	first, second := plan.Beads[0], plan.Beads[1]

	ready, err := client.Ready(ctx, plan.Epic)
	require.NoError(t, err)
	require.Len(t, ready, 1, "an open blocker keeps the second bead out of the ready set (D-20)")
	assert.Equal(t, first, ready[0].ID)

	claimed, err := client.ClaimFor(ctx, first, "eng-1")
	require.NoError(t, err)
	assert.Equal(t, string(domain.BeadStateInProgress), claimed.Status)
	assert.Equal(t, beads.ActorFor("eng-1"), claimed.Assignee)
	_, err = client.ClaimFor(ctx, first, "eng-2")
	assert.Error(t, err, "a bead another agent holds is not claimable")

	_, err = client.PostMessage(ctx, first, beads.ActorFor("eng-1"),
		domain.ImplReport{Summary: "did it", FilesChanged: []string{"x.go"}, HowToVerify: "go test ./..."})
	require.NoError(t, err)
	msgs, err := client.Messages(ctx, first)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, domain.MessageImplReport, msgs[0].Type)

	gate, err := client.CreatePRGate(ctx, first, 7, "PR #7")
	require.NoError(t, err)
	assert.Equal(t, first, gate.Blocks, "the gate names the bead it blocks")
	_, err = client.Close(ctx, first, "merged")
	assert.Error(t, err, "an open gh:pr gate refuses the close (D-20)")

	require.NoError(t, client.ResolveGate(ctx, gate.ID, "merged by hand"))
	_, err = client.Close(ctx, first, "merged")
	require.NoError(t, err)
	ready, err = client.Ready(ctx, plan.Epic)
	require.NoError(t, err)
	require.Len(t, ready, 1, "closing the blocker releases the dependent")
	assert.Equal(t, second, ready[0].ID)

	list, err := client.ListRun(ctx, "run-1", true)
	require.NoError(t, err)
	assert.Len(t, list, 3, "the epic and its two children carry the run label")
}

// TestE2EKillAndResume is the Phase 6 kill-and-resume exit criterion: a run
// is killed the way a second Ctrl-C kills it — no grace, no lease release,
// no worktree cleanup — and a second invocation of the same stack picks the
// same run up and finishes it.
//
// It exercises, against the real beads.Client driving the fixture tracker
// (testdata/orchestrator/fakebd, the stand-in this file's header
// describes; the real bd 1.2.2 is covered by the tagged integration
// suite), real git and the mock cartridge: RestoreRunConfig putting the
// frozen snapshot back
// (P6-11); beads.RecoverStale returning the killed process's claims to the
// ready set (P3-05); gitx.Resumer reattaching the worktrees and
// reconciling branch state into bead state, with the run acting on what it
// found (P6-15); the planner not planning the brief a second time; and the
// run finishing with both beads shipped exactly as the uninterrupted run
// ships them.
func TestE2EKillAndResume(t *testing.T) {
	ctx := context.Background()
	env := e2eNewEnv(t)

	// --- the run that gets killed ---------------------------------------
	first := e2eBuild(t, env, false, false)
	first.start(ctx)

	// Kill it once a bead is really in flight: the plan has landed, an
	// engineer holds a lease and its worktree exists.
	var held domain.BeadID
	require.Eventually(t, func() bool {
		list, err := first.beadSource(ctx)
		if err != nil {
			return false
		}
		for _, b := range list {
			if b.Type == domain.BeadTypeEpic {
				continue
			}
			if b.State != domain.BeadStateInProgress || b.Assignee == "" {
				continue
			}
			// Wait for the worktree too, so the resume has something to
			// reattach (P6-15).
			if _, serr := os.Stat(first.Manager.PathFor(b.ID)); serr != nil {
				continue
			}
			held = b.ID
			return true
		}
		return false
		// require.Eventually's message arguments are evaluated at the call,
		// before the wait has begun, so the diagnosis has to be taken here
		// rather than passed in — it was always "problems: []" otherwise.
	}, e2eTimeout, 10*time.Millisecond, "a bead to be claimed by an engineer; %s", e2eLazy{first})

	require.NoError(t, first.Run.Kill("SIGINT (twice)"))
	out, err := first.wait(e2eTimeout)
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason, "a killed run stops")
	first.stopHelpers()

	// A kill releases nothing: that is what the resume has to repair.
	killed := first.States(ctx)
	assert.Equal(t, domain.BeadStateInProgress, killed[held], "the killed run left %s claimed", held)
	assert.DirExists(t, first.Manager.PathFor(held), "the killed run kept its worktree")
	before, err := LoadState(first.RC.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, PhaseFinished, before.Phase)
	assert.Equal(t, 0, before.Resumes)

	// --- the run that resumes it ----------------------------------------
	second := e2eBuild(t, env, false, true)
	require.Equal(t, first.RC.Run.ID, second.RC.Run.ID, "the resume found the same run")
	assert.Equal(t, before.Run.Config.Ammo, second.RC.Run.Config.Ammo, "the frozen snapshot came back (P6-11)")
	assert.Equal(t, e2eBrief, second.RC.Run.Brief, "so did the brief")
	assert.Equal(t, first.Run.Epic(), second.Run.Epic(), "and the run's epic (D-22)")
	assert.Contains(t, second.Recovered.IDs(), held, "the stale lease on %s was recovered (P3-05)", held)

	second.start(ctx)
	out2, err := second.wait(e2eTimeout)
	require.NoError(t, err, "%s", e2eLazy{second})
	assert.Empty(t, second.Problems(), "the conductor hit no problem")
	assert.Equal(t, events.FinishCompleted, out2.Reason)

	// The resumed run planned nothing: it inherited the epic and its beads.
	assert.Equal(t, 1, second.Planner.Status().Plans, "the brief is planned once across both runs")
	assert.True(t, second.Planner.Status().ResumedPlan)
	assert.Zero(t, second.Adapter.Uses("plan"), "the resumed run fired no product owner")
	assert.Equal(t, 1, second.BDCalls()["create-graph"], "one plan was created, by the first run (D-22)")

	// It closed both beads and merged them, exactly as an uninterrupted
	// safety-off run does.
	e2eAssertBothBeadsClosed(ctx, t, second)
	for _, o := range second.Outcomes() {
		assert.Equal(t, DeliveryMerged, o.Kind, "safety off merges")
	}
	for _, f := range e2eWorkFiles {
		assert.FileExistsf(t, filepath.Join(second.Repo.Dir, f), "%s landed on the default branch", f)
	}
	assert.Equal(t, second.Repo.Head(t), e2eRemoteHead(t, second), "the default branch was pushed")

	// state.json counted the resume and the run log is one file across both
	// invocations (P6-01, P6-08).
	after, err := LoadState(second.RC.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, 1, after.Resumes)
	assert.Equal(t, PhaseFinished, after.Phase)
	assert.Equal(t, events.FinishCompleted, after.Finish)
	assert.Empty(t, after.Agents, "no agent survived the run")
	evs, err := ReadEventLog(second.RC.Run.StateDir)
	require.NoError(t, err)
	var started int
	for _, ev := range evs {
		if rs, ok := ev.(events.RunStarted); ok {
			started++
			if started == 2 {
				assert.True(t, rs.Resumed, "the second RunStarted says it is a resume")
			}
		}
	}
	assert.Equal(t, 2, started, "both invocations appended to the same run log")
}
