package agy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name the adapter uses.
const Name = "agy"

// VerifiedVersion is the agy release whose local help and protocol were audited.
const VerifiedVersion = "1.4.7"

// MinVersion is the oldest agy release this adapter accepts.
var MinVersion = proc.V(1, 4, 7)

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives the agy CLI. Use New; the zero value is not usable.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

// New constructs an adapter without performing I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable, or "agy".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect checks installation and version only. agy 1.4.7 exposes no cheap
// local authentication-status command, so authentication remains unknown.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	bin := a.Binary()
	path, err := proc.LookPath(bin)
	if err != nil {
		st.Detail = fmt.Sprintf("%s not found on PATH", bin)
		return st, ClassifyProbe(a.name, "lookup", err)
	}
	st.Installed, st.Path = true, path

	tool, err := proc.Probe(ctx, proc.Spec{
		Command:    bin,
		Args:       []string{versionFlag},
		Env:        a.childEnv(nil),
		InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()),
		Timeout:    probeTimeout,
	})
	if err != nil {
		st.Detail = "agy --version failed"
		return st, ClassifyProbe(a.name, "agy --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion {
		st.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			st.Detail = fmt.Sprintf("agy %s is older than the minimum %s (verified with %s)", st.Version, MinVersion, VerifiedVersion)
			return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
		}
	}
	st.Auth = cartridge.AuthUnknown
	st.Detail = "auth not probed: agy has no cheap local auth status command"
	return st, nil
}

// Run starts one print-mode process. The process timeout backs up agy's own
// --print-timeout and proc handles cancellation, killing and descendant reaping.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.ToolPolicy == cartridge.ToolReadExec {
		return nil, ErrReadExecUnsupported
	}
	if spec.ToolPolicy == cartridge.ToolReadOnly {
		for _, arg := range a.cfg.Args {
			if arg == flagDisableSlashCommands {
				return nil, ErrPlanSlashConflict
			}
		}
	}
	prompt := Prompt(spec)
	promptArg := prompt
	cleanup := func() {}
	if len(prompt) > promptFileThreshold {
		path, err := writePromptFile(spec.Workdir, prompt)
		if err != nil {
			return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not write temporary prompt file", err)
		}
		promptArg = promptFileInstruction(path)
		cleanup = func() { _ = os.Remove(path) }
	}
	ps := spec.ProcSpec(a.Binary(), a.argsWithPrompt(spec, promptArg)...)
	ps.Env = a.childEnv(spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		cleanup()
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe, cleanup)
	return pipe, nil
}

func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe, cleanup func()) {
	defer cleanup()
	p := NewParser()
	for line := range h.Lines() {
		switch line.Source {
		case proc.Stdout:
			p.Stdout(line.Text)
		case proc.Stderr:
			p.Stderr(line.Text)
		}
	}
	res, procErr := h.Wait()
	p.Parse()
	result := p.Result(res.ExitCode, res.Duration)
	if output := p.Output(); output != "" {
		pipe.Emit(cartridge.TextDelta(output))
	}
	cleanup()
	pipe.Finish(result, Classify(a.name, p, procErr, res.ExitCode))
}
