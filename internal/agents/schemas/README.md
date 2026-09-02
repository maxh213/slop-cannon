# Agent output schemas

This directory holds the JSON Schema files (draft 2020-12) for every
structured message an agent must end its reply with: `plan`, `impl_report`,
`verdict`, `response`, `arbitration` and `escalation` (D-06, D-25).

## Ownership

- **P2-07** (`internal/cartridge/structured`) owns the *mechanism*: the
  `sc-result` fenced-block contract, the prompt suffix, the extractor, the
  schema validator (`github.com/santhosh-tekuri/jsonschema/v6`), the retry
  loop, and this README.
- **P5-07** owns the *schemas*: one `<name>.schema.json` per message type in
  this directory, the Go package that embeds them (`//go:embed *.json` in a
  `schemas.go` here) and hands the embedded `fs.FS` to
  `structured.LoadFS`, the Go structs, the validation tests, and the
  published copies under `docs/schemas/`.
- **P5-08** owns the role-aware `RunAgent` helper that composes
  `structured.Fire` with the per-role contract.

Until P5-07 lands there are no schema files here; the structured package's
own tests use fixtures under `internal/cartridge/structured/testdata/schemas`.

## Conventions for schema files

- File name `<message type>.schema.json`; the contract name is the file name
  without `.schema.json` (`verdict.schema.json` -> `verdict`), which must
  equal the `type` value the bead comment writer (P3-03) stamps on the
  message.
- Every file is registered under the synthetic base `sc://schemas/` before
  compilation, so cross-file references are plain relative `$ref`s
  (`"$ref": "item.schema.json"` or `"$ref": "item.schema.json#/$defs/x"`).
  Set `"$id": "sc://schemas/<file>"` to match. No `http(s)` references: the
  compiler has no network loader and the build must stay offline.
- Set `"$schema": "https://json-schema.org/draft/2020-12/schema"`; the
  metaschemas are bundled with the library.
- `format` assertions are enforced (`AssertFormat`), so only use formats the
  library implements (date-time, email, uri, uuid, regex, ...).
- Prefer `additionalProperties: false` and short `description`s: the whole
  schema file is pasted into the agent prompt by
  `Contract.PromptSuffix`, so keep it self-contained and readable. Only the
  contract's own file is shown, so a `$ref` to another file is invisible to
  the model; inline shared definitions under `$defs` when the model needs to
  see them.
- Enum values must match `internal/domain` (`domain.VerdictOutcomes()`,
  `domain.Severities()`, ...); P5-07's tests should assert that.
