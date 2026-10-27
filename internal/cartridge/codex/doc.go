// Package codex implements cartridge.Cartridge on top of the Codex CLI's
// "codex exec --json" protocol. Only the personal codex cartridge is
// registered directly. Dandelion delegates codex-animus through the same
// factory with its separate CODEX_HOME.
//
// Codex's read-only sandbox still permits shell commands, so it cannot hold
// cartridge.ToolReadOnly. ToolReadExec uses read-only and ToolReadWrite uses
// workspace-write; both disable interactive approval through native config.
package codex
