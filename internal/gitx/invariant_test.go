package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// P9-02 — safety invariant tests.
//
// Almost everything a run does to the target repository goes through git,
// so the guarantee "the cannon cannot damage the target repo" is largely a
// statement about the command lines a run produces. These tests run the
// real deliverers over a real fixture repository with a bare origin behind
// a git shim that records every invocation and then execs the real git
// (cuGit), so the scenario happens for real while the exact argv is kept
// for judgement. Safety on records gh as well, because a pull request can
// land work on the default branch with no git call at all (D-11), so
// "damage in git terms" alone would miss it.
//
// What is watched, exactly: every git and gh invocation made by anything
// running with the shims on PATH. That is the deliverers and the worktree
// manager, and it is also the shell git an engineer session runs in its
// own worktree — the "agent shell git" event below and
// [TestAnAgentsOwnGitIsCaughtByTheSameChecker] put agent-issued commands
// through the same checker, because the engineer's tool policy is
// read/write (internal/agents maps events.TaskImplement,
// TaskAddressFeedback and TaskResolveConflict to cartridge.ToolReadWrite,
// which the Claude cartridge enforces with Bash allowed and no deny list),
// so the only production guard on what an engineer does with git is a
// sentence in its prompt. What is not watched: the production run path
// around delivery — branch cleanup, resume, the delivery settler and the
// external pull request reviewer — which is covered from the other side by
// internal/orchestrator's end-to-end tests asserting that neither default
// branch tip moves; running that path under this shim is a discovered task
// rather than part of P9-02.
//
// Two invariants are asserted, one per safety mode:
//
//   - safety on (D-06's default, [PRDeliverer]): no invocation ever checks
//     out, merges or pushes the default branch, and neither the local nor
//     the remote default branch moves at all. Work reaches the human only
//     as a pushed sc/ branch and a pull request.
//   - safety off ([Manager.DeliverMerge]): the default branch is only ever
//     moved by a fast-forward — every merge carries --ff-only, no ref update
//     of the default branch is forced, no merge commit lands on it, and it
//     never moves backwards.
//
// A handful of rules hold in both modes, because they are damage rather
// than delivery: the default branch is never deleted, renamed or checked
// out, "git pull" is never run, nothing that rewrites the branch it runs
// on (commit, reset, revert, cherry-pick, rebase …) is ever run in the
// main working tree, where the default branch is the one checked out, and
// nothing that discards what is in that tree (clean, checkout of paths,
// restore, stash) is run there either — the user's uncommitted work is
// theirs. A file the test leaves untracked in the main working tree stands
// in for it and is checked after every step, so the repository half of the
// invariant sees that damage too and not only moved refs.
//
// Both are checked across the P6-09 scenario re-created here in git terms
// (two beads, one with a dependency on the other, an engineer commit and a
// review round each, a pull that lands upstream in between, then delivery)
// and across randomised event sequences, where an event picks a bead and
// does one of the things a run does to a worktree — commit, commit
// leftovers, run git as the agent would, deliver, resolve a conflict, take
// an upstream change — in an order no scenario test would think of.
//
// The invariants are asserted from the recorded argv, so a future change
// that reaches for "git checkout main", "git push --force origin main" or
// "gh pr merge" fails here even when the fixture happens to end in a
// healthy state.

// ivSafety names the delivery mode a scenario runs in (D-06).
type ivSafety string

const (
	// ivSafetyOn delivers through a pull request and never writes to the
	// default branch.
	ivSafetyOn ivSafety = "on"
	// ivSafetyOff merges into the default branch, fast-forward only.
	ivSafetyOff ivSafety = "off"
)

// ivEnv is what judging one recorded git invocation needs: which branch is
// the default one, which remote it lives on, where the main working tree
// is, and which safety mode was in force.
type ivEnv struct {
	safety  ivSafety
	def     string // the default branch, "main" in the fixture
	remote  string // the default remote, "origin"
	mainDir string // the main working tree, with symlinks resolved
}

// ivCall is one recorded git invocation split into the subcommand and its
// arguments, with git's own global options (-c, -C, --git-dir …) removed.
type ivCall struct {
	seq  int
	dir  string   // the directory git ran in, with symlinks resolved
	args []string // the full argument list, for reporting
	sub  string   // the subcommand, "" when the call had none
	rest []string // everything after the subcommand
}

// String renders the call the way a reader would have typed it.
func (c ivCall) String() string {
	return fmt.Sprintf("#%d git %s (in %s)", c.seq, strings.Join(c.args, " "), c.dir)
}

// ivGlobalValueOpts are git's global options that take their value as the
// next argument, so the subcommand scanner can step over both.
var ivGlobalValueOpts = map[string]bool{
	"-c": true, "-C": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true, "--config-env": true,
}

// ivValueFlags are the subcommand flags whose value is a separate argument
// and is therefore not an operand.
var ivValueFlags = map[string]bool{
	"-b": true, "-B": true, "-m": true, "--message": true, "--onto": true,
	"-s": true, "--strategy": true, "-X": true, "--strategy-option": true,
	"-o": true, "--push-option": true, "--repo": true, "--exec": true,
	"--depth": true, "-C": true, "-c": true, "--reuse-message": true,
	"--upstream": true,
}

// ivSplit separates git's global options from the subcommand and its own
// arguments. "git -c merge.ff=only merge --ff-only x" splits into "merge"
// and ["--ff-only", "x"].
func ivSplit(args []string) (sub string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
		name, _, hasValue := strings.Cut(a, "=")
		if ivGlobalValueOpts[name] && !hasValue {
			i++
		}
	}
	return "", nil
}

// ivOperands returns the non-flag arguments of a subcommand. With
// stopAtEndOfOptions the scan stops at "--" (checkout, where what follows
// are paths); otherwise "--" is skipped and what follows is taken as
// operands (push, where the remote and the refspecs follow it).
func ivOperands(rest []string, stopAtEndOfOptions bool) []string {
	var out []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			if stopAtEndOfOptions {
				return out
			}
			return append(out, rest[i+1:]...)
		case strings.HasPrefix(a, "-") && a != "-":
			name, _, hasValue := strings.Cut(a, "=")
			if ivValueFlags[name] && !hasValue {
				i++
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

// ivFlagValue returns the value of the first of names present in rest,
// accepting both "--flag value" and "--flag=value".
func ivFlagValue(rest []string, names ...string) (string, bool) {
	for i, a := range rest {
		name, value, hasValue := strings.Cut(a, "=")
		for _, want := range names {
			if name != want {
				continue
			}
			if hasValue {
				return value, true
			}
			if i+1 < len(rest) {
				return rest[i+1], true
			}
		}
	}
	return "", false
}

// ivHasFlag reports whether rest contains any of names, in either the
// "--flag" or the "--flag=value" spelling.
func ivHasFlag(rest []string, names ...string) bool {
	for _, a := range rest {
		name, _, _ := strings.Cut(a, "=")
		for _, want := range names {
			if name == want {
				return true
			}
		}
	}
	return false
}

// ivSpec is one refspec operand of a push or a fetch.
type ivSpec struct {
	src   string
	dst   string
	force bool // the refspec was written with a leading "+"
}

// ivRefspecs returns the refspecs of a push or fetch. The first operand is
// the remote (a name, a URL, or "." for the repository itself) and every
// operand after it is a refspec; a refspec without a colon pushes or
// fetches the same name on both sides.
func ivRefspecs(rest []string) []ivSpec {
	ops := ivOperands(rest, false)
	if len(ops) < 2 {
		return nil
	}
	specs := make([]ivSpec, 0, len(ops)-1)
	for _, op := range ops[1:] {
		s := ivSpec{}
		if strings.HasPrefix(op, "+") {
			s.force, op = true, op[1:]
		}
		if src, dst, ok := strings.Cut(op, ":"); ok {
			s.src, s.dst = src, dst
		} else {
			s.src, s.dst = op, op
		}
		specs = append(specs, s)
	}
	return specs
}

// ivIsDefaultRef reports whether ref names the default branch, in any of
// the spellings git accepts: "main", "refs/heads/main", "origin/main",
// "refs/remotes/origin/main", and the same with a trailing "^{commit}".
func ivIsDefaultRef(ref string, env ivEnv) bool {
	ref = strings.TrimSuffix(strings.TrimPrefix(ref, "+"), "^{commit}")
	if ref == "" {
		return false
	}
	names := []string{env.def, "refs/heads/" + env.def}
	if env.remote != "" {
		names = append(names, env.remote+"/"+env.def, "refs/remotes/"+env.remote+"/"+env.def)
	}
	for _, n := range names {
		if ref == n {
			return true
		}
	}
	return false
}

// ivBranchOperands returns the branch names of a "git branch" call. No
// flag is treated as taking a separate value here, because -m and -M take
// branch names rather than values; over-reporting a rename of the default
// branch is the safe direction for an invariant.
func ivBranchOperands(rest []string) []string {
	var out []string
	for _, a := range rest {
		if a == "--" || strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ivIsTrackingRef reports whether ref is the remote-tracking ref of the
// default branch rather than the branch itself. Writing it is what every
// fetch does; writing refs/heads/<default> is not.
func ivIsTrackingRef(ref string, env ivEnv) bool {
	return strings.HasPrefix(ref, "refs/remotes/") || (env.remote != "" && ref == env.remote+"/"+env.def)
}

// ivWorktreeWriters are the subcommands that rewrite the branch checked out
// where they run, so running one in the main working tree moves the default
// branch. A delivery only ever runs them in a bead's worktree.
var ivWorktreeWriters = map[string]bool{
	"commit": true, "reset": true, "revert": true,
	"cherry-pick": true, "am": true, "apply": true, "stash": true,
	"filter-branch": true,
}

// ivTreeDestroyers are the subcommands that throw away what is in the
// working tree they run in without touching a ref: "clean" deletes
// untracked files, "checkout"/"switch" with paths and "restore" overwrite
// modified ones. In a bead's worktree that is the agent tidying up after
// itself; in the main working tree it is the user's uncommitted work, and
// no part of a run has any business there — nothing in gitx runs any of
// them, in either mode.
var ivTreeDestroyers = map[string]bool{
	"clean": true, "restore": true, "checkout": true, "switch": true,
}

// ivInspect judges one recorded invocation and returns a reason for every
// invariant it breaks (usually none).
func ivInspect(c ivCall, env ivEnv) []string {
	var out []string
	flag := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	inMain := c.dir != "" && c.dir == env.mainDir

	switch c.sub {
	case "merge":
		switch env.safety {
		case ivSafetyOn:
			flag("safety on merges nothing: %s", c)
		default:
			if !ivHasFlag(c.rest, "--ff-only") {
				flag("a merge without --ff-only can put a merge commit on %s: %s", env.def, c)
			}
			if ivHasFlag(c.rest, "--no-ff", "--squash", "--no-commit", "-m", "--message") {
				flag("a merge that is not a plain fast-forward: %s", c)
			}
		}
	case "pull":
		flag("git pull merges into the checked-out branch: %s", c)
	case "checkout", "switch":
		if name, ok := ivFlagValue(c.rest, "-b", "-B", "-c", "-C"); ok && ivIsDefaultRef(name, env) {
			flag("creating or resetting %s: %s", env.def, c)
		}
		for _, ref := range ivOperands(c.rest, true) {
			if ivIsDefaultRef(ref, env) {
				flag("checking out %s: %s", env.def, c)
			}
		}
	case "worktree":
		ops := ivOperands(c.rest, false)
		if len(ops) == 0 || ops[0] != "add" {
			break
		}
		// "worktree add <path> -b <branch> <base>" checks out the new
		// branch; naming the default branch as the base is fine, checking
		// it out is not.
		if name, ok := ivFlagValue(c.rest, "-b", "-B"); ok {
			if ivIsDefaultRef(name, env) {
				flag("a worktree on %s: %s", env.def, c)
			}
			break
		}
		if len(ops) >= 3 && ivIsDefaultRef(ops[2], env) {
			flag("a worktree checking out %s: %s", env.def, c)
		}
	case "branch":
		if !ivHasFlag(c.rest, "-D", "-d", "--delete", "-M", "-m", "--move", "-f", "--force") {
			break
		}
		for _, ref := range ivBranchOperands(c.rest) {
			if ivIsDefaultRef(ref, env) {
				flag("deleting or moving %s: %s", env.def, c)
			}
		}
	case "update-ref", "symbolic-ref":
		for _, ref := range ivOperands(c.rest, true) {
			if ivIsDefaultRef(ref, env) {
				flag("writing the ref of %s: %s", env.def, c)
			}
		}
	case "push":
		if ivHasFlag(c.rest, "--mirror", "--all") {
			flag("a push that carries every ref, %s included: %s", env.def, c)
		}
		forced := ivHasFlag(c.rest, "-f", "--force", "--force-with-lease")
		specs := ivRefspecs(c.rest)
		if len(specs) == 0 && inMain {
			// "git push" and "git push <remote>" carry no refspec, so git
			// pushes the branch that is checked out where the command
			// runs (push.default = simple or current). In the main working
			// tree that is the default branch, spelled nowhere on the
			// command line.
			specs = []ivSpec{{src: env.def, dst: env.def}}
		}
		for _, s := range specs {
			if !ivIsDefaultRef(s.dst, env) {
				continue
			}
			switch {
			case env.safety == ivSafetyOn:
				flag("pushing %s: %s", env.def, c)
			case forced || s.force || ivHasFlag(c.rest, "--delete", "-d"):
				flag("a forced or deleting push of %s is not a fast-forward: %s", env.def, c)
			}
		}
	case "fetch":
		// A fetch without a refspec only updates remote-tracking refs; one
		// with a refspec can write refs/heads/<default> in place, which is
		// how a fast-forward is applied when the default branch is not
		// checked out.
		forced := ivHasFlag(c.rest, "-f", "--force")
		for _, s := range ivRefspecs(c.rest) {
			if !ivIsDefaultRef(s.dst, env) || ivIsTrackingRef(s.dst, env) {
				continue
			}
			switch {
			case env.safety == ivSafetyOn:
				flag("updating %s in place: %s", env.def, c)
			case forced || s.force:
				flag("a forced update of %s is not a fast-forward: %s", env.def, c)
			}
		}
	case "rebase":
		if inMain {
			flag("a rebase in the main working tree rewrites %s: %s", env.def, c)
		}
		if ops := ivOperands(c.rest, true); len(ops) >= 2 && ivIsDefaultRef(ops[1], env) {
			flag("rebasing %s itself: %s", env.def, c)
		}
	}

	if ivWorktreeWriters[c.sub] && inMain {
		flag("%s in the main working tree moves %s: %s", c.sub, env.def, c)
	}
	if ivTreeDestroyers[c.sub] && inMain {
		flag("%s in the main working tree discards the user's uncommitted work: %s", c.sub, c)
	}
	return out
}

// ivGHAllowed are the gh command lines a run is allowed to make: reading
// pull requests, opening one for a bead's branch, leaving a review on it,
// and making the label a pull request carries. Everything else is a
// violation by default — "gh pr merge" above all, which lands work on the
// default branch with no git call for the checker above to see and which
// D-11 forbids outright for a pull request sc did not open.
var ivGHAllowed = map[string]bool{
	"pr list": true, "pr view": true, "pr diff": true, "pr status": true,
	"pr create": true, "pr review": true,
	"label create": true, "label list": true,
	"auth status": true, "repo view": true,
}

// ivGHDamage names the gh commands worth reporting in their own words,
// because they are the ones that would break the invariant rather than
// merely being unrecognised.
var ivGHDamage = map[string]string{
	"pr merge":    "merges a pull request, landing work on the default branch with no git command for the checker above to see",
	"pr close":    "closes a pull request a human may still be reading",
	"repo delete": "deletes the repository",
	"repo rename": "renames the repository",
	"repo edit":   "can change the repository's default branch",
	"repo sync":   "can force the repository's branches to match another's",
}

// ivGHKey is the "<command> <subcommand>" key of a gh invocation, "" for a
// bare "gh" or one that only carries flags (gh --version, gh --help). A
// flag's separate value would be read as a word, so a command line like
// "gh --repo acme/x pr list" would key wrongly — nothing in gitx writes
// one, and the allow list errs towards reporting rather than passing.
func ivGHKey(args []string) string {
	var words []string
	for i := 0; i < len(args) && len(words) < 2; i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			continue
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// ivInspectGH judges one recorded gh invocation. gh is checked with an
// allow list rather than a deny list: git can be reasoned about verb by
// verb, but gh's surface is the whole GitHub API, so anything a delivery
// has never needed is reported and a reviewer decides.
func ivInspectGH(args []string) []string {
	key := ivGHKey(args)
	line := "gh " + strings.Join(args, " ")
	switch {
	case key == "" || ivGHAllowed[key]:
		return nil
	case ivGHDamage[key] != "":
		return []string{fmt.Sprintf("gh %s %s: %s", key, ivGHDamage[key], line)}
	default:
		return []string{fmt.Sprintf("gh %s is not one of the commands a delivery makes: %s", key, line)}
	}
}

// ivScanGH judges every gh invocation recorded so far.
func ivScanGH(t *testing.T, gh *testutil.Shim) []string {
	t.Helper()
	var bad []string
	for _, inv := range gh.Invocations(t) {
		bad = append(bad, ivInspectGH(inv.Args)...)
	}
	return bad
}

// ivParse reads the shim's log and splits every invocation into an ivCall.
func ivParse(t *testing.T, git *testutil.Shim) []ivCall {
	t.Helper()
	invs := git.Invocations(t)
	calls := make([]ivCall, 0, len(invs))
	for _, inv := range invs {
		sub, rest := ivSplit(inv.Args)
		calls = append(calls, ivCall{seq: inv.Seq, dir: ivResolvePath(inv.Dir), args: inv.Args, sub: sub, rest: rest})
	}
	return calls
}

// ivResolvePath resolves symlinks so a temporary directory reached through
// one still compares equal to the path git printed.
func ivResolvePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// ivScan judges every call and returns one reason per violation.
func ivScan(calls []ivCall, env ivEnv) []string {
	var bad []string
	for _, c := range calls {
		bad = append(bad, ivInspect(c, env)...)
	}
	return bad
}

// ivAssertCommands checks every git invocation recorded so far against the
// mode's invariant and reports each violation with the command that broke
// it. It returns the calls it judged so a caller can also assert that the
// scenario really produced the commands it is supposed to.
func ivAssertCommands(t *testing.T, git *testutil.Shim, env ivEnv, when string) []ivCall {
	t.Helper()
	calls := ivParse(t, git)
	bad := ivScan(calls, env)
	assert.Empty(t, bad, "safety %s, %s: git was asked to do something a delivery may never do:\n%s",
		env.safety, when, strings.Join(bad, "\n"))
	return calls
}

// ivCount returns how many recorded calls ran the given subcommand.
func ivCount(calls []ivCall, sub string) int {
	n := 0
	for _, c := range calls {
		if c.sub == sub {
			n++
		}
	}
	return n
}

// ivStats counts what the deliveries in one sequence actually did, so a
// sequence of no-ops cannot pass for a tested one.
type ivStats struct {
	delivered  int // deliveries that returned no error
	conflicted int // deliveries that stopped on a rebase conflict (D-10)
	refused    int // deliveries refused for any other reason
}

// add folds another sequence's counts into these.
func (s *ivStats) add(o ivStats) {
	s.delivered += o.delivered
	s.conflicted += o.conflicted
	s.refused += o.refused
}

// String renders the counts for a failure message.
func (s ivStats) String() string {
	return fmt.Sprintf("%d delivered, %d conflicted, %d refused", s.delivered, s.conflicted, s.refused)
}

// ivFixture is a fixture repository with a bare origin, a recording git
// shim, a clone of the remote standing in for everyone else pushing to it,
// and (for safety on) a gh shim, a PR tracker and a deliverer.
type ivFixture struct {
	*wtFixture
	git     *testutil.Shim
	gh      *testutil.Shim
	tracker *dpTracker
	d       *PRDeliverer
	env     ivEnv
	beads   []domain.Bead
	clone   string // a working clone of the bare remote, for upstream commits

	mainSeen   string // the local default branch tip last asserted
	remoteSeen string // the remote default branch tip last asserted
	statusSeen string // "git status" in the main working tree, last asserted
	seen       int    // git invocations already returned by ivAssertStep
	stats      ivStats
}

// ivUserFile is a file the test leaves untracked in the main working tree
// and never mentions again: the user's own uncommitted work, which a run
// must leave exactly where it found it. Nothing a bead does writes to this
// path, so a merge can never legitimately collide with it.
const ivUserFile = "the-users-own-notes.txt"

// ivUserFileBody is what that file holds, so overwriting it is caught as
// well as deleting it.
const ivUserFileBody = "the user's own notes, uncommitted and untracked\n"

// ivBeads are the two beads of the P6-09 scenario: the second depends on
// the first, so it is delivered after the first has landed.
var ivBeads = []domain.Bead{
	{ID: "fx-i1", Title: "Fix the mean() bug", Type: domain.BeadTypeBug, Priority: 1},
	{ID: "fx-i2", Title: "Guard max() against an empty slice", Type: domain.BeadTypeBug, Priority: 2},
}

// ivSetup builds the fixture for one safety mode: a repository with a bare
// origin behind the recording git shim, a worktree for each bead, and the
// deliverer the mode uses. Safety on also gets the gh shim and a GitHub
// fetch URL so [PRDeliverer] takes its normal path while pushes still land
// in the local bare remote.
func ivSetup(t *testing.T, safety ivSafety) *ivFixture {
	t.Helper()
	ctx := context.Background()
	git := cuGit(t) // records every invocation, then execs the real git
	f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})

	iv := &ivFixture{
		wtFixture: f,
		git:       git,
		beads:     ivBeads,
		env: ivEnv{
			safety:  safety,
			def:     f.r.DefaultBranch,
			remote:  "origin",
			mainDir: ivResolvePath(f.repo.Root),
		},
	}
	iv.clone = ivClone(t, f.r)

	if safety == ivSafetyOn {
		iv.gh = dpGH(t)
		iv.tracker = &dpTracker{}
		d, err := NewPRDeliverer(f.m, iv.tracker, WithPRLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		require.NoError(t, err)
		iv.d = d
		// gh only ever sees a GitHub remote; pushes still go to the bare
		// repository through the push URL, so nothing reaches the network.
		f.r.Git(t, "remote", "set-url", "--push", "origin", f.r.RemoteDir)
		f.r.Git(t, "remote", "set-url", "origin", "https://github.com/acme/widgets.git")
	}

	for _, bead := range iv.beads {
		_, err := f.m.Create(ctx, bead)
		require.NoError(t, err)
	}
	// The worktrees exist, so the repository may go dirty now (Create
	// refuses a dirty one): leave the user's own file in the main working
	// tree and watch it for the rest of the scenario.
	require.NoError(t, os.WriteFile(filepath.Join(f.repo.Root, ivUserFile), []byte(ivUserFileBody), 0o644))
	iv.mainSeen, iv.remoteSeen = iv.tips(t)
	iv.statusSeen = iv.mainStatus(t)
	require.Contains(t, iv.statusSeen, ivUserFile, "the user's own file is what the main working tree holds")
	return iv
}

// ivClone makes a working clone of the bare remote with the fixture's
// isolated git environment. Commits pushed from it are what everyone else
// working on the repository does while the run is going.
func ivClone(t *testing.T, r *testutil.FixtureRepo) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "upstream")
	_, err := ivRun(t, r, filepath.Dir(dir), "clone", "--quiet", r.RemoteDir, dir)
	require.NoError(t, err)
	return dir
}

// ivRun runs the real git (never the shim) in dir with the fixture's
// isolated environment, so what a test does itself is never mistaken for
// what the code under test did.
func ivRun(t *testing.T, r *testutil.FixtureRepo, dir string, args ...string) (string, error) {
	t.Helper()
	bin, err := testutil.RealBinary("git")
	require.NoError(t, err)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = r.Env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ivMustRun is ivRun for a command that must succeed.
func ivMustRun(t *testing.T, r *testutil.FixtureRepo, dir string, args ...string) string {
	t.Helper()
	out, err := ivRun(t, r, dir, args...)
	require.NoError(t, err, "git %s in %s: %s", strings.Join(args, " "), dir, out)
	return out
}

// tips returns the local and remote default branch tips ("" when the ref
// does not exist).
func (f *ivFixture) tips(t *testing.T) (local, remote string) {
	t.Helper()
	local, _ = ivRun(t, f.r, f.r.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+f.env.def)
	remote, _ = ivRun(t, f.r, f.r.Dir, "--git-dir", f.r.RemoteDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+f.env.def)
	return local, remote
}

// isAncestor reports whether old is an ancestor of newer in the repository
// gitDir names (empty means the fixture's own).
func (f *ivFixture) isAncestor(t *testing.T, gitDir, old, newer string) bool {
	t.Helper()
	args := []string{}
	if gitDir != "" {
		args = append(args, "--git-dir", gitDir)
	}
	args = append(args, "merge-base", "--is-ancestor", old, newer)
	_, err := ivRun(t, f.r, f.r.Dir, args...)
	return err == nil
}

// mainStatus is "git status" in the main working tree, in the porcelain
// form and including untracked files, so both the user's own file and any
// change to a tracked one show up.
func (f *ivFixture) mainStatus(t *testing.T) string {
	t.Helper()
	return ivMustRun(t, f.r, f.r.Dir, "status", "--porcelain", "--untracked-files=all")
}

// ivRepoViolations returns a reason for everything the repository itself
// says a step should not have done: safety on leaves both default branch
// tips exactly where they were, safety off may only move them forward and
// may never put a merge commit on either, and in both modes the main
// working tree — the user's, not the run's — must be untouched, the user's
// own uncommitted file included.
//
// It updates the remembered state, so the next call compares against what
// this one saw, and it returns reasons rather than asserting so that
// [TestAnAgentsOwnGitIsCaughtByTheSameChecker] can prove this half fires on
// real damage.
func (f *ivFixture) ivRepoViolations(t *testing.T) []string {
	t.Helper()
	var bad []string
	flag := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }
	local, remote := f.tips(t)
	switch f.env.safety {
	case ivSafetyOn:
		if local != f.mainSeen {
			flag("the local %s moved (%s -> %s)", f.env.def, f.mainSeen, local)
		}
		if remote != f.remoteSeen {
			flag("%s/%s moved (%s -> %s)", f.env.remote, f.env.def, f.remoteSeen, remote)
		}
	default:
		if local != "" && f.mainSeen != "" && local != f.mainSeen && !f.isAncestor(t, "", f.mainSeen, local) {
			flag("the local %s moved off its own history (%s -> %s)", f.env.def, f.mainSeen, local)
		}
		if remote != "" && f.remoteSeen != "" && remote != f.remoteSeen && !f.isAncestor(t, f.r.RemoteDir, f.remoteSeen, remote) {
			flag("%s/%s moved off its own history (%s -> %s)", f.env.remote, f.env.def, f.remoteSeen, remote)
		}
	}
	if local != "" {
		if merges := ivMustRun(t, f.r, f.r.Dir, "log", "--merges", "--oneline", "refs/heads/"+f.env.def); merges != "" {
			flag("a merge commit reached the local %s (D-10): %s", f.env.def, merges)
		}
	}
	if remote != "" {
		if merges := ivMustRun(t, f.r, f.r.Dir, "--git-dir", f.r.RemoteDir, "log", "--merges", "--oneline", "refs/heads/"+f.env.def); merges != "" {
			flag("a merge commit reached %s/%s (D-10): %s", f.env.remote, f.env.def, merges)
		}
	}
	if status := f.mainStatus(t); status != f.statusSeen {
		flag("the main working tree changed under the user (%q -> %q)", f.statusSeen, status)
		f.statusSeen = status
	}
	switch body, err := os.ReadFile(filepath.Join(f.repo.Root, ivUserFile)); {
	case err != nil:
		flag("the user's own uncommitted file is gone: %v", err)
	case string(body) != ivUserFileBody:
		flag("the user's own uncommitted file was rewritten: %q", body)
	}
	f.mainSeen, f.remoteSeen = local, remote
	return bad
}

// ivAssertRepo asserts that the repository half of the invariant holds.
func (f *ivFixture) ivAssertRepo(t *testing.T, when string) {
	t.Helper()
	bad := f.ivRepoViolations(t)
	assert.Empty(t, bad, "safety %s, %s: the repository is not where it should be:\n%s",
		f.env.safety, when, strings.Join(bad, "\n"))
}

// ivAssert checks all three parts of the invariant — the git commands, the
// gh commands and the state the repository is in — and returns every git
// call recorded so far.
func (f *ivFixture) ivAssert(t *testing.T, when string) []ivCall {
	t.Helper()
	calls := ivAssertCommands(t, f.git, f.env, when)
	if f.gh != nil {
		bad := ivScanGH(t, f.gh)
		assert.Empty(t, bad, "safety %s, %s: gh was asked to do something a delivery may never do:\n%s",
			f.env.safety, when, strings.Join(bad, "\n"))
	}
	f.ivAssertRepo(t, when)
	return calls
}

// ivAssertStep is [ivFixture.ivAssert] for one step of a sequence: it
// returns only the git invocations that step produced, so a caller can tell
// an event that really exercised the code from one that did nothing at all.
func (f *ivFixture) ivAssertStep(t *testing.T, when string) []ivCall {
	t.Helper()
	all := f.ivAssert(t, when)
	fresh := all[min(f.seen, len(all)):]
	f.seen = len(all)
	return fresh
}

// ivWork writes a file in the bead's worktree and commits it, the way an
// engineer session ends. content chooses the file: the shared one when
// shared is true, so an upstream change conflicts with it.
func (f *ivFixture) ivWork(t *testing.T, bead domain.Bead, content string, shared bool) {
	t.Helper()
	path := f.m.PathFor(bead.ID)
	rel := fmt.Sprintf("stats/%s.md", bead.ID)
	if shared {
		rel = ivSharedFile
	}
	acWrite(t, path, rel, content)
	wtRepo, err := f.m.Open(context.Background(), bead.ID)
	if err != nil {
		return // the worktree is mid-rebase or gone; the delivery reports it
	}
	if _, err := wtRepo.CommitAll(context.Background(), "work on "+string(bead.ID)); err != nil {
		t.Logf("commit in %s: %v", bead.ID, err) // tolerated: mid-rebase, nothing staged
	}
}

// ivSharedFile is the file both the beads and the upstream clone edit, so
// a delivery hits a real rebase conflict.
const ivSharedFile = "stats/NOTES.md"

// ivUpstream commits in the clone and pushes to the bare remote, which is
// what a colleague merging their own work looks like from the run's side.
// The clone is refreshed first, so its own push is always a fast-forward
// and the remote's history stays the responsibility of this test alone.
func (f *ivFixture) ivUpstream(t *testing.T, content string, shared bool) {
	t.Helper()
	if _, err := ivRun(t, f.r, f.clone, "fetch", "--quiet", "origin"); err != nil {
		return
	}
	if _, err := ivRun(t, f.r, f.clone, "reset", "--hard", "--quiet", "origin/"+f.env.def); err != nil {
		return
	}
	rel := "upstream/" + content + ".md"
	if shared {
		rel = ivSharedFile
	}
	path := filepath.Join(f.clone, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("upstream: "+content+"\n"), 0o644))
	ivMustRun(t, f.r, f.clone, "add", "-A")
	ivMustRun(t, f.r, f.clone, "commit", "--quiet", "-m", "upstream "+content)
	ivMustRun(t, f.r, f.clone, "push", "--quiet", "origin", "HEAD:refs/heads/"+f.env.def)
	// The remote moved because this test moved it, which is a fast-forward
	// by construction; remember it so the next check judges the code under
	// test rather than the test's own colleague.
	_, f.remoteSeen = f.tips(t)
}

// ivDeliver runs the mode's delivery for one bead, counting the outcome.
// A randomised sequence is allowed to fail a delivery — it may have left
// nothing to deliver, or a conflict for the engineer — because the
// invariant is about what git was asked to do on the way, not about the
// delivery succeeding. It returns the error for a caller that wants it.
func (f *ivFixture) ivDeliver(t *testing.T, bead domain.Bead) error {
	t.Helper()
	ctx := context.Background()
	var err error
	if f.env.safety == ivSafetyOn {
		req := dpRequest()
		req.Bead = bead
		_, err = f.d.Deliver(ctx, req)
	} else {
		// "true" keeps the fixture's own go test out of the delivery;
		// P4-06's detection is covered by its own tests.
		_, err = f.m.DeliverMerge(ctx, bead, MergeOptions{CheckCommand: "true"})
	}
	switch {
	case err == nil:
		f.stats.delivered++
	case errors.Is(err, ErrRebaseConflict):
		f.stats.conflicted++
		t.Logf("delivering %s stopped on a conflict: %v", bead.ID, err)
	default:
		f.stats.refused++
		t.Logf("delivering %s: %v", bead.ID, err)
	}
	return err
}

// ivResolveConflict finishes what an engineer's conflict round does (D-10):
// it writes a resolution for every conflicted path and stages it, leaving
// the rebase for the next delivery attempt to continue.
func (f *ivFixture) ivResolveConflict(t *testing.T, bead domain.Bead) bool {
	t.Helper()
	wtRepo, err := f.m.Open(context.Background(), bead.ID)
	if err != nil || !wtRepo.RebaseInProgress() {
		return false
	}
	paths, err := wtRepo.ConflictedPaths(context.Background())
	if err != nil || len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		acWrite(t, wtRepo.Root, p, "resolved by the engineer\n")
	}
	ivMustRun(t, f.r, wtRepo.Root, "add", "-A")
	return true
}

// ivAgentGit runs one git command line the way an engineer session's Bash
// tool call would: through the shim on PATH, in the directory the session
// works in, with nothing in gitx involved. The engineer's tool policy is
// read/write with an unrestricted Bash, so these command lines are as real
// a part of what a run does to the repository as the deliverers' own, and
// the checker judges them the same way. The output and error are returned
// for a caller that cares; a command that fails is still recorded, which
// is what the invariant reads.
func (f *ivFixture) ivAgentGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := f.git.Command(t, args...)
	cmd.Dir = dir
	cmd.Env = f.r.Env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ivAgentCommands are the git command lines an engineer session plausibly
// runs inside its own worktree: reading where it is and what it changed,
// committing, and throwing away its own scratch work. Every one of them is
// within the invariant, because the worktree is the agent's to write.
var ivAgentCommands = [][]string{
	{"status", "--porcelain"},
	{"diff", "--stat"},
	{"log", "--oneline", "-n", "5"},
	{"rev-parse", "--abbrev-ref", "HEAD"},
	{"commit", "--allow-empty", "--quiet", "-m", "checkpoint from the agent"},
	{"checkout", "--", "."},
}

// ivAgentSession runs one of those command lines in the bead's worktree.
// A failure is expected sometimes — the worktree may be mid-rebase — and
// tolerated, because what is judged is the command line, not its exit code.
func (f *ivFixture) ivAgentSession(t *testing.T, bead domain.Bead, args []string) {
	t.Helper()
	if out, err := f.ivAgentGit(t, f.m.PathFor(bead.ID), args...); err != nil {
		t.Logf("agent ran git %s in %s: %v: %s", strings.Join(args, " "), bead.ID, err, out)
	}
}

func TestSafetyOnNeverTouchesTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	f := ivSetup(t, ivSafetyOn)

	// The P6-09 scenario in git terms: two beads, an engineer commit and a
	// review round each, an upstream change landing in between, then the
	// dependent bead delivered after the one it depends on — and a retry of
	// the first delivery, which is what a gate poll of a bead whose pull
	// request is still open leads to.
	for i, bead := range f.beads {
		f.ivWork(t, bead, fmt.Sprintf("engineer round 1 on %s\n", bead.ID), i == 0)
		// The session's own git, run from its Bash the way an engineer
		// would, is judged with everything else.
		for _, args := range ivAgentCommands {
			f.ivAgentSession(t, bead, args)
		}
		acWrite(t, f.m.PathFor(bead.ID), "stats/leftovers.md", "left uncommitted\n")
		leftovers, err := f.m.CommitLeftovers(ctx, bead, 1)
		require.NoError(t, err)
		require.True(t, leftovers.Committed)
		f.ivAssert(t, "after the review round of "+string(bead.ID))
	}

	f.ivUpstream(t, "release-notes", false)
	f.ivAssert(t, "after an upstream commit")

	for _, bead := range f.beads {
		req := dpRequest()
		req.Bead = bead
		res, err := f.d.Deliver(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, domain.BeadStateDelivering, res.State)
		assert.True(t, res.Pushed, "the bead's branch reaches the remote")
		f.ivAssert(t, "after delivering "+string(bead.ID))
	}
	require.NoError(t, f.ivDeliver(t, f.beads[0]), "a second delivery of the same bead is a no-op retry")

	calls := f.ivAssert(t, "at the end of the scenario")

	// The invariant must not hold merely because nothing happened: the
	// branches really are on the remote, and the pushes really ran.
	assert.GreaterOrEqual(t, ivCount(calls, "push"), len(f.beads), "every bead's branch was pushed")
	heads := ivMustRun(t, f.r, f.r.Dir, "ls-remote", "--heads", f.r.RemoteDir)
	for _, bead := range f.beads {
		assert.Contains(t, heads, "refs/heads/"+BranchName(bead), "%s was delivered as a branch", bead.ID)
	}
	assert.Len(t, f.tracker.gates, len(f.beads), "each bead ends behind its own gh:pr gate (D-20)")
	// At least one per bead: the retry above asks gh for one more, because
	// the shim's "pr list" never remembers the ones it opened.
	assert.GreaterOrEqual(t, strings.Count(strings.Join(dpCalls(t, f.gh), "\n"), "pr create"), len(f.beads),
		"a pull request per bead is what the human is given instead")
	assert.Zero(t, ivCount(calls, "merge"), "safety on never merges")
}

func TestSafetyOffOnlyFastForwardsTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	f := ivSetup(t, ivSafetyOff)
	start := f.mainSeen

	for _, bead := range f.beads {
		f.ivWork(t, bead, fmt.Sprintf("engineer round 1 on %s\n", bead.ID), false)
		acWrite(t, f.m.PathFor(bead.ID), fmt.Sprintf("stats/%s-leftovers.md", bead.ID), "left uncommitted\n")
		leftovers, err := f.m.CommitLeftovers(ctx, bead, 1)
		require.NoError(t, err)
		require.True(t, leftovers.Committed)
	}
	// Someone else lands work on the default branch first, so the local
	// branch is behind its remote and the second bead has to be rebased
	// onto a branch that moved twice.
	f.ivUpstream(t, "release-notes", false)
	f.ivAssert(t, "after an upstream commit")

	for _, bead := range f.beads {
		res, err := f.m.DeliverMerge(ctx, bead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Positive(t, res.Commits, "%s landed commits on %s", bead.ID, f.env.def)
		assert.True(t, res.Pushed, "%s reached the remote", bead.ID)
		assert.Equal(t, f.env.def, res.Target)
		assert.True(t, res.Rebased, "%s was rebased onto a %s that moved under it", bead.ID, f.env.def)
		f.ivAssert(t, "after delivering "+string(bead.ID))
	}

	calls := f.ivAssert(t, "at the end of the scenario")

	// Non-vacuity: the default branch really did move, through a real
	// fast-forward merge, and the remote agrees with it.
	local, remote := f.tips(t)
	assert.NotEqual(t, start, local, "the default branch received the work")
	assert.Equal(t, local, remote, "the remote default branch matches the local one")
	assert.True(t, f.isAncestor(t, "", start, local), "the default branch only moved forward")
	assert.Positive(t, ivCount(calls, "merge"), "a fast-forward merge is how the work landed")
	for _, c := range calls {
		if c.sub == "merge" {
			assert.Contains(t, c.rest, "--ff-only", "every merge is a fast-forward: %s", c)
		}
	}
	subjects := ivMustRun(t, f.r, f.r.Dir, "log", "--format=%s", "refs/heads/"+f.env.def)
	for _, bead := range f.beads {
		assert.Contains(t, subjects, "work on "+string(bead.ID), "%s's work is on %s", bead.ID, f.env.def)
	}
}

// ivEvent is one thing that can happen to a run, drawn by the randomised
// sequences with the weight beside it.
type ivEvent struct {
	name   string
	weight int
	// expectGit is true when the event always reaches git through the
	// shim, so a step that recorded nothing means the event did nothing
	// and the checks after it prove nothing. It is false for the two
	// events that legitimately run no git of ours: the upstream commit
	// (deliberately made with the real git, so the test's own colleague is
	// never mistaken for the code under test) and resolving a conflict,
	// which returns immediately when there is no rebase in progress.
	expectGit bool
	do        func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, n int)
}

// ivEvents is the alphabet the randomised sequences draw from. Everything
// a run does to a git repository between the first engineer session and
// delivery is in it, so a sequence exercises orderings no scenario test
// would write down: a delivery with nothing to deliver, two deliveries in
// a row, an upstream change landing while a worktree is mid-rebase, a
// conflict resolved and then another one opened on top of it.
var ivEvents = []ivEvent{
	{name: "engineer commit", weight: 4, expectGit: true, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, n int) {
		f.ivWork(t, bead, fmt.Sprintf("round %d\n", n), rng.IntN(2) == 0)
	}},
	{name: "agent shell git", weight: 2, expectGit: true, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, _ int) {
		f.ivAgentSession(t, bead, ivAgentCommands[rng.IntN(len(ivAgentCommands))])
	}},
	{name: "commit leftovers", weight: 2, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, n int) {
		acWrite(t, f.m.PathFor(bead.ID), fmt.Sprintf("stats/left-%d.md", n), "uncommitted\n")
		if _, err := f.m.CommitLeftovers(context.Background(), bead, n); err != nil {
			t.Logf("leftovers for %s: %v", bead.ID, err) // tolerated (P6-16)
		}
	}},
	{name: "upstream commit", weight: 2, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, _ domain.Bead, n int) {
		f.ivUpstream(t, fmt.Sprintf("change-%d", n), rng.IntN(2) == 0)
	}},
	{name: "deliver", weight: 4, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		_ = f.ivDeliver(t, bead) // the outcome is counted in f.stats
	}},
	{name: "resolve conflict", weight: 2, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		f.ivResolveConflict(t, bead)
	}},
	{name: "reattach worktree", weight: 1, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		if _, err := f.m.Reattach(context.Background(), bead.ID); err != nil {
			t.Logf("reattaching %s: %v", bead.ID, err)
		}
	}},
	{name: "read the diff", weight: 1, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		if _, err := f.m.Diff(context.Background(), bead.ID); err != nil {
			t.Logf("diffing %s: %v", bead.ID, err)
		}
	}},
}

// ivPick draws one event with the weights above.
func ivPick(rng *rand.Rand) ivEvent {
	total := 0
	for _, e := range ivEvents {
		total += e.weight
	}
	n := rng.IntN(total)
	for _, e := range ivEvents {
		if n < e.weight {
			return e
		}
		n -= e.weight
	}
	return ivEvents[len(ivEvents)-1]
}

// ivEventsPerSequence is how many events one randomised sequence applies.
// Every event is followed by a full check of both invariants, so a
// sequence costs what it is long; twelve is enough for deliveries,
// conflicts, resolutions and retries to interleave.
const ivEventsPerSequence = 12

// ivSeeds are the fixed seeds the randomised sequences run with. They are
// constants rather than time-derived, so a failure is reproducible by
// running the same subtest again, and the totals asserted at the end of
// the test are the same on every machine. They are chosen against the
// event alphabet above — adding an event changes every draw, so a seed set
// that no longer reaches a rebase conflict fails the end-of-mode check
// below rather than quietly testing less.
var ivSeeds = []uint64{1, 5, 17, 1729}

func TestSafetyInvariantsUnderRandomisedEventSequences(t *testing.T) {
	for _, safety := range []ivSafety{ivSafetyOn, ivSafetyOff} {
		total := ivStats{}
		for _, seed := range ivSeeds {
			t.Run(fmt.Sprintf("safety-%s/seed-%d", safety, seed), func(t *testing.T) {
				rng := rand.New(rand.NewPCG(seed, seed))
				f := ivSetup(t, safety)
				// Every bead starts with a session behind it, the way the
				// scheduler only ever delivers a bead an engineer has
				// worked on; the shared file makes an upstream change
				// conflict with it later.
				for i, bead := range f.beads {
					f.ivWork(t, bead, fmt.Sprintf("first session on %s\n", bead.ID), i == 0)
				}
				log := []string{"0. first engineer session on every bead"}
				f.ivAssertStep(t, log[0]) // sets the mark the per-step check counts from

				for i := 1; i <= ivEventsPerSequence; i++ {
					event := ivPick(rng)
					bead := f.beads[rng.IntN(len(f.beads))]
					event.do(t, f, rng, bead, i)
					step := fmt.Sprintf("%d. %s (%s)", i, event.name, bead.ID)
					log = append(log, step)
					// Only the invocations this event produced count: the
					// log is never empty after step 0, so asserting on the
					// whole of it would prove nothing about the step.
					fresh := f.ivAssertStep(t, "after event "+step)
					if event.expectGit {
						assert.NotEmpty(t, fresh, "%s ran no git at all, so the checks after it judged nothing", step)
					}
					if t.Failed() {
						t.Fatalf("sequence so far:\n%s", strings.Join(log, "\n"))
					}
				}
				total.add(f.stats)
				t.Logf("sequence (%s):\n%s", f.stats, strings.Join(log, "\n"))
			})
		}
		// The invariants above are only worth something if the sequences
		// really delivered: a run of pure no-ops would satisfy every one
		// of them. The seeds are fixed, so these totals are stable.
		t.Run("safety-"+string(safety)+"/the sequences delivered", func(t *testing.T) {
			assert.Positive(t, total.delivered, "some delivery succeeded across the sequences (%s)", total)
			if safety == ivSafetyOff {
				// Only the merge path rebases, so only it can conflict;
				// reaching a conflict is what makes the mid-rebase
				// orderings above real (D-10).
				assert.Positive(t, total.conflicted, "some delivery stopped on a rebase conflict across the sequences (%s)", total)
			}
		})
	}
}

// TestIvTheShimCatchesAForbiddenCommand runs one of the commands the
// invariant forbids through the very shim the scenarios watch, so the
// whole path — shim, log, parser, checker — is known to catch a delivery
// that starts writing to the default branch, rather than only the
// synthetic argv of TestIvInspect.
func TestIvTheShimCatchesAForbiddenCommand(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	before := len(ivScan(ivParse(t, f.git), f.env))
	require.Zero(t, before, "the fixture itself is within the invariant")

	cmd := f.git.Command(t, "push", "--quiet", "origin", "refs/heads/"+f.env.def+":refs/heads/"+f.env.def)
	cmd.Dir = f.r.Dir
	cmd.Env = f.r.Env()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "pushing the default branch to where it already is: %s", out)

	bad := ivScan(ivParse(t, f.git), f.env)
	require.Len(t, bad, 1, "the forbidden command is caught exactly once")
	assert.Contains(t, bad[0], "pushing "+f.env.def)
}

// TestAnAgentsOwnGitIsCaughtByTheSameChecker closes the other half of the
// question the invariant answers. An engineer session runs read/write with
// an unrestricted Bash inside a linked worktree of the target repository,
// so "git push origin HEAD:refs/heads/main" or "git clean -fdx" from an
// agent is possible and the only production guard is a sentence in its
// prompt. Here the agent's own command lines go through the same shim as
// the deliverers', and the checker judges them identically: what is
// legitimate inside the bead's worktree stays legitimate, and the same
// verbs aimed at the default branch or at the user's own working tree are
// caught — argv first, and for real damage the repository half as well.
func TestAnAgentsOwnGitIsCaughtByTheSameChecker(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	wt := f.m.PathFor(f.beads[0].ID)
	require.Empty(t, ivScan(ivParse(t, f.git), f.env), "the fixture itself is within the invariant")

	def := f.env.def
	cases := []struct {
		name string
		dir  string
		args []string
		want string // a substring of the expected violation, "" for none
	}{
		{name: "the agent pushing the default branch from its worktree", dir: wt,
			args: []string{"push", "--quiet", "origin", "refs/heads/" + def + ":refs/heads/" + def},
			want: "pushing " + def},
		{name: "the agent checking out the default branch", dir: f.r.Dir,
			args: []string{"checkout", def}, want: "checking out " + def},
		{name: "the agent pushing with no refspec from the main working tree", dir: f.r.Dir,
			args: []string{"push", "--quiet", "origin"}, want: "pushing " + def},
		{name: "the agent throwing away the user's working tree", dir: f.r.Dir,
			args: []string{"checkout", "--", "."}, want: "discards the user's uncommitted work"},
		// --dry-run so the fixture survives to the repository check below;
		// the checker judges the command line, which is a commit in the
		// user's own working tree either way.
		{name: "the agent committing in the main working tree", dir: f.r.Dir,
			args: []string{"commit", "--dry-run", "--allow-empty", "-m", "sneaky"},
			want: "moves " + def},
		{name: "the agent committing in its own worktree", dir: wt,
			args: []string{"commit", "--allow-empty", "--quiet", "-m", "checkpoint"}},
		{name: "the agent throwing away its own scratch work", dir: wt,
			args: []string{"checkout", "--", "."}},
		{name: "the agent reading the main working tree", dir: f.r.Dir,
			args: []string{"status", "--porcelain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.git.Reset(t)
			// The command may fail (no upstream, nothing to commit); the
			// invocation is recorded either way and that is what is judged.
			if out, err := f.ivAgentGit(t, tc.dir, tc.args...); err != nil {
				t.Logf("git %s in %s: %v: %s", strings.Join(tc.args, " "), tc.dir, err, out)
			}
			calls := ivParse(t, f.git)
			require.Len(t, calls, 1, "exactly the agent's command was recorded")
			bad := ivScan(calls, f.env)
			if tc.want == "" {
				assert.Empty(t, bad, "the agent's own worktree is the agent's to write")
				return
			}
			require.NotEmpty(t, bad, "the agent's command went unjudged")
			assert.Contains(t, strings.Join(bad, "\n"), tc.want)
		})
	}

	// Damage the repository half must see too, so this is not only a
	// statement about strings: the agent deletes the user's untracked work.
	require.Empty(t, f.ivRepoViolations(t), "the repository is still where the scenario left it")
	f.git.Reset(t)
	if out, err := f.ivAgentGit(t, f.r.Dir, "clean", "-fdx", "--quiet"); err != nil {
		t.Logf("git clean in the main working tree: %v: %s", err, out)
	}
	assert.Contains(t, strings.Join(ivScan(ivParse(t, f.git), f.env), "\n"), "clean in the main working tree")
	assert.Contains(t, strings.Join(f.ivRepoViolations(t), "\n"), "the user's own uncommitted file is gone",
		"the repository half sees the user's work vanish, not only the command that did it")
}

// TestIvTheGHShimCatchesAPullRequestMerge is the gh half of
// TestIvTheShimCatchesAForbiddenCommand: "gh pr merge" lands work on the
// default branch without running a single git command, so the git checker
// would never see it. It is caught by the gh allow list instead.
func TestIvTheGHShimCatchesAPullRequestMerge(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	require.Empty(t, ivScanGH(t, f.gh), "the fixture itself asks gh for nothing forbidden")

	cmd := f.gh.Command(t, "pr", "merge", "12", "--squash", "--delete-branch")
	cmd.Dir = f.r.Dir
	cmd.Env = f.r.Env()
	// The shim refuses the command; that it was asked for at all is the
	// violation.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("gh pr merge: %v: %s", err, out)
	}

	bad := ivScanGH(t, f.gh)
	require.Len(t, bad, 1, "the forbidden command is caught exactly once")
	assert.Contains(t, bad[0], "landing work on the default branch")
}

func TestIvInspectGH(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // a substring of the expected violation, "" for none
	}{
		{name: "listing pull requests", args: []string{"pr", "list", "--head", "sc/fx-1", "--json", "number"}},
		{name: "viewing one", args: []string{"pr", "view", "12", "--json", "state"}},
		{name: "opening one", args: []string{"pr", "create", "--head", "sc/fx-1", "--base", "main", "--body-file", "-"}},
		{name: "reviewing one", args: []string{"pr", "review", "12", "--request-changes", "--body-file", "-"}},
		{name: "making the label", args: []string{"label", "create", "slop-cannon", "--force"}},
		{name: "probing the binary", args: []string{"--version"}},
		{name: "merging one", args: []string{"pr", "merge", "12", "--squash"},
			want: "landing work on the default branch"},
		{name: "closing one", args: []string{"pr", "close", "12"}, want: "still be reading"},
		{name: "deleting the repository", args: []string{"repo", "delete", "acme/widgets", "--yes"},
			want: "deletes the repository"},
		{name: "changing the default branch", args: []string{"repo", "edit", "--default-branch", "sc/fx-1"},
			want: "default branch"},
		{name: "anything else at all", args: []string{"api", "-X", "DELETE", "repos/acme/widgets/git/refs/heads/main"},
			want: "not one of the commands a delivery makes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ivInspectGH(tc.args)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}
}

func TestIvInspect(t *testing.T) {
	// The checker itself must catch the commands the invariants are about,
	// so a scenario that stops producing them cannot pass silently.
	env := ivEnv{def: "main", remote: "origin", mainDir: "/repo"}
	cases := []struct {
		name   string
		safety ivSafety
		dir    string
		args   []string
		want   string // a substring of the expected violation, "" for none
	}{
		{name: "safety on refuses a push of the default branch", safety: ivSafetyOn,
			args: []string{"push", "--quiet", "--", "origin", "refs/heads/main:refs/heads/main"}, want: "pushing main"},
		{name: "safety on refuses a checkout of the default branch", safety: ivSafetyOn,
			args: []string{"checkout", "main"}, want: "checking out main"},
		{name: "safety on refuses a merge", safety: ivSafetyOn,
			args: []string{"-c", "merge.ff=only", "merge", "--ff-only", "--quiet", "sc/fx-1"}, want: "merges nothing"},
		{name: "safety on refuses an in-place update of the default branch", safety: ivSafetyOn,
			args: []string{"fetch", "--quiet", ".", "sc/fx-1:refs/heads/main"}, want: "updating main in place"},
		{name: "safety on allows pushing a bead branch", safety: ivSafetyOn,
			args: []string{"push", "--quiet", "--", "origin", "refs/heads/sc/fx-1:refs/heads/sc/fx-1"}},
		{name: "safety on allows a fetch of the tracking refs", safety: ivSafetyOn,
			args: []string{"fetch", "--quiet", "--prune", "origin"}},
		{name: "safety on allows a worktree based on the default branch", safety: ivSafetyOn,
			args: []string{"worktree", "add", "--quiet", "/wt/fx-1", "-b", "sc/fx-1", "main"}},
		{name: "safety off allows a fast-forward merge", safety: ivSafetyOff,
			args: []string{"-c", "merge.ff=only", "merge", "--ff-only", "--quiet", "sc/fx-1"}},
		{name: "safety off refuses a merge commit", safety: ivSafetyOff,
			args: []string{"merge", "--no-ff", "sc/fx-1"}, want: "without --ff-only"},
		{name: "safety off refuses a forced push of the default branch", safety: ivSafetyOff,
			args: []string{"push", "--force", "origin", "main"}, want: "forced or deleting push"},
		{name: "safety off refuses a forced refspec", safety: ivSafetyOff,
			args: []string{"push", "origin", "+refs/heads/sc/fx-1:refs/heads/main"}, want: "not a fast-forward"},
		{name: "safety off allows the delivery push", safety: ivSafetyOff,
			args: []string{"push", "--quiet", "origin", "refs/heads/main:refs/heads/main"}},
		{name: "a rebase in the main working tree is refused", safety: ivSafetyOff, dir: "/repo",
			args: []string{"rebase", "--quiet", "main"}, want: "rewrites main"},
		{name: "a rebase in a worktree is allowed", safety: ivSafetyOff, dir: "/wt/fx-1",
			args: []string{"rebase", "--no-verify", "--quiet", "main"}},
		{name: "a commit in the main working tree is refused", safety: ivSafetyOff, dir: "/repo",
			args: []string{"commit", "--quiet", "-m", "sneaky"}, want: "moves main"},
		{name: "deleting the default branch is refused", safety: ivSafetyOff,
			args: []string{"branch", "-D", "--quiet", "main"}, want: "deleting or moving main"},
		{name: "deleting a bead branch is allowed", safety: ivSafetyOff,
			args: []string{"branch", "-D", "--quiet", "sc/fx-1"}},
		{name: "renaming the default branch is refused", safety: ivSafetyOff,
			args: []string{"branch", "-m", "main", "old-main"}, want: "deleting or moving main"},
		{name: "a fetch of the default branch's tracking ref is allowed", safety: ivSafetyOn,
			args: []string{"fetch", "origin", "refs/heads/main:refs/remotes/origin/main"}},
		// A push with no refspec names no branch at all: git pushes the one
		// that is checked out where it runs, which in the main working tree
		// is the default branch.
		{name: "safety on refuses a refspec-less push from the main working tree", safety: ivSafetyOn, dir: "/repo",
			args: []string{"push", "--quiet", "origin"}, want: "pushing main"},
		{name: "safety on refuses a bare git push in the main working tree", safety: ivSafetyOn, dir: "/repo",
			args: []string{"push"}, want: "pushing main"},
		{name: "safety off refuses a forced refspec-less push from the main working tree", safety: ivSafetyOff, dir: "/repo",
			args: []string{"push", "--force-with-lease", "origin"}, want: "not a fast-forward"},
		{name: "safety off allows a refspec-less push from the main working tree", safety: ivSafetyOff, dir: "/repo",
			args: []string{"push", "--quiet", "origin"}},
		{name: "a refspec-less push from a worktree is allowed", safety: ivSafetyOn, dir: "/wt/fx-1",
			args: []string{"push", "--quiet", "origin"}},
		// The user's uncommitted work in the main working tree is theirs,
		// in either mode; the same commands in a bead's worktree are the
		// agent tidying up after itself.
		{name: "git clean in the main working tree is refused", safety: ivSafetyOn, dir: "/repo",
			args: []string{"clean", "-fdx"}, want: "discards the user's uncommitted work"},
		{name: "git clean in a worktree is allowed", safety: ivSafetyOn, dir: "/wt/fx-1",
			args: []string{"clean", "-fdx"}},
		{name: "checking out paths in the main working tree is refused", safety: ivSafetyOff, dir: "/repo",
			args: []string{"checkout", "--", "."}, want: "discards the user's uncommitted work"},
		{name: "checking out paths in a worktree is allowed", safety: ivSafetyOff, dir: "/wt/fx-1",
			args: []string{"checkout", "--", "stats/mean.go"}},
		{name: "git restore in the main working tree is refused", safety: ivSafetyOff, dir: "/repo",
			args: []string{"restore", "--staged", "--worktree", "."}, want: "discards the user's uncommitted work"},
		{name: "git pull is refused", safety: ivSafetyOn, args: []string{"pull", "origin", "sc/fx-1"}, want: "merges into the checked-out branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := env
			e.safety = tc.safety
			sub, rest := ivSplit(tc.args)
			dir := tc.dir
			if dir == "" {
				dir = "/wt/fx-1"
			}
			got := ivInspect(ivCall{seq: 1, dir: dir, args: tc.args, sub: sub, rest: rest}, e)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, strings.Join(got, "\n"), tc.want)
		})
	}
}

func TestIvSplitAndOperands(t *testing.T) {
	sub, rest := ivSplit([]string{"-c", "commit.gpgsign=false", "-c", "rebase.autoStash=false", "rebase", "--quiet", "main"})
	assert.Equal(t, "rebase", sub)
	assert.Equal(t, []string{"--quiet", "main"}, rest)

	sub, rest = ivSplit([]string{"--git-dir=/tmp/x.git", "log", "--merges"})
	assert.Equal(t, "log", sub)
	assert.Equal(t, []string{"--merges"}, rest)

	assert.Empty(t, ivOperands([]string{"--quiet", "--prune"}, false))
	assert.Equal(t, []string{"origin", "refs/heads/a:refs/heads/b"},
		ivOperands([]string{"--quiet", "--", "origin", "refs/heads/a:refs/heads/b"}, false))
	assert.Equal(t, []string{"main"}, ivOperands([]string{"main", "--", "stats/mean.go"}, true))
	assert.Equal(t, []string{"add", "/wt/fx-1", "main"},
		ivOperands([]string{"add", "--quiet", "/wt/fx-1", "-b", "sc/fx-1", "main"}, false))

	specs := ivRefspecs([]string{"--quiet", "origin", "+refs/heads/a:refs/heads/main", "sc/b"})
	require.Len(t, specs, 2)
	assert.Equal(t, ivSpec{src: "refs/heads/a", dst: "refs/heads/main", force: true}, specs[0])
	assert.Equal(t, ivSpec{src: "sc/b", dst: "sc/b"}, specs[1])

	env := ivEnv{def: "main", remote: "origin"}
	for _, ref := range []string{"main", "refs/heads/main", "origin/main", "refs/remotes/origin/main", "+main", "main^{commit}"} {
		assert.True(t, ivIsDefaultRef(ref, env), "%s names the default branch", ref)
	}
	for _, ref := range []string{"", "sc/fx-1", "refs/heads/mainline", "upstream/main", "HEAD"} {
		assert.False(t, ivIsDefaultRef(ref, env), "%s does not name the default branch", ref)
	}
}
