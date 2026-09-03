# Cartridges

A cartridge is an LLM CLI adapter plus its model. The orchestrator fires
every agent through the `cartridge.Cartridge` interface
(`internal/cartridge`): `Detect` says whether the CLI is installed and logged
in, `Run` starts one session for a `RunSpec` (prompts, workdir, model, tool
policy, limits) and streams `SessionEvent`s until a `Done` or `Error`.

Only **Claude Code** ships today (D-19). Kimi, Animus and a custom-command
adapter are planned; the interface, registry and mock cartridge are
adapter-agnostic so each is one package plus one registry line.

## Claude Code (`claude`)

Package `internal/cartridge/claude`. Registered under `claude`; configured by
`[cartridges.claude]` (`model`, `command`, `args`, `env`, see
[config.md](config.md)).

**Verified CLI version: `2.1.258 (Claude Code)`** (`claude --version`,
2026-09-02, `/home/<user>/.local/bin/claude`). Every flag below was checked
against `claude --help` and `claude -p --help` of that release, and the
fixtures in `testdata/claude/` were captured from it. Older releases are
refused below `2.0.0`; releases between 2.0.0 and 2.1.258 are untested.

### Invocation

```
claude -p --output-format stream-json --verbose \
       [--model M] [--append-system-prompt S] [--max-turns N] \
       --permission-mode dontAsk [--allowedTools …] [--disallowedTools …] \
       <cartridges.claude.args…>
```

- cwd is `RunSpec.Workdir` (the agent's worktree).
- The **user prompt is written to stdin** (`claude -p` reads it when no
  positional prompt is given). It never appears in `argv`, so it has no
  length limit and cannot be mistaken for an option.
- The system prompt is appended with `--append-system-prompt`, keeping
  Claude Code's own system prompt (tool descriptions, CLAUDE.md discovery).
  Linux limits one argument to 128 KiB; vibes and rules files are capped at
  64 KiB each in config, so keep their number small.
- `--model` is `RunSpec.Model`, else `cartridges.claude.model`, else the
  CLI's default.
- `--max-turns` is passed only when `RunSpec.MaxTurns > 0`.
- `cartridges.claude.args` are appended **last**, so a user flag overrides
  the adapter's when both set the same option (Commander keeps the last
  occurrence). They are not applied to Detect's probes.
- Session persistence is left on so `claude --resume <Result.SessionID>`
  can reopen an agent's transcript; add `--no-session-persistence` to
  `args` to turn it off.
- `RunSpec.Timeout` becomes a process-group timeout; SIGTERM, then SIGKILL
  five seconds later. Cancelling the context or `Session.Kill` does the
  same.

### Verified flags

| Flag | In `claude --help` 2.1.258 | Used for |
|---|---|---|
| `-p, --print` | yes | non-interactive mode |
| `--output-format stream-json` | yes (`text`, `json`, `stream-json`) | one JSON object per line |
| `--verbose` | yes | required for stream-json to include every message |
| `--model <model>` | yes (alias or full name) | model selection |
| `--append-system-prompt <prompt>` | yes | role prompt, vibes, rules |
| `--max-turns <n>` | **not listed**, but accepted and enforced (fixture `error-max-turns.jsonl`) | turn cap |
| `--permission-mode <mode>` | yes: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` | `dontAsk`: anything not pre-approved is denied without a prompt |
| `--allowedTools, --allowed-tools <tools…>` | yes, comma or space separated, permission-rule syntax such as `Bash(git log:*)` | pre-approve tools per policy |
| `--disallowedTools, --disallowed-tools <tools…>` | yes | remove editing tools; a removed tool is unavailable to the model and its subagents ("No such tool available") |
| `--tools <tools…>` | yes | not used (it re-shapes the built-in set and busted the prompt cache in testing; `--disallowedTools` is enough) |
| `--version` | yes | Detect |
| `auth status --json` | yes (`claude auth --help`) | Detect's auth probe, local, no network |

Also verified but not used: `--no-session-persistence`, `--max-budget-usd`,
`--include-partial-messages` (token-level deltas; the adapter emits one
`TextDelta` per text block instead), `--bare` (API-key only, drops OAuth, so
unusable for subscription users), `--restricted`, `--dangerously-skip-permissions`.

Running `claude` from inside another Claude Code session works, but the
adapter strips the parent session's markers anyway (see Environment).

### Tool policy mapping

All three policies run with `--permission-mode dontAsk`; `Read` is never
gated; `WebFetch`, `WebSearch` and any other prompting tool stay denied.

| `ToolPolicy` | Flags | Effect |
|---|---|---|
| `read_only` | `--allowedTools <ReadOnlyShell> --disallowedTools Edit,Write,MultiEdit,NotebookEdit` | Claude Code 2.1.258 offers `Glob` and `Grep` **only when `Bash` is disallowed** (compare the `tools` arrays in the `init` lines of `tool-disallowed.jsonl` and `shell-denied.jsonl`), so with a restricted `Bash` a planner explores with shell commands. `ReadOnlyShell` (`claude.ReadOnlyShell`) allows exactly `ls`, `pwd`, `tree`, `find`, `fd`, `cat`, `head`, `tail`, `wc`, `file`, `stat`, `grep`, `rg`, `ag`, `diff`, `git status`, `git log`, `git show`, `git diff`, `git branch`, `git ls-files`, `git blame`, `git rev-parse` and `bd` (each with any arguments); every other command is refused (fixture `shell-denied.jsonl`: `ls` ran, `touch` was denied, no file appeared). |
| `read_exec` | `--allowedTools Bash --disallowedTools Edit,Write,MultiEdit,NotebookEdit` | Testers can run builds and tests. A shell command can still write files; the policy is about which tools are offered and the worktree is disposable. |
| `read_write` | `--allowedTools Bash,Edit,Write,MultiEdit,NotebookEdit` | Engineers. |

P5-06 may tune `ReadOnlyShell`; the conformance suite (P2-09) checks that a
write is refused under `read_only`.

### Environment

The child gets only: `PATH`; `cartridge.DefaultInheritEnv()` (HOME, locale,
XDG, TLS and proxy variables); every `ANTHROPIC_*`, `CLAUDE_*`, `AWS_*`,
`GOOGLE_*` variable plus `CLOUD_ML_REGION`, `DISABLE_AUTOUPDATER`,
`DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING`,
`DISABLE_NON_ESSENTIAL_MODEL_CALLS`, `MAX_THINKING_TOKENS`,
`BASH_DEFAULT_TIMEOUT_MS`, `BASH_MAX_TIMEOUT_MS`, `MCP_TIMEOUT` when set;
`DISABLE_AUTOUPDATER=1` (agents never self-update mid-run; override in
config); `cartridges.claude.env`; `RunSpec.Env`.

Always dropped, even though they match `CLAUDE_*`: the markers a running
Claude Code session sets for its children (`CLAUDECODE`,
`CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SESSION_ID`,
`CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_BRIDGE_SESSION_ID`,
`CLAUDE_CODE_EXECPATH`, `CLAUDE_CODE_SSE_PORT`, `CLAUDE_CODE_MESSAGING_*`,
`CLAUDE_PID`, `CLAUDE_EFFORT`). Without this an agent started from inside a
Claude Code session would attach to that session.

### Detect

1. `claude` (or `cartridges.claude.command`) must be on `PATH`, else
   `ErrNotInstalled`.
2. `claude --version` must succeed and report `>= 2.0.0`, else
   `ErrNotInstalled` with the version in `Status.Detail`.
3. `claude auth status --json` (local; fixture `auth-status.json`):
   `loggedIn: false` gives `AuthMissing` and `ErrAuth`; success gives
   `AuthOK` and a detail such as `logged in as user@example.com, max`. If
   the subcommand is missing or prints no JSON, `Auth` stays unknown and
   Detect succeeds; `Run` finds out.

### stream-json parsing

One JSON object per stdout line (`testdata/claude/*.jsonl`):

| Line | Handling |
|---|---|
| `system` / `init` | session id, model |
| `system` / `thinking_tokens` | ignored |
| `rate_limit_event` | remembered when `status` is not `allowed`; `resetsAt` becomes `Error.RetryAfter` if the run then fails |
| `assistant` | one line per content block, the message's `usage` repeated on each. `text` → `TextDelta`; `tool_use` → `ToolUse` (input rendered on one line: the command for Bash, the path for file tools, sorted `k=v` otherwise); `thinking` skipped. Usage is summed once per message id so a crash mid-run still reports what was consumed. |
| `user` | `tool_result` blocks → `ToolResult` named after the matching `tool_use`; `is_error` passed through; output cut at 4 KiB |
| `result` | terminal. `Result.Output` is its `result` field, else the last assistant text. `Usage` comes from its `usage` and `total_cost_usd` (`HasCost` true), `Turns` from `num_turns`, `SessionID` from `session_id`. |
| anything else, non-JSON lines | ignored; kept for error messages (the CLI prints `Client.listTools() called but …` on stdout after the result) |

### Error-kind mapping

`Classify` decides in this order: process cause → result line → stderr /
stray output → exit status.

| Signal | Kind |
|---|---|
| `RunSpec.Timeout` elapsed (`proc.ErrTimeout`) | `timeout` |
| `Session.Kill`, context cancelled | `killed` |
| result `api_error_status` 401, 403 | `auth` |
| result `api_error_status` 402 | `credits_exhausted` |
| result `api_error_status` 429, 503, 529 | `rate_limited` |
| result `api_error_status` other (404 unknown model, 400, 500) | `crash` |
| text containing `credit balance`, `insufficient credit`, `insufficient_quota`, `out of credits`, `purchase more`, `plans & billing`, `billing`, `payment required`, `spending limit`, `quota exceeded`, `extra usage` | `credits_exhausted` (cartridge is dead for the run, D-18) |
| text containing `not logged in`, `please run /login`, `please login`, `invalid api key`, `invalid x-api-key`, `authentication_error`, `authentication failed`, `invalid authentication`, `oauth token`, `token has expired`, `token expired`, `unauthorized`, `permission_error` | `auth` |
| text containing `rate_limit_error`, `rate limit`, `rate-limit`, `too many requests`, `overloaded_error`, `overloaded`, `usage limit`, `hit your limit`, `limit reached`, `try again later` | `rate_limited` (back off; `RetryAfter` from the last rejected `rate_limit_event`) |
| a rejected `rate_limit_event` and no other match | `rate_limited` |
| result `subtype` `error_max_turns`, `error_max_budget_usd`, `error_during_execution`, any other `error_*`, or `is_error` with nothing matched | `crash` (retryable; nothing is wrong with the cartridge) |
| non-zero exit, no result line, nothing matched on stderr | `crash` |
| zero exit without a result line | `crash` ("without a result line") |
| binary missing at start | `not_installed` |

"Text" is the result line's `errors[]` joined, else its `result`, plus the
last assistant text and stderr. Matching is case-insensitive substring in
table order: credits phrases are checked before rate-limit phrases because
billing messages mention limits too. The table lives in `claude.TextRules`
and `claude.KindForStatus`.

Observed live (fixtures): 404 unknown model → `crash`; `error_max_turns` →
`crash`. The auth, rate-limit and credits phrases come from Claude Code's
own messages and the Anthropic API error types (`authentication_error`,
`permission_error`, `rate_limit_error`, `overloaded_error`,
`invalid_request_error` with "credit balance is too low"); they were not
reproducible on a logged-in account with credits, so extend `TextRules`
when a new phrasing shows up in a run log.

### Fixtures

See `testdata/claude/README.md` for what each file shows and the exact
commands that produced it. Unit tests replay them through a fake `claude`
on `PATH` (`internal/testutil.FakeBin`) and never call the real CLI.
`TestDocs_MappingTableMatchesCode` fails when this document stops
mentioning a flag, a `TextRules` phrase, a read-only shell command or a
dropped session marker, so update both together.

One opt-in smoke test fires the installed CLI for real (Detect, then one
`claude-haiku-4-5-20251001` turn asking for "PONG"; a fraction of a cent):

```
SC_CLAUDE_LIVE=1 go test -run TestLive -v ./internal/cartridge/claude/
```

It is skipped without the variable and never runs in CI. Last run against
2.1.258 on 2026-09-02: Detect reported `Auth:ok`, the session ended with
`Done`, 10 input / 72 output tokens, about 3 s. Running it from inside a
Claude Code session works because the adapter drops the parent session's
markers.

## Conformance suite

Package `internal/cartridge/conformance` (P2-09) runs the same fixed set of
prompts against any cartridge and prints a pass/fail table. It is the
acceptance test for an adapter: the mock runs it in every `make check`, and a
real CLI runs it on demand.

| Check | Policy | What must hold |
|---|---|---|
| `detect` | – | `Detect` returns no error and `Status.Ready()` is true. When it fails every other check is skipped. |
| `echo` | `read_only` | "Reply with exactly the token `sc-…`": `Result.Output` and the streamed `TextDelta`s contain the token, exit code 0. |
| `structured` | `read_only` | Fired through `structured.Fire` with the `conformance` contract (`{"token": string, "ok": true}`), so the prompt suffix, block extraction, schema validation and the one automatic retry are all exercised; the block must carry the token. |
| `write` | `read_write` | "Create a file named `sc-conformance.txt` containing the token": the file exists in the session's workdir with the token in it. |
| `refuse` | `read_only` | The same request must leave the workdir empty; the session still ends with `Done` (denied tool calls show up as error tool results, counted in the detail). |
| `timeout` | `read_exec` | "Run `tail -f /dev/null` in the foreground": with `RunSpec.Timeout` = 15 s the session must end with `ErrTimeout`, within 30 s of the deadline. |
| `kill` | `read_exec` | The same prompt; `Session.Kill` after 5 s must end the session with `ErrKilled`. |
| `beads-readonly` | `read_exec` | The read-only `bd` shim (P3-09, `beads.WriteShim`) is put first on the session's `PATH` and the workdir is turned into a throwaway beads project (`git init` + `bd init` with the D-13 flags; bd refuses everything with "no beads database found" before it looks at `--readonly` otherwise). "Run `bd update sc-shim-probe -s open`, then `bd ready --json`, and report both outputs verbatim": a tool result or the reply must contain `operation 'update' is not allowed in read-only mode`. The shim chain is verified directly (`Shim.Verify`) before the session runs. Skipped, not failed, when `bd` or `git` is not installed (so in CI); `Options.BD` names the real binary explicitly (tests pass a fake). |

Every session is also held to the stream contract: at least one event,
exactly one terminal event in last position, agreeing with `Wait` (`Done` ⇔
nil error, `Error` ⇔ a `*cartridge.Error`). Each check gets its own fresh
workdir under a temporary directory that is removed afterwards.

### Running it

```
make cartridge-conformance                  # CARTRIDGE=mock: scripted, no CLI, < 1 s
make cartridge-conformance CARTRIDGE=claude # the installed Claude Code, about 50 s, ~$0.12
```

The target runs `TestConformance` in the package with `-v`; the table is
printed to stdout and the target fails when any check fails. Knobs:

- `CARTRIDGE=<name>`: the registry name to build (`mock` or `claude` today).
  Any name but `mock` fires the real CLI and is **skipped when `CI` is set**,
  so CI only ever runs the mock.
- `SC_CONFORMANCE_MODEL=<model>`: the model for real cartridges; the default
  is the cheapest one per cartridge (`claude-haiku-4-5-20251001` for
  Claude).
- `SC_CONFORMANCE_CHECKS=detect,timeout,kill`: run a subset (unselected
  checks are reported as `SKIP`).

Real runs use the default timeouts (`conformance.DefaultTimeout` 3 min per
session, 15 s for the timeout check, `Kill` after 5 s); the mock run uses
100 ms and 50 ms so the whole suite takes about 150 ms. Programmatic use:
`conformance.Run(ctx, cartridge, conformance.Options{...})` returns a
`Report` whose `Table()` is what the target prints; `MockScenario(token)`
is the scenario that passes every check, for tests that need a known-good
cartridge.

### Expected output: mock

```
cartridge conformance: mock
detect: installed=true version="mock/1" auth=ok path=mock (scenario conformance)
token: sc-efbb49dcd85a

CHECK           RESULT  TIME   DETAIL
detect          PASS    0s     installed=true version="mock/1" auth=ok (scenario conformance)
echo            PASS    0s     output contains the token; 2 events, 58 in / 4 out tokens
structured      PASS    3ms    valid block from fence; 1 attempt(s), 340 in / 19 out tokens
write           PASS    1ms    sc-conformance.txt created with the token; 5 events, 81 in / 6 out tokens
refuse          PASS    0s     no file created; 5 events, 1 refused tool call(s), 81 in / 6 out tokens
timeout         PASS    100ms  ended after 100ms with timeout 100ms, 3 events
kill            PASS    50ms   ended 50ms after Run with Kill at 50ms, 3 events
beads-readonly  PASS    18ms   bd update refused through the shim; 7 events, 86 in / 8 out tokens

8 passed, 0 failed, 0 skipped in 173ms; 786 in / 53 out tokens
```

### Expected output: Claude Code

Run on 2026-09-02 against `2.1.258 (Claude Code)`, logged in with a Max
subscription, from inside another Claude Code session (the adapter drops the
parent's session markers). Every check passed; the whole run took 49 s and
reported $0.12 of usage:

```
cartridge conformance: claude
detect: installed=true version="2.1.258" auth=ok path=/home/<user>/.local/bin/claude (logged in as <you>, max)
token: sc-af98d2d935e0

CHECK           RESULT  TIME    DETAIL
detect          PASS    240ms   installed=true version="2.1.258" auth=ok (logged in as <you>, max)
echo            PASS    2.59s   output contains the token; 2 events, 10 in / 88 out tokens, $0.0168
structured      PASS    4.33s   valid block from fence; 1 attempt(s), 9 in / 242 out tokens, $0.0185
write           PASS    6.2s    sc-conformance.txt created with the token; 4 events, 18 in / 320 out tokens, $0.0271
refuse          PASS    6.11s   no file created; 4 events, 1 refused tool call(s), 18 in / 289 out tokens, $0.0268
timeout         PASS    15.66s  ended after 15.658s with timeout 15s, 3 events
kill            PASS    5.54s   ended 5.543s after Run with Kill at 5s, 1 events
beads-readonly  PASS    8.66s   bd update refused through the shim; 6 events, 18 in / 404 out tokens, $0.0276

8 passed, 0 failed, 0 skipped in 49.34s; 83 in / 1347 out tokens, $0.1168
```

What the checks looked like from the stream (`-v` logs every event):

- `refuse`: the model called `Write`, got
  `No such tool available: Write. Write is disabled for this session` back
  as an error tool result (`--disallowedTools`), and answered "I am not able
  to create files". Nothing appeared in the workdir.
- `timeout`: `Bash` ran `tail -f /dev/null`; 15 s later the process group
  got SIGTERM, the tool reported exit code 137 and `Wait` returned
  `cartridge claude: timed out`.
- `kill`: the session was killed 5 s in, while the first tool call was still
  running; `Wait` returned `cartridge claude: killed`.
- `beads-readonly`: `Bash` ran `bd update sc-shim-probe -s open` and got
  exit code 1 with `operation 'update' is not allowed in read-only mode`
  from the shim-wrapped bd, then `bd ready --json` answered `[]`; the model
  quoted both. Nothing in the throwaway beads project changed.
- The hang command is `tail -f /dev/null` rather than `sleep` because Claude
  Code 2.1.258's Bash tool blocks a standalone `sleep N` ("Blocked:
  standalone sleep 300 … use run_in_background") and the model then
  backgrounds it and replies at once, which is not a hang.

A failed check prints `FAIL: <reason>` in the detail column and the target
exits 1, for example when the hung session finishes on its own:

```
timeout     FAIL    14.76s  FAIL: the hung session finished successfully instead of timing out (output: "Waiting for sleep command to complete...") [ended after 14.756s with timeout 15s, 6 events]

6 passed, 1 failed, 0 skipped in 40.06s; 91 in / 1605 out tokens, $0.1211
```

The `beads-readonly` check is what P3-09 asked the suite to include: it
proves the cartridge hands the agent's `PATH` through to the CLI's shell, so a
`bd` shim installed by the orchestrator is the `bd` the model runs. With the
mock the two commands run through `sh -c`; with Claude Code the model runs
them in its Bash tool and quotes the refusal. The `.beads/` directory the
check creates lives in the per-check workdir and is removed with it.

## Adding a new cartridge

**Only Claude Code ships today (D-19).** Kimi (P2-04), Animus (P2-05) and a
custom-command adapter (P2-06) are planned and stay blocked until the human
reopens them; do not pick them up on your own. This guide is for the day
they are reopened, or for any other LLM CLI: the interface, registry, mock,
selection policy, structured-output contract and conformance suite are
adapter-agnostic, so a new cartridge is **one package under
`internal/cartridge/<name>` plus one registry line**, and nothing in the
orchestrator changes.

Budget: the mock-backed worked example at the end takes well under an hour;
a real CLI adapter is a Phase 2 `M`/`L` task (P2-03 was `L`) because the
parsing, classification and fixtures need the real tool on the machine.

### 1. The contract

Everything an adapter implements lives in `internal/cartridge`
(`cartridge.go`, `spec.go`, `registry.go`); read its package documentation
first. The interface is small:

```go
type Cartridge interface {
    Name() string
    Detect(ctx context.Context) (Status, error)
    Run(ctx context.Context, spec RunSpec) (Session, error)
}

type Session interface {
    Events() <-chan SessionEvent // ends with exactly one Done or Error, then closes
    Wait() (Result, error)       // nil or *cartridge.Error; callable many times
    Kill()                       // stop the process group; Wait then reports ErrKilled
}
```

The rules every implementation must keep (the conformance suite checks the
observable ones):

| Rule | Why |
|---|---|
| The factory (`func(name string, cfg config.Cartridge) (Cartridge, error)`) does **no I/O**: no `LookPath`, no subprocess, no network. | `Registry.Build` constructs every configured cartridge at startup; probing is `Detect`'s job and is cached by the selector. |
| `Name()` returns the registry name the factory was given (empty means your package's default). | Config, events and logs call the cartridge by that name; two instances of one adapter may be registered under different names. |
| `Detect` fills `Status` as far as it got and, on failure, returns a `*cartridge.Error` whose `Kind` is `not_installed`, `auth`, `credits_exhausted`, `rate_limited`, `timeout` or `crash`. An unknown auth state (`AuthUnknown`, no error) is allowed when the CLI has no cheap probe. | `Status.Ready()` and the error kind drive `sc doctor` and the D-12 fallback. |
| `Run` validates the spec (`spec.Validate()`), starts the process, and returns. A non-nil error means **nothing started**: a `*cartridge.Error` (`not_installed`, `killed`, `crash`) or the validation error. | The supervisor treats a `Run` error and a failed session differently. |
| Cancelling `ctx` kills the session; `RunSpec.Timeout` kills it too and ends it with `ErrTimeout`. | Timeouts are enforced by the adapter, not the caller. |
| The event stream ends with exactly one `EventDone` or `EventError`, as the last event, agreeing with `Wait` (`Done` ⇔ nil error); then the channel closes. `Wait` never blocks on an undrained channel. | `cartridge.Collect`, the structured retry loop and the TUI all rely on it. Use `cartridge.Pipe`; it enforces this for you. |
| Every error from `Wait` or an `EventError` is a `*cartridge.Error` with a valid `Kind`; a rate limit carries `RetryAfter` when the CLI said. | The supervisor acts on the kind alone (see the package docs for what each kind triggers). |
| `Result.Output` is the **final assistant text** (the model's last reply, or the whole transcript for a plain-text CLI); `Result.Usage` is what the run consumed, partially filled even on failure; `Result.ExitCode` is the process's, `-1` when killed. | `structured.Fire` parses the `sc-result` block out of `Output`; usage feeds the cost display and the optional caps. |
| The child gets a **clean environment**: `spec.ProcSpec(bin, args...)` wires `Dir`, `Timeout`, `Env` and `InheritEnv` (`PATH` + `cartridge.DefaultInheritEnv()` + `spec.InheritEnv`); add your CLI's own variables (API keys, endpoints) to `spec.EnvAllowlist()` and nothing else. | Agents must not inherit sc's or the user's unrelated secrets; nested-session markers of the parent tool must be stripped (see the Claude section's Environment). |
| Safe for concurrent use: the orchestrator fires many sessions of one cartridge at once, from different goroutines, in different worktrees. | Keep per-session state in the goroutine that pumps the process, not on the adapter. |

Helpers you should use instead of re-inventing them: `cartridge.NewPipe`
(the `Session`), `cartridge.NewError` / `AsError` / `KindOf` (errors),
`RunSpec.ProcSpec` / `EnvAllowlist` / `DefaultInheritEnv` (environment),
`internal/proc` (`LookPath`, `Probe`, `Run`, `Stream`, process-group kill
with a grace period), the event constructors `TextDelta`, `ToolUse`,
`ToolResult`, `Done`, `ErrorEvent`, and `cartridge.Collect` in tests.

### 2. Package layout

Mirror `internal/cartridge/claude`:

```
internal/cartridge/<name>/
  doc.go          package doc: what the CLI is, verified version, links here
  adapter.go      Name, MinVersion, Adapter, New (factory), Detect, Run, init()
  args.go         flag constants, Args(spec), ToolFlags(policy), InheritEnv
  stream.go       Parser for the CLI's streaming output -> SessionEvents, Result
  classify.go     Classify / ClassifyStart / ClassifyProbe -> *cartridge.Error
  *_test.go       fixture replay through testutil.FakeBin, docs drift test, opt-in live test
testdata/<name>/  captured CLI output + README.md with the exact commands
```

The skeleton below compiles against the current packages (it is vetted as
part of writing this guide; replace `acme` with the CLI's name). A
plain-text CLI can keep `pump` as written; a streaming-JSON CLI grows a
`Parser` like `claude/stream.go`.

```go
// Package acme drives the acme CLI. Verified against acme <version>; see
// docs/cartridges.md.
package acme

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name and the [cartridges.<name>] config key.
const Name = "acme"

// MinVersion is the oldest CLI release the adapter accepts.
var MinVersion = proc.V(1, 0, 0)

// gracePeriod is the SIGTERM-to-SIGKILL delay on timeout, cancel or Kill.
const gracePeriod = 5 * time.Second

// Adapter drives the acme CLI. Build it through the registry.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() { cartridge.MustRegister(Name, New) }

// New is the cartridge.Factory. It does no I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Binary is the executable: cfg.Command, else "acme".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect implements cartridge.Cartridge: binary on PATH, version gate, then
// a cheap local auth probe.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	path, err := proc.LookPath(a.Binary())
	if err != nil {
		st.Detail = a.Binary() + " not found on PATH"
		return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, err)
	}
	st.Installed, st.Path = true, path

	tool, err := proc.Probe(ctx, a.probeSpec("--version"))
	if err != nil {
		st.Detail = "acme --version failed"
		return st, cartridge.NewError(cartridge.KindCrash, a.name, st.Detail, err)
	}
	st.Version = tool.Output
	if tool.HasVersion {
		st.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			st.Detail = "acme " + st.Version + " is older than the minimum " + MinVersion.String()
			return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
		}
	}

	// Auth probe: replace with whatever the CLI offers locally (a status
	// subcommand, a credentials file). Leave Auth unknown, without an
	// error, when the CLI has no cheap probe; Run finds out.
	st.Auth = cartridge.AuthUnknown
	st.Detail = "auth probe not implemented"
	return st, nil
}

// probeSpec is a Detect invocation: clean environment, bounded time,
// configured env but not configured args.
func (a *Adapter) probeSpec(args ...string) proc.Spec {
	return proc.Spec{
		Command:    a.Binary(),
		Args:       args,
		Env:        a.cfg.Env,
		InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()),
		Timeout:    15 * time.Second,
	}
}

// InheritEnv adds the CLI's own variables to the base allowlist.
func InheritEnv(base []string) []string {
	return append(append([]string(nil), base...), "ACME_API_KEY", "ACME_BASE_URL")
}

// Args maps the spec onto CLI flags; ToolFlags is the P5-06 hook.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	args := []string{"--non-interactive", "--json"}
	if m := spec.Model; m != "" || a.cfg.Model != "" {
		if m == "" {
			m = a.cfg.Model
		}
		args = append(args, "--model", m)
	}
	if spec.SystemPrompt != "" {
		args = append(args, "--system", spec.SystemPrompt)
	}
	args = append(args, ToolFlags(spec.ToolPolicy)...)
	return append(args, a.cfg.Args...)
}

// ToolFlags maps a tool policy onto the CLI's tool controls (P5-06).
func ToolFlags(p cartridge.ToolPolicy) []string {
	switch p {
	case cartridge.ToolReadOnly:
		return []string{"--tools", "read"}
	case cartridge.ToolReadExec:
		return []string{"--tools", "read,shell"}
	default:
		return []string{"--tools", "read,shell,write"}
	}
}

// Run implements cartridge.Cartridge.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	ps.Env = mergeEnv(a.cfg.Env, spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.Stdin = strings.NewReader(spec.Prompt)
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		return nil, classifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe)
	return pipe, nil
}

// pump turns process output into events and finishes the pipe.
func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe) {
	var out, stderr strings.Builder
	var usage cartridge.Usage
	for l := range h.Lines() {
		switch l.Source {
		case proc.Stdout:
			// Parse the CLI's streaming format here; a plain-text CLI is
			// one TextDelta per line.
			out.WriteString(l.Text + "\n")
			ev := cartridge.TextDelta(l.Text + "\n")
			ev.At = l.At
			pipe.Emit(ev)
		case proc.Stderr:
			stderr.WriteString(l.Text + "\n")
		}
	}
	res, err := h.Wait()
	result := cartridge.Result{
		Output:   strings.TrimSpace(out.String()),
		Usage:    usage,
		Duration: res.Duration,
		ExitCode: res.ExitCode,
	}
	pipe.Finish(result, classify(a.name, err, res.ExitCode, stderr.String()))
}

// classifyStart maps a failure to start the process.
func classifyStart(name string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return cartridge.NewError(cartridge.KindNotInstalled, name, "", err)
	case errors.Is(err, context.Canceled):
		return cartridge.NewError(cartridge.KindKilled, name, "", err)
	default:
		return cartridge.NewError(cartridge.KindCrash, name, "failed to start", err)
	}
}

// classify maps the exit of a session onto an error kind: process cause
// first (timeout, killed), then the CLI's own messages, then the exit code.
// Fill the table in docs/cartridges.md from the same rules.
func classify(name string, procErr error, exitCode int, stderr string) error {
	if procErr != nil {
		if k := cartridge.KindOf(procErr); k == cartridge.KindTimeout || k == cartridge.KindKilled {
			return cartridge.NewError(k, name, "", procErr)
		}
	}
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "insufficient credits"):
		return cartridge.NewError(cartridge.KindCreditsExhausted, name, firstLine(stderr), procErr)
	case strings.Contains(text, "not logged in"):
		return cartridge.NewError(cartridge.KindAuth, name, firstLine(stderr), procErr)
	case strings.Contains(text, "rate limit"):
		return &cartridge.Error{Kind: cartridge.KindRateLimited, Cartridge: name, Msg: firstLine(stderr), Cause: procErr, RetryAfter: time.Minute}
	}
	if procErr != nil || exitCode != 0 {
		return cartridge.NewError(cartridge.KindCrash, name, firstLine(stderr), procErr)
	}
	return nil
}

func mergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		for k, v := range l {
			out[k] = v
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

Things the skeleton glosses over that a real adapter needs:

- **Prompt delivery.** Prefer stdin (no argv length limit, no option
  parsing). If the CLI only takes a positional prompt, remember Linux's
  128 KiB per-argument cap and say so in your section above.
- **System prompt.** Use the CLI's *append* flavour when it has one so its
  own tool descriptions and project-file discovery survive; the role
  prompts, vibes and rules (capped at 64 KiB each in config) arrive in
  `RunSpec.SystemPrompt`.
- **Configured `args` last**, so a user's flag wins when both set the same
  option, and never on Detect's probes.
- **`MaxTurns`** maps to the CLI's turn cap when it has one and is otherwise
  ignored; `Timeout` is always enforced by `ProcSpec`.
- **Usage.** Fill `InputTokens`, `OutputTokens`, cache tokens and `CostUSD`
  (`HasCost: true`) from whatever the CLI reports; `Turns` counts assistant
  turns. Sum usage as messages arrive so a crash mid-run still reports what
  it consumed. Leave fields zero rather than guessing.
- **`SessionID`** is the CLI's resumable session id when it has one, so a
  human can reopen an agent's transcript.
- **Tool events.** Emit `ToolUse(tool, id, input)` with the input rendered
  on one line (the command for a shell tool, the path for a file tool) and
  `ToolResult(tool, id, output, isError)` with the output cut to a few KiB;
  the TUI shows them verbatim. Refusals the CLI makes on its own (a denied
  tool) should arrive as error tool results; the conformance `refuse` check
  counts them.

### 3. Error-kind mapping table

Every adapter must document which of its CLI's signals map onto which
`cartridge.Kind`, in a table like the Claude one above, and keep the
table and the code in step (Claude's `TestDocs_MappingTableMatchesCode`
walks `TextRules` and fails when the document stops mentioning a phrase; do
the same). Start from this template and delete the rows your CLI cannot
produce:

| Signal from `<name>` | Kind | Notes |
|---|---|---|
| `RunSpec.Timeout` elapsed (`proc.ErrTimeout`) | `timeout` | always first; retryable |
| `Session.Kill`, context cancelled (`proc.ErrKilled`, `context.Canceled`) | `killed` | always second; never retried |
| *binary missing at start* (`exec.ErrNotFound`) | `not_installed` | `Run` error, not a session error |
| *version below `MinVersion`* | `not_installed` | Detect only; put the version in `Status.Detail` |
| *HTTP 401 / 403, "not logged in", "invalid api key", "token expired"* | `auth` | needs a human; `Detect` should catch it first |
| *HTTP 402, "insufficient credits", "quota exceeded", "billing"* | `credits_exhausted` | the cartridge is dead for the run (D-18); check **before** rate-limit phrases because billing messages mention limits too |
| *HTTP 429 / 503 / 529, "rate limit", "overloaded", "too many requests"* | `rate_limited` | fill `RetryAfter` when the CLI gives a reset time |
| *the CLI's own error result (turn cap, budget cap, tool failure)* | `crash` | retryable; nothing is wrong with the cartridge |
| *non-zero exit with nothing matched* | `crash` | put the first stderr line in `Msg` |
| *zero exit but no parseable result* | `crash` | "without a result line" |
| *anything else* | `crash` | `AsError` turns `unknown` into `crash` |

Precedence matters: decide **process cause → the CLI's structured error →
its free text (credits, then auth, then rate limit) → exit status**, exactly
as `claude.Classify` does, and write the order down. Keep the phrase table in
one exported variable so tests and the docs test can walk it. Test the
mapping with `errors.Is(err, cartridge.ErrX)` and `cartridge.KindOf(err)`;
the supervisor uses nothing else.

`Detect` uses the same kinds: `not_installed` (missing or too old), `auth`
(the probe says logged out), and `crash`/`timeout` when the probe itself
fails. Anything the probe cannot tell leaves `Status.Auth` unknown with a
nil error.

### 4. Tool-policy mapping (P5-06)

`RunSpec.ToolPolicy` is one of `read_only`, `read_exec`, `read_write`
(`cartridge.ToolPolicies()`); `AllowsExec()` and `AllowsWrite()` say what
each permits. Keep the mapping behind **one per-adapter function**
(`ToolFlags(policy) []string` in Claude and in the skeleton) so P5-06 can
tune it without touching `Run`, and document it in a table like the Claude
one:

| `ToolPolicy` | Who gets it | What the CLI must enforce |
|---|---|---|
| `read_only` | PO, testers reading a worktree | read files and run **read-only** shell commands (listing, `cat`, `grep`, `git log/diff/status`, `bd`); no file edits, no arbitrary shell, no network tools |
| `read_exec` | testers running the check command | read + run arbitrary shell commands; no file-editing tools (a shell command can still write; the policy is about which tools are offered and the worktree is disposable) |
| `read_write` | engineers | everything, still without interactive prompts |

Rules that apply to every adapter:

- Always run in the CLI's **unattended / no-prompt mode** (Claude:
  `--permission-mode dontAsk`); a permission prompt on a headless agent is
  a hang, which the `timeout` check will catch.
- Prefer *removing* editing tools over denying them: a removed tool cannot
  be called by subagents either.
- Under `read_only`, prefer a shell allowlist over no shell if the CLI's
  file-search tools disappear when the shell is removed (as Claude Code's
  `Glob`/`Grep` do); see `claude.ReadOnlyShell`.
- The `bd` shim (P3-09) must stay callable under every policy; it is
  read-only by construction.
- **CLI without tool controls.** If the CLI cannot restrict its tools, map
  every policy to the same invocation, document that `read_only` and
  `read_exec` are not enforced, and note the planned fallback: the
  orchestrator copies the worktree to a temporary directory for testers so
  edits cannot leak (documented by P5-06, not implemented yet). Such an
  adapter will fail the conformance `refuse` check until that lands; say so
  in your section.

The conformance suite verifies the mapping end to end: `write` must create
the file under `read_write`, `refuse` must create nothing under `read_only`.

### 5. Registry registration and configuration

1. **Register from `init`:** `cartridge.MustRegister(Name, New)` (the
   skeleton does). The registry is `cartridge.Default`; `Register` fails on
   a duplicate name, so one package registers one name (a second name for
   the same adapter is `cartridge.Register("other", pkg.New)` at the call
   site, which is how tests get two cartridges).
2. **Link the package into the binaries that need it.** Registration is a
   side effect of importing, so add a blank import
   (`_ "slop-cannon/internal/cartridge/<name>"`) wherever the default
   registry is used: the `sc` command wiring (P8-01) and
   `internal/cartridge/conformance/conformance_test.go` (so
   `make cartridge-conformance CARTRIDGE=<name>` can build it). Nothing in
   `internal/cartridge` imports adapters, and adapters must not import
   `conformance`, `structured` or `mock` except from their own tests.
3. **Lift the D-19 gate:** add the name to `config.KnownCartridges`
   (`internal/config/schema.go`); until then `sc cartridge --add <name>`
   fails with `cartridge "<name>" is not supported yet (only "claude" for
   now)`. Update `internal/cli` golden files and `docs/config.md` /
   `docs/cli.md` (`make update-golden`, `make docs-cli`) where that text
   appears, and record the change as a new Decision superseding D-19.
4. **Configuration** arrives as `config.Cartridge` (`[cartridges.<name>]`):
   `model` (passed to the CLI), `command` (alternative binary; use
   `cfg.Binary(Name)`), `args` (appended last to every firing) and `env`
   (set on the child). Do not add adapter-specific config fields; use `env`.
5. **Selection needs nothing from you.** `cartridge.NewSelector` /
   `BuildSelector` implement D-12 (active cartridge, per-role override,
   Detect-probed with a TTL, Warning + fallback when an override fails) over
   `Registry.Build(cfg)`; `sc doctor` lists every configured cartridge by
   checking its binary on `PATH`. The supervisor's multi-cartridge fallback
   (P6-07) is written against the interface and tested with two mocks.

### 6. Fixtures and unit tests

- Capture the CLI's real output for the cases you classify (a normal reply,
  a tool call and its result, a denied tool, an error result, a wrong model,
  the version and auth probes) into `testdata/<name>/` with a `README.md`
  listing the exact command that produced each file, as
  `testdata/claude/README.md` does. Scrub user names and tokens.
- Replay them through a fake binary on `PATH`: `testutil.FakeBin(t,
  "<name>", script, testutil.WithStdinCapture())` installs a shell script
  under the CLI's name; the Claude tests use one script that answers
  `--version` and the auth probe from `SC_CLAUDE_VERSION` /
  `SC_CLAUDE_AUTH_FILE` and otherwise `cat`s the fixture named by
  `SC_CLAUDE_FIXTURE` (all passed through `config.Cartridge.Env`, which is
  why the adapter must forward its configured env to the child). Assert the
  argv the shim recorded, the prompt it captured on stdin, the events,
  `Result`, `Usage` and the error kinds. Unit tests never call the real CLI
  and never touch the real home (`t.TempDir()`, `XDG_*` overrides).
- Add a docs drift test modelled on `claude.TestDocs_MappingTableMatchesCode`
  so this document and the code cannot diverge.
- Add an opt-in live smoke test (`SC_<NAME>_LIVE=1 go test -run TestLive
  ./internal/cartridge/<name>/`) that runs Detect and one cheap turn, skipped
  otherwise and in CI.
- `go build ./internal/cartridge/<name>/... && go vet ... && go test -race
  ...`, then `make check`.

### 7. Run the conformance suite against it

The suite (`internal/cartridge/conformance`, P2-09) is the acceptance test:
`detect`, `echo`, `structured`, `write`, `refuse`, `timeout`, `kill` and
`beads-readonly` (`conformance.CheckNames()`), plus the stream-contract
assertions on every session. `beads-readonly` needs `bd` and `git` on
`PATH` (or `Options.BD` naming the real binary) and reports `SKIP`, not
`FAIL`, without them or on a platform with no POSIX shell. Two ways to
run it:

- **Real CLI, from the Makefile:** after adding the blank import to
  `conformance_test.go` and a cheap default model to its
  `conformanceDefaultModels` map,

  ```
  make cartridge-conformance CARTRIDGE=<name>            # ~1 minute, a few cents
  SC_CONFORMANCE_MODEL=<model> make cartridge-conformance CARTRIDGE=<name>
  SC_CONFORMANCE_CHECKS=detect,echo make cartridge-conformance CARTRIDGE=<name>
  ```

  The driver builds the cartridge with `cartridge.New(name,
  config.Cartridge{Model: model})` from the default registry, is skipped
  when `CI` is set, and logs every event with `-v`. Paste the resulting
  table into an "Expected output: `<Name>`" subsection of the conformance
  section above, with the CLI version and date, as done for Claude.
- **Programmatically, from your own tests** (the only way for a cartridge
  that needs setup the driver cannot do, such as a scripted token):

  ```go
  rep, err := conformance.Run(ctx, c, conformance.Options{Token: token, Timeout: 10 * time.Second})
  require.NoError(t, err)
  require.NoError(t, rep.Err(), rep.Table())
  ```

  `Options` also takes `Model`, `HangTimeout`, `KillAfter`, `Workdir`,
  `Checks` (a subset of `conformance.CheckNames()`; everything else is
  reported as `SKIP: not selected`), `Env`, an `Observer` and `BD`. The
  report's `Table()` is what the Makefile target prints; `rep.Err()` is nil
  when nothing failed (skips do not count) and `rep.Passed()` says the same.

A check that fails prints `FAIL: <reason>` in its detail column; the
reasons are written to be actionable (`sc-conformance.txt was not created
under read_write`, `the hung session finished successfully instead of
timing out`, `stream contract: 2 terminal events, want exactly 1`).

### 8. Document it

Add a "## <Name> (<name>)" section above, following the Claude section:
verified CLI version and date, the exact invocation, a verified-flags table,
the tool-policy table, the environment allowlist, Detect's probes, the
output parsing table, the error-kind table and the fixtures. The Phase 2
exit criteria require the flags to be verified against the installed CLI's
`--help` and recorded here. Mention the cartridge in `docs/config.md` and
the README's Cartridges section (P10-01).

### Checklist

- [ ] `internal/cartridge/<name>` with `var _ cartridge.Cartridge = (*Adapter)(nil)`
- [ ] factory does no I/O; `init()` calls `cartridge.MustRegister(Name, New)`
- [ ] `Detect`: PATH → version gate (`MinVersion`) → cheap auth probe; kinds `not_installed` / `auth` / `crash` / `timeout`; `Status` filled as far as known
- [ ] `Run`: `spec.Validate()`, `spec.ProcSpec(...)`, prompt on stdin, `GracePeriod`, `cartridge.NewPipe` with the process kill, events pumped from `proc.Stream`
- [ ] clean child environment: `EnvAllowlist()` + the CLI's own variables, parent-session markers stripped
- [ ] configured `args` appended last; not applied to probes
- [ ] `Result.Output` is the final assistant text; `Usage` summed as it streams; `SessionID` when the CLI has one
- [ ] error-kind table filled in (section 3) and mirrored by a docs drift test; precedence documented
- [ ] tool-policy mapping behind one function (section 4), documented; `read_only` refuses writes
- [ ] fixtures in `testdata/<name>/` with a README of commands; unit tests replay them through `testutil.FakeBin`; opt-in live test
- [ ] name added to `config.KnownCartridges` with a Decision superseding D-19; goldens and docs regenerated
- [ ] blank import in the `sc` wiring and in `conformance_test.go`; default model in `conformanceDefaultModels`
- [ ] `make cartridge-conformance CARTRIDGE=<name>` passes; expected output recorded above
- [ ] section for the cartridge in this file; `make check` green

### Worked example: a second mock-backed cartridge

This is the exercise behind P2-10's acceptance criterion: register a second
cartridge, backed by the mock adapter, under its own name, and prove it
through selection and conformance. It touches nothing outside a new
package, takes well under an hour, and is how P6-07's two-cartridge fallback
tests and any future "two real cartridges" scenario are exercised.

`internal/cartridge/parrot/parrot.go`:

```go
// Package parrot is the worked example from docs/cartridges.md: a second
// cartridge backed by the mock adapter, registered under its own name so
// selection, fallback and conformance can be exercised with two cartridges.
package parrot

import (
	"strings"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
)

// Name is the registry name and the [cartridges.parrot] config key.
const Name = "parrot"

// Adapter is the mock adapter playing under the parrot name.
type Adapter struct{ *mock.Adapter }

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() { cartridge.MustRegister(Name, New) }

// New is the cartridge.Factory: it plays the scenario file named by
// SC_MOCK_SCENARIO in cfg.Env, or one that echoes every prompt.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	s := mock.DefaultScenario()
	if path := strings.TrimSpace(cfg.Env[mock.ScenarioEnv]); path != "" {
		loaded, err := mock.Load(path)
		if err != nil {
			return nil, err
		}
		s = loaded
	}
	a, err := mock.NewAdapter(name, s)
	if err != nil {
		return nil, err
	}
	return &Adapter{Adapter: a}, nil
}
```

`internal/cartridge/parrot/parrot_test.go`:

```go
package parrot

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/conformance"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// TestParrot_Conformance runs the conformance suite against the parrot
// cartridge built from the default registry.
func TestParrot_Conformance(t *testing.T) {
	t.Parallel()
	token := conformance.NewToken()
	path := filepath.Join(t.TempDir(), "scenario.json")
	require.NoError(t, conformance.MockScenario(token).WriteFile(path))

	c, err := cartridge.New(Name, config.Cartridge{Env: map[string]string{mock.ScenarioEnv: path}})
	require.NoError(t, err)

	rep, err := conformance.Run(context.Background(), c, conformance.Options{
		Token:       token,
		Timeout:     10 * time.Second,
		HangTimeout: 100 * time.Millisecond,
		KillAfter:   50 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, rep.Err(), rep.Table())
	assert.Equal(t, Name, rep.Cartridge)
}

// TestParrot_Selection shows two cartridges side by side: parrot is the
// active one and the tester role overrides it (D-12).
func TestParrot_Selection(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		ActiveCartridge: Name,
		Cartridges:      map[string]config.Cartridge{Name: {}, mock.Name: {}},
		Roles:           map[domain.Role]string{domain.RoleTester: mock.Name},
	}
	sel, err := cartridge.BuildSelector(cartridge.Default, cfg)
	require.NoError(t, err)
	eng, err := sel.Select(context.Background(), domain.RoleEngineer)
	require.NoError(t, err)
	assert.Equal(t, Name, eng.Name())
	tst, err := sel.Select(context.Background(), domain.RoleTester)
	require.NoError(t, err)
	assert.Equal(t, mock.Name, tst.Name())
}
```

`go test -race ./internal/cartridge/parrot/` passes both in a few seconds
(re-verified against the tree on 2026-09-03 by extracting the listings above
into a throwaway package: `gofmt` and `go vet` clean, both tests green with
all eight checks; the package is not checked in, it is the exercise). The
`beads-readonly` check uses whatever `bd` is first on `PATH`; on a machine
without one it reports `SKIP` and the test still passes, and a test that
must not depend on the host can point `Options.BD` at a fake the way
`conformance_test.go`'s `conformanceFakeBD` does. `rep.Table()` reads exactly like the mock table in
"Expected output: mock" with `cartridge conformance: parrot` on top. To make
`make cartridge-conformance CARTRIDGE=parrot` work as well you would add the
blank import to `conformance_test.go`; the driver would then build parrot
with an empty config, so the echo-everything default scenario passes `echo`
but not `structured` or `write`, which is why mock-backed cartridges run the
suite programmatically with a known token instead.

Agents who completed this exercise (P2-10's "Done when"): wf-p234
(integrator), 2026-09-02 — pasted the two listings above into a temporary
`internal/cartridge/parrot` package, `go vet` clean, `TestParrot_Selection`
and `TestParrot_Conformance` (all eight checks, `beads-readonly` included)
green on the first run, about five minutes end to end; the package was
removed again since the example is the exercise.
