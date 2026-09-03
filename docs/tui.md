# The TUI

`sc run` watches a run in a full-screen terminal UI. Everything it draws is
folded out of the event bus, so the display is a view of the run and never a
source of truth: closing it does not stop the run, and reopening it on a
resumed run rebuilds the same picture from the events.

`sc tui-demo` renders a scripted run with no orchestrator, no repository and
no network. It is the fastest way to see every view.

## Layout

```
▄█▓═▶  slop-cannon · run <id> · <mode> · ammo <n>/<n> · safety on   header
 1 Graph  2 Agents  3 Feedback  4 PRs  5 Log                       tabs
────────────────────────────────────────────────────────────────
                       the active view                            main pane
tab next view • 1…5 jump to view • ↑/k up • ↓/j down • ? help      footer
```

The header drops fields from the left as the terminal narrows, keeping the
run id, and the cannon fires at the graph when an agent is spawned and
reacts to verdicts and failures (`--no-anim` or `SC_NO_ANIM=1` turns the
animations off).

## Keys

| Key | What it does |
|---|---|
| `tab` / `shift+tab` | next / previous view |
| `1`…`5` | jump straight to a view |
| `↑`/`k`, `↓`/`j` | move the cursor inside the view |
| `?` | help overlay: the global keys, plus the keys of the active view |
| `q` | quit; on a live run it first asks to confirm the stop |
| `ctrl+c` | quit immediately |

Views add keys of their own. The `?` overlay lists them under the view's
name, and the view puts the important ones in its own section headings, so a
binding is discoverable without opening the overlay.

## Views

| Tab | Shows |
|---|---|
| Graph | the bead DAG, with `enter` for a bead's detail |
| Agents | a card per live agent, the history of the ones that ended, and their transcripts |
| Feedback | review rounds per bead: verdicts, responses, arbitration ([tui-feedback.md](tui-feedback.md)) |
| PRs | deliveries, their pull requests and check state; `o` opens a URL |
| Log | every event, newest last |

## Agents view

The pane has two sections. **LIVE** is one bordered card per agent whose
subprocess is still running:

```
LIVE · AMMO 2/6 · ENTER TRANSCRIPT ─────────────────────────────
╭──────────────────────────────────────────────────────────────╮
│ ■ engineer · av-2 · claude/fable · implement r1     2m0s ⠹    │
│ step 12: editing internal/http/client.go                      │
╰──────────────────────────────────────────────────────────────╯
```

A card carries the role glyph and role (`■` engineer, `▲` tester, `◆`
product owner — the same glyphs the Graph view uses), the tester's lens, the
bead it is working on, its cartridge and model, what it was fired to do with
its round and attempt, how long it has been running, a spinner, and its last
line of output. A narrow pane drops the optional fields from the right
rather than truncating them mid-word.

The elapsed time and the spinner advance ten times a second while any agent
is live, and stop the moment the last one exits, so an idle run costs
nothing.

**HISTORY** is one line per agent that ended, newest first: `✓` finished,
`✗` failed, `⊗` killed, then what it produced or why it stopped, and how
long it ran.

### Transcript and follow mode

`enter` opens the selected agent's transcript: its card, a heading saying
which lines are on screen and whether the pane is following, and the output
itself. Lines the agent wrote to stderr are marked `!`; the view's own notes
(spawned, finished, failed) are marked `·`.

| Key | What it does |
|---|---|
| `enter` | open the transcript of the selected agent |
| `esc` | back to the card list |
| `f` | toggle follow mode |
| `↑` / `↓` | scroll one line |
| `pgup` / `pgdn` (`ctrl+b` / `ctrl+f`) | scroll a screenful |
| `home` (`g`) / `end` (`G`) | oldest / newest line |

A pane in **follow mode** is pinned to the newest line, so it streams as the
agent writes. Scrolling up leaves follow mode and freezes the viewport —
lines arriving while you read do not move the text under you — and scrolling
(or `end`) back to the newest line re-enters it. The heading says which of
the two you are in.

The selection is pinned to the agent, not to the row it is on: when a live
agent ends it leaves the LIVE section for the head of HISTORY and every row
below it moves, but the cursor — and an open transcript — stays with the
agent you chose.

Two things bound the transcript:

- Only the newest **500 lines** per agent are kept: the pane is a live tail,
  not an archive, so the oldest lines are dropped as new ones arrive.
- Every line is scrubbed before it is drawn. A cartridge is a CLI, so its
  output can carry ANSI escapes, carriage returns and other control
  characters, which would otherwise repaint or corrupt the frame; the view
  keeps the printable text, honours a carriage return the way a terminal
  does (a self-rewriting progress line lands as its final state) and turns
  tabs into spaces.

## PRs view

One row per delivery, in the order the pull requests were opened, under a
line saying what safety means for this run and how the deliveries are doing.

| Column | What it holds |
|---|---|
| `BEAD` | the bead delivered, or the review bead of an external pull request |
| `PR` | `#42`, `ext #77` for a pull request the cannon only reviews (D-11), `-` for work that went straight to the default branch |
| `STATUS` | `open`, `awaiting merge` (safety on: a human merges it, D-20), `reviewing`, `reviewed`, `merged`, `closed`, `needs human`, `failed` — each with its own glyph |
| `CHECKS` | what is known about the pull request's checks; for an external pull request, the review the cannon posted on it |
| `REF` | the pull request URL, or the short merge sha |

`o` opens the selected row's URL. The `REF` column is only shown when the
pane is wide enough for a whole URL; in a narrower terminal — the canonical
80 columns included — the selected row's URL moves to the detail line at the
bottom, where it can still be read and copied. The detail line also says
whether there is a URL to open, and the footer reports what `o` did.

**`SC_OPENER` replaces the browser.** `o` hands the URL to the platform
opener — `open` on macOS, `rundll32 url.dll,FileProtocolHandler` on Windows,
`xdg-open` elsewhere — or to the program named by `SC_OPENER`, with the URL
appended as its only argument:

```console
$ SC_OPENER=firefox sc run --brief "fix the retry bug"
```

The value names one executable; arguments inside it are not parsed. It runs
with a minimal environment plus the variables a desktop opener needs
(`DISPLAY`, `WAYLAND_DISPLAY`, `BROWSER`, `DBUS_SESSION_BUS_ADDRESS` and
friends), is killed after 15 seconds, and a failure — no opener on a
headless box, a non-zero exit — is reported in the footer instead of the key
silently doing nothing. Only `http` and `https` URLs are ever opened;
anything else is refused before the opener runs.

**Check status is derived, not polled.** The cannon never asks GitHub for
check runs. `failing` means a `check_failed` review round is in flight for
the bead, `conflict` means an engineer is resolving a merge conflict (D-10),
`passed` means the pull request was merged so whatever gates the repository
requires were satisfied, `pending` means the pull request is open and none of
those have happened, and `-` means nothing is known — including on an
abandoned or parked delivery, where nothing is running any more and which the
counts keep out of "in flight" for the same reason.

**A review is reported once it is posted.** The testers post one consolidated
review per round (D-11), so while a round is still collecting verdicts the
`CHECKS` column counts them (`1/3`, or `1/?` when the round never announced
its lenses) and the status stays `reviewing`. Only when the round is complete
does the row say `reviewed` and show the outcome the review went out with —
one `changes` verdict makes the whole review a request for changes.

## Colour

The TUI has one theme, and every style comes from it, so a frame does not
depend on the developer's `TERM`. It picks 24-bit, 256-colour or 16-colour
from the environment and uses a colour-blind-safe palette; every state also
has a one-cell glyph, so meaning survives with no colour at all.
`--no-color`, `NO_COLOR` and `CLICOLOR=0` switch colour off; `TERM=dumb` or
an unset `TERM` does too.

## No terminal

`sc run --headless` prints one line per event instead of drawing anything,
and `--headless --json` prints the raw event stream as JSONL. Both are
what CI should use; see `docs/cli.md`.
