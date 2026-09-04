package cli

import (
	"context"
	"log/slog"
	"sync"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
)

// The per-agent log files (P9-18). D-23 gives a run one log per agent at
// <run-dir>/agents/<agent-id>.log, [logging.RunLogger.Agent] opens them and
// `sc logs <agent-id>` (P8-05) reads them back — but nothing was writing
// them, so the directory the first-run notice points at stayed empty and
// `sc logs` for a real agent answered "no log". This is the writer.
//
// It sits on the publish path rather than on a bus subscription on purpose:
// [events.Bus] drops events a slow subscriber cannot take, and agent output
// is by far the most frequent event, so a subscriber is exactly the place a
// transcript would silently lose lines. The run already writes events.jsonl
// synchronously in front of the bus (P8-20); one more append per agent line
// keeps the log complete and keeps "durable before rendered" true.
//
// What it writes is what the renderers see: the events reach it already
// scrubbed and redacted (P7-17, P9-05), so an agent log holds no credential
// the event stream does not.

// agentLogSink writes the agent-scoped part of a run's event stream to the
// per-agent log files. The zero value is not usable; build one with
// [newAgentLogSink], which returns nil when there is no run logger, and
// call [agentLogSink.Observe] for every event the run publishes.
type agentLogSink struct {
	logs *logging.RunLogger

	mu sync.Mutex
	// broken remembers the agents whose file could not be opened, so a
	// failing disk warns once rather than once per output line.
	broken map[domain.AgentID]bool
	// warn reports the first failure per agent.
	warn *slog.Logger
}

// newAgentLogSink returns a sink writing through logs, or nil when logs is
// nil (the tests that assemble a run without a log directory). A nil sink
// is usable: Observe does nothing.
func newAgentLogSink(logs *logging.RunLogger, warn *slog.Logger) *agentLogSink {
	if logs == nil {
		return nil
	}
	if warn == nil {
		warn = slog.Default()
	}
	return &agentLogSink{logs: logs, broken: make(map[domain.AgentID]bool), warn: warn}
}

// Observe writes ev to the log of the agent it belongs to. Events that name
// no agent are ignored, so this is cheap on the run's own events. It is safe
// for concurrent use and never returns an error: a run must not fail because
// a transcript could not be written.
func (s *agentLogSink) Observe(ev events.Event) {
	if s == nil || ev == nil {
		return
	}
	switch e := ev.(type) {
	case events.AgentSpawned:
		s.log(e.Agent.ID, slog.LevelInfo, "spawned",
			slog.String("role", e.Agent.Role.String()),
			slog.String("task", string(e.Task)),
			slog.String("bead", e.Agent.Bead.String()),
			slog.Int("round", e.Round),
			slog.Int("attempt", e.Attempt),
			slog.Int("pid", e.Agent.PID),
		)
	case events.AgentOutput:
		// The line itself is the message: `sc logs` prints these back to a
		// human, and a transcript reads better than a "line" attribute.
		s.log(e.Agent, slog.LevelInfo, e.Line, slog.String("stream", string(e.Stream)))
	case events.AgentFinished:
		s.log(e.Agent, slog.LevelInfo, "finished",
			slog.String("role", e.Role.String()),
			slog.String("bead", e.Bead.String()),
			slog.Duration("duration", e.Duration),
			slog.Int("exit_code", e.ExitCode),
			slog.String("produced", string(e.Produced)),
		)
	case events.AgentFailed:
		s.log(e.Agent, slog.LevelError, "failed",
			slog.String("role", e.Role.String()),
			slog.String("bead", e.Bead.String()),
			slog.String("failure", string(e.Failure)),
			slog.String("error", e.Err),
			slog.Duration("duration", e.Duration),
			slog.Int("exit_code", e.ExitCode),
			slog.Int("attempt", e.Attempt),
			slog.Bool("will_retry", e.WillRetry),
		)
	}
}

// log appends one record to an agent's file, opening it on first use.
func (s *agentLogSink) log(id domain.AgentID, level slog.Level, msg string, attrs ...slog.Attr) {
	if id == "" {
		return
	}
	s.mu.Lock()
	broken := s.broken[id]
	s.mu.Unlock()
	if broken {
		return
	}
	l, err := s.logs.Agent(id)
	if err != nil {
		s.mu.Lock()
		first := !s.broken[id]
		s.broken[id] = true
		s.mu.Unlock()
		if first {
			s.warn.Warn("could not open the agent log",
				slog.String("agent", id.String()), slog.Any("err", err))
		}
		return
	}
	l.LogAttrs(context.Background(), level, msg, attrs...)
}
