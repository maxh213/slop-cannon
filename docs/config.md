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
`"45s"`); a bare number is rejected.

| Key | Type | Default | Files | Meaning |
|---|---|---|---|---|
| `schema_version` | integer | `1` | both | Layout version; written by every save. Older files are migrated on load; a newer version is refused with "upgrade sc". A missing key means version 1. |
| `active_cartridge` | string | `""` | global | Cartridge used by runs (and by every role without an override). The first cartridge added becomes active; `sc cartridge --use NAME` switches. Must name a configured cartridge. A run refuses to start while it is blank. |
| `ammo` | integer ≥ 3 | `10` | both | Concurrency cap on agent subprocesses. `sc ammo N` prints the allocation it implies (see below). |
| `safety` | bool | `true` | both | `true`: finished work is pushed as a branch and opened as a PR for a human to merge. `false`: rebased and fast-forward merged straight into the default branch (D-10). |
| `vibes` | string array | `[]` | both | Style preference files appended to every agent prompt as soft guidance. Paths are relative to the config file's directory (a file at the repo root is stored as `../vibes.md`) or absolute. Each file must exist, be UTF-8 text, and be at most 64 KiB. |
| `rules` | string array | `[]` | both | Hard constraint files appended to every agent prompt as MUST / MUST NOT. Same path and size rules as `vibes`. |
| `check_command` | string | `""` | both | Command run in the worktree before a safety-off merge. Empty means autodetect (`make test`, `go test ./...`, `npm test`, `pytest`, `cargo test` by the files present). |
| `idle_replan_interval` | duration > 0 | `"10m"` | both | Autopilot only: when the PO has nothing to add, the run idles this long before planning again (D-18). |
| `limits.max_review_rounds` | integer ≥ 1 | `5` | both | Review rounds per bead before the PO escalates (D-08). |
| `limits.max_pushback_rounds` | integer ≥ 0 | `2` | both | Pushback exchanges per feedback item before PO arbitration (D-08). |
| `limits.agent_timeout` | duration > 0 | `"30m"` | both | Maximum wall-clock time for one agent session. |
| `limits.max_tokens` | integer ≥ 0 | `0` (unlimited) | both | Opt-in token budget for a run. Crossing it pauses an autopilot run with a banner and finishes a brief run (D-18); enforced only when the cartridge reports usage. |
| `limits.max_cost_usd` | number ≥ 0 | `0` (unlimited) | both | Opt-in cost budget, same semantics as `max_tokens`. |
| `cartridges.<name>.model` | string | `""` | global | Model passed to the CLI, e.g. `fable` for Claude Code. |
| `cartridges.<name>.command` | string | `<name>` | global | Executable to run instead of the cartridge name (a full path or a different binary). |
| `cartridges.<name>.args` | string array | `[]` | global | Extra arguments prepended to every invocation. |
| `cartridges.<name>.env` | table of strings | `{}` | global | Extra environment variables for the CLI process. |
| `roles.<role>.cartridge` | string | `""` | both | Per-role cartridge override (`po`, `engineer`, `tester`), D-12. Parsed and validated now; it only has an effect once a second cartridge exists. |

Only `claude` is a valid cartridge name for now (D-19). Any other name, in a
file or on the command line, fails with
`cartridge "<name>" is not supported yet (only "claude" for now)`.

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
