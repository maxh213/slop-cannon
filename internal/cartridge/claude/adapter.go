package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name the adapter registers under.
const Name = "claude"

// VerifiedVersion is the Claude Code release every flag in this package was
// checked against ("claude --help", 2026-09-02) and the fixtures in
// testdata/claude were captured with.
const VerifiedVersion = "2.1.258"

// MinVersion is the oldest Claude Code the adapter accepts. Releases before
// 2.0 predate the stream-json result shape and the dontAsk permission mode
// the adapter depends on.
var MinVersion = proc.V(2, 0, 0)

// Timing defaults.
const (
	// probeTimeout bounds each Detect probe when the context has no
	// deadline.
	probeTimeout = 15 * time.Second
	// gracePeriod is how long a session gets to exit after SIGTERM before
	// SIGKILL, on timeout, cancellation or Kill.
	gracePeriod = 5 * time.Second
)

// Adapter drives the Claude Code CLI ("claude -p"). Build it through the
// registry (cartridge.New("claude", cfg)) or New; the zero value is not
// usable.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() {
	cartridge.MustRegister(Name, New)
}

// New is the cartridge.Factory for Claude Code. name is the registry name;
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
// Command, else "claude".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// authStatus is the JSON "claude auth status --json" prints.
type authStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	Email            string `json:"email"`
	SubscriptionType string `json:"subscriptionType"`
}

// Detect implements cartridge.Cartridge: it resolves the binary, runs
// "claude --version" and checks MinVersion, then "claude auth status --json"
// as the auth probe (local, no network). An older CLI without that
// subcommand leaves Auth unknown rather than failing.
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
		st.Detail = "claude --version failed"
		return st, ClassifyProbe(a.name, "claude --version", err)
	}
	st.Version = tool.Output
	if tool.HasVersion {
		st.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			st.Detail = fmt.Sprintf("claude %s is older than the minimum %s (verified with %s)", st.Version, MinVersion, VerifiedVersion)
			return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
		}
	}

	res, err := proc.Run(ctx, a.probeSpec(authStatusSubcommand, "status", "--json"))
	if err != nil {
		if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
			st.Detail = "auth probe did not finish"
			return st, ClassifyProbe(a.name, "claude auth status", err)
		}
		// Older CLIs have no "auth status"; a failing probe is not proof of
		// a missing login. Report what was seen and let Run find out.
		st.Auth = cartridge.AuthUnknown
		st.Detail = "auth probe unavailable: " + firstLine(err.Error())
		return st, nil
	}
	var as authStatus
	if jerr := json.Unmarshal([]byte(extractJSONObject(res.Stdout)), &as); jerr != nil {
		st.Auth = cartridge.AuthUnknown
		st.Detail = "auth probe printed no JSON"
		return st, nil
	}
	if !as.LoggedIn {
		st.Auth = cartridge.AuthMissing
		st.Detail = "not logged in (run: claude auth login, or set ANTHROPIC_API_KEY)"
		return st, cartridge.NewError(cartridge.KindAuth, a.name, st.Detail, nil)
	}
	st.Auth = cartridge.AuthOK
	st.Detail = describeAuth(as)
	return st, nil
}

// probeSpec is the invocation for a Detect probe: the binary with args, a
// clean environment (the default allowlist plus the Claude variables and the
// configured env) and a timeout. Configured extra args are not applied to
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

func describeAuth(as authStatus) string {
	var parts []string
	switch {
	case as.Email != "":
		parts = append(parts, "logged in as "+as.Email)
	case as.AuthMethod != "":
		parts = append(parts, "logged in via "+as.AuthMethod)
	default:
		parts = append(parts, "logged in")
	}
	if as.SubscriptionType != "" {
		parts = append(parts, as.SubscriptionType)
	}
	if as.APIProvider != "" && as.APIProvider != "firstParty" {
		parts = append(parts, as.APIProvider)
	}
	return strings.Join(parts, ", ")
}

// Run implements cartridge.Cartridge. The prompt is written to stdin; the
// rest of the spec becomes arguments (see Args) and a clean environment
// (see InheritEnv). Events flow as the CLI prints them; the outcome is
// classified when the process exits (see Classify).
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	ps := spec.ProcSpec(a.Binary(), a.Args(spec)...)
	ps.Env = a.childEnv(spec.Env)
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

// extractJSONObject returns the first top-level JSON object in s, skipping
// any plain-text preamble the CLI prints before it.
func extractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return ""
	}
	depth := 0
	inStr, esc := false, false
	for j := i; j < len(s); j++ {
		c := s[j]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[i : j+1]
			}
		}
	}
	return s[i:]
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
