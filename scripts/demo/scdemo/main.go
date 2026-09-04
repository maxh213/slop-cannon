// Command sc-demo is the sc binary the deterministic demo drives (P8-07).
//
// It is `sc` with two development-only differences, and nothing else:
//
//   - it imports internal/cartridge/mock, whose init registers the mock
//     adapter in cartridge.Default, so a run can be scripted from a JSON
//     scenario instead of calling a real model;
//   - it adds "mock" to config.KnownCartridges, so `active_cartridge =
//     "mock"` resolves instead of being refused as an unsupported name.
//
// Both are deliberately absent from cmd/sc: a user must never be able to
// point the shipped binary at a scripted cartridge and mistake the result
// for work. scripts/demo/demo.sh builds this binary into a temporary
// directory and throws it away with the rest of the demo's machine.
//
// Everything else — the run assembly, the TUI, the orchestrator, git, the
// bd client — is the production code path, which is the point: the demo
// records what sc actually does, not a re-enactment of it.
package main

import (
	"os"

	_ "slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cli"
	"slop-cannon/internal/config"
)

func main() {
	config.KnownCartridges = append(config.KnownCartridges, "mock")
	os.Exit(cli.Execute())
}
