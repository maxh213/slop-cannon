# Changelog

All notable changes to `sc` (Slop Cannon) are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
with the scope written down in [docs/releasing.md](docs/releasing.md): the
command line, the configuration file, the machine-readable output and the exit
codes are the promised surface; everything under `internal/` is not.

Add what you change to **Unreleased** as you land it — one line per user-visible
change, under the Keep a Changelog heading it belongs to — and the release
checklist in [docs/releasing.md](docs/releasing.md) turns that section into the
next version's entry.

## [Unreleased]

### Added

- The `dandelion` cartridge knows the `claude-animus` account: Claude Code with
  `CLAUDE_CONFIG_DIR` = `DANDELION_CLAUDE_ANIMUS_CONFIG_DIR`, else
  `~/.claude-animus`, launched and policed exactly like `claude-work`.
- `po_planners` (default 1; Animus intends 2) coordinates independent PO
  producers with unique IDs behind one completion barrier. A fresh snapshot,
  at most one read-only semantic judge with no retry loop, and deterministic
  reconciliation produce one graph under the run epic. The combined proposed
  and accepted cap is `N × max_beads_per_plan` (16 at N=2 with the default 8),
  with no trimming. Weighted demands reserve whole rounds within `ammo`, one
  signals reading is shared per round, and the signals threshold uses the
  combined cap. Canonical candidate/external `to_id` remapping preserves
  existing beads; multi-PO planning does not deprioritise them. Default-one
  planning keeps its exact legacy demand, producer, quota and no-judge path.

- `auto_merge = true` (or `--auto-merge`; safety on only, off by default)
  lets a run squash-merge the pull requests it opened itself once they are
  green, instead of waiting for a human: `gh pr merge --squash
  --match-head-commit`, never `--admin` or `--auto`. Every clause of the
  ownership check must hold (the bead's branch, the default branch as base,
  the `slop-cannon` label, a marker naming this run and bead, created after
  the run started, opened by the `gh` login), and the head must be the commit
  the testers approved or a branch update the run itself asked GitHub for;
  any other commit on the branch sends the bead to `needs_human`. A head
  that does not contain the default branch's tip is updated and re-checked
  first, a failed check or a conflict goes back to the engineer, and
  `auto_merge_checks` (required) names the checks that must have succeeded.
  `auto_merge_spacing` (default 10m) spaces the merges so each deploy can
  finish.
- `conventional_pr_titles` titles the run's pull requests
  `<type>: <title>` from the bead's issue type, for repositories whose CI
  enforces Conventional Commit titles.
- Direct adapters for Agy, Codex, Kimi, Grok Build, Junie, Hermes Agent and
  Kilo join Claude Code, Cursor Agent and dandelion. Large prompts use stdin
  or private prompt files with cleanup on every exit path, restricted policies
  use only native tool enforcement, and account-specific homes are isolated
  per firing. `codex-animus` is a routed Codex account with its own
  `CODEX_HOME`, not a separate direct cartridge. Both Codex and Codex Animus
  have real `read_exec` and `read_write` drivers; personal Codex being logged
  out and the Codex Animus app-server probe error are current account
  availability issues, not missing adapters. `codex-animus` is not in
  Dandelion's high chain.
- The `dandelion` cartridge can route all eight additional provider IDs
  (`agy`, `kimi`, `grok`, `codex`, `codex-animus`, `junie`, `hermes` and
  `kilo`) through seven direct CLI families, with Codex Animus delegated
  through Codex. Kilo uses `DriverKilo` for `read_exec` and `read_write` as a
  standard-route balance fallback only; it has no high route. `read_only`
  remains Claude only; `read_exec` supports Claude, Codex/Codex Animus, Kimi,
  Grok, Hermes and Kilo; `read_write` covers every routable installed
  provider. Agy and Grok `read_only` stay disabled after failed live
  enforcement canaries.
- The Kilo 7.5.6 adapter sends the complete combined prompt on stdin, sets
  `KILO_NO_DAEMON=1`, disables project config and automatic updates, and
  generates an in-memory agent config with native `read_exec`/`read_write`
  permissions. At the CLI boundary it normalizes the pinned Dandelion model
  to `kilo/deepseek/deepseek-v4.1-flash-1017` without changing direct custom
  IDs, `RunSpec.Model`, or the unqualified route log. Dandelion's
  `SC_KILO_MIN_BALANCE_USD=10` control is removed from the Kilo child
  environment. Cached Kilo routes are refreshed, and a fresh `profile --json`
  check precedes every delegated launch; profile failure or a balance at or
  below the floor excludes Kilo and reroutes without launching it. The
  `balance - floor` in-session allowance is best effort, not a hard
  account-wide cap: a final request can overshoot before cost is reported,
  concurrent sessions can spend the same balance, and other clients or later
  billed steps can take the account below $10. Real direct/delegated checks
  covered structured output, large stdin, native policy behavior,
  timeout/kill without a daemon, below-allowance cost and fail-closed reroute.
- Per-role Dandelion route modes: `roles.<role>.model = "standard"` or
  `"high"` selects the per-firing route mode, with case and whitespace
  tolerated. Empty, other and legacy values fall back to the configured mode,
  while the real routed model still replaces `spec.Model` for the delegated
  CLI. Role models are effective config, not `ConfigSnapshot` fields: current
  repo role-model changes apply on the next ordinary resume, while the active
  cartridge and its default model/mode stay frozen. The Animus setup is now
  applied with repo-sourced `standard` for engineers and testers and `high`
  for the PO; PO `high` is verified from effective config only because no PO
  fired during the bounded 180-second wait.
- `signals_command` (with `signals_interval`, default 30m, and
  `limits.signals_timeout`, default 5m) gives the autopilot product owner what
  production is saying: sc runs the command, and its output reaches the
  planning prompt fenced as untrusted evidence. A changed reading can plan
  while engineers are busy, up to one plan's worth of beads still to ship.
  A plan that copies an email address, phone number or IP address from the
  reading, or 8 or more of its words in a row, into a bead is rejected.
  `sc status` and the TUI header show the last reading and when the next is
  due.
- `sc watch [PATH]` opens the run TUI on a run that is already firing, from
  any terminal: one started with `--headless`, from a script, or in a
  terminal that has since closed. It shows everything the run has done so far
  and then follows it live. It only reads the run's `events.jsonl`: `q` leaves
  while the run keeps firing, and the run controls are not offered.
- `sc dash [PATH]` serves a read-only browser dashboard for the latest run or
  `--run ID`, with `--host`, `--port`, `--base-path` and repeatable
  `--allowed-host` flags. One cached event-log fold follows live, historical
  and resumed runs and keeps finished and crashed runs available; observer
  `/health` is separate from run liveness and quiet state. The bounded,
  redacted snapshot API has no run controls and serves no prompts, diffs or
  raw transcripts; notices carry a fixed category instead of raw prose.
  Loopback is the default, exact Host validation blocks DNS rebinding, and
  sharing beyond the machine belongs behind an authenticating reverse proxy.
- `sc dash` reports host CPU, memory, swap and root-disk usage on Linux every
  two seconds. CPU percentages use matching consecutive samples with bounded
  per-core data, zero-swap rows are omitted, and failed or unsupported
  collection preserves last-good data without changing observer health.
- `sc dash` can label a Dandelion agent with the actual account, model and
  effective effort selected for its exact `(incarnation/run, agent, supervisor
  attempt)`, reconstructed read-only from bounded `sc.log` backfill and
  following. Direct run-started records bind generations and preserve prior
  history. The latest structured route is selected only within one exact
  attempt; duplicate attempts within a generation are `unknown`, and old
  last-seen cards keep their own model. Explicit `pending`, `no_route` and
  `unknown` states permit no global/latest-route guessing. Attempt-fenced
  snapshot metadata remains bounded and cached (HTTP never reads logs), while
  the requested `high`/`standard` model configuration and adapter behavior stay
  distinct. Route selection does not prove process start or success, and
  non-Dandelion agent rendering is unchanged.
- `sc dash --dandelion` opts into a Dandelion quota panel, with
  `--dandelion-refresh` (Go duration, default 10m, minimum 1m) setting the
  collection interval and Dandelion's max-age. Sequential probes have a fixed
  five-minute timeout and bounded output. Dandelion may reuse a fresh cache
  and otherwise performs real external probes; sc's last-good normalized data
  is memory-only. Attempt and success freshness are separate, failures retain
  last-good data with one of four safe collector error codes, HTTP requests
  never trigger probes, and observer health is unaffected. The redacted,
  bounded snapshot omits raw responses and raw collector subprocess
  stderr/errors; unavailable/error provider reasons remain bounded,
  explicitly allowlisted Dandelion diagnostics. The panel shows both routes,
  probe state, providers and every quota window, with accessible
  tone-threshold gauges and a default-on `usage only` toggle persisted in
  `localStorage` (zero-percent windows count as usage). Missing, malformed
  and zero timestamps are blanked with their DOM metadata rather than shown
  as year-one dates.
- The product owner can give a bead a longer agent timeout in its plan
  (`timeout`, up to `limits.max_agent_timeout`, default 24h) when one
  session on it will genuinely need longer than the default.
- `roles.<role>.model` fires one role with its own model, e.g. the product
  owner on Opus while engineers and testers stay on the cartridge's model.
- `research` (default on) lets the planning product owner search and read
  the web; in autopilot it researches comparable products (their features,
  the quality-of-life improvements users expect, what people enjoy) before
  pitching, and never pitches manipulative engagement mechanics.
- `nice` (default 10) lowers a run's scheduling priority before it fires,
  so every agent, check command and test browser it starts runs niced and a
  busy run no longer starves the machine. `0` turns it off.
- `sc resume --run ID` continues a named run instead of the most recent one.

### Changed

- A rate limit only pauses: it no longer uses up a firing's crash retries
  or counts towards the three failed firings that park a bead, so a bead
  waits however long a cartridge (or every `dandelion` account) is limited
  instead of being parked as failed.
- The default `limits.agent_timeout` is 6 hours instead of 30 minutes, so an
  engineer on a real feature is no longer killed half-way through.
- A run resumed with `--safety off` settles the pull requests it opened
  while safety was on: a merged one closes its bead, an open one is landed
  the safety-off way and then closed with a comment naming the commit. A
  pull request someone pushed to after `sc` opened it is left alone.
- In autopilot the product owner pitches new features once the
  maintenance work is planned, instead of only proposing what is broken, so
  a mature repository no longer runs it dry. Each pitch says what it adds
  for the user and is scoped to one shippable slice; an empty plan now has
  to name the directions it considered.
- A resume says when the repo or global config now disagrees with the
  run's recorded ammo, safety or cartridge, and names the flag that would
  apply it.
- The TUI takes every event already waiting in one step instead of one per
  update, so a long backlog catches up at once rather than trailing the run.
- Graph boxes widen from 22 up to 44 columns when the whole graph fits
  across the terminal, so bead titles are shown whole on a wide screen.
- Agent cards show the title of the bead each agent is working on; on a
  narrow terminal the cartridge name gives way to it.
- The PRs view's BEAD column fits the longest bead id (up to 24 columns), so
  a repository prefix such as `PandaJump-5c5` no longer pushes the PR number
  out of its column.

### Fixed

- Dandelion route generation selection now freezes each launch's spawn time
  and current/previous direct event starts, then selects history in
  `(previous event start, current event start]` before matching the exact
  agent/attempt. This fixes rotation with five direct starts and only four
  retained event starts: current replay recovers generation five while an old
  last-seen card keeps its frozen interval and model four. Arrival skew,
  invalid intervals, missing/ambiguous/damaged history and overflows are
  `unknown`, never a previous-generation, global/latest-route, `pending` or
  `routed` guess. Requested and routed models remain separate.

- Parallel planning commit/resume recovery retains returned IDs,
  `Unfinished` acceptance evidence and the run epic. A fresh run-scoped
  listing is used when positive ID evidence is absent: a proven graph is
  adopted without a second apply and review/plan publication happens once,
  while an unfinished round whose graph cannot be proven regenerates. Resume
  therefore cannot duplicate planning, graph creation or epic adoption.

- Dashboard balance projection normalizes literal `$` to `USD` and retains
  only `amount` and `currency`, with regression coverage for projection and
  clone isolation. The final live Kilo balance was `31.232105 USD`, rendered
  as `balance $31.23` exactly once without a synthetic gauge or percentage;
  Kilo remains visible in `usage only` with zero windows. The dashboard
  service keeps `ProtectHome=read-only` and adds only narrow `ReadWritePaths`
  exceptions for `~/.local/share/kilo`, `~/.cache/kilo` and
  `~/.local/state/kilo`, required by Kilo's runtime startup/auth paths.
- A run that stamps its lock just as `sc --force` takes the lock over no
  longer writes its own record back over the takeover's. The check that the
  lock is still the file it read compared only device and inode, and ext4
  and other filesystems hand a freed inode number to the next new file, so a
  third `sc` could find a lock naming a dead process and start next to a
  live run. The stamp now also requires the file's content to be unchanged.
- An agent no longer leaves processes running after it finishes. A tester
  that drove a TUI with `script -c "npm start"` left the app running for good,
  because `script` moves it out of the agent's process group, and one long
  run piled up 162 dashboards and over 20 GiB of memory. Every agent session
  and the delivery check now tag their environment, and when they exit the
  run stops their process group and, on Linux, every process still carrying
  the tag: SIGTERM first, so a git can finish and remove its lock, then
  SIGKILL after the grace period.
- A bead being re-delivered after a conflict round is never delivered a
  second time at once. The conductor could list it while it passed through
  approved and start another delivery in the same worktree, and both checks
  failed.
- `Conductor.release` now clears a bead's stale `shipped` marker as well as
  its in-flight reservation. A conflict or failed check sent back and
  re-approved between conductor listings is therefore delivered again,
  instead of remaining approved without a delivery.
- Dandelion route labels retain valid exact-attempt evidence after resumes.
  Direct run-started records bind history to `(incarnation/run, agent,
  supervisor attempt)` instead of resetting it, old last-seen cards keep their
  own model, and duplicate attempts within one generation remain `unknown`.
  This fixes valid routes previously suppressed after resumes without changing
  adapters or requested `domain.Agent.Model`, and without global/latest-route
  guessing.
- A firing whose context is cancelled when its attempt also fails now returns
  `errors.Join` of both errors, preserving the context cancellation and the
  attempt failure for `errors.Is` instead of discarding the cancellation.
- A bead whose work is already on the default branch is closed instead of
  handed to a person when its next delivery finds nothing to merge: a commit
  whose subject starts `sc(<bead>): ` now counts as proof that it landed,
  when the run recorded neither the branch's base nor a merge SHA.
- The TUI no longer shows an agent from before `sc resume` as running
  forever. A resumed run marks the previous process's unfinished agents as
  killed, so they leave the live list and stop counting against ammo.
- A bead whose re-delivery after a conflict round cannot be fast-forwarded
  (the default branch moved while its check ran) is retried like any other
  delivery, instead of being left in delivering with its engineer's session
  marked failed.
- Agents no longer fail to start when several fire at once. The read-only
  bd check opened the tracker for writing on every start, which failed as
  "no beads database found" while other agents were writing; bd's own
  read-only enforcement is now checked once per bd binary. A process-wide
  double-checked mutex makes the load/verify/store sequence atomic, with a
  16-way overlap regression and failure-not-cached retry coverage. The mutex
  coordinates only one `sc` process; independent processes still rely on bd's
  own locking.
- A resumed bead whose scope the product owner narrowed resumes in its
  fresh review round. The replay used to keep it at the capped round that
  escalated it and escalate it again.
- Testers no longer send a bead back because its branch is behind the
  default branch. On a busy run master moved faster than engineers could
  rebase, and one bead spent three rounds chasing it; the delivery rebases
  and checks before anything lands.
- A resumed run no longer fires reviews of a bead whose worktree is
  mid-rebase. The delivery handoff owns a bead for the whole of its conflict
  round; the feedback loop used to review it at the same time, every tester
  failed for want of a diff and the bead was marked failed under the
  engineer still resolving it.
- Claude agents no longer move commands into the background. A headless
  session ended "waiting" for its test run and produced no result block
  (`bad_output`), losing the whole session. Foreground commands get a
  10-minute default and 30-minute maximum Bash timeout instead.
- The product owner's ruling on a bead that failed its delivery check at
  the review-round cap is applied. The conductor used to overwrite the
  escalation with the delivery's changes_requested, so every ruling was
  refused and the bead ended up failed.
- A bead is never given two engineers at once. A resumed run that found a
  delivery stopping on conflicts used to fire the conflict round and a
  fresh implement round into the same worktree.
- With safety off, an approved bead whose branch has nothing to merge is
  closed when every tester approved it (another bead already landed the
  work), instead of being handed to a human.
- A conflict or rework round no longer claims its bead as `sc:sc:<agent>`,
  which made the engineer's lease renewals fail.
- Re-delivering a bead whose pull request has already merged no longer
  pushes the old branch tip (which failed as non-fast-forward).

## [0.1.0] - 2026-09-05

First release. There is no earlier tag, so this entry is the whole of the
initial development — every commit from `4d43b6a` (init, 2026-09-02) to the
tag, which is phases 0 to 10 of `IMPLEMENTATION-PROGRESS.md`.

### Added

- **The cannon.** `sc ./repo` fires a crew of LLM agents at a git repository: a
  product owner plans the work as beads (`bd` issues), engineers implement each
  bead in its own git worktree, and three tester lenses (UX, QA, test) review
  it. An engineer may push back on feedback, the product owner arbitrates, and
  the loop runs until every tester is satisfied or a limit sends the bead to a
  human. `sc ./repo -m "brief"` runs one brief and finishes; a bare path is
  autopilot, which ends only when you stop it or the cartridges run out of
  credits (D-18).
- **Commands**: `run` (also the bare `sc PATH` form), `status`, `logs`,
  `resume`, `stop`, `doctor`, `cartridge`, `ammo`, `safety`, `vibes`, `rules`,
  `config show|path|edit`, `completion` (bash, zsh, fish) and `version`.
  Persistent flags `--repo`, `--global`, `--json`, `--verbose`, `--no-color`;
  run flags `-m/--message`, `--headless`, `--resume`, `--ammo`, `--safety`,
  `--cartridge`, `--review-prs`, `--no-anim`, `--force` and `--dry-run`.
  Full reference in [docs/cli.md](docs/cli.md), generated from the command tree.
- **TUI** with five views — Graph (the bead DAG), Agents (a card and a
  transcript per agent), Feedback (review rounds, verdicts, responses,
  arbitration), PRs (deliveries and their pull requests) and Log — plus live run
  controls (pause, stop, re-fire a bead the run gave up on, change ammo or
  safety mid-run, raise a budget that paused the run) and cannon
  animations on every shot, burst and close. `--no-anim` / `SC_NO_ANIM=1` turns
  the animation off, `NO_COLOR` the colour.
- **Headless mode** (`--headless`): the same run rendered as one line per event
  with a fixed glyph table, plus an `events.jsonl` stream, for CI and for
  terminals that are not terminals. Exit codes are a contract: `0` finished or
  stopped, `1` failed, `3` every cartridge out of credits.
- **Cartridges** — the LLM CLI adapter layer: an interface, a registry, a
  scriptable mock used by the tests and the demo, a structured-output contract
  with JSON Schemas for all six message types, per-role tool policies, an
  error-kind mapping (rate limit vs. out of credits vs. crash) and an
  eight-check conformance suite. Claude Code is the only adapter that ships
  (D-19); [docs/cartridges.md](docs/cartridges.md) documents adding another.
- **Beads as the task graph.** `sc` reads and writes `bd` 1.2.2+ exclusively
  through the CLI, serialised through one writer: custom statuses for the
  workflow states, `sc_` metadata and `sc:` labels for run bookkeeping,
  structured bead comments as the agents' messages, atomic graph plans with
  explicit dependency edges, claims with leases, and native `gh:pr` gates so a
  bead closes only when its code has landed. Agents get a read-only `bd` shim on
  `PATH`. See [docs/beads-conventions.md](docs/beads-conventions.md).
- **Git, worktrees and delivery.** One worktree per bead, leftovers
  auto-committed so reviewers always see a committed diff, size-capped diffs for
  reviewers, and two delivery paths (D-10): safety on pushes `sc/<bead>-<slug>`
  and opens a pull request with `gh`; safety off rebases, runs the repository's
  check command (detected when not configured) and fast-forward merges. A merge
  conflict goes back to the engineer as a conflict round.
- **External pull request review** (`--review-prs`): the cannon also reviews
  open pull requests it did not open and posts one consolidated review per round.
- **Resume and crash recovery**: run state in a versioned `state.json`, one run
  per repository behind a lock (`--force` to break a dead one), `sc resume` to
  continue the most recent run, and reattachment that clears the wreckage a
  killed run leaves behind, including stale git index locks.
- **Limits and budgets**: review, pushback and conflict round caps, an agent
  timeout, a plan size cap, plan rejection and failure caps, and opt-in token
  and cost budgets that pause an autopilot run with a banner rather than ending
  it.
- **Configuration**: a global `$XDG_CONFIG_HOME/slop-cannon/config.toml` and a
  per-repository `.slop-cannon/config.toml`, merged under CLI flags, with
  `schema_version = 1` and in-memory migration on load. `sc config show` prints
  the merged result with the source of every value. Every key is documented in
  [docs/config.md](docs/config.md).
- **Nothing of ours inside your repository** (D-23): runs, logs, prompts and
  worktrees live under `$XDG_STATE_HOME/slop-cannon/`. `bd init` runs with
  `--setup-exclude`, and the `.gitignore` hunk it adds is reverted when it is
  the only change. The one committable file is `.slop-cannon/config.toml`, and
  only a config command creates it.
- **`sc doctor`** and a first-run experience that check `git`, `bd` (with the
  1.2.2 version gate), `gh` and the configured cartridge CLIs, and print a
  numbered fix list when something is missing.
- **Documentation**: a [docs index](docs/README.md) over the CLI, config, TUI,
  feedback view, prompts, architecture, cartridges, beads conventions, JSON
  schemas, performance, goldens and a grep-verified
  [troubleshooting guide](docs/troubleshooting.md) covering every error the code
  can print.
- **A deterministic demo**: `make demo` builds a throwaway machine (a copy of
  the fixture repository with a bare origin, stand-ins for `bd` and `gh`, its
  own `HOME`) and fires the real `sc run` at it with the mock cartridge, so the
  recorded GIF in the README is the product, not a mock-up.
  `make demo DEMO_ARGS=--check` asserts the run from its own `events.jsonl`.
- **Packaging**: goreleaser builds static `linux`/`darwin` binaries for `amd64`
  and `arm64` with `checksums.txt`, a Homebrew cask, `install.sh`, and a GitHub
  Actions workflow that gates on `make check` and releases on a `v*` tag.
  `sc version` reports the tag, commit and build date it was built from.

### Security

- Secrets hygiene end to end: `.env`, `.env.*`, `*.env`, `*.pem` and `id_*` are
  always withheld from agents, `exclude` globs add more, and a withheld file's
  diff is replaced by a placeholder that names neither the path nor the pattern.
  Credentials that appear anyway — `sk-`/`sk-ant-` keys, GitHub tokens, `Bearer`
  values, private key bodies, AWS access key ids, `*_TOKEN=`-style assignments —
  are masked once, where the run publishes, so they reach neither the terminal,
  nor `events.jsonl`, nor `sc.log`.
- Agent output is scrubbed of escape and control characters before it is drawn
  or logged, so a cartridge cannot repaint the terminal.
- Agents cannot write to the task graph: each worktree gets a `bd` shim that
  execs the real binary with `--readonly`.
- Safety on (the default) never touches the default branch, and `sc` never
  merges a pull request it did not open, whatever the safety setting.

### Known limitations

- Claude Code is the only cartridge (D-19). Kimi, Animus and a custom-command
  adapter are planned and deliberately deferred.
- `bd` 1.2.2 or newer is required, and `gh` is required for safety-on delivery
  and for external pull request review.
- The Go module path is `slop-cannon`, so there is no `go install` from a module
  proxy yet: install from a release archive, `install.sh`, the Homebrew cask, or
  build from a checkout with `make build`. The Install footer on the GitHub
  release page says the same, and offers a `git clone` at the tag rather than a
  `go install`.
- The repository has no `origin` remote yet, so its owner and name — and every
  URL built from them, including the two link definitions below — are a guess
  (`maxh213/slop-cannon`), and `v0.1.0` is deliberately not tagged. Both links
  therefore resolve to nothing as shipped. They start working when the
  repository is published under that name and the tag is pushed; if it lands
  anywhere else, they move with it, together with the defaults in
  `.goreleaser.yaml`, `install.sh` and the README. See
  [docs/releasing.md § Cutting a release](docs/releasing.md#cutting-a-release).

<!-- Provisional: see "Known limitations" above — no remote, no v0.1.0 tag. -->
[Unreleased]: https://github.com/maxh213/slop-cannon/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/maxh213/slop-cannon/releases/tag/v0.1.0
