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
// both.

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
// --readonly although they change state (verified live: `config set` and
// `config unset` write the database, `import` inserts issues, `metrics`
// changes global settings, `dolt` commits and pushes, `init`, `restore`,
// `hooks` and `setup` touch the repository, `rename` rewrites ids). The
// shim refuses them with ReadOnlyMessage before bd runs. One-word entries
// cover every subcommand of that command; two-word entries only that pair,
// so `config get` and `config list` stay available.
var ShimDenied = []string{
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
}

// shimValueFlags are bd's global flags that take a separate value, so the
// shim skips the value when looking for the subcommand.
var shimValueFlags = []string{"-C", "--directory", "--actor", "--db", "--dolt-auto-commit"}

// PromptNote is the sentence every role prompt carries about beads (P3-09
// "add a line to every role prompt"; consumed by the Phase 5 prompt
// builders).
const PromptNote = "You may read the beads task tracker if it helps (`bd ready`, `bd show <id>`, " +
	"`bd list -n 0 --flat`, `bd blocked`, `bd dep list <id>`), but never write to it: " +
	"the `bd` on your PATH is read-only and refuses every write. The cannon records your results for you."

// ShimVerifyTimeout bounds each of the two bd calls Shim.Verify makes.
const ShimVerifyTimeout = DefaultTimeout

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

// ShimOptions customises WriteShim.
type ShimOptions struct {
	// Real is the path of the genuine bd binary. Empty means RealBD().
	Real string
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

// ShimScript returns the shell script of a shim that execs real. The script
// finds the bd subcommand (skipping global flags and their values), refuses
// every ShimDenied command with ReadOnlyMessage on stderr and exit status 1,
// and otherwise execs real with --readonly prepended to the arguments.
func ShimScript(real string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(ShimHeader + "\n")
	b.WriteString("# Agents read beads through this shim; every write is refused. The real\n")
	b.WriteString("# binary runs with --readonly; the commands bd does not block in that\n")
	b.WriteString("# mode are refused here with the same message.\n")
	b.WriteString("_sc_cmd=''\n_sc_sub=''\n_sc_skip=0\n")
	b.WriteString("for _sc_arg in \"$@\"; do\n")
	b.WriteString("  if [ \"$_sc_skip\" = 1 ]; then _sc_skip=0; continue; fi\n")
	b.WriteString("  case \"$_sc_arg\" in\n")
	b.WriteString("    " + strings.Join(shimValueFlags, "|") + ") _sc_skip=1; continue ;;\n")
	b.WriteString("    -*) continue ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  if [ -z \"$_sc_cmd\" ]; then _sc_cmd=$_sc_arg; else _sc_sub=$_sc_arg; break; fi\n")
	b.WriteString("done\n")
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

// Verify executes the shim the way an agent would, from dir (the worktree,
// or any directory inside the repository): `bd version --json` must succeed
// through it, and `bd update <probe> -s open` must be refused with
// ReadOnlyMessage. It returns the first failure so callers can log it and
// abort the agent before it starts.
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
	_, err = proc.Run(ctx, s.spec(dir, "update", "sc-shim-probe", "-s", "open"))
	if err == nil {
		return fmt.Errorf("beads: shim %s: `bd update` was not refused", s.Path)
	}
	var exit *proc.ExitError
	if !errors.As(err, &exit) || exit.Cause != nil {
		return fmt.Errorf("beads: shim %s: running `bd update`: %w", s.Path, err)
	}
	if !errors.Is(KindOf("", StderrMessage(exit.Result.Stderr)), ErrReadOnly) {
		return fmt.Errorf("beads: shim %s: `bd update` failed for another reason: %s", s.Path, ErrorSummary(err))
	}
	return nil
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
