// Package dandelion implements cartridge.Cartridge on top of dandelion, the
// user's quota router: every firing first runs "dandelion route [--high]",
// which prints the account and model that still have quota
// ("claude-fable-5-1 max claude"), and then runs the session through the
// Claude Code or Cursor Agent adapter for that account. It registers
// itself with the default registry under "dandelion" from init (D-30).
//
// One cartridge name stands for many accounts, which shapes everything
// else; docs/cartridges.md records the details and D-30 the trade-offs.
//
//   - Mode: [cartridges.dandelion] model is "high" (route --high) or
//     "standard"/empty (route); the routed model wins over RunSpec.Model,
//     so roles.<role>.model is ignored for this cartridge.
//   - Accounts: [Providers] classifies every dandelion provider id. Claude
//     accounts run through internal/cartridge/claude (claude-work,
//     claude-deepseek and claude-ollama with the CLAUDE_CONFIG_DIR that
//     dandelion's launch.ts gives them), cursor through
//     internal/cartridge/cursor, and every other id is passed to the router
//     in DANDELION_DISABLE so it is never probed or named.
//   - Tool policy: read_only and read_exec firings may only be routed to
//     Claude accounts, whose flags hold them natively; cursor is disabled
//     for them too, and a route naming a disallowed account is no route.
//   - Cost: one router call probes every enabled provider (about six
//     seconds), so an answer is cached per mode and account set for
//     [CacheTTL], concurrent firings share one call, and --max-age lets a
//     running dandelion dashboard answer from its snapshot.
//   - Errors: "none" is a rate limit with [NoRouteRetryAfter]; a bad
//     routes.json is not_installed (a human must fix it); an account that
//     runs out of credits or is logged out is a short rate limit, never
//     credits_exhausted or auth, because the supervisor retires or fails a
//     cartridge by name. Such an account is benched (left out of routing)
//     until its reset time, or [BenchFor].
package dandelion
