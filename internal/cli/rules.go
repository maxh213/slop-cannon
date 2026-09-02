package cli

import (
	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// promptRules is the rules list (README: "rules which get communicated to
// all agents"). The command itself is the shared implementation in vibes.go.
var promptRules = promptKind{
	name:  "rules",
	short: "Manage the rule files every agent must follow",
	long: `Rules are files of hard constraints (what agents MUST and MUST NOT do:
branches to leave alone, commands never to run, APIs that are frozen) that
are appended to every agent prompt. Vibes (sc vibes) are taste; rules are
law.`,
	add:    (*config.Doc).AddRules,
	remove: (*config.Doc).RemoveRules,
	paths:  (*config.Doc).RulesPaths,
	stored: func(d *config.Doc) []string { return d.File.Rules },
}

func init() { register(newRulesCommand) }

// newRulesCommand builds `sc rules`.
func newRulesCommand() *cobra.Command { return newPromptCommand(promptRules) }
