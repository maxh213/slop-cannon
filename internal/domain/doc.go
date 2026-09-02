// Package domain holds the shared, dependency-free types that every other
// slop-cannon package builds on.
//
// The package is organised around the vocabulary of the README and the
// decisions recorded in IMPLEMENTATION-PROGRESS.md:
//
//   - [Role] and [Lens] name the kinds of agents (product owner, engineer,
//     tester) and the three reviewer perspectives (ux, qa, test).
//   - [Bead] is one unit of work tracked in the target repository's beads
//     (`bd`) database; [BeadState] mirrors the bd statuses of D-07 one to one
//     and also names the bd built-ins the cannon only ever reads, and
//     [BeadType] covers every issue type bd returns, so anything `bd` prints
//     from a repository decodes.
//   - [Agent] is a live LLM subprocess working on a bead; [Run] is one firing
//     of the cannon at a repository.
//   - [Verdict], [FeedbackItem], [Response], [Arbitration], [Escalation],
//     [ImplReport] and [Plan] are the structured messages agents exchange
//     through bead comments (D-06).
//
// Every enumeration is a string type with a String method, a ParseX function,
// and encoding.TextMarshaler / TextUnmarshaler implementations, so values
// marshal to and from JSON (and TOML, and map keys) as their canonical
// lower-case spelling. Parsing is lenient about case, surrounding whitespace
// and hyphens ("Needs-Human" parses as [BeadStateNeedsHuman]); marshalling
// only ever emits the canonical form. Unmarshalling an empty string yields the
// zero value so optional fields such as [Agent.Lens] round-trip cleanly, while
// ParseX rejects an empty string because user input must be explicit.
//
// [Priority] is the one numeric enumeration; its Go zero value is the most
// urgent priority, so [Bead] and [PlanBead] decode a missing priority as
// [PriorityDefault] rather than zero (D-22).
//
// The package has no I/O and imports only the standard library.
package domain
