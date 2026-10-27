// Package grok implements cartridge.Cartridge on top of the Grok Build CLI.
// A firing is one headless streaming-json process in the agent worktree, with
// a temporary prompt file, native tool controls, an embedded agent instead of
// a shared leader, subagents disabled, and a clean GROK_*/XAI_* environment.
// Configured arguments cannot replace the protocol or policy controls.
// Read-only firings are refused: Grok 1.0.30 cannot guarantee a closed shell
// allowlist when always-approve is active, so the adapter fails closed rather
// than claiming enforcement it cannot verify.
//
// The parser accepts text, tool/tool_call, tool_update/tool_call_update,
// usage, end, and error events. Successful sessions require an end event;
// costs are reported only for complete sessions. Timeout, cancellation, and
// Kill use internal/proc process-group and descendant cleanup, and every path
// removes the temporary prompt file.
package grok
