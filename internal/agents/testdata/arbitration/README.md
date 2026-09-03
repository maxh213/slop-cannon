# Product owner ruling prompts (P5-05)

Two assembled prompts, produced by `arbitration_test.go` from the fixtures in
that file:

| File | Mode | Task |
|---|---|---|
| `po-arbitrate.golden.md` | one disputed feedback item | `arbitrate` |
| `po-escalate.golden.md` | a bead out of review rounds | `escalate` |

They pin the wording of both rulings, the read-only tool policy, and the fact
that the real `arbitration` / `escalation` output contract reaches the agent —
so a change to `internal/agents/schemas/{arbitration,escalation}.schema.json`
rewrites the tail of these files, which is the point: the schema is what the
product owner is actually shown.

Regenerate with `make update-golden` (`UPDATE_GOLDEN=1 go test ./...`) after an
intended change, and read the diff before committing it.
