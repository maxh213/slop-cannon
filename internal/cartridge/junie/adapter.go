package junie

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

// Name is the registry name of the Junie adapter.
const Name = "junie"

// DefaultBinary is the Junie executable used when no command is configured.
const DefaultBinary = "junie"

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives non-interactive Junie CLI tasks.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() {
	cartridge.MustRegister(Name, New)
}

// New builds a Junie adapter without performing I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	if err := validateEfforts(cfg.Args); err != nil {
		return nil, err
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name returns the adapter's registry name.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable or DefaultBinary.
func (a *Adapter) Binary() string { return a.cfg.Binary(DefaultBinary) }

// Detect resolves the binary and reads its version. Junie has no cheap local
// authentication probe, so Detect deliberately leaves AuthUnknown; firing
// errors are classified from the CLI's diagnostics.
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
		st.Detail = "junie --version failed"
		return st, ClassifyProbe(a.name, "junie --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	st.Auth = cartridge.AuthUnknown
	st.Detail = "authentication not probed: no cheap local auth-status command"
	return st, nil
}

// Run starts one read_write Junie task and returns its streaming session.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := validatePolicy(spec.ToolPolicy); err != nil {
		return nil, err
	}
	task, promptPath, err := taskForSpec(spec.Prompt, spec.Workdir)
	if err != nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not write large prompt file", err)
	}
	extensionDir, err := os.MkdirTemp("", "sc-junie-extensions-")
	if err != nil {
		removePromptFile(promptPath)
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not create isolated extensions directory", err)
	}
	ps := spec.ProcSpec(a.Binary(), a.args(spec, extensionDir, task)...)
	ps.Env = a.childEnv(spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		removePromptFile(promptPath)
		_ = os.RemoveAll(extensionDir)
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe, extensionDir, promptPath)
	return pipe, nil
}

func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe, extensionDir, promptPath string) {
	defer removePromptFile(promptPath)
	defer func() { _ = os.RemoveAll(extensionDir) }()
	p := NewParser()
	for line := range h.Lines() {
		if line.Source == proc.Stdout {
			p.Stdout(line.Text)
		} else {
			p.Stderr(line.Text)
		}
	}
	res, procErr := h.Wait()
	result := p.Result(res.ExitCode, res.Duration)
	err := Classify(a.name, p, procErr, res.ExitCode)
	if err == nil && result.Output != "" {
		pipe.Emit(cartridge.TextDelta(result.Output))
	}
	removePromptFile(promptPath)
	_ = os.RemoveAll(extensionDir)
	pipe.Finish(result, err)
}
