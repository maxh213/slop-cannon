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
// so bd itself refuses every write ("operation 'update' is not allowed in
// read-only mode") while `bd ready`, `bd show`, `bd list` and friends keep
// working. The subcommands bd 1.2.2 does not gate behind --readonly
// (ShimDenied, verified live) are refused by the shim with the same message
// shape, so the client's error classification (ErrReadOnly) applies to
// both. Because bd's --readonly is a plain pflag bool that a later
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

// ShimDenied lists the bd subcommands that bd 1.2.2 still executes under
// --readonly although they change state (all verified live against bd
// 1.2.2: `config set` and `config unset` write the database, `import`
// inserts issues, `metrics` changes global settings, `dolt` commits and
// pushes, `init`, `restore`, `hooks` and `setup` touch the repository,
// `rename` rewrites ids, and the two Dolt-level families found in the
// P3-09 review on 2026-09-03 — `branch <name>` creates a Dolt branch and
// `vc commit`/`vc merge` commit and merge one, all exiting 0 under
// --readonly). The shim refuses them with ReadOnlyMessage before bd runs.
// One-word entries cover every subcommand of that command; two-word
// entries only that pair, so `config get` and `config list` stay
// available. `branch` and `vc` are denied whole: their read-only forms
// (`bd branch` listing, `bd vc status`) are of no use to an agent and
// telling them apart from `bd branch <name>` would need bd's own flag
// parser.
var ShimDenied = []string{
	"branch",
	"config set",
	"config unset",
	"dolt",
	"hooks",
	"import",
	"init",
	"metrics",
	"rename",
	"restore",
	"setup",
	"vc",
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

// ShimScript returns the shell script of a shim that execs real. The
// script scans every argument the way cobra's own flag stripping does
// (skipping global flags and the values of shimValueFlags, stopping at a
// bare "--", which makes everything after it positional and inert), then:
//
//   - refuses any --readonly argument of the caller's with
//     ReadOnlyMessage(argument) plus ShimOverrideHint, because
//     `--readonly=false` would otherwise win over the flag the shim
//     passes;
//   - refuses every ShimDenied command with ReadOnlyMessage(command);
//   - otherwise execs real with --readonly prepended to the arguments.
//
// Both refusals print on stderr and exit 1 without running bd at all.
func ShimScript(real string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(ShimHeader + "\n")
	b.WriteString("# Agents read beads through this shim; every write is refused. The real\n")
	b.WriteString("# binary runs with --readonly; the commands bd does not block in that\n")
	b.WriteString("# mode, and any --readonly argument that would switch it off, are\n")
	b.WriteString("# refused here with the same message.\n")
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
	b.WriteString("_sc_op=''\n")
	var one, two []string
	for _, d := range ShimDenied {
		if strings.Contains(d, " ") {
			two = append(two, shellQuote(d))
		} else {
			one = append(one, shellQuote(d))
		}
	}
	if len(one) > 0 {
		b.WriteString("case \"$_sc_cmd\" in\n  " + strings.Join(one, "|") + ") _sc_op=$_sc_cmd ;;\nesac\n")
	}
	if len(two) > 0 {
		b.WriteString("case \"$_sc_cmd $_sc_sub\" in\n  " + strings.Join(two, "|") + ") _sc_op=\"$_sc_cmd $_sc_sub\" ;;\nesac\n")
	}
	b.WriteString("if [ -n \"$_sc_op\" ]; then\n")
	b.WriteString("  echo \"Error: operation '$_sc_op' is not allowed in read-only mode\" >&2\n")
	b.WriteString("  exit 1\nfi\n")
	b.WriteString("exec " + shellQuote(real) + " --readonly \"$@\"\n")
	return b.String()
}

// ShimDirFor returns the shim directory belonging to the agent worktree at
// wt: ShimDirName beside the worktree tree, keyed by the worktree's own
// name, so <run-dir>/wt/<bead-id> gets <run-dir>/bin/<bead-id> (D-23).
// wt is cleaned but not required to exist.
func ShimDirFor(wt string) string {
	wt = filepath.Clean(wt)
	return filepath.Join(filepath.Dir(filepath.Dir(wt)), ShimDirName, filepath.Base(wt))
}

// InstallForWorktree is the one call an orchestrator makes per agent: it
// writes the read-only bd shim for the worktree at wt into
// ShimDirFor(wt) (or opts.Dir), runs the Verify self-check from inside the
// worktree unless opts.SkipVerify is set, and returns the shim. Put it in
// front of the agent's PATH with Shim.Apply(spec.Env) — the cartridge
// RunSpec environment — so every `bd` the agent runs is the read-only one.
//
// wt must be an absolute path (the worktree paths gitx.Manager hands out
// are). Nothing is written inside wt, so the agent's worktree stays clean.
func InstallForWorktree(ctx context.Context, wt string, opts ShimOptions) (Shim, error) {
	if wt == "" {
		return Shim{}, errors.New("beads: InstallForWorktree: empty worktree path")
	}
	if !filepath.IsAbs(wt) {
		return Shim{}, fmt.Errorf("beads: InstallForWorktree: worktree path %q is not absolute", wt)
	}
	dir := opts.Dir
	if dir == "" {
		dir = ShimDirFor(wt)
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

// shimProbes are the refusals Verify checks after bd's own: one per
// mechanism the shim adds on top of --readonly. Every probe uses an
// argument form that would be harmless if the shim ever let it through
// (`bd config set` without a key errors, `bd branch` only lists, `bd vc
// status` only reports, and the override probe names a bead id that does
// not exist), so verification can never change a database.
var shimProbes = []struct {
	// op is the operation name the shim must report.
	op string
	// args is what to run through the shim.
	args []string
}{
	{op: "config set", args: []string{"config", "set"}},
	{op: "branch", args: []string{"branch"}},
	{op: "vc", args: []string{"vc", "status"}},
	{op: ReadOnlyFlag + "=false", args: []string{ReadOnlyFlag + "=false", "update", "sc-shim-probe", "-s", "open"}},
}

// Verify executes the shim the way an agent would, from dir (the worktree,
// or any directory inside the repository), and fails on the first problem
// so callers can log it and abort the agent before it starts:
//
//   - `bd version --json` must succeed through the shim;
//   - `bd update <probe> -s open` must be refused by bd itself with
//     ReadOnlyMessage;
//   - every shimProbes entry — the commands bd 1.2.2 runs anyway under
//     --readonly (`config set`, `branch`, `vc`) and an attempt to switch
//     --readonly off — must be refused by the shim, naming that operation.
//
// Only the first two probes start bd; the rest are refused by the script
// itself, so the check stays cheap enough to run before every agent.
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
	if _, err := s.verifyRefused(ctx, dir, "", "update", "sc-shim-probe", "-s", "open"); err != nil {
		return err
	}
	for _, p := range shimProbes {
		if _, err := s.verifyRefused(ctx, dir, p.op, p.args...); err != nil {
			return err
		}
	}
	return nil
}

// verifyRefused runs one probe through the shim and requires it to fail
// with a read-only refusal. When op is not empty the message must be
// exactly ReadOnlyMessage(op), which only the shim's own refusals produce;
// with an empty op any ErrReadOnly message (bd's own) is accepted. It
// returns the message so callers can log it.
func (s Shim) verifyRefused(ctx context.Context, dir, op string, args ...string) (string, error) {
	shown := Binary + " " + strings.Join(args, " ")
	_, err := proc.Run(ctx, s.spec(dir, args...))
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
