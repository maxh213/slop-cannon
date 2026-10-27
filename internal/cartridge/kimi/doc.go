// Package kimi adapts Kimi CLI's non-interactive stream-json protocol.
//
// Each firing gets a private Markdown agent profile and staged prompt file.
// The profile keeps the built-in system prompt and bounds tools and subagents.
// Additional instructions join the user input literally. Files are removed on
// every path. Inputs beyond the safe argv budget stay in their workdir-private
// mode-0600 file and are loaded through Kimi's native Read tool.
//
// ToolReadExec and ToolReadWrite have native tool-list enforcement.
// ToolReadOnly is deliberately unavailable: removing Bash breaks the current
// planner, while allowing unrestricted Bash would not be read-only. Kimi does
// not report usage in this protocol, so Result.Usage stays zero.
package kimi
