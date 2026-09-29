# Slop Cannon

The idea behind this project is a command line interface / TUI which you can fire at a project. 

You will watch it generate a graph of task and dependencies and agents and feedback loops and revieve any prs etc that get pushed. 

you will see cool cannon animations when you watch it from the TUI. 

## Ammo breakdown

1. engineer
2. UX / QA / TESTER
3. product owner / business analyst (deeply researches the project / folder and comes up with ideas based on what's inside and things to add or fix and makes beads for the agents to work on)
4-inf → add more of 1 & 2. Ideally, the split will be three testers for each implementor to give feedback. The feedback will loop untill all testors are satisfied. an engineer should be able to push back on tester feedback if needed.

```
sc ./repo-path 

sc ./repo-path -m "here's what I want done"

sc cartridge --add claude --model fable
sc cartridge --remove claude
sc cartridge --add animus
sc cartridge --add kimi --model 3
sc ammo 12
sc vibes --add vibes.md
sc rules --add rules.md
sc safety off
```

safety = PRs or straight to main
cartridge = cli subscription which gets used
ammo = number of sub agents (minmum 3)
vibes = stylistic choices which get communicated to all agents
rules = rules which get communicated to all agents
-m / message = start work with this brief
just a repo path with no parameters = full auto pilot

defaults
- safety on
- ammo 10
- vibes/rules - none
- cartridge is always blank you need to give it a subscription for it to work

# dependencies 
- beads / dolt
- some kind of cli llm subscription

## Demo

![The Slop Cannon TUI firing at a fixture repository: a plan lands, agents are fired at two beads, a tester asks for changes, and both beads close through pull requests](docs/demo.gif)

That recording is `make demo`, and it is not a mock-up of the product — it is
the product. The demo builds a throwaway machine for one run (a copy of
`testdata/fixture-repo` with a bare origin, a stand-in for `bd` and for `gh`,
and its own `HOME`, config and state directories, all inside a temporary
directory it deletes afterwards) and then fires the real `sc run` at it. Only
the model is scripted: the mock cartridge plays `scripts/demo/scenario.json`,
so the same run happens every time, with no network, no API key and nothing
written outside the temporary directory.

- `make demo` — the TUI, as recorded above.
- `make demo DEMO_ARGS=--headless` — one line per event; needs no terminal.
- `make demo DEMO_ARGS=--check` — headless, then asserts from the run's own
  `events.jsonl` that every cannon animation really happened.
- `make demo DEMO_ARGS=--keep` — leaves the demo machine on disk and prints
  its path, so you can read the run's beads, logs and worktrees.

`scripts/demo/demo.sh --help` lists the rest. To re-record the GIF you need
[VHS](https://github.com/charmbracelet/vhs) with `ttyd` and `ffmpeg` on
`PATH`; `make demo DEMO_ARGS=--check` once to warm the build cache, then
`vhs demo.tape`, which writes `docs/demo.gif`. `demo.tape` presses its keys
while the run is still going, because the TUI hands the terminal back the
moment the run finishes — `scripts/demo/scdemo/demo_test.go` holds that
timing (and the scenario) to it.

## Install

`sc` is a single static Go binary. Three ways to get it:

**From a release** (Linux and macOS, amd64 and arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/maxh213/slop-cannon/HEAD/install.sh | sh
```

`install.sh` downloads the archive for your platform, verifies it against the
release's `checksums.txt`, and puts `sc` in the first writable of
`/usr/local/bin` and `~/.local/bin`. `--version <tag>`, `--dir <path>` and
`--repo <owner/name>` (or `SC_VERSION`, `SC_INSTALL_DIR`, `SC_REPO`) override
that; `install.sh --help` lists them all.

**From Homebrew**, once the tap is published:

```sh
brew install maxh213/tap/slop-cannon
```

**From source** — Go 1.27 or newer, and the only route that works before the
first tag is cut:

```sh
git clone https://github.com/maxh213/slop-cannon
cd slop-cannon
make install          # go install ./cmd/sc, with version metadata
# or: make build      # writes ./bin/sc instead
```

The module is named `slop-cannon`, not a URL, so `go install <url>/cmd/sc`
does **not** work — clone first.

### What `sc` needs on the machine

| Tool | Version | Why |
|---|---|---|
| `git` | 2.30+ | worktrees, branches, rebases, the merges a run makes |
| [`bd`](https://github.com/gastownhall/beads) (beads, embedded Dolt) | 1.2.2+ | the bead tracker every task, dependency and review comment lives in — `brew install beads`, or the upstream `scripts/install.sh` |
| an LLM CLI you are logged into | Claude Code 2.0+ today | the agents themselves — see [Cartridges](#cartridges) |
| `gh` | any recent | optional, but needed to open pull requests, to close beads when you merge them, and to review pull requests `sc` did not open |

`sc doctor` checks every one of them and tells you what to do about each row
it does not like. `sc` initialises the repository's `.beads/` database itself
on the first run (D-13) — you do not have to run `bd init`.

## Quickstart

```sh
# 1. one-off: point the cannon at a subscription you already pay for
sc cartridge --add claude --model fable

# 2. check the machine is ready (git, bd, gh, the cartridge CLI, the repo)
sc doctor

# 3. see what a run would be, without firing anything
sc ./my-project --dry-run

# 4. fire
sc ./my-project -m "add a --json flag to the export command"
```

Step 4 opens the TUI. `tab` moves between the five views (Graph, Agents,
Feedback, PRs, Log), `?` shows the keys of the view you are in, `p` pauses,
`s` stops, and `q` quits — on a live run it asks first. The run holds the
finished graph on screen until you press `q`.

- **A brief run** (`-m "…"`) decomposes what you asked for into beads and
  finishes when all of them have settled.
- **Autopilot** (a bare repository path, no `-m`) lets the product owner
  invent the work from the repository itself, and never stops on its own
  (D-18) — you stop it.
- **No terminal?** `sc ./my-project --headless -m "…"` renders one log line
  per event instead, and exits `0` when the run ended the way you asked,
  `1` when it failed, `3` when every cartridge is out of credits.

While it runs, from any other terminal — inside the repository, or with
`--repo PATH`:

```sh
sc watch                  # the full TUI, live: beads, agents, reviews, PRs
sc status                 # phase, bead states, PRs, live agents, recent events
sc status sc-1a2b         # one bead's whole timeline out of bd
sc logs --follow          # the orchestrator's log; --follow works on a bead id too
sc stop                   # end the run going in this repository
sc resume                 # continue the most recent run after a crash or a stop
```

Nothing a run writes goes inside your repository except the beads database
and the branches and commits the agents make: logs, prompts, transcripts,
`events.jsonl` and the agents' worktrees all live under
`$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/<run-id>/` (D-23). `sc` prints
the full list of those paths once, after the first run it fires at a
repository.

## Commands

| Command | What it does |
|---|---|
| `sc PATH` / `sc run PATH` | fire the cannon at a repository (`-m` for a brief, nothing for autopilot) |
| `sc resume` | continue the most recent run of a repository |
| `sc stop` | stop the run going in a repository, from another terminal |
| `sc watch` | open the TUI on a run that is already firing, from any terminal |
| `sc status [BEAD-ID]` | the state of a run, or one bead's timeline, reconciled with `bd` |
| `sc logs [AGENT-ID\|BEAD-ID]` | tail the run's log, an agent's log, or every agent that touched a bead |
| `sc doctor` | check git, bd, dolt, gh, the cartridges, the repo and its `.gitignore` |
| `sc cartridge` | add, remove, list or switch the LLM CLI subscriptions |
| `sc ammo N` | how many agents may run at once (minimum 3, default 10) |
| `sc safety on\|off` | pull requests, or straight to the default branch |
| `sc vibes` / `sc rules` | the style files and the hard-rule files every agent is given |
| `sc config show\|path\|edit` | the merged configuration and where each value came from |
| `sc completion` | a bash, zsh or fish completion script |
| `sc version` | version, commit and build date |

Every command, every flag, and the examples from `--help` are in
[docs/cli.md](docs/cli.md), generated from the command tree itself.
`--repo PATH`, `--json`, `--no-color` and `-v` work on every command; a run
also takes `--headless`, `--dry-run`, `--ammo`, `--safety`, `--cartridge`,
`--resume`, `--review-prs`, `--no-anim` and `--force`.

## How the feedback loop works

One product owner plans; one engineer implements each bead in its own
worktree; up to three testers review it through the ux, qa and test lenses;
the engineer may push back, and a held argument is settled by the product
owner. Nothing merges until every lens is satisfied.

```
        ┌──────────────────────────────────────────────────────────────┐
        │  PRODUCT OWNER plans (your brief, or the repo itself)        │
        │  → an epic and its children, minted atomically in bd         │
        └───────────────────────────┬──────────────────────────────────┘
                                    │ open
                                    ▼
        ┌──────────────────────────────────────────────────────────────┐
        │  SCHEDULER claims a ready bead when an engineer slot is free │
        └───────────────────────────┬──────────────────────────────────┘
                                    │ in_progress
                                    ▼
        ┌──────────────────────────────────────────────────────────────┐
        │  ENGINEER works in its own git worktree and commits          │
        └───────────────────────────┬──────────────────────────────────┘
                                    │ in_review
                                    ▼
        ┌──────────────────────────────────────────────────────────────┐
        │  TESTERS — one per lens, in parallel                         │
        │     ux  ──┐                                                  │
        │     qa  ──┼──►  a verdict comment each: approve, or items    │
        │     test──┘                                                  │
        └────────┬────────────────────────────────────┬────────────────┘
                 │ all approved                       │ changes requested
                 │                                    ▼
                 │                    ┌───────────────────────────────┐
                 │                    │ items consolidated, one per   │
                 │                    │ place, handed back to the     │
                 │                    │ engineer                      │
                 │                    └───────────────┬───────────────┘
                 │                                    ▼
                 │                    ┌───────────────────────────────┐
                 │                    │ ENGINEER fixes — or pushes    │
                 │                    │ back. A disputed item goes to │
                 │                    │ the tester that raised it;    │
                 │                    │ past max_pushback_rounds the  │
                 │                    │ PO arbitrates, and its ruling │
                 │                    │ is binding.                   │
                 │                    └───────────────┬───────────────┘
                 │                                    │ next round
                 │                    only the lenses that did not
                 │                    approve re-review — unless the
                 │                    diff moved more than
                 │                    re_review_threshold, and then
                 │                    everybody does (D-09)
                 │                                    │
                 │       ┌────────────────────────────┘
                 │       │  at max_review_rounds the PO rules on the
                 │       ▼  whole bead instead → escalated
                 │ approved
                 ▼
        ┌──────────────────────────────────────────────────────────────┐
        │  DELIVERY                                                    │
        │   safety on   push sc/<bead>-<slug>, open a PR, wait on a    │
        │               gh:pr gate — you merge, the bead closes        │
        │   safety off  rebase, run the check command, fast-forward    │
        │  conflict → back to the engineer  ·  check failed → a qa     │
        │  feedback item  ·  no remote / no gh → needs_human           │
        └───────────────────────────┬──────────────────────────────────┘
                                    ▼  closed
```

Meanwhile the run also reviews pull requests it did **not** open (D-11): it
adopts them, fires testers at them, posts one consolidated `gh pr review`,
and never merges them. `--review-prs=false` turns that off.

The workflow state of a bead **is** its `bd` status (`open`, `in_progress`,
`in_review`, `changes_requested`, `approved`, `delivering`, `escalated`,
`needs_human`, `closed`), so nothing that matters for a resume lives only in
memory: kill `sc` mid-run and `sc resume` picks the graph back up from `bd`
and from git. The whole picture — package by package, one bead's life step by
step, the state machine and the resume model — is
[docs/architecture.md](docs/architecture.md); how a run uses `bd` is
[docs/beads-conventions.md](docs/beads-conventions.md).

### Ammo, in practice

The [Ammo breakdown](#ammo-breakdown) at the top is the original sketch, and
it is what got built — with one refinement. `ammo` is a concurrency **cap**,
not a fixed roster: the scheduler keeps one
slot for the product owner when it needs one, lets
`max(1, floor((ammo - 1) / 4))` engineers work at once, and gives every
remaining slot to testers, so each engineer can have its three reviewers.

| ammo | PO | engineers in flight | testers | testers per bead |
| ---: | ---: | ---: | ---: | ---: |
| 3 | 1 | 1 | 1 | 1 |
| 4 | 1 | 1 | 2 | 2 |
| 10 (default) | 1 | 2 | 7 | 3 |
| 13 | 1 | 3 | 9 | 3 |
| 17 | 1 | 4 | 12 | 3 |

`sc ammo N` prints the allocation the number implies before it saves it. The
full table, and the order demands are served in when slots free up, are in
[docs/architecture.md § Ammo](docs/architecture.md#ammo-d-05). One thing in
the sketch's example block has not arrived: `animus` and `kimi` are still
refusals today — see [Cartridges](#cartridges).

## Config

Two TOML files merge with the built-in defaults, highest precedence first:

| Layer | Where |
|---|---|
| CLI flags | `--ammo`, `--safety`, `--cartridge` on a run |
| Repo config | `<repo>/.slop-cannon/config.toml` |
| Global config | `~/.config/slop-cannon/config.toml` (`$XDG_CONFIG_HOME`) |
| Defaults | built in |

`sc config show` prints the merged result with the layer every value came
from; `sc config path` prints both paths; `sc config edit` opens one in
`$EDITOR`. The config subcommands write the repo file when you are inside a
git repository and the global file otherwise, and `--global` always means the
global file. Cartridges are subscriptions and live only in the global file.

```toml
# <repo>/.slop-cannon/config.toml
schema_version = 1
ammo = 12
safety = false
rules = ["../docs/rules.md"]
vibes = ["../vibes.md"]
check_command = "make check"
exclude = ["certs", "*.key"]

[limits]
max_review_rounds = 3
max_tokens = 2000000
```

The keys worth knowing first: `ammo`, `safety`, `rules` and `vibes` (files
appended to every prompt as MUST and as preferences), `check_command` (run in
the worktree before a safety-off merge; autodetected when blank),
`exclude` (path globs no agent is ever shown, on top of the always-on `.env`,
`*.pem` and `id_*` patterns), and `limits.max_tokens` / `limits.max_cost_usd`
(opt-in budgets that stop new work and pause an autopilot run).

Every key, its type, its default and its exact meaning — plus secrets
hygiene, the state-directory layout and schema migration — is in
[docs/config.md](docs/config.md).

## Safety

Safety is on by default, and on means **the cannon never touches your default
branch**.

| | safety on (default) | safety off |
|---|---|---|
| Where work lands | a branch `sc/<bead-id>-<slug>`, pushed | rebased onto the default branch and fast-forwarded |
| Who merges | you, in GitHub | `sc`, after the check command passes |
| Bead closes when | you merge the pull request and the gate poller sees it | the merge lands |
| Needs `gh` | yes | no (a remote is optional) |

`sc safety off` writes the setting; `--safety off` overrides it for one run;
`S` in the TUI flips it mid-run after asking. On top of that switch:

- **Agents get a read-only `bd`.** Every agent subprocess runs with a shim on
  `PATH` that refuses any write, so an agent cannot close its own bead or
  rewrite the graph (D-21). Only the orchestrator writes.
- **Agents are asked, never trusted.** Every reply must satisfy a JSON
  schema ([docs/schemas/](docs/schemas/README.md)); a malformed one is
  rejected and retried rather than acted on.
- **Secrets are withheld and masked.** Files matching `exclude` or the
  always-on patterns are left out of the tree an agent sees and their diffs
  are replaced by a placeholder; a credential that reaches text anyway is
  rewritten to `[REDACTED]` once, where the run publishes, so every sink —
  terminal, `sc.log`, `events.jsonl`, the transcripts, later prompts, bead
  comments and the pull request body — carries the masked text.
- **One run per repository.** A run takes a lock; a second `sc` refuses and
  names the run holding it. `--force` is there for a lock whose process is
  genuinely gone.
- **Nothing is hidden.** Everything an agent was told is on disk under the
  run directory, and `sc logs` reads it back.

## Cartridges

A cartridge is an LLM CLI adapter plus its model: the CLI you already log
into, driven as a subprocess. `sc` makes no API calls of its own and wants no
API key.

**Claude Code and Cursor Agent ship today** (D-29): Claude verified against
`2.1.258 (Claude Code)` and refusing anything below 2.0.0, Cursor against
`cursor-agent` `2026.09.28-64d2043`:

```sh
sc cartridge --add claude --model fable
sc cartridge --add cursor --model auto
sc cartridge --list
sc cartridge --use claude
```

Roles can use different cartridges (`[roles.engineer] cartridge = "cursor"`),
for example engineers and testers on Cursor while the product owner stays on
Claude; see [docs/config.md](docs/config.md).

Kimi, Animus and a custom-command adapter are planned; until then
`sc cartridge --add kimi` fails with
`cartridge "kimi" is not supported yet (only "claude", "cursor" for now)`. The
interface, the registry and the conformance suite are adapter-agnostic, so
each new one is a package and a registry line: the whole recipe — the
contract, the error-kind and tool-policy mapping tables to fill in, the
fixtures, the conformance run and a worked example — is
[docs/cartridges.md § Adding a new cartridge](docs/cartridges.md#adding-a-new-cartridge).

There is also a `mock` cartridge that plays a scripted scenario. It is how
the tests and `make demo` run a whole cannon with no model at all, and it is
deliberately not registered in the shipped binary.

## Documentation

| Page | What it covers |
|---|---|
| [docs/](docs/README.md) | the index: every page below, grouped by using it, how it works, and working on it |
| [docs/cli.md](docs/cli.md) | every command and flag, generated from the command tree |
| [docs/config.md](docs/config.md) | both TOML files, every key with its default, secrets hygiene |
| [docs/tui.md](docs/tui.md) | the five views, the keys, the run controls, and what `--headless` does instead |
| [docs/troubleshooting.md](docs/troubleshooting.md) | every error, warning and `sc doctor` row `sc` can print, keyed by the words you saw — start here when a run refuses or stops |
| [docs/performance.md](docs/performance.md) | the three speed budgets (graph layout, scheduler tick, event bus), where the benchmarks are and what they currently measure |
| [docs/architecture.md](docs/architecture.md) | what each package owns, one bead's life, the state machine, the resume model |
| [docs/cartridges.md](docs/cartridges.md) | the adapter interface, the Claude Code adapter, and how to add your own |
| [docs/beads-conventions.md](docs/beads-conventions.md) | how a run uses `bd`: statuses, `sc_` metadata, labels, structured comments, gates |
| [docs/prompts.md](docs/prompts.md) | what each agent is actually told, the headless line format, and the exit codes |
| [docs/releasing.md](docs/releasing.md) | the versioning policy and how a release is cut |
| [CHANGELOG.md](CHANGELOG.md) | the release notes: what changed in each version, newest first |

## FAQ

**Do I need an API key?** No. `sc` shells out to the LLM CLI you are already
logged into, so it runs on your subscription. It never talks to a model API
itself.

**Will it push to my main branch?** Not with safety on, which is the default:
work goes to an `sc/…` branch and a pull request, and a human merges it. You
have to say `sc safety off` for anything else.

**What does it write inside my repository?** The beads database in `.beads/`
(created on the first run), an optional `.slop-cannon/config.toml` if you set
repo config, and the branches and commits the agents make. Logs, prompts,
transcripts, events and the agents' worktrees live outside the repository,
under `$XDG_STATE_HOME/slop-cannon/`.

**It crashed / I killed it. Now what?** `sc resume` continues the most recent
run of that repository: the bead states are in `bd` and the work is in git,
so the run picks up the graph, reconciles the worktrees and branches it left
behind, and carries on. `sc status` says what the last run got to first.

**Can I watch or steer a run from another terminal?** `sc status`,
`sc status <bead-id>` and `sc logs --follow` are read-only and work on a
live run, a finished one and a crashed one. `sc stop` ends one.

**How do I stop it spending money?** `limits.max_tokens` and
`limits.max_cost_usd` stop new work being started once they are crossed; an
autopilot run then pauses and waits for you, and `b` in the TUI raises the
ceiling. They are floors on when the run stops starting things, not ceilings
on the total — the reviews already owed still fire.

**Does it need GitHub?** Only for the parts that are about pull requests:
opening them (safety on), closing beads when you merge them, and reviewing
pull requests `sc` did not open. Safety off against a repository with no
remote at all works.

**Why beads and not GitHub issues?** The graph is the product: dependencies,
per-bead review history, structured comments and custom statuses, all local,
all fast enough to poll every couple of seconds, and all still there after a
crash. `bd show <id>` reads as the history of a bead.

**Something printed a message I do not understand.**
[docs/troubleshooting.md](docs/troubleshooting.md) is keyed by the words you
saw.

## Development

Go 1.27+ is required. All day-to-day work goes through the Makefile:

| Target | What it does |
|---|---|
| `make build` | Builds `bin/sc` with version, commit, and build date injected via `-ldflags` |
| `make test` | `go test -race ./...` |
| `make lint` | Runs `golangci-lint` (auto-installed into `bin/` at the pinned version; config in `.golangci.yml`) |
| `make fmt` | Fails if any Go file is not `gofmt -s` clean |
| `make check` | `fmt` + `lint` + `test`; the gate CI runs and every task must pass |
| `make install` | `go install ./cmd/sc` with the same version metadata |
| `make integration` | Runs tests tagged `integration` (they need real `bd`, `git`, `gh`) |
| `make demo` | Runs the deterministic demo above (`DEMO_ARGS=--check` asserts it, `--headless` needs no terminal) |
| `make cartridge-conformance` | Runs the cartridge conformance suite (`CARTRIDGE=mock` by default; a real CLI needs a login, so it never runs in CI) |
| `make clean` | Removes `bin/`, `dist/`, and coverage output |
| `make tools` | Installs the pinned developer tools into `bin/` |
| `make update-golden` | Regenerates `testdata/**/*.golden` and `docs/cli.md` from the current output (`UPDATE_GOLDEN=1 go test ./...`) |
| `make update-golden-tui` | Regenerates just the TUI frames in `testdata/tui/` (`UPDATE_GOLDEN=1 go test ./internal/tui/...`) |
| `make check-golden` | Fails if any golden file or `docs/cli.md` is stale; regenerates in place, so run it on a clean tree |
| `make docs-cli` | Regenerates `docs/cli.md` from the `sc` command tree |
| `make help` | Lists the targets |

CI (`.github/workflows/ci.yml`) runs `make check` on Ubuntu and macOS for every push and pull request, and runs `make integration` in a separate job that is skipped when `bd` is not installed.

Golden files (including the TUI frames in `testdata/tui/`) are explained in [docs/goldens.md](docs/goldens.md): how they stay deterministic, how to regenerate them, and how to read a diff. Benchmarks and the three speed budgets they defend are in [docs/performance.md](docs/performance.md).

When `sc` (or the demo) says something you did not expect, [docs/troubleshooting.md](docs/troubleshooting.md) is keyed by the message: what happened, and what to do about it.

## License

MIT — see [LICENSE](LICENSE).
