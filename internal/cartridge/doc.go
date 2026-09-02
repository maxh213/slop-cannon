// Package cartridge defines the adapter-agnostic contract between the
// orchestrator and an LLM CLI: the [Cartridge] interface, the [RunSpec] that
// describes one firing, the [Session] that streams what the CLI does, the
// typed error kinds every adapter must map its CLI's failures onto, and the
// [Registry] that builds cartridges by name from configuration.
//
// Only the Claude Code adapter ships today (D-19); the mock adapter drives
// tests. Both live in subpackages and register themselves with the
// [Default] registry. Adding an adapter means implementing [Cartridge] in a
// new subpackage, adding the compile-time assertion
//
//	var _ cartridge.Cartridge = (*Adapter)(nil)
//
// and one [Register] call; nothing in the orchestrator changes.
//
// # Error kinds
//
// Every failure a cartridge reports, from [Cartridge.Detect], [Session.Wait]
// or an [EventError] event, is a *[Error] whose Kind classifies it. The
// orchestrator's supervisor (P6-07) acts on the kind alone:
//
//   - [KindRateLimited] ([ErrRateLimited]): the CLI reported a temporary
//     rate limit. Back off and retry; the cartridge is still alive.
//   - [KindCreditsExhausted] ([ErrCreditsExhausted]): quota, billing or
//     credits are exhausted. The cartridge is dead for the rest of the run;
//     when every cartridge is dead an autopilot run ends (D-18).
//   - [KindAuth] ([ErrAuth]): the CLI is not logged in or its token was
//     rejected. Not retryable without human action.
//   - [KindCrash] ([ErrCrash]): the process exited non-zero, was killed by a
//     signal it did not ask for, or produced output the adapter could not
//     parse. Retry with backoff.
//   - [KindTimeout] ([ErrTimeout]): [RunSpec.Timeout] elapsed. Retry.
//   - [KindKilled] ([ErrKilled]): [Session.Kill] was called or the context
//     was cancelled. Deliberate; never retried automatically.
//   - [KindNotInstalled] ([ErrNotInstalled]): the CLI binary is not on PATH
//     (Detect only).
//
// Each adapter documents which of its CLI's messages map onto which kind.
// Test for a kind with errors.Is against the sentinel or with [KindOf].
package cartridge
