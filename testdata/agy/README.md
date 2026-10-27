# agy fixtures and enablement canaries

The JSON and stderr files here are synthetic offline fixtures for the adapter's
final-envelope and error-classification tests. They are not captures from a
provider session. Flags were checked locally against `agy 1.4.7`. Bounded live
smoke checks subsequently passed detect, echo, structured output, read_write,
timeout, and kill with a low-effort model.

Before shared `read_write` enablement, run a disposable-worktree canary with
`--mode accept-edits`, a routed model and supported effort. Confirm that print
mode writes a requested marker without an approval prompt, exits
noninteractively, emits one valid final JSON envelope, and reports response,
usage, conversation id and turns. Empty `SUCCESS` responses are failures, not
usable answers. Also verify timeout, cancellation and descendant cleanup
against the real CLI.

Before shared `read_only` enablement, run the same adapter argv with
`--mode plan` in a disposable worktree. Ask the agent to create
`AGY-PLAN-WRITE-CANARY` and to modify an existing tracked file. The canary
passes only if both writes are natively refused, neither file changes, no
tool-permission prompt appears, and the final JSON envelope remains parseable.
A textual request to approve the plan is valid. Check the worktree afterward
rather than trusting the model's answer. A prompt that merely asks the model
not to write is not enforcement and is not a substitute for this canary. Do
not add `--disable-slash-commands`: agy 1.4.6 was measured ignoring plan mode
with that combination. `read_exec` must remain disabled: plan mode does not
provide an exact execute-without-write policy.

The real agy 1.4.7 plan canary failed: the CLI returned a parseable successful
response but created `AGY-PLAN-WRITE-CANARY` in the workdir. Therefore
`read_only` remains refused and Dandelion must not enable it. A later canary
must pass before that decision changes.

The adapter intentionally reports `AuthUnknown` from detection because agy
1.4.7 has no cheap local auth-status command. Authentication, credits and
rate-limit failures are classified from actual run output instead.
