package kimi

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name of the Kimi Code adapter.
const Name = "kimi"

// VerifiedVersion is the installed Kimi Code release whose help and bundled
// print/profile implementation this adapter was audited against.
const VerifiedVersion = "2.3.1"

// MinVersion rejects the legacy Python CLI, whose flags and YAML profiles are
// incompatible with Kimi Code 2.x Markdown profiles.
var MinVersion = proc.V(2, 3, 1)

const (
	probeTimeout = 15 * time.Second
	gracePeriod  = 5 * time.Second
)

// Adapter drives Kimi CLI in print mode. Construct it with New.
type Adapter struct {
	name string
	cfg  config.Cartridge
}

var _ cartridge.Cartridge = (*Adapter)(nil)

func init() { cartridge.MustRegister(Name, New) }

// New is the cartridge.Factory for Kimi CLI. It performs no I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	return &Adapter{name: name, cfg: cfg}, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Binary returns the configured executable, defaulting to "kimi".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// Detect resolves the binary and reads its version. Kimi has no cheap local
// authentication-status command, so authentication deliberately stays unknown.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	path, err := proc.LookPath(a.Binary())
	if err != nil {
		st.Detail = fmt.Sprintf("%s not found on PATH", a.Binary())
		return st, ClassifyProbe(a.name, "lookup", err)
	}
	st.Installed, st.Path = true, path
	tool, err := proc.Probe(ctx, proc.Spec{
		Command:    a.Binary(),
		Args:       []string{versionFlag},
		Env:        a.childEnv(nil),
		InheritEnv: InheritEnv(cartridge.DefaultInheritEnv()),
		Timeout:    probeTimeout,
	})
	if err != nil {
		st.Detail = "kimi --version failed"
		return st, ClassifyProbe(a.name, "kimi --version", err)
	}
	st.Version = strings.TrimSpace(tool.Output)
	if tool.HasVersion {
		st.Version = tool.Version.String()
		if !tool.Version.AtLeast(MinVersion) {
			st.Detail = fmt.Sprintf("kimi %s is older than the minimum %s (verified with %s)", st.Version, MinVersion, VerifiedVersion)
			return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, nil)
		}
	}
	st.Auth = cartridge.AuthUnknown
	st.Detail = "version detected; no cheap authentication status probe"
	return st, nil
}

// Run creates a private profile and prompt file, starts print mode, and
// streams parsed records. Both files remain private to this firing and are
// removed after a start failure or process termination.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := profileTools(spec); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(spec.Workdir, ".sc-kimi-run-*")
	if err != nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not create private run directory in workdir", err)
	}
	profilePath, prompt, cleanup, err := prepareRun(dir, spec)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	input := Input(spec)
	argvInput := input
	if len(input) > maxPromptArgBytes {
		argvInput = readPromptInstruction(prompt.Name())
	}

	ps := spec.ProcSpec(a.Binary(), a.Args(spec, profilePath, argvInput)...)
	ps.Env = a.childEnv(spec.Env)
	if spec.MaxTurns > 0 {
		ps.Env[envMaxSteps] = strconv.Itoa(spec.MaxTurns)
	}
	ps.InheritEnv = InheritEnv(spec.EnvAllowlist())
	ps.Stdin = prompt
	ps.GracePeriod = gracePeriod
	h, err := proc.Stream(ctx, ps)
	if err != nil {
		cleanup()
		os.RemoveAll(dir)
		return nil, ClassifyStart(a.name, err)
	}
	pipe := cartridge.NewPipe(a.name, h.Kill)
	go a.pump(h, pipe, prompt, dir)
	return pipe, nil
}

func readPromptInstruction(path string) string {
	return "Use the Read tool to read this exact file: " + strconv.Quote(path) + ". " +
		"Follow its complete contents as the task instructions, including the final structured-output contract."
}

func prepareRun(dir string, spec cartridge.RunSpec) (string, *os.File, func(), error) {
	profilePath, err := writeProfile(dir, spec)
	if err != nil {
		return "", nil, nil, err
	}
	prompt, err := os.CreateTemp(dir, "prompt-*.txt")
	if err != nil {
		return "", nil, nil, fmt.Errorf("kimi: create prompt file: %w", err)
	}
	cleanup := func() {
		prompt.Close()
		os.RemoveAll(dir)
	}
	if _, err := prompt.WriteString(Input(spec)); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("kimi: write prompt file: %w", err)
	}
	if _, err := prompt.Seek(0, 0); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("kimi: rewind prompt file: %w", err)
	}
	return profilePath, prompt, cleanup, nil
}

func (a *Adapter) pump(h *proc.Handle, pipe *cartridge.Pipe, prompt *os.File, dir string) {
	defer os.RemoveAll(dir)
	defer prompt.Close()
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
	outcome := Classify(a.name, p, err, res.ExitCode)
	prompt.Close()
	os.RemoveAll(dir)
	pipe.Finish(result, outcome)
}
