package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// NewRootCommand builds the sc command tree. Output is written to out so
// tests can capture it.
func NewRootCommand(out io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "sc",
		Short:         "Slop Cannon fires LLM agents at a repository",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(out)
	root.SetErr(out)
	root.AddCommand(newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sc version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "sc %s (commit %s, built %s)\n", Version, Commit, Date)
			return err
		},
	}
}

// Execute runs the root command with os.Args and returns the process exit
// code. Errors are printed to stderr.
func Execute() int {
	root := NewRootCommand(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sc:", err)
		return 1
	}
	return 0
}
