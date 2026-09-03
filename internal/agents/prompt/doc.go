// Package prompt assembles the prompt one agent is fired with (P5-01).
//
// A prompt is layered, and the layers are always rendered in the same order
// so two runs with the same inputs produce byte-identical text:
//
//  1. the role base prompt, an embedded Markdown template per
//     [slop-cannon/internal/domain.Role] (plus a lens template for testers);
//  2. the assignment block: who the agent is, what it was fired to do, the
//     tool policy it runs under and the read-only beads note (D-21, P3-09);
//  3. the human's brief, when the run has one;
//  4. Rules, verbatim, framed as MUST;
//  5. Vibes, verbatim, framed as preferences;
//  6. repository context: root, default branch, detected stack, check
//     command, a tree summary and the head of the README;
//  7. the open beads the product owner must not duplicate;
//  8. the bead the agent owns;
//  9. the round history: reports, verdicts, responses and rulings so far;
//  10. the caller's task-specific instructions ([Section]);
//  11. the output contract from P2-07, appended with
//     [slop-cannon/internal/cartridge/structured.Contract.WithPromptSuffix].
//
// Rules, vibes and the output contract are never truncated: they are the
// parts an agent may not miss. When the assembled text exceeds the token
// budget the builder degrades the repository tree first, then the round
// history, then the README head, then the open-bead list, and records every
// step in [Prompt.Truncations].
//
// Every prompt is written to <runDir>/prompts/<agent-id>.md for debugging
// (D-23: runDir is the caller's run directory under XDG_STATE_HOME, never
// anything inside the target repository). Building a prompt does not write
// it: the caller that fires the agent must call [Prompt.Write] with its run
// directory, and a caller that does not leaves the run with no debug copy.
//
// Quoted context — the tree summary, the README head, a diff — is wrapped
// with [Fenced], which picks a fence longer than any run of backticks inside
// the content. Prompt layers assembled elsewhere ([Section] bodies) should
// use it too: a three-backtick wrapper around a README or a Markdown diff
// lets the content close the block early and turns the rest of the prompt
// into structure the agent misreads.
package prompt
