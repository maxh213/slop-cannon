package logging

import (
	"context"
	"log/slog"

	"slop-cannon/internal/events"
)

// LogEvent writes one bus event to l as a single record carrying "kind",
// "run_id" and "at" attributes from the event. [events.Warning] logs at Warn
// and [events.Error] at Error, using the event's own message and adding its
// source, error, bead and agent fields when set; every other kind logs at
// Info with the kind as the message. It is the bridge from the event stream
// to sc.log and the headless renderer (D-15); l's level filter applies.
func LogEvent(ctx context.Context, l *slog.Logger, ev events.Event) {
	if l == nil || ev == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("kind", ev.Kind().String()),
		slog.String("run_id", ev.RunID().String()),
		slog.Time("at", ev.At()),
	}
	switch e := ev.(type) {
	case events.Warning:
		attrs = appendNotice(attrs, e.Source, e.Err, e.Bead.String(), e.Agent.String())
		l.LogAttrs(ctx, slog.LevelWarn, e.Message, attrs...)
	case events.Error:
		attrs = appendNotice(attrs, e.Source, e.Err, e.Bead.String(), e.Agent.String())
		l.LogAttrs(ctx, slog.LevelError, e.Message, attrs...)
	default:
		l.LogAttrs(ctx, slog.LevelInfo, ev.Kind().String(), attrs...)
	}
}

func appendNotice(attrs []slog.Attr, source, errText, bead, agent string) []slog.Attr {
	for _, kv := range [...]struct{ k, v string }{
		{"source", source}, {"error", errText}, {"bead", bead}, {"agent", agent},
	} {
		if kv.v != "" {
			attrs = append(attrs, slog.String(kv.k, kv.v))
		}
	}
	return attrs
}
