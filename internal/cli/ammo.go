package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// AmmoResult is the --json shape of `sc ammo`: the D-05 allocation the value
// implies plus the config file it was written to (Path is empty and Source
// is set when the command only reported the effective value).
type AmmoResult struct {
	config.Allocation
	// Scope is the config file that holds the value ("global" or "repo").
	Scope config.Scope `json:"scope"`
	// Path is the config file that was written; empty when nothing was written.
	Path string `json:"path,omitempty"`
	// Source is where the effective value came from when reporting only.
	Source config.Source `json:"source,omitempty"`
}

const ammoLong = `Set the ammo: how many agent subprocesses may be alive at once.

The minimum is 3 (one engineer, one tester, one product owner); the default
is 10. The value is written to the repo config when --repo (default: the
current directory) is inside a git repository, otherwise to the global
config; --global always writes the global config.

The allocation the value implies is printed: one product-owner slot, at most
max(1, floor((ammo-1)/4)) engineers in flight, and every remaining slot for
testers so each engineer can have three reviewers.

Without N the effective value and where it comes from is printed instead.`

func init() { register(newAmmoCommand) }

func newAmmoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ammo [N]",
		Short: "Set how many agents run at once (minimum 3, default 10)",
		Long:  ammoLong,
		Example: indent(`sc ammo 12
sc --global ammo 6
sc ammo`, "  "),
		Args: cobra.MaximumNArgs(1),
		RunE: runAmmo,
	}
}

// runAmmo implements `sc ammo [N]`.
func runAmmo(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return showAmmo(cmd, opts)
	}
	ammo, err := parseAmmo(args[0])
	if err != nil {
		return err
	}
	root, inRepo := config.FindRepoRoot(opts.Repo)
	doc, err := config.Open(config.Target(inRepo, opts.Global), root)
	if err != nil {
		return err
	}
	alloc, err := doc.SetAmmo(ammo)
	if err != nil {
		return err
	}
	if err := doc.Save(); err != nil {
		return err
	}
	res := AmmoResult{Allocation: alloc, Scope: doc.Scope, Path: doc.Path}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	_, err = fmt.Fprintf(out, "%s\nsaved to %s\n", alloc, doc.Path)
	return err
}

// showAmmo prints the effective ammo value and its source without writing.
func showAmmo(cmd *cobra.Command, opts GlobalOptions) error {
	eff, err := config.Load(opts.Repo)
	if err != nil {
		return err
	}
	alloc, err := config.Allocate(eff.Ammo)
	if err != nil {
		return err
	}
	src := eff.Sources[config.KeyAmmo]
	if src == "" {
		src = config.SourceDefault
	}
	res := AmmoResult{Allocation: alloc, Scope: ammoScopeOf(src), Source: src}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	_, err = fmt.Fprintf(out, "%s (from %s)\n", alloc, src)
	return err
}

// ammoScopeOf maps a config source to the scope whose file set the value;
// defaults are reported under the global scope, where a write would land
// when no repo is involved.
func ammoScopeOf(src config.Source) config.Scope {
	if src == config.SourceRepo {
		return config.ScopeRepo
	}
	return config.ScopeGlobal
}

// parseAmmo turns the positional argument into an int. Anything that is not
// a whole number is rejected; the MinAmmo check is left to config.Allocate
// so the D-05 message stays in one place.
func parseAmmo(arg string) (int, error) {
	s := strings.TrimSpace(arg)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("ammo: %q is not a whole number (example: sc ammo 12)", arg)
	}
	return n, nil
}
