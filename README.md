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
| `make clean` | Removes `bin/`, `dist/`, and coverage output |
| `make tools` | Installs the pinned developer tools into `bin/` |
| `make update-golden` | Regenerates `testdata/**/*.golden` and `docs/cli.md` from the current output (`UPDATE_GOLDEN=1 go test ./...`) |
| `make docs-cli` | Regenerates `docs/cli.md` from the `sc` command tree |
| `make help` | Lists the targets |

CI (`.github/workflows/ci.yml`) runs `make check` on Ubuntu and macOS for every push and pull request, and runs `make integration` in a separate job that is skipped when `bd` is not installed.
