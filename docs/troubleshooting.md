# Troubleshooting

Every row below is keyed by words `sc` can actually print, exactly as they
appear in the output — not by the name the code knows the failure under.
Find the words you saw, read what happened, do the thing in the last
column. Errors are the first eleven sections; the warnings a run prints
while it carries on are under [Warnings the run prints](#warnings-the-run-prints),
and what `sc doctor` itself says is under
[What `sc doctor` prints](#what-sc-doctor-prints). A handful of tests in
`internal/cli` keep the page honest, and they are worth knowing about,
because they are what makes this a complete list rather than a sample:

- `TestTroubleshootingMessagesStillExistInTheCode` — every key is still
  text the code builds, so a reworded error breaks this page instead of
  leaving you searching for a string that no longer exists.
- `TestTroubleshootingCoversEveryMessageTheCommandLayerBuilds` — every
  message `internal/cli` and `internal/config` build, each `fmt.Errorf` and
  not only the named sentinels, matches a row here or is exempted in the
  test with a written reason. Those two packages are where the words you
  read are written: the command layer prints what it returns as
  `sc: <message>`, and the config package reads the files you edit.
- `TestTroubleshootingCoversEveryUserFacingSentinel` — every exported
  sentinel error of the packages underneath (`beads`, `gitx`, `cartridge`,
  `agents`, `orchestrator`, `proc`, and every package under them) is
  documented or exempted the same way. What reaches you from those is the
  sentinel's own sentence, printed in front of the tool's own words: `bd
  show: bead not found: no issues found matching the provided IDs`.
- `TestTroubleshootingCoversEveryMessageTheWrappedPackagesBuild` — and every
  *other* message those six packages build, each `fmt.Errorf` as well as
  each sentinel, matches a row here or is exempted with a written reason.
  That is where most of a run's failures are written — every run-lock
  refusal, every delivery step, everything `bd`, `git` and `gh` are driven
  through — so a new sentence in any of them now has to be decided about
  rather than shipped unread. Most of the exemptions say one of three
  things: the wrapped sentinel is what you read and this is only its detail,
  the message names a step and hands on the error under it, or the call is
  one `sc` assembles itself and reaching it is a bug in `sc`.
- `TestTroubleshootingCoversEveryWarningTheRunRaises` — every warning the
  run can raise is documented or exempted the same way. A warning is not an
  error — the run carries on — but it is what says a bead has stopped being
  worked on, and it was outside the audit until round 4, because warnings
  are built with `fmt.Sprintf` rather than `fmt.Errorf`.
- `TestTroubleshootingCoversTheDoctorRowsOwnWords` — every detail an
  `sc doctor` row carries, which is also what a refused run prints as its
  numbered fix list, is documented or exempted as a passing row's words.
- `TestTroubleshootingSendsNobodyToAFileNothingWrites` — no remedy here may
  send you to a file nothing writes.
- `TestTroubleshootingExemptionsAreReal` and
  `TestTroubleshootingWarningAndDetailExemptionsAreReal` — an exemption
  cannot outlive the message it excuses, or quietly cover one this page now
  documents.
- `TestTroubleshootingLinksAreLocalFiles` and
  `TestTroubleshootingIsReachableFromTheReadme` — the pages this one points
  at exist, and something points at this one.

**Start here, whatever the message.** `sc doctor` checks git, `bd`, `gh`, the
cartridge CLIs, the repository and the bead database in one pass, prints a
hint under every row that is not `pass`, and exits 1 when anything failed.
When a run refuses to start, `sc` prints the failing rows itself as a
numbered fix list — the same words, from the same check — and then suggests
`sc doctor` for the rest.

**Exit codes.** `0` the run finished, was stopped, or ended on a budget you
set (`budget_reached`), `1` the run failed or a command was refused, `3`
every cartridge is out of credits. The full contract, and what `--headless`
prints beside it, is in [prompts.md](prompts.md#exit-codes).

**Where things are.** Nothing `sc` writes lives inside the target repository:
logs, prompts, events and worktrees go under
`$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/<run-id>/` (see
[cli.md](cli.md)), and the first run that completes in a repository prints
the paths. `sc status` reads the same files back, and `sc status --json`
gives them to a script. The one thing `sc` may write inside the repository is
`.slop-cannon/config.toml`, and only when a config command created it.
[prompts.md](prompts.md#the-debug-copy-of-every-prompt) maps a run directory
file by file, including the copy of every prompt an agent was fired with.

---

## Starting a run

| Message | What happened | What to do |
|---|---|---|
| `is not a git repository (no .git in it or any parent directory)` | The path given to `sc` is not inside a git working tree, and `sc` only fires at repositories. | `git init`, or point `sc` at the right path: `sc /path/to/repo`. |
| `no such directory` | The path given to `sc` does not exist, and `sc` only fires at a git repository that is already there. | Check the spelling; a relative path is resolved against the current directory. Then name the repository: `sc /path/to/repo`. |
| `not a directory` | The path exists but is a file, and `sc` fires at a repository rather than at one of its files. This is also what `--repo` says when it is given a file. | Name the repository directory: `sc /path/to/repo`, or `--repo /path/to/repo`. |
| `names a run to continue, so it needs --resume` | `--run ID` was given to `sc PATH` without `--resume`. It only picks which earlier run to continue. | `sc resume --run ID`, or `sc PATH --resume --run ID`. |
| `has no state.json (sc status lists the runs)` | `sc resume --run ID` named a run that does not exist, or one that was killed before it recorded anything, so there is nothing of it to continue. | `sc status --run ID` or the run directories under the state directory show which runs exist; resume one of those, or start a new run. |
| `could not lower the run's priority` | The run could not set its niceness (`nice` in the config), usually because the system refused it. The run fires anyway at normal priority. | Nothing, or set `nice = 0` to stop trying. |
| `must be between 0 (leave the priority alone)` | `nice` in a config file is negative or above 19. A negative niceness would need privileges and would raise the run above the rest of the machine. | Use a value from `0` (leave the priority alone) to `19` (the lowest). |
| `must not be empty` | A flag that needs a value was given an empty one: `-m ""`, `--cartridge ""`, `sc vibes --add ""`. | Give it a value, or leave the flag out and keep the configured default. |
| `must not be negative` | `sc tui-demo --delay` was given a negative pause between events. | Use `0` for no pause, or a duration such as `--delay 40ms`. |
| `a resumed run keeps its original brief` | `-m` was combined with `--resume`. A resumed run continues the beads the earlier run planned, so a new brief would have nowhere to go. | Drop `-m` to resume, or drop `--resume` to start a new run from the brief. |
| `the TUI needs a terminal: stdin and stdout are not a TTY` | The full-screen interface was asked for from a pipe, a CI job, or an editor's task runner. | Add `--headless` for log lines, or `--headless --json` for one JSON envelope per event. |
| `tui-demo needs a terminal: stdin and stdout are not a TTY` | The same, for `sc tui-demo`. | Run it in a terminal, or use its `--json` output. |
| `watch needs a terminal: stdin and stdout are not a TTY` | `sc watch` draws the run TUI, and it was asked for from a pipe, a CI job or an editor's task runner. | Run it in a terminal. `sc status` and `sc logs --follow` report the same run as text. |
| `prints the scripted stream; a recording is already JSONL` | `sc tui-demo --events` was combined with `--replay`. The flag exists to print the demo's own scripted events; a recording already is that file. | Drop one of the two: `--events` for the script, or `cat` the recording `--replay` names. |
| `is already going in this repository` | Another `sc` process holds this repository's run lock. Only one run at a time per repository (P9-04), and a linked `git worktree` of the same repository counts as the same repository. | Wait for it to finish, or stop it: `sc stop` interrupts it from any terminal (P9-24), and Ctrl-C in the terminal it is running in does the same. `kill` the pid the message (and `sc status`) names if neither is possible. `--force` takes the lock, and only makes sense once you are sure the other run is gone. |
| `no run is going in` | `sc stop` was asked to stop a run in a repository whose run lock nothing holds. | Nothing is running there: `sc status` shows the last run. Check you named the right repository (`sc stop PATH`, or `--repo`). |
| `is not going any more` | `sc stop` found a run lock whose holder has died: the run was killed or crashed, and the lock is the only thing left of it. | Nothing to stop. The next run in that repository removes the lock with a `removed a stale run lock` notice; `sc resume` continues the run itself. |
| `which this machine cannot signal` | `sc stop` found a run lock written on another machine — a state directory on a shared file system — and cannot interrupt a process it cannot see. | Stop it on the machine the message names, or, once you are sure it is gone, start again here with `--force`. |
| `so the run holding it cannot be named` | `sc stop` could not parse the run lock, so it does not know which process to interrupt. | `sc status` shows what the run directory says. If nothing is running, delete the `run.lock` the message names. |
| `so it cannot stop itself` | `sc stop` found that the run lock names its own process. It only happens when a run and the stop share a process (an embedder, or a test). | Press Ctrl-C, or call `Run.Stop` in the embedding program. |
| `could not interrupt run` | The interrupt `sc stop` sent to the run's process was refused — usually a run started by another user. | Run `sc stop` as the user the run belongs to, or `kill` the pid `sc status` names from an account that may signal it. |
| `did not stop within` | `sc stop` interrupted the run and waited, and the run was still going when the wait ran out. A graceful stop lets the agents in flight finish their session, which can take minutes. | Wait longer (`sc stop --wait 10m`), or stop it immediately with `sc stop --now`, which interrupts twice and kills the agents where they stand. |
| `its output was closed` | A headless run was stopped because whatever its output was going to went away: `sc run --headless` piped into `head`, a pipe whose reader exited, a terminal that closed. It ends as a user stop, so the lock is released and the worktrees are cleaned (P9-20). | Nothing. If the run should have carried on, send the output somewhere that stays — a file, or `--json` into a reader that consumes all of it. |
| `removed an unreadable run lock` | A `run.lock` that could not be parsed was still there after the grace period (half a minute), so `sc` treated it as debris, removed it and carried on. | Nothing. It is a notice, not an error. If it repeats every run, something else is writing that file. |
| `the run lock names no process (pid` | The lock file is there but names no live process — a pid of zero, or a record written by a crashed run before it could stamp itself. | Nothing to fix by hand: `sc` treats it as stale and takes it over. `sc status` shows what the lock says. |
| `keeps being retaken` | `sc` removed a stale run lock and found another process had taken it in the same instant, several times over. Two `sc` runs are starting against one repository at once. | Start one run at a time. If nothing else is running, something is re-creating `<state-dir>/run.lock`; delete it while no run is going. |
| `--force: took the run lock from run` | `--force` was given while another run's lock was still in place, so `sc` took it. | Nothing, if you were sure that run had stopped. If it is still going, stop this one and let it finish: two runs at one repository fight over the same beads and branches. |
| `orchestrator: remove the stale run lock` | `sc` proved the lock's holder is gone but could not delete the file — usually a read-only state directory, or one owned by another user. | Check the permissions of `$XDG_STATE_HOME/slop-cannon/<repo-id>/`, then delete `run.lock` there by hand. |
| `cannot tell whether it is still alive` | The lock was written by another machine — a state directory on a shared file system — so this host cannot probe that process. | Check the run on the machine named in the message. If it really stopped, start again with `--force`. |
| `if no run is going, delete it and try again` | The lock file could not be parsed, and it is younger than the grace period, so `sc` waits rather than deleting what might be a run starting right now. | Wait half a minute and retry; if nothing is running, delete the file the message names. |
| `removed a stale run lock` | A previous run was killed or crashed. `sc` found its lock, proved the process is gone, removed it and carried on. | Nothing. It is a notice, not an error. |
| `orchestrator: another run holds the lock` | The lock check ran again as the run started and found a live holder. | As above: `sc status`, then wait, stop it, or `--force`. |
| `preparing the bead tracker` | The run could not open or create the repository's `bd` database (D-13). The rest of the line is `bd`'s own message. | `sc doctor` runs the same checks and prints a hint under each; the numbered fix list under the failure is that check. |
| `registering the bead statuses` | `bd` refused the custom statuses `sc` registers for the D-07 workflow. | Usually a `bd` older than 1.2.2, or a database another tool owns: `sc doctor`, then `bd status list` in the repository. |
| `resolving the default branch` | The branch delivery targets could not be worked out. | The line under it says which of the `gitx:` rows below it was: no commits, a detached `HEAD`, or no `main`/`master`. |
| `recovering the previous run's leases` | `--resume` could not read or release the leases the previous run left on its beads. | Check `bd` works in the repository (`sc doctor`); `bd list --status in_progress` shows what is still claimed. |
| `no run directory at` | `sc status` or `sc logs` was asked for a run whose directory has been deleted or pruned. | The runs still on disk are the directories under `$XDG_STATE_HOME/slop-cannon/<repo-id>/runs/`; `ls` it and pass one of those ids to `--run`. Without `--run` both commands read the most recent run, which is usually what you want. |
| `no runs recorded for` | `sc status`, `sc logs` or `sc watch` was pointed at a repository `sc` has never fired at, so there is no run to read. | Fire a run first, or point the command at the right repository with `--repo`. |
| `the run wrote no log yet` | `sc logs` found the run directory but no `sc.log` in it: the run was killed before it wrote its first line, or the directory was made by hand. | Read `events.jsonl` in the same directory, or start a new run. |
| `the run has no agent logs yet` | `sc logs` was given an agent or bead id, and the run has not fired its first agent (or was killed before it did). | Add `--follow`: it waits for the first agent log of that bead and starts printing when it appears (P8-38). `sc logs` with no argument tails the run's own log meanwhile. |
| `this run has:` | `sc logs` was given an id no agent log matches. The message lists the agent ids the run does have. | Copy one of the ids it lists, or pass the bead id — `sc logs sc-1` prints every agent that worked on that bead. |
| `a followed log has no end to encode` | `sc logs --follow` was combined with `--json`, and a stream that never ends cannot be one JSON document. | Pick one: `--json` for a snapshot a script can parse, `--follow` for a live tail. |
| `orchestrator: no state file` | The run directory exists but the run never got as far as its first state write. | Read `events.jsonl` in the same directory, or start a new run. |
| `orchestrator: no event log` | The run directory has no `events.jsonl`: the run was never started, or the directory was pruned. | Nothing to recover; start a new run. |
| `is not a whole number (example: sc ammo 12)` | `sc ammo` was given something that is not an integer. | `sc ammo 12`. |
| `ammo must be at least 3: one engineer, one tester, one product owner` | Fewer than three agents cannot run the loop at all. | Use 3 or more; the default is 10. |
| `unsupported shell` | `sc completion` was asked for a shell it does not generate. | Use one of the shells the message lists. |
| `needs a shell argument: one of` | `sc completion` was run without saying which shell to generate for. | Name one: `sc completion fish`. The message lists the shells it knows. |
| `accepts exactly one shell argument` | `sc completion` was given more than one argument. | One shell per run: `sc completion zsh > ~/.zsh/completions/_sc`. |
| `is not wired up yet (P7-20)` | A TUI run-control key (re-fire a bead, mark one done, change safety or ammo mid-run) that has no orchestrator seam yet. | Stop the run, change the setting on the command line or in the config, and fire again. |
| `the run has no delivery handoff` | A TUI key asked for a delivery step — re-running a pull request, opening a conflict round — in a run assembled without a delivery path. | Nothing mid-run: the run found no `gh` (safety on) or no merge path (safety off). `sc doctor`, then fire again. |

## Configuration files

`sc config show` prints the merged configuration and where each value came
from; the keys are documented in [config.md](config.md).

| Message | What happened | What to do |
|---|---|---|
| `use a Go duration such as` | A duration key was given a bare number or an unparseable string. | Quote it as a Go duration: `idle_replan_interval = "10m"`. |
| `safety must be on or off, got` | `--safety` or the `safety` key carries something else. | `on` opens pull requests, `off` merges to the default branch (D-10). |
| `schema_version must be a positive integer` | The config file's `schema_version` is missing, negative or not a number. | Remove the key to get the current default, or set it to the version in [config.md](config.md). |
| `schema_version must be an integer` | The `schema_version` key holds something that is not a number at all — a string, or a table. | Set it to the version in [config.md](config.md), or remove the key to get the current default. |
| `no migration to version` | The config file declares a `schema_version` this `sc` has no upgrade path from, usually a file written by a newer `sc`. | Upgrade `sc`, or set the key to the version in [config.md](config.md). |
| `bytes exceeds the` | A vibes or rules file is larger than the per-file limit. Every agent is given these files, so an oversized one would crowd the work out of the prompt. | Trim it, or point at the section that matters. |
| `is not supported yet` | A cartridge name that is not one `sc` ships an adapter for (D-29: `claude` and `cursor`). | `sc cartridge --add claude --model <model>` or `sc cartridge --add cursor --model auto`. |
| `is not configured` | The active cartridge names an entry no config file defines, usually after hand-editing or removing one. | Add it back, or `sc cartridge --use NAME` to pick one that exists. |
| `cartridges live in the global config only; use the global file` | A `[cartridges]` entry was written to a repository config. Cartridges are subscriptions and belong to the machine, not to a checkout (D-03). | Run the cartridge command without `--repo`, or move the block to the global file. |
| `cartridge name must not be empty` | `--add` or `--use` was given an empty name. | Name the cartridge: `sc cartridge --use claude`. |
| `--model requires --add NAME` | `sc cartridge --model <model>` was given on its own. A model belongs to the cartridge being added, so there is nothing to attach it to. | `sc cartridge --add claude --model <model>`; `sc cartridge --list` shows what is configured. |
| `nothing removed` | `sc vibes --remove` or `sc rules --remove` named a file that is not in that list. | `sc config show` lists the files that are; paths are compared after resolving them. |
| `not a regular file` | A vibes or rules path is a directory, a device or a socket. | Point at a file. |
| `not valid UTF-8 text` | A vibes or rules file is not text, so no agent could be given it. | Convert it to UTF-8, or point at the source document instead. |
| `contains NUL bytes; not a text file` | The same, for a binary file that happens to decode. | As above. |
| `no editor configured: set $EDITOR (for example: export EDITOR=vim)` | `sc config edit` has no editor to open. | Export `EDITOR`, or edit the file the message names by hand. |
| `agents: exclude pattern` | An entry of the `exclude` key is not a valid glob — usually an unclosed `[` class, as in `exclude = ["secrets/[a-z"]`. The run refuses to start rather than leave you believing a file is hidden when it is not (P9-05). | Fix or drop the pattern the message names; `exclude` takes [path.Match](https://pkg.go.dev/path#Match) globs such as `"*.key"`, `"secrets/*"` or `"deploy/**"`. |
| `needs safety = true: auto-merge merges the run's own pull requests, and safety off opens none` | `auto_merge` is on while `safety` is off, in the config files or through `--auto-merge` / `--safety off`. Auto-merge only ever merges a pull request the run opened itself, and safety off opens none (D-31). | Turn safety back on, or auto-merge off: `--auto-merge=false`, or `auto_merge = false` in the file the message names. |
| `auto_merge is on but safety is off` | The same, when the pair was put together on the command line. | As above. |
| `needs at least one auto_merge_checks name: without one, a pull request whose quick checks finish before its tests have even started reads as green` | `auto_merge` is on, in a config file, through `--auto-merge` or recorded by a run being resumed, but `auto_merge_checks` names no check. GitHub lists only the checks it has created so far, so with nothing required a pull request whose quick title check has passed while the CI run has not registered yet would read as green (D-31). | Name the checks that must pass, for example `auto_merge_checks = ["test"]` for a workflow job called `test`, and resume with `--auto-merge`; or turn auto-merge off with `--auto-merge=false`. |
| `auto_merge is on:` | The same, when `--auto-merge` turned it on. | As above. |
| `merges its own pull requests (auto_merge)` | A resumed run recorded `auto_merge` on and this invocation passed `--safety off`. The recorded setting is kept on a resume like safety itself (D-31), so the two would contradict each other. | Pass `--auto-merge=false` together with `--safety off`, or resume without `--safety`. |
| `add ".env" to the repository's .gitignore` | `sc doctor` found a repository whose `.gitignore` does not ignore dotenv files. It is a warning, not a refusal. | Add `.env` to `.gitignore`. Excluding a file from the prompts is only half the job: an engineer that commits a `.env` has leaked the credential whatever the prompt said. |

## Cartridges (the LLM CLI)

A cartridge is the CLI subscription `sc` fires: it is added once with
`sc cartridge --add`, and [cartridges.md](cartridges.md) describes the
adapters.

| Message | What happened | What to do |
|---|---|---|
| `no cartridge loaded` | The run resolved a configuration with no active cartridge. Nothing can be fired without one (D-04). | `sc cartridge --add claude --model <model>`, then fire again. |
| `cartridge: no active cartridge configured (add one with: sc cartridge --add claude --model <model>)` | The same, from the cartridge selector. | As above. The command is the one every other message names, so following any of them gets you to the same place. |
| `which is no longer configured` | A resumed run was started with a cartridge that has since been removed from the configuration, so the run cannot fire the agents it planned. | Add it back (`sc cartridge --add <name> --model <model>`), or start a new run instead of resuming. |
| `is out of credits` | Every configured cartridge reported it is out of credits or rate limited, so nothing can be fired. `sc` exits 3 for this and for nothing else. | Wait for the quota to reset, or add a cartridge on another subscription with `sc cartridge --add`. |
| `cartridge: unknown cartridge` | The configuration names a cartridge `sc` has no adapter for. | `sc cartridge --list` shows what is available; Claude and Cursor ship today (D-29). |
| `cartridge: not built` | The active cartridge, or a per-role override, names a cartridge that was not built for this run — usually a stale `[roles]` entry. | Fix the name in the config, or remove the role override. |
| `cartridge: not installed` | The cartridge's CLI is not on `PATH`. | Install it, or set `command` under `[cartridges.<name>]` to its absolute path. |
| `cartridge: not authenticated` | The CLI is installed but not logged in, or its token was rejected. | Run the CLI once yourself and log in (for Claude Code: `claude`), then fire again. |
| `cartridge: credits exhausted` | The subscription's quota, credits or billing are gone. That cartridge is dead for the rest of the run; when every one is dead, the run ends with exit code 3 (D-18). | Wait for the window to reset, or add another cartridge and `sc cartridge --use` it. |
| `cartridge: rate limited` | The CLI reported a temporary limit. `sc` backs off and retries; the cartridge stays alive. | Nothing, unless it repeats — then lower `ammo` so fewer agents run at once. |
| `cartridge: timed out` | A session outlived `limits.agent_timeout` and was killed. | Raise the limit in the config, or split the work into smaller beads. |
| `cartridge: crashed` | The CLI exited non-zero, died on a signal, or printed something the adapter could not parse. | `sc logs <bead-id>` prints what that session printed, and the run's own log has the exit status; running the same command by hand usually reproduces it. |
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
| `status: empty bead id` | `sc status ""` — the bead argument was empty, usually an unset shell variable. | Name the bead you want the timeline of (`sc status sc-3`), or drop the argument to see the whole run. |
| `not found in` | `sc status <bead>` was given an id this repository's bead database does not have: a typo, or a bead from another repository. | `sc status` lists the run's beads and `bd list` every bead; `--repo` points the command at another repository. |
| `bead not found` | No bead has that id: a typo, or a bead from another repository. | `sc status` lists this run's beads. |
| `bd rejected the request` | `bd` refused the input — an unknown status or type, a priority out of range, an empty title. | The message carries `bd`'s own text; fix the value it names. |
| `bead is blocked by open issues` | A bead was closed while its blockers are still open, which D-20 forbids. | Close the blockers first; `bd dep tree <id>` shows them. |
| `bead is already claimed by another actor` | Two agents raced for the same bead and this one lost. | Nothing: the scheduler picks other work. If it persists, a stale lease from a killed run is the usual cause — `--resume` recovers those. |
| `bead is not claimable in its current status` | The bead is closed, or in a status the claim protocol does not take. | `sc status <bead>` shows the status; move it back to open in `bd` if it should be worked on. |
| `bead is no longer held by the agent` | An agent's lease was recovered, released or taken while it was working, or the bead moved on (to `delivering`, say) before the agent's last renewal. An assignee of `sc:sc:<agent>` was a bug in `sc` before P6-62; a run on a newer `sc` writes `sc:<agent>`. | Usually harmless: the run re-fires the bead. Repeated lease loss means two `sc` processes are on one repository. |
| `beads database is locked` | Another program holds the embedded database. | Close the other `bd` (an editor plugin, a second shell) and try again. |
| `bd is in read-only mode` | A write was attempted through the read-only `bd` shim every agent gets (P3-09). Agents may read the tracker, never write it. | Nothing to fix: the orchestrator makes every bead change. If your own command hit this, run it outside an agent worktree. |
| `dependency would create a cycle` | A plan or a `bd dep add` would make two beads depend on each other. | Remove one edge; the product owner is asked to re-plan automatically. |
| `beads: dependency cycle` | `sc`'s own validation found the cycle before `bd` did, in a plan or in the graph it read back. The message names the beads, the first repeated at the end. | As above: the plan is refused and the product owner re-plans. |
| `bd warned about the graph plan` | `bd`'s dry run printed a warning, usually a field it would silently drop. The plan is refused rather than applied lossily (D-24). | Read the warning in the log; it names the field. Re-planning usually clears it. |
| `dry-run edge count does not match the plan` | `bd` would not create every dependency the plan asked for. | Same as above: the plan is refused and re-planned; report it if it repeats. |
| `dry-run node count does not match the plan` | `bd` would not create every bead the plan asked for. | As above. |
| `bd did not return an id for every plan key` | The graph was applied but `bd` reported fewer ids than beads. | The run stops using that plan. Check the database with `bd list`; a half-applied plan is safe to close by hand. |
| `orchestrator: bead carries a status slop-cannon did not register` | A bead is in one of the repository's own custom statuses, so it belongs to another tool and `sc` will not move it (P6-12). | Nothing: it is deliberate. Move the bead to a status `sc` knows if you want it worked on. |
| `(no real binary on PATH)` | `sc` went looking for the genuine `bd` (or `git`) to put behind the read-only shim every agent gets, and found only shims or nothing at all. | Install the tool the message names and make sure it is on the `PATH` `sc` itself runs with; `sc doctor` reports both. |
| `needs a POSIX shell, unsupported on` | The read-only `bd` shim and the git guard are `/bin/sh` scripts, so agents cannot be sandboxed on a platform without one (Windows). | Run `sc` under WSL, macOS or Linux. |
| `bd init in` | `sc` tried to create the bead database in the repository and `bd init` failed; the rest of the line is `bd`'s own message. | `sc doctor` checks `bd`; running `bd init` in the repository by hand shows the same failure with more context. |
| `malformed custom status` | The repository's `bd` configuration has a `status.custom` entry that is not `name:category`, so `sc` cannot read the D-07 registry it has to extend. | `bd config get status.custom` shows it; fix or remove the entry. |
| `is malformed, refusing to replace it` | The same entry, found while `sc` was about to write its own statuses. It is left alone rather than overwritten, because it is the repository's own configuration. | As above: fix `status.custom` by hand, then fire again. |
| `after setting status.custom=` | `sc` wrote the workflow statuses it needs (D-07) and `bd` did not list them back, so the tracker and `sc` disagree about what a bead may be. | Check the `bd` version (`sc doctor`; 1.2.2 or newer), then `bd config get status.custom`. |
| `(agents must be able to read beads)` | The read-only `bd` shim failed its own self-check: it refused a read an agent needs. The run is refused rather than fired with a shim that would block its agents. | It is a bug in `sc`; report it with the line, which names the command that was refused. |
| `was not refused` | The other half of that self-check: a write the shim must refuse went through to `bd`. The run is refused rather than fired with agents that could write the tracker (D-21). | As above — report it with the line. |

## Git, worktrees and the repository

Each bead gets its own worktree under the run directory; nothing is checked
out inside the target repository (D-23).

| Message | What happened | What to do |
|---|---|---|
| `gitx: not a git repository` | The path is not inside a git working tree. | As in the first table: point `sc` at a repository. |
| `gitx: repository has no commits` | `HEAD` is on an unborn branch, so there is no base to branch from. | Make the first commit. |
| `gitx: HEAD is detached` | The repository is not on a branch. | `git switch <branch>` before firing. |
| `gitx: cannot determine the default branch (no remote HEAD, main, or master)` | There is no remote `HEAD`, and no `main` or `master` to fall back on. | `git remote set-head origin --auto`, or create the branch delivery should target. |
| `which is not a pull request gate; leaving it alone` | A run resumed with safety off found the bead blocked by a gate that is not a `gh:pr` gate, so it is not delivery's to clear. | `bd gate list` shows it; clear it yourself if it should not block the bead. |
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
| `gitx: branch has no commit to deliver` | The engineer's branch carries no commit past its base, so closing the bead would ship nothing (D-20). When every tester's final verdict approved it, the bead is closed instead (see `closed with nothing to merge`); this is the case where they did not. | `sc logs <bead-id>` shows the session that should have written it: it usually failed or wrote nothing. The bead goes back for another round. |
| `closed with nothing to merge` | With safety off, an approved bead's branch had no commit, and every tester approved it against its acceptance criteria as the default branch stands: usually another bead already landed the same work. The bead is closed without a merge SHA. | Nothing. If the product owner keeps planning work that has already shipped, look at the duplicates in `sc status`. |
| `gitx: push rejected by the remote` | The remote refused the push: a protected branch, or someone else pushed first. | Check branch protection rules, then let the delivery retry: it tries five times, waiting longer each time, and hands the bead to a person if the push keeps failing (`a delivery failed too many times`). |
| `gitx: rebase stopped on conflicts` | The bead's branch conflicts with the default branch. | Nothing by hand: `sc` fires a conflict round and the engineer resolves it in the worktree. |
| `orchestrator: the worktree is mid-rebase` | Something was asked of a worktree that is stopped in a rebase, and doing it would have committed the conflict markers. | Let the conflict round finish; to intervene, resolve the rebase in `runs/<run-id>/wt/<bead-id>` by hand. |
| `orchestrator: no conflict round is pending for this bead` | A conflict resolution arrived for a bead that has none outstanding. | Nothing: a duplicate answer, already logged. |
| `gitx: check command failed` | The repository's `check_command` exited non-zero on the rebased branch, so the merge did not happen. | Run the same command yourself; the failure is real. `check_command` is configured in [config.md](config.md). |
| `gitx: local default branch has diverged from its remote` | The local default branch and its remote each have commits the other lacks, so neither a fast-forward nor a push can work. | Reconcile them yourself (`git pull --rebase` on a clean tree), then resume. |
| `gitx: default branch cannot be fast-forwarded` | The default branch moved while the delivery ran — something outside the run pushed to it, since `sc` never delivers two beads at once. | Nothing: the delivery rebases and retries, up to five times. |
| `gitx: rebased branch still contains a merge commit` | Fast-forwarding would have put a merge commit on the default branch, which safety-off delivery never does. | Rebase the bead's branch to a linear history, or deliver it as a pull request with safety on. |
| `gitx: worktree is not on the bead's branch` | The bead's worktree was switched to another branch, probably by hand. | `git switch` it back to the `sc/` branch the message names. |
| `could not confirm pull request #` | With safety on, a pull request already exists for the bead's `sc/` branch, but `gh` stopped answering as logged in (or disappeared) while the run judged whose it is. The run does not push onto a pull request it cannot show is its own, so the bead is handed to a person. | Fix `gh` (`gh auth status`) and retry the bead. If the pull request is not the run's, close it or move it off the branch first. |
| `is not this run's own` | With safety on, a pull request already exists for the bead's `sc/` branch and it fails the ownership check (D-31): it was opened before the run started, by somebody else, without the `slop-cannon` label or the run's marker, against another base, or it is not the number the bead recorded. The run neither pushes to its branch nor reuses or merges it, and the bead is handed to a person. The rest of the message names the clause that failed. | Look at the pull request. Close it (or move it off the branch) and retry the bead, or merge the work yourself. |
| `gitx: the bead's branch and its pull request's head have diverged` | Auto-merge was about to send the bead back to its engineer (a failed check or a conflict), and the pull request's head on GitHub and the bead's local branch both have commits the other lacks — usually somebody pushed to the pull request by hand. Nothing was moved; the bead is with a person. | Bring the two together yourself (merge the pull request's head into the bead's branch in its worktree, `runs/<run-id>/wt/<bead-id>`), then retry the bead. |
| `the pull request at` | The same, with both commits named. | As above. |
| `checked out, not` | The bead's worktree is on another branch than the bead's own, so its branch could not be brought up to the pull request's head. | `git switch` the worktree back to the `sc/` branch the message names. |
| `gitx: the pull request's head` | The pull request's head commit could not be fetched from the remote while the bead's branch was being brought up to it. | Usually the network: the next poll tries again. |
| `orchestrator: auto-merge needs a run scope, a bead reader, the default branch, the run's start, gh, the branches, a ledger and an outcome applier` | The run was started with `auto_merge` on, but a part the merge is guarded by could not be assembled (D-31): the run id, the bead reader, the default branch, the run's start time, `gh`, the pull request path, `state.json` or the conductor. The run refuses to start rather than merge with a guard missing. | It is a bug in `sc`, not your configuration: report it. Until it is fixed, run with `--auto-merge=false` and merge by hand. |
| `gitx: no verdicts to post` | A review was posted with no verdicts in it. | Nothing to do: nothing was written to the pull request. |

## GitHub (`gh`)

`gh` is only needed for safety-on delivery and for reviewing pull requests
`sc` did not open (D-11).

| Message | What happened | What to do |
|---|---|---|
| `gitx: gh is not installed` | The GitHub CLI is not on `PATH`. | Install it, or run with `--safety off` to merge locally instead. |
| `gitx: gh is not authenticated (run gh auth login or set GH_TOKEN)` | `gh` is installed but not logged in to the host of this repository's remote. | `gh auth login`, or export `GH_TOKEN`. |
| `gitx: no git remote points at a GitHub repository` | None of the remotes maps to GitHub, so no pull request can be opened. | Add a GitHub remote, or use `--safety off`. |
| `external pull request review stopped` | The reviewer (D-11) gave up polling GitHub: `gh` is missing, not logged in, or no remote points at GitHub. Polling again cannot fix any of those, so the rest of the run carries on without it. | Fix `gh` (`sc doctor` says which of the three it is), or run with `--review-prs=false` to stop asking. |
| `orchestrator: pr checkout: fetching #` | The pull request's head could not be fetched — neither its `pull/<n>/head` reference nor its head branch. A fork with the pull-request references disabled, or the network being down. | Check `git fetch origin pull/<n>/head` by hand. The review is retried on the next poll; if it keeps failing the review bead is marked failed and the run says so. |
| `orchestrator: pr review context for` | The shared prompt context — the bead, the repository summary, the rules and vibes — could not be assembled for a review bead, so the lens was not fired. The rest of the line says which step failed. | Usually `bd` being unavailable for a moment (`sc doctor`). The review is retried on the next poll. |
| `orchestrator: pr checkout: moving` | The pull request was fetched but its worktree could not be moved onto it, usually because something else is holding the worktree. | Look in `<state-dir>/runs/<run-id>/wt/<bead-id>`; deleting that directory makes the next poll check the pull request out again. |
| `gitx: gh api user: unexpected login` | `gh api user --jq .login` printed something that is not a login. Auto-merge and the reuse of an existing pull request both compare a pull request's author with it (D-31), so nothing is reused or merged until it reads. | Run `gh api user --jq .login` yourself; a token that is not a user's (a GitHub App's) has no login, and with one the run cannot prove a pull request is its own. |
| `posting the review of pull request #` | `gh pr review` failed. The verdicts are safe: the review is retried on the next poll, three times in all. | Usually GitHub being unreachable — nothing to do. After the last attempt the review bead is handed to a human and the pull request is left untouched. |

## Agents and their answers


Every agent must end its session with a structured `sc-result` block. What
a session printed is in its own file under `<run-dir>/agents/`, which
`sc logs <agent-id>` and `sc logs <bead-id>` read back; the run's own log is
`sc logs` with no argument, and every event of the run is in `events.jsonl`
beside it.

Text a session printed can come back with `[REDACTED]` in it. That is not a
failure: `sc` masks anything shaped like a credential once, where the run
publishes, so the terminal, the transcripts, `sc.log`, `events.jsonl`, the
prompts of later agents, the comment on the bead and the pull request body
all carry the masked form.
[config.md](config.md#secrets-withheld-files-and-masked-values) lists the
rules, what they deliberately do not catch, and the `exclude` key that hides
whole files from an agent.

| Message | What happened | What to do |
|---|---|---|
| `structured: no sc-result block in output` | The session ended without the result block the contract asks for — usually a model that answered in prose, or a CLI that truncated its output. | The firing is retried. If it repeats, check the cartridge's model: a smaller model follows the contract less reliably. |
| `structured: sc-result block is truncated` | The block started and never closed: the session was cut short. | As above; a timeout or a killed process is the usual cause. |
| `structured: sc-result block is not valid JSON` | The block closed but its body is not JSON. | Nothing, usually: the firing is retried. `sc logs <bead-id>` shows the raw output the block was cut from. |
| `structured: result does not match schema` | The JSON is well formed but breaks the contract — a missing field, a value outside its enum. | The message lists the violations, one per line, and `sc logs <bead-id>` shows the JSON they were found in. Persistent failures belong in a bug report. |
| `structured: unknown contract` | A result was decoded against a contract name that is not registered. | An `sc` bug rather than a user problem; report it with the run id. |
| `the plan breaks a planning rule` | The product owner's plan was schema-valid but broke a planning rule: too many beads, a duplicate of an open bead, dangling dependencies, a cycle, or empty acceptance criteria. These are the words a rejected plan prints, on the end of `structured: result does not match schema (plan)`, with the rules it broke listed under it, one per line. | `sc` asks it to re-plan. Raising `max_beads_per_plan` helps when the brief is genuinely large; the duplicate and cycle rules are about the beads already in `bd`, so `bd list` shows what the plan collided with. |
| `agents: plan rejected` | The same rejection under the name the planner matches it by. It is not usually what you read — a rejected plan reaches you as the row above — but it is what a warning's `error` field carries in `sc.log` and `events.jsonl` when the plan was rejected as it was being applied. | As above. |
| `agents: plan proposes no beads` | The plan is empty. In autopilot that is a legitimate answer — nothing worth doing right now (D-18). | In brief mode, give a more concrete brief. In autopilot, nothing. |
| `the brief must be decomposed into at least one` | The same empty plan in brief mode, where it is not a legitimate answer: you asked for something and the product owner proposed no work. | Say what you want in concrete terms — a file, a behaviour, a bug — and fire again. |
| `beads, the limit is` | The plan has more beads than `limits.max_beads_per_plan` allows, so it is refused and re-planned. | Nothing, usually: the next plan is smaller. Raise `limits.max_beads_per_plan` in the config when the brief is genuinely large. |
| `orchestrator: planning: the product owner could not produce a usable plan` | Brief mode gave up after the configured number of rejected or failed plans. | Rewrite the brief in smaller, concrete terms, or raise `limits.max_plan_rejections` and `limits.max_plan_failures`. |
| `agents: prompt is over its token budget` | Even after dropping everything droppable, the assembled prompt exceeds the budget. | Split the bead, or trim the vibes and rules files: they are sent to every agent. |
| `is not allowed from an agent session` | An agent ran a git command the guard on its `PATH` refuses (P9-22). Agents work in their own worktree and commit there; `sc` pushes, opens the pull request and merges. The refused verbs are the ones that could move a branch outside that worktree — `push`, `pull`, `fetch`, `checkout`, `switch`, `clean`, `branch -D`, `config`, and anything git has grown since. | Nothing: it is working as intended, and the agent is told what it may run instead. You see it in `sc logs <bead-id>`, not on your own terminal. |
| `git may only be run inside` | The same guard, from the other side: an agent left its worktree — `cd` into the repository, say — and ran git there, where the default branch is the one checked out. | As above. |
| `agents: cartridge has no tool controls and the copied-worktree fallback is not implemented` | A cartridge that cannot restrict its own tools was selected, and the degrade that would contain it is not built (P5-06). | Keep that role on Claude, whose tool controls are native. Cursor has none (D-29), so its read-only firings run in a copy of the worktree, and a product owner fired without a worktree (planning, arbitration) cannot be copied. |
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
| `orchestrator: conductor: listing the run's beads` | The run-level reconciliation with `bd` could not list this run's beads, so that pass did nothing. | Nothing at once: the next pass, a second later, tries again, and the warning below it says the run carries on. If it repeats, `sc doctor` and `bd list` in the repository. |
| `orchestrator: conductor: listing the ready beads` | The same pass could not ask `bd` which beads are claimable, so no new work was offered that second. Whatever was already approved was still delivered. | As above. |
| `orchestrator: conductor: bd knows no bead` | A prompt was being built for a bead `bd` no longer has — usually one deleted by hand while the run was going. | Leave the run's beads to the run. `bd list` shows what is there; the firing is retried and the bead is dropped if it stays missing. |
| `orchestrator: conductor: worktree for` | The bead's worktree could not be created before its agent was fired. The rest of the line is git's own message. | The `gitx:` rows above name the usual causes (an occupied path, a branch checked out elsewhere). The firing is retried. |
| `orchestrator: conductor: committing` | D-14's leftovers commit — whatever the last engineer left uncommitted, committed before the testers read it — failed. | Look in `<state-dir>/runs/<run-id>/wt/<bead-id>`: a worktree stopped mid-rebase is handled on its own, anything else is git refusing, and the line carries its words. |
| `orchestrator: the supervisor has stopped; no new agents are fired` | The run is shutting down: you stopped it, or a limit ended it. In-flight agents finish, nothing new starts. | Nothing. `--resume` continues the work. |
| `orchestrator: this run has no budget to raise: neither limits.max_tokens nor limits.max_cost_usd is set` | The TUI's `b` key was pressed on a run with no cap, so there is nothing to raise. | Nothing to do while it runs — an uncapped run is never held. Set one of those keys in the config file to cap the next run ([config.md](config.md)). |
| `orchestrator: a new budget must be above what the run has already spent` | The number typed into the `b` prompt is under (or equal to) what the run has spent, so raising to it would stop the run again at once. The line says what has been spent. | Type a larger number. The prompt asks for the whole new ceiling, not the amount to add. |
| `was written by a newer sc (state version` | The run's `state.json` was written by a newer build of `sc` than the one reading it, so resuming it could misread the run. | Use the newer `sc`, or start a new run rather than resuming this one. |
| `belongs to run` | A state or lock file was read for one run and turned out to name another — usually two runs sharing a state directory, or a hand-edited path. | Let `sc` pick the run (`sc status` lists them) rather than pointing it at a directory by hand. |

### The run paused itself and nothing is happening

An autopilot run that crosses `limits.max_tokens` or `limits.max_cost_usd`
pauses instead of ending (D-18): it warns with a banner naming the cap, fires
nothing more, and waits for you. In the TUI, `p` resumes it — and resuming
grants another budget's worth, so it will pause again rather than run on
unwatched. There is no way to clear that pause in `--headless` mode: `sc
resume` restarts an *earlier* run from disk rather than the live one, so
stopping it is the answer: Ctrl-C in its own terminal, or `sc stop` from any
other one. Either finishes the run as a user stop, the banner naming the cap
stays in the run log, and the summary still reports what was spent.
Until then the run holds the repository's run lock, which is why a second
`sc` in the same repository is refused (see `sc status`).

A brief run never pauses over its budget: it stops starting new work, lets the
reviews already in flight finish, and ends with the budget in its summary
(see [config.md](config.md) for both keys).

Restarting the same run with `--resume` does not hand it a fresh budget. The
spend is kept in the run's `state.json`, so a resumed run carries what the
earlier invocations already spent; a resume over a crossed cap grants one more
budget's worth, exactly as clearing a pause in the TUI does, and then stops
again. If a run really needs more than that, `b` in the TUI raises the cap of
the live run; otherwise raise `limits.max_tokens` (or `limits.max_cost_usd`)
and start a new run — a resumed run behaves by the configuration it was
started with (P6-11).

## Warnings the run prints

A warning is not an error: the run carries on. It is what `sc` says when
something degraded — a bead abandoned, a plan dropped, a step skipped — and
it reaches `sc.log`, `events.jsonl`, the TUI's log pane and `sc status`
alike, tagged with the component that raised it (`planning`, `scheduler`,
`delivery`, `gates`, `pr-review`, `supervisor`, `run`, `limits`, `rules`).
Most of them need nothing from you; the ones that do are here.
`TestTroubleshootingCoversEveryWarningTheRunRaises` holds this list to
every warning the code can raise, so a new one is documented here or
exempted in the test with a written reason.

### Planning

| Message | What happened | What to do |
|---|---|---|
| `the product owner's plan broke a planning rule and was dropped` | An autopilot planning round produced a plan the validator refused, and nothing was created. The count in the message is how many in a row. The rule it broke is on the `the plan breaks a planning rule` row under [Agents and their answers](#agents-and-their-answers). | Nothing at once: the next round re-plans. If it repeats, the brief or the repository is the cause — `max_beads_per_plan`, or a backlog of open beads every plan duplicates. |
| `the planning product owner failed` | The planning session itself failed — a crashed CLI, a timeout, no result block — rather than producing a bad plan. | `sc logs` shows the session. Repeated failures are usually the cartridge: `sc doctor`. |
| `giving up on this round of planning; the next one is at` | Autopilot hit `limits.max_plan_rejections` or `limits.max_plan_failures` for this round, so it dropped it and backed off. The next attempt is at the time in the message. | Nothing: autopilot never stops (D-18). To make the wait shorter, lower `idle_replan_interval`. |
| `the signals command gave no reading; planning goes ahead without production evidence` | `signals_command` exited non-zero, timed out (`limits.signals_timeout`) or did not start (D-32). The error is the exit status and the last line it wrote to standard error. The plan went ahead with a one-line note instead of the evidence, and the same kind of failure (exit status, timeout, did not start) is not reported again until it changes; a reading cut short by the run stopping is not reported. | Run the command yourself in the repository root; it gets your environment, so an expired token or a missing CLI shows up the same way. `sc status` shows the last reading's exit code and when the next one is due. |
| `the brief could not be turned into beads` | Brief mode gave up: every plan it got was rejected or failed. The run ends rather than idling. | Rewrite the brief in smaller, concrete terms, or raise `limits.max_plan_rejections` and `limits.max_plan_failures`. |
| `is over its token budget` | A prompt — the planning one, a review, a feedback round — did not fit the budget even after everything droppable was dropped. Planning fires it anyway; a review round says so and carries on. | Trim the vibes and rules files (every agent is given them), or split the bead. |

### Agents and the scheduler

| Message | What happened | What to do |
|---|---|---|
| `firings failed in a row; it will not be offered an agent again` | The scheduler gave up on one bead's work after `limits.max_firing_failures` consecutive failures, moved it down the D-07 failed edge and stopped offering it. **This is why a bead stops being worked on.** The bead keeps whatever the last agent left behind. | `sc logs <bead-id>` shows the sessions that failed. Fix the cause, then `--resume`: a resumed run offers the bead again. |
| `agent failed; retrying` | One firing failed and is being retried. | Nothing unless it repeats into the give-up above. |
| `could not work out what to schedule` | The scheduler could not read the work queue this tick, usually because `bd` failed. | `sc doctor`. The tick is retried. |
| `stopped renewing the lease` | An agent's claim on its bead could not be renewed, so another actor may take it. | Usually harmless. Persistent lease loss means two `sc` processes are on one repository. |
| `leaving them to the supervisor` | The scheduler stopped while agents were still running; the supervisor is stopping them. | Nothing. |

### Cartridges

| Message | What happened | What to do |
|---|---|---|
| `is out of credits and is retired for this run (D-18)` | That cartridge is dead for the rest of the run. When the last one goes, the run ends with exit code 3. | Add another cartridge and `sc cartridge --use` it, or wait for the quota window. |
| `is rate limited; pausing it for` | The CLI reported a temporary limit, so `sc` is holding that cartridge for the time in the message. | Nothing, unless it repeats — then lower `ammo`. |

### The run and its shutdown

| Message | What happened | What to do |
|---|---|---|
| `agent event(s) were dropped: the run log could not keep up with the agents' output` | Agents printed faster than the run could publish, so some output never reached `events.jsonl` or the TUI. The work is unaffected; the record of it has a hole. | Nothing to fix mid-run. If you need the whole transcript, the per-agent files under `<run-dir>/agents/` are written directly and are not dropped. |
| `the agent event forwarder did not drain in time` | The same queue was still draining when the run shut down, so its last events were abandoned. | As above. |
| `stopping immediately, agents will be killed` | A second Ctrl-C (or a second signal) arrived while the run was already stopping, so it stopped waiting for the agents. | Nothing. Worktrees and the run lock may be left behind; the next run's stale-lock notice clears the lock. |
| `some agents could not be stopped cleanly` | One or more sessions did not exit when asked and were left to the operating system. | Check for stray cartridge processes if the machine feels busy afterwards. |
| `a helper did not stop; ending the run without it` | A background helper (the gate poller, the pull request reviewer) outlived the shutdown deadline. | Nothing: the run has already finished. |
| `the worktree cleanup did not run` | The run ended without removing its worktrees, usually because it could not list its beads. | Remove them by hand: `git worktree list`, then `git worktree remove <path>` for the ones under `<state-dir>/runs/<run-id>/wt/`. |
| `some worktrees could not be cleaned up` | Cleanup ran and some worktrees survived it — usually ones with uncommitted work in them (see `gitx: worktree has uncommitted changes`). | As above; inspect before removing. |
| `could not release the run lock` | The run finished but its `run.lock` is still on disk. | Nothing: the next run proves the process is gone and removes it. If it does not, delete `run.lock` under `<state-dir>` by hand. |
| `could not stamp the run lock` | The lock was taken but its details (the run id, the pid) could not be written into it. | Nothing. Another `sc` will treat it as unreadable and wait out the grace period rather than take it. |
| `a run-level sync with bd failed; the run carries on and retries` | The periodic reconciliation with `bd` failed once. | Nothing unless it repeats: then `sc doctor` and `bd list` in the repository. |
| `a delivery failed too many times; the bead was handed to a person` | A bead's delivery was tried five times — the message names how many and why the last one failed — and every attempt failed the same way, so `sc` stopped retrying and moved the bead to `needs_human`. The usual causes are `gh` not being authenticated, a remote that keeps refusing the push, and `bd` refusing to close a bead whose merge has already landed. | Look at the reason in the message, then at the repository: with safety off the merge may already be on the default branch, in which case only the bead is behind. `sc status` shows where the bead is; fix the cause and put it back in `approved` (`bd update <id> --status approved`) and the next pass delivers it again. |
| `a delivery failed too many times and the bead could not be handed to a person; it is where bd left it` | The same give-up, with `bd` refusing the move to `needs_human` as well. | `sc status <bead-id>` says where the bead is; check `bd` works (`sc doctor`), then move it yourself. |
| `a delivery is still running after the conductor stopped` | The run stopped while a pull request or merge was in flight. | Check the repository: the branch is there, and the pull request may have been opened. `sc status` and `gh pr list` say which. |
| `could not open the agent log` | A per-agent transcript under `<run-dir>/agents/` could not be written. | Check the state directory's permissions; the run's own `sc.log` still has the session's events. |
| `and a contained session must not reach it (P6-20)` | A reviewing agent gets a copy of the engineer's worktree, and a symlink in it pointed outside the tree, so the copy left it out. The reviewer sees a tree that differs from the engineer's by that link. | Nothing, unless the review needs that file: then commit its contents rather than linking to them from outside the repository. |
| `the run could not be paused over its budget` | The run crossed `limits.max_tokens` or `limits.max_cost_usd` but could not pause itself. | Stop it with Ctrl-C: it finishes as a user stop and the summary still reports what was spent. |
| `no new work will be started` | The budget banner: the run reached `limits.max_tokens` or `limits.max_cost_usd`, and the line says which and what it has spent. An autopilot run then pauses and waits for you; a brief run lets the reviews in flight finish and ends. | See *The run paused itself and nothing is happening* below: `p` in the TUI grants another budget's worth, `b` raises the cap, Ctrl-C or `sc stop` ends it. |
| `which slop-cannon did not register; leaving it alone` | A bead in the run's own graph carries one of the repository's custom `bd` statuses, so it belongs to another tool and the run will not move it (P6-12, D-07). The line names the bead and the status. | Nothing: it is deliberate. Move the bead to a status `sc` knows (`bd update <id> --status open`) if you want it worked on. |
| `which slop-cannon only reads; leaving it alone` | The same, for one of the states `sc` knows but never writes — a bead somebody parked in `deferred`, say. | Nothing. `sc status <bead>` shows where it is. |

### Resuming

| Message | What happened | What to do |
|---|---|---|
| `could not be reattached` | `--resume` found a bead whose worktree or branch it could not pick up again. | The bead is re-fired from its branch; `git worktree list` shows what is left under the old run directory. |
| `could not schedule the conflict round` | A resumed run knew a bead was mid-conflict but could not re-open the round. | `--resume` again, or resolve the rebase by hand in `runs/<run-id>/wt/<bead-id>`. |
| `could not re-run the pull request delivery` | A resumed run could not restart a delivery the previous run had begun. | `sc status` says where the bead is; the branch still holds the work. |
| `could not finish the interrupted delivery` | A resumed run picked up a delivery the previous run had begun and could not carry it through. | `sc status` says where the bead is; the branch still holds the work, and the next round re-delivers it. |
| `refusing a bead-status write the D-07 table has no edge for` | On resume, `bd` reported a bead in a state the workflow has no transition out of, so nothing was written for it. | `sc status <bead>` shows the state; move the bead by hand in `bd` if it should be worked on. |

### Delivery and conflicts

| Message | What happened | What to do |
|---|---|---|
| `the check command failed at delivery; sending the bead back as a qa change request` | `check_command` failed on the rebased branch, so the bead went back for another round instead of being delivered. | Nothing: the engineer is asked to fix it. Run the same command yourself to see what broke. |
| `delivery handed to a human` | Delivery stopped and the bead is waiting for a person. The rest of the line says why. | `sc status` lists what is waiting; the branch and any pull request are untouched. |
| `a delivery failed too many times` | One bead's delivery was retried up to its limit and failed every time, so it stops being retried and waits for a person. | Read the failures in `sc logs`; they are usually `gh`, branch protection, or a check command. Fix the cause, then `--resume`. |
| `a pull request exists but this run has safety off; leaving it to a human` | Safety-off delivery merges locally, and this bead already has a pull request open, but the run was assembled without a way to settle it. `sc` will not merge behind it. | Merge or close the pull request yourself, or re-run with safety on. |
| `has commits the local branch lacks; leaving it to a human` | A run resumed with safety off lands a bead's open pull request itself, but someone pushed to that pull request after `sc` opened it. Landing the local branch would leave their commits behind. | Merge or close the pull request yourself; the next resume closes the bead once it has merged. |
| `could not be compared with the local branch; leaving it to a human` | A run resumed with safety off could not read the pull request's branch on the remote (`git ls-remote` failed, or the local branch is gone), so it did not land the bead. | Check the remote with `git ls-remote origin`, then merge the pull request yourself or resume again. |
| `could not be cleared; leaving it to a human` | A run resumed with safety off could not resolve the bead's `gh:pr` gate — `bd` failed, or the gate is not a pull request gate — so the bead cannot close. | `bd gate list` shows the gate; resolve it with `bd gate resolve <id>` if it is yours to clear. |
| `stays open: the bead did not land yet` | A run resumed with safety off tried to land a bead that had a pull request, and the merge did not happen yet: a conflict round, a failed check or a hand-off. | Nothing: the bead is delivered the usual safety-off way. Close the pull request once it has landed. |
| `but could not be closed` | A run resumed with safety off landed a bead that had a pull request, and `gh pr close` failed afterwards. | Close the pull request yourself; the work is already on the default branch. |
| `the conflicted paths could not be listed; opening the conflict round without them` | The rebase conflicted and `git` would not say which files, so the engineer is asked to resolve it without that list. | Nothing; the worktree still shows the conflict markers. |
| `could not be measured; reviewing it again` | A conflict resolution came back but the run could not diff it, so it goes round again rather than being trusted. | Nothing unless it loops: then look at `runs/<run-id>/wt/<bead-id>`, which is where the rebase is. |

### Gates and merged pull requests

| Message | What happened | What to do |
|---|---|---|
| `gate polling skipped: gh is unavailable, so merged pull requests will not close their beads` | With safety on, `sc` polls GitHub to close beads whose pull requests were merged. Without `gh` it cannot, so beads stay open after you merge them. | `sc doctor`, then `gh auth login`. Until then, close the beads yourself with `bd`. |
| `could not check the pull request gates` | One poll of the gates failed. | Nothing: it is retried. Repeated failures are `bd` or `gh` — `sc doctor`. |
| `until the pull request is merged or a human resolves it` | A gate stays open because its pull request is neither merged nor closed. | Merge or close the pull request; the next poll closes the bead. |
| `not delivering; leaving it alone` | A gate resolved for a bead that is not in a deliverable state, so nothing was moved. | Nothing: `sc status <bead>` shows where the bead actually is. |
| `names no bead it blocks; leaving it alone` | A gate in the database blocks nothing, so there is nothing for `sc` to close. | Nothing: it is someone else's gate. `bd gate list` shows them. |
| `needs a human` | A gate was raised that only a person can clear — an escalation, or a pull request `sc` will not merge for you. | `sc status` lists what is waiting and why. |
| `gates but printed no per-gate verdict` | `bd` checked the gates and reported no result for any of them. | Usually a `bd` older than 1.2.2: `sc doctor`. |

### Auto-merge (D-31)

Only a run started with `safety = true` and `auto_merge = true` prints
these. Every one names the bead, and the latest one per bead is also the
`auto-merge:` line of `sc status`. Waiting is the normal state: the run
looks again on every gate poll and merges as soon as everything holds.

| Message | What happened | What to do |
|---|---|---|
| `auto-merge:` | What auto-merge last decided about a bead's pull request, with the reason. The rows below are the reasons worth looking up. | Read the reason; most of them clear on their own. |
| `it is left for a human to merge` | The pull request behind the bead's gate fails the ownership check (D-31): another head or base, no `slop-cannon` label, no marker naming this run and bead, another number, created before the run started, or opened by another login. It is never merged, updated or sent back; the bead stays in `delivering` for a human. | Merge or close it yourself. If it should be the run's, the clause named says what is different. |
| `holds commits the run did not review` | The pull request is the run's own, but its head is neither the commit the run delivered and its testers approved (`sc_pr_head`) nor that commit plus GitHub's clean update merges of the default branch: somebody pushed to the branch after delivery, or a merge on it changes more than merging its parents. It is never merged, updated or sent back; the bead stays in `delivering` for a human. | Review what was added (`git log <sc_pr_head>..<head>`) and merge the pull request yourself, or remove the commits and the next poll merges it. |
| `no reviewed commit is recorded` | The bead has no `sc_pr_head`, so no head can be shown to be the work the run reviewed. A bead delivered by an older `sc` is like this. | Merge the pull request yourself. |
| `could not show that pull request #` | Judging whether the head holds only reviewed work needs a fetch and a few git commands in the repository, and one failed. Nothing merges. | Usually the network: the next poll tries again. The rest of the line names the git error. |
| `but the bead records #` | The gate waits on another pull request than the one the bead recorded in `sc_pr_number`, so the run cannot tell which is its own and merges neither. | Look at both with `bd show <bead-id>` and `gh pr view`; merge the right one by hand. |
| `GitHub reports pull request #` | Everything else holds but GitHub does not report the pull request `CLEAN`. `BLOCKED` is a branch-protection requirement such as a required review, `UNSTABLE` a failing check that is not required, `HAS_HOOKS` a pending merge hook, `UNKNOWN` GitHub still working it out. Auto-merge never bypasses any of them. | For `BLOCKED`, give the review the branch protection asks for; the next poll merges. The rest usually clear by themselves. |
| `has no checks yet` | The pull request's head has no checks or statuses at all. An empty rollup is never read as green. | Nothing while CI starts. If the repository has no CI on pull requests, auto-merge will never merge: merge by hand, or add a check. |
| `still running` | At least one check has not finished (or finished with a result `sc` does not know, such as `STALE`). | Nothing: the next poll looks again. |
| `not reported as successful` | A name in `auto_merge_checks` is not among the checks that succeeded on the head: not reported yet, or only skipped or neutral. | Nothing while CI runs. If it never appears, the name in `auto_merge_checks` does not match the check's name on GitHub. |
| `is a draft` | The pull request was marked as a draft, by hand. | Mark it ready for review if it should merge. |
| `GitHub reported no head commit for pull request #` | `gh pr view` printed no `headRefOid`, and a merge must name the head commit it was judged on. | Nothing: the next poll reads it again. |
| `green; the next merge is allowed at` | The pull request is ready, but the run merged another one less than `auto_merge_spacing` ago, so the deploy that merge started can finish first. A resumed run counts from the last merge any invocation made. | Nothing. Lower `auto_merge_spacing` if your deploys are faster. |
| `green; another pull request was merged in this poll` | The run merges at most one pull request per poll. | Nothing. |
| `squash-merged pull request #` | The run merged its own pull request. The bead closes when the next gate poll sees the merge. | Nothing. |
| `waiting for the update GitHub was asked for` | The pull request is behind the default branch and GitHub was already asked to merge the default branch into it once for this head. | Nothing while GitHub updates it. If it stays behind, run `gh pr update-branch <n>` yourself. |
| `GitHub is merging it in, and the checks run again` | The pull request was behind the default branch, so the run asked GitHub to merge the default branch into it (`gh pr update-branch`, a server-side merge, never a force push). | Nothing: the checks run on the new head and the next green poll merges. |
| `back to its engineer` | A check failed, or the pull request conflicts with the default branch. The bead went back to its engineer as a change request: one qa item per failed check with its URL, or one item to merge the default branch in and resolve. The engineer's push goes to the same pull request. | Nothing. A head that comes back with the same failure or conflict goes to a person instead (`it needs a human`). |
| `was sent back to its engineer but the feedback loop could not take it` | The bead moved to `changes_requested`, but the run could not hand it to its engineer. | `sc status <bead-id>`; resuming the run hands it over again. |
| `could not record in state.json that` | A bead was sent back to its engineer, but writing that head into `state.json` failed. This invocation still remembers it; a resume would send the same head round once more instead of handing it to a person. | Check the disk under the state directory. |
| `could not bring the bead's branch up to its pull request's head` | Before sending the bead back, the run fetches and fast-forwards the bead's branch onto the pull request's head, and that failed. The bead stays where it is. | Usually the network: the next poll tries again. The rest of the line names the git error. |
| `could not read the gh login, so no pull request can be shown to be the run's own` | `gh api user --jq .login` failed, so the author clause of the ownership check cannot be judged and nothing merges. | `gh auth status`; the next poll reads it again. |
| `could not read pull request #` | `gh pr view` failed for the bead's pull request. | Usually GitHub being unreachable: the next poll reads it again. |
| `could not find the branch of` | The bead's `sc/` branch is gone from the repository, so the head clause of the ownership check cannot be judged and nothing merges. | Check nothing outside `sc` is pruning `sc/` branches; merge the pull request by hand. |
| `could not record the merge in state.json, so it was not made` | The time of a merge is written to `state.json` before the merge, so the spacing holds across a resume; the write failed, so no merge was asked for. | Check the disk under the state directory. The next poll tries again. |
| `gh could not merge pull request #` | `gh pr merge --squash --match-head-commit` failed: GitHub refused it (the head moved after the poll read it, or a requirement changed) or could not be reached. The spacing starts anyway. | Nothing usually: the next poll judges the new state. The rest of the line is GitHub's reason. |
| `gh could not update pull request #` | `gh pr update-branch` failed. | Run it yourself to see GitHub's reason; the next poll tries again. |

### The review loop

| Message | What happened | What to do |
|---|---|---|
| `so the answer was not applied and the item is still owed` | The engineer argued with a binding ruling instead of doing it, so the item stays open and the round goes again. | Nothing: the loop re-asks. Repeated arguing on one item is worth reading in `sc logs <bead-id>`. |
| `ruling the loop cannot apply` | An arbitration or escalation ruling arrived for something the loop is no longer tracking. | Nothing: a duplicate answer from a re-fired agent. |
| `on a bead in state` | An event arrived for a bead whose state has no edge for it (D-07), so it was ignored. | Nothing, usually. Persistent ones are worth reporting with the bead id. |
| `refused work on a bead with an open rules violation` | The rules compliance gate stopped a firing or a transition while a `rules` verdict item is unresolved (D-08, P9-06). | Fix the code so the rule is met, or change the rules file if the rule itself is wrong. |
| `refused a resumed thread; the bead needs a person` | On `--resume`, the rules gate found a bead whose stored thread would settle a `rules` violation, so the thread was dropped and the bead parked for a person. | Read the violation in `sc status <bead>`, fix the code or the rules file, then resume again. |
| `its thread settles a rules-file violation the gate holds open` | The same, reported once the bead reached `needs_human`. | As above. |
| `could not be parked in needs_human; it is where bd left it` | The rules gate refused a resumed thread but `bd` would not move the bead, so it stayed where it was. | `sc status <bead>` says where; move it in `bd` if it should be worked on. |
| `a narrow_scope ruling dropped open rules-file violations` | A product owner narrowed a bead's acceptance criteria in a way that would have dropped open `rules` items with them. The items stay; the narrowing does not silently discard them (D-08). | Nothing: the loop keeps asking for them. The message lists which. |

### Reviewing pull requests `sc` did not open

| Message | What happened | What to do |
|---|---|---|
| `polling for external pull requests failed; retrying` | One poll of GitHub failed. Unlike `external pull request review stopped`, this one is retried. | Nothing unless it repeats: then `sc doctor` for `gh`. |
| `no review will be posted` | The review bead of a pull request was closed or failed before its review could be posted. | Nothing: the pull request is untouched. |
| `no gh: the review of pull request #` | A review was produced but there is no usable `gh` to post it with. | `gh auth login`, then the next poll posts it. The verdicts are in `sc logs` meanwhile. |
| `could not build the review of pull request #` | The reviewing agents ran but their verdicts could not be assembled into a review. | Nothing: the next poll tries again, three times in all. |
| `its review bead` | Housekeeping on a review bead: it is being left, handed to a human, or closed, and the message says which. | Nothing. `sc status` lists the review beads. |

### The bead database

| Message | What happened | What to do |
|---|---|---|
| `could not read the beads project metadata` | `.beads/metadata.json` could not be read, so `sc` used what `bd` itself reports. | Nothing: it is a notice. A missing or unreadable `metadata.json` in a working repository is worth checking into `git status`. |
| `metadata.json says dolt_mode` | The file and `bd` disagree about the database mode; `sc` trusts `bd`. | Nothing. If it persists, `bd` and the file were written by different versions — `sc doctor` reports the one in use. |

## What `sc doctor` prints

`sc doctor` is the first thing to run, and when a run refuses to start it
prints the same rows as a numbered fix list. These are the words its rows
carry, rather than the errors above.

| Message | What happened | What to do |
|---|---|---|
| `not found on PATH` | The tool that row names — `git`, `bd`, `gh`, or a cartridge's CLI — is not installed, or not on the `PATH` `sc` sees. | Install it, or, for a cartridge, set `command` under `[cartridges.<name>]` to its absolute path. `gh` is only needed for safety-on delivery and pull request review. |
| `--version failed` | The tool is on `PATH` but `git --version` or `gh --version` did not run: a wrapper script, a broken install, or a binary for another architecture. | Run the command yourself; the row carries what it printed. |
| `git status failed in` | The repository row could not read the working tree's state. | Run `git status` in the directory the row names; it is usually a permissions problem or a repository another user owns. |
| `does not ignore .env` | The repository's `.gitignore` does not ignore dotenv files, and an engineer that commits one has leaked the credential whatever the prompts said (P9-05). | Add `.env` to `.gitignore`. It is a warning, not a refusal. |
| `is older than` | The tool is installed but below the minimum `sc` supports (git 2.20, `bd` 1.2.2). | Upgrade it; the row names the version found and the path it was found at. |
| `version unknown` | The tool answered `--version` with something `sc` could not parse. | Usually harmless — the row is a warning, not a failure. Check you are not shadowing the real binary with a wrapper script. |
| `not authenticated` | `gh`, or a cartridge's CLI, is installed but not logged in. | `gh auth login`, or run the cartridge's CLI once yourself (for Claude Code: `claude`). |
| `not logged in (run: claude auth login, or set ANTHROPIC_API_KEY)` | The Claude CLI is installed but has no credentials. | Either of the two things it names. |
| `auth probe` | `sc` could not tell whether the cartridge is logged in: the probe did not finish, printed no JSON, or could not be run. | Run the CLI once by hand. `sc` fires it anyway; a real auth failure surfaces as `cartridge: not authenticated`. |
| `sc has only been tested against embedded mode` | The beads project is on the multi-writer Dolt **server** backend rather than the embedded one — `sc doctor`'s `dolt` row when `bd context` (or `.beads/metadata.json`) says so, and the same notice from a run. `sc` continues; it has simply never been tested there (P3-17). | Nothing, unless something misbehaves. Server mode is the only mode that needs the standalone `dolt` CLI, which beads pins at 2.2.0; embedded mode links the engine into `bd` and ignores the `dolt` on your machine. |
| `backend unknown: bd context failed` | `sc doctor` could not ask `bd` which Dolt backend the project uses, and `.beads/metadata.json` could not stand in either. | Run `bd context --json` in the repository; the row carries what `bd` printed. It is a warning, not a refusal. |
| `no cartridge configured` | No cartridge at all is in the configuration. | `sc cartridge --add claude --model <model>`. |
| `no active cartridge` | Cartridges are configured but none is selected. | `sc cartridge --use claude`. |
| `uncommitted change` | The target repository has uncommitted work. Worktrees are made from a clean tree. | Commit or stash it. It is a warning: `sc doctor` still passes. |
| `clean-tree check skipped: git is not usable` | The repository row could not run because the `git` row failed. | Fix `git` first; the rest of the report is still worth reading. |
| `not initialised:` | The repository has no `.beads/` directory. | Nothing to do before a run: `sc` creates one (D-13). `bd init` does it by hand. |
| `could not be read:` | A file `sc doctor` checks — the config, the repository's `.gitignore` — could not be opened. | The row names it; check its permissions. |

## The demo (`make demo`)

The deterministic demo (P8-07) is `scripts/demo/demo.sh`; the README explains
what it builds. It writes nothing outside its own temporary directory, so a
failure here never leaves anything behind to clean up. These messages come
from the script and from VHS rather than from `sc`, which is why they are
listed by symptom rather than in a message table.

| Symptom | What happened | What to do |
|---|---|---|
| `demo.sh: go is not on PATH` (or `git`) | The demo compiles `sc` and the `bd` stand-in for every run, so it needs the Go toolchain and git. | Install Go 1.27+ and git, or just look at the recording: `docs/demo.gif`. |
| `demo.sh: unknown argument` | `DEMO_ARGS` carries something the script does not take. | `scripts/demo/demo.sh --help` lists them: `--headless`, `--check`, `--no-anim`, `--keep`, `--safety-off`. |
| `the TUI needs a terminal` | `make demo` was run from a pipe, a CI job or an editor task runner. | `make demo DEMO_ARGS=--headless`, or `--check` to assert the run instead of watching it. |
| `demo.sh: check: MISS …` | `--check` replayed the run's own `events.jsonl` and one of the five cannon animations was never raised, so `scripts/demo/scenario.json` and the run have drifted apart. | The line names the missing event. `go test ./scripts/demo/scdemo/` checks the scenario without firing a run, and `make demo DEMO_ARGS=--keep` leaves the machine — repository, state directory, event log — on disk to read. |
| `demo.sh: check: the run wrote no event log` | The run failed before it opened its state directory, usually a build or `bd` stand-in failure further up the output. | Read the lines above it; `go build ./scripts/demo/scdemo ./testdata/orchestrator/fakebd` reproduces a build failure on its own. |
| A re-recorded `docs/demo.gif` shows a shell prompt, or `vhs: command not found` | Recording `demo.tape` needs VHS with `ttyd` and `ffmpeg` on `PATH`, and a tree that builds: VHS records whatever the terminal shows, compiler errors included. | Install all three (`go install github.com/charmbracelet/vhs@latest` covers VHS only), then `make demo DEMO_ARGS=--check` once — it warms the build cache and fails loudly if the demo is broken — and only then `vhs demo.tape`. |

---

If none of this matches, `sc status --json` and the run's `events.jsonl` are
the full record of what happened, and `sc doctor --json` is the full record
of what the machine looks like. Both belong in a bug report, with the `sc`
version from `sc version`.

**See also:** the [docs index](README.md) lists every page. The ones nearest
to this one are [cli.md](cli.md) (what each command and flag does),
[config.md](config.md) (every key you might have set wrong),
[prompts.md](prompts.md) (what an agent was told, where the run wrote it
down, and the exit codes) and [performance.md](performance.md) (when the
problem is that something is slow rather than broken).
