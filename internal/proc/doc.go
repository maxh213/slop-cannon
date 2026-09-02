// Package proc runs external processes with an explicit environment
// allowlist, line-by-line streaming, timeouts, and process-group cancellation.
//
// Every subprocess that sc starts (git, gh, bd, cartridge CLIs) goes through
// this package so that the rules are the same everywhere:
//
//   - The child sees only the environment described by [Spec]: PATH from the
//     parent, the variables named in [Spec.InheritEnv], and the explicit
//     values in [Spec.Env]. Nothing else leaks, which keeps tokens out of
//     agent subprocesses by default.
//   - The child is the leader of its own process group. On timeout,
//     cancellation, or [Handle.Kill] the whole group is signalled, so shells
//     and the grandchildren they spawn die together.
//   - [Run] captures stdout and stderr and returns them in [Result].
//     [Stream] delivers each line as it arrives on [Handle.Lines], tagged
//     with the stream it came from, so callers can log or render output live.
//   - Exit codes and wall-clock durations are always recorded, even when the
//     process was killed.
//
// [LookPath], [Probe], and [ParseVersion] cover the "is tool X installed and
// new enough?" checks that sc doctor and the adapters perform.
package proc
