package cli

import (
	"context"
	"log/slog"
)

// One "run" key per log record (P8-05).
//
// Three layers each stamp the run id on the run's logger:
// [logging.OpenRunLog] bakes it into sc.log's handler, `sc run` adds it to
// the terminal half of the headless logger, and orchestrator.New adds it
// to whatever logger it is given. Each is right on its own — none can
// assume the others ran — but together they wrote the key two or three
// times into every line, and a JSON object with a repeated key is
// ambiguous: `jq` and encoding/json keep the last, other readers the
// first. `sc logs` and `sc logs --json` print those lines verbatim, so the
// fix belongs where the logger is assembled: the handler below keeps the
// value the run's own handlers carry and drops every later copy.

// logAttrOnce wraps h so that key is written at most once per record: an
// attribute with that key is dropped, whether it arrives through
// [slog.Logger.With] or on the record itself. h is expected to carry the
// value already (the run logger bakes "run" in), which is what makes
// dropping the later copies lossless.
//
// Attributes inside a group are not top-level keys and cannot collide with
// one, so WithGroup hands back the wrapped handler and stops filtering.
func logAttrOnce(h slog.Handler, key string) slog.Handler {
	if h == nil || key == "" {
		return h
	}
	return &attrOnceHandler{h: h, key: key}
}

// attrOnceHandler is [logAttrOnce]'s handler.
type attrOnceHandler struct {
	h   slog.Handler
	key string
}

// Enabled implements [slog.Handler].
func (a *attrOnceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return a.h.Enabled(ctx, l)
}

// Handle implements [slog.Handler], dropping the record's own copies of the
// key. A record without one is passed through untouched, which is the
// common case and costs one scan of its attributes.
func (a *attrOnceHandler) Handle(ctx context.Context, r slog.Record) error {
	found := false
	r.Attrs(func(at slog.Attr) bool {
		found = at.Key == a.key
		return !found
	})
	if !found {
		return a.h.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(at slog.Attr) bool {
		if at.Key != a.key {
			out.AddAttrs(at)
		}
		return true
	})
	return a.h.Handle(ctx, out)
}

// WithAttrs implements [slog.Handler].
func (a *attrOnceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, at := range attrs {
		if at.Key != a.key {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		return a
	}
	return &attrOnceHandler{h: a.h.WithAttrs(kept), key: a.key}
}

// WithGroup implements [slog.Handler].
func (a *attrOnceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return a
	}
	return a.h.WithGroup(name)
}
