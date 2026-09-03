# Tester prompt goldens (P5-04)

One assembled prompt per lens and mode — the nine firings a tester can have —
produced by `internal/agents/tester_test.go` from fixtures defined in that
file:

| Lens | `review` | `re_review` | `evaluate_pushback` |
|---|---|---|---|
| `ux` | `ux-review` | `ux-re_review` | `ux-evaluate_pushback` |
| `qa` | `qa-review` | `qa-re_review` | `qa-evaluate_pushback` |
| `test` | `test-review` | `test-re_review` | `test-evaluate_pushback` |

They pin what each tester is actually told: its lens template, the rules
(framed as MUST, with the mandatory `rules` item category) and the vibes, the
repository and bead context, the round history, the diff from
`internal/gitx`, the items under judgement with the engineer's responses, and
the mode's own instructions.

Each golden stops just before the output contract, which
`TestTesterPromptCarriesEveryLayer` pins separately with
`Contract.HasPromptSuffix`, so editing `internal/agents/schemas/verdict.schema.json`
does not rewrite all nine files.

Regenerate them with `make update-golden` (`UPDATE_GOLDEN=1 go test ./...`)
after an intended change, and read the diff before committing it.
