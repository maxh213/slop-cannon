// Package conformance runs a fixed set of checks against any
// cartridge.Cartridge and reports a pass/fail table, so every adapter is
// held to the same contract before the orchestrator relies on it (P2-09).
//
// The checks fire real sessions with fixed prompts (see [Checks]): Detect
// must report a ready CLI; a session must echo a token back; a session
// must end with a valid structured ```sc-result block (through
// structured.Fire, so the retry path is exercised too); a read_write
// session must create a file in its workdir; the same request under
// read_only must leave no file behind; a hung session must end with
// cartridge.ErrTimeout when RunSpec.Timeout elapses; and Session.Kill must
// end a hung session with cartridge.ErrKilled. Every session is also held
// to the event-stream contract: exactly one terminal event, last, matching
// Wait's outcome.
//
// The kill check uses Session.Kill and never a cancelled context, because
// an adapter is only required to notice a cancellation while it still has
// work to do: a CLI whose process exited a moment before the signal
// reports its result, not a kill. The mock is explicit about the same
// window (see the mock package doc, P2-11), so a scripted rule that must
// end with cartridge.ErrKilled always keeps a step in hand.
//
// [Run] executes the suite against one cartridge and returns a [Report];
// [Report.Table] renders it for a terminal. The suite runs against the mock
// cartridge in the normal test run ([MockScenario] scripts replies for
// every prompt) and against a real CLI with
//
//	make cartridge-conformance CARTRIDGE=claude
//
// which is never run in CI. See docs/cartridges.md for the expected output.
package conformance
