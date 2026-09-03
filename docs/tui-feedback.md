# The Feedback view

`Feedback` is the third tab of the run TUI (`3` jumps to it). It is the
review history of the run: every bead a tester looked at, what was said
about it, and where each tester stands. [tui.md](tui.md) covers the chrome
around it — the header, the environment variables and the other views.

## Layout

The left column lists the beads that went through review, one row each,
with a tick per lens — `U` ux, `Q` qa, `T` test:

| Tick | Meaning |
|---|---|
| `✓` | the tester approved and is not being asked again |
| `✗` | the tester asked for changes |
| `◐` | the newest round asked this lens and its verdict has not arrived |
| `·` | the lens has not reviewed this bead |

The right pane is the selected bead's whole history, one section per review
round, in the order things were said:

- `⚙ report` — the engineer's implementation report for the round.
- `→ fixed`, `↩ pushback`, `⊘ wontfix` — the engineer's answers to the items
  the *previous* round raised. Each names the item id it answers, and is
  drawn in the round it was given, not in the round the item was raised in.
- `✓ qa approve` / `✗ qa changes` — each tester's verdict, with the items it
  raised underneath, prefixed by a severity glyph (`!` blocker, `▲` major,
  `•` minor, `·` nit) and, once a later round restates them, their status in
  brackets: `[resolved]`, `[accepted]`, `[held]`, `[overruled]`.
- `⚖ uphold_tester` / `⚖ uphold_engineer` / `⚖ compromise` — the product
  owner's ruling on an item the engineer and the tester could not settle.
- `▲ escalation …` — the product owner's decision once the bead hit the
  review round cap: `approve_with_notes`, `narrow_scope` or `block`.

The same item id can be raised in several rounds; each statement of it is
its own line, carrying the answers that statement drew.

In a terminal too narrow to split (under about 61 columns of pane) the bead
list collapses to a single `▌ 3/7 sc-e5f6  ux ✓  qa ✓  test ◐` selector line
and the history uses the whole width.

## Keys

The footer and the `?` overlay list the keys every view shares — `tab` and
`1`…`5` to switch view, `↑`/`k` and `↓`/`j` to move, `q` to stop or quit.
Inside the Feedback view, `↑`/`↓` move between beads and these keys act on
the selected bead's history:

| Key | Action |
|---|---|
| `enter`, `d` | toggle details: file and line, the item's full text, its suggestion, the round each answer replies to, and the report's files, verify command and risks |
| `pgdown`, `ctrl+f` | scroll the history down one screen |
| `pgup`, `ctrl+b` | scroll the history up one screen |
| `end`, `G` | jump to the newest round |
| `home`, `g` | jump to the first round |

Moving to another bead starts its history at the first round again.

## Colour

Every state is carried by a glyph as well as a colour, so the view still
reads under `NO_COLOR`, `--no-color` and on a 16-colour terminal. See
[config.md](config.md) for the colour settings.
