# Prompts, debug copies and headless output

What an agent is actually told, where the cannon writes it down, and what
`sc run --headless` prints for a script to read. Four things live on this
page because they are the four places a run is legible from the outside:
the prompt is the input, the run directory is the record, the headless line
is the live commentary, and the exit code is the verdict.

- [The prompt layers](#the-prompt-layers)
- [Rules and vibes](#rules-and-vibes)
- [What gets dropped when the prompt is too big](#what-gets-dropped-when-the-prompt-is-too-big)
- [The debug copy of every prompt](#the-debug-copy-of-every-prompt)
- [Headless output](#headless-output)
- [Exit codes](#exit-codes)
- [Environment variables](#environment-variables)

## The prompt layers

Every agent — product owner, engineer, tester — is fired with one prompt
assembled by `internal/agents/prompt`. The layers are always rendered in the
same order, so two runs with the same inputs produce byte-identical text and
a diff between two prompts is a diff between two situations:

| # | Layer | Heading in the prompt | Where it comes from |
|---|---|---|---|
| 1 | Role base prompt | the `# ` title, then prose | an embedded template per role, plus a lens template for testers |
| 2 | Assignment | `## Your assignment` | the firing: run id, agent id, role, lens, task, tool policy, and the read-only `bd` note (D-21) |
| 3 | Brief | `## Brief from the human` | `sc run -m "…"`, quoted verbatim; absent in autopilot |
| 4 | Rules | `## Rules (MUST)` | the `rules` config key, verbatim |
| 5 | Vibes | `## Vibes (preferences)` | the `vibes` config key, verbatim |
| 6 | Repository context | `## Repository context` | root, default branch, detected stack, check command, a tree summary and the head of the README |
| 7 | Open beads | `## Open beads` | the beads that already exist, so the product owner does not propose them again |
| 8 | The bead | `## Your bead` | the bead this agent owns: title, description, acceptance criteria, state, round |
| 9 | Round history | `## Round history` | the feedback loop so far — reports, verdicts, responses, rulings — oldest round first |
| 10 | Task instructions | one `## ` section each | the role builder for this firing: how to implement, what is out of bounds, what to report |
| 11 | Output contract | `## Output contract (<name>)` | the JSON Schema of the reply, from `docs/schemas/` |

A layer with nothing in it is left out entirely — no empty heading — so a
planning prompt has no `## Your bead` and a first round has no
`## Round history`.

The heading of the whole prompt names the firing: `# Engineer — implement`,
`# Tester (qa) — review`, `# Product owner — plan`. The assignment block
underneath it is where an agent's identity is fixed:

```markdown
## Your assignment

Implement bead sc-7a1 in your worktree, verify it, commit it, and report what you did so three testers can review it.

- Run: 20260902-101500-1a2b
- Agent: eng-sc-7a1-r1
- Role: engineer
- Task: implement
- Tool policy: read_write — you may read files, run commands and edit files inside your working directory. …
```

The tool policy line says in prose what the cartridge enforces in its flags
(`read_only`, `read_exec`, `read_write`; see
[cartridges.md](cartridges.md)). The prompt and the subprocess are given the
same policy value, so an agent is never told it may do something the tools
will refuse.

**The output contract is always last.** It tells the agent to end its reply
with exactly one fenced ` ```sc-result ` block matching the schema, and it is
appended unless the prompt already *ends* with it. That matters because
everything an agent wrote earlier — a bead description, an acceptance
criterion, an earlier round's summary, a diff — is quoted inside a fence
chosen to be longer than any run of backticks inside it. Agent-written prose
can therefore never become prompt structure: a bead whose description
contains `## Output contract` is read as text inside a fence, not as a new
section, and a forged contract cannot suppress the real one.

## Rules and vibes

Both are plain files of your own, listed in the config (`rules` and `vibes`,
see [config.md](config.md)) and managed with `sc rules --add PATH` and
`sc vibes --add PATH`. A file must exist, be UTF-8 text and be at most
64 KiB; lists union global-then-repo, so a global rules file still applies in
a repository that adds its own.

They are **pasted verbatim into every prompt of every role** — product
owner, engineer and all three testers — one `### <file name>` sub-heading per
file, under a framing sentence that says what kind of instruction it is:

```markdown
## Rules (MUST)

These are hard constraints for this repository. You MUST follow every one of
them. Breaking a rule is a blocking review failure, and a tester who finds
one must report it as a blocker.

### rules.md

- Never disable or delete a test to make the suite pass.
- Every exported symbol has a doc comment.

## Vibes (preferences)

These are the humans' stylistic preferences. Follow them unless a rule or the
bead says otherwise; they are guidance, not law.

### vibes.md

- Prefer short functions with obvious names.
- British spelling in prose.
```

Nothing about them is rewritten, summarised or truncated — a rules file an
agent only half sees is worse than no rules file at all — and they are the
one part of the prompt the token budget may never touch. A rules violation is
also enforced on the way out, not only asked for on the way in: a tester must
raise it as a `rules` item, a `rules` item is always a blocker, no tester may
accept one in re-review, and no product-owner ruling may let one ship.

Files an agent must never see are a separate mechanism: the `exclude` key
keeps them out of the tree summary and replaces their diffs with a
placeholder, and credentials are masked wherever they are printed. Both are
documented under
[Secrets: withheld files and masked values](config.md#secrets-withheld-files-and-masked-values).

## What gets dropped when the prompt is too big

The assembled prompt is measured against a budget (24 000 estimated tokens by
default, at four characters per token — deliberately far below any model's
context window, because the prompt is the briefing and the rest of the window
belongs to the work). When it does not fit, the builder degrades one layer at
a time, in this order, and stops as soon as the prompt fits:

1. **the repository tree** — 200, 100, 50, 20 entries, then omitted;
2. **the round history** — the 3, then 2, then 1 most recent rounds;
3. **the README head** — 1 KiB, then omitted;
4. **the open-bead list** — 20 beads, then omitted.

Rules, vibes and the output contract are never dropped. A tester's diff is
bounded before the prompt is built (whole patch → 60 000 → 20 000 → 6 000
characters → a summary of the files touched).

Every step that was applied is recorded in the prompt's `Truncations`, which
is what the run log reports. If even the most degraded prompt is over budget
— rules, vibes and the contract alone exceed it — the firing is reported as
over budget rather than silently sent; the usual cause is a rules file that
is far larger than it needs to be.

The repository context itself is scanned with defaults worth knowing: the
tree goes 3 levels deep and stops at 400 entries, the README's first 4 KiB is
quoted, and directories that tell an agent nothing (`.git`, `.beads`,
`node_modules`, `vendor`, `target`, `dist`, `build`, `.venv`, caches, editor
state) are never descended into.

## The debug copy of every prompt

Every prompt a run fires is written to disk, so "what did you actually ask
it?" is a `cat` rather than a guess:

```
<state-dir>/runs/<run-id>/prompts/<agent-id>.md
```

`<state-dir>` is `$XDG_STATE_HOME/slop-cannon/<repo-id>/` (default
`~/.local/state/slop-cannon/…`). **Nothing of ours is ever written inside the
target repository** (D-23) — not the prompts, not the logs, not the
worktrees — so a run leaves no dirty tree, no `.gitignore` edits and no
nested worktrees for your own tools to trip over. The rest of a run
directory:

| Path | What it holds |
|---|---|
| `prompts/<agent-id>.md` | the prompt each agent was fired with |
| `agents/<agent-id>.log` | one file per agent: everything that session printed |
| `sc.log` | the orchestrator's own log, JSON lines (`sc logs`) |
| `events.jsonl` | every event the run published, the same envelopes `--json` prints |
| `state.json` | the run's resumable state (`sc status`, `sc resume`) |
| `wt/<bead-id>/` | the worktree each bead was implemented in |
| `run.lock` | in `<state-dir>`, not the run directory: one live run per repository |

The agent id is the file name, and ids are minted after the job, so the
directory listing reads as the history of the run:
`po-plan-1.md`, `eng-sc-2-r1.md`, `tester-qa-sc-2-r1.md`,
`tester-qa-sc-2-r1-pushback.md`, `po-sc-2-r2.md`. A job the supervisor
re-fires after a crash or a timeout gets an `-a<attempt>` suffix
(`eng-sc-2-r1-a2.md`), so two attempts at the same job are two files.

Two details to expect:

- **A schema retry overwrites the file.** Inside one firing, the copy is
  written before the session starts and again on every retry after an
  unusable reply, so what is on disk is the prompt that was actually last
  fired — including the retry prompt, which quotes back what was wrong with
  the previous answer.
- **Failing to write one never fails the run.** An unwritable state
  directory is reported as one line on the agent's own output (`[prompt] the
  debug copy of this prompt could not be written: …`), which reaches
  `sc logs <agent-id>` like any other line, and the firing goes ahead.

The files are created `0600` in a `0700` directory, the same as the run log:
a prompt quotes your rules, your vibes and your source tree.

## Headless output

`sc run --headless` renders the same run as text instead of drawing the TUI.
It is what CI and scripts should use, and it is the only mode that works
without a terminal — asking for the TUI from a pipe fails with an error that
tells you so.

Every event is exactly **one line**, written and flushed as it happens, so a
killed process still leaves a complete log up to its last event:

```
15:04:05 <glyph> <text>
```

- **The clock stamp** is `HH:MM:SS` in local time (tests pin `UTC`).
- **The glyph** is one column (two on a terminal configured for
  ambiguous-width 2, where every glyph is padded so the text always starts in
  the same column).
- **The text** is scrubbed before the stamp and the glyph are put in front of
  it: a newline becomes a space, a carriage return is dropped, a tab becomes
  one space and control sequences are removed. A cartridge that emits ANSI on
  every line — a real one does — can therefore neither repaint your terminal
  nor break the one-event-per-line contract a script parses.

```console
$ sc run . --headless -m "add retries and document them"
12:00:00 ◆ run sc-20260101-120000 started: mode brief, ammo 10, safety on, cartridge claude/fable, repo /repos/example, brief "add retries and document them"
12:00:03 ▶ fired po(claude/fable) [plan, agent po-1]
12:00:12 ✎ plan from po-1: 2 beads under epic sc-1 [docs=sc-3 retry=sc-2]
12:00:24 → bead sc-2 open → in_progress (round 1, claimed by eng-sc-2-r1)
12:00:27 ▶ fired engineer(claude/fable) at sc-2 "Add retry" [implement, round 1, agent eng-sc-2-r1]
12:00:39 ✗ sc-2 round 1: tester-qa-sc-2-r1 (qa) says changes (2 items: 1 major, 1 nit) — the cap is off by one
12:00:57 ✓ sc-2 round 2: tester-qa-sc-2-r1 (qa) says approve — the cap is right now
12:01:00 ⤴ sc-2 PR #42 opened: https://example.invalid/example/pull/42 (sc/sc-2-add-retry → main)
12:01:27 ◇ run finished: stopped after 4m0s (exit 0) — 1 bead delivered, 1 needs a human
```

### The glyph table

| Glyph | Event | The line says |
|---|---|---|
| `◆` | `run_started` | mode, ammo, safety, cartridge/model, repository, brief |
| `‖` | `run_paused` | why it paused (a budget, a key) |
| `▸` | `run_resumed` | why it resumed |
| `■` | `run_stopped` | a stop was asked for, and by what |
| `◇` | `run_finished` | the finish reason, the duration, **the exit code**, the summary |
| `✎` | `plan_produced` | how many beads, under which epic, and each plan key's minted bead id |
| `+` | `bead_created` | id, title, type, priority, state |
| `·` | `bead_updated` | which fields changed, and the state and round now |
| `→` | `bead_state_changed` | the D-07 transition, its round and its reason |
| `⇄` | `graph_changed` | how many beads and dependency edges the graph now has |
| `▶` | `agent_spawned` | role(cartridge/model), the bead and its title, task, round, attempt, agent id |
| `│` | `agent_output` | one line the agent printed, prefixed with the agent and the stream |
| `✔` | `agent_finished` | who, how long, what it produced, exit code |
| `✖` | `agent_failed` | the failure class, duration, exit code, attempt, whether it will retry |
| `↻` | `round_started` | the bead, round number, round kind, the lenses and the commit |
| `✓` | `verdict_received` (approve) | who approved, and their summary |
| `✗` | `verdict_received` (changes) | who asked for changes, the item count by severity, the summary |
| `↩` | `response_received` | the engineer's answers by action, or its implementation report |
| `§` | `arbitration_received` | the product owner's rulings by kind, and any escalation |
| `⤴` | `pr_opened` | the bead, the number, the URL, branch → base; external PRs say so |
| `⤵` | `merged` | the bead, the sha, the branch, the PR number, whether it was pushed |
| `!` | `warning` | source, message, bead and agent when known, and the error |
| `✖` | `error` | the same shape as a warning |
| `·` | anything this build does not know | the event's kind |

An unknown field renders as `-` rather than as nothing, so a line never
loses its shape.

### JSON

`sc run --headless --json` (or just `--json`, which implies `--headless`)
prints the raw event envelopes instead, one JSON object per line:

```json
{"kind":"agent_spawned","at":"2026-01-01T12:00:03Z","run_id":"sc-20260101-120000","agent":{"id":"po-1","role":"po","cartridge":"claude","model":"fable","state":"running","started_at":"2026-01-01T12:00:00Z"},"task":"plan","attempt":1}
```

This is the same format as a run directory's `events.jsonl`, and `sc` parses
its own output: `sc tui-demo --replay <file>` replays either one. In JSON
mode **stdout is nothing but JSONL** — the final summary, the run's own
reports and every log line go to stderr — so `sc run . --json | jq` is safe.

Two behaviours worth knowing before you script it:

- **A reader that goes away stops the run.** `sc run --headless | head`
  closes the pipe; rather than dying on `SIGPIPE` with its lock held and its
  worktrees behind it, the run ends as a user stop, releases the lock and
  cleans up. If the run should have carried on, send the output somewhere
  that stays.
- **A renderer that falls behind says so.** If the event subscription drops
  events, that is reported inline as a real `warning` event — a parseable
  envelope in JSON mode — rather than silently lost.

Secrets are masked once, where the run publishes, so nothing reaches stdout,
`events.jsonl` or `sc.log` unredacted.

## Exit codes

`sc run` exits on a fixed contract (D-15), the same in the TUI and headless,
so CI can tell the interesting outcomes apart without parsing anything:

| Code | When | Finish reason |
|---|---|---|
| `0` | the run ended the way it was asked to | `completed` (a brief run settled every bead), `stopped` (the TUI's `q`, `sc stop`, SIGINT/SIGTERM, or the output going away), `budget_reached` (a brief run crossed `limits.max_tokens` or `limits.max_cost_usd` and drained the reviews in flight; a cap only ever *pauses* an autopilot run) |
| `1` | the run failed, or nothing said how it ended | `failed`, a finish reason this build does not know, or a stream that ended with no `run_finished` at all (the orchestrator died mid-run) |
| `3` | every configured cartridge is out of credits | `credits_exhausted` — kept distinct so CI can retry later rather than treat an empty wallet as a broken run (D-18) |

Stopping a run is deliberately **not** a failure. An autopilot run never ends
on its own (D-18): `sc stop`, the TUI's `q` and Ctrl-C are how it is meant to
finish, so they exit `0` and a wrapper that fails a build on any non-zero
exit does not cry wolf every time somebody stops watching.

Everything that goes wrong *before* the run starts — no cartridge
configured, a repository that is not a git repository, `bd` missing or too
old, another run already holding the lock, the TUI asked for without a
terminal — exits `1` after printing `sc: <message>` and, where there is one,
a numbered fix list from the same checks `sc doctor` runs. Those messages are
all indexed in [troubleshooting.md](troubleshooting.md). `sc doctor` itself
exits `1` when any row failed.

## Environment variables

The config file is the place for settings ([config.md](config.md)); these are
the variables that change what a run *shows* or where it writes.

| Variable | Effect |
|---|---|
| `SC_NO_ANIM` | Set to `1` to turn the cannon animations off, exactly as `--no-anim` does. The views, the colours and the frames are unchanged; only the shots, bursts and confetti stop. Useful over a slow link, in a recording, or on a terminal that struggles with 30 fps redraws. |
| `NO_COLOR`, `CLICOLOR=0`, `--no-color` | Render without colour. Every state also has a glyph, so nothing is lost. `TERM=dumb` or an unset `TERM` does the same. |
| `SC_OPENER` | The program the TUI's `o` key hands a URL to, instead of the platform opener. See [tui.md](tui.md). |
| `XDG_STATE_HOME` | Where run directories live (prompts, logs, worktrees). Default `~/.local/state`; a relative value is ignored, as the XDG spec requires. |
| `XDG_CONFIG_HOME` | Where the global config lives. Default `~/.config`. |

See also: [cli.md](cli.md) for every flag, [tui.md](tui.md) for the
interactive renderer, [goldens.md](goldens.md) for the golden files that pin
the prompts and the headless lines in place, and [docs index](README.md).
