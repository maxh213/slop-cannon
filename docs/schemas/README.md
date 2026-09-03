# Agent output schemas

Every slop-cannon agent ends its reply with one fenced ` ```sc-result ` block
containing a single JSON object. This directory publishes the JSON Schema
(draft 2020-12) each of those objects must satisfy, plus worked examples.

The files here are **generated copies** of the schemas embedded in the `sc`
binary at `internal/agents/schemas/`. A test fails when they drift;
`make update-golden` refreshes them. Edit the embedded originals, never these.

## The six messages

There is one schema per structured message type (D-06, D-25). The file name
without `.schema.json` is both the contract name the prompt refers to and the
`type` field the orchestrator stamps on the message when it stores it as a
bead comment.

| Message | Schema | Written by | Read by |
|---|---|---|---|
| `plan` | [plan.schema.json](plan.schema.json) | product owner (autopilot or brief mode) | the beads graph-plan builder (`bd create --graph`, D-22/D-24) |
| `impl_report` | [impl_report.schema.json](impl_report.schema.json) | engineer, after implementing a bead | the three testers |
| `verdict` | [verdict.schema.json](verdict.schema.json) | tester, one per lens per round | the feedback loop and the engineer |
| `response` | [response.schema.json](response.schema.json) | engineer, answering feedback items | the originating testers, then the product owner |
| `arbitration` | [arbitration.schema.json](arbitration.schema.json) | product owner, on a disputed item | engineer and tester; the ruling is binding (D-08) |
| `escalation` | [escalation.schema.json](escalation.schema.json) | product owner, when a bead runs out of review rounds | the orchestrator, and a human for `block` |

## Conventions

- Draft 2020-12, `"$id": "sc://schemas/<file>"`. No `http(s)` references: the
  validator has no network loader and the build stays offline.
- `additionalProperties: false` everywhere, and shared definitions are inlined
  under `$defs` in the same file. The whole file is pasted into the agent's
  prompt, so anything it cannot see it cannot obey.
- Every schema accepts an optional `type` property pinned to its own name, so
  the object stored in a bead comment (which carries `type`) validates against
  the same schema as the agent's bare result.
- Enum values are the `internal/domain` enums; a test asserts the two agree, so
  a new severity or lens cannot be added on one side only.
- Priorities are bd's scale: `0` most urgent, `4` least, `2` the default (D-22).
- Sizes are `s`/`m`/`l`, plan types are bd's plannable core types
  (`task`, `bug`, `feature`, `chore`).

## Conditional rules worth knowing

These are enforced by the schemas, not just by convention:

- A `verdict` whose `outcome` is `changes` must carry at least one item.
- A feedback item in the `rules` category must have severity `blocker`: a
  rules-file violation is never negotiable.
- A `response` of `pushback` or `wontfix` must state a `rationale`.
- An `arbitration` of `compromise` must carry the `instruction` that replaces
  both positions.
- An `escalation` must fill the field that goes with its outcome:
  `approve_with_notes` needs `notes`, `narrow_scope` needs `new_acceptance`,
  `block` needs `summary`.
- A plan bead always has a non-empty `key` (no whitespace), `title` and
  `acceptance`; `deps` names other keys in the same plan.

## Examples

`examples/valid/` holds documents that validate. They double as the examples
in this documentation and as test fixtures.

| Example | Shows |
|---|---|
| [plan-brief-mode.json](examples/valid/plan-brief-mode.json) | three beads with a dependency chain, types, priorities and sizes |
| [plan-nothing-to-add.json](examples/valid/plan-nothing-to-add.json) | the empty plan autopilot uses to mean "nothing new this round" (D-18) |
| [impl_report-full.json](examples/valid/impl_report-full.json) | every optional field, including `type` as stored in a bead comment |
| [impl_report-minimal.json](examples/valid/impl_report-minimal.json) | the smallest legal report: a summary |
| [verdict-approve.json](examples/valid/verdict-approve.json) | an approval that still carries a settled nit |
| [verdict-changes.json](examples/valid/verdict-changes.json) | a rules blocker plus a coverage item |
| [response-mixed.json](examples/valid/response-mixed.json) | `fixed`, `pushback` and `wontfix` side by side |
| [arbitration-compromise.json](examples/valid/arbitration-compromise.json) | a compromise with the instruction that settles it |
| [arbitration-uphold-tester.json](examples/valid/arbitration-uphold-tester.json) | a ruling with no instruction needed |
| [escalation-approve-with-notes.json](examples/valid/escalation-approve-with-notes.json) | shipping with what is left recorded |
| [escalation-narrow-scope.json](examples/valid/escalation-narrow-scope.json) | replacing the acceptance criteria |
| [escalation-block.json](examples/valid/escalation-block.json) | handing the bead to a human |

`examples/invalid/` holds documents that must be rejected. Each has a
`.expected` file with the exact error the validator produces, so the error text
itself is reviewed and cannot silently become unreadable.

| Example | Broken rule |
|---|---|
| [plan-empty-acceptance.json](examples/invalid/plan-empty-acceptance.json) | acceptance criteria present but empty |
| [plan-priority-out-of-range.json](examples/invalid/plan-priority-out-of-range.json) | priority 7 is off bd's 0–4 scale |
| [plan-key-with-whitespace.json](examples/invalid/plan-key-with-whitespace.json) | a key with a space cannot be a graph-plan node key |
| [plan-unknown-field.json](examples/invalid/plan-unknown-field.json) | `acceptance_criteria` instead of `acceptance` (the node field bd 1.2.2 rejects, D-24) |
| [plan-non-plannable-type.json](examples/invalid/plan-non-plannable-type.json) | `epic` is the run's own bead, not a plan item |
| [impl_report-missing-summary.json](examples/invalid/impl_report-missing-summary.json) | no summary |
| [impl_report-commit-not-a-sha.json](examples/invalid/impl_report-commit-not-a-sha.json) | `HEAD~1` is not a commit sha |
| [verdict-changes-without-items.json](examples/invalid/verdict-changes-without-items.json) | changes requested with nothing to fix |
| [verdict-rules-item-not-a-blocker.json](examples/invalid/verdict-rules-item-not-a-blocker.json) | a `rules` item filed as `minor` |
| [verdict-unknown-outcome.json](examples/invalid/verdict-unknown-outcome.json) | `reject` is not an outcome |
| [response-pushback-without-rationale.json](examples/invalid/response-pushback-without-rationale.json) | disagreement without a reason |
| [response-no-responses.json](examples/invalid/response-no-responses.json) | an empty answer to feedback |
| [arbitration-compromise-without-instruction.json](examples/invalid/arbitration-compromise-without-instruction.json) | a compromise nobody can act on |
| [escalation-block-without-summary.json](examples/invalid/escalation-block-without-summary.json) | blocking without telling the human why |
| [escalation-narrow-scope-without-acceptance.json](examples/invalid/escalation-narrow-scope-without-acceptance.json) | narrowing scope without new criteria |

## Using them from Go

```go
c, err := agents.ContractFor(domain.MessageVerdict) // compiled schema
prompt = c.WithPromptSuffix(prompt)                 // show it to the model
verdict, parsed, err := agents.ParseVerdict(output) // read the reply back
```

`agents.ValidateJSON` checks a raw object, `agents.DecodeJSON` validates and
decodes it, and a failure is a `*structured.ParseError` whose `Feedback()` is
the paragraph the retry prompt shows the model.
