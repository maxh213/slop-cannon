// Package hermes implements cartridge.Cartridge on top of Hermes Agent's
// "hermes chat --oneshot --format stream-json" protocol. It registers itself
// under "hermes". Flags and the JSONL protocol were checked against Hermes
// Agent [VerifiedVersion]. Opt-in live tests verify provider calls.
//
// Hermes has no separate system-prompt flag, so [Stdin] joins the system and
// user prompts and sends them through --query-file -. RunSpec.MaxTurns maps to
// --max-turns and RunSpec.Timeout maps to agent.run_budget_seconds in the
// isolated config as well as the process timeout. The gateway rejects the
// legacy --run-budget CLI option. Configured args are appended, which is how a router supplies
// --reasoning with one of Hermes' supported levels.
//
// # Tool policies and home isolation
//
// read_exec enables only the terminal toolset (which has no native edit tools)
// and read_write enables terminal,file. Web is added only when RunSpec.Web is
// true. read_only is refused: Hermes has no read-only terminal or standalone
// read-file toolset, so pretending to enforce it would be unsafe.
//
// Every firing creates a fresh private temporary directory with separate
// HERMES_HOME and HOME directories. Thus sessions, memory, profiles, logs and
// helper-process state cannot be shared by concurrent firings. The directory
// is removed after process exit, including timeout, kill, cancellation and
// start failure; it is never removed while the process is still running.
// --ignore-rules suppresses memory and rule injection. The only config is a
// fresh non-secret model.provider=nous and per-firing budget file needed by Hermes' first-run
// inventory; no user config, plugins or MCP state enters the temporary home.
//
// Nous authentication is the deliberate exception to state isolation. The
// adapter forwards HERMES_SHARED_AUTH_DIR, defaulting to the original Hermes
// root's shared directory (or an explicit override), so Hermes can adopt and
// refresh its shared Nous identity. An existing root auth.json is also exposed
// through Hermes' supported symlink alias, with auth.lock aliased to the same
// root lock so concurrent refreshes stay serialized; this covers root logins
// that predate the shared store. The adapter stats paths but never opens,
// reads or copies credential contents. Every other profile-local file stays
// in the temporary home and is cleaned up.
package hermes
