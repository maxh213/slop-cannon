package agy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

type textRule struct {
	needle string
	kind   cartridge.Kind
}

var textRules = []textRule{
	{"credit balance", cartridge.KindCreditsExhausted},
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"not signed in", cartridge.KindAuth},
	{"not logged in", cartridge.KindAuth},
	{"authentication required", cartridge.KindAuth},
	{"authentication failed", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"permission_denied", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"resource has been exhausted", cartridge.KindRateLimited},
	{"model_capacity_exhausted", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"usage limit", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForText classifies agy diagnostics, checking credits before auth and rate limits.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range textRules {
		if strings.Contains(text, rule.needle) {
			return rule.kind
		}
	}
	return cartridge.KindUnknown
}

// KindForStatus maps an HTTP status in AGY_ERROR to a cartridge failure kind.
func KindForStatus(status int) cartridge.Kind {
	switch status {
	case 0:
		return cartridge.KindUnknown
	case 401, 403:
		return cartridge.KindAuth
	case 402:
		return cartridge.KindCreditsExhausted
	case 429, 503, 529:
		return cartridge.KindRateLimited
	default:
		return cartridge.KindCrash
	}
}

// Classify gives process timeout/cancellation and agy's own print-timeout
// warning precedence over a partial final response.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}
	diag := p.diagnostics()
	if strings.Contains(strings.ToLower(diag), "print timeout after") {
		return cartridge.NewError(cartridge.KindTimeout, name, "agy print timeout elapsed", procErr)
	}
	if p.agentErr != nil {
		return classifyAgentError(name, p.agentErr, procErr)
	}

	if r := p.envelope; r != nil && r.Status == "ERROR" {
		msg := strings.TrimSpace(r.Error)
		if msg == "" {
			msg = strings.TrimSpace(p.Output())
		}
		kind := KindForText(msg + "\n" + diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		if msg == "" {
			msg = "agy reported ERROR"
		}
		return cartridge.NewError(kind, name, truncate(msg, maxDiagnosticLine), procErr)
	}

	if procErr != nil {
		kind := KindForText(diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("agy exited with status %d", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + tail
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	if p.parseErr != nil {
		msg := p.parseErr.Error()
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + tail
		}
		return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
	}
	if p.envelope == nil {
		return cartridge.NewError(cartridge.KindCrash, name, "agy exited 0 without a final JSON result", nil)
	}
	if p.envelope.Status != "SUCCESS" {
		return cartridge.NewError(cartridge.KindCrash, name, fmt.Sprintf("agy returned unknown result status %q", p.envelope.Status), nil)
	}
	if strings.TrimSpace(p.Output()) == "" {
		msg := "agy returned SUCCESS with an empty response"
		if n := len(p.envelope.DeniedActions); n > 0 {
			msg += fmt.Sprintf(" after %d denied tool action(s)", n)
		}
		return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
	}
	return nil
}

func classifyAgentError(name string, ae *agentError, procErr error) error {
	kind := KindForText(ae.ShortError)
	if kind == cartridge.KindUnknown && ae.ErrorCode == 402 {
		kind = cartridge.KindCreditsExhausted
	}
	if kind == cartridge.KindUnknown {
		switch strings.ToUpper(ae.Status) {
		case "UNAUTHENTICATED", "PERMISSION_DENIED":
			kind = cartridge.KindAuth
		case "RESOURCE_EXHAUSTED", "UNAVAILABLE":
			kind = cartridge.KindRateLimited
		}
	}
	if kind == cartridge.KindUnknown {
		kind = KindForStatus(ae.ErrorCode)
	}
	if kind == cartridge.KindUnknown {
		kind = cartridge.KindCrash
	}
	msg := strings.TrimSpace(ae.ShortError)
	if msg == "" {
		msg = "agy reported " + ae.Status
	}
	return cartridge.NewError(kind, name, truncate(msg, maxDiagnosticLine), procErr)
}

// ClassifyStart converts process-start failures into typed cartridge errors.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "agy is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start agy", err)
}

// ClassifyProbe converts version-probe failures into typed cartridge errors.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "agy is not on PATH", err)
	}
	if kind := cartridge.KindOf(err); kind == cartridge.KindTimeout || kind == cartridge.KindKilled {
		return cartridge.NewError(kind, name, what+" did not finish", err)
	}
	var exitErr *proc.ExitError
	if errors.As(err, &exitErr) {
		if kind := KindForText(exitErr.Result.Stderr + "\n" + exitErr.Result.Stdout); kind != cartridge.KindUnknown {
			return cartridge.NewError(kind, name, what+" failed", err)
		}
	}
	return cartridge.NewError(cartridge.KindCrash, name, what+" failed", err)
}

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
