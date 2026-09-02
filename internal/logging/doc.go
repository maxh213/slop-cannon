// Package logging configures slog for slop-cannon: text output for
// terminals, JSON output for files, per-run and per-agent log files, and a
// redaction hook that keeps credentials out of every line (P0-07).
//
// # Handlers
//
// [NewTerminalHandler] wraps [slog.TextHandler] for a human at a terminal and
// [NewFileHandler] wraps [slog.JSONHandler] for files that `sc status` and
// support tooling read back. Both take [Options]: Verbose lowers the level to
// [slog.LevelDebug], otherwise [slog.LevelInfo] applies (see [Level]).
// [NewMultiHandler] fans one record out to several handlers, which is how a
// headless run logs to stderr and to the run file at once (D-15).
//
// # Redaction
//
// Every handler built here installs [Redactor.ReplaceAttr], so values that
// look like tokens are rewritten before they reach the writer: API keys
// beginning with "sk-", GitHub tokens ("ghp_", "gho_", "ghu_", "ghs_",
// "ghr_", "github_pat_"), and "Bearer …" credentials. The message, string
// attributes, errors and nested groups are all covered; the prefix survives
// ("sk-[REDACTED]") so a log still says which kind of secret was present.
// [Redact] applies the same rules to any string, for callers that log through
// other channels (transcripts, event files).
//
// # Files
//
// Per D-23 a run's files live under <state-dir>/runs/<run-id>/. [PathsFor]
// derives the log locations from that directory: sc.log for the orchestrator
// and agents/<agent-id>.log for each agent. [OpenRunLog] creates the
// directory tree and returns a [RunLogger] whose [RunLogger.Agent] method
// opens agent files lazily; [RunLogger.Close] flushes and closes them all.
// Files are opened in append mode with 0600 permissions.
package logging
