# How slop-cannon uses beads (`bd`)

Beads is the persistent task graph: every unit of work the cannon fires at
is a `bd` issue inside the target repository's `.beads/` database, and
everything `sc` knows about a bead is readable with plain `bd` commands.
This document records the conventions so a human reading `bd show <id>`
understands the history, and so later phases append their own sections
here. The verified facts behind it are in `IMPLEMENTATION-PROGRESS.md`,
Appendix A.

## Requirements

- `bd` 1.2.2 or newer on `PATH`, built with embedded Dolt (`brew install
  beads` or the upstream `scripts/install.sh`). A `CGO_ENABLED=0 go
  install` build is server-mode only and cannot open the database; `sc
  doctor` and every run check this first (`beads.Detect`).
- No standalone `dolt` binary is needed. Embedded mode is the only mode
  `sc` supports.
- `bd ping --json` is the health check. `bd doctor --json` prints nothing
  in embedded mode and is not used.

## Invocation contract (D-21)

Every `bd` call `sc` makes goes through `internal/beads` and `internal/proc`
with the same shape (`beads.Spec`):

| Aspect | Value |
|---|---|
| Environment | `BD_JSON_ENVELOPE=1`, `BD_NON_INTERACTIVE=1`, `BEADS_ACTOR=sc:<orchestrator-or-agent-id>` (`sc` for plumbing) |
| Inherited from the parent | `PATH`, `HOME`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, `BEADS_DIR`; nothing else |
| Repository | `-C <repo>` before the command, except `bd init` (see below) |
| Output | `--json` on every command that supports it; listings add `-n 0 --flat` |
| Timeout | 30 s per call, 2 min for `bd init` |

Output decoding (`beads.Decode`) skips any non-JSON preamble (bd's one-time
metrics notice; the text line `bd gate check` prints first) and unwraps the
`{"schema_version":1,"data":…}` envelope. Some commands print plain text
even with `--json` (`bd init`, `bd info`); their callers do not decode.

The orchestrator is the only writer and serialises calls through one
in-process queue; agents get a read-only `bd` shim (P3-09).

## Initialisation (D-13)

On the first run in a repository without `.beads/`, `sc` runs, with the
repository root as the working directory:

```
bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude -p <prefix>
```

Why these flags (all verified against bd 1.2.2): the default `bd init`
writes `CLAUDE.md`, `AGENTS.md`, `.claude/`, `.codex/`, `.agents/`,
installs five git hooks, appends to `.gitignore`, and commits `.beads/` to
the user's repository. `--skip-agents --skip-hooks` stops the files and the
hooks; `--setup-exclude` lists `.beads/` (plus `**/RECOVERY*.md` and
`**/SESSION*.md`) in `.git/info/exclude` instead of committing anything;
`--non-interactive` never prompts; `--init-if-missing` makes a repeat run a
no-op. `--stealth` is not used because it edits the user's global git
configuration.

What still happens, and what `sc` does about it:

| Side effect of `bd init` | Handling |
|---|---|
| `.beads/` created in the repository root | Kept; it is excluded from git, so `git status` stays clean |
| `.git/info/exclude` gains a "Beads fork protection" block | Kept (local-only, never committed) |
| `.gitignore` gains a `# Beads / Dolt files (added by bd init)` block (the file is created if absent) | Reverted when that block is the only change: the previous bytes are written back, or the new file is deleted. Anything else is left in place with a warning in the log and in `InitResult.GitignoreWarning` |
| `.git/config` gains `[beads] role = maintainer` | Left alone; bd reads it and it is not visible to `git status` or `git log` |
| stderr notice "No Dolt remote configured" | Informational; `sc` does not configure Dolt remotes |

`bd init` refuses `-C <dir>` on a directory that has no beads project yet
("cannot use -C directory … no beads project found"), which is why it is
the one call run with the working directory set instead. `bd init` outside
a git repository would run `git init` itself, so `sc` refuses first
(`beads.ErrNotGitRepo`).

If `bd init` fails, `sc` still reverts the `.gitignore` block and removes
the partial `.beads/` so the next attempt starts clean.

**Prefix.** bd defaults the issue prefix to the directory name; `sc` passes
it explicitly (`-p`) so ids stay stable if the directory is renamed. The
default is the directory name with runs of characters outside
`[A-Za-z0-9_-]` replaced by `_` (bd rejects a space, and turns `.` into `_`
itself); bd may still rewrite it (a leading digit gains `bd_`), and
`InitResult.Prefix` reports the value bd printed. Issue ids then look like
`<prefix>-<hash>`, for example `slop-cannon-a3f2`.

**Health check.** After init (or immediately, when `.beads/` already
exists) `sc` runs `bd -C <repo> ping --json` and requires `status: ok`.

**What `sc` never writes into the target repository:** run state, logs,
prompts, and worktrees live under `$XDG_STATE_HOME/slop-cannon/<repo-id>/`
(D-23). The one committable file `sc` may create is
`.slop-cannon/config.toml`, and only when a config command is used.

## Conventions (D-06, D-07)

Everything `sc` records about a bead is in the bead itself, so `bd show
<id> --include-comments` (or `bd export`) tells the whole story and a
resumed run rebuilds its state from beads alone. The helpers live in
`internal/beads` (`conventions.go`, `statuses.go`, `messages.go`).

### Actors

Every write names who did it through `BEADS_ACTOR`:

| Actor | Used for |
|---|---|
| `sc` | plumbing: init, status registration, metadata, labels, closes |
| `sc:<orchestrator-id>` | the orchestrator's own writes during a run |
| `sc:<agent-id>` | a message an agent produced (`beads.ActorFor`), for example `sc:eng-sc-68s-r2` or `sc:tester-qa-sc-68s-r2` |

Agents never write to beads themselves (their `bd` is read-only, P3-09);
the orchestrator records their output under their actor, so `created_by`
and comment `author` fields read as if the agent had done it.

### Labels

Labels are only for cheap filtering (`bd list -l …`); they carry no state.

| Label | Meaning |
|---|---|
| `sc:run:<run-id>` | every bead a run created, epic included (`beads.RunLabel`). `bd list -l sc:run:<id> --all` lists a whole run |
| `sc:role:<role>` | the role that last worked the bead: `po`, `engineer`, `tester` (`beads.RoleLabel`) |
| `sc:external-pr` | a review bead for a pull request the cannon did not open (D-11) |

### Metadata

Run bookkeeping goes in bead metadata under `sc_` keys (`bd:` and `_`
are reserved by beads). `sc` writes metadata through `--metadata @file`
so values keep their JSON types; `--set-metadata k=v` would store `1` as
a number too but `"true"` as a boolean, so the readers accept both
numbers and strings.

| Key | Type | Meaning |
|---|---|---|
| `sc_run` | string | run id that created the bead |
| `sc_kind` | string | `epic` for the run epic, `work` for a work bead |
| `sc_size` | string | the PO's `s`/`m`/`l` estimate |
| `sc_round` | number | review rounds completed (starts at 0) |
| `sc_agent` | string | agent id holding the claim (P3-05) |
| `sc_lease` | string | RFC 3339 UTC time the claim was last renewed (P3-05) |
| `sc_pr_url` | string | pull request opened for the bead (safety on) |
| `sc_pr_number` | number | pull request number |
| `sc_merge_sha` | string | commit that landed on the default branch |

Read with `bd show <id> --json` (`.metadata`) or filter with
`bd list --metadata-field sc_run=<id>`.

### Statuses

The workflow state is the bead's bd **status**. `open`, `in_progress` and
`closed` are bd built-ins; the rest are custom statuses `sc` registers at
the start of every run (`Client.EnsureStatuses`):

| Status | Category | Meaning |
|---|---|---|
| `open` | active | ready to be claimed; the only status `bd ready` lists |
| `in_progress` | wip | claimed (`bd update --claim`) and being implemented |
| `in_review` | wip | diff committed, testers reviewing |
| `changes_requested` | wip | a tester asked for changes |
| `approved` | wip | every assigned tester approved |
| `delivering` | wip | pushing, opening the PR or merging |
| `closed` | done | landed on the default branch |
| `escalated` | frozen | round cap hit, the PO must rule |
| `needs_human` | frozen | a person has to act |
| `failed` | frozen | abandoned after unrecoverable errors |

Registration is `bd config set status.custom "<existing>,<missing D-07
entries>"`: the repository's own custom statuses are read first
(`bd config get status.custom`), kept in place with their categories, and
only the missing `sc` entries are appended; the call is a no-op when
nothing is missing and refuses to touch a value it cannot parse. The
`wip`/`frozen` categories are what keep those beads out of `bd ready` and,
for `frozen`, out of the way; `done` would hide them from `bd list`.

`sc` never sets bd's own `blocked`, `hooked`, `deferred` or `pinned`
(`Client.SetStatus` refuses them), closes only with `bd close --reason`
(so blockers that are still open are honoured, D-20), and claims with
`bd update --claim` (atomic assignee + `in_progress`).

**Foreign statuses (P3-14).** A target repository may carry custom
statuses of its own (say `triage:active`). `bd list --all` returns those
beads too, so the strict converter (`Issue.Bead`) refuses them and the
lenient one (`Issue.SafeBead`) maps them by category to a state the
orchestrator never transitions, keeping the original name in
`Issue.Status`:

| Foreign category | Mapped state |
|---|---|
| `active` | `pinned` |
| `wip` | `blocked` |
| `frozen` | `deferred` |
| `done` | `closed` (a finished blocker still counts as satisfied) |
| unknown | `deferred` |

The cheap way to never meet them is to list by run (`Client.ListRun`,
`bd list -l sc:run:<id>`), which is what the orchestrator does.

### Claims and leases (P3-05)

An engineer owns a bead through bd's atomic claim plus two `sc_` keys:

- `Client.ClaimFor(id, agent)` is **one** call, `bd update <id> --claim
  --metadata @{sc_agent, sc_lease}` as actor `sc:<agent>`. bd refuses a
  claim (`ErrAlreadyClaimed`, or `ErrNotClaimable` for any status other
  than an unassigned `open` bead or the same actor's `in_progress` bead)
  before it writes anything, so exactly one of several concurrent callers
  wins and there is never a half-claimed bead. Claiming again as the same
  agent is idempotent and renews the lease.
- `Client.Renew(id, agent)` moves `sc_lease` forward every
  `DefaultRenewInterval` (5 min) while the agent runs; it fails with
  `ErrLeaseLost` when the bead is no longer `in_progress` and assigned to
  that agent.
- `Client.Release(id)` is `bd update <id> -s open --assignee "" --unset-metadata
  sc_agent --unset-metadata sc_lease` in one call; other `sc_` keys stay.
- `Client.Reclaim(id, agent)` is the `changes_requested → in_progress` edge
  (and the conflict round): bd's `--claim` refuses every custom status and
  the bead still carries the previous engineer as assignee, so a new round
  is a plain `bd update -s in_progress --assignee sc:<agent> --metadata …`.
  The orchestrator is the single writer (D-21), so no atomic check is
  needed there.
- `Client.RecoverStale(opts)` lists `in_progress` beads of the run and
  releases those whose lease is older than `MaxAge` (`DefaultLeaseMaxAge`,
  15 min) and whose agent is not alive; `MaxAge` 0 releases every lease
  held by a dead agent, which is what `--resume` does. Beads assigned to a
  foreign actor are never touched.

Read `sc_agent`/`sc_lease` in `bd show <id>` to see who holds a bead and
since when; a `bd history <id>` entry per claim, renewal and release shows
the hand-overs.

### The run epic

Each run owns one `epic` bead (`Client.CreateRunEpic`): title `Run
<run-id>` unless the brief gives a better one, priority 1, the `sc:run`
label, metadata `sc_run` and `sc_kind: epic`, and the brief as its
description. Work beads are its children (`--parent`, or `parent_key` in
a graph plan), so `bd ready --parent <epic>`, `bd epic status` and `bd
swarm status <epic>` show one run. On resume the epic is found with
`bd list -l sc:run:<id> --metadata-field sc_kind=epic --all`.

### Structured messages

Agents exchange structured messages as bead comments (`bd comment <id>
--stdin`, author = the agent's actor). A message comment is a one-line
human headline, a blank line and one fenced JSON block whose object
carries a `type` next to the payload's own fields:

````
Verdict (qa, round 2): changes requested, 1 item

```json
{
  "type": "verdict",
  "lens": "qa",
  "round": 2,
  "outcome": "changes",
  "items": [
    {"id": "qa-r2-1", "severity": "major", "title": "empty slice panics", "file": "stats/stats.go", "line": 12, "status": "open"}
  ]
}
```
````

The six types (D-25) and their payloads (`internal/domain`):

| `type` | Payload | Written by |
|---|---|---|
| `plan` | `Plan` (`brief`, `beads[]`) | PO, on the run epic |
| `impl_report` | `ImplReport` (`summary`, `files_changed`, `how_to_verify`, `risks`, `questions`, `commit`) | engineer |
| `verdict` | `Verdict` (`lens`, `round`, `outcome`, `summary`, `items[]`) | tester |
| `response` | `{"responses": [Response…]}` (`item_id`, `action`, `rationale`) | engineer |
| `arbitration` | `Arbitration` (`item_id`, `ruling`, `instruction`, `rationale`) | PO |
| `escalation` | `Escalation` (`outcome`, `notes`, `new_acceptance`, `summary`) | PO |

The reader (`beads.ParseMessage`, `Client.Messages`) takes the first
fenced block whose object has a `type`, tolerates prose before, between
and after fences (and a bare JSON object without a fence), ignores
comments with no such block (a human's note), and keeps a block it cannot
decode as a message with an error rather than dropping it from the
history. The writer is `Client.PostMessage(id, actor, payload)`.

### Reading a bead's history

`bd show <id> --include-comments` shows the bead and its comments in
order; `bd comments <id>` lists just the comments; `bd history <id>`
shows the Dolt commits behind every write. `sc status <bead-id>` renders
the same information as a timeline (`beads.FormatTimeline`): creation,
the claim, every message with its actor, then the close or the current
status with round, assignee and PR:

```
fx-so0  Fix Mean skipping the first value
  status closed; run r1; assignee sc:engineer-1
  2026-09-02 18:38:49                  created (bug, p1)
  2026-09-02 18:38:50  sc:engineer-1   claimed
  2026-09-02 18:38:51  sc:engineer-1   Implementation report: fixed Mean (1 file)
  2026-09-02 18:38:53  sc:tester-qa-1  Verdict (qa, round 1): changes requested, 1 item
  2026-09-02 18:38:54  sc:engineer-1   Responses: 1 item (1 fixed)
  2026-09-02 18:38:56                  closed: fixed in round 1
```

## Dolt

bd stores issues in a Dolt database (a MySQL-compatible engine with
Git-style commits) under `.beads/`. Everything below is verified against
bd 1.2.2 (Appendix A of `IMPLEMENTATION-PROGRESS.md`) and implemented in
`internal/beads/dolt.go`.

**Embedded mode is the only supported mode.** `bd init` writes
`.beads/metadata.json` with `"dolt_mode": "embedded"` and keeps the data
under `.beads/embeddeddolt/`; the engine runs inside the `bd` process, so
no `dolt` binary and no server are involved (`bd dolt status` prints `Dolt
engine: embedded (in-process, no server)`). `beads.ReadProjectMetadata`
reads that file without running bd, and `bd context --json`
(`beads.ReadContext`, `Client.Context`) reports the same `dolt_mode` plus
`beads_dir`, `repo_root`, `database`, `project_id`, `role` and
`bd_version`. bd's server mode (`dolt sql-server`, the multi-writer path,
`BEADS_DOLT_SERVER_MODE`) needs the standalone `dolt` CLI and has not been
tested with `sc`: when `bd context` reports it, `Client.CheckDolt` logs
`beads reports Dolt mode "server"; sc has only been tested against embedded
mode and continues anyway` and the run goes on. A standalone `dolt` on the
machine (2.3.1 here) is irrelevant to embedded mode, which links its own
engine. `--server` support is a discovered task for the day a user needs
multi-writer access.

**Single writer (D-21).** The embedded engine takes an exclusive `flock` on
its data directory. A second `bd` process does not fail: bd retries the
open with exponential backoff until its context is cancelled, so
concurrent writers queue (~0.2 s each). `sc` never relies on that queue:
the orchestrator is the only writer, every call goes through one
`beads.Client` (or clients derived with `WithActor`, which share its
mutex), and agents only get the read-only shim. Contention with a human
running `bd` by hand shows up as a slower call, or as a timeout after 30 s
(`ErrLocked` is matched on lock messages for future bd versions only).

**Every write is a Dolt commit.** `dolt.auto-commit` defaults to `on` in
embedded mode (`bd config get dolt.auto-commit` prints `on` even though
`config.yaml` has no such line), so each `bd create/update/comment/dep
add/close/config set` is committed as it happens. Nothing is ever pending:
`sc` never runs `bd dolt commit`, `bd dolt push`/`pull` (there is no Dolt
remote), or any sync step, and a crash loses nothing that a call had
already returned. `Client.CheckDolt` verifies this at run start: it runs
`bd context --json` and `bd config get dolt.auto-commit --json`, reads
`metadata.json`, and warns (without stopping) when the mode is not
embedded, when `metadata.json` disagrees with bd, or when auto-commit is
`off` or `batch` (`dolt.auto-commit is "off", not "on": writes are not
Dolt commits until `bd dolt commit`…`). Do not set
`--dolt-auto-commit off|batch` or `bd config set dolt.auto-commit` on a
repository the cannon fires at.

**Debugging with `bd history`.** `bd history <id>` (and `--json` through
`Client.History(id, limit)`) lists the Dolt commits made since the bead was
created, newest first, each with the bead's core columns (`id`, `title`,
`status`, `priority`, `issue_type`, `owner`, timestamps) as of that commit.
Note that it shows every commit made after the bead existed, whichever
bead the write touched, not only commits that changed this bead; with
auto-commit on the count therefore grows by exactly one per `sc` write to
the database, which the tagged integration test asserts. An unknown id
prints `data: null` with exit 0; the client reports it as `ErrNotFound`.
Entries look like:

```
📜 History for fx-fbq (2 entries)

66iccmnv 2026-09-02 20:02:53
  Author: root
  ◐ fx-fbq: Add Median [P2 - in_progress]

3pl4ohrf 2026-09-02 20:02:53
  Author: root
  ◐ fx-fbq: Add Median [P2 - in_progress]
```

The author is Dolt's committer (`root` in embedded mode); the `sc` actor
of a change is in the bead's `created_by`, `assignee` and comment authors
(D-06), not in the Dolt commit.

## Metrics

bd sends anonymous usage metrics by default and prints a one-time notice
about it. `sc` leaves that setting alone; `bd metrics off` disables it.
