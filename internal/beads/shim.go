package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"slop-cannon/internal/proc"
)

// Read-only bd shim for agents (P3-09, D-21).
//
// Agents never write to beads: the orchestrator is the only writer. Each
// agent worktree gets a directory holding a `bd` shim, prepended to the
// agent's PATH, that execs the real binary with the global --readonly flag
// so bd itself refuses every issue-level write ("operation 'update' is not
// allowed in read-only mode").
//
// bd's --readonly is not enough on its own, so the shim is an allow-list
// (ShimAllowed): only the read commands an agent needs reach the binary at
// all, and everything else is refused with the same message shape, so the
// client's error classification (ErrReadOnly) applies to both. bd 1.2.2
// runs a long tail of state-changing commands under --readonly —
// `config set`/`set-many`/`apply` (the D-07 status registry), `import`,
// `metrics`, `dolt`, `branch`, `vc`, `backup` (which can push the whole
// database off-machine), `federation`, `audit`, `init`, `restore`,
// `hooks`, `setup`, `rename` — and a deny-list only ever closes the
// entries someone thought to check, which is how the P3-09 review found
// `bd config set-many status.custom=…` still writing through the shim.
// Because bd's --readonly is a plain pflag bool that a later
// `--readonly=false` turns back off, the shim also refuses any --readonly
// argument of its own (ReadOnlyFlag).
//
// The shim is a guard rail, not a sandbox: an agent that invokes the real
// binary by absolute path bypasses it, which is why D-21 also puts
// PromptNote in every role prompt and keeps the orchestrator the only
// writer.

// ShimHeader is the second line of every generated shim. IsShim recognises a
// file by it, and RealBD skips such files when resolving the genuine binary.
const ShimHeader = "# slop-cannon read-only bd shim (P3-09). Do not edit."

// ReadOnlyMessage is bd's refusal text for op, which the shim reproduces
// for the commands it refuses itself: "operation 'update' is not allowed in
// read-only mode". StderrMessage/KindOf map it to ErrReadOnly.
func ReadOnlyMessage(op string) string {
	return fmt.Sprintf("operation '%s' is not allowed in read-only mode", op)
}

// ShimAllowed is the allow-list of bd commands the shim lets through to the
// real binary; every other command is refused with ReadOnlyMessage before
// bd runs. An agent only ever reads beads (D-21), so this list holds read
// commands and nothing else.
//
// It is an allow-list on purpose. bd 1.2.2 executes many state-changing
// commands under --readonly (the package comment lists the families found
// so far), and each bd release adds more, so a deny-list closes one hole at
// a time while leaving the class open — the P3-09 review reproduced
// `bd config set-many status.custom=pwned:wip` writing through a
// deny-list shim and overwriting the D-07 custom-status registry, plus
// `config apply`, `backup init`/`backup sync` (a database exfiltration
// path), `federation add-peer` and `audit record`. With an allow-list a
// new bd verb is refused until someone deliberately adds it here.
//
// One-word entries allow the command with any subcommand except those in
// ShimAllowedExceptions; two-word entries allow only that pair, so
// `config get` works and `config set` does not. A call with no command at
// all (`bd`, `bd --version`, `bd -- config set x`) is allowed: cobra prints
// help or the version and touches nothing.
var ShimAllowed = []string{
	"blocked",
	"children",
	"comments",
	"config get",
	"config list",
	"config show",
	"count",
	"dep cycles",
	"dep list",
	"dep tree",
	"export",
	"graph",
	"help",
	"history",
	"list",
	"query",
	"ready",
	"search",
	"show",
	"stale",
	"statuses",
	"types",
	"version",
}

// ShimAllowedExceptions are the write subcommands of a command ShimAllowed
// admits as a whole, refused despite that allowance. `bd comments <id>`
// lists an issue's comments — the second word is an issue id, not a
// subcommand, so no two-word allow entry can express the read — while
// `bd comments add` writes one. bd's own --readonly refuses that write too;
// listing it here means the shim never even asks.
var ShimAllowedExceptions = []string{
	"comments add",
}

// ReadOnlyFlag is bd's global read-only flag, which the shim always passes
// and never lets an agent repeat: it is an ordinary pflag bool, so a later
// `--readonly=false` (also `=0`, `=F`) anywhere in the argument list wins
// and re-enables every write (verified live against bd 1.2.2 on
// 2026-09-03: update, comment and create all succeeded through a shim
// without this guard). ShimScript therefore refuses any argument equal to
// ReadOnlyFlag or starting with ReadOnlyFlag+"=" before exec.
const ReadOnlyFlag = "--readonly"

// ShimOverrideHint is the second stderr line the shim prints when it
// refuses a --readonly argument, so an agent understands why. Only the
// first ("Error: …") line is what StderrMessage reads.
const ShimOverrideHint = "hint: the bd on your PATH is slop-cannon's read-only shim; " +
	"it always runs bd with --readonly and refuses a --readonly argument of your own."

// ShimDeniedHint is the second stderr line the shim prints when it refuses
// a command that is not in ShimAllowed, telling the agent what it may run
// instead. Like ShimOverrideHint it starts with "hint:", so StderrMessage
// still reports the "Error: …" line above it.
const ShimDeniedHint = "hint: the bd on your PATH is slop-cannon's read-only shim; " +
	"it allows reads only (bd ready, bd show <id>, bd list, bd blocked, bd dep list <id>, " +
	"bd comments <id>, bd config get <key>). The cannon records your results for you."

// shimValueFlags are bd's global flags that take a separate value, so the
// shim skips the value when looking for the subcommand — and, like cobra's
// own flag stripping, does not mistake such a value for a --readonly
// override (`bd -C --readonly=false …` passes it to -C).
var shimValueFlags = []string{"-C", "--directory", "--actor", "--db", "--dolt-auto-commit"}

// PromptNote is the sentence every role prompt carries about beads (P3-09
// "add a line to every role prompt"; consumed by the Phase 5 prompt
// builders).
const PromptNote = "You may read the beads task tracker if it helps (`bd ready`, `bd show <id>`, " +
	"`bd list -n 0 --flat`, `bd blocked`, `bd dep list <id>`), but never write to it: " +
	"the `bd` on your PATH is read-only, refuses every write, and rejects any `--readonly` argument that would " +
	"turn that off. The cannon records your results for you."

// ShimVerifyTimeout bounds each of the calls Shim.Verify makes.
const ShimVerifyTimeout = DefaultTimeout

// ShimDirName is the directory, beside the run's worktrees, that holds the
// per-agent shim directories: <run-dir>/bin/<bead-id>/bd for the worktree
// <run-dir>/wt/<bead-id> (D-23). The shim deliberately lives next to the
// worktree and not inside it, so nothing of ours shows up in the agent's
// `git status` or can be committed to the target repository.
const ShimDirName = "bin"

// WorktreesDirName is the directory of a run that holds one linked worktree
// per bead, <run-dir>/wt/<bead-id> (D-23). It mirrors gitx.WorktreesDirName;
// beads does not import gitx because gitx imports beads.
const WorktreesDirName = "wt"

// ErrShimLayout is returned by ShimDirFor and InstallForWorktree for a path
// that is not a run's worktree, so no shim directory is ever created
// outside the run directory.
var ErrShimLayout = errors.New("beads: not a run worktree")

// shimProbeBead is the bead id Verify names in its write probes. It does not
// exist, so even a bd that ignored --readonly entirely would change nothing.
const shimProbeBead = "sc-shim-probe"

// Shim is a generated read-only bd shim on disk.
type Shim struct {
	// Dir is the directory holding the shim; prepend it to the agent's PATH
	// (PathEnv, Apply).
	Dir string
	// Path is the absolute path of the shim script, Dir/bd.
	Path string
	// Real is the genuine bd binary the shim execs.
	Real string
}

// ShimOptions customises WriteShim and InstallForWorktree.
type ShimOptions struct {
	// Real is the path of the genuine bd binary. Empty means RealBD().
	Real string
	// Dir overrides where InstallForWorktree puts the shim; empty means
	// ShimDirFor(worktree). WriteShim ignores it (it takes dir directly).
	Dir string
	// SkipVerify makes InstallForWorktree return without running the
	// Shim.Verify self-check. Only for callers that verify themselves.
	SkipVerify bool
}

// WriteShim writes the read-only bd shim into dir (created if missing,
// existing shim overwritten) and returns it. dir is normally the per-worktree
// bin directory under the run's state directory (D-23), never inside the
// target repository. The shim is a POSIX shell script; on Windows WriteShim
// returns an error. A missing real binary satisfies proc.IsNotFound.
func WriteShim(dir string, opts ShimOptions) (Shim, error) {
	if runtime.GOOS == "windows" {
		return Shim{}, fmt.Errorf("beads: the read-only bd shim needs a POSIX shell, unsupported on %s", runtime.GOOS)
	}
	if dir == "" {
		return Shim{}, errors.New("beads: shim directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Shim{}, fmt.Errorf("beads: resolving shim directory: %w", err)
	}
	real := opts.Real
	if real == "" {
		real, err = RealBD()
		if err != nil {
			return Shim{}, err
		}
	}
	if !filepath.IsAbs(real) {
		real, err = filepath.Abs(real)
		if err != nil {
			return Shim{}, fmt.Errorf("beads: resolving real bd path: %w", err)
		}
	}
	if IsShim(real) {
		return Shim{}, fmt.Errorf("beads: %s is itself a read-only shim, refusing to chain shims", real)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return Shim{}, fmt.Errorf("beads: creating shim directory: %w", err)
	}
	s := Shim{Dir: abs, Path: filepath.Join(abs, Binary), Real: real}
	// Write to a temporary file and rename so a concurrently running agent
	// never sees a half-written script.
	tmp, err := os.CreateTemp(abs, "."+Binary+".*")
	if err != nil {
		return Shim{}, fmt.Errorf("beads: creating shim: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(ShimScript(real))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		return Shim{}, fmt.Errorf("beads: writing shim: %w", errors.Join(werr, cerr))
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return Shim{}, fmt.Errorf("beads: making shim executable: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		_ = os.Remove(tmpName)
		return Shim{}, fmt.Errorf("beads: installing shim: %w", err)
	}
	return s, nil
}

// shimAllowLists splits ShimAllowed and ShimAllowedExceptions into the
// one-word commands, the two-word pairs that are allowed, the two-word
// pairs that are refused despite a one-word allowance, and the command
// families whose refusals name both words (`config`, `dep`, `comments`), so
// `bd config set-many …` is refused as "config set-many". Every element is
// shell-quoted, ready for a `case` pattern.
func shimAllowLists() (one, two, except, families []string) {
	seen := map[string]bool{}
	family := func(cmd string) {
		if !seen[cmd] {
			seen[cmd] = true
			families = append(families, shellQuote(cmd))
		}
	}
	for _, a := range ShimAllowed {
		cmd, sub, isPair := strings.Cut(a, " ")
		if !isPair {
			one = append(one, shellQuote(a))
			continue
		}
		two = append(two, shellQuote(cmd+" "+sub))
		family(cmd)
	}
	for _, e := range ShimAllowedExceptions {
		cmd, sub, isPair := strings.Cut(e, " ")
		if !isPair {
			continue
		}
		except = append(except, shellQuote(cmd+" "+sub))
		family(cmd)
	}
	return one, two, except, families
}

// ShimScript returns the shell script of a shim that execs real. The
// script scans every argument the way cobra's own flag stripping does
// (skipping global flags and the values of shimValueFlags, stopping at a
// bare "--", which makes everything after it positional and inert), then:
//
//   - refuses any --readonly argument of the caller's with
//     ReadOnlyMessage(argument) plus ShimOverrideHint, because
//     `--readonly=false` would otherwise win over the flag the shim
//     passes;
//   - refuses every command that ShimAllowed does not admit, and every
//     pair ShimAllowedExceptions takes back, with ReadOnlyMessage(command)
//     plus ShimDeniedHint;
//   - otherwise execs real with --readonly prepended to the arguments.
//
// Both refusals print on stderr and exit 1 without running bd at all.
func ShimScript(real string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(ShimHeader + "\n")
	b.WriteString("# Agents read beads through this shim; every write is refused. Only the\n")
	b.WriteString("# read commands of beads.ShimAllowed reach the real binary, which runs\n")
	b.WriteString("# with --readonly; every other command, and any --readonly argument that\n")
	b.WriteString("# would switch that off, is refused here with bd's own message.\n")
	b.WriteString("_sc_cmd=''\n_sc_sub=''\n_sc_skip=0\n_sc_end=0\n_sc_bad=''\n_sc_n=0\n")
	b.WriteString("for _sc_arg in \"$@\"; do\n")
	// Everything after a bare "--" is positional for cobra/pflag, so it can
	// neither name a command nor set a flag: stop looking.
	b.WriteString("  if [ \"$_sc_end\" = 1 ]; then continue; fi\n")
	b.WriteString("  if [ \"$_sc_skip\" = 1 ]; then _sc_skip=0; continue; fi\n")
	b.WriteString("  case \"$_sc_arg\" in\n")
	b.WriteString("    --) _sc_end=1; continue ;;\n")
	b.WriteString("    " + shellQuote(ReadOnlyFlag) + "|" + shellQuote(ReadOnlyFlag) + "=*)\n")
	b.WriteString("      if [ -z \"$_sc_bad\" ]; then _sc_bad=$_sc_arg; fi\n")
	b.WriteString("      continue ;;\n")
	b.WriteString("    " + strings.Join(shimValueFlags, "|") + ") _sc_skip=1; continue ;;\n")
	b.WriteString("    -*) continue ;;\n")
	b.WriteString("    '') continue ;;\n")
	b.WriteString("  esac\n")
	// The whole argument list is scanned (no early exit) so a --readonly
	// override after the command is caught too, as bd would honour it.
	b.WriteString("  _sc_n=$((_sc_n + 1))\n")
	b.WriteString("  if [ \"$_sc_n\" = 1 ]; then _sc_cmd=$_sc_arg\n")
	b.WriteString("  elif [ \"$_sc_n\" = 2 ]; then _sc_sub=$_sc_arg\n  fi\n")
	b.WriteString("done\n")
	b.WriteString("if [ -n \"$_sc_bad\" ]; then\n")
	b.WriteString("  echo \"Error: operation '$_sc_bad' is not allowed in read-only mode\" >&2\n")
	b.WriteString("  echo " + shellQuote(ShimOverrideHint) + " >&2\n")
	b.WriteString("  exit 1\nfi\n")
	// A call with no command reaches cobra's root: help or --version.
	b.WriteString("_sc_ok=0\n")
	b.WriteString("if [ -z \"$_sc_cmd\" ]; then _sc_ok=1; fi\n")
	one, two, except, families := shimAllowLists()
	if len(one) > 0 {
		b.WriteString("case \"$_sc_cmd\" in\n  " + strings.Join(one, "|") + ") _sc_ok=1 ;;\nesac\n")
	}
	if len(two) > 0 {
		b.WriteString("case \"$_sc_cmd $_sc_sub\" in\n  " + strings.Join(two, "|") + ") _sc_ok=1 ;;\nesac\n")
	}
	if len(except) > 0 {
		b.WriteString("case \"$_sc_cmd $_sc_sub\" in\n  " + strings.Join(except, "|") + ") _sc_ok=0 ;;\nesac\n")
	}
	b.WriteString("if [ \"$_sc_ok\" = 0 ]; then\n")
	b.WriteString("  _sc_op=$_sc_cmd\n")
	if len(families) > 0 {
		b.WriteString("  case \"$_sc_cmd\" in\n")
		b.WriteString("    " + strings.Join(families, "|") + ")\n")
		b.WriteString("      if [ -n \"$_sc_sub\" ]; then _sc_op=\"$_sc_cmd $_sc_sub\"; fi ;;\n")
		b.WriteString("  esac\n")
	}
	b.WriteString("  echo \"Error: operation '$_sc_op' is not allowed in read-only mode\" >&2\n")
	b.WriteString("  echo " + shellQuote(ShimDeniedHint) + " >&2\n")
	b.WriteString("  exit 1\nfi\n")
	b.WriteString("exec " + shellQuote(real) + " --readonly \"$@\"\n")
	return b.String()
}

// ShimDirFor returns the shim directory belonging to the agent worktree at
// wt: ShimDirName beside the run's worktree tree, keyed by the worktree's
// own name, so <run-dir>/wt/<bead-id> gets <run-dir>/bin/<bead-id> (D-23).
// wt is cleaned but not required to exist.
//
// wt must have exactly that shape — an absolute path whose parent directory
// is named WorktreesDirName — or the answer would name a directory outside
// the run (climbing two levels from /home/me/proj lands on /home/bin/proj).
// Anything else fails with ErrShimLayout; a caller with its own layout
// passes ShimOptions.Dir instead.
func ShimDirFor(wt string) (string, error) {
	if wt == "" {
		return "", fmt.Errorf("%w: empty worktree path", ErrShimLayout)
	}
	clean := filepath.Clean(wt)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: worktree path %q is not absolute", ErrShimLayout, wt)
	}
	parent := filepath.Dir(clean)
	base := filepath.Base(clean)
	if parent == clean || base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("%w: worktree path %q has no name", ErrShimLayout, wt)
	}
	if filepath.Base(parent) != WorktreesDirName {
		return "", fmt.Errorf("%w: %q is not <run-dir>/%s/<bead-id>, so the shim directory would land outside the run",
			ErrShimLayout, wt, WorktreesDirName)
	}
	return filepath.Join(filepath.Dir(parent), ShimDirName, base), nil
}

// InstallForWorktree is the one call an orchestrator makes per agent: it
// writes the read-only bd shim for the worktree at wt into
// ShimDirFor(wt) (or opts.Dir), runs the Verify self-check from inside the
// worktree unless opts.SkipVerify is set, and returns the shim. Put it in
// front of the agent's PATH with Shim.Apply(spec.Env) — the cartridge
// RunSpec environment — so every `bd` the agent runs is the read-only one.
//
// wt must be an absolute path in the D-23 layout (the worktree paths
// gitx.Manager hands out are), unless opts.Dir says where the shim goes;
// see ShimDirFor. Nothing is written inside wt, so the agent's worktree
// stays clean.
func InstallForWorktree(ctx context.Context, wt string, opts ShimOptions) (Shim, error) {
	if wt == "" {
		return Shim{}, errors.New("beads: InstallForWorktree: empty worktree path")
	}
	if !filepath.IsAbs(wt) {
		return Shim{}, fmt.Errorf("beads: InstallForWorktree: worktree path %q is not absolute", wt)
	}
	dir := opts.Dir
	if dir == "" {
		var err error
		dir, err = ShimDirFor(wt)
		if err != nil {
			return Shim{}, err
		}
	}
	s, err := WriteShim(dir, ShimOptions{Real: opts.Real})
	if err != nil {
		return Shim{}, err
	}
	if opts.SkipVerify {
		return s, nil
	}
	if err := s.Verify(ctx, wt); err != nil {
		return Shim{}, err
	}
	return s, nil
}

// IsShim reports whether path is a shim generated by WriteShim (its second
// line is ShimHeader). Missing or unreadable files are not shims.
func IsShim(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	lines := strings.SplitN(string(buf[:n]), "\n", 3)
	return len(lines) >= 2 && lines[1] == ShimHeader
}

// RealBD resolves the genuine bd binary on PATH, skipping any read-only
// shim so a caller whose own PATH already carries one (a nested sc, or a
// test) still finds the real tool. The error wraps exec.ErrNotFound when
// nothing else provides bd, so proc.IsNotFound reports it.
func RealBD() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, Binary)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 || IsShim(candidate) {
			continue
		}
		if !filepath.IsAbs(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
		}
		return candidate, nil
	}
	return "", fmt.Errorf("beads: %s: %w (no real binary on PATH)", Binary, exec.ErrNotFound)
}

// PrependPath returns path with dir first, removing any earlier occurrence
// of dir so repeated calls do not grow it. An empty path yields dir alone.
func PrependPath(dir, path string) string {
	parts := []string{dir}
	for _, p := range filepath.SplitList(path) {
		if p == "" || p == dir {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// PathEnv returns the parent's PATH with the shim directory first: the value
// to give the agent process.
func (s Shim) PathEnv() string {
	return PrependPath(s.Dir, os.Getenv("PATH"))
}

// Apply sets PATH in env (a cartridge RunSpec.Env map, for example) to
// PathEnv, prepending to a PATH already present in env rather than to the
// parent's. It does nothing on a nil map.
func (s Shim) Apply(env map[string]string) {
	if env == nil {
		return
	}
	if current, ok := env["PATH"]; ok {
		env["PATH"] = PrependPath(s.Dir, current)
		return
	}
	env["PATH"] = s.PathEnv()
}

// shimReadProbes are the reads Verify runs through the shim: the commands
// P3-09 requires to keep working for an agent. They must not be refused —
// a shim that denied everything (an over-broad ShimAllowed edit, a chained
// wrapper) would otherwise pass the self-check and only fail once an agent
// was running.
var shimReadProbes = [][]string{
	{"ready", "--json", "-n", "0"},
	{"show", shimProbeBead, "--json"},
}

// shimProbes are the refusals Verify checks: the write an agent is most
// likely to attempt, one command per family bd 1.2.2 executes anyway under
// --readonly (so the allow-list, not bd, must stop them), and an attempt to
// switch --readonly off. Every probe uses an argument form that would be
// harmless if the shim ever let it through (the write probes name a bead id
// that does not exist and the rest are bare commands that need arguments),
// so verification can never change a database.
var shimProbes = []struct {
	// op is the operation name the shim must report.
	op string
	// args is what to run through the shim.
	args []string
}{
	{op: "update", args: []string{"update", shimProbeBead, "-s", "open"}},
	{op: "config set", args: []string{"config", "set"}},
	{op: "config set-many", args: []string{"config", "set-many"}},
	{op: "backup", args: []string{"backup"}},
	{op: "federation", args: []string{"federation"}},
	{op: "audit", args: []string{"audit"}},
	{op: "import", args: []string{"import"}},
	{op: "branch", args: []string{"branch"}},
	{op: "vc", args: []string{"vc", "status"}},
	{op: "comments add", args: []string{"comments", "add"}},
	{op: ReadOnlyFlag + "=false", args: []string{ReadOnlyFlag + "=false", "update", shimProbeBead, "-s", "open"}},
}

// Verify executes the shim the way an agent would, from dir (the worktree,
// or any directory inside the repository), and fails on the first problem
// so callers can log it and abort the agent before it starts:
//
//   - `bd version --json` must succeed through the shim;
//   - every shimReadProbes entry (`bd ready`, `bd show`) must reach bd —
//     the shim may not refuse a read;
//   - every shimProbes entry — a write, one command per family bd runs
//     anyway under --readonly, and an attempt to switch --readonly off —
//     must be refused by the shim, naming that operation;
//   - the real binary must still enforce --readonly itself, checked by
//     running it directly (the shim refuses the write before bd sees it),
//     so the second line of defence D-21 relies on is proven too.
//
// Only the version, read and bd-side probes start bd; the rest are refused
// by the script itself, so the check stays cheap enough to run before every
// agent.
func (s Shim) Verify(ctx context.Context, dir string) error {
	res, err := proc.Run(ctx, s.spec(dir, "version", "--json"))
	if err != nil {
		return fmt.Errorf("beads: shim %s cannot run bd: %w", s.Path, err)
	}
	var probe struct {
		Version string `json:"version"`
	}
	if err := Decode(res.Stdout, &probe); err != nil || probe.Version == "" {
		return fmt.Errorf("beads: shim %s: bd version printed no version: %q", s.Path, strings.TrimSpace(res.Stdout))
	}
	for _, args := range shimReadProbes {
		if err := s.verifyAllowed(ctx, dir, args...); err != nil {
			return err
		}
	}
	for _, p := range shimProbes {
		if _, err := s.verifyRefused(ctx, dir, p.op, p.args...); err != nil {
			return err
		}
	}
	return s.verifyBDReadOnly(ctx, dir)
}

// verifyAllowed runs one read probe through the shim and requires that the
// shim did not refuse it. bd's own failures are accepted — dir may hold no
// beads database yet, and the probe's bead id does not exist — because this
// checks the shim, not the project; anything the shim itself refuses comes
// back as a read-only message.
func (s Shim) verifyAllowed(ctx context.Context, dir string, args ...string) error {
	shown := Binary + " " + strings.Join(args, " ")
	_, err := proc.Run(ctx, s.spec(dir, args...))
	if err == nil {
		return nil
	}
	var exit *proc.ExitError
	if !errors.As(err, &exit) || exit.Cause != nil {
		return fmt.Errorf("beads: shim %s: running `%s`: %w", s.Path, shown, err)
	}
	if msg := StderrMessage(exit.Result.Stderr); errors.Is(KindOf("", msg), ErrReadOnly) {
		return fmt.Errorf("beads: shim %s: refuses the read `%s`: %s (agents must be able to read beads)", s.Path, shown, msg)
	}
	return nil
}

// verifyRefused runs one probe through the shim and requires it to fail
// with a read-only refusal. When op is not empty the message must be
// exactly ReadOnlyMessage(op), which only the shim's own refusals produce;
// with an empty op any ErrReadOnly message (bd's own) is accepted. It
// returns the message so callers can log it.
func (s Shim) verifyRefused(ctx context.Context, dir, op string, args ...string) (string, error) {
	return s.refused(ctx, s.spec(dir, args...), Binary+" "+strings.Join(args, " "), op)
}

// verifyBDReadOnly checks the guarantee underneath the shim: the real
// binary refuses a write of its own accord when it is given --readonly.
// The shim's allow-list stops such a call before bd ever runs, so this
// probe invokes the real binary directly, with the same environment an
// agent would have. The bead it names does not exist.
func (s Shim) verifyBDReadOnly(ctx context.Context, dir string) error {
	args := []string{"update", shimProbeBead, "-s", "open"}
	sp := Spec(CallOptions{Dir: dir, Timeout: ShimVerifyTimeout, ReadOnly: true}, args...)
	sp.Command = s.Real
	sp.Env["PATH"] = s.PathEnv()
	shown := Binary + " " + ReadOnlyFlag + " " + strings.Join(args, " ")
	if _, err := s.refused(ctx, sp, shown, ""); err != nil {
		return fmt.Errorf("%w (%s does not enforce %s itself)", err, s.Real, ReadOnlyFlag)
	}
	return nil
}

// refused runs sp and requires it to fail with a read-only refusal, shown
// as the command line shown. When op is not empty the message must be
// exactly ReadOnlyMessage(op), which only the shim's own refusals produce;
// with an empty op any ErrReadOnly message (bd's own) is accepted. It
// returns the message so callers can log it.
func (s Shim) refused(ctx context.Context, sp proc.Spec, shown, op string) (string, error) {
	_, err := proc.Run(ctx, sp)
	if err == nil {
		return "", fmt.Errorf("beads: shim %s: `%s` was not refused", s.Path, shown)
	}
	var exit *proc.ExitError
	if !errors.As(err, &exit) || exit.Cause != nil {
		return "", fmt.Errorf("beads: shim %s: running `%s`: %w", s.Path, shown, err)
	}
	msg := StderrMessage(exit.Result.Stderr)
	if !errors.Is(KindOf("", msg), ErrReadOnly) {
		return "", fmt.Errorf("beads: shim %s: `%s` failed for another reason: %s", s.Path, shown, ErrorSummary(err))
	}
	if op != "" && msg != ReadOnlyMessage(op) {
		return "", fmt.Errorf("beads: shim %s: `%s` was refused as %q, want %q", s.Path, shown, msg, ReadOnlyMessage(op))
	}
	return msg, nil
}

// spec builds the proc.Spec that runs the shim script itself (by path, not
// `bd` from PATH) with the D-21 environment and the shim directory first on
// PATH, as an agent process would see it.
func (s Shim) spec(dir string, args ...string) proc.Spec {
	sp := Spec(CallOptions{Dir: dir, Timeout: ShimVerifyTimeout}, args...)
	sp.Command = s.Path
	sp.Env["PATH"] = s.PathEnv()
	return sp
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping embedded
// single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
