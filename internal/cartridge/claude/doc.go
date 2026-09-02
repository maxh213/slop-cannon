// Package claude implements cartridge.Cartridge on top of the Claude Code
// CLI: one firing is one "claude -p --output-format stream-json" process in
// the agent's worktree, with the prompt on stdin and the tool policy mapped
// onto Claude Code's permission rules. It registers itself with the default
// registry under "claude" from init.
//
// Every flag the adapter uses was verified against "claude --help" of
// [VerifiedVersion]; the list, the tool-policy mapping and the error-kind
// mapping table are recorded in docs/cartridges.md. The stream-json samples
// the parser tests run against were captured from that version and live in
// testdata/claude.
//
// # Lifecycle
//
//   - [Adapter.Detect] resolves the binary, runs "claude --version" (and
//     refuses anything older than [MinVersion]) and "claude auth status
//     --json", a local probe that says whether a login or API key is
//     configured.
//   - [Adapter.Run] validates the spec, builds the arguments with
//     [Adapter.Args] and a clean environment with [InheritEnv], and streams
//     the process through a [Parser] into a cartridge.Pipe. Cancelling the
//     context, the spec timeout, or Session.Kill stops the process group.
//   - [Classify] turns the exit status, the result line and any error text
//     into the typed cartridge error kinds; the substring table is
//     [TextRules] and the HTTP status map is [KindForStatus].
//
// # Environment
//
// The child sees only PATH, the cartridge.DefaultInheritEnv variables, the
// Claude-related variables listed by [InheritEnv] (ANTHROPIC_*, CLAUDE_*,
// AWS_*, GOOGLE_* and a few named knobs), [DefaultEnv], the configured
// [cartridges.claude] env and the RunSpec env. The markers a running Claude
// Code session sets for its children (CLAUDECODE, CLAUDE_CODE_SESSION_ID,
// ...) are always dropped so an agent never attaches to the session sc was
// launched from.
package claude
