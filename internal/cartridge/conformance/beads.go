package conformance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

// CheckBeads is the read-only shim check: with the bd shim (P3-09) first on the session's
// PATH, `bd update` inside a read_exec session is refused with bd's
// read-only message while `bd ready` still works. Skipped when bd or git is
// not installed or the platform has no POSIX shell.
const CheckBeads = "beads-readonly"

// beadsMarker starts the beads check's prompt (MockScenario matches on it).
const beadsMarker = "Run the shell command `bd update sc-shim-probe -s open`"

// beadsPrefix is the beads prefix of the throwaway project the check
// initialises in its workdir.
const beadsPrefix = "sc"

// beadsSetupTimeout bounds `git init` in the check's workdir.
const beadsSetupTimeout = 30 * time.Second

// BeadsPrompt is the beads check's prompt: run a write and a read through
// bd and report both outputs so the refusal shows up in the transcript.
func BeadsPrompt() string {
	return fmt.Sprintf("%s, then the shell command `bd ready --json`, and reply with the complete output of both commands verbatim, including any error lines. Do not try to work around errors and do nothing else.", beadsMarker)
}

// skipError makes a check report StatusSkip instead of StatusFail.
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

func skipf(format string, args ...any) error {
	return &skipError{reason: fmt.Sprintf(format, args...)}
}

// runBeads is CheckBeads. It initialises a beads project in the check's
// workdir (bd refuses everything with "no beads database found" before it
// looks at --readonly otherwise), installs the shim under the suite's base
// directory, verifies the shim chain directly, and then fires a read_exec
// session with the shim first on PATH. The session must surface
// beads.ReadOnlyMessage("update") in a tool result or its final output.
func runBeads(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	if runtime.GOOS == "windows" {
		return "", cartridge.Usage{}, skipf("the read-only bd shim needs a POSIX shell")
	}
	real := e.opts.BD
	if real == "" {
		var err error
		real, err = beads.RealBD()
		if err != nil {
			return "", cartridge.Usage{}, skipf("bd is not installed: %v", err)
		}
	}
	if _, err := proc.LookPath("git"); err != nil {
		return "", cartridge.Usage{}, skipf("git is not installed: %v", err)
	}

	spec, err := e.spec(CheckBeads, cartridge.ToolReadExec, BeadsPrompt())
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	if err := beadsProject(ctx, real, spec.Workdir); err != nil {
		return "", cartridge.Usage{}, err
	}
	shim, err := beads.WriteShim(filepath.Join(e.base, CheckBeads+"-bin"), beads.ShimOptions{Real: real})
	if err != nil {
		return "", cartridge.Usage{}, fmt.Errorf("install the read-only bd shim: %w", err)
	}
	if err := shim.Verify(ctx, spec.Workdir); err != nil {
		return "", cartridge.Usage{}, fmt.Errorf("the shim chain does not hold before any session ran: %w", err)
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	shim.Apply(spec.Env)

	o, err := e.run(ctx, CheckBeads, spec, nil)
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("%d events, %s", len(o.events), describeUsage(o.res.Usage))
	if o.err != nil {
		return detail, o.res.Usage, fmt.Errorf("session failed: %w", o.err)
	}
	want := beads.ReadOnlyMessage("update")
	if !transcriptContains(o, want) {
		return detail, o.res.Usage, fmt.Errorf("the session never reported the read-only refusal %q (output: %q)", want, o.res.Output)
	}
	return "bd update refused through the shim; " + detail, o.res.Usage, nil
}

// beadsProject turns dir into a git repository with an initialised beads
// database, using real as the bd binary. Nothing is committed (D-13 flags).
func beadsProject(ctx context.Context, real, dir string) error {
	git := proc.Spec{Command: "git", Args: []string{"init", "-q"}, Dir: dir, Timeout: beadsSetupTimeout}
	if _, err := proc.Run(ctx, git); err != nil {
		return fmt.Errorf("git init in the workdir: %w", err)
	}
	init := beads.Spec(beads.CallOptions{Dir: dir, Timeout: beads.InitTimeout}, beads.InitArgs(beadsPrefix)...)
	init.Command = real
	if _, err := proc.Run(ctx, init); err != nil {
		return fmt.Errorf("bd init in the workdir: %s", beads.ErrorSummary(err))
	}
	return nil
}

// transcriptContains reports whether any tool result or the final output
// of the session contains text.
func transcriptContains(o outcome, text string) bool {
	if strings.Contains(o.res.Output, text) {
		return true
	}
	for _, ev := range o.events {
		if ev.Kind == cartridge.EventToolResult && strings.Contains(ev.Output, text) {
			return true
		}
		if ev.Kind == cartridge.EventTextDelta && strings.Contains(ev.Text, text) {
			return true
		}
	}
	return false
}

// isSkip reports whether err asks for StatusSkip.
func isSkip(err error) (string, bool) {
	var se *skipError
	if errors.As(err, &se) {
		return se.reason, true
	}
	return "", false
}
