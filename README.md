# Slop Cannon

The idea behind this project is a command line interface / TUI which you can fire at a project. 

You will watch it generate a graph of task and dependencies and agents and feedback loops and revieve any prs etc that get pushed. 

you will see cool cannon animations when you watch it from the TUI. 

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
| `make clean` | Removes `bin/`, `dist/`, and coverage output |
| `make tools` | Installs the pinned developer tools into `bin/` |
| `make update-golden` | Regenerates `testdata/**/*.golden` and `docs/cli.md` from the current output (`UPDATE_GOLDEN=1 go test ./...`) |
| `make update-golden-tui` | Regenerates just the TUI frames in `testdata/tui/` (`UPDATE_GOLDEN=1 go test ./internal/tui/...`) |
| `make check-golden` | Fails if any golden file or `docs/cli.md` is stale; regenerates in place, so run it on a clean tree |
| `make docs-cli` | Regenerates `docs/cli.md` from the `sc` command tree |
| `make help` | Lists the targets |

CI (`.github/workflows/ci.yml`) runs `make check` on Ubuntu and macOS for every push and pull request, and runs `make integration` in a separate job that is skipped when `bd` is not installed.

Golden files (including the TUI frames in `testdata/tui/`) are explained in [docs/goldens.md](docs/goldens.md): how they stay deterministic, how to regenerate them, and how to read a diff.

When `sc` (or the demo) says something you did not expect, [docs/troubleshooting.md](docs/troubleshooting.md) is keyed by the message: what happened, and what to do about it.
