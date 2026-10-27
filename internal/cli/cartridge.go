package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// Flag names of `sc cartridge`.
const (
	FlagCartridgeAdd    = "add"
	FlagCartridgeModel  = "model"
	FlagCartridgeRemove = "remove"
	FlagCartridgeList   = "list"
	FlagCartridgeUse    = "use"
)

// CartridgeAddHint is the command users are told to run when no cartridge
// is configured or active. Other commands (run, doctor) reuse it so the hint
// reads the same everywhere.
const CartridgeAddHint = "sc cartridge --add claude --model <model>"

// CartridgeInfo is one configured cartridge as printed by `sc cartridge
// --json`.
type CartridgeInfo struct {
	// Name is the cartridge name (the [cartridges.<name>] key).
	Name string `json:"name"`
	// Model is the model passed to the CLI; empty means the CLI's default.
	Model string `json:"model,omitempty"`
	// Command overrides the executable name when set.
	Command string `json:"command,omitempty"`
	// Args are extra arguments prepended to every invocation.
	Args []string `json:"args,omitempty"`
	// Env is extra environment for the CLI process.
	Env map[string]string `json:"env,omitempty"`
	// Active reports whether this is the cartridge runs use by default.
	Active bool `json:"active"`
}

// CartridgeStatus is the --json shape of every `sc cartridge` invocation:
// the state of the global config after the command ran.
type CartridgeStatus struct {
	// Path is the global config file the cartridges live in.
	Path string `json:"path"`
	// Active is the active cartridge name, or "" when none is active.
	Active string `json:"active"`
	// Cartridges lists every configured cartridge in name order.
	Cartridges []CartridgeInfo `json:"cartridges"`
	// Warning is set when the command left no cartridge active.
	Warning string `json:"warning,omitempty"`
}

const cartridgeLong = `A cartridge is an LLM CLI subscription sc can fire: the CLI to run and the
model to pass it. Cartridges are subscriptions, so they always live in the
global config, never in a repository (--global and --repo make no difference
here).

The first cartridge added becomes the active one; --use switches. Removing
the active cartridge promotes the next configured one, or leaves the cannon
without an active cartridge (a run refuses to start until one is added).
Re-adding a configured cartridge updates its model.

Supported: "claude" (Claude Code), "cursor" (Cursor Agent), "agy", "codex",
"kimi", "grok", "junie", "hermes", "kilo", and "dandelion", the quota router that
fires whichever supported account still has quota. Dandelion's --model is
"high" (the strongest model with quota) or "standard"; the router names the
real model, so roles.<role>.model is ignored for it. codex-animus is a routed
Codex account, not a direct cartridge. Any other name is rejected. Without
flags the configured cartridges are listed.`

const cartridgeExamples = `sc cartridge --add claude --model fable
sc cartridge --add dandelion --model high
sc cartridge --remove claude
sc cartridge --add codex --model gpt-5.1-codex-mini
sc cartridge --add kimi --model kimi-k2.5
sc cartridge --add kilo --model deepseek/deepseek-v4.1-flash-1017
sc cartridge --use claude
sc cartridge --list`

func init() { register(newCartridgeCommand) }

// newCartridgeCommand builds `sc cartridge`.
func newCartridgeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cartridge",
		Short:   "Add, remove, list, or switch the LLM CLI subscriptions",
		Long:    cartridgeLong,
		Example: indent(cartridgeExamples, "  "),
		Args:    cobra.NoArgs,
		RunE:    runCartridge,
	}
	f := cmd.Flags()
	f.String(FlagCartridgeAdd, "", "add cartridge `NAME` (or update its model); the first one added becomes active")
	f.String(FlagCartridgeModel, "", "model `M` to pass to the cartridge's CLI (with --add)")
	f.String(FlagCartridgeRemove, "", "remove cartridge `NAME`")
	f.Bool(FlagCartridgeList, false, "list the configured cartridges (the default)")
	f.String(FlagCartridgeUse, "", "make cartridge `NAME` the active one")
	cmd.MarkFlagsMutuallyExclusive(FlagCartridgeAdd, FlagCartridgeRemove, FlagCartridgeList, FlagCartridgeUse)
	return cmd
}

// runCartridge dispatches on the flag given and prints the resulting state.
func runCartridge(cmd *cobra.Command, _ []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	add, _ := f.GetString(FlagCartridgeAdd)
	model, _ := f.GetString(FlagCartridgeModel)
	remove, _ := f.GetString(FlagCartridgeRemove)
	use, _ := f.GetString(FlagCartridgeUse)
	if f.Changed(FlagCartridgeModel) && !f.Changed(FlagCartridgeAdd) {
		return fmt.Errorf("--%s requires --%s NAME", FlagCartridgeModel, FlagCartridgeAdd)
	}

	// Cartridges are subscriptions: always the global file (D-03).
	doc, err := config.OpenGlobal()
	if err != nil {
		return err
	}

	stdout := cmd.OutOrStdout()
	var message, warning string
	switch {
	case f.Changed(FlagCartridgeAdd):
		message, err = cartridgeAdd(doc, add, model, f.Changed(FlagCartridgeModel))
	case f.Changed(FlagCartridgeRemove):
		message, warning, err = cartridgeRemove(doc, remove)
	case f.Changed(FlagCartridgeUse):
		message, err = cartridgeUse(doc, use)
	default:
		// --list, or no flag at all.
		if !opts.JSON {
			return cartridgeWriteList(stdout, doc)
		}
	}
	if err != nil {
		return err
	}
	if message != "" {
		if err := doc.Save(); err != nil {
			return err
		}
	}
	if opts.JSON {
		st := cartridgeStatusOf(doc)
		st.Warning = warning
		return writeJSON(stdout, st)
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return err
	}
	if warning != "" {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warning); err != nil {
			return err
		}
	}
	return nil
}

// cartridgeAdd adds or updates cartridge name and returns the line to print.
// When --model was not given on a re-add the configured model is kept.
func cartridgeAdd(doc *config.Doc, name, model string, modelSet bool) (string, error) {
	name = strings.TrimSpace(name)
	existing, present := doc.File.Cartridges[name]
	if present && !modelSet {
		model = existing.Model
	}
	activated, err := doc.AddCartridge(name, model)
	if err != nil {
		return "", err
	}
	verb := "added"
	if present {
		verb = "updated"
	}
	msg := fmt.Sprintf("%s cartridge %q (%s)", verb, name, cartridgeModelText(model))
	if activated {
		msg += "; it is now the active cartridge"
	}
	return msg, nil
}

// cartridgeRemove deletes cartridge name and returns the line to print plus
// a warning when the removal left no cartridge active.
func cartridgeRemove(doc *config.Doc, name string) (message, warning string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("cartridge name must not be empty")
	}
	promoted, wasActive, err := doc.RemoveCartridge(name)
	if err != nil {
		return "", "", err
	}
	message = fmt.Sprintf("removed cartridge %q", name)
	switch {
	case promoted != "":
		message += fmt.Sprintf("; %q is now the active cartridge", promoted)
	case wasActive:
		warning = "no cartridge is active; add one with: " + CartridgeAddHint
	}
	return message, warning, nil
}

// cartridgeUse makes cartridge name active and returns the line to print.
func cartridgeUse(doc *config.Doc, name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := doc.SetActive(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("active cartridge is now %q", name), nil
}

// cartridgeModelText describes a model for text output.
func cartridgeModelText(model string) string {
	if model == "" {
		return "default model"
	}
	return "model " + model
}

// cartridgeStatusOf snapshots doc as a CartridgeStatus.
func cartridgeStatusOf(doc *config.Doc) CartridgeStatus {
	st := CartridgeStatus{Path: doc.Path, Cartridges: []CartridgeInfo{}}
	if doc.File.ActiveCartridge != nil {
		st.Active = *doc.File.ActiveCartridge
	}
	for _, name := range doc.File.CartridgeNames() {
		c := doc.File.Cartridges[name]
		st.Cartridges = append(st.Cartridges, CartridgeInfo{
			Name:    name,
			Model:   c.Model,
			Command: c.Command,
			Args:    c.Args,
			Env:     c.Env,
			Active:  name == st.Active,
		})
	}
	return st
}

// cartridgeWriteList prints the configured cartridges as a text table.
func cartridgeWriteList(w io.Writer, doc *config.Doc) error {
	st := cartridgeStatusOf(doc)
	if len(st.Cartridges) == 0 {
		_, err := fmt.Fprintln(w, "no cartridges configured; add one with: "+CartridgeAddHint)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODEL\tACTIVE")
	for _, c := range st.Cartridges {
		active := ""
		if c.Active {
			active = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.Model, active)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if st.Active == "" {
		_, err := fmt.Fprintln(w, "no cartridge is active; pick one with: sc cartridge --use NAME")
		return err
	}
	return nil
}
