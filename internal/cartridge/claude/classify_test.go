package claude

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

func TestKindForText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want cartridge.Kind
	}{
		{"", cartridge.KindUnknown},
		{"all good", cartridge.KindUnknown},
		{"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.", cartridge.KindCreditsExhausted},
		{"insufficient_quota", cartridge.KindCreditsExhausted},
		{"You have exceeded your monthly spending limit", cartridge.KindCreditsExhausted},
		{"HTTP 402 Payment Required", cartridge.KindCreditsExhausted},
		{"Not logged in · Please run /login", cartridge.KindAuth},
		{"Invalid API key · Please run /login", cartridge.KindAuth},
		{`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, cartridge.KindAuth},
		{"OAuth token has expired. Please obtain a new token or refresh your existing token.", cartridge.KindAuth},
		{"401 Unauthorized", cartridge.KindAuth},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your organization's rate limit"}}`, cartridge.KindRateLimited},
		{"Claude AI usage limit reached|1788372000", cartridge.KindRateLimited},
		{"You've hit your limit · resets 3pm", cartridge.KindRateLimited},
		{"API Error: 529 overloaded_error", cartridge.KindRateLimited},
		{"Too Many Requests", cartridge.KindRateLimited},
		// Credits win over rate-limit phrasing in the same message.
		{"Usage limit reached and your credit balance is too low", cartridge.KindCreditsExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			assert.Equal(t, tc.want, KindForText(tc.text))
		})
	}
}

func TestKindForStatus(t *testing.T) {
	t.Parallel()
	assert.Equal(t, cartridge.KindUnknown, KindForStatus(0))
	assert.Equal(t, cartridge.KindAuth, KindForStatus(401))
	assert.Equal(t, cartridge.KindAuth, KindForStatus(403))
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForStatus(402))
	assert.Equal(t, cartridge.KindRateLimited, KindForStatus(429))
	assert.Equal(t, cartridge.KindRateLimited, KindForStatus(503))
	assert.Equal(t, cartridge.KindRateLimited, KindForStatus(529))
	assert.Equal(t, cartridge.KindCrash, KindForStatus(404))
	assert.Equal(t, cartridge.KindCrash, KindForStatus(500))
	assert.Equal(t, cartridge.KindCrash, KindForStatus(400))
}

func TestTextRules_AreValidKinds(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range TextRules {
		assert.True(t, r.Kind.Valid(), r.Needle)
		assert.NotEqual(t, cartridge.KindUnknown, r.Kind, r.Needle)
		assert.Equal(t, r.Needle, string([]byte(r.Needle)), "needle must be lower case: %q", r.Needle)
		assert.False(t, seen[r.Needle], "duplicate needle %q", r.Needle)
		seen[r.Needle] = true
	}
}

func TestClassify_ProcessCausesWinOverOutput(t *testing.T) {
	t.Parallel()
	p := NewParser()
	p.Line(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s"}`)

	timeout := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: -1, TimedOut: true}, Cause: proc.ErrTimeout}
	err := Classify("claude", p, timeout, -1)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
	assert.True(t, errors.Is(err, cartridge.ErrTimeout))
	assert.True(t, errors.Is(err, proc.ErrTimeout), "the proc cause stays reachable")

	killed := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: -1, Cancelled: true}, Cause: proc.ErrKilled}
	assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(Classify("claude", p, killed, -1)))

	cancelled := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: -1, Cancelled: true}, Cause: context.Canceled}
	assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(Classify("claude", p, cancelled, -1)))
}

func TestClassify_StderrWithoutResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stderr string
		want   cartridge.Kind
	}{
		{"auth", "Not logged in · Please run /login", cartridge.KindAuth},
		{"credits", "Credit balance is too low", cartridge.KindCreditsExhausted},
		{"rate", "API Error: 429 rate_limit_error", cartridge.KindRateLimited},
		{"unknown", "error: unknown option '--bogus-flag'", cartridge.KindCrash},
		{"empty", "", cartridge.KindCrash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			p.Stderr(tc.stderr)
			exit := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: 1}}
			err := Classify("claude", p, exit, 1)
			var ce *cartridge.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.want, ce.Kind)
			assert.Contains(t, ce.Msg, "exited with status 1 before printing a result")
			if tc.stderr != "" {
				assert.Contains(t, ce.Msg, tc.stderr)
			}
			assert.True(t, errors.Is(err, exit), "the exit error is the cause")
		})
	}
}

func TestClassify_ResultTextMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want cartridge.Kind
		msg  string
	}{
		{"status 401", `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Invalid authentication credentials"}`, cartridge.KindAuth, "api error (HTTP 401)"},
		{"status 429", `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"rate limited"}`, cartridge.KindRateLimited, "HTTP 429"},
		{"status 402", `{"type":"result","subtype":"success","is_error":true,"api_error_status":402,"result":"pay up"}`, cartridge.KindCreditsExhausted, "pay up"},
		{"status 500 with credits text", `{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"Your credit balance is too low"}`, cartridge.KindCreditsExhausted, "credit balance"},
		{"errors array", `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Not logged in","second"]}`, cartridge.KindAuth, "error during execution: Not logged in; second"},
		{"max budget", `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"errors":["Reached maximum budget"]}`, cartridge.KindCrash, "budget exhausted: Reached maximum budget"},
		{"error subtype without is_error", `{"type":"result","subtype":"error_max_turns","is_error":false}`, cartridge.KindCrash, "claude reported max turns reached"},
		{"unknown error subtype", `{"type":"result","subtype":"error_something_new","is_error":true}`, cartridge.KindCrash, "claude reported something new"},
		{"is_error with terminal reason only", `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error"}`, cartridge.KindCrash, "claude reported api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			p.Line(tc.line)
			err := Classify("claude", p, nil, 0)
			var ce *cartridge.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.want, ce.Kind)
			assert.Contains(t, ce.Msg, tc.msg)
			assert.Zero(t, ce.RetryAfter)
		})
	}
}

func TestClassify_SuccessIgnoresEarlierWarnings(t *testing.T) {
	t.Parallel()
	p := NewParser()
	p.Stderr("some warning about rate limit approaching")
	p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1}}`)
	p.Line(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)
	assert.NoError(t, Classify("claude", p, nil, 0))
}

func TestClassifyStart(t *testing.T) {
	t.Parallel()
	assert.NoError(t, ClassifyStart("claude", nil))
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(ClassifyStart("claude", exec.ErrNotFound)))
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(ClassifyStart("claude", &exec.Error{Name: "claude", Err: exec.ErrNotFound})))
	assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(ClassifyStart("claude", context.Canceled)))
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(ClassifyStart("claude", errors.New("chdir: no such directory"))))
}

func TestClassifyProbe(t *testing.T) {
	t.Parallel()
	assert.NoError(t, ClassifyProbe("claude", "x", nil))
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(ClassifyProbe("claude", "x", exec.ErrNotFound)))
	timeout := &proc.ExitError{Command: "claude", Cause: proc.ErrTimeout}
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(ClassifyProbe("claude", "x", timeout)))
	auth := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: 1, Stderr: "Not logged in"}}
	assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(ClassifyProbe("claude", "x", auth)))
	other := &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: 2, Stderr: "boom"}}
	err := ClassifyProbe("claude", "claude --version", other)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "claude --version failed")
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	assert.Zero(t, retryAfter(nil, now))
	assert.Zero(t, retryAfter(&rateLimitInfo{ResetsAt: 0}, now))
	assert.Zero(t, retryAfter(&rateLimitInfo{ResetsAt: 900}, now))
	assert.Equal(t, 5*time.Minute, retryAfter(&rateLimitInfo{ResetsAt: 1300}, now))
}
