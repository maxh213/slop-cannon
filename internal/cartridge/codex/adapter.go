package codex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

const (
	// Name is the personal Codex registry name and default executable.
	Name = "codex"
	// AnimusName reuses this adapter with an isolated CODEX_HOME.
	AnimusName = "codex-animus"

	animusHomeEnv = "DANDELION_CODEX_ANIMUS_HOME"
	probeTimeout  = 15 * time.Second
	gracePeriod   = 5 * time.Second
)

// Adapter drives codex exec. Build it through New; the zero value is not
// usable.
type Adapter struct {
	name    string
	cfg     config.Cartridge
	homeDir func() (string, error)
}

var (
	_ cartridge.Cartridge       = (*Adapter)(nil)
	_ agents.ToolPolicyEnforcer = (*Adapter)(nil)
)

func init() {
	cartridge.MustRegister(Name, New)
}

// New builds a Codex adapter. An empty name selects Name; AnimusName selects
// the same CLI and parser with the Animus account home. Other names, including
// Dandelion's delegate name, preserve explicitly supplied account homes.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg, homeDir: os.UserHomeDir}, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured command, else "codex" for both account names.
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect resolves the CLI, reads its version, and runs "codex login status".
// The status probe asks the CLI about its login; the adapter never opens
// CODEX_HOME or any credential file itself.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	bin := a.Binary()
	path, err := proc.LookPath(bin)
	if err != nil {
		st.Detail = fmt.Sprintf("%s not found on PATH", bin)
		return st, ClassifyProbe(a.name, "lookup", err)
	}
	st.Installed, st.Path = true, path

	env, err := a.childEnv(nil)
	if err != nil {
		return st, cartridge.NewError(cartridge.KindCrash, a.name, "could not configure Codex", err)
	}
	probe := func(args ...string) proc.Spec {
		return proc.Spec{
			Command: bin, Args: args, Env: env,
			InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()), Timeout: probeTimeout,
		}
	}
	tool, err := proc.Probe(ctx, probe(versionFlag))
	if err != nil {
		st.Detail = "codex --version failed"
		return st, ClassifyProbe(a.name, "codex --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion {
		st.Version = tool.Version.String()
	}

	res, err := proc.Run(ctx, probe("login", "status"))
	out := res.Stdout + "\n" + res.Stderr
	if st, recognised, authErr := authFromStatus(a.name, st, out); recognised {
		return st, authErr
	}
	if err != nil {
		if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
			st.Detail = "auth probe did not finish"
			return st, ClassifyProbe(a.name, "codex login status", err)
		}
		st.Auth = cartridge.AuthUnknown
		st.Detail = "auth probe unavailable: " + firstLine(err.Error())
		return st, nil
	}
	st.Auth = cartridge.AuthUnknown
	st.Detail = "auth probe said nothing recognisable: " + firstLine(out)
	return st, nil
}

func authFromStatus(name string, st cartridge.Status, out string) (cartridge.Status, bool, error) {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "not logged in"), strings.Contains(low, "not authenticated"):
		st.Auth = cartridge.AuthMissing
		st.Detail = "not logged in (run: codex login)"
		return st, true, cartridge.NewError(cartridge.KindAuth, name, st.Detail, nil)
	case strings.Contains(low, "logged in"), strings.Contains(low, "authenticated"), strings.Contains(low, "api key configured"):
		st.Auth = cartridge.AuthOK
		st.Detail = firstLine(out)
		if st.Detail == "" {
			st.Detail = "logged in"
		}
		return st, true, nil
	default:
		return st, false, nil
	}
}

// Run starts codex exec with the prompt on stdin and streams its JSONL events
// into a cartridge.Pipe. read_only is refused before a process is started.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := a.ToolPolicyEnforcement(spec.ToolPolicy); err != nil {
		return nil, err
	}
	env, err := a.childEnv(spec.Env)
	if err != nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not configure Codex", err)
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	ps.Env = env
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.Stdin = strings.NewReader(spec.Prompt)
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe)
	return pipe, nil
}

func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe) {
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
	pipe.Finish(result, Classify(a.name, p, err, res.ExitCode))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
