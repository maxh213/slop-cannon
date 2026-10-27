package kilo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name of the Kilo adapter.
const Name = "kilo"

// VerifiedVersion is the Kilo CLI release defining this adapter's contract.
const VerifiedVersion = "7.5.6"

// MinVersion is the first Kilo release supported by this adapter.
var MinVersion = proc.V(7, 5, 6)

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives one isolated Kilo CLI process per firing.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() { cartridge.MustRegister(Name, New) }

// New constructs a Kilo adapter without performing I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name returns the adapter's registry name.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable, or "kilo" by default.
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect checks only local installation and version. Authentication and
// balance are deliberately not probed here; an enabled floor checks them fresh
// immediately before each actual firing.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var status cartridge.Status
	path, err := proc.LookPath(a.Binary())
	if err != nil {
		status.Detail = fmt.Sprintf("%s not found on PATH", a.Binary())
		return status, ClassifyProbe(a.name, "lookup", err)
	}
	status.Installed, status.Path = true, path
	tool, err := proc.Probe(ctx, proc.Spec{
		Command: a.Binary(), Args: []string{versionFlag},
		Env: a.childEnv(nil, ""), InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()), Timeout: probeTimeout,
	})
	if err != nil {
		status.Detail = "kilo --version failed"
		return status, ClassifyProbe(a.name, "kilo --version", err)
	}
	status.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion {
		status.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			status.Detail = fmt.Sprintf("kilo %s is older than the minimum %s (verified with %s)", status.Version, MinVersion, VerifiedVersion)
			return status, cartridge.NewError(cartridge.KindNotInstalled, a.name, status.Detail, nil)
		}
	}
	status.Auth = cartridge.AuthUnknown
	status.Detail = "version checked; authentication is verified by the firing"
	return status, nil
}

// Run validates policy and optional fresh balance before starting Kilo. The
// process and all descendants are reaped by internal/proc; no daemon is stopped.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	configContent, err := ConfigContent(spec)
	if err != nil {
		return nil, err
	}
	allowance, err := a.checkBalance(ctx, spec)
	if err != nil {
		return nil, err
	}
	processSpec := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	processSpec.Env = a.childEnv(spec.Env, configContent)
	processSpec.InheritEnv = InheritEnv(spec.EnvAllowlist())
	processSpec.Stdin = strings.NewReader(Stdin(spec))
	processSpec.GracePeriod = gracePeriod
	handle, err := proc.Stream(ctx, processSpec)
	if err != nil {
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, handle.Kill)
	go a.pump(handle, pipe, allowance)
	return pipe, nil
}

func (a *Adapter) pump(handle *proc.Handle, pipe *cartridge.Pipe, allowance float64) {
	parser := NewParser()
	budgetExhausted := false
	for line := range handle.Lines() {
		switch line.Source {
		case proc.Stdout:
			for _, event := range parser.Line(line.Text) {
				event.At = line.At
				pipe.Emit(event)
			}
			if allowance > 0 && !budgetExhausted {
				if cost, ok := parser.Cost(); ok && cost >= allowance {
					budgetExhausted = true
					handle.Kill()
				}
			}
		case proc.Stderr:
			parser.Stderr(line.Text)
		}
	}
	result, processErr := handle.Wait()
	outcome := parser.Result(result.ExitCode, result.Duration)
	pipe.Finish(outcome, Classify(a.name, parser, processErr, result.ExitCode, budgetExhausted))
}
