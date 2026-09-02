// Package structured is the output contract between an agent prompt and the
// orchestrator: the model is asked to end its reply with one fenced block
//
//	```sc-result
//	{ ...JSON... }
//	```
//
// whose JSON matches a named JSON Schema, and this package turns a
// cartridge's final text back into validated JSON.
//
// The pieces, in the order a caller meets them:
//
//   - [Contract] pairs a schema name with its compiled JSON Schema (draft
//     2020-12 by default). [Contract.PromptSuffix] is the paragraph the
//     prompt builder (P5-01) appends so the model knows the rules and the
//     schema; [Contract.Parse] extracts and validates the block from a
//     [cartridge.Result].Output.
//   - [Registry] loads every schema file of an fs.FS (the embedded
//     internal/agents/schemas directory, P5-07) into contracts so `$ref`s
//     between files resolve without touching the network.
//   - [Extract] is the schema-free half of Parse: it finds the block in
//     prose-wrapped, double-block, unclosed and fence-less outputs and
//     reports [ParseError]s a retry prompt can quote.
//   - [Fire] runs a cartridge session for a contract and, when the reply has
//     no usable block, fires once more with the validation error fed back
//     (one retry by default). P5-08 builds the role-aware RunAgent helper
//     on top of it.
//
// Every failure to obtain a result is a *[ParseError] with a [Reason];
// errors.Is works against [ErrMissing], [ErrTruncated], [ErrInvalidJSON] and
// [ErrSchema]. Cartridge failures (crash, timeout, rate limit, credits) pass
// through untouched as *[cartridge.Error] so the supervisor's policy still
// applies.
package structured
