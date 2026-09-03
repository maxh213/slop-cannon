package conformance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

// CheckBeads is the read-only shim check: with the bd shim (P3-09) first on the session's
// PATH, `bd update` inside a read_exec session is refused with bd's
// read-only message while `bd ready` still works. Skipped when bd or git is
// not installed, when bd is older than beads.MinVersion (its CLI differs, so
// the check would fail for an environment reason), or when the platform has
// no POSIX shell.
const CheckBeads = "beads-readonly"

// beadsMarker starts the beads check's prompt (MockScenario matches on it).
const beadsMarker = "Run the shell command `bd update sc-shim-probe -s open`"

// beadsPrefix is the beads prefix of the throwaway project the check
// initialises in its workdir.
const beadsPrefix = "sc"

// beadsSetupTimeout bounds `git init` in the check's workdir.
const beadsSetupTimeout = 30 * time.Second

// Bounds for retryTextBusy: how many times a freshly written executable is
// re-run when the kernel reports it busy, and the pause between attempts.
const (
	textBusyAttempts = 20
	textBusyBackoff  = 10 * time.Millisecond
)

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
// looks at --readonly otherwise), after checking that bd is new enough to
// have the CLI this suite drives, installs the shim under the suite's base
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
	if err := beadsVersionOK(ctx, real); err != nil {
		return "", cartridge.Usage{}, err
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
	if err := retryTextBusy(ctx, func() error { return shim.Verify(ctx, spec.Workdir) }); err != nil {
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

// beadsVersionOK skips the check unless real satisfies beads.MinVersion.
// The check drives the bd CLI directly (`bd init` with the D-13 flags, then
// the shim), and an older bd has a different CLI — 1.0.5 has no
// `--init-if-missing`, so beadsProject would fail with "unknown flag" —
// which is an environment problem, not a cartridge failure, so it must not
// be reported as one. It mirrors beads.Detect, which cannot be used here
// because it always resolves bd on PATH while the check honours Options.BD.
func beadsVersionOK(ctx context.Context, real string) error {
	spec := beads.Spec(beads.CallOptions{Timeout: beadsSetupTimeout}, "version", "--json")
	spec.Command = real
	res, err := proc.Run(ctx, spec)
	if err != nil {
		return skipf("bd version failed (%s): %s", real, beads.ErrorSummary(err))
	}
	var reported struct {
		Version string `json:"version"`
	}
	if err := beads.Decode(res.Stdout, &reported); err != nil {
		return skipf("bd version printed no version (%s): %v", real, err)
	}
	v, ok := proc.ParseVersion(reported.Version)
	if !ok {
		return skipf("bd reported version %q (%s)", reported.Version, real)
	}
	if !v.AtLeast(beads.MinVersion) {
		return skipf("bd %s is older than %s (%s); %s", v, beads.MinVersion, real, beads.InstallHint)
	}
	return nil
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

// retryTextBusy runs fn again, after a short pause, while it fails with
// ETXTBSY. Executing a file that was written moments ago races with any
// concurrent fork in this process (golang/go#22315): the child inherits
// the writer's descriptor until it execs, and until then the kernel
// refuses to run the file. The window is microseconds long, so a few
// attempts always suffice; any other error is returned at once.
func retryTextBusy(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < textBusyAttempts; i++ {
		if err = fn(); !isTextBusy(err) {
			return err
		}
		select {
		case <-time.After(textBusyBackoff):
		case <-ctx.Done():
			return err
		}
	}
	return err
}

// isTextBusy reports whether err is ETXTBSY, either directly (the shim
// itself could not be executed) or as the shell's "Text file busy"
// complaint about the bd it chains to.
func isTextBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ETXTBSY) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "text file busy")
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
