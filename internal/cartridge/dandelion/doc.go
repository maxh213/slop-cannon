// Package dandelion implements cartridge.Cartridge on top of dandelion, the
// user's quota router: every firing first runs "dandelion route [--high]",
// which prints the account and model that still have quota
// ("claude-fable-5-1 max claude"), and then runs the session through the
// adapter registered for that account's driver. It registers itself with the
// default registry under "dandelion" from init (D-30).
//
// One cartridge name stands for many accounts, which shapes everything
// else; docs/cartridges.md records the details and D-30 the trade-offs.
//
//   - Mode: [cartridges.dandelion] model is "high" (route --high) or
//     "standard"/empty (route); the routed model wins over RunSpec.Model,
//     so roles.<role>.model is ignored for this cartridge.
//   - Accounts: [Providers] classifies every dandelion provider id and its
//     driver. Claude and Codex Animus accounts receive their isolated config
//     homes, and each driver's route mapper passes only supported effort.
//     Unavailable binaries are passed in DANDELION_DISABLE. Kilo launches
//     require a fresh route and balance profile on every firing.
//   - Tool policy: read_only may use Claude only; read_exec may use Claude,
//     Codex, Kimi, Grok, Hermes and Kilo; read_write may use every driver. agy, Grok
//     and Junie have no read_only routes. A route naming an account the policy
//     or current binary availability does not allow is no route.
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
