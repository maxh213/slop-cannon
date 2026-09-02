// Command sc is the Slop Cannon CLI/TUI entry point.
package main

import (
	"os"

	"slop-cannon/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
