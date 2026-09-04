package cli

import (
	"github.com/spf13/cobra"
)

// `sc resume` (P8-05) is sugar, and deliberately nothing more: it is
// `sc <repo> --resume`, with the same flags, the same resolution
// ([ResolveRunWithNotes], which puts the frozen ConfigSnapshot back, P6-11)
// and the same firing path ([runTarget]). A user who has just been told
// "the run was killed" by `sc status` should not have to remember that
// continuing it is spelled as a flag on the run form.
//
// Everything it does happens in run.go; this file only sets the flag. That
// is the point — a second implementation of "resume" would be a second
// thing to keep in step with the first.

func init() { register(newResumeCommand) }

const resumeLong = `Continue the most recent run of a repository.

This is exactly "sc PATH --resume": the run keeps its id, its brief and the
configuration it was started with (a config file edited since does not
change it), its worktrees are reattached, stale leases are recovered and
the agents that were in flight are fired again.

PATH may be the repository root or any directory inside it; without it the
--repo directory (the current one by default) is used. --ammo, --safety and
--cartridge still override the frozen configuration for this invocation and
say so. There is nothing to resume in a repository sc has never run in.`

const resumeExamples = `sc resume
sc resume ./repo-path
sc resume --repo ./repo-path --headless`

// newResumeCommand builds `sc resume [PATH]`.
func newResumeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "resume [PATH]",
		Short:   "Continue the most recent run of a repository",
		Long:    resumeLong,
		Example: indent(resumeExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runResume,
	}
	addRunFlags(cmd)
	// --resume is what the command is, and -m is refused with it (a
	// resumed run keeps its original brief), so neither belongs in this
	// command's help. Both still parse, so a user who types one gets the
	// explanation rather than "unknown flag".
	f := cmd.Flags()
	_ = f.MarkHidden(FlagRunResume)
	_ = f.MarkHidden(FlagRunMessage)
	return cmd
}

// runResume is the RunE of `sc resume`: set --resume and hand over to the
// run form.
func runResume(cmd *cobra.Command, args []string) error {
	if err := cmd.Flags().Set(FlagRunResume, "true"); err != nil {
		return err
	}
	return runTarget(cmd, args)
}
