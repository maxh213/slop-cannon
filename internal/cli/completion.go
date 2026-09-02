package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// FlagCompletionNoDescriptions is the `sc completion` flag that strips the
// per-command descriptions from the generated script.
const FlagCompletionNoDescriptions = "no-descriptions"

// CompletionShells lists the shells `sc completion` can generate a script
// for, in the order they are shown to the user.
var CompletionShells = []string{"bash", "zsh", "fish"}

const completionLong = `Print a shell completion script for sc to stdout. Source it (or install it
where your shell looks for completions) and tab completion works for every
sc command and flag.

bash (needs bash-completion; bash 4 or newer):
  # once, for the current shell:
  source <(sc completion bash)
  # permanently:
  sc completion bash > /etc/bash_completion.d/sc                # Linux
  sc completion bash > $(brew --prefix)/etc/bash_completion.d/sc  # macOS

zsh:
  # once, for the current shell:
  source <(sc completion zsh)
  # permanently (then start a new shell; add "autoload -U compinit; compinit"
  # to ~/.zshrc if completions are not enabled yet):
  sc completion zsh > "${fpath[1]}/_sc"

fish:
  # once, for the current shell:
  sc completion fish | source
  # permanently:
  sc completion fish > ~/.config/fish/completions/sc.fish`

const completionExamples = `sc completion bash
sc completion zsh > "${fpath[1]}/_sc"
sc completion fish > ~/.config/fish/completions/sc.fish`

func init() { register(newCompletionCommand) }

// newCompletionCommand builds `sc completion bash|zsh|fish`. Registering a
// command named "completion" makes cobra skip its own default completion
// command, so this is the only one in the tree.
func newCompletionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "completion " + strings.Join(CompletionShells, "|"),
		Short:                 "Print a shell completion script (bash, zsh, or fish)",
		Long:                  completionLong,
		Example:               indent(completionExamples, "  "),
		Args:                  completionArgs,
		ValidArgs:             CompletionShells,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			noDesc, err := cmd.Flags().GetBool(FlagCompletionNoDescriptions)
			if err != nil {
				return err
			}
			return WriteCompletion(cmd.OutOrStdout(), cmd.Root(), args[0], !noDesc)
		},
	}
	cmd.Flags().Bool(FlagCompletionNoDescriptions, false, "omit command descriptions from the completion script")
	return cmd
}

// completionArgs accepts exactly one argument naming a supported shell.
func completionArgs(cmd *cobra.Command, args []string) error {
	choices := strings.Join(CompletionShells, ", ")
	switch len(args) {
	case 0:
		return fmt.Errorf("%s needs a shell argument: one of %s", cmd.CommandPath(), choices)
	case 1:
		if !completionSupported(args[0]) {
			return fmt.Errorf("unsupported shell %q: choose one of %s", args[0], choices)
		}
		return nil
	default:
		return fmt.Errorf("%s accepts exactly one shell argument (one of %s), received %d", cmd.CommandPath(), choices, len(args))
	}
}

// completionSupported reports whether shell is one of CompletionShells.
func completionSupported(shell string) bool {
	for _, s := range CompletionShells {
		if s == shell {
			return true
		}
	}
	return false
}

// WriteCompletion writes the completion script for root and the given shell
// ("bash", "zsh", or "fish") to w. withDescriptions keeps the per-command
// descriptions in the script (bash and fish only; zsh always has them).
func WriteCompletion(w io.Writer, root *cobra.Command, shell string, withDescriptions bool) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(w, withDescriptions)
	case "zsh":
		if withDescriptions {
			return root.GenZshCompletion(w)
		}
		return root.GenZshCompletionNoDesc(w)
	case "fish":
		return root.GenFishCompletion(w, withDescriptions)
	default:
		return fmt.Errorf("unsupported shell %q: choose one of %s", shell, strings.Join(CompletionShells, ", "))
	}
}
