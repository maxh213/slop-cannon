package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"slop-cannon/internal/proc"
)

// Binary is the name of the beads CLI looked up on PATH.
const Binary = "bd"

// DefaultActor is the BEADS_ACTOR used when a call does not name one. The
// orchestrator and agents pass "sc:<id>" (D-21); plumbing calls such as
// Detect and EnsureInitialised use this value.
const DefaultActor = "sc"

// DefaultTimeout bounds one bd call when CallOptions.Timeout is zero. bd
// answers in 0.15-0.2 s (Appendix A); the margin covers a cold embedded
// Dolt engine and lock contention with a human running bd by hand.
const DefaultTimeout = 30 * time.Second

// InheritEnv lists the parent variables a bd subprocess may see, in addition
// to PATH: enough for bd to find its own configuration and the database,
// nothing that could carry a token into it.
var InheritEnv = []string{
	"HOME",
	"XDG_CONFIG_HOME",
	"XDG_STATE_HOME",
	"XDG_DATA_HOME",
	"XDG_CACHE_HOME",
	"BEADS_DIR",
}

// CallOptions describes how one bd invocation is set up. The zero value
// runs bd in the current directory as DefaultActor with DefaultTimeout.
type CallOptions struct {
	// Repo, when set, is passed as `-C <repo>` before the command so bd
	// resolves the workspace from there. It is the D-21 default for every
	// call except `bd init`, which refuses -C on a directory that has no
	// beads project yet and must run with Dir instead.
	Repo string
	// Dir is the working directory of the subprocess. Empty means the
	// parent's cwd.
	Dir string
	// Actor is the BEADS_ACTOR value ("sc:<agent-id>"). Empty means
	// DefaultActor.
	Actor string
	// Timeout kills the call when it elapses. Zero means DefaultTimeout.
	Timeout time.Duration
	// Stdin is connected to bd's standard input (for `bd comment --stdin`).
	Stdin io.Reader
	// ReadOnly prepends the global --readonly flag so any write is refused
	// (used by the agent shim, P3-09).
	ReadOnly bool
}

// Spec builds the proc.Spec for `bd <args>` under the D-21 contract:
// BD_JSON_ENVELOPE=1 and BD_NON_INTERACTIVE=1 are always set, BEADS_ACTOR
// names the caller, only InheritEnv (plus PATH) leaks from the parent, and
// `-C <repo>` precedes the command when opts.Repo is set. Callers add
// --json and the listing flags (-n 0 --flat) themselves.
func Spec(opts CallOptions, args ...string) proc.Spec {
	actor := opts.Actor
	if actor == "" {
		actor = DefaultActor
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	full := make([]string, 0, len(args)+3)
	if opts.ReadOnly {
		full = append(full, "--readonly")
	}
	if opts.Repo != "" {
		full = append(full, "-C", opts.Repo)
	}
	full = append(full, args...)
	return proc.Spec{
		Command: Binary,
		Args:    full,
		Dir:     opts.Dir,
		Env: map[string]string{
			"BD_JSON_ENVELOPE":   "1",
			"BD_NON_INTERACTIVE": "1",
			"BEADS_ACTOR":        actor,
		},
		InheritEnv: append([]string(nil), InheritEnv...),
		Stdin:      opts.Stdin,
		Timeout:    timeout,
	}
}

// Run executes `bd <args>` with Spec(opts, args...) and returns the captured
// result. The error semantics are those of proc.Run: a non-zero exit is a
// *proc.ExitError with Result populated, and a missing binary satisfies
// proc.IsNotFound.
func Run(ctx context.Context, opts CallOptions, args ...string) (proc.Result, error) {
	return proc.Run(ctx, Spec(opts, args...))
}

// ErrNoJSON is wrapped by FindJSON when bd's output holds no JSON value at
// all (for example a plain error line, or the text that `bd init --json`
// and `bd info --json` print).
var ErrNoJSON = errors.New("no JSON object in output")

// Envelope is the BD_JSON_ENVELOPE=1 shape bd prints for --json commands:
// {"schema_version":1,"data":...}. Error, Code and Hint are the documented
// error shape; bd 1.2.2 actually reports failures as {"data":{"error":...}}
// with exit status 1 (P3-12), so callers should check both. When bd prints
// a legacy value without the envelope, Data holds that value and
// SchemaVersion is zero.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data"`
	Pagination    json.RawMessage `json:"pagination,omitempty"`
	Error         string          `json:"error,omitempty"`
	Code          string          `json:"code,omitempty"`
	Hint          string          `json:"hint,omitempty"`
}

// FindJSON locates the JSON value in bd's standard output, skipping any
// non-JSON preamble: bd prints a one-time metrics notice, and commands such
// as `bd gate check --json` print a text line before the envelope. Every
// '{' or '[' is tried as the start of a complete value; the first one that
// parses as an envelope (an object with schema_version) wins, else the first
// one that parses at all. It returns ErrNoJSON (wrapped) when nothing parses.
func FindJSON(stdout string) (json.RawMessage, error) {
	var first json.RawMessage
	rest := stdout
	for {
		i := strings.IndexAny(rest, "{[")
		if i < 0 {
			break
		}
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(rest[i:])).Decode(&raw); err == nil {
			if isEnvelope(raw) {
				return raw, nil
			}
			if first == nil {
				first = raw
			}
		}
		rest = rest[i+1:]
	}
	if first != nil {
		return first, nil
	}
	return nil, fmt.Errorf("%w %q", ErrNoJSON, strings.TrimSpace(stdout))
}

// isEnvelope reports whether raw is an object carrying schema_version.
func isEnvelope(raw json.RawMessage) bool {
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.SchemaVersion != nil
}

// ParseEnvelope finds the JSON value in stdout (see FindJSON) and returns it
// as an Envelope. A legacy value without the envelope is returned with Data
// set to the whole value.
func ParseEnvelope(stdout string) (Envelope, error) {
	raw, err := FindJSON(stdout)
	if err != nil {
		return Envelope{}, err
	}
	if isEnvelope(raw) {
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return Envelope{}, fmt.Errorf("decoding bd envelope: %w", err)
		}
		return env, nil
	}
	return Envelope{Data: raw}, nil
}

// Decode unmarshals the payload bd printed on stdout into v: the `data`
// member when the BD_JSON_ENVELOPE shape is present, else the whole value
// (legacy shapes, and objects that carry schema_version without data). Any
// preamble before the JSON is skipped (see FindJSON).
func Decode(stdout string, v any) error {
	raw, err := FindJSON(stdout)
	if err != nil {
		return err
	}
	payload := raw
	if isEnvelope(raw) {
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("decoding bd envelope: %w", err)
		}
		if len(env.Data) > 0 {
			payload = env.Data
		}
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("decoding bd output: %w", err)
	}
	return nil
}

// ErrorSummary renders a failed call on one line for reports and log
// lines: "timed out" for a timeout, "exit status N: <message>" for a
// non-zero exit, and err.Error() for anything else. A *Error's parsed
// Message wins when present; otherwise the message is the
// first stderr line beginning with "Error:" (bd's convention, which skips
// the one-time metrics notice printed before it), else the first non-empty
// stderr line, else the first stdout line, else nothing.
func ErrorSummary(err error) string {
	var bdErr *Error
	if errors.As(err, &bdErr) && bdErr.Message != "" {
		if bdErr.ExitCode > 0 {
			return fmt.Sprintf("exit status %d: %s", bdErr.ExitCode, bdErr.Message)
		}
		return bdErr.Message
	}
	var exit *proc.ExitError
	if errors.As(err, &exit) {
		switch {
		case errors.Is(err, proc.ErrTimeout):
			return "timed out"
		case exit.Result.ExitCode >= 0:
			if tail := firstLine(exit.Result.Stderr, exit.Result.Stdout); tail != "" {
				return fmt.Sprintf("exit status %d: %s", exit.Result.ExitCode, tail)
			}
			return fmt.Sprintf("exit status %d", exit.Result.ExitCode)
		}
	}
	return err.Error()
}

// firstLine returns the first line of a or b that starts with "Error:"
// (case-insensitively), else the first non-empty line of a, else of b,
// else "".
func firstLine(a, b string) string {
	fallback := ""
	for _, s := range []string{a, b} {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if len(line) >= 6 && strings.EqualFold(line[:6], "error:") {
				return line
			}
			if fallback == "" {
				fallback = line
			}
		}
	}
	return fallback
}
