# Golden files

A golden file is the exact output a test expects, checked into `testdata/`
next to the code that produces it. When rendering changes on purpose you
regenerate the files and commit the diff; when it changes by accident the
diff is the bug report.

Everything below is driven by one environment variable: **`UPDATE_GOLDEN`**.
When it is set to anything non-empty, a golden helper writes what it was
given instead of comparing; when it is unset, it compares and fails. Never
set it in CI.

## Updating

| Command | What it rewrites |
|---|---|
| `make update-golden` | Every golden in the repo, plus `docs/cli.md` (`UPDATE_GOLDEN=1 go test ./...`) |
| `make update-golden-tui` | Only the TUI frames in `testdata/tui/` (`UPDATE_GOLDEN=1 go test ./internal/tui/...`) |
| `make docs-cli` | `docs/cli.md` alone, from the `sc` command tree |
| `make check-golden` | Nothing: it regenerates in place and fails if anything changed |

Always read the diff before committing it. A regenerated golden is a change
to what a user sees, so it belongs in the same commit as the code that
changed it, and an unexpected hunk in a file you did not touch means
something leaked into the output (a wall clock, a map iteration, a colour
profile, a path from your machine).

`make check-golden` writes to the working tree, so run it on a clean tree —
it is the explicit "the goldens are current" gate. `make check` (what CI
runs) catches the same thing implicitly: a stale golden fails its test.

## TUI frames (`testdata/tui/`)

Two kinds of file live here, both plain UTF-8 text with no ANSI colour:

- **Per-view frames** — `graph-*`, `agents-*`, `feedback-*`, `prs-*`,
  `anim-*`, `headless*`. Each is one component's pane, owned by the task
  that wrote the view.
- **Composed frames** — `frame-*`, from `internal/tui/golden_test.go`. Each
  is a whole terminal: header, tab bar, rule, pane and footer, plus the help
  and stop-confirmation overlays, exactly as the user sees them.

Every frame is deterministic by construction, and a test enforces each part
of that:

- **Fixed clock** — frames are rendered from the scripted demo run
  (`tui.DemoEvents`), whose timestamps start at `tui.DemoStart`
  (2026-01-01T12:00:00Z) and advance by `tui.DemoStep`. Nothing in a frame
  may come from `time.Now`; a view that needs the current time takes a clock
  in its options, and the test passes a fixed one.
- **Fixed size** — the model is sent an explicit `tea.WindowSizeMsg`;
  the composed frames use 80×24 unless the scenario is about another size
  (120×40 wide, 60×14 narrow, 24×5 tiny). A frame is always exactly the
  terminal: `h` lines, none wider than `w` columns.
- **No colour** — the models are built with `NoColor: true`, so a frame is
  identical on a truecolor terminal, under `NO_COLOR`, and in a CI log.
- **No animation** — `NoAnim: true`, and the animation goldens step frames
  by hand with `Animator.Advance()` rather than by a ticker.

### Adding or changing a composed frame

1. Add a `gfScenario` to `gfScenarios()` in `internal/tui/golden_test.go`
   with a new name and, if it needs one, a size.
2. Run `make update-golden-tui`.
3. Read `testdata/tui/frame-<name>.golden`, then run `go test
   ./internal/tui/...` once more to confirm it compares clean.

Renaming or dropping a scenario leaves an orphaned file behind;
`TestGoldenFrameFilesAreReadable` fails until you delete it, because every
`frame-*.golden` must be produced by a scenario.

### When a frame will not settle

If `TestGoldenFramesAreDeterministic` fails, the frame differs between two
renders of the same input: something non-deterministic reached the view.
Usual suspects, in order — a `time.Now` call, iteration over a map, a
duration computed against the wall clock, and a `lipgloss.NewStyle()` that
picked up the global renderer's colour profile instead of the theme's
(`Theme.Style`).
