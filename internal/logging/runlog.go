package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"slop-cannon/internal/domain"
)

// Permissions for log files and their directories. Logs are redacted but
// still contain repository paths, prompts and agent output, so they are
// private to the user.
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// ErrClosed is returned by [RunLogger.Agent] after [RunLogger.Close].
var ErrClosed = errors.New("logging: run logger is closed")

// RunLogger owns the log files of one run: sc.log for the orchestrator and
// one file per agent, opened on first use. All methods are safe for
// concurrent use. Obtain one with [OpenRunLog].
type RunLogger struct {
	paths RunPaths
	opts  Options
	run   domain.RunID

	mu     sync.Mutex
	closed bool
	file   *os.File
	logger *slog.Logger
	agents map[domain.AgentID]*agentLog
}

type agentLog struct {
	file   *os.File
	logger *slog.Logger
}

// OpenRunLog creates runDir and its agents directory if needed, opens
// <runDir>/sc.log for appending, and returns a RunLogger writing JSON lines
// to it. Every logger it hands out carries a "run" attribute. Reopening an
// existing run directory appends, which is what resume needs.
func OpenRunLog(runDir string, run domain.RunID, opts Options) (*RunLogger, error) {
	p := PathsFor(runDir)
	if err := os.MkdirAll(p.AgentsDir, dirPerm); err != nil {
		return nil, fmt.Errorf("logging: create run dir: %w", err)
	}
	f, err := openAppend(p.Log)
	if err != nil {
		return nil, err
	}
	l := NewFileLogger(f, opts).With(slog.String("run", run.String()))
	return &RunLogger{
		paths:  p,
		opts:   opts,
		run:    run,
		file:   f,
		logger: l,
		agents: make(map[domain.AgentID]*agentLog),
	}, nil
}

func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("logging: open %s: %w", path, err)
	}
	return f, nil
}

// Paths returns the locations this logger writes to.
func (r *RunLogger) Paths() RunPaths { return r.paths }

// Logger returns the orchestrator logger (sc.log). It stays usable after
// Close, but records are then discarded by the closed file and the error is
// swallowed by slog, so close last.
func (r *RunLogger) Logger() *slog.Logger { return r.logger }

// Agent returns the logger for one agent, opening agents/<id>.log on the
// first call for that id. Records carry "run" and "agent" attributes. The
// same *slog.Logger is returned for repeated calls with the same id.
func (r *RunLogger) Agent(id domain.AgentID) (*slog.Logger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if a, ok := r.agents[id]; ok {
		return a.logger, nil
	}
	f, err := openAppend(r.paths.AgentLog(id))
	if err != nil {
		return nil, err
	}
	l := NewFileLogger(f, r.opts).With(
		slog.String("run", r.run.String()),
		slog.String("agent", id.String()),
	)
	r.agents[id] = &agentLog{file: f, logger: l}
	return l, nil
}

// CloseAgent closes one agent's file. Calling it for an agent that was never
// opened, or already closed, is a no-op. The logger previously returned for
// that id must not be used afterwards.
func (r *RunLogger) CloseAgent(id domain.AgentID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return nil
	}
	delete(r.agents, id)
	if err := a.file.Close(); err != nil {
		return fmt.Errorf("logging: close agent log %s: %w", id, err)
	}
	return nil
}

// Close closes every agent file and then sc.log. It is safe to call more
// than once; later calls return nil.
func (r *RunLogger) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for id, a := range r.agents {
		if err := a.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("logging: close agent log %s: %w", id, err))
		}
		delete(r.agents, id)
	}
	if err := r.file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("logging: close %s: %w", r.paths.Log, err))
	}
	return joinErrors(errs)
}

// joinErrors returns nil for no errors, the error itself for one, and an
// errors.Join for several.
func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
}
