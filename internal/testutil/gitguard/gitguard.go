// Package gitguard judges the command lines a run produces (P9-02, P9-23).
//
// Almost everything the cannon does to the target repository goes through
// git, so the guarantee "the cannon cannot damage the target repo" is
// largely a statement about the argv it produces. A test installs a
// recording shim in front of git (and, for safety on, gh) with
// [testutil.FakeBin], runs the real thing, and then hands the recorded
// invocations to [Scan] and [ScanGH], which return one sentence per
// invariant broken.
//
// It lives here rather than in a single package's test file because two
// suites need the same judgement over different scopes: internal/gitx puts
// the deliverers, the worktree manager, an agent's own shell git and the
// resumer under it directly, and internal/orchestrator drives whole
// end-to-end runs under it, which extends the same argv invariant to
// branch cleanup, the delivery settler, the gate poller and the external
// pull request reviewer — the production run path P9-02 could only cover
// from the other side, by asserting that neither default branch tip moved.
//
// Two invariants are asserted, one per safety mode (D-06):
//
//   - safety on ([SafetyOn], gitx.PRDeliverer): no invocation ever checks
//     out, merges or pushes the default branch. Work reaches the human
//     only as a pushed sc/ branch and a pull request.
//   - safety off ([SafetyOff], gitx.Manager.DeliverMerge): the default
//     branch is only ever moved by a fast-forward — every merge carries
//     --ff-only, no ref update of the default branch is forced, no merge
//     commit lands on it.
//
// A handful of rules hold in both modes, because they are damage rather
// than delivery: the default branch is never deleted, renamed or checked
// out, "git pull" is never run, nothing that rewrites the branch it runs
// on (commit, reset, revert, cherry-pick, rebase …) is ever run in the
// main working tree, where the default branch is the one checked out, and
// nothing that discards what is in that tree (clean, checkout of paths,
// restore, stash) is run there either — the user's uncommitted work is
// theirs.
//
// gh is judged with an allow list rather than a deny list: git can be
// reasoned about verb by verb, but gh's surface is the whole GitHub API,
// so anything a delivery has never needed is reported and a reviewer
// decides. "gh pr merge" above all, which lands work on the default branch
// with no git call for the git checker to see, and which D-11 forbids
// outright for a pull request sc did not open. It is damage by default; a
// scenario that runs with auto_merge on (D-31) opts in with
// [GHOptions.AutoMerge], and even then only the exact merge auto-merge
// makes passes — a squash that names the head commit, never --admin,
// --auto or --delete-branch. Whose pull request was merged is not in the
// argv, so such a scenario must also assert that itself.
package gitguard

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"slop-cannon/internal/testutil"
)

// Safety names the delivery mode a scenario runs in (D-06).
type Safety string

const (
	// SafetyOn delivers through a pull request and never writes to the
	// default branch.
	SafetyOn Safety = "on"
	// SafetyOff merges into the default branch, fast-forward only.
	SafetyOff Safety = "off"
)

// SafetyFor maps the configuration's boolean onto a [Safety].
func SafetyFor(on bool) Safety {
	if on {
		return SafetyOn
	}
	return SafetyOff
}

// Env is what judging one recorded git invocation needs: which branch is
// the default one, which remote it lives on, where the main working tree
// is, and which safety mode was in force.
type Env struct {
	// Safety is the delivery mode the scenario ran in.
	Safety Safety
	// Default is the default branch, "main" in the fixtures.
	Default string
	// Remote is the default remote, normally "origin".
	Remote string
	// MainDir is the main working tree, with symlinks resolved; use
	// [ResolvePath] to fill it.
	MainDir string
}

// Call is one recorded git invocation split into the subcommand and its
// arguments, with git's own global options (-c, -C, --git-dir …) removed.
type Call struct {
	// Seq is the 1-based position of the call in the shim's log.
	Seq int
	// Dir is the directory git ran in, with symlinks resolved.
	Dir string
	// Args is the full argument list, for reporting.
	Args []string
	// Sub is the subcommand, "" when the call had none.
	Sub string
	// Rest is everything after the subcommand.
	Rest []string
}

// String renders the call the way a reader would have typed it.
func (c Call) String() string {
	return fmt.Sprintf("#%d git %s (in %s)", c.Seq, strings.Join(c.Args, " "), c.Dir)
}

// globalValueOpts are git's global options that take their value as the
// next argument, so the subcommand scanner can step over both.
var globalValueOpts = map[string]bool{
	"-c": true, "-C": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true, "--config-env": true,
}

// valueFlags are the subcommand flags whose value is a separate argument
// and is therefore not an operand.
var valueFlags = map[string]bool{
	"-b": true, "-B": true, "-m": true, "--message": true, "--onto": true,
	"-s": true, "--strategy": true, "-X": true, "--strategy-option": true,
	"-o": true, "--push-option": true, "--repo": true, "--exec": true,
	"--depth": true, "-C": true, "-c": true, "--reuse-message": true,
	"--upstream": true,
}

// Split separates git's global options from the subcommand and its own
// arguments. "git -c merge.ff=only merge --ff-only x" splits into "merge"
// and ["--ff-only", "x"].
func Split(args []string) (sub string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:]
		}
		name, _, hasValue := strings.Cut(a, "=")
		if globalValueOpts[name] && !hasValue {
			i++
		}
	}
	return "", nil
}

// operands returns the non-flag arguments of a subcommand. With
// stopAtEndOfOptions the scan stops at "--" (checkout, where what follows
// are paths); otherwise "--" is skipped and what follows is taken as
// operands (push, where the remote and the refspecs follow it).
func operands(rest []string, stopAtEndOfOptions bool) []string {
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
			if valueFlags[name] && !hasValue {
				i++
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

// flagValue returns the value of the first of names present in rest,
// accepting both "--flag value" and "--flag=value".
func flagValue(rest []string, names ...string) (string, bool) {
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

// hasFlag reports whether rest contains any of names, in either the
// "--flag" or the "--flag=value" spelling.
func hasFlag(rest []string, names ...string) bool {
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

// refspec is one refspec operand of a push or a fetch.
type refspec struct {
	src   string
	dst   string
	force bool // the refspec was written with a leading "+"
}

// refspecs returns the refspecs of a push or fetch. The first operand is
// the remote (a name, a URL, or "." for the repository itself) and every
// operand after it is a refspec; a refspec without a colon pushes or
// fetches the same name on both sides.
func refspecs(rest []string) []refspec {
	ops := operands(rest, false)
	if len(ops) < 2 {
		return nil
	}
	specs := make([]refspec, 0, len(ops)-1)
	for _, op := range ops[1:] {
		s := refspec{}
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

// isDefaultRef reports whether ref names the default branch, in any of the
// spellings git accepts: "main", "refs/heads/main", "origin/main",
// "refs/remotes/origin/main", and the same with a trailing "^{commit}".
func isDefaultRef(ref string, env Env) bool {
	ref = strings.TrimSuffix(strings.TrimPrefix(ref, "+"), "^{commit}")
	if ref == "" {
		return false
	}
	names := []string{env.Default, "refs/heads/" + env.Default}
	if env.Remote != "" {
		names = append(names, env.Remote+"/"+env.Default, "refs/remotes/"+env.Remote+"/"+env.Default)
	}
	for _, n := range names {
		if ref == n {
			return true
		}
	}
	return false
}

// branchOperands returns the branch names of a "git branch" call. No flag
// is treated as taking a separate value here, because -m and -M take
// branch names rather than values; over-reporting a rename of the default
// branch is the safe direction for an invariant.
func branchOperands(rest []string) []string {
	var out []string
	for _, a := range rest {
		if a == "--" || strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// isTrackingRef reports whether ref is the remote-tracking ref of the
// default branch rather than the branch itself. Writing it is what every
// fetch does; writing refs/heads/<default> is not.
func isTrackingRef(ref string, env Env) bool {
	return strings.HasPrefix(ref, "refs/remotes/") || (env.Remote != "" && ref == env.Remote+"/"+env.Default)
}

// worktreeWriters are the subcommands that rewrite the branch checked out
// where they run, so running one in the main working tree moves the
// default branch. A delivery only ever runs them in a bead's worktree.
var worktreeWriters = map[string]bool{
	"commit": true, "reset": true, "revert": true,
	"cherry-pick": true, "am": true, "apply": true, "stash": true,
	"filter-branch": true,
}

// treeDestroyers are the subcommands that throw away what is in the
// working tree they run in without touching a ref: "clean" deletes
// untracked files, "checkout"/"switch" with paths and "restore" overwrite
// modified ones. In a bead's worktree that is the agent tidying up after
// itself; in the main working tree it is the user's uncommitted work, and
// no part of a run has any business there.
var treeDestroyers = map[string]bool{
	"clean": true, "restore": true, "checkout": true, "switch": true,
}

// Inspect judges one recorded git invocation and returns a reason for
// every invariant it breaks (usually none).
func Inspect(c Call, env Env) []string {
	var out []string
	flag := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	inMain := c.Dir != "" && c.Dir == env.MainDir

	switch c.Sub {
	case "merge":
		switch env.Safety {
		case SafetyOn:
			flag("safety on merges nothing: %s", c)
		default:
			if !hasFlag(c.Rest, "--ff-only") {
				flag("a merge without --ff-only can put a merge commit on %s: %s", env.Default, c)
			}
			if hasFlag(c.Rest, "--no-ff", "--squash", "--no-commit", "-m", "--message") {
				flag("a merge that is not a plain fast-forward: %s", c)
			}
		}
	case "pull":
		flag("git pull merges into the checked-out branch: %s", c)
	case "checkout", "switch":
		if name, ok := flagValue(c.Rest, "-b", "-B", "-c", "-C"); ok && isDefaultRef(name, env) {
			flag("creating or resetting %s: %s", env.Default, c)
		}
		for _, ref := range operands(c.Rest, true) {
			if isDefaultRef(ref, env) {
				flag("checking out %s: %s", env.Default, c)
			}
		}
	case "worktree":
		ops := operands(c.Rest, false)
		if len(ops) == 0 || ops[0] != "add" {
			break
		}
		// "worktree add <path> -b <branch> <base>" checks out the new
		// branch; naming the default branch as the base is fine, checking
		// it out is not.
		if name, ok := flagValue(c.Rest, "-b", "-B"); ok {
			if isDefaultRef(name, env) {
				flag("a worktree on %s: %s", env.Default, c)
			}
			break
		}
		if len(ops) >= 3 && isDefaultRef(ops[2], env) {
			flag("a worktree checking out %s: %s", env.Default, c)
		}
	case "branch":
		if !hasFlag(c.Rest, "-D", "-d", "--delete", "-M", "-m", "--move", "-f", "--force") {
			break
		}
		for _, ref := range branchOperands(c.Rest) {
			if isDefaultRef(ref, env) {
				flag("deleting or moving %s: %s", env.Default, c)
			}
		}
	case "update-ref", "symbolic-ref":
		for _, ref := range operands(c.Rest, true) {
			if isDefaultRef(ref, env) {
				flag("writing the ref of %s: %s", env.Default, c)
			}
		}
	case "push":
		if hasFlag(c.Rest, "--mirror", "--all") {
			flag("a push that carries every ref, %s included: %s", env.Default, c)
		}
		forced := hasFlag(c.Rest, "-f", "--force", "--force-with-lease")
		specs := refspecs(c.Rest)
		if len(specs) == 0 && inMain {
			// "git push" and "git push <remote>" carry no refspec, so git
			// pushes the branch that is checked out where the command
			// runs (push.default = simple or current). In the main working
			// tree that is the default branch, spelled nowhere on the
			// command line.
			specs = []refspec{{src: env.Default, dst: env.Default}}
		}
		for _, s := range specs {
			if !isDefaultRef(s.dst, env) {
				continue
			}
			switch {
			case env.Safety == SafetyOn:
				flag("pushing %s: %s", env.Default, c)
			case forced || s.force || hasFlag(c.Rest, "--delete", "-d"):
				flag("a forced or deleting push of %s is not a fast-forward: %s", env.Default, c)
			}
		}
	case "fetch":
		// A fetch without a refspec only updates remote-tracking refs; one
		// with a refspec can write refs/heads/<default> in place, which is
		// how a fast-forward is applied when the default branch is not
		// checked out.
		forced := hasFlag(c.Rest, "-f", "--force")
		for _, s := range refspecs(c.Rest) {
			if !isDefaultRef(s.dst, env) || isTrackingRef(s.dst, env) {
				continue
			}
			switch {
			case env.Safety == SafetyOn:
				flag("updating %s in place: %s", env.Default, c)
			case forced || s.force:
				flag("a forced update of %s is not a fast-forward: %s", env.Default, c)
			}
		}
	case "rebase":
		if inMain {
			flag("a rebase in the main working tree rewrites %s: %s", env.Default, c)
		}
		if ops := operands(c.Rest, true); len(ops) >= 2 && isDefaultRef(ops[1], env) {
			flag("rebasing %s itself: %s", env.Default, c)
		}
	}

	if worktreeWriters[c.Sub] && inMain {
		flag("%s in the main working tree moves %s: %s", c.Sub, env.Default, c)
	}
	if treeDestroyers[c.Sub] && inMain {
		flag("%s in the main working tree discards the user's uncommitted work: %s", c.Sub, c)
	}
	return out
}

// GHAllowed are the gh command lines a run is allowed to make: reading
// pull requests, opening one for a bead's branch, leaving a review on it,
// and making the label a pull request carries. Everything else is a
// violation by default.
var GHAllowed = map[string]bool{
	"pr list": true, "pr view": true, "pr diff": true, "pr status": true,
	"pr create": true, "pr review": true,
	"label create": true, "label list": true,
	"auth status": true, "repo view": true,
}

// GHLoginQuery is the one `gh api` call a run makes: reading the
// authenticated login, which D-31's ownership check compares a pull
// request's author with. Any other `gh api` call is reported.
var GHLoginQuery = []string{"api", "user", "--jq", ".login"}

// GHOptions widens what [InspectGHWith] allows. The zero value judges
// exactly as [InspectGH] does.
type GHOptions struct {
	// AutoMerge allows the D-31 merge and branch update of a run started
	// with auto_merge on: `gh pr merge <n> --squash --match-head-commit
	// <sha>` with none of --admin, --auto, --delete-branch/-d, --merge or
	// --rebase, and `gh pr update-branch <n>` without --rebase. Everything
	// else about `pr merge` stays damage.
	AutoMerge bool
}

// GHDamage names the gh commands worth reporting in their own words,
// because they are the ones that would break the invariant rather than
// merely being unrecognised.
var GHDamage = map[string]string{
	"pr merge":    "merges a pull request, landing work on the default branch with no git command for the checker above to see",
	"pr close":    "closes a pull request a human may still be reading",
	"repo delete": "deletes the repository",
	"repo rename": "renames the repository",
	"repo edit":   "can change the repository's default branch",
	"repo sync":   "can force the repository's branches to match another's",
}

// GHKey is the "<command> <subcommand>" key of a gh invocation, "" for a
// bare "gh" or one that only carries flags (gh --version, gh --help). A
// flag's separate value would be read as a word, so a command line like
// "gh --repo acme/x pr list" would key wrongly — nothing sc writes one,
// and the allow list errs towards reporting rather than passing.
func GHKey(args []string) string {
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

// InspectGH judges one recorded gh invocation.
func InspectGH(args []string) []string { return InspectGHWith(args, GHOptions{}) }

// InspectGHWith judges one recorded gh invocation under opts.
func InspectGHWith(args []string, opts GHOptions) []string {
	key := GHKey(args)
	line := "gh " + strings.Join(args, " ")
	switch {
	case key == "" || GHAllowed[key]:
		return nil
	case key == "api user" && slices.Equal(args, GHLoginQuery):
		return nil
	case opts.AutoMerge && key == "pr merge":
		return autoMergeViolations(args, line)
	case opts.AutoMerge && key == "pr update-branch":
		if hasFlag(args, "--rebase") {
			return []string{"gh pr update-branch --rebase rewrites the pull request's branch: " + line}
		}
		return nil
	case GHDamage[key] != "":
		return []string{fmt.Sprintf("gh %s %s: %s", key, GHDamage[key], line)}
	default:
		return []string{fmt.Sprintf("gh %s is not one of the commands a delivery makes: %s", key, line)}
	}
}

// autoMergeViolations judges a `gh pr merge` under [GHOptions.AutoMerge]:
// it must be a squash, it must name the head commit it was judged on, and
// it must not bypass protection, defer the merge to GitHub or delete the
// branch.
func autoMergeViolations(args []string, line string) []string {
	var out []string
	if !hasFlag(args, "--squash") {
		out = append(out, "an auto-merge is a squash merge, and this one is not: "+line)
	}
	if v, ok := flagValue(args, "--match-head-commit"); !ok || v == "" || strings.HasPrefix(v, "-") {
		out = append(out, "an auto-merge names the head commit it was judged on, and this one does not: "+line)
	}
	for _, f := range []string{"--admin", "--auto", "--delete-branch", "-d", "--merge", "-m", "--rebase", "-r"} {
		if hasFlag(args, f) {
			out = append(out, fmt.Sprintf("an auto-merge never passes %s: %s", f, line))
		}
	}
	return out
}

// Parse reads the shim's log and splits every invocation into a [Call].
func Parse(t testing.TB, git *testutil.Shim) []Call {
	t.Helper()
	invs := git.Invocations(t)
	calls := make([]Call, 0, len(invs))
	for _, inv := range invs {
		sub, rest := Split(inv.Args)
		calls = append(calls, Call{Seq: inv.Seq, Dir: ResolvePath(inv.Dir), Args: inv.Args, Sub: sub, Rest: rest})
	}
	return calls
}

// ResolvePath resolves symlinks so a temporary directory reached through
// one still compares equal to the path git printed.
func ResolvePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// Scan judges every call and returns one reason per violation.
func Scan(calls []Call, env Env) []string {
	var bad []string
	for _, c := range calls {
		bad = append(bad, Inspect(c, env)...)
	}
	return bad
}

// ScanGH judges every gh invocation recorded so far.
func ScanGH(t testing.TB, gh *testutil.Shim) []string {
	t.Helper()
	return ScanGHWith(t, gh, GHOptions{})
}

// ScanGHWith judges every gh invocation recorded so far under opts.
func ScanGHWith(t testing.TB, gh *testutil.Shim, opts GHOptions) []string {
	t.Helper()
	var bad []string
	for _, inv := range gh.Invocations(t) {
		bad = append(bad, InspectGHWith(inv.Args, opts)...)
	}
	return bad
}

// Count returns how many recorded calls ran the given subcommand.
func Count(calls []Call, sub string) int {
	n := 0
	for _, c := range calls {
		if c.Sub == sub {
			n++
		}
	}
	return n
}
