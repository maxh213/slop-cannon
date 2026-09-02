# Slop Cannon — Implementation Progress

Source of truth for building `sc` from `README.md`. Multiple agents work this file in a loop.
Read the **Agent Protocol** and **Decisions** sections before touching any task.

Last regenerated from README: 2026-09-02.

---

## Agent Protocol

1. **Read** this whole file, then `README.md`, then `docs/` (if present). Never work from memory of an older version.
2. **Pick** the first `[ ]` task, lowest phase first, whose `deps` are all `[x]`. Tasks in different phases may run in parallel when their deps allow it (see *Parallelism* below).
3. **Claim** it: change `[ ]` to `[~]` and append `(claimed: <agent-id> <ISO-8601 UTC>)` to the task line. Commit immediately with message `claim <TASK-ID>`. One task per agent at a time.
4. **Implement** it. Every task ends with `make check` green (build + lint + tests). Add tests with the code, not later.
5. **Finish**: change `[~]` to `[x]`, replace the claim note with `(done: <agent-id> <commit-sha>)`, and add a one-line note if you deviated from the task text. Commit with message `<TASK-ID>: <short summary>`.
6. **Blocked**: change to `[!]` with `(blocked: <reason>)`. Pick another task. Do not silently skip.
7. **Stale claims**: a `[~]` task with no commits referencing it for 4 hours may be reclaimed. Note the takeover on the task line.
8. **New work you discover** goes in *Discovered Tasks* at the bottom with the next free ID in the relevant phase (e.g. `P3-08`). Never renumber or delete existing IDs.
9. **Deviations from a Decision** require a new entry in *Decisions* (mark the old one superseded). Do not quietly change architecture.
10. **Do not** edit files that another agent's `[~]` task obviously owns. If you must, coordinate via a note on their task line.
11. A phase is complete when every task in it is `[x]` and its *Exit criteria* pass. Update the *Phase Status* table when that happens.

**Status legend**: `[ ]` todo · `[~]` in progress · `[x]` done · `[!]` blocked
**Size**: S (< 1 hour of focused work), M (1–3 h), L (3–8 h). Split any L you cannot finish in one loop iteration into discovered subtasks.

**Global definition of done** for every task:
- Code compiles, `make check` passes, new behaviour has unit tests.
- Public functions have doc comments. No `TODO` without a task ID next to it.
- User-facing behaviour is reflected in `docs/` or `--help` text.
- No secrets, no absolute paths from your machine, no commented-out code.

---

## Phase Status

| Phase | Name | Status | Notes |
|---|---|---|---|
| 0 | Foundations & domain model | done (round 1, wf-p0, `be10e4e`) | `make check` green locally; `go run ./cmd/sc version` prints; coverage domain 100%, events 97.7%. CI green-on-push not yet observed (no git remote); workflow validated with actionlint v1.7.12. |
| 1 | CLI & configuration | not started | |
| 2 | Cartridges (LLM CLI adapters) | not started | |
| 3 | Beads integration (task graph) | not started | |
| 4 | Git, worktrees, PRs & safety | not started | |
| 5 | Agent roles & prompts | not started | |
| 6 | Orchestrator & feedback loops | not started | |
| 7 | TUI & cannon animations | not started | |
| 8 | Modes & end-to-end wiring | not started | |
| 9 | Hardening & guardrails | not started | |
| 10 | Docs, packaging & release | not started | |

**Parallelism**: Phase 0 first. Then Phase 1. After that, Phases 2, 3, 4 are independent of each other. Phase 5 needs only Phase 2's interface and mock (P2-01, P2-02). Phase 7 can start once P0-05/P0-06 exist, using the fake event generator (P7-01). Phase 6 needs 2, 3, 4, 5. Phases 8–10 are sequential after 6 and 7.

---

## Glossary (README term → code concept)

| README term | Meaning in code |
|---|---|
| Cannon / firing at a repo | One orchestrator **Run** against a target git repository |
| Ammo | Concurrency budget: maximum number of agent subprocesses alive at once (minimum 3) |
| Cartridge | An LLM CLI backend adapter plus its model. Only Claude Code for now (D-19); the design is pluggable and more cartridges (Kimi, Animus, custom commands, others) will be added in future |
| Bead | One unit of work: an issue in the beads (`bd`) tracker inside the target repo. `bd` embeds Dolt in-process; no separate `dolt` binary is needed (Appendix A) |
| Engineer | Implementor agent; owns a bead in its own git worktree |
| UX / QA / Tester | Reviewer agents; three **lenses**: `ux`, `qa`, `test` |
| Product owner / BA | Planner agent: researches the repo, creates beads with dependencies, arbitrates disputes |
| Feedback loop | Review rounds on a bead until every assigned tester approves |
| Vibes | Style preference files appended to every agent prompt (soft guidance) |
| Rules | Hard constraint files appended to every agent prompt (MUST/MUST NOT) |
| Safety on | Finished work is pushed as a branch and opened as a PR; humans merge |
| Safety off | Finished work is rebased and merged straight into the default branch |
| Brief (`-m`) | A human-written description that the PO decomposes into beads |
| Autopilot | No brief: the PO researches the repo and invents the work itself |

---

## Decisions

Recorded defaults so agents never block on unspecified points. Change only via a new numbered entry.

- **D-01 Language & stack**: Go 1.23+. CLI: `spf13/cobra`. TUI: `charmbracelet/bubbletea`, `lipgloss`, `bubbles`, `harmonica` (spring animations). Config: TOML via `pelletier/go-toml/v2`. Logging: `log/slog`. Rationale: `bd` and Dolt are Go, single static binary, best-in-class TUI animation support. Alternative considered: TypeScript + Ink (the `.idea` module is a generic WEB_MODULE; not treated as a stack signal). If the human overrides this, record D-01a and adjust P0-01. **Confirmed 2026-09-02 (P0-01)**: module path `slop-cannon` (no git remote, so no owner), `go 1.27.0` in `go.mod`; pinned cobra v1.10.2, bubbletea v1.3.10, lipgloss v1.1.0, bubbles v1.0.0, harmonica v0.2.0, go-toml/v2 v2.4.3, plus testify v1.12.1 and `charmbracelet/x/exp/teatest` for tests.
- **D-02 Module layout**: `cmd/sc/` (main), `internal/cli`, `internal/config`, `internal/domain`, `internal/events`, `internal/proc`, `internal/cartridge/{claude,mock}`, `internal/beads`, `internal/gitx`, `internal/agents`, `internal/orchestrator`, `internal/tui`, `internal/logging`, `testdata/`, `docs/`.
- **D-03 Config precedence**: CLI flags > repo config `<repo>/.slop-cannon/config.toml` > global config `$XDG_CONFIG_HOME/slop-cannon/config.toml` (default `~/.config/slop-cannon/`) > built-in defaults. Cartridges live in global config only (they are subscriptions). `ammo`, `safety`, `vibes`, `rules` may live in either; config subcommands write to the repo config when cwd is inside a git repo, else global; `--global` forces global.
- **D-04 Built-in defaults** (from README): `safety = true`, `ammo = 10`, `vibes = []`, `rules = []`, `cartridges = []` (a run refuses to start with no cartridge).
- **D-05 Ammo allocation**: `ammo` is a concurrency cap, not a fixed roster. Logical targets: PO = 1 slot when needed; `max_engineers_in_flight = max(1, floor((ammo-1)/4))`; every remaining slot is available to testers so each engineer can have 3 testers (README's ideal ratio). Scheduler priority when slots free up: arbitration > reviews awaiting testers > implementation of ready beads > planning. Minimum ammo 3 = one of each role.
- **D-06 Bead state lives in beads** (revised 2026-09-02 after verifying bd 1.2.2, see Appendix A): workflow state is the bead's **status**, using bd's native custom-status feature (`bd config set status.custom …`, D-07). Run bookkeeping goes in bead **metadata** with `sc_` keys (`sc_run`, `sc_round`, `sc_pr_url`, `sc_pr_number`, `sc_merge_sha`, `sc_agent`, `sc_lease`, `sc_kind`); `bd:` and `_` metadata prefixes are reserved by beads. Labels are used only for cheap filtering: `sc:run:<run-id>`, `sc:role:<role>`, `sc:external-pr`. Structured agent messages are bead comments written with `--actor sc:<agent-id>` and `bd comment <id> --stdin`, containing one fenced JSON block with a `type` field (`impl_report`, `verdict`, `response`, `arbitration`, `plan`). Nothing that matters for resume is kept only in memory.
- **D-07 Bead states** (revised 2026-09-02): logical states map 1:1 onto bd statuses. Built-in: `open` (ready/claimable), `in_progress` (claimed + implementing, set atomically by `bd update --claim`), `closed` (done). Custom, registered at run start with `bd config set status.custom "in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen"` (merged with any custom statuses the repo already has, never replacing them): `in_review`, `changes_requested`, `approved`, `delivering`, `escalated` (PO arbitration), `needs_human`, `failed`. The `wip`/`frozen` categories keep these beads out of `bd ready` automatically. bd's own `blocked` and `deferred` statuses are never set by `sc`. Flow: `open → in_progress → in_review → (changes_requested → in_progress …) → approved → delivering → closed`. Transitions are pure functions in `internal/orchestrator/state.go`.
- **D-08 Feedback loop limits**: `max_review_rounds = 5`, `max_pushback_rounds = 2` per item, then PO arbitration; PO ruling is binding. Round cap without approval → `escalated`; PO may approve-with-notes, narrow scope, or mark `blocked` for a human.
- **D-09 Which testers re-review**: only testers who did not approve in the previous round re-review. An approval stands unless the engineer's diff changes more than `re_review_threshold` (default 40% of changed lines), in which case all three re-review.
- **D-10 Safety semantics**: on → push `sc/<bead-id>-<slug>` and open a PR with `gh`; never touch the default branch. off → rebase onto default branch, run the repo's `check_command` if configured/detected, fast-forward merge, push if a remote exists. Merge conflicts send the bead back to the engineer as a `conflict` round (does not count against `max_review_rounds`).
- **D-11 External PRs**: when `gh` and a remote exist, the cannon also reviews open PRs it did not create: one review bead per PR, testers post a consolidated review via `gh pr review`. It never merges a PR it did not create, regardless of safety.
- **D-12 Cartridge selection**: the first cartridge added becomes active; `sc cartridge --use NAME` switches. Optional per-role override in config (`[roles.tester] cartridge = "<name>"`); with only Claude supported (D-19) the override is parsed and validated but has no practical effect until a second adapter exists. Every cartridge is invoked as a subprocess in the agent's worktree; no SDKs.
- **D-13 Beads init** (revised 2026-09-02): if the target repo has no `.beads/`, `sc` runs `bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude -p <prefix>` and logs it (autopilot never prompts). Reasons, all verified live: the default `bd init` writes CLAUDE.md, AGENTS.md, `.claude/`, `.codex/`, `.agents/`, installs five git hooks, and **auto-commits** to the user's repo; `--skip-agents --skip-hooks` stops the files and hooks but still commits `.beads/` config; `--setup-exclude` puts `.beads/` in `.git/info/exclude` so nothing is committed; `--stealth` also edits the user's global git config and is not used. `bd init` still appends a "Beads / Dolt files" block to the repo `.gitignore`; `sc` reverts that hunk when it is the only change to `.gitignore`, otherwise leaves it and warns. `sc` writes nothing else into the target repo: run state and worktrees live outside it (D-23). `.slop-cannon/config.toml` is the one committable file and is created only when a config command is used. Prefix defaults to the directory name; `sc` passes it explicitly so it is stable.
- **D-14 Agents commit their own work**; the orchestrator auto-commits any leftovers after an engineer exits (`sc(<bead>): round <n> leftovers`) so testers always review a committed diff.
- **D-15 Headless is a first-class mode** (`--headless`): identical run, events rendered as log lines / JSONL. CI and the dogfooding task use it.
- **D-16 File name**: this file is `IMPLEMENTATION-PROGRESS.md` (lowercase extension, matching `README.md`).
- **D-17 Animus is deferred** (human decision, 2026-09-02): no adapter, no module, not a known cartridge name. `sc cartridge --add animus` fails with `cartridge "animus" is not supported yet`. Revisit only when the human reopens it. Superseded in scope by D-19, which defers every non-Claude adapter the same way.
- **D-18 Autopilot never stops on its own** (human decision, 2026-09-02). An autopilot run ends only when: (a) the user stops it (`s` key, `sc stop`, or SIGINT/SIGTERM), or (b) every configured cartridge is **out of credits**. There is no bead cap and no runtime cap. Out of credits means the cartridge CLI reports quota/billing/credit exhaustion (a distinct error kind from a rate limit, which only triggers backoff). When the ready queue is empty the PO plans more work; when the PO has nothing to add the run idles and re-plans after `idle_replan_interval` (default 10 min). Optional cost/token caps, if the user sets them, **pause** the run with a banner rather than finishing it. Brief mode still finishes when all its beads are settled.
- **D-20 Dependencies gate delivery** (2026-09-02, from bd semantics): `bd ready` only lists beads whose blockers are `closed`, and `bd close` refuses a bead whose blockers are still open. Therefore a bead is closed only when its code has landed on the default branch. Safety off: rebase + fast-forward merge, then `bd close`. Safety on: the bead stays in `delivering` after the PR opens, and `sc` creates a native gate (`bd gate create --type=gh:pr --blocks <bead> --await-id <pr-number>`), polls `bd gate check --type=gh:pr` (which shells out to `gh pr view`), and closes the bead when the gate resolves; dependents therefore wait for the human merge, which is the point of safety. A PR closed without merge escalates the gate and moves the bead to `needs_human`. Dependency direction rule for every writer: `bd dep add <issue> <depends-on>`; never use `bd create --deps blocks:<id>` (verified to create the reverse edge); plain `--deps <id>` or the graph plan `deps` field mean "depends on".
- **D-21 bd invocation contract** (2026-09-02): the orchestrator is the **only** beads writer and serialises all `bd` calls through one in-process queue (embedded Dolt is single-writer: the engine takes an exclusive `flock` on the data directory, and beads opens it with unbounded exponential backoff, so a second process waits rather than fails; 30 parallel writers succeeded in testing but queued at ~0.2 s each, and serialising in-process keeps latency predictable and avoids stacking blocked processes). Every call sets `BD_JSON_ENVELOPE=1` (future-proof `{"schema_version":1,"data":…}` shape, errors as `{"error","code","hint"}`), `BD_NON_INTERACTIVE=1`, `BEADS_ACTOR=sc:<orchestrator-or-agent-id>`, `-C <repo>`, `--json`, and `-n 0 --flat` on listings (defaults truncate at 50/100 and render trees). The parser skips any non-JSON preamble (bd prints a one-time metrics notice). Minimum version `1.2.2` (latest release, verified 2026-08-15); features documented upstream but absent in 1.2.2 (events journal, `revision` field) are not relied on. Agents never write to beads: each worktree gets a `bd` shim first on `PATH` that execs the real binary with `--readonly` (P3-09), and prompts say so.
- **D-22 Plans are created atomically with `bd create --graph`** (2026-09-02): the PO's plan JSON is converted to bd's graph-plan schema (`nodes[]` with `key,title,type,priority,description,acceptance_criteria,labels,metadata,deps[{target}]`, plus `parent_key` for the run epic) and applied with `--dry-run` first, then for real; the result maps plan keys to minted IDs. Priorities use bd's scale, `0` (highest) to `4`, default `2`. Each run owns one `epic` bead; work beads are its children so `bd ready --parent <epic>` and `bd swarm status <epic>` show the run. *Schema detail superseded by D-24: node-level `deps` and `acceptance_criteria` are not accepted by bd 1.2.2.*
- **D-23 Nothing of ours lives inside the target repo** (2026-09-02): run state, logs, prompts, and worktrees go under `$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/<run-id>/` (default `~/.local/state/slop-cannon/…`), worktrees at `…/wt/<bead-id>`. This avoids `.gitignore` edits, nested worktrees confusing the user's tools, and dirty-tree noise. Supersedes the `.slop-cannon/run/` paths in D-13/D-15 and Phase 6/7 tasks; treat every `.slop-cannon/run/<id>/` mention below as `<state-dir>/runs/<id>/`.
- **D-19 Claude is the only adapter for now** (human decision, 2026-09-02): the only known cartridge name is `claude`. `sc cartridge --add <anything else>` fails with `cartridge "<name>" is not supported yet (only "claude" for now)`. More cartridges are explicitly planned for the future. The `Cartridge` interface, registry, mock, and structured-output contract stay adapter-agnostic so Kimi, Animus, a custom-command adapter, or any other CLI can be added later by dropping in one package and one registry line, without touching the orchestrator (see P2-10 for the guide). Multi-cartridge fallback in the supervisor (P6-07) is implemented against the interface and tested with two mock cartridges.
- **D-24 Graph-plan dependencies go in top-level `edges[]`** (2026-09-02, supersedes the node schema detail of D-22; evidence captured during P0-09 in `internal/testutil/testdata/bd/create-graph-dry-run.{json,stderr}`): bd 1.2.2 rejects node-level `deps` and `acceptance_criteria` as unknown fields (`warning: graph plan node["max"] has unknown field(s): [acceptance_criteria deps] (silently dropped …)`) and the dry run reports `edge_count: 0`. P3-10 must therefore emit every dependency as `edges[{from_key,to_key,type:"blocks"}]` (from = the dependent bead, to = its blocker, matching `bd dep add <issue> <depends-on>`), verify the edge count in the `--dry-run` result equals the plan's dependency count, and set acceptance criteria with `bd update --acceptance` after creation unless P3-11 finds the accepted node field name. Any `warning:` line on the dry-run stderr fails the plan build. The remaining D-22 rules (epic parent, `--dry-run` first, key→id mapping, priorities) stand.

---

## Open Questions for the Human (assumed defaults in force until answered)

- **Q-01** Is Go acceptable, or do you want TypeScript/Ink? *(Assumed: Go, D-01.)*
- **Q-02** ~~What is the "animus" cartridge?~~ Resolved 2026-09-02: ignore for now (D-17).
- **Q-03** ~~Should autopilot ever stop on its own?~~ Resolved 2026-09-02: never, except user stop or all cartridges out of credits (D-18).
- **Q-04** Should the PO be allowed to close/deprioritise beads it created earlier? *(Assumed: yes, in autopilot only.)*
- **Q-05** Does "ammo" count the PO, or only engineers + testers? *(Assumed: counts the PO, D-05.)*
- **Q-06** bd sends anonymous usage metrics by default (`bd metrics off` disables). `sc` leaves that setting alone and documents it. Say so if you want `sc doctor` to warn about it.

---

## Phase 0 — Foundations & Domain Model

**Goal**: the repo builds, tests run in CI, and the shared types every other phase depends on exist.
**Exit criteria**: `make check` green locally and in CI; `go run ./cmd/sc version` prints a version; `internal/domain` and `internal/events` have ≥ 80% coverage.

- [x] **P0-01 — Confirm stack and record it** (deps: none; size: S) (done: wf-p0 be10e4e) — no git remote, so module path is `slop-cannon`; `cmd/sc/deps.go` (build tag `deps`, never compiled) pins the D-01 libraries in go.mod until real packages import them (see P0-10).
  - Do: read D-01. If nothing in this file overrides it, create `go.mod` (module path `github.com/<owner>/slop-cannon`; use the git remote owner, else `slop-cannon`). Pin Go version. Add the D-01 libraries as dependencies with `go get`.
  - Done when: `go build ./...` succeeds on an empty `cmd/sc/main.go`; D-01 is marked confirmed here with the go.mod module path.
- [x] **P0-02 — Repo scaffolding** (deps: P0-01; size: S) (done: wf-p0 be10e4e) — `internal/cli` is a minimal cobra root + `version` subcommand (not just `doc.go`) so `go run ./cmd/sc version` works; P1-01 extends it and must keep the `cli.Version/Commit/Date` ldflags variables (P1-10). LICENSE holder is "Slop Cannon contributors". `.idea/` was already tracked by the human's initial commit and is left as is.
  - Do: create the D-02 directory layout with `doc.go` files; `.gitignore` (`.idea/`, `bin/`, `dist/`, `.slop-cannon/run/`, `*.log`); `.editorconfig`; `LICENSE` (MIT unless the human says otherwise); `cmd/sc/main.go` that calls `cli.Execute()`.
  - Done when: tree matches D-02; `git status` is clean after `make build`.
- [x] **P0-03 — Makefile** (deps: P0-02; size: S) (done: wf-p0 be10e4e) — `make lint` installs golangci-lint v2.13.2 into `./bin` on first use; extra targets `tools` and `help`.
  - Do: targets `build` (to `bin/sc`, ldflags inject version/commit/date), `test` (`go test -race ./...`), `lint` (`golangci-lint run`, config in `.golangci.yml` with `govet`, `staticcheck`, `errcheck`, `revive`), `fmt` (`gofmt -l` check), `check` (fmt + lint + test), `install`, `integration` (tests tagged `integration`), `clean`.
  - Done when: `make check` passes; README section "Development" lists the targets.
- [x] **P0-04 — CI workflow** (deps: P0-03; size: S) (done: wf-p0 be10e4e) — validated with actionlint v1.7.12; "green on first push" is unverified because the repo has no remote yet.
  - Do: `.github/workflows/ci.yml` running `make check` on push and PR, matrix `ubuntu-latest` + `macos-latest`, Go from `go.mod`, module cache enabled. Integration tests are a separate job that skips if `bd` is unavailable.
  - Done when: workflow file lints with `actionlint` (or is reviewed by a second agent) and runs green on the first push.
- [x] **P0-05 — Domain model package** (deps: P0-02; size: M) (done: wf-p0 be10e4e) — also defines `StateCategory`, `BeadType`, `Size`, `AgentState`, `ItemStatus`, `EscalationOutcome`/`Escalation`, `MessageType`, a typed `ConfigSnapshot` (P1-02 may extend), and a pure `Plan.Validate()` that P5-02 should reuse; `ParseX("")` errors while `UnmarshalText("")` yields the zero value.
  - Do: `internal/domain`: `Role` (`po`, `engineer`, `tester`), `Lens` (`ux`, `qa`, `test`), `BeadID`, `Bead` (id, title, description, acceptance, priority, size, deps, state, assignee, labels, round, pr_url), `BeadState` per D-07, `Agent` (id, role, lens, cartridge, model, bead, state, started_at, pid), `Run` (id, repo_path, mode `brief|autopilot`, brief, started_at, config snapshot), `Verdict` (`approve|changes`, items), `FeedbackItem` (id, severity `blocker|major|minor|nit`, title, detail, file, line, suggestion, status), `Response` (item_id, action `fixed|pushback|wontfix`, rationale), `Arbitration`, `ImplReport`, `Plan`. All enums have `String()`, `Parse()`, JSON marshal/unmarshal.
  - Done when: table-driven tests cover every enum and a JSON round trip of every struct; `go doc ./internal/domain` reads coherently.
- [x] **P0-06 — Event model and bus** (deps: P0-05; size: M) (done: wf-p0 be10e4e) — events are value types; `Meta` JSON field is `run_id`; `Subscribe` returns a `*Subscription` (`Events()/Dropped()/Done()/Unsubscribe()`); extras: `Kinds()/New()` for P7-01, `JSONLReader/ReadAll` for P6-08, `FinishReason`/`Failure`/`RoundKind` enums; the bus never transforms events, so redaction happens at construction (P9-09).
  - Do: `internal/events`: an `Event` interface with `Kind()`, `At()`, `RunID()` and concrete events: `RunStarted/Paused/Resumed/Stopped/Finished`, `PlanProduced`, `BeadCreated/Updated/StateChanged`, `GraphChanged`, `AgentSpawned/Output/Finished/Failed`, `RoundStarted`, `VerdictReceived`, `ResponseReceived`, `ArbitrationReceived`, `PROpened`, `Merged`, `Warning`, `Error`. A `Bus` with `Publish` and `Subscribe(ctx, buffer)`; slow subscribers drop with a counter, never block publishers. A `JSONLWriter` subscriber.
  - Done when: tests prove ordering per subscriber, non-blocking publish, JSONL round trip of every event kind.
- [x] **P0-07 — Logging** (deps: P0-06; size: S) (done: wf-p0 be10e4e) — per D-23 the caller passes the run dir: `OpenRunLog(runDir, runID, opts)` writes `<runDir>/sc.log` and `<runDir>/agents/<agent-id>.log` (0600/0700); also `NewMultiHandler` (headless stderr+file) and `LogEvent` (event→log bridge); CLI wiring is P1-11.
  - Do: `internal/logging`: `slog` setup with text handler for terminals and JSON handler for files; per-run log at `.slop-cannon/run/<run-id>/sc.log`; per-agent log at `.../agents/<agent-id>.log`; `--verbose` raises level. Redaction hook for values that look like tokens (`sk-…`, `ghp_…`, `Bearer …`).
  - Done when: a test asserts redaction and file placement.
- [x] **P0-08 — Process runner** (deps: P0-02; size: M) (done: wf-p0 be10e4e) — `Stream` returns `*Handle`; line source enum is `Source` (`Stdout`/`Stderr`); `Spec.GracePeriod` gives SIGTERM-then-SIGKILL; non-zero exit is `*ExitError` with `Result` populated and `errors.Is` on `ErrTimeout`/`ErrKilled`/context causes; unix-only tests are build-tagged, `kill_other.go` is the non-unix fallback.
  - Do: `internal/proc`: `Run(ctx, Spec) (Result, error)` and `Stream(ctx, Spec) (Handle, error)` where `Spec` = command, args, dir, env (explicit allowlist + `PATH`), stdin, timeout. Streams stdout/stderr line by line; kills the whole process group on cancel; captures exit code; records duration. `LookPath` helper with version probing.
  - Done when: tests cover timeout kill, cancellation kill of a child that spawns a grandchild, stdout/stderr interleaving, and non-zero exit.
- [x] **P0-09 — Fixture repo generator** (deps: P0-02; size: M) (done: wf-p0 be10e4e) — bd samples (45 files, captured from bd 1.2.2 with `BD_JSON_ENVELOPE=1`) live at `internal/testutil/testdata/bd/` (`BDSamplesDir(t)`), not root `testdata/bd/`; P3-02 should use them from there. `FakeBD(t, dir)`/`BDKey`/`StageBDSamples` replay them; `FakeBin` uses `t.Setenv` so it cannot be used from `t.Parallel` tests. The fixture's one planted TODO (Median) intentionally has no task id.
  - Do: `internal/testutil`: `NewFixtureRepo(t)` creates a temp git repo from `testdata/fixture-repo/` (a small Go or shell project with a Makefile `test` target, two intentional bugs, one TODO, a README) with an initial commit and a `main` default branch; optional bare remote. Also `FakeBin(t, name, script)` that puts a shim executable on `PATH` for `bd`, `gh`, `git` interception, recording invocations to a file. The `bd` shim replays envelope-shaped JSON (D-21) captured from real bd 1.2.2 into `testdata/bd/`.
  - Done when: fixture builds and its tests fail exactly on the planted bugs; shim records invocations in order.

---

## Phase 1 — CLI & Configuration

**Goal**: every command in the README parses, validates, persists, and prints sensibly, without any orchestration yet.
**Exit criteria**: all README example commands succeed or fail with the intended message; `sc config show` displays merged config with sources; `sc doctor` exits non-zero on a machine without `bd`.

- [ ] **P1-01 — Root command and global flags** (deps: P0-03; size: S)
  - Do: `internal/cli`: cobra root `sc` with `--repo PATH` (default cwd), `--global`, `--verbose`, `--json`, `--no-color`; `sc version`; help text whose examples mirror the README block verbatim. Unknown subcommand suggests nearest.
  - Done when: `sc --help` and `sc version` work; help golden test.
- [ ] **P1-02 — Config schema, load, merge, save** (deps: P1-01, P0-05; size: M)
  - Do: `internal/config`: TOML schema (`[cartridges.<name>] model, command, args, env`, `active_cartridge`, `ammo`, `safety`, `vibes[]`, `rules[]`, `check_command`, `limits.*` from D-08/D-18, `idle_replan_interval`, `[roles.<role>] cartridge` (parsed per D-12, inert until a second adapter exists)). `Load(repoPath) (Effective, error)` implementing D-03 with per-key source tracking; `SaveGlobal`, `SaveRepo`; atomic writes; schema version key with migration hook. Defaults per D-04.
  - Done when: tests cover precedence for every key, missing files, malformed TOML error message with line number, atomic write; `docs/config.md` documents every key.
- [ ] **P1-03 — `sc cartridge` commands** (deps: P1-02; size: M)
  - Do: `--add NAME [--model M]`, `--remove NAME`, `--list`, `--use NAME`. Known name: `claude` only (D-19). Any other name, including `kimi` and `animus`, is rejected with the D-19 message. Adding the first cartridge sets it active; removing the active one promotes the next or leaves it blank with a warning. Always writes global config (D-03).
  - Done when: README examples `sc cartridge --add claude --model fable` and `--remove claude` pass an integration test on a temp `XDG_CONFIG_HOME`; `--add kimi --model 3` and `--add animus` fail with the D-19 message.
- [ ] **P1-04 — `sc ammo N`** (deps: P1-02; size: S)
  - Do: validate integer ≥ 3 (error: `ammo must be at least 3: one engineer, one tester, one product owner`); print the D-05 allocation it implies (`ammo 12 → 1 PO, up to 2 engineers, up to 9 testers`).
  - Done when: tests for 2, 3, 10, 12, non-integer.
- [ ] **P1-05 — `sc vibes` and `sc rules`** (deps: P1-02; size: S)
  - Do: `--add FILE`, `--remove FILE`, `--list`, `--show` (prints concatenated content). Store paths relative to the config file's directory when inside it, else absolute. Validate the file exists and is UTF-8 text ≤ 64 KiB; dedupe.
  - Done when: README examples `sc vibes --add vibes.md`, `sc rules --add rules.md` pass tests; removing a non-listed file is a clear error.
- [ ] **P1-06 — `sc safety` and `sc config`** (deps: P1-02; size: S)
  - Do: `sc safety on|off` (also accepts `true/false`, prints the D-10 meaning). `sc config show` prints effective config with a source column; `sc config path` prints both file paths; `sc config edit` opens `$EDITOR`.
  - Done when: golden test of `config show` output with `--json` and without.
- [ ] **P1-07 — `sc doctor`** (deps: P1-02, P0-08; size: M)
  - Do: checks with pass/warn/fail rows: `git ≥ 2.30`; `bd` present and `bd version --json` ≥ 1.2.2 (fail otherwise, hint: `brew install beads` or the upstream install script; note that `CGO_ENABLED=0 go install` builds are server-mode only and cannot run embedded Dolt); embedded capability confirmed by `bd ping --json` when `.beads/` exists (the standalone `dolt` binary is **not** required and is not checked); `gh` present + authenticated (warn only; required for safety-on gates and external PR review); each configured cartridge CLI present (fail if active one missing); repo at `--repo` is a git repo with a clean tree (warn); `.beads/` present (warn: will be initialised per D-13). Exit 1 on any fail.
  - Done when: tests using `FakeBin` shims for each row; output golden.
- [ ] **P1-08 — Run command parsing (no orchestration)** (deps: P1-03…P1-06; size: M)
  - Do: `sc <path> [-m|--message BRIEF] [--headless] [--resume] [--ammo N] [--safety on|off] [--cartridge NAME] [--no-anim] [--review-prs=true|false]`. Resolve `RunConfig` (domain.Run + effective config). Refuse when no cartridge is active with: `no cartridge loaded — add one with: sc cartridge --add claude --model <model>`. Refuse when path is not a git repo. For now print the resolved RunConfig and exit 0.
  - Done when: tests for all flag/config interactions; README examples `sc ./repo-path` and `sc ./repo-path -m "..."` resolve correctly.
- [ ] **P1-09 — Completions and CLI docs** (deps: P1-08; size: S)
  - Do: `sc completion bash|zsh|fish`; `make docs-cli` generates `docs/cli.md` from cobra.
  - Done when: generated doc is committed and a test fails if it is stale.

---

## Phase 2 — Cartridges (LLM CLI Adapters)

**Goal**: a uniform way to fire any supported LLM CLI at a prompt in a working directory and get structured results back. Claude Code is the only adapter in this phase; the abstraction exists so more cartridges can be added in future (D-19).
**Exit criteria**: mock cartridge drives all orchestrator tests; the Claude adapter passes the conformance suite on a machine with Claude Code installed; its flags are verified against the installed CLI's `--help` and recorded in `docs/cartridges.md`. Deferred adapters (P2-04, P2-05, P2-06) do not block phase completion.

- [ ] **P2-01 — Cartridge interface and registry** (deps: P0-05, P0-08; size: M)
  - Do: `internal/cartridge`: `type Cartridge interface { Name() string; Detect(ctx) (Status, error); Run(ctx, RunSpec) (Session, error) }`. `RunSpec`: system prompt, user prompt, workdir, model, tool policy (`read_only|read_exec|read_write`), max turns, timeout, env allowlist, labels. `Session`: `Events() <-chan SessionEvent` (`TextDelta`, `ToolUse`, `ToolResult`, `Done{Result, Usage}`, `Error`), `Wait()`, `Kill()`. `Usage`: input/output tokens, cost USD if reported. Typed error kinds on `Error`/`Detect`: `ErrRateLimited` (temporary, backoff), `ErrCreditsExhausted` (quota/billing exhausted, cartridge is dead for this run, D-18), `ErrAuth`, `ErrCrash`, `ErrTimeout`; each adapter documents which CLI messages map to which kind. Registry keyed by name; construction from config.
  - Done when: interface documented; a compile-time check for each adapter; registry tests.
- [ ] **P2-02 — Mock cartridge** (deps: P2-01; size: M)
  - Do: `cartridge/mock`: scripted by a YAML/JSON scenario file or programmatic builder: match on label (role, lens, bead id, round) → canned events with delays; failure injection (crash, timeout, malformed JSON, rate-limit, credits-exhausted); optional side effects in workdir (write file, run command) so engineer runs produce real diffs in tests. Records every RunSpec it received.
  - Done when: tests demonstrate matching, delays under a fake clock, side effects, failure injection.
- [ ] **P2-03 — Claude Code adapter** (deps: P2-01; size: L)
  - Do: `cartridge/claude`: invoke `claude -p` with `--output-format stream-json`, `--model`, `--append-system-prompt`, tool policy mapped to `--allowedTools`/`--disallowedTools` (or the current equivalents), `--max-turns`, cwd = workdir, `--permission-mode` suited to unattended use. Verify every flag against `claude --help` on the machine and record the verified CLI version in `docs/cartridges.md`. Parse stream-json into SessionEvents; extract usage/cost from the final result message. `Detect` checks binary + version + a cheap auth probe.
  - Done when: unit tests parse recorded stream-json fixtures (put real captured samples in `testdata/claude/`); conformance suite (P2-09) passes locally.
- [!] **P2-04 — Kimi adapter** (deps: P2-01; size: M) (blocked: deferred by human 2026-09-02, see D-19 — do not pick this up)
  - Do: nothing until the human reopens it. When reopened: research the Kimi CLI's non-interactive mode, model flag (README example: `--model 3`), streaming output; implement the P2-01 contract; document the verified version in `docs/cartridges.md`.
  - Done when: reopened and fixture-based parsing tests pass.
- [!] **P2-05 — Animus adapter** (deps: P2-01; size: M) (blocked: deferred by human 2026-09-02, see D-17/D-19 — do not pick this up)
  - Do: nothing until the human reopens it. When reopened: research the CLI, implement like P2-04, and record a Decision.
  - Done when: reopened and adapter tests pass.
- [!] **P2-06 — Custom command adapter** (deps: P2-01; size: M) (blocked: deferred by human 2026-09-02, see D-19 — do not pick this up)
  - Do: nothing until the human reopens it. When reopened: `cartridge/custom` with a config-driven template (`command`, `args` with `{prompt}`, `{system}`, `{model}`, `{workdir}` placeholders, `prompt_via = argv|stdin|file`), plain-text stdout streamed as TextDelta, exit code → Done/Error.
  - Done when: reopened and tests with a shell script as the "LLM" pass.
- [ ] **P2-07 — Structured output contract** (deps: P2-01; size: M)
  - Do: `cartridge/structured`: prompt suffix instructing the model to end with a fenced block ` ```sc-result ... ``` ` containing JSON matching a named schema; parser tolerant of surrounding prose; on invalid/missing JSON, one automatic retry with the validation error fed back; JSON Schema files in `internal/agents/schemas/` validated with a schema library.
  - Done when: tests with valid, truncated, double-block, prose-wrapped, and totally missing outputs.
- [ ] **P2-08 — Cartridge selection policy** (deps: P2-01, P1-02; size: S)
  - Do: `Select(role) Cartridge` implementing D-12 (active cartridge, per-role override). Emits a Warning event when the chosen cartridge's Detect fails and falls back to active. Logic must not assume a single adapter (D-19).
  - Done when: table tests using two mock cartridges registered under different names.
- [ ] **P2-09 — Conformance suite** (deps: P2-02, P2-07; size: M)
  - Do: `make cartridge-conformance CARTRIDGE=<name>` (only `claude` and `mock` exist for now): runs a fixed set of prompts (echo a token, produce a structured result, write a file in workdir under read_write, refuse under read_only, respect timeout) against a real cartridge. Skipped in CI. Prints a pass/fail table.
  - Done when: mock passes it; running it against Claude is documented with expected output.
- [ ] **P2-10 — "Adding a cartridge" guide** (deps: P2-03, P2-08, P2-09; size: S)
  - Do: `docs/cartridges.md` section "Adding a new cartridge": the interface to implement, the error-kind mapping table to fill in, tool-policy mapping (P5-06), registry registration, how to run the conformance suite against it, and a checklist. State clearly that only Claude ships today and that Kimi, Animus, and custom commands are planned. Link it from the README's Cartridges section (P10-01).
  - Done when: a second agent can follow the guide to register a second mock-backed cartridge in under an hour; note their agent id here.

---

## Phase 3 — Beads Integration (Task Graph)

**Goal**: beads is the persistent task graph; `sc` reads and writes it exclusively through the `bd` CLI, using only commands and flags verified against bd 1.2.2 (Appendix A).
**Exit criteria**: integration test (tagged) initialises beads in a fixture repo without committing to it, creates a plan with `bd create --graph`, reads back the ready set, moves a bead through every D-07 status, and survives an `sc` restart.

- [ ] **P3-01 — bd detection, version gate, init** (deps: P0-08; size: S)
  - Do: `internal/beads`: `Detect()` runs `bd version --json` (fields `version`, `build`), enforces ≥ 1.2.2, returns install hints from Appendix A. `EnsureInitialised(repo)` implements D-13 exactly: the verified init command, then `.gitignore` hunk revert, then `bd ping --json` as the health check (`bd doctor --json` printed nothing in embedded mode and is not used). Re-runs are no-ops thanks to `--init-if-missing`.
  - Done when: tests via `FakeBin`; an integration test asserts `git log` and `git status` of the fixture are unchanged after init except the excluded `.beads/`.
- [ ] **P3-02 — bd client wrapper** (deps: P3-01, P0-05; size: L)
  - Do: one `Client` with a mutex-serialised `run(args…)` implementing D-21 (env, `-C`, envelope parsing with preamble skipping, JSON error → typed errors keyed on `code` and message: `ErrNotFound`, `ErrBlockedOpen` for close refusals, `ErrReadOnly`, `ErrLocked`). Typed methods, all verified: `Create(BeadInput)` (`bd create <title> -t -p --description --acceptance -l --metadata @file --parent --silent/--json`), `CreateGraph(plan)` (`bd create --graph <file> [--dry-run]`, D-22), `Show(ids…)` (`bd show … --json --include-comments`; returns an **array**), `List(filter)` (`bd list --json -n 0 --flat [--all] [-s a,b] [-l] [--parent] [--metadata-field k=v]`), `Ready(parent)` (`bd ready --json -n 0 [--parent]`), `Blocked()` (`bd blocked --json`, gives `blocked_by[]`), `Update(id, patch)` (`bd update … -s --assignee --set-metadata k=v --metadata @file --add-label --remove-label --json`), `Claim(id)` (`bd update <id> --claim --json` with `BEADS_ACTOR` set to the agent id), `Close(id, reason)` (`bd close <id> --reason … --json`; surfaces the blockers-open refusal), `Comment(id, body)` (`bd comment <id> --stdin`; note `bd comments add -f -` does **not** read stdin), `Comments(id)` (`bd comments <id> --json`), `AddDep(issue, dependsOn)` (`bd dep add <issue> <depends-on> --json`), `DepList(ids…)` (`bd dep list … --json`), `Export()` (`bd export`, JSONL with labels, dependencies, comments per issue, for whole-graph snapshots), `SetCustomStatuses`, `Statuses()` (`bd statuses --json`), `ConfigSet/Get`, `Ping()`.
  - Done when: unit tests against the `FakeBin` shim replaying captured JSON in `testdata/bd/` (capture with the real binary and commit the samples); integration test against real `bd` on the fixture repo; a test proves calls are serialised.
- [ ] **P3-03 — Slop-cannon conventions in beads** (deps: P3-02; size: M)
  - Do: implement D-06/D-07: `EnsureStatuses()` unions the D-07 custom statuses with the repo's existing `status.custom` and sets them once per run; `SetStatus(id, state)`; metadata helpers for every `sc_` key; label helpers; structured comment writer/reader (`PostMessage(id, actor, msg any)`, `Messages(id) []Message` parsing the first fenced JSON block with `type`, tolerating prose around it); run epic creation. Document everything in `docs/beads-conventions.md` so a human reading `bd show <id>` understands the history.
  - Done when: round-trip tests for every message type; a bead's history renders as a readable timeline in `sc status`; `bd ready` never lists a bead in a custom status (test).
- [ ] **P3-04 — Graph model** (deps: P3-02; size: M)
  - Do: `Graph` built from one `Export()` snapshot (or `List()` + `DepList()` for the run's beads): nodes, `blocks` edges only (ignore `parent-child`, `related`, `discovered-from` for readiness), topological layers, ready set consistent with `bd ready` semantics (all blockers `closed`), cycle detection naming the cycle (bd also rejects cycles at write time), critical path length, counts per status. Pure data structure with no I/O.
  - Done when: tests: diamond deps, cycle, 200-node random DAG performance (< 50 ms); a differential test asserts the model's ready set equals `bd ready` on the fixture.
- [ ] **P3-05 — Claim, lease, release** (deps: P3-03; size: M)
  - Do: `Claim(id, agentID)` = `bd update --claim` with `BEADS_ACTOR=<agentID>` (atomic in bd; sets assignee + `in_progress`), then `sc_lease` metadata = now and `sc_agent`; `Renew` updates `sc_lease`; `Release` = `bd update -s open --assignee ""` and clears the keys. `RecoverStale(maxAge)` re-opens beads whose `sc_lease` is older than the max and whose agent is not alive (used on resume and by the supervisor).
  - Done when: concurrency test with 10 goroutines claiming the same bead through the serialised client sees exactly one winner; stale recovery test.
- [ ] **P3-06 — Embedded Dolt handling** (deps: P3-01; size: S)
  - Do: document and enforce D-21's single-writer model: embedded mode (`metadata.json` `dolt_mode: embedded`) is the only supported mode in this phase; `dolt.auto-commit` defaults to `on` in embedded mode so every write is already a Dolt commit and no `bd dolt commit`/sync step is needed; `bd history <id>` is available for debugging. If `bd context --json` reports server mode, warn that `sc` has not been tested against it and continue. Record a discovered task for optional `--server` support if a user needs multi-writer access.
  - Done when: integration test asserts `bd history` shows one commit per `sc` write; docs section "Dolt".
- [ ] **P3-07 — Graph watcher** (deps: P3-04, P0-06; size: S)
  - Do: poll `List(--all)` every `graph_poll_interval` (default 2 s, one serialised read of ~0.2 s), diff by `updated_at`, and publish `GraphChanged` with added/changed ids; coalesce bursts. (`bd list --watch` exists for humans; the events journal documented upstream is not in 1.2.2.)
  - Done when: test with fake clock.
- [ ] **P3-08 — Gates for PR merges** (deps: P3-02; size: S)
  - Do: `CreatePRGate(bead, prNumber)` = `bd gate create --type=gh:pr --blocks <bead> --await-id <n> --reason … --json`; `CheckGates()` = `bd gate check --type=gh:pr --json` (needs `gh`; surfaces the "gh CLI not installed" error as a Warning); `GateFor(bead)` via `bd gate list --json`; escalation (`--escalate`) marks PRs closed without merge. Used by D-20.
  - Done when: tests with `bd` and `gh` shims covering resolved, still open, and closed-without-merge.
- [ ] **P3-09 — Read-only `bd` shim for agents** (deps: P3-01; size: S)
  - Do: per worktree, write `<wt-bin>/bd` (a tiny script or Go stub) that execs the real `bd` with `--readonly` prepended, and prepend `<wt-bin>` to the agent's `PATH`; verify `bd update` inside an agent session fails with `operation 'update' is not allowed in read-only mode` while `bd ready`/`bd show` work. Add a line to every role prompt: read beads if useful, never write; the cannon records your results.
  - Done when: test executes the shim; conformance suite (P2-09) includes this check.
- [ ] **P3-10 — Plan application via graph plans** (deps: P3-02, P3-03; size: M)
  - Do: implement D-22: convert the PO's validated plan into the graph-plan JSON (Appendix A schema), run `--dry-run`, then apply; map keys→IDs from the result; attach `sc_` metadata and `sc:run` labels in the plan itself so creation is one atomic call; handle the "unknown field silently dropped" warning by failing the build if any warning appears in `--dry-run` output.
  - Done when: tests: a 6-node plan with a diamond dependency lands with correct `blocked_by`; a plan with an unknown field fails loudly.

---

## Phase 4 — Git, Worktrees, PRs & Safety

**Goal**: every engineer works in an isolated worktree; finished work is delivered exactly as the safety setting dictates.
**Exit criteria**: safety-on test proves the default branch is never modified; safety-off test proves a fast-forward merge; conflict test proves the bead is returned to the engineer.

- [ ] **P4-01 — Repo basics** (deps: P0-08; size: S)
  - Do: `internal/gitx`: `Open(path)`, default branch detection (`origin/HEAD`, then `main`, then `master`), `IsClean`, `Fetch`, `HasRemote`, `HeadSHA`, `MergeBase`. All via `git` subprocess; no libgit.
  - Done when: tests on the fixture repo with and without remote.
- [ ] **P4-02 — Worktree manager** (deps: P4-01; size: M)
  - Do: `Create(bead) (Worktree, error)` with plain `git worktree add <state-dir>/runs/<run-id>/wt/<bead-id> -b sc/<bead-id>-<slug> <default>` (D-23; outside the repo, so no `.gitignore` edits; `bd worktree create` is not used because it writes to the repo `.gitignore`). bd discovers the shared `.beads/` from a linked worktree through the git common dir (verified with `bd where`), so `bd` works inside worktrees with no extra configuration. `Remove`, `Prune`, `List`, `Reattach` for resume; guard against nested worktree creation inside another worktree.
  - Done when: tests: create/remove/list, reattach after process restart, refusal on dirty target.
- [ ] **P4-03 — Auto-commit leftovers and diff stats** (deps: P4-02; size: S)
  - Do: implement D-14: after an engineer session ends, `git add -A && git commit` if dirty with a conventional message; return `DiffStats` (files, +/- lines) versus merge-base.
  - Done when: tests for dirty and clean cases; ignores files matched by the repo's `.gitignore`.
- [ ] **P4-04 — Diff provider for reviewers** (deps: P4-02; size: S)
  - Do: `Diff(worktree) (Diff, error)` unified diff against merge-base with rename detection; if larger than `max_diff_bytes` (default 200 KiB) return a file list plus per-file stats and let testers open files themselves; compute the D-09 change ratio between rounds.
  - Done when: tests including the size cap and ratio.
- [ ] **P4-05 — Safety ON delivery: push and PR** (deps: P4-03, P3-03; size: M)
  - Do: push branch; `gh pr create --title "<bead title>" --body <generated>` with body sections: bead link/id, implementation report, per-tester verdict summary, rounds, `Generated by slop-cannon`; label `slop-cannon`; store `sc_pr_url`/`sc_pr_number` metadata on the bead and create the `gh:pr` gate (P3-08, D-20); if `gh` is missing or unauthenticated: push only, print the compare URL / manual instructions, and move the bead to `needs_human` (no gate can be checked); if no remote: leave the branch and say so. Idempotent on retry (finds existing PR for the branch).
  - Done when: tests with a `gh` shim covering create, existing PR, missing gh, no remote.
- [ ] **P4-06 — Safety OFF delivery: rebase, check, merge** (deps: P4-03; size: M)
  - Do: implement D-10 off-path: `git rebase <default>` in the worktree; run `check_command` (from config; else autodetect `make test`, `npm test`, `go test ./...`, `pytest`, `cargo test` by files present); fast-forward merge into the default branch in the main repo; push if a remote exists; then `bd close` (D-20). Merges are serialised by the orchestrator (one at a time); `bd merge-slot` is not needed while `sc` is the only merger. Any failure returns a typed `DeliveryError{Stage, Output}`; rebase conflicts abort cleanly and leave the worktree in a conflict state for the engineer.
  - Done when: tests for happy path, failing check, conflict; the default branch never gets a merge commit.
- [ ] **P4-07 — PR watcher and external review posting** (deps: P4-01, P3-02; size: M)
  - Do: implement D-11: poll `gh pr list --json` every `pr_poll_interval` (default 60 s), skip PRs labelled `slop-cannon`, create one review bead per new PR (`bd create -t task --external-ref gh-<n>` with label `sc:external-pr` and `sc_pr_url` metadata; there is no custom issue type), and provide `PostReview(pr, verdicts)` using `gh pr review --approve|--request-changes --body`. Never merge.
  - Done when: tests with a `gh` shim; the review body golden test.
- [ ] **P4-08 — Cleanup and resume support** (deps: P4-02, P4-05, P4-06; size: S)
  - Do: on run finish prune worktrees of `done` beads; keep worktrees of `failed`/`blocked` beads and print their paths; on `--resume` reattach worktrees and reconcile branch state (merged? PR open?) into bead state.
  - Done when: tests: finish with mixed states; resume after simulated crash.

---

## Phase 5 — Agent Roles & Prompts

**Goal**: each role has a prompt builder, a tool policy, and a structured output schema, verified with golden tests and the mock cartridge.
**Exit criteria**: every role's prompt contains vibes and rules when configured; every structured output validates against its schema; the PO produces a valid plan for the fixture repo through the mock.

- [ ] **P5-01 — Prompt assembly** (deps: P2-07, P1-05, P0-05; size: M)
  - Do: `internal/agents/prompt`: layered builder: role base prompt (embedded `.md` templates) + **Rules** section (verbatim, framed as MUST) + **Vibes** section (verbatim, framed as preferences) + repo context (`tree` summary depth 3 capped at 400 entries, README head, detected stack, check command, default branch) + bead context + round history + output contract from P2-07. Token budget with truncation order (repo tree first, then history). Every prompt is written to `.slop-cannon/run/<id>/prompts/<agent-id>.md` for debugging.
  - Done when: golden tests per role; test that rules and vibes appear in all three roles' prompts.
- [ ] **P5-02 — Product owner / BA agent** (deps: P5-01; size: L)
  - Do: two planning modes. **Autopilot**: research the repo (read-only tools), produce `plan` output: a short product brief plus proposed beads (plan-local `key`, title, description, acceptance criteria, `deps` by key, priority 0–4 on bd's scale with 0 highest and 2 default, `type` from bd's core types `task|bug|feature|chore`, size S/M/L in `sc_size` metadata), max `max_beads_per_plan` (default 8), avoiding duplicates of open beads (list supplied in prompt). **Brief mode**: decompose `-m` text into beads with the same shape; ask no questions. Plan validator: dep indices valid, no cycles, acceptance non-empty. Creator turns a plan into beads via P3-10 (`bd create --graph`, D-22) with `sc:role:po` origin label.
  - Done when: schema tests; mock-driven test creates beads for the fixture repo; validator rejects cyclic plans.
- [ ] **P5-03 — Engineer agent** (deps: P5-01; size: M)
  - Do: modes: **implement** (bead + acceptance; must run check command before finishing; commit; output `impl_report`: summary, files changed, how to verify, risks, questions), **address feedback** (items from all testers; output `response[]` with `fixed|pushback|wontfix` and rationale; README requires pushback to be possible), **resolve conflict** (after failed rebase). Tool policy `read_write`.
  - Done when: schema + golden tests for all three modes.
- [ ] **P5-04 — Tester agents (three lenses)** (deps: P5-01, P4-04; size: M)
  - Do: lenses: `ux` (behaviour, usability, CLI/UI ergonomics, error messages), `qa` (correctness, edge cases, regressions, runs the test suite and reproduces bugs), `test` (test coverage and quality; may propose concrete test code in suggestions). Modes: **review** (diff + impl report → `verdict`), **re-review** (previous items + responses + new diff → verdict updating item statuses), **evaluate pushback** (accept or hold with reason). Tool policy `read_exec`. A mandatory item category `rules` — a tester must flag any rules-file violation as a blocker.
  - Done when: schema + golden tests per lens and mode.
- [ ] **P5-05 — PO arbitration and escalation modes** (deps: P5-02; size: S)
  - Do: **arbitrate** (item, engineer rationale, tester hold reason → `arbitration`: `uphold_tester|uphold_engineer|compromise` with instruction); **escalate** (round cap reached → `approve_with_notes|narrow_scope{new acceptance}|block{summary for human}`).
  - Done when: schema + golden tests.
- [ ] **P5-06 — Tool policy mapping** (deps: P2-01; size: S)
  - Do: map `read_only|read_exec|read_write` to Claude Code's tool controls (allowed/disallowed tools, permission mode); keep the mapping behind a per-adapter method so future adapters without tool controls can fall back to copying the worktree to a temp dir for testers (documented, not implemented now).
  - Done when: tests for the Claude mapping and the mock; conformance suite includes a read_only refusal check.
- [ ] **P5-07 — Schemas and validation** (deps: P2-07; size: S)
  - Do: JSON Schema files for `plan`, `impl_report`, `verdict`, `response`, `arbitration`, `escalation`; Go structs generated or hand-written with validation tests; schemas published in `docs/schemas/`.
  - Done when: every example in docs validates; invalid examples fail with readable messages.
- [ ] **P5-08 — Agent output parsing and retry policy** (deps: P5-07, P2-07; size: S)
  - Do: `RunAgent(ctx, spec) (T, Transcript, error)` generic helper: fire cartridge, stream `AgentOutput` events, parse structured result, one retry on schema failure, typed errors for timeout/crash/rate-limit.
  - Done when: tests with the mock for each failure kind.

---

## Phase 6 — Orchestrator & Feedback Loops

**Goal**: the core loop: plan → fire engineers → fire testers → loop until satisfied → deliver, under the ammo budget, fully resumable.
**Exit criteria**: P6-09 end-to-end test passes with the mock cartridge; kill-and-resume test passes; scheduler never exceeds ammo.

- [ ] **P6-01 — Run lifecycle** (deps: P0-06, P0-07, P1-08; size: M)
  - Do: `internal/orchestrator`: `New(RunConfig)`, `Start`, `Pause` (no new agents; running ones finish), `Resume`, `Stop` (graceful: SIGTERM agents, 10 s grace, SIGKILL, release leases, keep worktrees), `Wait`. Run directory creation; `state.json` (run metadata, active agents, last event seq) written on every transition; lockfile `.slop-cannon/run.lock` (P9-04). SIGINT once = graceful stop, twice = immediate.
  - Done when: tests with fake agents for each transition; state.json golden.
- [ ] **P6-02 — Scheduler and ammo allocation** (deps: P6-01, P3-04, P3-05, P2-08; size: L)
  - Do: implement D-05: a tick loop (on events + interval) that fills free slots in priority order: arbitrations/escalations → reviews owed on `in_review` beads (3 testers per bead, one per lens, fewer if ammo cannot afford, never zero) → engineers for ready beads up to `max_engineers_in_flight` → PO planning when the ready queue is empty (autopilot) or at start. Slot accounting is exact; a crashing agent frees its slot. Deterministic under a fake clock for tests.
  - Done when: property test: for random event sequences, live agents ≤ ammo at all times; allocation examples for ammo 3, 4, 10, 12, 13 match the D-05 table in docs.
- [ ] **P6-03 — Bead state machine** (deps: P0-05, P3-03; size: M)
  - Do: `Transition(state, event) (state, error)` per D-07 as a pure table over the bd statuses; persistence via `SetStatus`/`Claim`/`Close` (P3-03/P3-05); invalid transitions are errors and Warning events, never panics.
  - Done when: table-driven test enumerates every (state, event) pair.
- [ ] **P6-04 — Feedback loop engine** (deps: P6-02, P6-03, P5-03, P5-04, P5-05; size: L)
  - Do: per bead: round counter; dispatch review to three lenses; aggregate verdicts (all approve → `approved`); else consolidate items (dedupe by file+title similarity), send to engineer, collect responses; `fixed` items go back to the originating tester; `pushback` items go to that tester's evaluate-pushback mode; hold after `max_pushback_rounds` → PO arbitrate; apply D-09 re-review rule; round cap → PO escalate; all outcomes recorded as bead comments and events.
  - Done when: mock-driven tests: (a) all approve round 1; (b) one tester requests changes, fixed, approved round 2; (c) pushback accepted; (d) pushback held → arbitration upholds engineer → approved; (e) arbitration upholds tester → engineer fixes; (f) round cap → escalation → block; (g) diff ratio triggers full re-review.
- [ ] **P6-05 — Planning loop and stop conditions** (deps: P6-02, P5-02; size: M)
  - Do: brief mode: PO plans once at start; run finishes when every bead of this run is `done`/`blocked`/`failed`. Autopilot (D-18): PO plans at start and whenever the ready queue is empty and no engineer is running; if the PO returns an empty plan the run idles and re-plans after `idle_replan_interval`; the run ends **only** on user stop or when the supervisor reports every configured cartridge as credits-exhausted (`RunFinished{Reason: credits_exhausted}`). No bead or runtime cap exists in autopilot. PO may deprioritise its own open beads in autopilot (Q-04). Emit `RunFinished{Reason, Summary}`.
  - Done when: tests under a fake clock: brief mode finishes; autopilot keeps re-planning through 24 simulated hours with empty plans; autopilot ends on simulated all-cartridges-exhausted; autopilot ends on Stop.
- [ ] **P6-06 — Delivery handoff** (deps: P6-04, P4-05, P4-06; size: M)
  - Do: on `approved` → `delivering`: call the safety-appropriate path; safety off success → `bd close` with `sc_merge_sha`; safety on success → stay `delivering` with the `gh:pr` gate until `CheckGates` resolves it, then `bd close` (D-20); conflict → bead to engineer in `resolve conflict` mode (D-10) then re-deliver without a new review unless the conflict diff ratio exceeds the D-09 threshold; check failure → treat as a `qa` change request round.
  - Done when: tests for success, conflict, failing check with both safety values.
- [ ] **P6-07 — Agent supervisor** (deps: P6-02, P5-08; size: M)
  - Do: per-agent timeout (`agent_timeout`, default 30 min), crash retry (max 2, exponential backoff), rate-limit detection (`ErrRateLimited`) → per-cartridge pause `rate_limit_backoff` (default 60 s doubling to 10 min); credits exhaustion (`ErrCreditsExhausted`) → mark that cartridge dead for the run, re-route its roles to any live cartridge (D-12 override ignored), and when no live cartridge remains signal the planning loop to finish with `credits_exhausted` (D-18); lease renewal every minute; zombie reaping on stop; all as events.
  - Done when: tests with mock failure injection for each case, including one cartridge exhausted with a fallback and all cartridges exhausted.
- [ ] **P6-10 — Gate polling loop** (deps: P3-08, P6-06; size: S)
  - Do: every `gate_poll_interval` (default 60 s) run `CheckGates()`; resolved gates close their beads (`bd close`) and emit `Merged`; escalated gates move beads to `needs_human`. Skipped with a single Warning when `gh` is unavailable.
  - Done when: tests with shims for resolve, escalate, and missing gh.
- [ ] **P6-08 — Events, run log, `sc status`** (deps: P6-01…P6-07; size: S)
  - Do: every transition publishes an event; JSONL run log at `.slop-cannon/run/<id>/events.jsonl`; `sc status [--repo]` prints run state, per-state bead counts, live agents, last 10 events, and a bead timeline with `sc status <bead-id>`.
  - Done when: golden tests for status output from a recorded events file.
- [ ] **P6-09 — Orchestrator end-to-end test** (deps: P6-01…P6-08, P2-02, P0-09; size: M)
  - Do: fixture repo + `bd` shim (or real `bd` under the integration tag) + mock cartridge scenario: brief mode, ammo 4, two beads with a dependency, one feedback round each, safety on with a `gh` shim, then again with safety off. Assert final bead states, PR creation calls, merge commits, event sequence, and that `state.json` is consistent at the end.
  - Done when: test passes in CI under 30 s.

---

## Phase 7 — TUI & Cannon Animations

**Goal**: watching a run is informative and fun: a live task graph, agent panels, feedback history, and cannon animations on every shot.
**Exit criteria**: TUI runs against the fake event generator and a real run; golden frame tests pass; idle CPU < 5% with animations on; `--headless` renders the same information as text.

- [ ] **P7-01 — App skeleton and fake event generator** (deps: P0-06, P0-05; size: M)
  - Do: `internal/tui`: Bubble Tea program subscribing to the bus; layout: header (cannon, run id, mode, ammo used/total, safety), main pane with tabs `Graph | Agents | Feedback | PRs | Log`, footer keymap; resize handling; `?` help overlay; `q` asks to confirm stop when a run is live. Hidden command `sc tui-demo` feeds a scripted event stream so TUI work never needs a real run.
  - Done when: `sc tui-demo` runs; teatest smoke test.
- [ ] **P7-02 — Graph view** (deps: P7-01, P3-04; size: L)
  - Do: layered DAG layout (topological levels left→right, or top→bottom when taller than wide), nodes as boxes with id, truncated title, state colour, round badge, assigned agent icon; edges with box-drawing characters; cursor navigation; `enter` opens a bead detail panel (description, acceptance, history). Narrow terminals (< 80 cols) fall back to an indented list grouped by level.
  - Done when: golden frames for diamond, chain, wide fan-out, 50-node graphs at 80×24 and 200×50.
- [ ] **P7-03 — Agents view** (deps: P7-01; size: M)
  - Do: one card per live agent (role glyph, lens, bead id, cartridge/model, elapsed, spinner, last output line); selecting a card shows its streaming transcript in a scrollable pane with follow mode; finished agents fade to a history list.
  - Done when: golden frames; scroll and follow tested.
- [ ] **P7-04 — Feedback view** (deps: P7-01; size: M)
  - Do: per bead: rounds as columns or sections; each tester's verdict with items (severity coloured), engineer responses inline, pushback/arbitration markers; a "satisfied" checkmark per tester.
  - Done when: golden frames from the P6-04 scenarios' recorded events.
- [ ] **P7-05 — PRs / deliveries view** (deps: P7-01; size: S)
  - Do: safety indicator, list of delivered beads with PR URL or merge SHA and check status; external review beads with posted review state; `o` opens the URL via the OS opener.
  - Done when: golden frame.
- [ ] **P7-06 — Cannon animations** (deps: P7-01; size: L)
  - Do: cannon sprite in the header pointing at the main pane. `AgentSpawned` → recoil + projectile travelling on a spring/arc (harmonica) to the target bead node (or to the Agents tab indicator when the graph is not visible); `VerdictReceived{approve}` → small burst at the node; `VerdictReceived{changes}` → ricochet spark back toward the cannon; `BeadStateChanged{done}` → confetti burst; `AgentFailed` → misfire smoke puff. Frame ticker 30 fps only while an animation is active; queue overlapping animations; `--no-anim`, `SC_NO_ANIM=1`, and `NO_COLOR` respected; tests use a deterministic clock so frames are reproducible.
  - Done when: golden frame sequences for each animation; a benchmark shows idle CPU < 5% with an animation queue empty.
- [ ] **P7-07 — Theme and styling** (deps: P7-01; size: S)
  - Do: single Lip Gloss theme with a colour-blind-safe state palette (todo grey, implementing blue, in_review yellow, changes orange, approved green, done bright green, escalated magenta, blocked/failed red), 256-colour and 16-colour fallbacks, `NO_COLOR` support.
  - Done when: a style test renders every state in each colour profile.
- [ ] **P7-08 — Headless renderer** (deps: P0-06; size: S)
  - Do: implement D-15: `--headless` prints one human-readable line per event (`12:01:03 ▶ fired engineer(claude/fable) at bd-1a2b "Add retry"`), `--json` prints raw JSONL; both flush immediately; exit code reflects `RunFinished.Reason`.
  - Done when: golden output for a recorded run; exit code tests.
- [ ] **P7-09 — TUI tests and golden files** (deps: P7-02…P7-07; size: M)
  - Do: `teatest`-based tests with `testdata/tui/*.golden`, `make update-golden`, deterministic width/height/clock; a CI check that goldens are current.
  - Done when: CI green with goldens; docs explain how to update them.
- [ ] **P7-10 — Interactive controls** (deps: P7-01, P6-01; size: M)
  - Do: keys: `p` pause/resume, `s` stop (confirm), `r` re-fire a `failed`/`blocked` bead, `x` mark a bead done manually (confirm), `S` toggle safety for future deliveries (confirm), `a` change ammo for future ticks. All go through the orchestrator API, never mutate state directly.
  - Done when: tests with a fake orchestrator asserting each call.

---

## Phase 8 — Modes & End-to-End Wiring

**Goal**: `sc ./repo` and `sc ./repo -m "..."` work end to end with a real cartridge, in TUI and headless.
**Exit criteria**: a recorded manual run against the fixture repo with Claude completes at least one bead through review to PR (safety on) and to merge (safety off); the demo script runs deterministically with the mock.

- [ ] **P8-01 — Wire run command to orchestrator + TUI** (deps: P6-09, P7-01, P7-08; size: M)
  - Do: replace the P1-08 stub: build run, start orchestrator, attach TUI or headless renderer, handle exit codes, print a final summary (beads done/blocked/failed, PRs, merges, duration, token/cost totals if any).
  - Done when: `sc ./fixture -m "fix the two bugs" --headless --cartridge mock` (mock is selectable via a hidden flag for tests) completes.
- [ ] **P8-02 — Brief mode manual verification** (deps: P8-01, P2-03; size: M)
  - Do: `scripts/manual/brief-run.sh`: prepares a fixture copy, runs with the active real cartridge, safety on, ammo 4; records the events file into `testdata/recorded/brief-run.jsonl` (redacted). Fix whatever breaks; file discovered tasks for anything larger than a fix.
  - Done when: the recorded run shows plan → implement → 3 verdicts → approved → PR opened.
- [ ] **P8-03 — Autopilot mode manual verification** (deps: P8-02; size: M)
  - Do: same as P8-02 without `-m`; confirm the PO invents sensible beads for the fixture (it should find the planted bugs and TODO); confirm it keeps re-planning when idle and only ends on `s`/SIGINT; with the mock, confirm it ends on credits exhaustion.
  - Done when: recorded run committed; PO plan quality reviewed and noted on this task.
- [ ] **P8-04 — External PR review mode** (deps: P8-01, P4-07; size: M)
  - Do: `--review-prs` default on when `gh` + remote exist (D-11); wire watcher → review beads → tester dispatch → posted review; TUI PRs view shows them.
  - Done when: test with a `gh` shim; manual verification against a throwaway GitHub repo documented.
- [ ] **P8-05 — `sc status`, `sc logs`, `sc resume`** (deps: P6-08, P8-01; size: S)
  - Do: `sc logs <agent-id|bead-id>` tails the relevant log; `sc resume [--repo]` is sugar for `sc <repo> --resume`; `sc status` works while another `sc` process holds the run lock (read-only).
  - Done when: tests.
- [ ] **P8-06 — First-run experience** (deps: P8-01, P1-07; size: S)
  - Do: on any startup failure print a numbered fix list (install `bd`, add cartridge, authenticate, etc.) and suggest `sc doctor`; on first successful run in a repo print where logs, worktrees, and config live.
  - Done when: golden outputs for the three most common failures.
- [ ] **P8-07 — Deterministic demo** (deps: P8-01, P7-06; size: M)
  - Do: `make demo` runs the TUI against the fixture with a mock scenario that exercises every animation; `demo.tape` for VHS produces `docs/demo.gif`.
  - Done when: GIF committed and embedded in README.

---

## Phase 9 — Hardening & Guardrails

**Goal**: the cannon cannot damage the target repo, cannot run away, and recovers from crashes.
**Exit criteria**: all invariant tests pass; troubleshooting doc covers every error message that exists in the code (grep-verified).

- [ ] **P9-01 — Limits and budgets** (deps: P6-05, P6-07; size: M)
  - Do: config keys `limits.max_review_rounds`, `max_pushback_rounds`, `agent_timeout`, and opt-in `max_tokens`, `max_cost_usd` (unset by default, enforced only when cartridges report usage). Crossing a cost/token cap stops new work and finishes in-flight reviews; in brief mode the run then finishes with the reason in the summary; in autopilot it **pauses** with a banner and waits for the user (D-18). No bead-count or runtime cap exists.
  - Done when: tests for each limit; docs/config.md updated.
- [ ] **P9-02 — Safety invariant tests** (deps: P6-06; size: M)
  - Do: with safety on, a `git` shim asserts no `checkout`/`merge`/`push` targets the default branch, ever, across the P6-09 scenario and randomised event sequences; with safety off, assert only fast-forward merges happen.
  - Done when: invariant tests in CI.
- [ ] **P9-03 — Crash recovery** (deps: P6-01, P4-08, P3-05; size: M)
  - Do: test that `kill -9` mid-round then `--resume` reconciles: stale leases recovered, worktrees reattached, agents that were mid-flight re-fired, delivered beads not re-delivered (check branch/PR state).
  - Done when: automated test with a real child process.
- [ ] **P9-04 — Single run per repo** (deps: P6-01; size: S)
  - Do: lockfile with PID and start time; a second `sc <repo>` refuses with the live run's id and suggests `sc status`; stale lock (dead PID) is removed with a notice.
  - Done when: tests.
- [ ] **P9-05 — Secrets hygiene** (deps: P5-01, P0-07; size: S)
  - Do: prompts never include `.env*`, `*.pem`, `id_*`, or files matched by a configurable `exclude` list; transcripts and events pass through the redaction hook; `sc doctor` warns if the repo's `.gitignore` lacks `.env`.
  - Done when: tests plant fake secrets and assert absence in prompts/logs/events.
- [ ] **P9-06 — Rules compliance gate** (deps: P5-04, P6-04; size: S)
  - Do: any verdict item in category `rules` is a blocker that cannot be overridden by arbitration in the engineer's favour; escalation cannot approve-with-notes while a rules blocker is open.
  - Done when: state machine and loop tests.
- [ ] **P9-07 — Performance** (deps: P7-02, P6-02; size: S)
  - Do: benchmarks: 200-bead graph layout < 100 ms, scheduler tick < 5 ms with 50 agents, event bus 10k events/s; fix hot spots found.
  - Done when: benchmarks committed with results in `docs/performance.md`.
- [ ] **P9-08 — Error message audit and troubleshooting doc** (deps: P8-06; size: M)
  - Do: grep every `errors.New`/`fmt.Errorf` user-facing string; ensure each says what happened, why, and what to do; `docs/troubleshooting.md` table keyed by message.
  - Done when: a test asserts every documented message still exists in code.

---

## Phase 10 — Docs, Packaging & Release

**Goal**: someone else can install `sc`, read what it does, and fire it in five minutes.
**Exit criteria**: `curl | sh` or `go install` works on a fresh machine; README quickstart verified by a second agent following it literally; v0.1.0 tagged.

- [ ] **P10-01 — README rewrite** (deps: P8-07; size: M)
  - Do: keep the original vision paragraphs and the "Ammo breakdown"; add Install, Quickstart, Commands (link `docs/cli.md`), Config, How the feedback loop works (diagram), Safety, Cartridges (Claude only today, more coming, link to the P2-10 guide), FAQ, Development, demo GIF.
  - Done when: a second agent follows Quickstart on a clean checkout and succeeds; note their agent id here.
- [ ] **P10-02 — Architecture doc** (deps: P6-09; size: M)
  - Do: `docs/architecture.md`: component diagram (CLI → orchestrator → cartridges/beads/gitx → TUI via bus), sequence diagram of one bead's life, state diagram of D-07, the D-05 allocation table, resume model.
  - Done when: reviewed for accuracy against code by a second agent.
- [ ] **P10-03 — Release pipeline** (deps: P0-04; size: M)
  - Do: goreleaser config for linux/darwin (amd64, arm64), checksums, `install.sh`, optional Homebrew tap formula; GitHub Actions release on tag.
  - Done when: a dry-run release produces artefacts; `sc version` shows the tag.
- [ ] **P10-04 — Versioning and changelog** (deps: P10-03; size: S)
  - Do: `CHANGELOG.md` (Keep a Changelog), semver policy in `docs/releasing.md`.
  - Done when: v0.1.0 entry written from git history.
- [ ] **P10-05 — Dogfood** (deps: P8-02, P10-01; size: L)
  - Do: run `sc . -m "<a real small task from Discovered Tasks>"` on this repository with safety on, ammo 4; review the PR it opens; record what went wrong as discovered tasks; merge if good.
  - Done when: the PR link and outcome are noted on this line.

---

## Discovered Tasks

Add tasks found during implementation here using the next free ID in the relevant phase. Keep the same format. Move them into the phase section only if you are also the one implementing them; otherwise leave them here so others can find them.

- [ ] **P0-10 — Remove `cmd/sc/deps.go`** (deps: P1-01, P1-02, P7-01, P7-06; size: S)
  - Do: once real packages import cobra, go-toml, bubbletea, lipgloss, bubbles, harmonica, testify and teatest, delete the `deps`-tagged pin file (or the imports that are no longer needed) and run `go mod tidy`.
  - Done when: `go mod tidy` leaves go.mod unchanged without the file.
- [ ] **P1-10 — Keep `cli.Version/Commit/Date` in sync with the Makefile ldflags** (deps: P1-01; size: S)
  - Do: the Makefile injects `-X slop-cannon/internal/cli.{Version,Commit,Date}`; P1-01 must keep those variable names or update `LDFLAGS`, and a test should assert `sc version` prints the injected values (build with `-ldflags` in the test or check the variables directly).
  - Done when: renaming any of the three fails a test.
- [ ] **P1-11 — Wire `--verbose` and the logging handlers into the CLI root** (deps: P1-01, P0-07; size: S)
  - Do: root `PersistentPreRun` sets `slog.SetDefault(logging.NewTerminalLogger(os.Stderr, logging.Options{Verbose: verbose}))`; the run command (P6-01/P8-01) opens the per-run log with `logging.OpenRunLog(<state-dir>/runs/<run-id>, runID, opts)` and uses `NewMultiHandler` in headless mode (D-15).
  - Done when: a test shows `--verbose` emits Debug lines and the run log lands under the D-23 run dir.
- [ ] **P3-11 — Verify bd 1.2.2 graph-plan node fields and apply D-24** (deps: P3-02; size: S)
  - Do: with the real binary, find the accepted node field name for acceptance criteria (if any) and confirm that top-level `edges[]` creates `blocks` dependencies with `edge_count` > 0 and correct `blocked_by`; update Appendix A's graph-plan snippet and the P3-10 converter accordingly; fail loudly on any dry-run `warning:` line. Evidence: `internal/testutil/testdata/bd/create-graph-dry-run.{json,stderr}`.
  - Done when: a captured sample with edges shows `edge_count` equal to the plan's dependency count and P3-10's tests use that shape.
- [ ] **P3-12 — Map bd 1.2.2 error shapes to typed errors** (deps: P3-02; size: S)
  - Do: bd 1.2.2 does **not** emit the `{"error","code","hint"}` envelope D-21/Appendix A describe: `bd show nope-1 --json` prints `{"data":{"error":"no issues found matching the provided IDs"},"schema_version":1}` with exit 1, while `bd close` blocked refusals and `--readonly` refusals print only a stderr line with empty stdout. P3-02's typed errors (`ErrNotFound`, `ErrBlockedOpen`, `ErrReadOnly`, `ErrLocked`) must key on exit status plus `data.error`/stderr text; keep the `code` path for future bd versions. Samples: `error-not-found.*`, `error-close-blocked.*`, `error-readonly.*`. Update D-21/Appendix A wording.
  - Done when: table tests over the captured samples map each to the right typed error.
- [ ] **P3-13 — Handle bd 1.2.2 output quirks in the client** (deps: P3-02; size: S)
  - Do: (a) `bd ready` lists the run epic itself alongside its children, so `Ready()` and the scheduler filter `issue_type == "epic"` (or use `--parent` and still filter); (b) `bd gate check --json` prints `Checked 1 gates: …` before the JSON envelope, and `bd init --json` / `bd info --json` print plain text, so the preamble skipper must tolerate a leading non-JSON line and the init/info callers must not expect JSON; (c) `--set-metadata sc_round=1` is stored as JSON number `1`, so metadata helpers accept numbers and strings. Samples: `ready.json`, `gate-check.txt`, `init.txt`, `info.txt`, `update.json`.
  - Done when: tests for each quirk against the captured samples.
- [ ] **P7-11 — Headless renderer keys off the event fields that now exist** (deps: P7-08; size: S)
  - Do: use `events.RunFinished.Reason` (`events.FinishReason` constants) for the exit code and `events.AgentSpawned.{BeadTitle,Task,Attempt}` for the `fired engineer(...) at <bead> "<title>"` line instead of re-deriving from a bead index; fold this into P7-08 if it is not started yet.
  - Done when: P7-08's golden output test uses those fields.
- [ ] **P9-09 — Redact at event construction, not in the bus** (deps: P9-05, P0-06; size: S)
  - Do: `events.Bus` deliberately never transforms events, so the orchestrator/proc layer must pass `AgentOutput.Line`, `AgentFailed.Err`, `Warning.Err`, `Error.Err` (and any prompt excerpts) through `logging.Redact` before `Publish`; add a helper (`events.Redacted(ev, redactor)` or construction-site calls) and a test that plants a token in agent output and asserts it never reaches a subscriber or the JSONL file.
  - Done when: the P9-05 secrets test also covers the events file.

---

## Appendix A — beads (`bd`) facts verified on 2026-09-02

Verified live against `bd` 1.2.2 (latest release, 2026-08-15; repo moved to `github.com/gastownhall/beads`, Go module path still `github.com/steveyegge/beads`) in a scratch repo, plus the upstream docs. Agents must not re-derive these; if a newer bd changes them, update this appendix and the affected tasks.

**Install and backend**
- `bd` embeds the Dolt engine; embedded mode is the default and needs **no** `dolt` binary (verified with `dolt` removed from `PATH`). The standalone `dolt` CLI is only for server mode. `CGO_ENABLED=0 go install` builds are server-mode only; `brew install beads`, the upstream `scripts/install.sh`, or a cgo build give embedded mode.
- Embedded mode is single-writer with a file lock. 30 concurrent `bd` writers all succeeded but queued (6.4 s total, ~0.2 s each); 20 readers concurrent with 10 writers all succeeded. `sc` serialises calls anyway (D-21).
- `dolt.auto-commit` defaults to `on` in embedded mode: every write is a Dolt commit; `bd history <id>` shows them. No manual commit/sync step is needed. `bd doctor --json` printed nothing in embedded mode; use `bd ping --json`, `bd info --json`, `bd where --json`, `bd context --json`.
- Typical latency: 0.15–0.2 s per command.

**Dolt itself (verified from dolthub/dolt, dolthub/driver, and dolthub.com docs)**
- Dolt is a MySQL-compatible SQL database with Git-style commits, branches, and merges; beads stores issues in tables under Dolt's own ref namespace, so nothing about issue state touches the code branches or branch protection.
- Embedded use goes through the `database/sql` driver `github.com/dolthub/driver` (beads pins `driver/v2` 2.2.0 and the Dolt engine at the v2.2.0 commit); DSN `file:///path?commitname=…&commitemail=…&database=…`; no server process. The engine holds an exclusive directory lock (`dolthub/fslock`, a `flock` on the noms `LOCK` file); the driver's `Config.BackOff` retries engine open on lock contention, and beads sets it to retry until context cancellation with a 5 s max interval, which is why concurrent `bd` processes queue instead of erroring.
- Transactions: Read Committed isolation; "if two writers attempt to write the same data, one writer is allowed to commit their transaction and the other is rolled back"; autocommit wraps each write in `BEGIN`/`COMMIT`; `@@dolt_transaction_commit` can turn every SQL commit into a Dolt commit (this is what `dolt.auto-commit: on` uses). Dolt serialises writes and is rated at roughly a few hundred writes per second, far above anything `sc` produces.
- Server mode (`dolt sql-server`, port 3307 in beads) is the multi-writer path and needs the standalone `dolt` CLI. Latest Dolt is 2.3.1 (2026-08-19); beads pins the CLI at **2.2.0** because 2.3.0/2.3.1 intermittently break `DOLT_RESET('--hard')` on a few percent of fresh databases. This affects server mode only; embedded mode links its own engine. The `dolt` 2.3.1 on this machine is therefore irrelevant to `sc` unless server mode is added later, and `sc doctor` should say so if it ever checks it.

**Init side effects (why D-13 uses the flags it does)**
- Default `bd init` writes `CLAUDE.md`, `AGENTS.md`, `.claude/settings.json`, `.codex/`, `.agents/skills/beads/`, installs `pre-commit`, `post-merge`, `pre-push`, `post-checkout`, `prepare-commit-msg` hooks, appends to `.gitignore`, and **commits** `.beads/` to the repo.
- `--skip-agents --skip-hooks` removes the files and hooks but still commits. `--setup-exclude` adds `.beads/` (and `**/RECOVERY*.md`, `**/SESSION*.md`) to `.git/info/exclude` and does not commit, but still appends the `.gitignore` block. `--stealth` edits global git config. `--init-if-missing` makes re-runs exit 0. `bd init` prints a one-time anonymous-metrics notice; `bd metrics off` disables collection.

**JSON contract**
- `BD_JSON_ENVELOPE=1` wraps every `--json` output as `{"schema_version":1,"data":…}` (plus `pagination` on truncated `bd ready`); it becomes the default in bd 2.0. Errors: `{"error","code","hint"}`, exit 1.
- Legacy shapes without the envelope: `create` returns an object with `schema_version`; `show`, `update`, `close`, `list`, `ready` return arrays (`show` even for one id).
- Issue fields seen: `id, title, description, acceptance_criteria, status, priority (int), issue_type, owner, assignee, created_at, created_by, updated_at, started_at, closed_at, close_reason, metadata (object), labels ([]), dependency_count, dependent_count, comment_count`; `show --include-comments` adds `comments[] {id, issue_id, author, text, created_at}`; `show --include-dependents`/`dep list` add `dependencies[]` with `dependency_type`; `blocked` adds `blocked_by[]`.
- `list` defaults: limit 50, tree format, closed excluded → always `-n 0 --flat`, `--all` for closed. `ready` default limit 100 → `-n 0`. `ready` returns only status `open` (category `active`).

**Statuses, types, priorities**
- Built-in statuses: `open` (active), `in_progress`, `blocked`, `hooked` (wip), `deferred`, `pinned` (frozen), `closed` (done). Custom: `bd config set status.custom "name:category,…"` stored in the database; categories `active|wip|done|frozen` control `bd ready` and default `bd list` visibility. `bd statuses --json` lists both.
- Core types: `task, bug, feature, chore, epic, decision, spike, story, milestone`; `gate` and `event` are created by `bd gate create`/`bd set-state`. Priority `0`–`4`, `0` highest, default `2`.
- IDs are hashes with the repo prefix (`sc-68s`); `--parent` children get hierarchical ids (`sc-dml.1`), graph-plan children get flat ids.

**Dependencies**
- `bd dep add <issue> <depends-on>` (aliases `--blocked-by`, `--depends-on`); types `blocks` (default, affects readiness), `parent-child`, `waits-for`, `related`, `tracks`, `discovered-from`, … Cycles are rejected at write time; `bd dep cycles` audits.
- `bd create --deps <id>` = new depends on id. `bd create --deps blocks:<id>` = **new blocks id** (reverse). Never use the typed form.
- `bd close` refuses when blockers are open (`--force` overrides). `bd blocked --json` explains why.

**Graph plan (`bd create --graph plan.json`)**
```json
{"commit_message":"optional",
 "nodes":[{"key":"a","title":"…","type":"task","priority":1,"description":"…","acceptance_criteria":"…",
           "labels":["sc:run:r1"],"metadata":{"sc_run":"r1"},"parent_key":"epic","deps":[{"target":"b","type":"blocks"}]}],
 "edges":[{"from_key":"x","to_key":"y","type":"blocks"}]}
```
Unknown fields are silently dropped with a warning on stderr; `--dry-run` validates. `deps[].target` resolves plan keys first, then literal ids. Result maps keys to ids. **Correction (P0-09 capture, D-24)**: against bd 1.2.2 the node-level `acceptance_criteria` and `deps` fields above were reported as unknown and dropped (`edge_count: 0`); use top-level `edges[]` for dependencies and see P3-11 for acceptance criteria. Samples: `internal/testutil/testdata/bd/create-graph-dry-run.{json,stderr}`.

**Claims, comments, labels, metadata**
- `bd update <id> --claim` is atomic: assignee = actor, status `in_progress`, idempotent for the same actor; `bd ready --claim --json` claims the first ready bead. Actor comes from `--actor` / `BEADS_ACTOR` / git user.name. `bd update -s open --assignee ""` releases.
- `bd comment <id> --stdin` (or positional text) writes a comment with author = actor; `bd comments add <id> -f <file>` works, but `-f -` does **not** read stdin. `bd comments <id> --json` lists.
- Labels: `-l a,b` on create, `--add-label/--remove-label/--set-labels` on update, `bd label add/remove/list`. `<dimension>:<value>` is the upstream state-cache convention; `bd set-state` uses it but also creates an `event` child bead per change (visible only with `--include-infra`), so `sc` uses statuses instead.
- Metadata: `--metadata '<json>'` or `@file`, `--set-metadata k=v`, filter with `bd list --metadata-field k=v`; prefixes `bd:` and `_` are reserved.

**Multi-agent features worth knowing**
- `--readonly` global flag blocks writes (`operation 'update' is not allowed in read-only mode`); `--sandbox` disables Dolt auto-push; `-C <dir>` like `git -C`.
- Gates: `bd gate create --type=gh:pr|gh:run|timer|human --blocks <id> --await-id <n>`; `bd gate check [--type=gh:pr] [--escalate]` uses `gh pr view`/`gh run view`; `bd gate list --json`; `bd gate resolve <gate>`.
- `bd merge-slot create|acquire|release` serialises merge-conflict work across agents. `bd batch` runs several simple writes in one transaction (`close`, `update status=…`, `create`, `dep add`). `bd swarm validate|status <epic>` reports parallel waves. `bd epic status --json`. `bd graph --all` / `--dot` / `--html` renders the DAG (useful for debugging the TUI layout).
- Worktrees: plain `git worktree add` shares the main `.beads/` via the git common dir; `bd worktree create` also edits `.gitignore`. `BEADS_DIR` overrides discovery.
- Env: `BD_NON_INTERACTIVE`, `BD_JSON_ENVELOPE`, `BEADS_ACTOR`, `BEADS_DIR`, `BD_DOLT_AUTO_COMMIT`, `BEADS_DOLT_SERVER_MODE`, `BEADS_HOOK_TIMEOUT`.

---

## Change Log of This File

- 2026-09-02 — Initial breakdown generated from README.md (46 lines). Decisions D-01…D-16 and open questions Q-01…Q-05 recorded.
- 2026-09-02 — Verified Dolt directly from dolthub/dolt, dolthub/driver, and dolthub.com transaction docs; added the Dolt subsection to Appendix A and expanded D-21's locking rationale.
- 2026-09-02 — Verified beads/Dolt usage against bd 1.2.2 live and upstream docs. Revised D-06, D-07, D-13; added D-20…D-23; rewrote Phase 3 (P3-01…P3-07, new P3-08…P3-10); updated P0-09, P1-07, P4-02, P4-05, P4-06, P4-07, P5-02, P6-03, P6-06; added P6-10; added Appendix A.
- 2026-09-02 — Human narrowed adapters to Claude only, with more to be added in future. Added D-19 and P2-10 (adding-a-cartridge guide); P2-04 (Kimi) and P2-06 (custom command) blocked alongside P2-05; module layout, glossary, D-12, P1-02, P1-03, P2-08, P2-09, P5-06 and the Phase 2 exit criteria updated.
- 2026-09-02 — Human confirmed all assumptions. Added D-17 (animus deferred; P2-05 blocked) and D-18 (autopilot never stops except user stop or all cartridges out of credits); removed bead/runtime caps; added `ErrCreditsExhausted` handling to P2-01, P2-02, P6-05, P6-07, P8-03, P9-01.
- 2026-09-02 — Phase 0 round 1 integrated (wf-p0, commit `be10e4e`): P0-01…P0-09 done; D-01 confirmed with module path `slop-cannon`; added D-24 (graph-plan edges) after the bd 1.2.2 capture in P0-09 contradicted the D-22/Appendix A node schema; discovered tasks P0-10, P1-10, P1-11, P3-11, P3-12, P3-13, P7-11, P9-09 recorded.
