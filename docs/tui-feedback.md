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
  a verdict raised. Each names the item id it answers, and is drawn in the
  round it was given, not in the round the item was raised in: above the
  round's verdicts when it answers an earlier round, and under them when the
  loop collected the answer inside the round that raised the item.
- `✓ qa approve` / `✗ qa changes` — each tester's verdict, with the items it
  raised underneath, prefixed by a severity glyph (`!` blocker, `▲` major,
  `•` minor, `·` nit), then the `(rules)` mark of a rules item and, once a
  later round restates them, their status in brackets: `[resolved]`,
  `[accepted]`, `[held]`, `[overruled]`. The marks come before the title,
  which is the field a narrow pane cuts.
- `⚖ uphold_tester` / `⚖ uphold_engineer` / `⚖ compromise` — the product
  owner's ruling on an item the engineer and the tester could not settle.
- `▲ escalation …` — the product owner's decision once the bead hit the
  review round cap: `approve_with_notes`, `narrow_scope` or `block`.

The same item id can be raised in several rounds; each statement of it is
its own line, carrying the answers that statement drew. Two lenses can also
raise the same item id in one round — the feedback loop consolidates a
round's items across the three lenses — and then one engineer response or
one ruling settles both occurrences: each lens holds its own item until its
own newest statement is settled, and the open count says so.

Text wider than the pane is never cut without a mark. The compact view ends
it in `…`; the details view wraps the whole of it onto indented
continuation lines, so nothing is out of reach.

In a terminal too narrow to split (under about 61 columns of pane) the bead
list collapses to a single `▌ 3/7 sc-e5f6  ux ✓  qa ✓  test ◐` selector line
and the history uses the whole width.

## Keys

The footer lists the keys every view shares — `tab` and `1`…`5` to switch
view, `↑`/`k` and `↓`/`j` to move, `q` to stop or quit. The `?` overlay
lists those and, under a **Feedback keys** heading, the view's own. Inside
the Feedback view, `↑`/`↓` move between beads and these keys act on the
selected bead's history:

| Key | Action |
|---|---|
| `enter`, `d` | toggle details: file and line, the item's full text (wrapped, never cut), its suggestion, the round and lens each answer replies to, and the report's files, verify command and risks |
| `pgdown`, `ctrl+f` | scroll the history down one screen |
| `pgup`, `ctrl+b` | scroll the history up one screen |
| `end`, `G` | jump to the newest round |
| `home`, `g` | jump to the first round |

Moving to another bead starts its history at the first round again.

## Colour

Every state is carried by a glyph as well as a colour, so the view still
reads under `NO_COLOR`, `--no-color` and on a 16-colour terminal. See
[config.md](config.md) for the colour settings.
