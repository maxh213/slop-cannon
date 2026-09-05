# Slop Cannon documentation

Everything `sc` ships as prose. The [project README](../README.md) is the
introduction; these are the reference pages behind it.

## Using it

| Page | What it covers |
|---|---|
| [cli.md](cli.md) | Every command, flag and example of the `sc` binary, generated from the command tree itself (`make docs-cli`). |
| [config.md](config.md) | The two TOML files, how they merge, every key with its default, secrets hygiene, the ammo allocation and schema migration. |
| [tui.md](tui.md) | The run TUI: layout, keys, the five views, the run controls, colour, and what `--headless` does instead. |
| [tui-feedback.md](tui-feedback.md) | The Feedback view in detail: the review history of a run, bead by bead and lens by lens. |
| [prompts.md](prompts.md) | What each agent is actually told — the prompt layers, rules and vibes — plus the per-agent debug copies in the run directory, the headless line format and glyph table, and the exit codes CI reads. |
| [troubleshooting.md](troubleshooting.md) | Every error, warning and `sc doctor` row `sc` can print, keyed by the words you saw, with what happened and what to do about it. Start here when a run refuses or stops. |

## How it works

| Page | What it covers |
|---|---|
| [architecture.md](architecture.md) | What each package owns, one bead's life from plan to merge, how ammo is divided, the bead state machine, and what a resumed run does with the last one's wreckage. |
| [cartridges.md](cartridges.md) | The LLM CLI adapter interface, the Claude Code adapter that ships today, tool policies, error-kind mapping, and how to add a cartridge of your own. |
| [beads-conventions.md](beads-conventions.md) | How a run uses `bd`: statuses, `sc_` metadata, labels, structured bead comments and gates — so `bd show <id>` reads as the history of a bead. |
| [schemas/](schemas/README.md) | The JSON Schema every agent reply must satisfy (`plan`, `impl_report`, `verdict`, `response`, `arbitration`, `escalation`), with valid and invalid examples. |

## Working on it

| Page | What it covers |
|---|---|
| [performance.md](performance.md) | The three speed budgets (graph layout, scheduler tick, event bus), where the benchmarks live, how to run them, and what the current numbers are. |
| [goldens.md](goldens.md) | Golden files: what is pinned, how `UPDATE_GOLDEN` works, and how to regenerate the TUI frames and prompt fixtures without breaking `make check-golden`. |
