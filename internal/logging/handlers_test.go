package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestLevel(t *testing.T) {
	if Level(false) != slog.LevelInfo {
		t.Errorf("Level(false) = %v", Level(false))
	}
	if Level(true) != slog.LevelDebug {
		t.Errorf("Level(true) = %v", Level(true))
	}
}

func TestVerboseRaisesLevel(t *testing.T) {
	var quiet, loud bytes.Buffer
	NewTerminalLogger(&quiet, Options{}).Debug("dbg")
	NewTerminalLogger(&loud, Options{Verbose: true}).Debug("dbg")
	if quiet.Len() != 0 {
		t.Errorf("debug logged without --verbose: %q", quiet.String())
	}
	if !strings.Contains(loud.String(), "level=DEBUG msg=dbg") {
		t.Errorf("debug not logged with --verbose: %q", loud.String())
	}
}

func TestLevelOverridesVerbose(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	l := NewFileLogger(&buf, Options{Verbose: true, Level: lv})
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("Level override not honoured: %q", buf.String())
	}
	lv.Set(slog.LevelDebug)
	l.Debug("now visible")
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("LevelVar change not honoured: %q", buf.String())
	}
}

func TestTerminalTimeFormat(t *testing.T) {
	var buf bytes.Buffer
	NewTerminalLogger(&buf, Options{}).Info("hi")
	re := regexp.MustCompile(`^time=\d{2}:\d{2}:\d{2}\.\d{3} level=INFO msg=hi\n$`)
	if !re.MatchString(buf.String()) {
		t.Errorf("unexpected terminal line: %q", buf.String())
	}
}

func TestCustomRedactorOption(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(Pattern{Name: "pin", Regexp: regexp.MustCompile(`pin=(\d{4,})`)})
	NewTerminalLogger(&buf, Options{Redactor: r}).Info("login pin=123456 sk-abcdefghijklmnop")
	if !strings.Contains(buf.String(), "pin=[REDACTED] sk-[REDACTED]") {
		t.Errorf("custom redactor not applied: %q", buf.String())
	}
}

type failingHandler struct{ slog.Handler }

func (failingHandler) Handle(context.Context, slog.Record) error { return errors.New("disk full") }

func TestMultiHandler(t *testing.T) {
	var text, file bytes.Buffer
	m := NewMultiHandler(
		NewTerminalHandler(&text, Options{}),
		NewFileHandler(&file, Options{Verbose: true}),
		nil,
	)
	if len(m.handlers) != 2 {
		t.Fatalf("nil handler not skipped: %d", len(m.handlers))
	}
	ctx := context.Background()
	if !m.Enabled(ctx, slog.LevelDebug) {
		t.Error("multi should be enabled when any child is")
	}
	l := slog.New(m).With("k", "v").WithGroup("g")
	l.Debug("only file", "token", "ghp_abcdefghijklmnop")
	l.Info("both")

	if strings.Contains(text.String(), "only file") {
		t.Errorf("text handler should filter debug: %q", text.String())
	}
	if !strings.Contains(text.String(), `msg=both k=v`) {
		t.Errorf("text missing info line: %q", text.String())
	}
	if !strings.Contains(file.String(), `"msg":"only file","k":"v","g":{"token":"ghp_[REDACTED]"}`) {
		t.Errorf("file missing redacted debug line: %q", file.String())
	}
	if !strings.Contains(file.String(), `"msg":"both"`) {
		t.Errorf("file missing info line: %q", file.String())
	}

	if m.WithGroup("") != m {
		t.Error("WithGroup(\"\") should return the receiver")
	}

	fm := NewMultiHandler(failingHandler{NewTerminalHandler(&text, Options{})}, failingHandler{NewTerminalHandler(&text, Options{})})
	var r slog.Record
	r.Level = slog.LevelInfo
	err := fm.Handle(ctx, r)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("child errors not propagated: %v", err)
	}
	if NewMultiHandler().Enabled(ctx, slog.LevelError) {
		t.Error("empty multi should be disabled")
	}
}
