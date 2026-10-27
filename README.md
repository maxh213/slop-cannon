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
sc dash                   # read-only browser dashboard at 127.0.0.1:8790
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
| `sc dash [PATH]` | serve a read-only browser dashboard for the latest or a named run |
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

### Read-only browser dashboard

`sc dash [PATH]` serves the most recent run at `http://127.0.0.1:8790/`;
`--run ID` selects another. It shows bead flow, live and recent agents,
review rounds, deliveries, notices and lifecycle activity for a live, quiet,
finished or crashed run, and keeps following if that run is resumed. It is
strictly observational: no run controls, lock ownership or state writes, and
no prompts, diffs, agent output, review transcripts or raw notice prose.
Notices expose only their time, kind, source and a fixed category.

`--host HOST` and `--port PORT` change the listener (port `0` chooses a free
port), and `--base-path /sc` puts the page and its two GET API endpoints
under that prefix. The snapshot API is bounded and redacted; `/health`
reports the dashboard observer's health, not the run's liveness. Run status,
lock state and inactivity (`quiet`) are separate snapshot fields, so a
finished or crashed run does not make a healthy observer look unhealthy.

#### Host resources

On Linux the dashboard samples the host every two seconds: aggregate and
per-core CPU usage, memory, swap and root-disk usage. CPU percentages need two
samples with matching topology; per-core results are bounded with omitted
counts. Memory, swap and disk are used/total byte pairs, and zero-swap systems
omit the swap row. Failed samples retain the last complete sample, while
unsupported platforms report no host snapshot; neither changes observer
`/health`.

#### Dandelion routed-model labels

For agents using the `dandelion` cartridge, the dashboard can show the model
and account Dandelion actually selected, for example
`engineer · claude-opus-5-5 · dandelion/claude · effort max`. This is read-only
evidence from the ordered structured `sc.log`, correlated first by a launch's
timestamp interval and then by exact agent and supervisor-attempt history
within that generation. It is not inferred from the global quota route or a
latest-route guess.

Each launch freezes its spawn time and the current and previous direct event
starts. Generation selection uses **`(previous event start, current event
start]`**: the previous boundary is excluded and the current one included.
Only direct `agent fired` and `dandelion routed a firing` records in the
selected generation and exact attempt supply route metadata. Arrival skew,
where the event start is ahead of `sc.log`, is `unknown`; it never selects the
previous generation.

This interval identity survives event-log rotation. With **five direct starts
but only four retained event starts**, current-run replay recovers generation
five, while an old last-seen card keeps its own valid frozen interval and
model four. Prior history is preserved, and an old card never inherits a newer
generation's model. The observer backfills the launch log from the beginning
and then follows complete appended lines with bounded, metadata-only parsing.
HTTP requests serve the cached snapshot and never read the log.

Within one exact attempt, a multi-session attempt shows its latest selected
structured route and may show the route count in a tooltip. Duplicate attempts
within one generation remain `unknown`. A new attempt never inherits the
previous attempt's model, and the browser shows routed metadata only for the
matching incarnation and `route_attempt`. This restores valid routes
previously suppressed after resumes without changing adapters or the
requested `domain.Agent.Model`. When no exact route is available, the subtitle
is explicit:

- `dandelion · routing…` — the exact live fired attempt has no route yet.
- `dandelion · no route` — the exact fired attempt has no route and a typed
  `rate_limited` failure. Error prose or another attempt's failure is not
  enough.
- `dandelion · route unknown` — history is missing, ambiguous or damaged.

Dandelion's configured `model` remains the requested `high` or `standard`
mode; it is not displayed as the actual routed model. Route selection also
does not prove that the delegated process started or succeeded. Non-Dandelion
agent rendering is unchanged, and these labels do not require the optional
quota panel below.

#### Optional Dandelion quota

Dandelion quota collection is off by default. `sc dash --dandelion` enables it
and adds the quota panel; `--dandelion-refresh DURATION` takes a Go duration,
defaults to `10m` and rejects values below `1m`. For example:

```sh
sc dash --dandelion --dandelion-refresh 15m
```

The first probe starts immediately and later probes run sequentially, never
concurrently. Each runs `dandelion --json --max-age <refresh-seconds>` with a
fixed five-minute timeout and bounded output. Dandelion can reuse its cache
while it is fresh within that age; otherwise it performs real external
provider probes. `sc` keeps its own last-good result in memory only and writes
no Dandelion quota cache to disk.

Last attempt and last success are separate: a failed refresh does not make
retained data look newly successful. The panel exposes probing and stale state
plus only the fixed collector error codes `not_installed`, `timed_out`,
`probe_failed` and `invalid_snapshot`; raw collector subprocess stderr/errors
are never serialized. Unavailable/error provider `reason` values are separate,
bounded, explicitly allowlisted Dandelion diagnostics. HTTP requests only read
the cached snapshot and never start probes; quota failures never change
observer `/health`.

The panel shows both standard and high routes, probe activity and
attempt/success ages, and each provider's name, plan, status, reason and
routing-off state. Every window shows its label, a clamped percentage gauge,
and reset and projected-full countdowns. Gauge tones are calm below 50%, warm
from 50% to below 80%, hot from 80% to below 95%, and crit at 95% or above.
An optional provider `balance` contains only `amount` and `currency`, is
projected independently of windows, and never creates a synthetic gauge,
reset, percentage or binding score. Literal `$` is normalized to `USD` at the
API boundary; the final live Kilo balance was `31.232105 USD`, rendered as
`balance $31.23` exactly once with zero meter elements and zero percentage
strings. Kilo remains visible in `usage only` with zero windows because it has
a valid balance.

The default-on `usage only` toggle persists in `localStorage`; zero-percent
windows still count as usage. Missing, malformed and zero timestamps are
blanked with their DOM metadata instead of becoming year-one dates or bogus
countdowns. The whole panel is hidden when `--dandelion` is not set.

The dashboard service keeps `ProtectHome=read-only` and grants only these
Kilo runtime startup/auth write exceptions, not a general home write or a
Dandelion quota cache:

```ini
ReadWritePaths=~/.local/share/kilo
ReadWritePaths=~/.cache/kilo
ReadWritePaths=~/.local/state/kilo
```

The default listener is loopback-only. Every HTTP `Host` header is validated
against `localhost`, `127.0.0.1`, `::1` and exact hostnames or IPs added with
the repeatable `--allowed-host HOSTNAME`; wildcards, schemes, ports and paths
are not accepted in that flag. This is DNS-rebinding protection, not
authentication. To share a dashboard, keep it on loopback behind an
authenticating reverse proxy where possible, preserve the public `Host`,
allow that hostname, and set `--base-path` to the prefix the proxy forwards.
Do not expose the unauthenticated server directly to a public network; a
non-loopback `--host` needs the same authenticated proxy boundary.

## How the feedback loop works

By default, one product owner plans; one engineer implements each bead in its
own worktree; up to three testers review it through the ux, qa and test
lenses; the engineer may push back, and a held argument is settled by the
product owner. Nothing merges until every lens is satisfied. The default-one
planning path is unchanged: one producer, the ordinary eight-bead cap, and no
semantic judge or candidate namespacing.

Setting `po_planners = N` above 1 instead runs N independent planning
producers as one combined round. The round waits for every producer, then
reconciles their candidates with at most one read-only semantic judge before
applying one graph under the run's epic. Its combined cap is
`N × max_beads_per_plan` — **16** with two planners and the default eight —
and an oversized graph is rejected, never trimmed. A combined round reserves
N weighted scheduler slots atomically within `ammo`, never partial capacity;
planning and arbitration caps are independent. See
[parallel PO planning](docs/architecture.md#parallel-po-planning-d-41),
[Ammo, in practice](#ammo-in-practice), and the
[`po_planners` configuration](docs/config.md#keys).

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
        │               gh:pr gate — you merge (or, with auto_merge,   │
        │               the run merges its own green PR), it closes    │
        │   safety off  rebase, run the check command, fast-forward    │
        │  conflict → back to the engineer  ·  check failed → a qa     │
        │  feedback item  ·  no remote / no gh → needs_human           │
        └───────────────────────────┬──────────────────────────────────┘
                                    ▼  closed
```

Meanwhile the run also reviews pull requests it did **not** open (D-11): it
adopts them, fires testers at them, posts one consolidated `gh pr review`,
and never merges them. A pull request that was already open when the run
started is left alone. `--review-prs=false` turns that off.

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
not a fixed roster: the ordinary allocation keeps one product-owner slot when
one is needed, lets `max(1, floor((ammo - 1) / 4))` engineers work at once,
and gives every remaining slot to testers, so each engineer can have its
three reviewers.

| ammo | PO | engineers in flight | testers | testers per bead |
| ---: | ---: | ---: | ---: | ---: |
| 3 | 1 | 1 | 1 | 1 |
| 4 | 1 | 1 | 2 | 2 |
| 10 (default) | 1 | 2 | 7 | 3 |
| 13 | 1 | 3 | 9 | 3 |
| 17 | 1 | 4 | 12 | 3 |

The table is the ordinary D-05 allocation, not permission to exceed `ammo`.
Default-one planning leaves `Demand.Slots` at zero, meaning one. A parallel
round reserves all `po_planners` slots at once and waits when they are not
free; a round larger than `ammo` never reserves partially. Planning and
arbitration caps are independent. A planning cap above `ammo` is served in
ammo-sized waves, but each weighted round must still fit whole.

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
`*.pem` and `id_*` patterns), `limits.max_tokens` / `limits.max_cost_usd`
(opt-in budgets that stop new work and pause an autopilot run), and
`signals_command` (autopilot only: a read-only command — a log query, an
error tracker's CLI — that sc runs every `signals_interval` and whose output
the product owner is given as untrusted production evidence, so real errors
get fixed first).

Every key, its type, its default and its exact meaning — plus secrets
hygiene, the state-directory layout and schema migration — is in
[docs/config.md](docs/config.md).

## Safety

Safety is on by default, and on means **the cannon never touches your default
branch**.

| | safety on (default) | safety off |
|---|---|---|
| Where work lands | a branch `sc/<bead-id>-<slug>`, pushed | rebased onto the default branch and fast-forwarded |
| Who merges | you, in GitHub — or the run, for its own green pull requests, with `auto_merge` | `sc`, after the check command passes |
| Bead closes when | the pull request merges and the gate poller sees it | the merge lands |
| Needs `gh` | yes | no (a remote is optional) |

**Auto-merge** (`auto_merge = true` or `--auto-merge`, safety on only, off by
default) lets the run merge the pull requests it opened itself once they are
green, for a repository where you would rather review the cannon's output
after it lands than click merge on every pull request (D-31). It is built
for a default branch that deploys to production, so it errs on waiting:

- **Only its own.** A pull request is merged only when every clause holds:
  its head is exactly the bead's `sc/` branch, its base is the default
  branch, it has the `slop-cannon` label and a body marker naming this run
  and this bead, it is the number the bead recorded, it was created after
  the run first started, and its author is your `gh` login. Nothing that
  existed before the run started is ever merged, reviewed, adopted or pushed
  to — with auto-merge off too: a pull request already on a bead's branch
  that fails the check is handed to you instead of reused.
- **Only what was reviewed.** The head must be the commit the run delivered
  and its testers approved, or that commit plus the merges of the default
  branch GitHub made because the run asked it to (`gh pr update-branch`). A
  commit anybody else pushes to the branch, or an update the run did not ask
  for, hands the bead to you.
- **Only when green.** At least one check, every check finished and passed,
  every name in `auto_merge_checks` among them (at least one is required), and GitHub reporting the
  pull request `CLEAN`. A required review (`BLOCKED`) waits for you; nothing
  is bypassed.
- **Only exactly.** `gh pr merge <n> --squash --match-head-commit <sha>`, the
  head the same poll judged — never `--admin`, `--auto` or
  `--delete-branch` — and at most one merge per `auto_merge_spacing`
  (default 10 minutes, kept across a resume), so each deploy finishes before
  the next merge starts another.
- **Never by force.** A pull request behind the default branch is updated
  by GitHub (`gh pr update-branch`); a failed check or a conflict goes back
  to the bead's engineer as a change request, and the fix is pushed to the
  same pull request as a plain fast-forward. `sc status` shows what each
  pull request is waiting for.

`conventional_pr_titles = true` titles the pull requests `fix: …`, `feat: …`
or `chore: …` from the bead's type (never an agent's own type or `!`), for repositories whose CI checks
Conventional Commit titles; a squash merge makes the title the commit
subject.

`sc safety off` writes the setting; `--safety off` overrides it for one run;
`S` in the TUI flips it mid-run after asking. On top of that switch:

- **Agents get a read-only `bd`.** Every agent subprocess runs with a shim on
  `PATH` that refuses any write, so an agent cannot close its own bead or
  rewrite the graph (D-21). Only the orchestrator writes. Within one `sc`
  process, a double-checked mutex serializes the once-per-binary verification
  and failed probes are retried rather than cached. Independent `sc`
  processes are not coordinated and still rely on bd's own locking.
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

**Claude Code, Cursor Agent, Agy, Codex, Kimi, Grok Build, Junie, Hermes
Agent and Kilo ship as direct adapters**, plus the dandelion router (D-29,
D-30, D-37, D-40). Claude is verified against `2.1.258 (Claude Code)` and
refuses anything below 2.0.0; Cursor is verified against `cursor-agent`
`2026.09.28-64d2043`; the Kilo adapter targets Kilo CLI 7.5.6:

```sh
sc cartridge --add claude --model fable
sc cartridge --add cursor --model auto
sc cartridge --add dandelion --model high
sc cartridge --add kimi
sc cartridge --add kilo --model deepseek/deepseek-v4.1-flash-1017
sc cartridge --list
sc cartridge --use claude
```

Roles can use different cartridges (`[roles.engineer] cartridge = "cursor"`),
for example engineers and testers on Cursor while the product owner stays on
Claude; see [docs/config.md](docs/config.md).

`dandelion` is a quota router rather than a CLI of its own (D-30): before
every firing it asks `dandelion route --high` (model `high`) or
`dandelion route` (model `standard`) which account still has quota, and fires
the provider's registered driver with the model the router names. A per-role
`standard` or `high` selects that firing's route mode, with case and
whitespace tolerated; empty, other and legacy values fall back to the
configured Dandelion mode. The real routed model still replaces `spec.Model`
for the delegated CLI. The expansion enables `agy`, `kimi`, `grok`, `codex`,
`codex-animus`, `junie`, `hermes` and `kilo` through seven direct CLI
families; Codex Animus shares Codex with an isolated `CODEX_HOME`. Kilo is a
standard-route balance fallback only and is never in Dandelion's high chain.

Role models are effective configuration, not `ConfigSnapshot` fields. On an
ordinary resume the active cartridge and its default model/mode stay frozen,
but current repo role-model changes take effect on the next resume.

The Animus setup is now applied and deployed with all three role models
sourced from the repo: engineers and testers use `standard`, and the PO uses
`high`. The PO did not fire during the bounded 180-second wait, so `high` is
verified from effective config only. Final Kilo completion evidence has
orchestrator PID 1637159 with a live lock and the same standard-work/high-PO
split, with dashboard and tunnel healthy. Existing monitor
`01M5R7ZMS30P8YMJ3W16PXNVW3` remains read-only at minutes 7/27/47 of each
20-minute cycle, with no automatic state changes and no indefinite monitoring
claim.

Restricted routing is native-only: `read_only` uses Claude accounts;
`read_exec` uses Claude, Codex/Codex Animus, Kimi, Grok, Hermes or Kilo;
`read_write` can use every routable installed provider. Agy and Grok
`read_only` are deliberately disabled after failed live enforcement canaries.
Kilo also refuses `read_only`, because its Bash permission is not a read-only
execution boundary.

Every Kilo firing sets `KILO_NO_DAEMON=1`, disables project configuration and
automatic updates, and supplies a generated in-memory agent configuration;
the complete combined prompt travels on stdin. At the CLI argument boundary,
the pinned Dandelion model `deepseek/deepseek-v4.1-flash-1017` is normalized
to `kilo/deepseek/deepseek-v4.1-flash-1017`; direct custom IDs and the
unqualified route model in `RunSpec.Model` and launch logs are not rewritten.

For delegated Kilo firings, Dandelion supplies
`SC_KILO_MIN_BALANCE_USD=10`, which is Dandelion-only and is removed from the
Kilo child environment. A cached Kilo route is only a hint: sc obtains a fresh
route and checks `kilo profile --json` immediately before every Kilo launch.
Profile failure or a balance at or below the floor fails closed, excludes Kilo
and reroutes without launching it. The in-session allowance is
`balance - floor`, enforced best effort from reported cost: a final request
can overshoot before its cost arrives, and concurrent sessions can spend the
same balance. Prelaunch freshness and that allowance cannot guarantee other
clients or later billed steps will never take the account below $10.

Real direct checks exercised balance-gated echo, structured output, native
file creation, large-prompt stdin, and timeout/kill cleanup without a daemon.
Delegated checks exercised a fresh profile before every launch, normalized
argv with an unqualified route log, `read_exec`/`read_write` policy behavior,
cost below the fresh allowance, and fail-closed reroute after injected profile
failure or an at-floor balance. `read_only` remains Claude-only and the high
chain is unchanged.

Large prompts travel by stdin or private files removed on every exit path,
cancellation and kill clean up process groups and descendants, and
account-specific homes do not leak between firings. See
[docs/cartridges.md](docs/cartridges.md) for each adapter's exact policy and
prompt handling.

Codex and Codex Animus have real `read_exec` and `read_write` drivers. The
personal Codex account being logged out and the Codex Animus app-server probe
error are current account availability issues, not missing adapters. `codex-animus`
is not in Dandelion's high chain.

Animus and a custom-command adapter remain planned. The interface, registry
and conformance suite are adapter-agnostic, so each new one is a package and a
registry line: the whole recipe — the contract, the error-kind and tool-policy
mapping tables to fill in, the fixtures, the conformance run and a worked
example — is
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
have to say `sc safety off` for anything else. With `auto_merge` on, GitHub
squash-merges the run's own green pull requests at the run's request — still
no push from `sc` to the default branch, and never a pull request the run did
not open.

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
