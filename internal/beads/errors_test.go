package beads

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
)

// errorsSample loads a captured bd failure (stdout, stderr and exit
// status) as the proc.Result and error the client would have seen.
func errorsSample(t *testing.T, name string) (proc.Result, error) {
	t.Helper()
	dir := envSamplesDir(t)
	res := proc.Result{}
	for _, ext := range []string{".json", ".jsonl", ".txt"} {
		if data, err := os.ReadFile(filepath.Join(dir, name+ext)); err == nil {
			res.Stdout = string(data)
			break
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, name+".stderr")); err == nil {
		res.Stderr = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, name+".exit")); err == nil {
		code, err := strconv.Atoi(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		res.ExitCode = code
	}
	if res.ExitCode == 0 {
		return res, nil
	}
	return res, &proc.ExitError{Command: Binary, Result: res}
}

// TestClassifyCapturedErrors is the P3-12 table: every captured bd 1.2.2
// failure maps to the right typed error, whether bd reported it in the
// envelope's data.error (exit 1) or on stderr with an empty stdout.
func TestClassifyCapturedErrors(t *testing.T) {
	cases := []struct {
		sample  string
		args    []string
		kind    error
		message string
		text    string
	}{
		{"error-not-found", []string{"show", "nope-1", "--json"}, ErrNotFound,
			"no issues found matching the provided IDs", "bd show: no issues found matching the provided IDs"},
		{"error-close-not-found", []string{"close", "nope-1", "--json"}, ErrNotFound,
			`resolving ID nope-1: no issue found matching "nope-1"`, ""},
		{"error-comment-not-found", []string{"comment", "nope-1", "--stdin", "--json"}, ErrNotFound,
			`resolving nope-1: no issue found matching "nope-1"`, ""},
		{"error-update-not-found", []string{"update", "nope-1", "-s", "open", "--json"}, ErrNotFound,
			`Error resolving nope-1: no issue found matching "nope-1"`, ""},
		{"error-parent-not-found", []string{"create", "X", "--parent", "nope-1", "--json"}, ErrNotFound,
			"parent issue nope-1 not found", "bd create: parent issue nope-1 not found"},
		{"error-close-blocked", []string{"close", "fx-nvi.1", "--reason", "done", "--json"}, ErrBlockedOpen,
			"cannot close fx-nvi.1: blocked by open issues [fx-nvi.2] (use --force to override)", ""},
		{"error-close-blocked-2", []string{"close", "fx-ctt", "--json"}, ErrBlockedOpen,
			"cannot close fx-ctt: blocked by open issues [fx-so0] (use --force to override)", ""},
		{"error-readonly", []string{"--readonly", "-C", "/r", "update", "fx-ctt", "-s", "open", "--json"}, ErrReadOnly,
			"operation 'update' is not allowed in read-only mode", "bd update: operation 'update' is not allowed in read-only mode"},
		{"error-readonly-close", []string{"--readonly", "close", "fx-ctt", "--json"}, ErrReadOnly,
			"operation 'close' is not allowed in read-only mode", ""},
		{"error-readonly-comment", []string{"--readonly", "comment", "fx-ctt", "--stdin", "--json"}, ErrReadOnly,
			"operation 'comment' is not allowed in read-only mode", ""},
		{"error-claim-taken", []string{"update", "fx-so0", "--claim", "--json"}, ErrAlreadyClaimed,
			"Error claiming fx-so0: issue already claimed by sc:engineer-1", ""},
		{"error-dep-cycle", []string{"dep", "add", "fx-nvi.2", "fx-nvi.1", "--json"}, ErrCycle,
			"adding dependency would create a cycle", "bd dep add: adding dependency would create a cycle"},
		{"error-no-project", []string{"-C", "/tmp", "ready", "--json"}, ErrNoProject,
			`cannot use -C directory "/tmp": no beads project found`, ""},
		{"error-bad-status", []string{"update", "fx-ctt", "-s", "not_a_status", "--json"}, ErrInvalid, "", ""},
		{"error-bad-type", []string{"create", "X", "-t", "nonsense", "--json"}, ErrInvalid,
			"validation failed for issue : invalid issue type: nonsense", ""},
		{"error-bad-priority", []string{"create", "X", "-p", "9", "--json"}, ErrInvalid,
			`invalid priority "9" (expected 0-4 or P0-P4, not words like high/medium/low)`, ""},
		{"error-empty-title", []string{"create", "", "-t", "task", "--json"}, ErrInvalid,
			"validation failed for issue : title is required", ""},
		{"error-comment-empty", []string{"comment", "fx-so0", "--stdin", "--json"}, ErrInvalid,
			"comment text cannot be empty", ""},
		{"error-show-no-args", []string{"show", "--json"}, ErrInvalid,
			"at least one issue ID is required (use positional args, --id flag, or --current)", ""},
	}
	for _, tc := range cases {
		t.Run(tc.sample, func(t *testing.T) {
			res, runErr := errorsSample(t, tc.sample)
			require.Error(t, runErr, "the sample must carry a non-zero exit status")
			err := classify(tc.args, res, runErr)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.kind)
			var be *Error
			require.ErrorAs(t, err, &be)
			assert.Equal(t, 1, be.ExitCode)
			assert.Equal(t, tc.args, be.Args)
			if tc.message != "" {
				assert.Equal(t, tc.message, be.Message)
			} else {
				assert.NotEmpty(t, be.Message)
			}
			if tc.text != "" {
				assert.Equal(t, tc.text, err.Error())
			}
			assert.Empty(t, be.Code, "bd 1.2.2 prints no code")
			assert.Empty(t, be.Hint)
			var exit *proc.ExitError
			assert.ErrorAs(t, err, &exit, "the proc error stays reachable")
			for _, other := range []error{ErrNotFound, ErrBlockedOpen, ErrReadOnly, ErrLocked, ErrAlreadyClaimed, ErrCycle, ErrNoProject, ErrInvalid} {
				if other != tc.kind {
					assert.False(t, errors.Is(err, other), "must not also be %v", other)
				}
			}
		})
	}
}

func TestClassifyExitZeroIsSuccess(t *testing.T) {
	for _, sample := range []string{"error-not-found-mixed", "error-dep-not-found", "close", "ready"} {
		res, runErr := errorsSample(t, sample)
		require.NoError(t, runErr, sample)
		assert.NoError(t, classify([]string{"x"}, res, nil), sample)
	}
}

func TestClassifyFutureEnvelope(t *testing.T) {
	// The documented {"error","code","hint"} shape keeps working when bd
	// starts printing it; a known code wins over the message.
	res := proc.Result{ExitCode: 1, Stdout: `{"schema_version":1,"error":"issue x-1 does not exist","code":"not_found","hint":"run bd list"}`}
	err := classify([]string{"show", "x-1", "--json"}, res, &proc.ExitError{Command: Binary, Result: res})
	assert.ErrorIs(t, err, ErrNotFound)
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Equal(t, "issue x-1 does not exist", be.Message)
	assert.Equal(t, "not_found", be.Code)
	assert.Equal(t, "run bd list", be.Hint)

	res = proc.Result{ExitCode: 1, Stdout: `{"schema_version":1,"data":{"error":"the database is locked by another process","code":"locked"}}`}
	err = classify([]string{"update"}, res, &proc.ExitError{Command: Binary, Result: res})
	assert.ErrorIs(t, err, ErrLocked)

	res = proc.Result{ExitCode: 1, Stdout: `{"schema_version":1,"error":"meh","code":"custom_thing"}`}
	err = classify([]string{"update"}, res, &proc.ExitError{Command: Binary, Result: res})
	require.ErrorAs(t, err, &be)
	assert.Nil(t, be.Kind, "an unknown code with a bland message maps to no kind")
	assert.Equal(t, "bd update: meh", err.Error())
}

func TestClassifyLockMessages(t *testing.T) {
	for _, msg := range []string{
		"Error: database is locked",
		"Error: could not acquire lock on /r/.beads/dolt/noms/LOCK",
		"error: unable to acquire lock: resource temporarily unavailable",
	} {
		res := proc.Result{ExitCode: 1, Stderr: msg + "\n"}
		err := classify([]string{"update", "x"}, res, &proc.ExitError{Command: Binary, Result: res})
		assert.ErrorIs(t, err, ErrLocked, msg)
	}
}

func TestClassifyTimeoutAndStartFailures(t *testing.T) {
	res := proc.Result{ExitCode: -1, TimedOut: true}
	err := classify([]string{"close", "x", "--json"}, res, &proc.ExitError{Command: Binary, Result: res, Cause: proc.ErrTimeout})
	assert.ErrorIs(t, err, proc.ErrTimeout)
	assert.Equal(t, "bd close: timed out", err.Error())
	var be *Error
	require.ErrorAs(t, err, &be)
	assert.Nil(t, be.Kind)

	err = classify([]string{"close", "x"}, proc.Result{}, exec.ErrNotFound)
	assert.True(t, proc.IsNotFound(err))
	assert.Equal(t, "bd close: not found on PATH", err.Error())

	err = classify([]string{"dep", "add", "a", "b"}, proc.Result{}, errors.New("chdir /nowhere: no such file"))
	assert.Equal(t, "bd dep add: chdir /nowhere: no such file", err.Error())

	res = proc.Result{ExitCode: 2}
	err = classify([]string{"list"}, res, &proc.ExitError{Command: Binary, Result: res})
	assert.Equal(t, "bd list: exit status 2", err.Error(), "no message at all falls back to the exit status")

	assert.NoError(t, classify([]string{"list"}, proc.Result{}, nil))
}

func TestErrorRendering(t *testing.T) {
	e := &Error{Command: "close", Kind: ErrBlockedOpen, Message: "cannot close x"}
	assert.Equal(t, "bd close: cannot close x", e.Error())
	assert.ErrorIs(t, e, ErrBlockedOpen)
	assert.Equal(t, []error{ErrBlockedOpen}, e.Unwrap())
	assert.Equal(t, "bd: failed", (&Error{}).Error())
	assert.Equal(t, "bd x: boom", (&Error{Command: "x", ExitCode: -1, Cause: errors.New("boom")}).Error())
}

func TestStderrMessage(t *testing.T) {
	preamble := envSample(t, "preamble.txt")
	roleNotice := "warning: beads.role not configured (GH#2950).\n  Fix: git config beads.role maintainer\n  Or:  git config beads.role contributor\n"
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain line", "cannot close x\n", "cannot close x"},
		{"Error prefix stripped", "Error: operation 'update' is not allowed in read-only mode\n", "operation 'update' is not allowed in read-only mode"},
		{"Error word kept", "Error claiming x: taken\n", "Error claiming x: taken"},
		{"after the role notice", roleNotice + "Error: parent issue nope-1 not found\n", "parent issue nope-1 not found"},
		{"after the metrics preamble", preamble + "cannot close x\n", "cannot close x"},
		{"preamble then notice then message", preamble + roleNotice + "cannot close x\n", "cannot close x"},
		{"only notices", preamble + roleNotice, ""},
		{"Error line preferred over earlier text", "some context\nError: real problem\n", "real problem"},
		{"first line when no Error line", "first\nsecond\n", "first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StderrMessage(tc.in))
		})
	}
}

func TestErrorMessagePrefersTheEnvelope(t *testing.T) {
	msg, code, hint := ErrorMessage(envSample(t, "error-not-found.json"), envSample(t, "error-not-found.stderr"))
	assert.Equal(t, "no issues found matching the provided IDs", msg)
	assert.Empty(t, code)
	assert.Empty(t, hint)

	msg, _, _ = ErrorMessage("", "Error: x\n")
	assert.Equal(t, "x", msg)

	msg, _, _ = ErrorMessage(`{"data":[],"schema_version":1}`, "Error: y\n")
	assert.Equal(t, "y", msg, "a data payload without an error string falls through to stderr")
}

func TestKindOf(t *testing.T) {
	assert.Equal(t, ErrNotFound, KindOf("NOT_FOUND", "anything"))
	assert.Equal(t, ErrBlockedOpen, KindOf("", "cannot close x: blocked by open issues [y]"))
	assert.Equal(t, ErrNotFound, KindOf("", "parent issue x not found"))
	assert.Equal(t, ErrInvalid, KindOf("", "unknown flag: --bogus"))
	assert.Nil(t, KindOf("", "something else entirely"))
	assert.Nil(t, KindOf("bogus_code", ""))
}

func TestCommandName(t *testing.T) {
	assert.Equal(t, "close", commandName([]string{"-C", "/r", "close", "x", "--json"}))
	assert.Equal(t, "dep add", commandName([]string{"--readonly", "-C", "/r", "dep", "add", "a", "b"}))
	assert.Equal(t, "config set", commandName([]string{"config", "set", "k", "v"}))
	assert.Equal(t, "show", commandName([]string{"show", "a", "b"}))
	assert.Equal(t, "comments", commandName([]string{"comments", "fx-1", "--json"}))
	assert.Equal(t, "create", commandName([]string{"create", "--graph", "/p.json", "--dry-run"}))
	assert.Equal(t, "", commandName([]string{"--json"}))
	assert.Equal(t, "dep", commandName([]string{"dep"}))
}
