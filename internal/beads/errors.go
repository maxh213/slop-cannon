package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"slop-cannon/internal/proc"
)

// Typed errors a Client call can return, matched with errors.Is. They are
// derived from bd's exit status plus the message it printed, because bd
// 1.2.2 does not emit the `{"error","code","hint"}` envelope D-21
// describes (P3-12): `bd show nope-1` prints `{"data":{"error":…}}`, while
// `bd close` refusals, `--readonly` refusals and claim conflicts print a
// single stderr line with an empty stdout. The `code` path is kept for
// future bd versions (see codeKinds).
var (
	// ErrNotFound: no issue matches the id (show, update, close, comment,
	// a missing --parent). Show returns it alongside the issues it did find.
	ErrNotFound = errors.New("bead not found")
	// ErrBlockedOpen: `bd close` refused because blockers are still open
	// (D-20); the message names them.
	ErrBlockedOpen = errors.New("bead is blocked by open issues")
	// ErrReadOnly: the write was refused under `--readonly` (P3-09).
	ErrReadOnly = errors.New("bd is in read-only mode")
	// ErrLocked: the database lock could not be taken. bd 1.2.2 waits for
	// the embedded Dolt lock instead of failing (D-21), so in practice
	// contention shows up as a timeout; the kind is kept for lock messages.
	ErrLocked = errors.New("beads database is locked")
	// ErrAlreadyClaimed: `bd update --claim` lost the race to another actor.
	ErrAlreadyClaimed = errors.New("bead is already claimed by another actor")
	// ErrCycle: `bd dep add` would create a dependency cycle.
	ErrCycle = errors.New("dependency would create a cycle")
	// ErrNoProject: the repository has no .beads/ (run EnsureInitialised).
	ErrNoProject = errors.New("no beads project found")
	// ErrInvalid: bd rejected the input (unknown status or type, priority
	// out of range, empty title or comment, missing id).
	ErrInvalid = errors.New("bd rejected the request")
)

// Error is the error a Client call returns when bd ran but failed. It
// wraps the typed kind (errors.Is(err, ErrNotFound)) and the underlying
// *proc.ExitError (errors.Is(err, proc.ErrTimeout), errors.As for the
// full Result), and carries the message bd printed.
type Error struct {
	// Command is the bd subcommand ("close", "dep add").
	Command string
	// Args is the full argument list that was run.
	Args []string
	// Kind is the typed error, or nil when bd's message matched nothing.
	Kind error
	// Message is what bd said: the envelope's error string or the stderr
	// line, with any "Error:" prefix removed.
	Message string
	// Code and Hint are the envelope fields, when a future bd prints them.
	Code string
	Hint string
	// ExitCode is bd's exit status (-1 when killed).
	ExitCode int
	// Stdout and Stderr are the captured output.
	Stdout string
	Stderr string
	// Cause is the underlying error (a *proc.ExitError, or the error that
	// stopped bd from starting).
	Cause error
}

// Error renders "bd <command>: <message>", falling back to the exit status
// or "timed out".
func (e *Error) Error() string {
	head := Binary
	if e.Command != "" {
		head += " " + e.Command
	}
	switch {
	case e.Message != "":
		return head + ": " + e.Message
	case errors.Is(e.Cause, proc.ErrTimeout):
		return head + ": timed out"
	case e.ExitCode >= 0 && e.Cause != nil:
		return fmt.Sprintf("%s: exit status %d", head, e.ExitCode)
	case e.Cause != nil:
		return head + ": " + e.Cause.Error()
	default:
		return head + ": failed"
	}
}

// Unwrap exposes the kind and the cause to errors.Is and errors.As.
func (e *Error) Unwrap() []error {
	var out []error
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	return out
}

// codeKinds maps the envelope `code` values a future bd may print to the
// typed errors. bd 1.2.2 prints none; the names follow its messages.
var codeKinds = map[string]error{
	"not_found":       ErrNotFound,
	"blocked":         ErrBlockedOpen,
	"blocked_open":    ErrBlockedOpen,
	"readonly":        ErrReadOnly,
	"read_only":       ErrReadOnly,
	"locked":          ErrLocked,
	"already_claimed": ErrAlreadyClaimed,
	"claimed":         ErrAlreadyClaimed,
	"cycle":           ErrCycle,
	"no_project":      ErrNoProject,
	"invalid":         ErrInvalid,
	"validation":      ErrInvalid,
}

// messageKinds maps substrings of bd 1.2.2's messages (lower-cased) to the
// typed errors, most specific first.
var messageKinds = []struct {
	needle string
	kind   error
}{
	{"blocked by open issues", ErrBlockedOpen},
	{"read-only mode", ErrReadOnly},
	{"already claimed", ErrAlreadyClaimed},
	{"would create a cycle", ErrCycle},
	{"no beads project found", ErrNoProject},
	{"no issue found matching", ErrNotFound},
	{"no issues found matching", ErrNotFound},
	{"not found", ErrNotFound},
	{"database is locked", ErrLocked},
	{"could not acquire lock", ErrLocked},
	{"unable to acquire lock", ErrLocked},
	{"resource temporarily unavailable", ErrLocked},
	{"lock file", ErrLocked},
	{"invalid", ErrInvalid},
	{"validation failed", ErrInvalid},
	{"is required", ErrInvalid},
	{"cannot be empty", ErrInvalid},
	{"at least one", ErrInvalid},
	{"unknown flag", ErrInvalid},
	{"unknown command", ErrInvalid},
}

// KindOf returns the typed error for a bd message and optional envelope
// code: the code wins when it is known, else the first message pattern
// that matches, else nil.
func KindOf(code, message string) error {
	if k, ok := codeKinds[strings.ToLower(strings.TrimSpace(code))]; ok {
		return k
	}
	lower := strings.ToLower(message)
	for _, m := range messageKinds {
		if strings.Contains(lower, m.needle) {
			return m.kind
		}
	}
	return nil
}

// classify turns the outcome of a bd run into nil or an *Error. A failure
// to start bd (not on PATH) is returned wrapped so proc.IsNotFound still
// holds.
func classify(args []string, res proc.Result, err error) error {
	if err == nil {
		return nil
	}
	e := &Error{Command: commandName(args), Args: args, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Cause: err}
	var exit *proc.ExitError
	if !errors.As(err, &exit) {
		e.ExitCode = -1
		if proc.IsNotFound(err) {
			e.Message = "not found on PATH"
		} else {
			e.Message = err.Error()
		}
		return e
	}
	if errors.Is(err, proc.ErrTimeout) {
		return e
	}
	e.Message, e.Code, e.Hint = ErrorMessage(res.Stdout, res.Stderr)
	e.Kind = KindOf(e.Code, e.Message)
	return e
}

// ErrorMessage extracts what bd said about a failure: the envelope's
// error (top-level `error`/`code`/`hint`, or bd 1.2.2's `data.error`) when
// stdout holds one, else the stderr line (see StderrMessage).
func ErrorMessage(stdout, stderr string) (message, code, hint string) {
	if env, err := ParseEnvelope(stdout); err == nil {
		if env.Error != "" {
			return env.Error, env.Code, env.Hint
		}
		var data struct {
			Error string `json:"error"`
			Code  string `json:"code"`
			Hint  string `json:"hint"`
		}
		if len(env.Data) > 0 && json.Unmarshal(env.Data, &data) == nil && data.Error != "" {
			return data.Error, data.Code, data.Hint
		}
	}
	return StderrMessage(stderr), "", ""
}

// StderrMessage picks the line that explains a failure out of bd's stderr:
// the first line starting with "Error" (with a leading "Error:" removed),
// else the first line that is not part of a notice. Notices are the
// one-time metrics text and the "warning: …" blocks bd prints before the
// real message, each a first line followed by indented continuation lines.
func StderrMessage(stderr string) string {
	fallback := ""
	inNotice := false
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "warning:") || strings.HasPrefix(lower, "thanks for using bd") {
			inNotice = true
			continue
		}
		if inNotice && (line[0] == ' ' || line[0] == '\t') {
			continue
		}
		inNotice = false
		if strings.HasPrefix(lower, "error") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "Error:"), "error:"))
		}
		if fallback == "" {
			fallback = trimmed
		}
	}
	return fallback
}

// commandName returns the bd subcommand in args ("close", "dep add"),
// skipping global flags and their values.
func commandName(args []string) string {
	var words []string
	skip := false
	for _, a := range args {
		switch {
		case skip:
			skip = false
		case a == "-C" || a == "--directory" || a == "--actor" || a == "--db":
			skip = true
		case strings.HasPrefix(a, "-"):
		default:
			words = append(words, a)
			if len(words) == 1 && !groupCommands[a] {
				return a
			}
			if len(words) == 2 {
				return strings.Join(words, " ")
			}
		}
	}
	return strings.Join(words, " ")
}

// groupCommands are bd commands whose first positional is a subcommand.
var groupCommands = map[string]bool{
	"dep": true, "deps": true, "config": true, "gate": true, "label": true,
	"merge-slot": true, "swarm": true, "epic": true, "metrics": true,
	"worktree": true, "dolt": true,
}
