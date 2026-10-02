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

- `sc watch [PATH]` opens the run TUI on a run that is already firing, from
  any terminal: one started with `--headless`, from a script, or in a
  terminal that has since closed. It shows everything the run has done so far
  and then follows it live. It only reads the run's `events.jsonl`: `q` leaves
  while the run keeps firing, and the run controls are not offered.
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
  read-only enforcement is now checked once per bd binary.
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
