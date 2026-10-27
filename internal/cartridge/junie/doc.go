// Package junie implements cartridge.Cartridge on top of the Junie CLI.
// Each firing is one non-interactive --task process with --output-format json.
// The final JSON object is required: its result is the assistant output, its
// errors are classified, and its llmUsage entries are summed into cartridge
// usage and cost. Junie 26.9.22 has no MaxTurns equivalent; that limit is ignored.
// Prompts over 32 KiB are written exactly to a private project-local file and
// replaced on argv by an instruction to read and follow that file. This avoids
// the exec argument limit, but depends on Junie honoring the file-read
// instruction; offline tests cannot verify model compliance with that indirection.
//
// Project and user configuration, MCP servers, custom models, skills,
// commands, agents, and extensions are not loaded from their default
// locations. Non-interactive Junie treats the project as trusted, so callers
// must only fire it in worktrees they already trust.
//
// Only read_write is supported. Interactive plan mode and the user's global
// action allowlist are not native read_only or read_exec enforcement.
// Timeout, cancellation, and Kill use internal/proc process-group and tagged
// descendant cleanup; the adapter never invokes gateway lifecycle commands.
package junie
