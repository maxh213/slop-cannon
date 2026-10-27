// Package dash serves a read-only, browser dashboard for one recorded run.
// One observer follows the run's event log into tui.State and backfills and
// follows the structured sc.log. For each Dandelion agent launch it freezes the
// spawn time and current/previous direct event starts, selects a generation in
// the timestamp interval (previous event start, current event start], and then
// selects exact agent/supervisor-attempt history within that generation. This
// preserves prior history across event-start rotation, and older last-seen
// cards keep their own frozen interval and model. Invalid intervals, arrival
// skew, duplicates, and missing, ambiguous or damaged history are unknown,
// never guessed into pending or routed. Route history is bounded and
// metadata-only; requested and routed models remain distinct, with no global
// or latest-route guessing.
// The observer periodically combines these streams with durable state and
// lock probes. A host collector adds bounded CPU, memory, swap and root-disk
// samples; failed or unsupported collection preserves the last-good snapshot
// and never changes observer health. An optional Dandelion quota collector
// feeds bounded, memory-only last-good data into the same observer loop, with
// the same health isolation. HTTP handlers only read bounded, redacted,
// deep-copied snapshots; they never read or replay logs or control the run.
package dash
