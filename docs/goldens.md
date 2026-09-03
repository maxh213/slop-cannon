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

"Every golden in the repo" means every extension: the frames and fixtures
named `*.golden`, and the prompt goldens named `*.golden.md` (Markdown, so an
editor renders them). `make check-golden` looks at both — its `git status`
pathspec lists `'*.golden' '*.golden.*' '*.expected' docs/cli.md`, and
`TestCheckGoldenCoversEveryGoldenSuffix` in `internal/tui/golden_test.go`
reads the Makefile and fails if a golden extension in the repository is not
covered by it. A pathspec that misses one is worse than no gate: the target
regenerates the file and then reports a clean tree.

Always read the diff before committing it. A regenerated golden is a change
to what a user sees, so it belongs in the same commit as the code that
changed it, and an unexpected hunk in a file you did not touch means
something leaked into the output (a wall clock, a map iteration, a colour
profile, a path from your machine).

`make check-golden` writes to the working tree, so run it on a clean tree —
it is the explicit "the goldens are current" gate, and CI runs it as its own
step after `make check`. `make check` catches the same thing implicitly: a
stale golden fails its test.

## Prompt goldens (`testdata/prompts/`, `testdata/agents/tester/`, `internal/agents/testdata/`)

The other family of goldens is whole assembled agent prompts (P5-01). They
are plain Markdown, one file per role, lens and mode, and they are what an
agent is actually told, so a diff here is a change to the product and not
only to a test:

| Directory | Files | Written by |
|---|---|---|
| `testdata/prompts/` | `po-plan`, `engineer-implement`, `tester-qa-review` | `internal/agents/prompt` (`TestBuildGoldenPO`, `…Engineer`, `…Tester`) |
| `testdata/prompts/` | `agent-engineer-implement`, `-address-feedback`, `-resolve-conflict` | `internal/agents` (`TestEngineerGolden*`) |
| `testdata/agents/tester/` | `<lens>-<mode>` for the three lenses × three modes | `internal/agents` (`TestTesterPromptGoldens`) |
| `internal/agents/testdata/plan/` | `po-plan-autopilot`, `po-plan-brief` | `internal/agents` (`TestPOPlanPromptGolden`) |
| `internal/agents/testdata/arbitration/` | `po-arbitrate`, `po-escalate` | `internal/agents` (`TestArbitrationPromptGolden`, `TestEscalationPromptGolden`) |

Most of them stop just before the output contract, which is pinned
separately, so an edit to `internal/agents/schemas/*.json` does not rewrite
every prompt; the three `internal/agents/prompt` role goldens end in a small
stand-in contract defined in the test for the same reason. The real contracts
are covered by `TestRealContractsAreAppended`.

As in `testdata/tui`, every file in these directories is accounted for:
`TestPromptGoldenFilesAreAccountedFor` (`internal/agents/prompt_goldens_test.go`)
lists each golden against the test that writes it, and fails when a file on
disk is not on the list, when a listed file is missing, or when one is empty,
not UTF-8, or carries a carriage return. Renaming a prompt fixture therefore
means editing `pgGoldens` in the same change, and a dropped one cannot sit
unnoticed.

Regenerate them with `make update-golden`, or just this family with
`UPDATE_GOLDEN=1 go test ./internal/agents/...`.

The prompt itself — the layers, their order, and the per-firing debug copies
under `<state-dir>/runs/<run-id>/prompts/<agent-id>.md` — is documented in the
`internal/agents/prompt` package comment; a user-facing page for it is still
open as P10-06.

## TUI frames (`testdata/tui/`)

Two kinds of file live here, both plain UTF-8 text with no colour:

- **Per-view frames** — `graph-*`, `agents-*`, `feedback-*`, `prs-*`,
  `anim-*`, `headless*`. Each is one component's pane, owned by the task
  that wrote the view.
- **Composed frames** — `frame-*`, from `internal/tui/golden_test.go`. Each
  is a whole terminal: header, tab bar, rule, pane and footer, plus the help
  and stop-confirmation overlays, exactly as the user sees them.

Every file in the directory is accounted for. `TestGoldenFrameFilesAreReadable`
reads all of them — not just the composed frames — and requires each to be
non-empty UTF-8 with no carriage return, and to be produced by something: a
composed frame comes from a `gfScenario`, and a per-view frame must be listed
in `gfViewGoldens` in the same file. Renaming or dropping a scenario in any
view therefore fails until the list is updated too, so a stale file cannot sit
in `testdata/tui` unnoticed.

Every frame is deterministic by construction, and a test enforces each part
of that:

- **Fixed clock** — frames are rendered from the scripted demo run
  (`tui.DemoEvents`), whose timestamps start at `tui.DemoStart`
  (2026-01-01T12:00:00Z) and advance by `tui.DemoStep`. Nothing in a frame
  may come from `time.Now`; a view that needs the current time takes a clock
  in its options, and the test passes a fixed one.
- **Fixed size** — the model is sent an explicit `tea.WindowSizeMsg`;
  the composed frames use 80×24 unless the scenario is about another size
  (120×40 wide, 60×14 narrow, 24×5 tiny, 40×2 shortest). A frame is always
  exactly the terminal: `h` lines, none wider than `w` columns, including on
  a terminal too short for the chrome, where `Model.View` clips.
- **No colour, and no global renderer** — the models are built with
  `NoColor: true` *and* an explicit `Theme`, and every style in the package
  is built from that theme (`Theme.Base`, `Theme.Style` and the semantic
  helpers). A `lipgloss.NewStyle()` anywhere in a view would use the global
  Lip Gloss renderer, whose colour profile is detected from the process's
  stdout — Ascii under a piped `go test`, ANSI under `CLICOLOR_FORCE`,
  truecolor on a real tty — and the frame would then depend on the machine.
  Two tests hold the line: `TestMain` pins the global renderer to *truecolor*
  so any such style shows up rather than hiding, and
  `TestGoldenFramesIgnoreTheGlobalRenderer` re-renders every frame under
  every profile and requires the bytes to match. Bold, underline and reset
  are the only escapes a frame may carry: with `NO_COLOR` they are what keeps
  the cursor, the selected box and the active tab visible (P7-07).
  Underline is allowed but unused — Lip Gloss underlines every space with its
  own escape run, which bloats a frame and confuses width measurement, so the
  active tab is bold instead.
- **Animation off, except once** — the frames use `NoAnim: true`, and the
  `anim-*` goldens step frames by hand with `Animator.Advance()` rather than
  by a ticker. One composed frame, `frame-anim`, runs with the layer on and
  is advanced by an explicit `AnimTickMsg`, so the overlay `Model.View`
  composites over the finished chrome is covered by a whole frame as well:
  the layer reads no clock and seeds its particles from a constant, so this
  is still exact. `SC_NO_ANIM` is unset in `TestMain` so a shell that exports
  it cannot switch that frame off.

### Adding or changing a composed frame

1. Add a `gfScenario` to `gfScenarios()` in `internal/tui/golden_test.go`
   with a new name and, if it needs one, a size.
2. Run `make update-golden-tui`.
3. Read `testdata/tui/frame-<name>.golden`, then run `go test
   ./internal/tui/...` once more to confirm it compares clean.

Renaming or dropping a scenario leaves an orphaned file behind;
`TestGoldenFrameFilesAreReadable` fails until you delete it, because every
`.golden` in `testdata/tui` must be produced by a scenario or listed in
`gfViewGoldens`.

### When a frame will not settle

If `TestGoldenFramesAreDeterministic` fails, the frame differs between two
renders of the same input: something non-deterministic reached the view.
Usual suspects, in order — a `time.Now` call, iteration over a map, a
duration computed against the wall clock, and a `lipgloss.NewStyle()` that
picked up the global renderer's colour profile instead of the theme's
(`Theme.Style`). The last of those has its own test:
`TestGoldenFramesIgnoreTheGlobalRenderer` fails with the name of the frame
that moved. If a golden only fails for one person, ask them for `TERM`,
`COLORTERM`, `NO_COLOR`, `CLICOLOR_FORCE` and `SC_NO_ANIM`, and whether they
ran a compiled test binary on a tty rather than `go test` through a pipe.

One more variable belongs on that list, and it is the only one the suite
handles by itself: **`RUNEWIDTH_EASTASIAN`**. A terminal configured for
ambiguous-width 2 exports it, and both go-runewidth and
`github.com/charmbracelet/x/ansi` — the width measurement under every Lip
Gloss pad, truncate and join — read it once, in a package initialiser, into a
condition no test can reach afterwards. In such a process `·`, `✓` and `▌`
measure two cells and the padding of every frame shifts, which would fail the
whole suite for that developer and, worse, would make
`make update-golden-tui` rewrite all of the frames with the wrong padding
while CI went red. So `TestMain` does not try to unset it — too late by then
— it hands the entire run to a child process started without it.
`TestGoldenFramesSurviveEastAsianWidth` proves that works by running the
golden tests again with `RUNEWIDTH_EASTASIAN=1` set on purpose. (The one
place the mode is exercised rather than neutralised is
`TestThemeGlyphColumnHoldsUnderEastAsianWidth`, which checks the glyph column
holds in both width modes; `TestMain` leaves that child alone.)
