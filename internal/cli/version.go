package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Build metadata injected by the Makefile through -ldflags
// (-X slop-cannon/internal/cli.{Version,Commit,Date}). The defaults are used
// by plain "go build" / "go run". Do not rename these (P1-10).
var (
	// Version is the semantic version or git describe output of this build.
	Version = "dev"
	// Commit is the short git commit hash of this build.
	Commit = "none"
	// Date is the UTC build timestamp in RFC 3339 format.
	Date = "unknown"
)

// VersionInfo is the --json shape of `sc version`.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func init() { register(newVersionCommand) }

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sc version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, err := GlobalOpts(cmd)
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeJSON(cmd.OutOrStdout(), VersionInfo{Version: Version, Commit: Commit, Date: Date})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "sc %s (commit %s, built %s)\n", Version, Commit, Date)
			return err
		},
	}
}
