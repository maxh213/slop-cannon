# Captured `bd` 1.2.2 output

Every file here is the verbatim output of a real `bd` 1.2.2 (embedded Dolt)
run on 2026-09-02 against a fresh copy of `testdata/fixture-repo`, with
`BD_JSON_ENVELOPE=1 BD_NON_INTERACTIVE=1 BEADS_ACTOR=sc:test` (D-21). Only
paths were rewritten (`/tmp/fixture-repo`, `/tmp/home`, `/tmp/plan.json`).
`testutil.FakeBD` replays them; `testutil.StageBDSamples` remaps them.

File layout per sample `<name>`:

- `<name>.json` / `.jsonl` / `.txt` - standard output (absent when bd printed nothing)
- `<name>.stderr` - standard error (absent when empty)
- `<name>.exit` - exit status (absent means 0)
- `preamble.txt` - bd's one-time metrics notice; the shim prints it to stderr on its first call

The shim picks a sample by key (see `testutil.BDKey`): the subcommand, plus
the second word for group commands (`dep add` becomes `dep-add`), and
`create --graph [--dry-run]` becomes `create-graph[-dry-run]`. Samples whose
name is not a key (for example `ready-after-close`, `error-close-blocked`)
are alternatives to install under a key with `StageBDSamples`.

| Sample | Command | Notes |
|---|---|---|
| `version` | `bd version --json` | `{"data":{"version":"1.2.2","build":"dev"}}` |
| `version-text` | `bd version` | plain text, no envelope |
| `init` | `bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude -p fx --json` | text even with `--json`; stderr carries the "No Dolt remote" notice |
| `init-again` | same command, second run | no stdout; stderr `Skipping init: workspace already initialized.` |
| `ping`, `where`, `context` | `bd <cmd> --json` | |
| `info` | `bd info --json` | plain text despite `--json` |
| `statuses-builtin` | `bd statuses --json` before custom statuses | |
| `config-set` | `bd config set status.custom "<D-07 list>" --json` | |
| `config-get` | `bd config get status.custom --json` | |
| `statuses` | `bd statuses --json` after the D-07 statuses were set | |
| `create-graph-dry-run` | `bd create --graph plan.json --dry-run --json` | stderr warns that `acceptance_criteria` and `deps` are unknown node fields |
| `create-graph` | `bd create --graph plan.json --json` | `data.ids` maps plan keys to ids: epic `fx-a0u`, mean `fx-z7f`, max `fx-0uu` |
| `ready` | `bd ready -n 0 --json` | `fx-a0u` (the epic itself), `fx-z7f`, `fx-0uu` |
| `ready-parent` | `bd ready -n 0 --parent fx-a0u --json` | `fx-z7f`, `fx-0uu` |
| `ready-after-close` | `bd ready -n 0 --json` after `fx-z7f` was closed and `fx-0uu` gated | `fx-a0u`, `fx-a0u.1` |
| `list` | `bd list -n 0 --flat --json` | |
| `list-all` | `bd list -n 0 --flat --all --json` | |
| `show` | `bd show fx-z7f --json --include-comments` | array with one issue in `in_review` with one comment |
| `show-multi` | `bd show fx-z7f fx-0uu --json` | |
| `error-not-found` | `bd show nope-1 --json` | exit 1, stdout `{"data":{"error":"no issues found matching the provided IDs"}}`, message on stderr |
| `update` | `bd update fx-z7f -s in_review --set-metadata sc_round=1 --add-label sc:role:engineer --json` | `sc_round` is stored as the JSON number `1`, not the string `"1"` |
| `update-claim` | `bd update fx-z7f --claim --json` | assignee `sc:test`, status `in_progress` |
| `update-claim-again` | same actor, second claim | idempotent |
| `comment` | `bd comment fx-z7f --stdin --json` | body was an `impl_report` fenced JSON block |
| `comments` | `bd comments fx-z7f --json` | |
| `create` | `bd create "Add Median" -t task -p 2 --description ... --acceptance ... -l sc:run:r1 --parent fx-a0u --json` | child id `fx-a0u.1` |
| `create-child` | `bd create "Write Median tests" -t task -p 2 -l sc:run:r1 --parent fx-a0u --json` | child id `fx-a0u.2` |
| `dep-add` | `bd dep add fx-a0u.1 fx-0uu --json` | `fx-a0u.1` depends on `fx-0uu` |
| `dep-list` | `bd dep list fx-a0u.1 --json` | |
| `dep-cycles` | `bd dep cycles --json` | |
| `blocked` | `bd blocked --json` | `fx-a0u.1` blocked by open `fx-a0u.2` |
| `error-close-blocked` | `bd close fx-a0u.1 --reason done --json` | exit 1, no stdout, stderr `cannot close fx-a0u.1: blocked by open issues [fx-a0u.2] (use --force to override)` |
| `close` | `bd close fx-z7f --reason "fixed in round 1" --json` | |
| `gate-create` | `bd gate create --type=gh:pr --blocks fx-0uu --await-id 1 --reason "PR #1" --json` | gate id `fx-jts` |
| `gate-list` | `bd gate list --json` | |
| `gate-check` | `bd gate check --type=gh:pr --json` | a text line followed by the envelope on stdout (the D-21 preamble case); stderr shows the `gh` not-authenticated failure |
| `error-readonly` | `bd --readonly update fx-0uu -s open --json` | exit 1, no stdout, stderr `Error: operation 'update' is not allowed in read-only mode` |
| `epic-status` | `bd epic status --json` | |
| `history` | `bd history fx-z7f --json` | one Dolt commit per write |
| `export` | `bd export` | JSONL, one issue per line, no envelope |

Facts observed while capturing that differ from Appendix A of
IMPLEMENTATION-PROGRESS.md (reported to the integrator, not changed here):
graph-plan nodes drop `acceptance_criteria` and `deps` as unknown fields, so
edges must use the top-level `edges` list; error envelopes are
`{"data":{"error":...}}` rather than `{"error","code","hint"}`, and some
refusals print only to stderr with an empty stdout; `bd ready` lists the run
epic itself.
