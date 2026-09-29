package cursor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name the adapter registers under.
const Name = "cursor"

// DefaultBinary is the executable the adapter runs when the configuration
// names no command (config.DefaultBinaries). The CLI also installs itself
// as "agent", which is too generic a name to look up on PATH, and "cursor"
// is the editor.
const DefaultBinary = "cursor-agent"

// VerifiedVersion is the Cursor Agent release every flag in this package was
// checked against ("cursor-agent --help", 2026-09-29) and the fixtures in
// testdata/cursor were captured with.
const VerifiedVersion = "2026.09.28-64d2043"

// MinVersion is the oldest Cursor Agent the adapter accepts. The CLI is
// versioned by release date; stream-json with tool_call started/completed
// lines and the --trust flag are what the adapter depends on, and releases
// from before August 2026 have not been checked.
var MinVersion = proc.V(2026, 8, 0)

// Timing defaults.
const (
	// probeTimeout bounds each Detect probe when the context has no
	// deadline.
	probeTimeout = 20 * time.Second
	// gracePeriod is how long a session gets to exit after SIGTERM before
	// SIGKILL, on timeout, cancellation or Kill.
	gracePeriod = 5 * time.Second
)

// Adapter drives the Cursor Agent CLI ("cursor-agent -p"). Build it through
// the registry (cartridge.New("cursor", cfg)) or New; the zero value is not
// usable.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() {
	cartridge.MustRegister(Name, New)
}

// New is the cartridge.Factory for Cursor Agent. name is the registry name;
// cfg supplies the model, an alternative binary (Command), extra arguments
// and extra environment. It does no I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Binary returns the executable Detect and Run invoke: the configured
// Command, else "cursor-agent".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect implements cartridge.Cartridge: it resolves the binary, runs
// "cursor-agent --version" and checks MinVersion, then "cursor-agent status"
// as the auth probe. The status probe prints "✓ Logged in as <email>" or
// "Not logged in" (fixtures status-logged-in.txt, status-logged-out.txt);
// anything else leaves Auth unknown rather than failing.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	bin := a.Binary()
	path, err := proc.LookPath(bin)
	if err != nil {
		st.Detail = fmt.Sprintf("%s not found on PATH", bin)
		return st, ClassifyProbe(a.name, "lookup", err)
	}
	st.Installed, st.Path = true, path

	tool, err := proc.Probe(ctx, a.probeSpec(versionFlag))
	if err != nil {
		st.Detail = "cursor-agent --version failed"
		return st, ClassifyProbe(a.name, "cursor-agent --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion && !tool.Version.AtLeast(MinVersion) {
		st.Detail = fmt.Sprintf("cursor-agent %s is older than the minimum %s (verified with %s)", st.Version, MinVersion, VerifiedVersion)
		return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
	}

	res, err := proc.Run(ctx, a.probeSpec(statusSubcommand))
	if err != nil {
		if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
			st.Detail = "auth probe did not finish"
			return st, ClassifyProbe(a.name, "cursor-agent status", err)
		}
		st.Auth = cartridge.AuthUnknown
		st.Detail = "auth probe unavailable: " + firstLine(err.Error())
		return st, nil
	}
	return authFromStatus(a.name, st, res.Stdout+"\n"+res.Stderr)
}

// authFromStatus reads the output of "cursor-agent status" into st.
func authFromStatus(name string, st cartridge.Status, out string) (cartridge.Status, error) {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "not logged in"):
		st.Auth = cartridge.AuthMissing
		st.Detail = "not logged in (run: cursor-agent login, or set CURSOR_API_KEY)"
		return st, cartridge.NewError(cartridge.KindAuth, name, st.Detail, nil)
	case strings.Contains(low, "logged in"):
		st.Auth = cartridge.AuthOK
		st.Detail = "logged in"
		if i := strings.Index(low, "logged in as "); i >= 0 {
			st.Detail = "logged in as " + firstLine(strings.TrimSpace(out[i+len("logged in as "):]))
		}
		return st, nil
	default:
		st.Auth = cartridge.AuthUnknown
		st.Detail = "auth probe said nothing recognisable: " + firstLine(out)
		return st, nil
	}
}

// probeSpec is the invocation for a Detect probe: the binary with args, a
// clean environment and a timeout. Configured extra args are not applied to
// probes; they are meant for firings.
func (a *Adapter) probeSpec(args ...string) proc.Spec {
	return proc.Spec{
		Command:    a.Binary(),
		Args:       args,
		Env:        a.childEnv(nil),
		InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()),
		Timeout:    probeTimeout,
	}
}

// Run implements cartridge.Cartridge. The system prompt and the prompt are
// written to stdin (see Stdin); the rest of the spec becomes arguments (see
// Args) and a clean environment (see InheritEnv). Events flow as the CLI
// prints them; the outcome is classified when the process exits.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	ps.Env = a.childEnv(spec.Env)
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.Stdin = strings.NewReader(Stdin(spec))
	ps.GracePeriod = gracePeriod

	h, err := proc.Stream(ctx, ps)
	if err != nil {
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe)
	return pipe, nil
}

// pump feeds the parser from the process and finishes the pipe.
func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe) {
	p := NewParser()
	for l := range h.Lines() {
		switch l.Source {
		case proc.Stdout:
			for _, ev := range p.Line(l.Text) {
				ev.At = l.At
				pipe.Emit(ev)
			}
		case proc.Stderr:
			p.Stderr(l.Text)
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
