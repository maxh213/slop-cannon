// Package events defines the run event model and the in-process [Bus] that
// fans events out to subscribers (TUI, headless renderer, JSONL run log).
//
// Every notable thing the orchestrator does is published as one immutable
// value implementing [Event]: the run starting or finishing, a plan landing,
// beads changing state, agents being fired and coming back, verdicts,
// responses, arbitrations, PRs and merges, plus [Warning] and [Error] for
// anything that deserves a human's eye. Each concrete event embeds [Meta],
// which carries the timestamp and the run id, and answers [Event.Kind] with a
// stable snake_case [Kind] such as "bead_state_changed".
//
// # Bus
//
// [Bus.Publish] never blocks and never panics: it takes one lock, offers the
// event to every live subscription with a non-blocking send, and returns. A
// subscriber whose buffer is full misses that event and its [Subscription.Dropped]
// counter grows, so a stalled TUI can never slow the orchestrator down. Every
// subscriber that keeps up sees the same events in the same order, and that
// order is the order in which Publish calls returned. Subscriptions end when
// their context is cancelled, when [Subscription.Unsubscribe] is called, or
// when the bus is closed; in every case the event channel is closed so
// range loops terminate.
//
// # JSONL
//
// [Marshal] and [Unmarshal] convert events to and from a one-object JSON
// envelope: the event's own fields plus a leading "kind" discriminator. Every
// kind round-trips, and [Kinds] lists them all so tests and fake generators can
// enumerate. [JSONLWriter] is a ready-made subscriber that appends one line per
// event to any io.Writer and flushes immediately (D-15); [JSONLReader] reads
// such a file back, which is how `sc status` and the golden tests replay a run.
//
// The package imports only the standard library and internal/domain.
package events
