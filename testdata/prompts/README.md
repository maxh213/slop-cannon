# Prompt goldens

One assembled prompt per role, produced by `internal/agents/prompt` (P5-01)
from the fixtures in `builder_test.go`:

| File | Role | Task |
|---|---|---|
| `po-plan.golden.md` | product owner | `plan` |
| `engineer-implement.golden.md` | engineer | `implement` |
| `tester-qa-review.golden.md` | tester, `qa` lens | `review` |

They pin the layer order, the wording of the embedded role templates, and the
fact that rules, vibes, the repository context, the bead, the round history
and the output contract all reach the agent.

Regenerate them with `make update-golden` (`UPDATE_GOLDEN=1 go test ./...`)
after an intended change, and read the diff before committing it: these files
are what every agent in a run is actually told.

Every `.golden.md` in this directory must be produced by a named test:
`TestPromptGoldenFilesAreAccountedFor` in
`internal/agents/prompt_goldens_test.go` lists each one against the test that
writes it and fails on a file that is orphaned, missing or unreadable. Rename
a fixture and you edit `pgGoldens` in the same change. `docs/goldens.md`
covers the whole family, including the tester and planning goldens elsewhere
in the tree.

The output contract at the end of each golden belongs to a small stand-in
schema defined in the test, not to a real message schema, so that changing
`internal/agents/schemas/*.json` does not rewrite every prompt golden. The
real contracts are covered by `TestRealContractsAreAppended`.

## Engineer agent goldens (P5-03)

`agent-engineer-implement.golden.md`, `agent-engineer-address-feedback.golden.md`
and `agent-engineer-resolve-conflict.golden.md` are the three engineer modes,
produced by `internal/agents/engineer_test.go`. They stop just before the
output contract, which is pinned separately by
`TestEngineerGolden*`/`engRequireContract`, so an edit to
`internal/agents/schemas/*.json` does not rewrite them either.
