package kimi

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

const exitRetryable = 75

type textRule struct {
	needle string
	kind   cartridge.Kind
}

// Quota rules precede generic 429/rate-limit rules: Kimi reports both account
// exhaustion and temporary throttling with HTTP 429, but only the latter is
// retryable.
var textRules = []textRule{
	{"insufficient balance", cartridge.KindCreditsExhausted},
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"quota exhausted", cartridge.KindCreditsExhausted},
	{"exhausted quota", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"exceeded your quota", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"recharge", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"not logged in", cartridge.KindAuth},
	{"please login", cartridge.KindAuth},
	{"authentication", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"rate limit", cartridge.KindRateLimited},
	{"rate_limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
	{"429", cartridge.KindRateLimited},
}

// KindForText classifies diagnostics, checking quota exhaustion before
// generic throttling phrases.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range textRules {
		if strings.Contains(text, rule.needle) {
			return rule.kind
		}
	}
	return cartridge.KindUnknown
}

// KindForStatus distinguishes permanent account failures from temporary API
// throttling. A bare 429 is temporary; quota text can refine it afterwards.
func KindForStatus(status int) cartridge.Kind {
	switch status {
	case 0:
		return cartridge.KindUnknown
	case 401, 403:
		return cartridge.KindAuth
	case 402:
		return cartridge.KindCreditsExhausted
	case 429, 500, 502, 503, 504, 529:
		return cartridge.KindRateLimited
	default:
		return cartridge.KindCrash
	}
}

// Classify converts process output and exit status into a cartridge error.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	kind := KindForText(diag)
	if p.retry != nil {
		retryText := p.retry.errorName + "\n" + p.retry.errorMessage
		if textKind := KindForText(retryText); textKind != cartridge.KindUnknown {
			kind = textKind
		} else if statusKind := KindForStatus(p.retry.statusCode); statusKind != cartridge.KindUnknown {
			kind = statusKind
		}
	}
	if exitCode != 0 && kind == cartridge.KindUnknown {
		if exitCode == exitRetryable {
			kind = cartridge.KindRateLimited
		} else {
			kind = cartridge.KindCrash
		}
	}
	if procErr != nil || exitCode != 0 {
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("kimi exited with status %d", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + tail
		} else if p.retry != nil && p.retry.errorMessage != "" {
			msg += ": " + p.retry.errorMessage
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	if !p.HasAssistant() {
		msg := fmt.Sprintf("kimi exited 0 without an assistant message (%d stdout lines)", p.lines)
		if tail := lastNonEmpty(p.noise); tail != "" {
			msg += ": " + tail
		}
		return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
	}
	return nil
}

// ClassifyStart wraps an error that prevented the CLI from starting.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "kimi is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start kimi", err)
}

// ClassifyProbe wraps a version-probe failure without probing authentication.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "kimi is not on PATH", err)
	}
	if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
		return cartridge.NewError(k, name, what+" did not finish", err)
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
		if text := strings.TrimSpace(lines[i]); text != "" {
			return text
		}
	}
	return ""
}
