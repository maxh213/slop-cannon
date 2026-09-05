# Captured `bd` 1.2.2 output for `internal/beads`

Every file is the verbatim output of the real `bd` 1.2.2 (embedded Dolt,
`bd version` reports `1.2.2 (dev)`) run on 2026-09-02 in a scratch copy of
`testdata/fixture-repo` with `BD_JSON_ENVELOPE=1 BD_NON_INTERACTIVE=1
BEADS_ACTOR=sc` (D-21) and an isolated `HOME`. Only paths were rewritten
(`/tmp/fixture-repo`, `/tmp/home`, `/tmp`). The P3-01 samples (version,
init, gitignore hunks, ping) were captured first; the command set below
was captured by P3-02 with one scripted session so the ids are consistent
across samples: epic `fx-nvi`, its graph-plan children `fx-so0` (Mean) and
`fx-ctt` (Max, blocked by Mean), and the hand-made children `fx-nvi.1`
(Median, blocked by `fx-nvi.2`) and `fx-nvi.2` (Median tests).

`testutil.FakeBD` replays the directory (same `<key>.json/.jsonl/.txt`,
`.stderr`, `.exit` layout as `internal/testutil/testdata/bd`, see that
README for the key rules); `clientStage` in `client_test.go` installs the
alternative samples under a key (for example `{"close": "error-close-blocked"}`).
`preamble.txt` is bd's one-time metrics notice, printed to stderr by the shim
on its first call, which is how the tests prove the stderr parser skips it.

Two later captures were made the same way, against the same bd 1.2.2, in a
fresh scratch repository of their own (2026-09-05): `create-external-ref`
(P3-19, prefix `fx`, so its id `fx-c5v` belongs to no earlier sample), and
the re-capture of `graph-dry-run-existing` after P3-18 put `parent_id` in
the graph plan — that one was taken with the epic of its own repository and
the id rewritten to `px-26w`, the epic the other `graph-*` samples use.

## Command samples

| Sample | Command | Notes |
|---|---|---|
| `version`, `first-call-version` | `bd version --json` | envelope with `version`, `build` |
| `version-text` | `bd version` | plain text, no envelope |
| `init`, `init-again`, `init-dash-C`, `gitignore-hunk`, `git-info-exclude-hunk`, `ping-no-db` | see P3-01 | init prints text even with `--json` |
| `ping` | `bd ping --json` | `status: ok` |
| `info` | `bd info --json` | plain text despite `--json` (P3-13 b) |
| `statuses-builtin` | `bd statuses --json` before any custom status | 7 built-ins |
| `config-set` | `bd config set status.custom "<D-07 list>" --json` | echoes key and value |
| `config-get` | `bd config get status.custom --json` | |
| `config-get-missing` | `bd config get sc.nothing --json` | unset key: empty value, exit 0 |
| `statuses` | `bd statuses --json` after the D-07 statuses were set | `custom_statuses` sorted by name |
| `create-graph-dry-run` | `bd create --graph plan.json --dry-run --json` | plan with top-level `edges` (D-24): `edge_count: 1`, `parent_deps: 2`, no warning |
| `create-graph-dry-run-warning` | same with `bogus_field` and node-level `deps` | stderr `warning: graph plan node["a"] has unknown field(s): [bogus_field deps]`, `edge_count: 0` |
| `create-graph` | `bd create --graph plan.json --json` | `data.ids` maps keys to ids |
| `create` | `bd create "Add Median" -t task -p 2 --description … --acceptance … -l sc:run:r1,sc:role:engineer --metadata @file --parent fx-nvi --json` | object, not array; metadata keeps JSON types |
| `create-external-ref` | `bd create "Review PR #9: Add Median" -t task -p 2 --description … --external-ref gh-9 -l sc:run:r1,sc:external-pr --metadata @file --json` | P3-19, captured 2026-09-05: bd echoes `external_ref` on the issue beside the `sc_external_ref` metadata |
| `create-silent` | `bd create … --silent` | prints only the id |
| `update-claim`, `update-claim-again` | `bd update fx-so0 --claim --json` as `sc:engineer-1`, twice | atomic claim; idempotent for the same actor |
| `comment` | `bd comment fx-so0 --stdin --json` | body on stdin; `author` is the actor |
| `comment-no-json` | `bd comment fx-so0 --stdin` | text confirmation |
| `comments` | `bd comments fx-so0 --json` | creation order |
| `comments-empty` | `bd comments fx-ctt --json` | `[]` |
| `show` | `bd show fx-so0 --json --include-comments` | an **array** with one issue; dependencies in the issue form with `dependency_type`; `comments[]` |
| `show-multi` | `bd show fx-so0 fx-ctt --json --include-comments` | |
| `show-dependents` | `bd show fx-so0 --json --include-dependents` | `dependents[]` in the issue form |
| `update` | `bd update fx-ctt -s in_review --set-metadata sc_round=1 --set-metadata sc_agent=engineer-1 --set-metadata sc_lease=… --add-label sc:role:engineer --json` | `sc_round` stored as the number `1` (P3-13 c) |
| `update-metadata-file` | `bd update fx-ctt --metadata @file --json` | `--metadata` **merges** into the existing object |
| `update-set-metadata-types` | `--set-metadata sc_text=hello --set-metadata sc_flag=true --set-metadata sc_float=1.5 --set-metadata 'sc_obj={"a":1}'` | bd types k=v values: bool, float, and an object literal becomes a string |
| `update-unset-metadata` | `--unset-metadata` for those four keys | |
| `update-acceptance` | `bd update fx-ctt --acceptance … --json` | |
| `update-release` | `bd update fx-ctt -s open --assignee "" --json` | the D-07 release |
| `dep-add`, `dep-add-again` | `bd dep add fx-nvi.1 fx-nvi.2 --json` | `fx-nvi.1` depends on `fx-nvi.2`; repeating is a no-op |
| `dep-list` | `bd dep list fx-nvi.1 --json` | one id: the **issue form** (`id` + `dependency_type`) |
| `dep-list-multi` | `bd dep list fx-nvi.1 fx-ctt --json` | several ids: the **edge form** (`issue_id`, `depends_on_id`, `type`) |
| `ready` | `bd ready -n 0 --json` | lists the epic `fx-nvi` itself (P3-13 a); `fx-so0` is claimed, `fx-ctt` and `fx-nvi.1` blocked |
| `ready-parent` | `bd ready -n 0 --parent fx-nvi --json` | descendants only |
| `blocked` | `bd blocked --json` | `blocked_by[]` and `blocked_by_count` |
| `list`, `list-default` | `bd list -n 0 --flat --json` and plain `bd list --json` | identical here (5 issues); the defaults truncate at 50 and hide closed |
| `list-filtered` | `bd list -n 0 --flat -s open -l sc:run:r1 --parent fx-nvi --metadata-field sc_run=r1 --json` | |
| `list-all` | `bd list -n 0 --flat --all --json` after the close | includes closed `fx-so0` |
| `close` | `bd close fx-so0 --reason "fixed in round 1" --json` | array with the closed issue |
| `close-again` | same command again | no error |
| `ready-after-close` | `bd ready -n 0 --json` after the close | `fx-ctt` became ready |
| `export`, `export-all` | `bd export`, `bd export --all` | JSONL, `_type: issue`, edges in edge form, comments inline |
| `readonly-show` | `bd --readonly show fx-ctt --json` | reads work under `--readonly` |

Every listing carries the `warning: beads.role not configured (GH#2950)`
notice on stderr (`*.stderr`) because bd checks the global git config; the
stderr parser skips such notices.

## Error samples (P3-12)

bd 1.2.2 does not print the documented `{"error","code","hint"}` envelope.
Two shapes occur, both exit 1 unless noted:

| Sample | Command | Shape | Typed error |
|---|---|---|---|
| `error-not-found` | `bd show nope-1 --json` | stdout `{"data":{"error":"no issues found matching the provided IDs"}}`, stderr `Error fetching nope-1: …` | `ErrNotFound` |
| `error-not-found-mixed` | `bd show fx-so0 nope-1 --json` | **exit 0**, the found issue on stdout, `Error fetching nope-1` on stderr | `Client.Show` returns the issues and `ErrNotFound` |
| `error-show-no-args` | `bd show --json` | `data.error` | `ErrInvalid` |
| `error-update-not-found` | `bd update nope-1 -s open --json` | stderr only: `Error resolving nope-1: no issue found matching "nope-1"` | `ErrNotFound` |
| `error-close-not-found` | `bd close nope-1 --json` | `data.error`: `resolving ID nope-1: …` | `ErrNotFound` |
| `error-comment-not-found` | `bd comment nope-1 --stdin --json` | `data.error` | `ErrNotFound` |
| `error-parent-not-found` | `bd create X --parent nope-1 --json` | stderr `Error: parent issue nope-1 not found` | `ErrNotFound` |
| `error-close-blocked`, `error-close-blocked-2` | `bd close fx-nvi.1 --reason done --json` | stderr only, empty stdout: `cannot close fx-nvi.1: blocked by open issues [fx-nvi.2] (use --force to override)` | `ErrBlockedOpen` |
| `error-readonly`, `error-readonly-close`, `error-readonly-comment` | `bd --readonly update/close/comment …` | stderr only: `Error: operation 'update' is not allowed in read-only mode` | `ErrReadOnly` |
| `error-claim-taken` | `bd update fx-so0 --claim` as `sc:engineer-2` | stderr only: `Error claiming fx-so0: issue already claimed by sc:engineer-1` | `ErrAlreadyClaimed` |
| `error-dep-cycle` | `bd dep add fx-nvi.2 fx-nvi.1 --json` | `data.error`: `adding dependency would create a cycle` | `ErrCycle` |
| `error-dep-not-found` | `bd dep add fx-nvi.1 nope-1 --json` | **exit 0**, `status: added` | none: bd accepts a dangling depends-on id |
| `error-no-project` | `bd -C /tmp ready --json` | stderr `Error: cannot use -C directory "/tmp": no beads project found` | `ErrNoProject` |
| `error-bad-status` | `bd update fx-ctt -s not_a_status --json` | `data.error` listing the valid statuses | `ErrInvalid` |
| `error-bad-type` | `bd create X -t nonsense --json` | stderr `Error: validation failed for issue : invalid issue type: nonsense` | `ErrInvalid` |
| `error-bad-priority` | `bd create X -p 9 --json` | stderr `Error: invalid priority "9" …` | `ErrInvalid` |
| `error-empty-title` | `bd create "" -t task --json` | stderr `Error: validation failed for issue : title is required` | `ErrInvalid` |
| `error-comment-empty` | `bd comment fx-so0 --stdin --json` with empty stdin | `data.error`: `comment text cannot be empty` | `ErrInvalid` |

No lock error was observed: bd waits for the embedded Dolt lock (D-21), so
contention surfaces as a timeout. `ErrLocked` is matched on lock messages
for future versions only.

The capture script lives in the P3-02 session scratchpad; it is a plain
sequence of the commands above and can be reconstructed from this table.

## Dolt samples (P3-06)

Captured by P3-06 in a fresh scratch repository (prefix `fx`, bead `fx-fbq`)
the same way as above; paths rewritten to `/tmp/fixture-repo`.

| Sample | Command | Notes |
|---|---|---|
| `context` | `bd context --json` | `dolt_mode: embedded`, `beads_dir`, `repo_root`, `database`, `project_id`, `role`, `bd_version`; bd reads it from its config files, no database needed |
| `context-server` | `bd -C <copy> context --json` after editing the copy's `metadata.json` to `dolt_mode: server` | **simulated** server mode: adds `server_host`; `is_redirected` and `role: contributor` come from running it outside the copy |
| `error-context-no-project` | `bd -C /tmp/plain context --json` on a git repo without `.beads/` | stderr only, exit 1: `cannot use -C directory …: no beads project found` → `ErrNoProject` |
| `history` | `bd history fx-fbq --json` after create, `update -s`, `update --set-metadata`, `comment` | array of `{CommitHash, Committer, CommitDate, Issue}` (capitalised keys), newest first; `Issue` carries the core columns only |
| `history-missing` | `bd history nope-1 --json` | `data: null`, **exit 0** → `Client.History` reports `ErrNotFound` |
| `history-text` | `bd history fx-fbq --limit 2` | the human rendering |
| `config-get-auto-commit`, `config-get-auto-commit-off` | `bd config get dolt.auto-commit --json` | `on` is the embedded default (printed even when `config.yaml` has no such line); `off` after `bd config set dolt.auto-commit off` |
| `project-metadata` | `cat .beads/metadata.json` | `dolt_mode: embedded`, `dolt_database`, `project_id` |
| `dolt-status` | `bd dolt status` | text: `Dolt engine: embedded (in-process, no server)` |

Measured while capturing (`bd history <id> --json | grep -c CommitHash`
after each command): every write (`create`, `update`, `comment`, `dep add`,
`close`, `config set`) adds exactly one commit to the history of **every**
bead that already existed, reads add none, a write with
`--dolt-auto-commit off` adds none until `bd dolt commit` adds one.

## Gate samples (P3-08)

Captured by P3-08 from bd 1.2.2 with a `gh` shim answering `gh pr view <n>
--json state,title` (state OPEN, MERGED or CLOSED, or a failure). `bd gate
check` prints its per-gate verdict lines before the JSON envelope, so those
samples are `.txt` (plus `.stderr`/`.exit` companions).

| Sample | Command | Notes |
|---|---|---|
| `gate-create` | `bd gate create --type=gh:pr --blocks fx-byi --await-id 1 --reason "PR #1" --json` | the gate issue; the blocked bead only appears in the description (`Ad-hoc gate blocking fx-byi`) |
| `gate-create-noawait` | the same without `--await-id` | accepted; the gate then never resolves |
| `gate-create-not-found` | `--blocks nope-1` | exit 1, `data.error` `issue not found: nope-1` |
| `gate-create-readonly` | under `--readonly` | exit 1, stderr `operation 'gate create' is not allowed in read-only mode` |
| `gate-list`, `gate-list-empty`, `gate-list-all`, `gate-list-two` | `bd gate list [--all] --json` | one open gate; `data: null` after it resolved; the resolved gate with `close_reason`; one open and one closed |
| `gate-show`, `gate-show-not-found` | `bd gate show <id> --json` | an object, not an array; exit 1 with `gate not found` |
| `gate-check`, `gate-check-open` | `bd gate check --type=gh:pr --json`, PR OPEN | `○ pending` line, then the envelope |
| `gate-check-merged` | PR MERGED | `✓ resolved`: bd closed the gate |
| `gate-check-closed`, `gate-check-closed-escalate` | PR CLOSED, without and with `--escalate` | `⚠ ESCALATE`; the gate stays open either way; `--escalate` only shells out to Gas Town `gt` (stderr Warning when absent) |
| `gate-check-missing-gh`, `gate-check-unauth`, `gate-check-bad-gh` | `gh` absent / not logged in / printing garbage | stderr `✗ <gate>: error checking - …`, exit 0, `errors: 1` |
| `gate-check-pr-missing` | `gh` cannot find the PR | ESCALATE `pull request not found` |
| `gate-check-none`, `gate-check-dry-run`, `gate-check-two` | no open gate (text only, no JSON); `--dry-run`; two gates | |
| `gate-resolve` | `bd gate resolve <id> --reason done --json` | text only, no JSON |

## Graph-plan samples (P3-10, P3-11)

Captured by P3-10 with the real binary in a scratch repository (prefix
`px`). Verified node fields accepted by bd 1.2.2: `key, title, type,
priority, description, labels, metadata, parent_key, parent_id, assignee`;
no spelling of acceptance criteria is accepted (`acceptance`,
`acceptance_criteria`, `notes`, `design`, `owner`, `id`, `deps` are all
warned about and dropped), and metadata values must be JSON strings.

| Sample | Command | Notes |
|---|---|---|
| `graph-plan` | the plan file `beads.BuildGraphPlan` writes for a six-bead diamond plus the run epic | 7 nodes, 6 top-level `edges`, `parent_key: epic` |
| `graph-dry-run` | `bd create --graph graph-plan.json --dry-run --json` | `node_count: 7`, `edge_count: 6`, `parent_deps: 6`, no warning |
| `graph-dry-run-mismatch` | **hand-edited** copy of `graph-dry-run` with `edge_count: 4` | drives the D-24 mismatch failure test |
| `graph-create` | `bd create --graph graph-plan.json --json` | key → id map including the epic `px-26w` |
| `graph-blocked` | `bd blocked --json` afterwards | `blocked_by` of every dependent bead |
| `graph-show-epic` | `bd show px-26w --json` | the epic with its children |
| `graph-plan-existing`, `graph-dry-run-existing`, `graph-create-existing` | the same plan joining an existing epic (no epic node) | 6 nodes, 6 edges, every node carries `parent_id: px-26w`, so `parent_deps: 6` (P3-18; re-captured 2026-09-05, see below) |
| `graph-update-finish` | `bd update <id> --acceptance … --json` | the per-bead finishing call (acceptance criteria cannot be set in the plan) |

## Hand-derived samples (P3-03, P3-14)

These were **not** captured: bd 1.2.2 has no foreign custom statuses in the
fixture, so they were written by hand in the shapes the captured samples
show, and the tests that use them say so.

| Sample | Stands for | Notes |
|---|---|---|
| `config-get-foreign` | `bd config get status.custom --json` in a repository with its own `triage:active,parked:frozen` statuses | drives the union in `EnsureStatuses` |
| `config-get-malformed` | the same with a value `EnsureStatuses` cannot parse | it refuses to overwrite |
| `statuses-foreign` | `bd statuses --json` with the foreign statuses registered | |
| `list-foreign` | `bd list --all --json -n 0 --flat` with a `triage` bead and a `blocked` bead | round-trips through `List()` into `SafeBead` states |
| `comments-messages` | `bd comments <id> --json` holding one comment per D-25 message type | round-trip of the message reader |
