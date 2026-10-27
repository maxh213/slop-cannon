package hermes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name of the Hermes adapter.
const Name = "hermes"

// VerifiedVersion is the Hermes Agent release whose chat help and stream-json
// emitter define this adapter's flags and protocol.
const VerifiedVersion = "0.22.5"

// MinVersion is the first release supported by this adapter. Older releases
// are not assumed to have the same query-file or stream contract.
var MinVersion = proc.V(0, 21, 3)

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives the Hermes Agent CLI. Build it with New; the zero value is
// not usable.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() { cartridge.MustRegister(Name, New) }

// New constructs a Hermes adapter without performing I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name returns the adapter's registry name.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable, or "hermes" by default.
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect checks installation and version only. Authentication is deliberately
// left unknown: firing the real auth flow can refresh Nous credentials and is
// not a side-effect-free detection probe. Run classifies auth failures.
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
		Env: a.cfg.Env, InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()), Timeout: probeTimeout,
	})
	if err != nil {
		status.Detail = "hermes --version failed"
		return status, ClassifyProbe(a.name, "hermes --version", err)
	}
	status.Version = tool.Output
	if tool.HasVersion {
		status.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			status.Detail = fmt.Sprintf("hermes %s is older than the minimum %s (verified with %s)", status.Version, MinVersion, VerifiedVersion)
			return status, cartridge.NewError(cartridge.KindNotInstalled, a.name, status.Detail, nil)
		}
	}
	status.Auth = cartridge.AuthUnknown
	status.Detail = "version checked; authentication is verified by the firing"
	return status, nil
}

// Run starts an isolated Hermes process. Temporary state belongs to this
// session until pump has reaped the process; only then is it removed.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := a.ToolPolicyEnforcement(spec.ToolPolicy); err != nil {
		return nil, err
	}
	home, err := newSessionHome(a.cfg.Env, spec.Env, spec.Timeout)
	if err != nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not create isolated Hermes home", err)
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	ps.Env = a.childEnv(home, spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.Stdin = strings.NewReader(Stdin(spec))
	ps.GracePeriod = gracePeriod
	handle, err := proc.Stream(ctx, ps)
	if err != nil {
		home.cleanup()
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, handle.Kill)
	go a.pump(handle, pipe, home)
	return pipe, nil
}

func (a *Adapter) pump(handle *proc.Handle, pipe *cartridge.Pipe, home *sessionHome) {
	parser := NewParser()
	for line := range handle.Lines() {
		switch line.Source {
		case proc.Stdout:
			for _, event := range parser.Line(line.Text) {
				event.At = line.At
				pipe.Emit(event)
			}
		case proc.Stderr:
			parser.Stderr(line.Text)
		}
	}
	processResult, processErr := handle.Wait()
	home.cleanup()
	result := parser.Result(processResult.ExitCode, processResult.Duration)
	pipe.Finish(result, Classify(a.name, parser, processErr, processResult.ExitCode))
}
