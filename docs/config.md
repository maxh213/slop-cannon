# Configuration

`sc` reads two TOML files and merges them with built-in defaults. Every
config subcommand (`sc cartridge`, `sc ammo`, `sc vibes`, `sc rules`,
`sc safety`) edits one of them; `sc config show` prints the merged result
with the layer each value came from; `sc config path` prints both paths.

## Files and precedence (D-03)

Highest precedence first:

| Layer | Where | Source label |
|---|---|---|
| CLI flags | `--ammo`, `--safety`, `--cartridge` on a run | `flag` |
| Repo config | `<repo>/.slop-cannon/config.toml` | `repo` |
| Global config | `$XDG_CONFIG_HOME/slop-cannon/config.toml` (default `~/.config/slop-cannon/config.toml`) | `global` |
| Built-in defaults | see the table below | `default` |

- Config subcommands write the **repo** file when the working directory (or
  `--repo`) is inside a git repository, otherwise the **global** file.
  `--global` always forces the global file.
- Cartridges (`[cartridges.*]` and `active_cartridge`) are subscriptions and
  live **only** in the global file. If a repo file sets them they are
  ignored and `sc config show` prints a warning.
- `vibes` and `rules` are **unioned** across layers (global entries first,
  then repo entries, duplicates removed by absolute path), so a global rules
  file still applies when a repository adds its own. Their source shows as
  `global+repo` when both files contribute.
- Every other key is replaced by the higher layer.
- The global file is written with mode `0600` (cartridge `env` may hold
  tokens); the repo file with `0644` so it can be committed. Both are written
  atomically (temp file in the same directory, fsync, rename), so a crash
  leaves either the old or the new file, never a partial one.
- A relative `XDG_CONFIG_HOME` / `XDG_STATE_HOME` is ignored, as the XDG
  spec requires.

Run state (logs, prompts, worktrees) never lives in the repository (D-23).
It goes under `$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/<run-id>/`
(default `~/.local/state/...`), where `<repo-id>` is the repository's
directory name plus 12 hex digits of the SHA-256 of its canonical absolute
path, for example `my-app-3f2c9a1b7d0e`.

## Keys

Unknown keys are errors ("unknown key" with the line number), so typos never
pass silently. Durations are Go duration strings (`"10m"`, `"1h30m"`,
`"45s"`); a bare number is rejected, and the refusal names the file, the
line and the column of the offending value like every other parse error
(`config.toml:7:24: gate_poll_interval: invalid duration "30"`).

| Key | Type | Default | Files | Meaning |
|---|---|---|---|---|
| `schema_version` | integer | `1` | both | Layout version; written by every save. Older files are migrated on load; a newer version is refused with "upgrade sc". A missing key means version 1. |
| `active_cartridge` | string | `""` | global | Cartridge used by runs (and by every role without an override). The first cartridge added becomes active; `sc cartridge --use NAME` switches. Must name a configured cartridge. A run refuses to start while it is blank. |
| `ammo` | integer ≥ 3 | `10` | both | Concurrency cap on agent subprocesses. `sc ammo N` prints the allocation it implies (see below). |
| `safety` | bool | `true` | both | `true`: finished work is pushed as a branch and opened as a PR for a human to merge. `false`: rebased and fast-forward merged straight into the default branch (D-10). |
| `vibes` | string array | `[]` | both | Style preference files appended to every agent prompt as soft guidance. Paths are relative to the config file's directory (a file at the repo root is stored as `../vibes.md`) or absolute. Each file must exist, be UTF-8 text, and be at most 64 KiB. |
| `rules` | string array | `[]` | both | Hard constraint files appended to every agent prompt as MUST / MUST NOT. Same path and size rules as `vibes`. |
| `exclude` | string array | `[]` | both | Extra path globs an agent must never be shown, on top of the always-on `.env`, `.env.*`, `*.env`, `*.pem` and `id_*` patterns (P9-05). A pattern is matched against a path's base name, its whole repository-relative path and every parent directory, so `certs` hides `certs/server.key`. Lists union global-then-repo, and the entries are globs rather than paths, so they are never resolved against the config file's directory. A withheld file is left out of the tree an agent sees and its diff is replaced by a placeholder that names neither the path nor the pattern. This key is the path half of secrets hygiene; the value half masks credentials wherever they are printed — see [Secrets: withheld files and masked values](#secrets-withheld-files-and-masked-values). |
| `check_command` | string | `""` | both | Command run in the worktree before a safety-off merge. Empty or blank means autodetect in this order: `make test` (a Makefile with a `test` target), `npm test` (a `package.json` with a `test` script), `go test ./...`, `pytest`, `cargo test`, by the files present. Nothing detected means no check runs. |
| `idle_replan_interval` | duration > 0 | `"10m"` | both | Autopilot only: when the PO has nothing to add, the run idles this long before planning again (D-18). |
| `signals_command` | string | `""` (off) | both | Autopilot only: a read-only command that prints what production is saying — recent errors from an error tracker, a log query, a chat export (P6-71, D-32). Like `check_command` it is run with `sh -c` in the repository root with your whole environment, so it can use the tokens your shell already has; **sc runs it, never the product owner**, whose shell stays the read-only allow-list. Its standard output (the last 32 KiB, cut at a line boundary, with control characters scrubbed and secrets masked) is given to the planning product owner, fenced as untrusted evidence, every time it plans; it is told to propose fixes for real issues first, to cite evidence by reference (an error id, a timestamp, a room or conversation id) and never to copy names, email addresses or message text into a bead; a plan that does is rejected and sent back to it with the reason before anything reaches bd — sc checks every bead's title, description and acceptance criteria (and the plan's brief) for an email address, a phone number or an IP address that the reading contains, and for 8 or more of the reading's words in a row, without quoting what was copied. That check is mechanical, not a privacy guarantee: a name on its own is not caught. What it prints is kept with the run like every prompt (in the run directory's `prompts/` and the transcripts), so give the run directory the care you give that output; keep tokens in the environment rather than written into the command, which `sc config show` and the run's state show as written. Only a bounded tail of its output is held while it runs. A command that fails, exits non-zero or times out never fails a plan: it is one warning per kind of failure (an exit status, a timeout) and a one-line note in the prompt; a reading cut short by the run stopping is dropped without a warning. A changed reading that prints something also offers a plan of its own while engineers are busy, at most once per `signals_interval`, not while every bead of the previous signals-driven plan is still `open`, not while the run already has `po_planners × max_beads_per_plan` or more beads still to ship (open, in progress, in review, changes requested, approved, delivering or escalated; a bd listing that fails counts as full), so the backlog never outgrows what the run and `auto_merge_spacing` can ship, and not during the `idle_replan_interval` hold after a plan that added nothing or failed. That plan is told engineers are still busy and to propose fixes for the new evidence alone, never duplicates of open or in-progress beads. The first reading after `sc resume` is the baseline: only a change from it offers a plan. Brief runs never run it. Because a repository's own `.slop-cannon/config.toml` can set it, read that file before you fire at a repository you did not write, as you would for `check_command`. |
| `signals_interval` | duration > 0 | `"30m"` | both | Least time between two runs of `signals_command`. A plan in between uses the last reading; `sc status` shows when it last ran, its exit code, its size and when it is next due. |
| `gate_poll_interval` | duration > 0 | `"1m"` | both | Safety on only: how often the run asks bd whether the pull request behind a `gh:pr` gate has merged, so the bead closes (P6-10, D-20). Each poll runs `gh pr view` once per open gate. |
| `pr_poll_interval` | duration > 0 | `"1m"` | both | How often the run asks GitHub for open pull requests it did not open, so it can review them (D-11, P8-04). Each poll runs `gh pr list` once. It also bounds how quickly a review bead is settled when its pull request is merged or closed while the review is still in flight (P8-11), so a run shorter than one interval polls exactly once, at the start. Only used when external review is on — see `--review-prs`. |
| `graph_poll_interval` | duration > 0 | `"2s"` | both | How often the standalone graph watcher lists the beads database (P3-07). Each poll runs one `bd list`, which costs about 0.2 s of bd time; a longer interval makes the graph lag, a shorter one competes with the run's own writes. **Not read by `sc run` today**: a run syncs the graph through its conductor instead, on that component's own faster cadence, and the watcher this key configures has no production caller yet (P1-20). Setting it changes nothing until it does. |
| `max_diff_bytes` | integer ≥ 1024 | `204800` (200 KiB) | both | The largest unified diff a reviewer is handed in full (P4-04). A bigger change set comes back as a file list with per-file statistics and the testers open the files themselves. At least 1 KiB: a smaller cap truncates every diff to nothing, and `0` would be read as "use the default" rather than "show nothing". |
| `re_review_threshold` | number in (0, 1] | `0.4` | both | The D-09 fraction of changed lines above which an earlier approval no longer stands: when a round moved more than this share of the change set, every tester re-reviews instead of only those who did not approve. `1` means only a complete rewrite does that; `0` is refused because it would be read as "use the default". |
| `max_beads_per_plan` | integer ≥ 1 | `8` | both | How many beads one PO planning producer may propose. A producer over this cap is rejected and asked to redo it — the surplus is never trimmed, because dropping beads would break the dependencies of the ones that stayed (P5-02). With `po_planners = N`, all candidates also share one proposed and accepted graph cap of `N × max_beads_per_plan`; exceeding that combined cap rejects the round rather than trimming it. The default one producer therefore retains the original cap of 8. |
| `po_planners` | integer ≥ 1 | `1` | both | How many independent PO producers run in one planning round (D-41). Values below 1 fail validation with `po_planners: must be at least 1`. The default preserves the exact legacy demand (zero `Slots`, treated as one), one `po-plan-1` producer, no semantic judge, no candidate namespacing and no contributor list. Above 1, producer IDs are unique, all producers finish before review or graph writes, and one combined round reserves N slots atomically; overall used slots never exceed `ammo`, and a round larger than ammo cannot reserve partial capacity. Planning and arbitration have independent caps; a planning cap above ammo is filled in ammo-sized waves, but that does not make an oversized weighted round reservable. The combined proposed/accepted cap is `N × max_beads_per_plan` (**16** at N=2 with the default 8). All contributors in a round share one signals reading, and the signals-backlog threshold uses the same combined cap. Animus intends `2`; that is intended configuration, not a deployment claim. |
| `nice` | integer 0-19 | `10` | both | Scheduling niceness a run gives itself before it fires anything, so every agent, check command and test browser it starts inherits it and a busy run does not starve the rest of the machine. `0` leaves the priority alone; `19` is the lowest. A run that cannot lower its priority says so and fires anyway. |
| `research` | bool | `true` | both | Lets the planning product owner search and read the web (the CLI's WebSearch and WebFetch; nothing else is added), so an autopilot plan researches comparable products before it pitches features. Only planning firings get it. `false` keeps the product owner to the repository. |
| `auto_merge` | bool | `false` | both | Safety on only (refused with `safety = false`): the run squash-merges **the pull requests it opened itself** once they are green, instead of waiting for a human (D-31). A pull request is merged only when every clause of the ownership check holds — its head is exactly the bead's `sc/<id>-<slug>` branch, its base is the default branch, it carries the `slop-cannon` label, its body's marker names this run and this bead, its number is the bead's `sc_pr_number`, it was created after the run first started, and its author is the `gh` login — and only when its head is the commit the run delivered and its testers approved (`sc_pr_head`) or that commit plus the clean merges of the default branch GitHub made because the run asked it to (`gh pr update-branch`, recorded in `state.json` first; a commit anybody else pushed to the branch, or an update the run did not ask for, sends the bead to `needs_human`), the bead's local branch has no approved commits the pull request lacks yet (a re-delivery that has not pushed waits), it is open, not a draft, has at least one check, every check has finished and passed, every `auto_merge_checks` name is among them, GitHub reports it `CLEAN`, and `auto_merge_spacing` has passed since the run's last merge. The merge is exactly `gh pr merge <n> --squash --match-head-commit <sha>`: never `--admin`, `--auto` or `--delete-branch`, so branch protection is never bypassed and a push after the checks ran cancels it. Refused without at least one `auto_merge_checks` name. A pull request whose head does not contain the default branch's current tip is updated by GitHub first (`gh pr update-branch`, once per head commit GitHub accepted it for) and merged only once its checks have passed again on the updated head: the run checks the ancestry itself (`git merge-base --is-ancestor`), because GitHub reports a pull request `BEHIND` only when branch protection requires branches to be up to date, and without that a pull request tested before the run's previous merge would still read `CLEAN`; a conflicted one, or one with a failed check, goes back to its engineer as a change request (once per head, kept across a resume); a blocked one (a required review) waits. Nothing that existed before the run started is ever merged. Frozen with the run, so `--resume` keeps it; `--auto-merge` overrides it like `--safety`. |
| `auto_merge_checks` | string array | `[]` | both | Check-run or status-context names that must be present on the pull request **and** successful before an auto-merge, on top of "every check that is there passed". **Required with `auto_merge`** (an empty list is refused): GitHub lists only the checks it has created so far, so a quick title check that passes before CI has even registered the tests would otherwise read as green. Name the ones that matter — for otwarteklatki/animus-chat, the `ci.yml` job: `["test"]`. Lists union global-then-repo, so a repository can require more checks but never fewer. For a repository whose CI jobs are `test` and `build` plus a title check: `["test", "build", "conventional-title"]`. |
| `auto_merge_spacing` | duration > 0 | `"10m"` | both | The least time between two auto-merges by one run, so each deploy a push to the default branch triggers finishes before the next merge starts another. The time of the last merge is written into the run's `state.json` before the merge is made, so a resumed run waits out the rest of it too. |
| `conventional_pr_titles` | bool | `false` | both | Titles the run's pull requests `<type>: <title starting lowercase>`, the type taken from the bead's issue type (`bug` → `fix`, `feature` → `feat`, `chore` and `task` → `chore`, anything else `feat`), for repositories whose CI enforces Conventional Commit titles. A squash merge makes the title the commit subject, so it is also what release-please reads. A bead title that already starts `<type>[(scope)][!]: ` keeps only its scope and subject: the type is always the bead's and a breaking-change `!` is dropped, so a planning agent's title never picks the version bump. An acronym at the start keeps its capitals (`feat: API keys rotate`). |
| `limits.max_review_rounds` | integer ≥ 1 | `5` | both | Review rounds per bead before the PO escalates (D-08). |
| `limits.max_pushback_rounds` | integer ≥ 1 | `2` | both | Pushback exchanges per feedback item before PO arbitration (D-08). At least one: the loop always lets the engineer answer an issue once before the PO rules on it, and `0` is refused rather than silently read as the default (P9-01). |
| `limits.max_conflict_rounds` | integer ≥ 1 | `3` | both | How many times one delivery may send a bead back to its engineer to resolve a merge conflict before it is parked in `needs_human` (D-10, P6-06). Conflict rounds are deliberately outside `max_review_rounds` and `max_pushback_rounds`, so this is the only thing bounding them: an engineer that keeps leaving the worktree mid-rebase would otherwise loop forever. Three allows the two rounds a legitimate conflict can need — one for the conflict, one more for the default branch moving again underneath the first resolution — plus a spare. It is not a review budget: a conflict round fires one engineer and no testers unless D-09 asks for them. |
| `limits.agent_timeout` | duration > 0 | `"6h"` | both | Maximum wall-clock time for one agent session. Generous on purpose: an engineer on a real feature can run well past half an hour, and a timeout throws its session away. Lower it to catch a hung CLI sooner. |
| `limits.max_agent_timeout` | duration ≥ `limits.agent_timeout` | `"24h"` | both | The longest session limit the product owner may give one bead in its plan (a bead's `timeout`, recorded as `sc_timeout`) when that bead's work genuinely needs longer than `limits.agent_timeout`. Every agent fired on the bead gets the longer limit; it never shortens the default. |
| `limits.check_timeout` | duration > 0 | `"30m"` | both | Maximum wall-clock time for the repository's check command when a delivery runs it before merging (P4-19). A check that runs out of time is killed and fails the delivery, and the bead goes back to its engineer as a qa change request. Raise it for a slow check under load: a full lint + unit + build + Playwright + quality-gate check on a busy machine was killed at the old fixed 30 minutes, and every delivery of that run failed. |
| `limits.signals_timeout` | duration > 0 | `"5m"` | both | Maximum wall-clock time for one run of `signals_command` (P6-71). A run that times out is killed with everything it started, and the plan goes ahead with a one-line note saying so. |
| `limits.max_tokens` | integer ≥ 0 | `0` (unlimited) | both | Opt-in token budget for a run: input plus output tokens (cache tokens excluded), summed over every agent session that finished. Crossing it stops new work — no fresh bead is implemented and no plan is made, while the work that finishes what is in flight (a round's reviews and arbitrations, and a delivery conflict round over a bead already taken) still gets its agents. An autopilot run then **pauses** at once with a banner and waits for you: the pause stops the whole dispatch loop, so only the agents already running finish and everything queued waits with it. Resuming grants another budget's worth, so it pauses again rather than carrying on unwatched (D-18). A brief run has no pause: it keeps offering the finishing work and ends when that drains, with the budget in its summary. Enforced only as far as the cartridge reports usage: a CLI that counts nothing spends nothing. **The cap is a floor on when new work stops, not a ceiling on what the run spends**: the session that crosses it has already been paid for, and every review the round already owes is still fired, so the total can land at several times the cap — a 4000-token cap over a three-lens round has been seen to finish at 13000. Set it as "stop starting things after roughly this much", not as "never spend more than this". The budget belongs to the run, not to one `sc run`: it is written into the run's `state.json` and read back by `--resume`, so a resumed run carries the earlier invocations' spend instead of starting again at zero. A resume over a crossed cap is you saying carry on, so it grants one more budget's worth — the same grant that clearing a pause makes, and no more. A paused run's cap can also be **raised in place**: `b` in the TUI asks for a new ceiling and the run carries on to that number instead of to another grant (P9-14). It changes the live run only — the file is not rewritten, and `--resume` behaves by the configuration the run was started with (P6-11). Reviewing somebody else's pull request (D-11) is new work too: once the cap is crossed no further tester is fired for one, including for a review already open, because the watcher adopts pull requests on its own and a run held at its cap must not keep taking them on. |
| `limits.max_cost_usd` | number ≥ 0 | `0` (unlimited) | both | Opt-in cost budget, same semantics as `max_tokens` — including the overshoot, and including the fact that the budget survives `--resume`. It waits for a cartridge that reports a price: an adapter whose CLI prints tokens but no cost never trips it, however long the run goes on. |
| `limits.max_plan_rejections` | integer ≥ 1 | `2` | both | How many plans in a row the planning validator may reject before the run gives up on that round of planning (P6-19). One rejection has already cost two plans: a rule failure is retried once inside the firing itself. A brief run that gives up finishes and says so; an autopilot run cools off and plans again (D-18). **The floor of 1 is deliberate**: the Go option behind this key (`orchestrator.PlannerOptions.RejectionLimit`) reads a negative value as "never give up", and the file cannot say that on purpose — a plan the validator keeps rejecting is a plan that keeps costing a product owner session, and in autopilot, which never ends on its own (D-18), "never give up" is a run that plans for ever with nothing to show. An embedder that wants it can pass the option; a config file cannot ask for it (P9-14). |
| `limits.max_plan_failures` | integer ≥ 1 | `5` | both | How many planning firings in a row may fail for a reason that is not the plan's fault — a cartridge crash, an agent timeout, an unreadable rules file — before the run gives up on that round the same way. Looser than `max_plan_rejections` because the supervisor has already spent its own retries on those failures. Its floor of 1 is deliberate for the same reason. |
| `cartridges.<name>.model` | string | `""` | global | Model passed to the CLI, e.g. `fable` for Claude Code. For `dandelion` it is the router's mode instead and must be `high` (`dandelion route --high`), `standard` or empty (`dandelion route`); the router names the real model (D-30). |
| `cartridges.<name>.command` | string | `<name>` (`cursor-agent` for `cursor`) | global | Executable to run instead of the default (a full path or a different binary). For `dandelion`, the router; each delegated driver's binary is looked up on `PATH`. |
| `cartridges.<name>.args` | string array | `[]` | global | Extra arguments prepended to every invocation. For `dandelion`, appended to the `dandelion route` call. |
| `cartridges.<name>.env` | table of strings | `{}` | global | Extra environment variables for the CLI process. For `dandelion`, set on the router and delegated CLI. Account-home variables such as `DANDELION_CLAUDE_WORK_CONFIG_DIR` and `DANDELION_CODEX_ANIMUS_HOME` are read here first. |
| `roles.<role>.cartridge` | string | `""` | both | Per-role cartridge override (`po`, `engineer`, `tester`), D-12: that role's agents fire on this configured cartridge, with its own `model`, instead of the active one. The role falls back to the active cartridge, with a warning, when this one fails its Detect probe. |
| `roles.<role>.model` | string | `""` | both | Per-role model (`po`, `engineer`, `tester`): that role's agents are fired with this model instead of the cartridge's own, e.g. `roles.po.model = "claude-opus-5-5"` for a stronger planner while engineers and testers stay on the cartridge's model. Applies to every firing of the role, planning and arbitration alike. For a role on `dandelion`, `standard` or `high` selects the per-firing route mode (case and whitespace tolerated); empty, other and legacy values fall back to the configured Dandelion mode, and the real routed model still replaces `spec.Model`. Role models are effective config, not `ConfigSnapshot` fields: current repo role-model changes apply on the next ordinary resume, while the active cartridge and its default model/mode stay frozen (D-39). |

A budget pause is cleared from the TUI (`p` resumes a paused run), and only
from there: `sc resume` restarts an *earlier* run from disk rather than
resuming the live one. So an autopilot run started with `--headless` that
crosses its budget stays paused, holding the repository's run lock, until you
interrupt it with Ctrl-C, which finishes it as a user stop: the banner naming
the cap stays in the run log, and the summary still reports what the run
spent. Give a headless autopilot run a budget you are content for it to stop
at, or run it in the TUI (P9-01, P9-14).

The valid cartridge names are `claude`, `cursor`, `dandelion`, `agy`, `codex`,
`kimi`, `grok`, `junie`, `hermes` and `kilo`. `codex-animus` is deliberately
not a direct cartridge: it is a Dandelion provider driven through `codex`
with an isolated `CODEX_HOME`. Any other name, in a file or on the command
line, fails with `cartridge "<name>" is not supported yet (only "claude",
"cursor", "dandelion", "agy", "codex", "kimi", "grok", "junie", "hermes",
"kilo" for now)`.

A direct Kilo configuration uses the ordinary cartridge keys:

```toml
[cartridges.kilo]
model = "deepseek/deepseek-v4.1-flash-1017"
args = ["--variant", "max"]
```

Kilo CLI 7.5.6 runs with `KILO_NO_DAEMON=1`, project configuration and
automatic updates disabled, a generated in-memory agent config, and the
complete combined prompt on stdin. `read_exec` and `read_write` are supported;
`read_only` is refused. Configured arguments come before the mandatory
invocation controls, so they cannot weaken those controls. At the CLI argument
boundary, the pinned Dandelion model becomes
`kilo/deepseek/deepseek-v4.1-flash-1017`; direct custom IDs and the
unqualified route model in `RunSpec.Model` and logs are not rewritten.

`SC_KILO_MIN_BALANCE_USD=10` is an internal Dandelion control, not a general
run-budget setting: Dandelion sets it for delegated Kilo and the adapter
removes it from the Kilo child environment. Every delegated launch checks a
fresh `kilo profile --json`; a profile failure or balance at or below the
floor fails closed and reroutes without Kilo. The in-session allowance is
`balance - floor`, best effort only: a final request can overshoot before its
cost is reported, concurrent sessions can spend the same balance, and other
clients or later billed steps can still take the account below $10. Direct
Kilo use does not enable this Dandelion floor.

The dashboard's systemd sandbox, not the sc TOML configuration, keeps
`ProtectHome=read-only` and allows writes only through these narrow Kilo
runtime startup/auth exceptions:

```ini
ReadWritePaths=~/.local/share/kilo
ReadWritePaths=~/.cache/kilo
ReadWritePaths=~/.local/state/kilo
```

No broader home write access or writable Dandelion quota cache is required.
The dashboard API normalizes literal `$` to `USD`; its final live Kilo balance
was `31.232105 USD`, rendered as `balance $31.23` exactly once with no
synthetic gauge and with Kilo visible in `usage only` with zero windows.

A common split puts engineers and testers on Cursor and keeps the product
owner on Claude:

```toml
[cartridges.cursor]
model = 'auto'

[roles.engineer]
cartridge = "cursor"

[roles.tester]
cartridge = "cursor"
```

A role on a cartridge other than the active one gets that cartridge's own
`model` (or its `roles.<role>.model`), never the active cartridge's. See
[cartridges.md § Cursor Agent](cartridges.md#cursor-agent-cursor) for what
running testers on a CLI without tool controls costs.

`dandelion` routes each firing to an installed provider allowed by its tool
policy, so it can be the only cartridge. The applied Animus setup uses
`standard` for engineers and testers and `high` for the product owner, with
all three role models sourced from the repo:

```toml
active_cartridge = "dandelion"

[cartridges.dandelion]
model = "high"

[roles.engineer]
cartridge = "dandelion"
model = "standard"

[roles.tester]
cartridge = "dandelion"
model = "standard"

[roles.po]
cartridge = "dandelion"
model = "high"
```

The PO did not fire during the bounded 180-second wait, so `roles.po.model =
"high"` is verified from effective configuration rather than launch-log
evidence.

`read_only` routes only to Claude accounts; `read_exec` can also use Codex or
Codex Animus, Kimi, Grok, Hermes or Kilo; `read_write` can use every routable
provider. Kilo is a standard-route balance fallback only and never joins the
high chain. See
[cartridges.md § Dandelion](cartridges.md#dandelion-dandelion).

## Secrets: withheld files and masked values

Two different leaks need two different tools, and `sc` applies both (P9-05,
P9-09).

**Paths are withheld.** A file matching `exclude` or one of the always-on
patterns is left out of the repository tree an agent is shown, and its diff
is replaced by a placeholder that names neither the path nor the pattern.
See the `exclude` row above.

**Values are masked.** A credential that reaches text anyway — pasted into a
bead, printed by a failing check, echoed by an agent — is rewritten to
`[REDACTED]`, with the hint before it kept so the line still reads:

```
2026-01-01 INFO  MY_API_TOKEN=[REDACTED]
export ANTHROPIC_API_KEY=[REDACTED]
$ curl -H 'Authorization: Bearer [REDACTED]'
the key sk-[REDACTED] was rejected
```

The masking happens once, where a run publishes, so every sink carries the
masked text: the terminal and the TUI, `<run>/sc.log`, `<run>/events.jsonl`,
the agent transcripts under `<run>/agents/` (which `sc logs` reads back),
the prompts of every later agent, the comment the run writes on a bead, and
the body of the pull request it opens — the only one of those that leaves
your machine.

The rules are deliberately few, so that ordinary prose and reviewed code
survive intact. What is masked:

| Rule | Matches | Example |
|---|---|---|
| `sk` | `sk-…` and `sk-ant-…` keys, 8+ characters after the prefix | `sk-ant-api03-…` |
| `github` | `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_` tokens | `ghp_…` |
| `bearer` | an `Authorization: Bearer <token>` value, any case | `Bearer eyJhbGci…` |
| `assignment` | an assignment whose **upper-case** name contains `TOKEN`, `SECRET`, `PASSWD`, `PASSWORD`, `KEY` or `CREDENTIAL`, with a value of 6+ characters — anywhere in a line, including behind a log prefix or a diff's `+` | `MY_API_TOKEN=hunter2…` |
| `pem` | the body of a `-----BEGIN … PRIVATE KEY-----` block | a `.pem` pasted into a comment |
| `aws` | an `AKIA…` or `ASIA…` access key id | `AKIAIOSFODNN7EXAMPLE` |

What is **not** masked, on purpose:

- a lower-case or mixed-case assignment (`apiKey := os.Getenv("API_KEY")`
  in a reviewed diff is code, not a leak), and prose that merely names a
  variable (`set DATABASE_PASSWORD before running`);
- a bare secret with no recognisable shape — a random password on a line of
  its own matches no rule;
- anything in the repository itself. Masking is about what `sc` prints and
  publishes; keeping a credential out of the repository is still your job,
  and `sc doctor` checks that your `.gitignore` covers the usual files.

The escape sequences a cartridge prints are stripped by the same hook, in
front of the masking, so a token cut in half by a colour code is still
masked. Both halves are idempotent: text that has already been through them
is unchanged by a second pass.

## Ammo allocation (D-05)

`ammo` is a concurrency cap, not a fixed roster. The ordinary allocation
reserves one product-owner slot when one is needed, lets at most
`max(1, floor((ammo - 1) / 4))` engineers work at once, and gives every
remaining slot to testers so each engineer can have three reviewers. A
parallel planning demand instead reserves all `po_planners` slots atomically,
as described in the key table; overall used slots still cannot exceed `ammo`.
Planning and arbitration caps are independent.

| ammo | PO | engineers | testers |
|---|---|---|---|
| 3 | 1 | 1 | 1 |
| 4 | 1 | 1 | 2 |
| 10 | 1 | 2 | 7 |
| 12 | 1 | 2 | 9 |
| 13 | 1 | 3 | 9 |

`sc ammo 2` fails with
`ammo must be at least 3: one engineer, one tester, one product owner`.

## Example

Global file, `~/.config/slop-cannon/config.toml`:

```toml
schema_version = 1
active_cartridge = "claude"
rules = ["/home/me/rules/no-force-push.md"]

[cartridges.claude]
model = "fable"
```

Repo file, `<repo>/.slop-cannon/config.toml`:

```toml
schema_version = 1
ammo = 12
safety = false
vibes = ["../vibes.md"]
rules = ["../docs/rules.md"]
check_command = "make check"
# Autopilot only: what production is saying, re-read at most every 30 minutes.
signals_command = "./scripts/recent-errors.sh"
signals_interval = "30m"

[limits]
max_review_rounds = 3
signals_timeout = "2m"
```

`sc config show` in that repository prints, among others:

```
ammo                       12                                    repo
safety                     false                                 repo
vibes                      [/path/to/repo/vibes.md]              repo
rules                      [/home/me/rules/no-force-push.md, /path/to/repo/docs/rules.md]  global+repo
active_cartridge           claude                                global
cartridges.claude.model    fable                                 global
limits.max_review_rounds   3                                     repo
limits.max_pushback_rounds 2                                     default
```

## Schema versions and migration

Every save stamps `schema_version` with the version the running `sc` writes.
On load, a file with an older version is upgraded in memory by the migration
steps in `internal/config/migrate.go` (one step per version, applied in
order) before it is decoded; the file on disk is rewritten only when a
config subcommand saves it. A file with a newer version than the binary
understands is refused so a downgrade never silently drops keys. There are
no migration steps yet: version 1 is the first layout.
