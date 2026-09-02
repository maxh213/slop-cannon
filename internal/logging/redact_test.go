package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestRedactStrings(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"openai key", "key=sk-abcdefghijklmnop123456", "key=sk-[REDACTED]"},
		{"anthropic key", "sk-ant-api03-AbCdEfGhIjKlMnOp_-12345 in env", "sk-[REDACTED] in env"},
		{"ghp", "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "token ghp_[REDACTED]"},
		{"gho", "gho_abcdefghijklmnop", "gho_[REDACTED]"},
		{"ghs", "x=ghs_abcdefghijklmnop;", "x=ghs_[REDACTED];"},
		{"fine grained", "github_pat_11ABCDEFG0123456789_abcdefghijklmnop", "github_pat_[REDACTED]"},
		{"bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", "Authorization: Bearer [REDACTED]"},
		{"bearer lowercase", "authorization: bearer abcdefghijklmnop", "authorization: bearer [REDACTED]"},
		{"bearer quoted", `{"auth":"Bearer abcdefghijklmnop"}`, `{"auth":"Bearer [REDACTED]"}`},
		{"multiple", "sk-abcdefghijklmnop and ghp_abcdefghijklmnop", "sk-[REDACTED] and ghp_[REDACTED]"},
		{"short prefix survives", "use an sk- key or ghp_ token", "use an sk- key or ghp_ token"},
		{"short token survives", "sk-short ghp_short Bearer short", "sk-short ghp_short Bearer short"},
		{"inside word not matched", "task-123 risk-assessment", "task-123 risk-assessment"},
		{"empty", "", ""},
		{"plain", "nothing to see here", "nothing to see here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestContainsSecret(t *testing.T) {
	if !ContainsSecret("Bearer abcdefghijklmnop") {
		t.Error("expected bearer token to be detected")
	}
	if ContainsSecret("nothing here") {
		t.Error("false positive")
	}
}

func TestNewRedactorExtraPattern(t *testing.T) {
	r := NewRedactor(Pattern{Name: "slack", Regexp: regexp.MustCompile(`\bxoxb-([A-Za-z0-9\-]{8,})`)})
	got := r.Redact("sk-abcdefghijklmnop xoxb-1234-abcdefghij")
	want := "sk-[REDACTED] xoxb-[REDACTED]"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	var nilR *Redactor
	if got := nilR.Redact("sk-abcdefghijklmnop"); got != "sk-abcdefghijklmnop" {
		t.Errorf("nil redactor should be a no-op, got %q", got)
	}
}

type stringerSecret struct{}

func (stringerSecret) String() string { return "bearer abcdefghijklmnop" }

func TestHandlersRedactEverything(t *testing.T) {
	secretMsg := "starting with token ghp_ABCDEFGHIJKLMNOPQRST"
	secretErr := errors.New("gh: bad credentials for Bearer abcdefghijklmnopqrstuvwxyz")

	build := func(name string, mk func(*bytes.Buffer) slog.Handler) func(t *testing.T) {
		return func(t *testing.T) {
			var buf bytes.Buffer
			l := slog.New(mk(&buf))
			l.With("ctx", "sk-ABCDEFGHIJKLMNOPctx").
				WithGroup("http").
				Info(secretMsg,
					"key", "sk-abcdefghijklmnop123",
					"err", secretErr,
					"stringer", stringerSecret{},
					"nested", slog.GroupValue(slog.String("auth", "Bearer abcdefghijklmnopq")),
					"anyval", []string{"x", "ghp_abcdefghijklmnop"},
					"count", 3,
				)
			out := buf.String()
			for _, leak := range []string{
				"ghp_ABCDEFGHIJKLMNOPQRST", "sk-abcdefghijklmnop123", "abcdefghijklmnopqrstuvwxyz",
				"bearer abcdefghijklmnop", "sk-ABCDEFGHIJKLMNOPctx", "Bearer abcdefghijklmnopq",
				"ghp_abcdefghijklmnop",
			} {
				if strings.Contains(out, leak) {
					t.Errorf("%s: output leaks %q:\n%s", name, leak, out)
				}
			}
			for _, keep := range []string{"ghp_[REDACTED]", "sk-[REDACTED]", "Bearer [REDACTED]", "count=3"} {
				if name == "json" && keep == "count=3" {
					keep = `"count":3`
				}
				if !strings.Contains(out, keep) {
					t.Errorf("%s: output lacks %q:\n%s", name, keep, out)
				}
			}
		}
	}
	t.Run("text", build("text", func(b *bytes.Buffer) slog.Handler { return NewTerminalHandler(b, Options{}) }))
	t.Run("json", build("json", func(b *bytes.Buffer) slog.Handler { return NewFileHandler(b, Options{}) }))
}

func TestFileHandlerKeepsStructureWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	l := NewFileLogger(&buf, Options{})
	l.Info("hello", "list", []string{"a", "b"}, "n", 1, "err", errors.New("boom"))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if _, ok := rec["list"].([]any); !ok {
		t.Errorf("list flattened: %#v", rec["list"])
	}
	if rec["err"] != "boom" {
		t.Errorf("err = %#v", rec["err"])
	}
	if rec["msg"] != "hello" || rec["level"] != "INFO" {
		t.Errorf("unexpected record: %v", rec)
	}
	if ts, _ := rec["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("file timestamps should be UTC, got %q", ts)
	}
}
