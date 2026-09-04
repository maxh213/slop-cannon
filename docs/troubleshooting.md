# Troubleshooting

Every row below is keyed by a message `sc` can actually print, exactly as it
appears in the output. Find the words you saw, read what happened, do the
thing in the last column. A test (`TestTroubleshootingMessagesStillExistInTheCode`)
keeps the keys honest: a message that no longer exists in the code cannot
stay on this page, and a new user-facing error cannot be added without a row
here.

**Start here, whatever the message.** `sc doctor` checks git, `bd`, `gh`, the
cartridge CLIs, the repository and the bead database in one pass, prints a
hint under every row that is not `pass`, and exits 1 when anything failed.
When a run refuses to start, `sc` prints the failing rows itself as a
numbered fix list — the same words, from the same check — and then suggests
`sc doctor` for the rest.

**Exit codes.** `0` the run finished or was stopped, `1` the run failed or a
command was refused, `3` every cartridge is out of credits.

**Where things are.** Nothing `sc` writes lives inside the target repository:
logs, prompts, events and worktrees go under
`$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/<run-id>/` (see
[cli.md](cli.md)), and the first run in a repository prints the paths. `sc
status` reads the same files back, and `sc status --json` gives them to a
script. The one thing `sc` may write inside the repository is
`.slop-cannon/config.toml`, and only when a config command created it.

---

## Starting a run

| Message | What happened | What to do |
|---|---|---|
| `is not a git repository (no .git in it or any parent directory)` | The path given to `sc` is not inside a git working tree, and `sc` only fires at repositories. | `git init`, or point `sc` at the right path: `sc /path/to/repo`. |
| `no such directory` | The path does not exist. | Check the spelling; a relative path is resolved against the current directory. |
| `not a directory` | The path exists but is a file. | Name the repository directory, not a file inside it. |
| `a resumed run keeps its original brief` | `-m` was combined with `--resume`. A resumed run continues the beads the earlier run planned, so a new brief would have nowhere to go. | Drop `-m` to resume, or drop `--resume` to start a new run from the brief. |
| `the TUI needs a terminal: stdin and stdout are not a TTY` | The full-screen interface was asked for from a pipe, a CI job, or an editor's task runner. | Add `--headless` for log lines, or `--headless --json` for one JSON envelope per event. |
| `tui-demo needs a terminal: stdin and stdout are not a TTY` | The same, for `sc tui-demo`. | Run it in a terminal, or use its `--json` output. |
| `is already going in this repository` | Another `sc` process holds this repository's run lock. Only one run at a time per repository (P9-04). | `sc status` names it. Wait for it, or stop it. `--force` takes the lock, and only makes sense once you are sure the other run is gone. |
| `cannot tell whether it is still alive` | The lock was written by another machine — a state directory on a shared file system — so this host cannot probe that process. | Check the run on the machine named in the message. If it really stopped, start again with `--force`. |
| `if no run is going, delete it and try again` | The lock file could not be parsed, and it is younger than the grace period, so `sc` waits rather than deleting what might be a run starting right now. | Wait half a minute and retry; if nothing is running, delete the file the message names. |
| `removed a stale run lock` | A previous run was killed or crashed. `sc` found its lock, proved the process is gone, removed it and carried on. | Nothing. It is a notice, not an error. |
| `orchestrator: another run holds the lock` | The lock check ran again as the run started and found a live holder. | As above: `sc status`, then wait, stop it, or `--force`. |
| `no run directory at` | `sc status` was asked for a run whose directory has been deleted or pruned. | `sc status` without arguments lists the runs that are still on disk. |
| `orchestrator: no state file` | The run directory exists but the run never got as far as its first state write. | Read `events.jsonl` in the same directory, or start a new run. |
| `orchestrator: no event log` | The run directory has no `events.jsonl`: the run was never started, or the directory was pruned. | Nothing to recover; start a new run. |
| `is not a whole number (example: sc ammo 12)` | `sc ammo` was given something that is not an integer. | `sc ammo 12`. |
| `ammo must be at least 3: one engineer, one tester, one product owner` | Fewer than three agents cannot run the loop at all. | Use 3 or more; the default is 10. |
| `unsupported shell` | `sc completion` was asked for a shell it does not generate. | Use one of the shells the message lists. |
| `is not wired up yet (P8-21)` | A TUI run-control key (re-fire a bead, mark one done, change safety or ammo mid-run) that has no orchestrator seam yet. | Stop the run, change the setting on the command line or in the config, and fire again. |

## Configuration files

`sc config show` prints the merged configuration and where each value came
from; the keys are documented in [config.md](config.md).

| Message | What happened | What to do |
|---|---|---|
| `use a Go duration such as` | A duration key was given a bare number or an unparseable string. | Quote it as a Go duration: `idle_replan_interval = "10m"`. |
| `safety must be on or off, got` | `--safety` or the `safety` key carries something else. | `on` opens pull requests, `off` merges to the default branch (D-10). |
| `schema_version must be a positive integer` | The config file's `schema_version` is missing, negative or not a number. | Remove the key to get the current default, or set it to the version in [config.md](config.md). |
| `is not supported yet` | A cartridge name that is not one `sc` ships an adapter for (D-19). | `sc cartridge --add claude --model <model>`. |
| `is not configured; add it with: sc cartridge --add` | The active cartridge names an entry no config file defines, usually after hand-editing or removing one. | Add it back, or `sc cartridge --use NAME` to pick one that exists. |
| `cartridges live in the global config only; use the global file` | A `[cartridges]` entry was written to a repository config. Cartridges are subscriptions and belong to the machine, not to a checkout (D-03). | Run the cartridge command without `--repo`, or move the block to the global file. |
| `cartridge name must not be empty` | `--add` or `--use` was given an empty name. | Name the cartridge: `sc cartridge --use claude`. |
| `nothing removed` | `sc vibes --remove` or `sc rules --remove` named a file that is not in that list. | `sc config show` lists the files that are; paths are compared after resolving them. |
| `not a regular file` | A vibes or rules path is a directory, a device or a socket. | Point at a file. |
| `not valid UTF-8 text` | A vibes or rules file is not text, so no agent could be given it. | Convert it to UTF-8, or point at the source document instead. |
| `contains NUL bytes; not a text file` | The same, for a binary file that happens to decode. | As above. |
| `no editor configured: set $EDITOR (for example: export EDITOR=vim)` | `sc config edit` has no editor to open. | Export `EDITOR`, or edit the file the message names by hand. |

## Cartridges (the LLM CLI)

A cartridge is the CLI subscription `sc` fires: it is added once with
`sc cartridge --add`, and [cartridges.md](cartridges.md) describes the
adapters.

| Message | What happened | What to do |
|---|---|---|
| `no cartridge loaded` | The run resolved a configuration with no active cartridge. Nothing can be fired without one (D-04). | `sc cartridge --add claude --model <model>`, then fire again. |
| `cartridge: no active cartridge configured (add one with: sc cartridge --add claude)` | The same, from the cartridge selector. | As above. |
| `cartridge: unknown cartridge` | The configuration names a cartridge `sc` has no adapter for. | `sc cartridge --list` shows what is available; only Claude ships today (D-19). |
| `cartridge: not built` | The active cartridge, or a per-role override, names a cartridge that was not built for this run — usually a stale `[roles]` entry. | Fix the name in the config, or remove the role override. |
| `cartridge: not installed` | The cartridge's CLI is not on `PATH`. | Install it, or set `command` under `[cartridges.<name>]` to its absolute path. |
| `cartridge: not authenticated` | The CLI is installed but not logged in, or its token was rejected. | Run the CLI once yourself and log in (for Claude Code: `claude`), then fire again. |
| `cartridge: credits exhausted` | The subscription's quota, credits or billing are gone. That cartridge is dead for the rest of the run; when every one is dead, the run ends with exit code 3 (D-18). | Wait for the window to reset, or add another cartridge and `sc cartridge --use` it. |
| `cartridge: rate limited` | The CLI reported a temporary limit. `sc` backs off and retries; the cartridge stays alive. | Nothing, unless it repeats — then lower `ammo` so fewer agents run at once. |
| `cartridge: timed out` | A session outlived `limits.agent_timeout` and was killed. | Raise the limit in the config, or split the work into smaller beads. |
| `cartridge: crashed` | The CLI exited non-zero, died on a signal, or printed something the adapter could not parse. | Read the agent log under `runs/<run-id>/agents/`; running the same command by hand usually reproduces it. |
| `cartridge: killed` | The session was stopped on purpose: you stopped the run, or the context was cancelled. | Nothing. |
| `orchestrator: no live cartridge is left for this run` | Every configured cartridge is out of credits or unusable, so no agent can fire. | Add or switch cartridges, then `--resume` the run. |

## The bead tracker (`bd`)

`sc` keeps the work as beads in the repository's `bd` database; the
conventions are in [beads-conventions.md](beads-conventions.md).

| Message | What happened | What to do |
|---|---|---|
| `brew install beads` | Part of the install hint printed whenever `bd` is missing or too old. `sc` needs beads 1.2.2 or newer. | `brew install beads`, or the upstream install script. |
| `embedded Dolt did not answer` | `bd` is installed but its database does not answer `bd ping`, which is what a `CGO_ENABLED=0 go install` build does: it is server-mode only. | Install a release binary rather than building with cgo disabled. |
| `not a git repository` | `bd` was asked to initialise a tracker outside a git working tree. | Run `sc` against a repository; `bd` stores the database beside `.git`. |
| `no beads project found` | The repository has no `.beads/` directory yet. | `sc` creates one on the first run (D-13); to do it by hand, `bd init` in the repository. |
| `has no beads database yet` | `sc status <bead>` was asked about a repository that has never been fired at. | Fire a run first, or `cd` to the right repository. |
| `bead not found` | No bead has that id: a typo, or a bead from another repository. | `sc status` lists this run's beads. |
| `bd rejected the request` | `bd` refused the input — an unknown status or type, a priority out of range, an empty title. | The message carries `bd`'s own text; fix the value it names. |
| `bead is blocked by open issues` | A bead was closed while its blockers are still open, which D-20 forbids. | Close the blockers first; `bd dep tree <id>` shows them. |
| `bead is already claimed by another actor` | Two agents raced for the same bead and this one lost. | Nothing: the scheduler picks other work. If it persists, a stale lease from a killed run is the usual cause — `--resume` recovers those. |
| `bead is not claimable in its current status` | The bead is closed, or in a status the claim protocol does not take. | `sc status <bead>` shows the status; move it back to open in `bd` if it should be worked on. |
| `bead is no longer held by the agent` | An agent's lease was recovered, released or taken while it was working. | Usually harmless: the run re-fires the bead. Repeated lease loss means two `sc` processes are on one repository. |
| `beads database is locked` | Another program holds the embedded database. | Close the other `bd` (an editor plugin, a second shell) and try again. |
| `bd is in read-only mode` | A write was attempted through the read-only `bd` shim every agent gets (P3-09). Agents may read the tracker, never write it. | Nothing to fix: the orchestrator makes every bead change. If your own command hit this, run it outside an agent worktree. |
| `dependency would create a cycle` | A plan or a `bd dep add` would make two beads depend on each other. | Remove one edge; the product owner is asked to re-plan automatically. |
| `bd warned about the graph plan` | `bd`'s dry run printed a warning, usually a field it would silently drop. The plan is refused rather than applied lossily (D-24). | Read the warning in the log; it names the field. Re-planning usually clears it. |
| `dry-run edge count does not match the plan` | `bd` would not create every dependency the plan asked for. | Same as above: the plan is refused and re-planned; report it if it repeats. |
| `dry-run node count does not match the plan` | `bd` would not create every bead the plan asked for. | As above. |
| `bd did not return an id for every plan key` | The graph was applied but `bd` reported fewer ids than beads. | The run stops using that plan. Check the database with `bd list`; a half-applied plan is safe to close by hand. |
| `orchestrator: bead carries a status slop-cannon did not register` | A bead is in one of the repository's own custom statuses, so it belongs to another tool and `sc` will not move it (P6-12). | Nothing: it is deliberate. Move the bead to a status `sc` knows if you want it worked on. |

## Git, worktrees and the repository

Each bead gets its own worktree under the run directory; nothing is checked
out inside the target repository (D-23).

| Message | What happened | What to do |
|---|---|---|
| `gitx: not a git repository` | The path is not inside a git working tree. | As in the first table: point `sc` at a repository. |
| `gitx: repository has no commits` | `HEAD` is on an unborn branch, so there is no base to branch from. | Make the first commit. |
| `gitx: HEAD is detached` | The repository is not on a branch. | `git switch <branch>` before firing. |
| `gitx: cannot determine the default branch (no remote HEAD, main, or master)` | There is no remote `HEAD`, and no `main` or `master` to fall back on. | `git remote set-head origin --auto`, or create the branch delivery should target. |
| `gitx: repository has no remote` | Something that needs a remote — a push, a pull request — was asked of a repository with none. | Add a remote, or run with `--safety off` so delivery merges locally. |
| `gitx: target repository has uncommitted changes` | Worktrees are made from a clean tree, so your uncommitted work cannot be carried into one. | Commit or stash first. Agents build on whatever is committed. |
| `gitx: unknown revision` | A branch, tag or commit could not be resolved. | Fetch, or check the name. |
| `gitx: revisions have no common ancestor` | Two branches share no history, so no diff base exists. | Usually a re-cloned or grafted repository; give the bead a branch that descends from the default one. |
| `gitx: worktree already exists` | A worktree for the bead is registered already, from an earlier run. | `--resume` reattaches it; otherwise remove it with `git worktree remove`. |
| `gitx: worktree is missing` | The bead's worktree was deleted underneath the run. | `--resume` recreates what it can; the branch still holds the commits. |
| `gitx: worktree has uncommitted changes` | Cleanup was asked to remove a worktree that still has work in it, and would not throw it away. | Inspect it under `runs/<run-id>/wt/`, commit or discard, then let cleanup run again. |
| `gitx: worktree path is occupied by something else` | The path exists but is not a worktree — a leftover directory or a file. | Delete the path under the run directory and retry. |
| `gitx: branch is already checked out elsewhere` | The bead's branch is checked out in another worktree, possibly one of yours. | `git worktree list` finds it; remove it or switch that worktree to another branch. |
| `gitx: refusing to nest a worktree inside another worktree` | `sc` was run from inside a linked worktree, or asked to create one inside another. | Fire at the main working tree. |
| `gitx: worktree directory must not be inside the target repository` | The run directory resolved to a path inside the repository, which D-23 forbids. | Leave `XDG_STATE_HOME` at its default, or set it to a path outside the repository. |

## Delivery: pull requests and merges

With safety on every bead is delivered as a pull request; with safety off it
is rebased and fast-forwarded onto the default branch (D-10).

| Message | What happened | What to do |
|---|---|---|
| `orchestrator: bead is not in a deliverable state` | Delivery was asked for a bead that is not approved, waiting for a human, or already being delivered. | Nothing: the run delivers a bead when its reviewers approve it. |
| `orchestrator: delivery handoff has no path for this safety setting` | Safety is on but no pull-request path was built, or off with no merge path. Usually a `gh` that could not be reached at assembly time. | `sc doctor` — with safety on, delivery needs `gh` installed and authenticated. |
| `gitx: bead branch does not exist` | The bead's branch is gone, so there is nothing to deliver. | The bead is re-fired; if it repeats, check nothing outside `sc` is pruning `sc/` branches. |
| `gitx: branch has no commit to deliver` | The engineer's branch carries no commit past its base, so closing the bead would ship nothing (D-20). | Read the agent log: the session usually failed or wrote nothing. The bead goes back for another round. |
| `gitx: push rejected by the remote` | The remote refused the push: a protected branch, or someone else pushed first. | Check branch protection rules, then let the delivery retry. |
| `gitx: rebase stopped on conflicts` | The bead's branch conflicts with the default branch. | Nothing by hand: `sc` fires a conflict round and the engineer resolves it in the worktree. |
| `orchestrator: the worktree is mid-rebase` | Something was asked of a worktree that is stopped in a rebase, and doing it would have committed the conflict markers. | Let the conflict round finish; to intervene, resolve the rebase in `runs/<run-id>/wt/<bead-id>` by hand. |
| `orchestrator: no conflict round is pending for this bead` | A conflict resolution arrived for a bead that has none outstanding. | Nothing: a duplicate answer, already logged. |
| `gitx: check command failed` | The repository's `check_command` exited non-zero on the rebased branch, so the merge did not happen. | Run the same command yourself; the failure is real. `check_command` is configured in [config.md](config.md). |
| `gitx: local default branch has diverged from its remote` | The local default branch and its remote each have commits the other lacks, so neither a fast-forward nor a push can work. | Reconcile them yourself (`git pull --rebase` on a clean tree), then resume. |
| `gitx: default branch cannot be fast-forwarded` | The default branch moved while the delivery ran. | Nothing: the delivery rebases and retries. |
| `gitx: rebased branch still contains a merge commit` | Fast-forwarding would have put a merge commit on the default branch, which safety-off delivery never does. | Rebase the bead's branch to a linear history, or deliver it as a pull request with safety on. |
| `gitx: worktree is not on the bead's branch` | The bead's worktree was switched to another branch, probably by hand. | `git switch` it back to the `sc/` branch the message names. |
| `gitx: no verdicts to post` | A review was posted with no verdicts in it. | Nothing to do: nothing was written to the pull request. |

## GitHub (`gh`)

`gh` is only needed for safety-on delivery and for reviewing pull requests
`sc` did not open (D-11).

| Message | What happened | What to do |
|---|---|---|
| `gitx: gh is not installed` | The GitHub CLI is not on `PATH`. | Install it, or run with `--safety off` to merge locally instead. |
| `gitx: gh is not authenticated (run gh auth login or set GH_TOKEN)` | `gh` is installed but not logged in to the host of this repository's remote. | `gh auth login`, or export `GH_TOKEN`. |
| `gitx: no git remote points at a GitHub repository` | None of the remotes maps to GitHub, so no pull request can be opened. | Add a GitHub remote, or use `--safety off`. |

## Agents and their answers

Every agent must end its session with a structured `sc-result` block; the
transcript and the raw output are kept under `runs/<run-id>/agents/`.

| Message | What happened | What to do |
|---|---|---|
| `structured: no sc-result block in output` | The session ended without the result block the contract asks for — usually a model that answered in prose, or a CLI that truncated its output. | The firing is retried. If it repeats, check the cartridge's model: a smaller model follows the contract less reliably. |
| `structured: sc-result block is truncated` | The block started and never closed: the session was cut short. | As above; a timeout or a killed process is the usual cause. |
| `structured: sc-result block is not valid JSON` | The block closed but its body is not JSON. | Read the raw output in the agent log; the retry usually succeeds. |
| `structured: result does not match schema` | The JSON is well formed but breaks the contract — a missing field, a value outside its enum. | The agent log records what was sent and what the schema wanted. Persistent failures belong in a bug report. |
| `structured: unknown contract` | A result was decoded against a contract name that is not registered. | An `sc` bug rather than a user problem; report it with the run id. |
| `agents: plan rejected` | The product owner's plan was schema-valid but broke a planning rule: too many beads, a duplicate of an open bead, dangling dependencies, a cycle, or empty acceptance criteria. | `sc` asks it to re-plan. Raising `max_beads_per_plan` helps when the brief is genuinely large. |
| `agents: plan proposes no beads` | The plan is empty. In autopilot that is a legitimate answer — nothing worth doing right now (D-18). | In brief mode, give a more concrete brief. In autopilot, nothing. |
| `orchestrator: planning: the product owner could not produce a usable plan` | Brief mode gave up after the configured number of rejected or failed plans. | Rewrite the brief in smaller, concrete terms, or raise `limits.max_plan_rejections` and `limits.max_plan_failures`. |
| `agents: prompt is over its token budget` | Even after dropping everything droppable, the assembled prompt exceeds the budget. | Split the bead, or trim the vibes and rules files: they are sent to every agent. |
| `agents: cartridge has no tool controls and the copied-worktree fallback is not implemented` | A cartridge that cannot restrict its own tools was selected, and the degrade that would contain it is not built (P5-06). | Use Claude, the cartridge that ships today (D-19). |
| `orchestrator: a rules-file violation is a hard constraint` | A verdict, an arbitration or a transition would have settled or shipped a `rules` violation in the engineer's favour. Rules files are not negotiable (D-08, P9-06). | Fix the code so the rule is met, or change the rules file if the rule itself is wrong. |
| `orchestrator: agent panicked` | An agent's own goroutine panicked. The run survives it and re-fires the work. | Report it with the run id: this is an `sc` bug, and the stack is in the run log. |
| `orchestrator: the agent session succeeded but the run could not record its result` | The model answered, and the bookkeeping after it — a bead update, a message — failed. The firing is never retried, so the work is not repeated blindly. | Read the log line under it: it names the failure, usually `bd` or git. |
| `proc: timed out` | A subprocess `sc` runs (`git`, `bd`, `gh`, a cartridge CLI) outlived its timeout and was killed. | Run the same command by hand; a slow network or a hung tool is the usual cause. |
| `proc: killed` | A streamed subprocess was stopped on purpose. | Nothing. |

## The run itself

| Message | What happened | What to do |
|---|---|---|
| `orchestrator: run has finished` | Something was asked of a run that has already ended: a late key press, a slow component reporting in. | Nothing. `--resume` starts the next run from where this one stopped. |
| `orchestrator: invalid bead state transition` | An event arrived for a bead in a state that event cannot act on (D-07). It is logged as a warning, never a crash. | Usually a duplicate answer from a re-fired agent. Persistent ones are worth reporting with the bead id. |
| `orchestrator: the bead's current state could not be read` | On `--resume`, `bd` could not say what state a bead is in, so nothing was written for it. | Check `bd` works in the repository (`sc doctor`), then resume again. |
| `orchestrator: the supervisor has stopped; no new agents are fired` | The run is shutting down: you stopped it, or a limit ended it. In-flight agents finish, nothing new starts. | Nothing. `--resume` continues the work. |

---

If none of this matches, `sc status --json` and the run's `events.jsonl` are
the full record of what happened, and `sc doctor --json` is the full record
of what the machine looks like. Both belong in a bug report, with the `sc`
version from `sc version`.
