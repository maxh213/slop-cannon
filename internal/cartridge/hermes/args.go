package hermes

import (
	"strconv"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	chatSubcommand   = "chat"
	flagQueryFile    = "--query-file"
	queryStdin       = "-"
	flagOneshot      = "--oneshot"
	flagFormat       = "--format"
	formatStreamJSON = "stream-json"
	flagIgnoreRules  = "--ignore-rules"
	flagSource       = "--source"
	sourceTool       = "tool"
	flagIn           = "--in"
	flagProvider     = "--provider"
	providerNous     = "nous"
	flagModel        = "--model"
	flagMaxTurns     = "--max-turns"
	flagToolsets     = "--toolsets"
	versionFlag      = "--version"
)

// SystemPromptSeparator separates the two logical messages in Hermes' single
// query input. It is content, not a policy control.
const SystemPromptSeparator = "\n\n---\n\n"

// ToolFlags maps the two enforceable policies to Hermes toolsets. The terminal
// toolset supplies terminal and process_manage, not write_file or patch. The
// file toolset adds read_file, write_file, patch and search_files.
func ToolFlags(p cartridge.ToolPolicy) []string {
	switch p {
	case cartridge.ToolReadExec:
		return []string{flagToolsets, "terminal"}
	case cartridge.ToolReadWrite:
		return []string{flagToolsets, "terminal,file"}
	default:
		return nil
	}
}

// Args builds one firing's argv. MaxTurns has a native Hermes equivalent;
// Timeout is set in the isolated config and enforced by the process deadline.
// Reasoning is deliberately supplied through configured extra
// args (for example --reasoning high), since RunSpec has no effort field.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	args := []string{
		chatSubcommand, flagQueryFile, queryStdin, flagOneshot,
		flagFormat, formatStreamJSON, flagIgnoreRules, flagSource, sourceTool,
		flagIn, spec.Workdir, flagProvider, providerNous,
	}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.MaxTurns > 0 {
		args = append(args, flagMaxTurns, strconv.Itoa(spec.MaxTurns))
	}
	args = append(args, withWebTools(ToolFlags(spec.ToolPolicy), spec.Web)...)
	return append(args, a.cfg.Args...)
}

func withWebTools(flags []string, web bool) []string {
	if !web || len(flags) == 0 {
		return flags
	}
	out := append([]string(nil), flags...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flagToolsets {
			out[i+1] += ",web"
			return out
		}
	}
	return out
}

// Stdin is the single query Hermes reads from --query-file -.
func Stdin(spec cartridge.RunSpec) string {
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return spec.Prompt
	}
	return spec.SystemPrompt + SystemPromptSeparator + spec.Prompt
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}
