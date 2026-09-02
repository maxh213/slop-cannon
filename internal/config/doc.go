// Package config loads, merges, and saves slop-cannon configuration
// (TOML) with the precedence from D-03: CLI flags, repo config, global config,
// built-in defaults.
//
// Two files exist. The global file at $XDG_CONFIG_HOME/slop-cannon/config.toml
// ([GlobalPath]) holds the cartridges (they are subscriptions, so they never
// belong to one repository) and any personal defaults. The repo file at
// <repo>/.slop-cannon/config.toml ([RepoConfigPath]) is the one committable
// file and may set ammo, safety, vibes, rules, check_command, limits and
// role overrides for that repository. [Load] merges both with the built-in
// defaults ([Defaults]) into an [Effective] that records, per key, which
// layer supplied the value, and [Effective.Apply] layers CLI flags on top.
//
// Subcommands that write configuration use [Target] to pick the file (repo
// when the working directory is inside a git repo, global otherwise or with
// --global), [Open] to read it into a [Doc], the Doc's helpers (AddCartridge,
// SetAmmo, AddVibes, ...) to change it, and [Doc.Save] to write it back
// atomically. Files carry a schema_version key; older versions are migrated
// on load through the hook in migrate.go.
//
// Run state never lives inside the target repository (D-23): [StateDir]
// returns the per-repository directory under $XDG_STATE_HOME/slop-cannon/.
package config
