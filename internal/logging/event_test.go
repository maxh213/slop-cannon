package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/events"
)

func TestLogEvent(t *testing.T) {
	var buf bytes.Buffer
	l := NewFileLogger(&buf, Options{})
	ctx := context.Background()
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	meta := events.MetaAt("run-1", at)

	LogEvent(ctx, l, events.RunStarted{Meta: meta})
	LogEvent(ctx, l, events.Warning{Meta: meta, Source: "gates", Message: "gh missing", Bead: "sc-1"})
	LogEvent(ctx, l, events.Error{Meta: meta, Source: "cartridge", Message: "crashed", Err: "auth: Bearer abcdefghijklmnop", Agent: "engineer-1"})
	LogEvent(ctx, nil, events.RunStarted{Meta: meta})
	LogEvent(ctx, l, nil)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	var recs []map[string]any
	for _, ln := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	if recs[0]["msg"] != "run_started" || recs[0]["level"] != "INFO" || recs[0]["kind"] != "run_started" || recs[0]["run_id"] != "run-1" || recs[0]["at"] != "2026-09-02T12:00:00Z" {
		t.Errorf("info record: %v", recs[0])
	}
	if recs[1]["level"] != "WARN" || recs[1]["msg"] != "gh missing" || recs[1]["source"] != "gates" || recs[1]["bead"] != "sc-1" || recs[1]["kind"] != "warning" {
		t.Errorf("warning record: %v", recs[1])
	}
	if _, has := recs[1]["error"]; has {
		t.Errorf("empty fields should be omitted: %v", recs[1])
	}
	if recs[2]["level"] != "ERROR" || recs[2]["agent"] != "engineer-1" || recs[2]["error"] != "auth: Bearer [REDACTED]" {
		t.Errorf("error record: %v", recs[2])
	}
}
