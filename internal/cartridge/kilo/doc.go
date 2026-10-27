// Package kilo adapts Kilo CLI 7.5.6's non-interactive JSON protocol.
//
// Each firing uses an in-memory generated agent configuration, native
// permissions, and the complete combined prompt on stdin. Project config,
// external plugins, daemons, and automatic updates are disabled. read_only is
// refused because Bash cannot provide a read-only execution boundary.
//
// SC_KILO_MIN_BALANCE_USD is an opt-in Dandelion control. When set, every
// firing performs a fresh profile probe and receives only the balance above
// that floor as its allowance. The allowance is a best-effort in-session cost
// guard, not an account-wide guarantee: a final request can overshoot before
// its cost is reported, and concurrent sessions can spend the same balance.
// The control is never passed to the Kilo child process.
package kilo
