// Package cursor implements cartridge.Cartridge on top of the Cursor Agent
// CLI: one firing is one "cursor-agent -p --output-format stream-json"
// process in the agent's working directory, with the prompt on stdin. It
// registers itself with the default registry under "cursor" from init
// (D-29, superseding the Claude-only scope of D-19).
//
// Every flag the adapter uses was verified against "cursor-agent --help" of
// [VerifiedVersion]; the flags, the error-kind table and the tool-policy
// story are recorded in docs/cartridges.md. The stream-json samples the
// parser tests replay were captured from that version and live in
// testdata/cursor.
//
// # Tool policy
//
// The CLI has no per-invocation tool controls (its permission rules live in
// a config file shared by every session of the user), so the adapter does
// not implement agents.ToolPolicyEnforcer. read_write needs no restriction;
// read_only and read_exec firings are run by the supervisor in a throwaway
// copy of the worktree (P6-20), which is what keeps a tester's edits out of
// the real tree.
//
// # Lifecycle
//
//   - [Adapter.Detect] resolves the binary, runs "cursor-agent --version"
//     (refusing anything older than [MinVersion]) and "cursor-agent status",
//     a local probe that says whether a login is configured.
//   - [Adapter.Run] validates the spec, builds the arguments with
//     [Adapter.Args], writes the system prompt and the prompt to stdin (the
//     CLI has no system-prompt flag) and streams the process through a
//     [Parser] into a cartridge.Pipe.
//   - [Classify] turns the exit status, the result line and any error text
//     into the typed cartridge error kinds; the substring table is
//     [TextRules].
package cursor
