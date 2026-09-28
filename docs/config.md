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
| `gate_poll_interval` | duration > 0 | `"1m"` | both | Safety on only: how often the run asks bd whether the pull request behind a `gh:pr` gate has merged, so the bead closes (P6-10, D-20). Each poll runs `gh pr view` once per open gate. |
| `pr_poll_interval` | duration > 0 | `"1m"` | both | How often the run asks GitHub for open pull requests it did not open, so it can review them (D-11, P8-04). Each poll runs `gh pr list` once. It also bounds how quickly a review bead is settled when its pull request is merged or closed while the review is still in flight (P8-11), so a run shorter than one interval polls exactly once, at the start. Only used when external review is on — see `--review-prs`. |
| `graph_poll_interval` | duration > 0 | `"2s"` | both | How often the standalone graph watcher lists the beads database (P3-07). Each poll runs one `bd list`, which costs about 0.2 s of bd time; a longer interval makes the graph lag, a shorter one competes with the run's own writes. **Not read by `sc run` today**: a run syncs the graph through its conductor instead, on that component's own faster cadence, and the watcher this key configures has no production caller yet (P1-20). Setting it changes nothing until it does. |
| `max_diff_bytes` | integer ≥ 1024 | `204800` (200 KiB) | both | The largest unified diff a reviewer is handed in full (P4-04). A bigger change set comes back as a file list with per-file statistics and the testers open the files themselves. At least 1 KiB: a smaller cap truncates every diff to nothing, and `0` would be read as "use the default" rather than "show nothing". |
| `re_review_threshold` | number in (0, 1] | `0.4` | both | The D-09 fraction of changed lines above which an earlier approval no longer stands: when a round moved more than this share of the change set, every tester re-reviews instead of only those who did not approve. `1` means only a complete rewrite does that; `0` is refused because it would be read as "use the default". |
| `max_beads_per_plan` | integer ≥ 1 | `8` | both | How many beads one PO planning firing may propose. A plan with more is rejected outright and the PO is asked to redo it — the surplus is never trimmed, because dropping beads would break the dependencies of the ones that stayed (P5-02). |
| `limits.max_review_rounds` | integer ≥ 1 | `5` | both | Review rounds per bead before the PO escalates (D-08). |
| `limits.max_pushback_rounds` | integer ≥ 1 | `2` | both | Pushback exchanges per feedback item before PO arbitration (D-08). At least one: the loop always lets the engineer answer an issue once before the PO rules on it, and `0` is refused rather than silently read as the default (P9-01). |
| `limits.max_conflict_rounds` | integer ≥ 1 | `3` | both | How many times one delivery may send a bead back to its engineer to resolve a merge conflict before it is parked in `needs_human` (D-10, P6-06). Conflict rounds are deliberately outside `max_review_rounds` and `max_pushback_rounds`, so this is the only thing bounding them: an engineer that keeps leaving the worktree mid-rebase would otherwise loop forever. Three allows the two rounds a legitimate conflict can need — one for the conflict, one more for the default branch moving again underneath the first resolution — plus a spare. It is not a review budget: a conflict round fires one engineer and no testers unless D-09 asks for them. |
| `limits.agent_timeout` | duration > 0 | `"6h"` | both | Maximum wall-clock time for one agent session. Generous on purpose: an engineer on a real feature can run well past half an hour, and a timeout throws its session away. Lower it to catch a hung CLI sooner. |
| `limits.max_tokens` | integer ≥ 0 | `0` (unlimited) | both | Opt-in token budget for a run: input plus output tokens (cache tokens excluded), summed over every agent session that finished. Crossing it stops new work — no fresh bead is implemented and no plan is made, while the work that finishes what is in flight (a round's reviews and arbitrations, and a delivery conflict round over a bead already taken) still gets its agents. An autopilot run then **pauses** at once with a banner and waits for you: the pause stops the whole dispatch loop, so only the agents already running finish and everything queued waits with it. Resuming grants another budget's worth, so it pauses again rather than carrying on unwatched (D-18). A brief run has no pause: it keeps offering the finishing work and ends when that drains, with the budget in its summary. Enforced only as far as the cartridge reports usage: a CLI that counts nothing spends nothing. **The cap is a floor on when new work stops, not a ceiling on what the run spends**: the session that crosses it has already been paid for, and every review the round already owes is still fired, so the total can land at several times the cap — a 4000-token cap over a three-lens round has been seen to finish at 13000. Set it as "stop starting things after roughly this much", not as "never spend more than this". The budget belongs to the run, not to one `sc run`: it is written into the run's `state.json` and read back by `--resume`, so a resumed run carries the earlier invocations' spend instead of starting again at zero. A resume over a crossed cap is you saying carry on, so it grants one more budget's worth — the same grant that clearing a pause makes, and no more. A paused run's cap can also be **raised in place**: `b` in the TUI asks for a new ceiling and the run carries on to that number instead of to another grant (P9-14). It changes the live run only — the file is not rewritten, and `--resume` behaves by the configuration the run was started with (P6-11). Reviewing somebody else's pull request (D-11) is new work too: once the cap is crossed no further tester is fired for one, including for a review already open, because the watcher adopts pull requests on its own and a run held at its cap must not keep taking them on. |
| `limits.max_cost_usd` | number ≥ 0 | `0` (unlimited) | both | Opt-in cost budget, same semantics as `max_tokens` — including the overshoot, and including the fact that the budget survives `--resume`. It waits for a cartridge that reports a price: an adapter whose CLI prints tokens but no cost never trips it, however long the run goes on. |
| `limits.max_plan_rejections` | integer ≥ 1 | `2` | both | How many plans in a row the planning validator may reject before the run gives up on that round of planning (P6-19). One rejection has already cost two plans: a rule failure is retried once inside the firing itself. A brief run that gives up finishes and says so; an autopilot run cools off and plans again (D-18). **The floor of 1 is deliberate**: the Go option behind this key (`orchestrator.PlannerOptions.RejectionLimit`) reads a negative value as "never give up", and the file cannot say that on purpose — a plan the validator keeps rejecting is a plan that keeps costing a product owner session, and in autopilot, which never ends on its own (D-18), "never give up" is a run that plans for ever with nothing to show. An embedder that wants it can pass the option; a config file cannot ask for it (P9-14). |
| `limits.max_plan_failures` | integer ≥ 1 | `5` | both | How many planning firings in a row may fail for a reason that is not the plan's fault — a cartridge crash, an agent timeout, an unreadable rules file — before the run gives up on that round the same way. Looser than `max_plan_rejections` because the supervisor has already spent its own retries on those failures. Its floor of 1 is deliberate for the same reason. |
| `cartridges.<name>.model` | string | `""` | global | Model passed to the CLI, e.g. `fable` for Claude Code. |
| `cartridges.<name>.command` | string | `<name>` | global | Executable to run instead of the cartridge name (a full path or a different binary). |
| `cartridges.<name>.args` | string array | `[]` | global | Extra arguments prepended to every invocation. |
| `cartridges.<name>.env` | table of strings | `{}` | global | Extra environment variables for the CLI process. |
| `roles.<role>.cartridge` | string | `""` | both | Per-role cartridge override (`po`, `engineer`, `tester`), D-12. Parsed and validated now; it only has an effect once a second cartridge exists. |

A budget pause is cleared from the TUI (`p` resumes a paused run), and only
from there: `sc resume` restarts an *earlier* run from disk rather than
resuming the live one. So an autopilot run started with `--headless` that
crosses its budget stays paused, holding the repository's run lock, until you
interrupt it with Ctrl-C, which finishes it as a user stop: the banner naming
the cap stays in the run log, and the summary still reports what the run
spent. Give a headless autopilot run a budget you are content for it to stop
at, or run it in the TUI (P9-01, P9-14).

Only `claude` is a valid cartridge name for now (D-19). Any other name, in a
file or on the command line, fails with
`cartridge "<name>" is not supported yet (only "claude" for now)`.

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

`ammo` is a concurrency cap, not a fixed roster. The scheduler reserves one
slot for the product owner when it needs one, lets at most
`max(1, floor((ammo - 1) / 4))` engineers work at once, and gives every
remaining slot to testers so each engineer can have three reviewers.

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

[limits]
max_review_rounds = 3
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
