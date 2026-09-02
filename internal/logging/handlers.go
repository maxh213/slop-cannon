package logging

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Options controls how handlers built by this package behave.
type Options struct {
	// Verbose lowers the minimum level to Debug (the --verbose flag).
	Verbose bool
	// Level, when set, overrides Verbose entirely. A *slog.LevelVar here
	// lets the level change at run time.
	Level slog.Leveler
	// AddSource records the file and line of each log call.
	AddSource bool
	// Redactor replaces the default rules; nil means [NewRedactor] defaults.
	Redactor *Redactor
}

// Level returns the minimum level implied by the verbose flag: Debug when
// verbose, Info otherwise.
func Level(verbose bool) slog.Level {
	if verbose {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func (o Options) leveler() slog.Leveler {
	if o.Level != nil {
		return o.Level
	}
	return Level(o.Verbose)
}

func (o Options) redactor() *Redactor {
	if o.Redactor != nil {
		return o.Redactor
	}
	return defaultRedactor
}

// terminalTime is the time layout used on terminals: the date is noise
// during a session, milliseconds are useful when reading agent timing.
const terminalTime = "15:04:05.000"

// NewTerminalHandler returns a text handler for a human at a terminal: local
// wall-clock time without the date, key=value attributes, and redaction.
func NewTerminalHandler(w io.Writer, opts Options) slog.Handler {
	red := opts.redactor()
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:     opts.leveler(),
		AddSource: opts.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String(slog.TimeKey, t.Format(terminalTime))
				}
			}
			return red.ReplaceAttr(groups, a)
		},
	})
}

// NewFileHandler returns a JSON handler for log files: one object per line,
// UTC RFC 3339 timestamps, and redaction. Pass a file opened in append mode.
func NewFileHandler(w io.Writer, opts Options) slog.Handler {
	red := opts.redactor()
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     opts.leveler(),
		AddSource: opts.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.Time(slog.TimeKey, t.UTC())
				}
			}
			return red.ReplaceAttr(groups, a)
		},
	})
}

// NewTerminalLogger is shorthand for slog.New(NewTerminalHandler(w, opts)).
func NewTerminalLogger(w io.Writer, opts Options) *slog.Logger {
	return slog.New(NewTerminalHandler(w, opts))
}

// NewFileLogger is shorthand for slog.New(NewFileHandler(w, opts)).
func NewFileLogger(w io.Writer, opts Options) *slog.Logger {
	return slog.New(NewFileHandler(w, opts))
}

// MultiHandler sends every record to each of its handlers. It is enabled
// for a level when any child is, and each child still applies its own level
// filter. Errors from children are collected and joined.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler fans out to hs. Nil entries are skipped.
func NewMultiHandler(hs ...slog.Handler) *MultiHandler {
	out := make([]slog.Handler, 0, len(hs))
	for _, h := range hs {
		if h != nil {
			out = append(out, h)
		}
	}
	return &MultiHandler{handlers: out}
}

// Enabled implements [slog.Handler].
func (m *MultiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle implements [slog.Handler]. Each child that is enabled for the
// record's level gets its own clone of the record.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return joinErrors(errs)
}

// WithAttrs implements [slog.Handler].
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: out}
}

// WithGroup implements [slog.Handler].
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: out}
}
