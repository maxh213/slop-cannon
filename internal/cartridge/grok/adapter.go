package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name of the Grok Build adapter.
const Name = "grok"

// VerifiedVersion is the installed Grok Build release whose help, bundled
// source documentation, and streaming shapes were audited.
const VerifiedVersion = "1.0.30"

// MinVersion is the oldest release with the audited native policy controls.
var MinVersion = proc.V(1, 0, 30)

var errReadOnlyUnsupported = errors.New("grok read_only is unsupported: Grok 1.0.30 cannot guarantee a closed shell allowlist while always-approve is active")

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives one Grok Build CLI process per firing.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() {
	cartridge.MustRegister(Name, New)
}

// New constructs a Grok adapter under a registry name without doing I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name returns the adapter's registry name.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable or "grok".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect resolves the binary and checks its version. Grok 1.0.30 has no
// verified local auth-status probe, so authentication is left unknown and is
// classified from a firing's structured errors or stderr.
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
		st.Detail = "grok --version failed"
		return st, ClassifyProbe(a.name, "grok --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion {
		st.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			st.Detail = fmt.Sprintf("grok %s is older than the minimum %s (verified with %s)", st.Version, MinVersion, VerifiedVersion)
			return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
		}
	}
	st.Auth = cartridge.AuthUnknown
	st.Detail = "auth probe unavailable; run grok login or set XAI_API_KEY"
	return st, nil
}

// Run starts a headless Grok session and streams its events.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.ToolPolicy == cartridge.ToolReadOnly {
		return nil, errReadOnlyUnsupported
	}
	promptFile, err := writePromptFile(spec.Prompt)
	if err != nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not write prompt file", err)
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec, promptFile)...)
	ps.Env = a.childEnv(spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		_ = os.Remove(promptFile)
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe, promptFile)
	return pipe, nil
}

func writePromptFile(prompt string) (string, error) {
	f, err := os.CreateTemp("", "sc-grok-prompt-*.txt")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := io.WriteString(f, prompt); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe, promptFile string) {
	defer func() { _ = os.Remove(promptFile) }()
	p := NewParser()
	for line := range h.Lines() {
		switch line.Source {
		case proc.Stdout:
			for _, ev := range p.Line(line.Text) {
				ev.At = line.At
				pipe.Emit(ev)
			}
		case proc.Stderr:
			p.Stderr(line.Text)
		}
	}
	res, err := h.Wait()
	result := p.Result(res.ExitCode, res.Duration)
	classified := Classify(a.name, p, err, res.ExitCode)
	_ = os.Remove(promptFile)
	pipe.Finish(result, classified)
}
