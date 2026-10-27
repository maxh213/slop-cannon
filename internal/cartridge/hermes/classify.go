package hermes

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

// Credits precede auth and rate limits because billing failures often mention
// limits too. Execution budgets intentionally fall through to KindCrash.
var textRules = []textRule{
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"credit balance", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"not logged in", cartridge.KindAuth},
	{"authentication required", cartridge.KindAuth},
	{"authentication failed", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"invalid token", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"no provider configured", cartridge.KindAuth},
	{"no api keys or providers found", cartridge.KindAuth},
	{"rate limit", cartridge.KindRateLimited},
	{"rate_limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForText classifies a Hermes diagnostic, or returns KindUnknown when no
// ordered error rule matches.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range textRules {
		if strings.Contains(text, rule.needle) {
			return rule.kind
		}
	}
	return cartridge.KindUnknown
}

// Classify gives process timeout/kill precedence, then uses the terminal
// result's error and exit_code. Missing terminal results are crashes, even on
// process exit zero, because the structured contract was not fulfilled.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	if result := p.result; result != nil {
		if result.Error == "" && result.ExitCode == 0 && procErr == nil {
			return nil
		}
		msg := strings.TrimSpace(result.Error)
		if msg == "" {
			msg = fmt.Sprintf("hermes exited with status %d", exitCode)
			if result.ExitCode != 0 {
				msg = fmt.Sprintf("hermes reported exit status %d", result.ExitCode)
			}
		}
		kind := KindForText(msg + "\n" + diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}

	if procErr != nil {
		kind := KindForText(diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("hermes exited with status %d before printing a result", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	msg := fmt.Sprintf("hermes exited 0 without a result line (%d stdout lines)", p.lines)
	if tail := lastNonEmpty(p.noise); tail != "" {
		msg += ": " + truncate(tail, 300)
	}
	return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
}

// ClassifyStart converts a process-start failure into a typed cartridge error.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "hermes is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start hermes", err)
}

// ClassifyProbe converts a detection-probe failure into a typed cartridge error.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "hermes is not on PATH", err)
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
		if value := strings.TrimSpace(lines[i]); value != "" {
			return value
		}
	}
	return ""
}
