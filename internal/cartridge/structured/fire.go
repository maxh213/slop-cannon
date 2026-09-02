package structured

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"slop-cannon/internal/cartridge"
)

// DefaultMaxRetries is how many extra sessions Fire runs when the reply has
// no valid block: one, as the task contract says.
const DefaultMaxRetries = 1

// RetryOutputTail is how much of the previous reply the retry prompt quotes,
// in runes, counted from the end (the block lives there).
const RetryOutputTail = 4000

// RetryContext is what a retry prompt builder gets.
type RetryContext struct {
	// Prompt is the prompt of the failed attempt (suffix included).
	Prompt string
	// Output is the failed attempt's final text.
	Output string
	// Err is why it failed.
	Err *ParseError
	// Attempt is the number of the attempt that failed, starting at 1.
	Attempt int
	// Contract is the contract being parsed.
	Contract *Contract
}

// RetryPromptFunc builds the prompt for the next attempt.
type RetryPromptFunc func(RetryContext) string

// Option configures Fire.
type Option func(*options)

type options struct {
	observer   func(cartridge.SessionEvent)
	maxRetries int
	retry      RetryPromptFunc
	noSuffix   bool
}

// WithObserver calls fn for every session event of every attempt, in
// order, before Fire returns.
func WithObserver(fn func(cartridge.SessionEvent)) Option {
	return func(o *options) { o.observer = fn }
}

// WithMaxRetries sets how many extra sessions Fire may run after a reply
// without a valid block (default DefaultMaxRetries; 0 disables retries;
// negative is treated as 0).
func WithMaxRetries(n int) Option {
	return func(o *options) { o.maxRetries = max(n, 0) }
}

// WithRetryPrompt replaces RetryPrompt as the builder of retry prompts.
func WithRetryPrompt(fn RetryPromptFunc) Option {
	return func(o *options) { o.retry = fn }
}

// WithoutPromptSuffix stops Fire from appending Contract.PromptSuffix to the
// spec's prompt (when the prompt builder has laid the contract out itself
// in another form). Fire never appends the suffix twice anyway.
func WithoutPromptSuffix() Option {
	return func(o *options) { o.noSuffix = true }
}

// Attempt is one session Fire ran.
type Attempt struct {
	// Prompt is what the session was asked.
	Prompt string
	// Result is what the session returned (Usage is filled even on failure
	// when the adapter reports it).
	Result cartridge.Result
	// Err is the parse error when the reply had no valid block, the
	// cartridge error when the session failed, or nil for the attempt that
	// succeeded.
	Err error
}

// Outcome is what Fire produced.
type Outcome struct {
	// Parsed is the validated block; empty when Fire returned an error.
	Parsed Parsed
	// Result is the successful attempt's result (Output holds the full
	// text, block included).
	Result cartridge.Result
	// Attempts lists every session run, in order, failures included.
	Attempts []Attempt
	// Usage is the sum over all attempts.
	Usage cartridge.Usage
}

// Fire runs spec through c, asking for contract's block, and parses the
// reply. When the reply has no valid block it fires again with a retry
// prompt (RetryPrompt by default) that quotes the failure, up to
// DefaultMaxRetries extra times. The returned error is the last *ParseError
// when every attempt's reply was unusable, or the *cartridge.Error of the
// first session that failed: cartridge failures are never retried here, the
// supervisor owns that policy. Cancelling ctx kills the running session.
func Fire(ctx context.Context, c cartridge.Cartridge, spec cartridge.RunSpec, contract *Contract, opts ...Option) (Outcome, error) {
	if c == nil {
		return Outcome{}, errors.New("structured: nil cartridge")
	}
	if contract == nil {
		return Outcome{}, errors.New("structured: nil contract")
	}
	o := options{maxRetries: DefaultMaxRetries, retry: RetryPrompt}
	for _, opt := range opts {
		opt(&o)
	}
	if !o.noSuffix {
		spec.Prompt = contract.WithPromptSuffix(spec.Prompt)
	}
	var out Outcome
	var lastErr error
	for attempt := 1; attempt <= 1+o.maxRetries; attempt++ {
		if attempt > 1 {
			prev := out.Attempts[len(out.Attempts)-1]
			pe, _ := AsParseError(prev.Err)
			spec.Prompt = o.retry(RetryContext{Prompt: prev.Prompt, Output: prev.Result.Output, Err: pe, Attempt: attempt - 1, Contract: contract})
		}
		res, err := runOnce(ctx, c, spec, o.observer)
		out.Usage = out.Usage.Add(res.Usage)
		if err != nil {
			out.Attempts = append(out.Attempts, Attempt{Prompt: spec.Prompt, Result: res, Err: err})
			return out, err
		}
		parsed, perr := contract.Parse(res.Output)
		if perr == nil {
			out.Attempts = append(out.Attempts, Attempt{Prompt: spec.Prompt, Result: res})
			out.Parsed, out.Result = parsed, res
			return out, nil
		}
		out.Attempts = append(out.Attempts, Attempt{Prompt: spec.Prompt, Result: res, Err: perr})
		lastErr = perr
		if err := ctx.Err(); err != nil {
			return out, cartridge.AsError(c.Name(), err)
		}
	}
	return out, lastErr
}

func runOnce(ctx context.Context, c cartridge.Cartridge, spec cartridge.RunSpec, observe func(cartridge.SessionEvent)) (cartridge.Result, error) {
	s, err := c.Run(ctx, spec)
	if err != nil {
		return cartridge.Result{}, err
	}
	return cartridge.Collect(s, observe)
}

// RetryPrompt is the default retry prompt: the original prompt, then a
// correction section that quotes the failure (ParseError.Feedback) and the
// tail of the previous reply, and tells the model not to redo work that
// already landed in the working directory.
func RetryPrompt(rc RetryContext) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(rc.Prompt, "\n"))
	b.WriteString("\n\n## Correction required\n\n")
	feedback := "it did not contain a usable result"
	if rc.Err != nil {
		feedback = rc.Err.Feedback()
	}
	fmt.Fprintf(&b, "This is attempt %d. Your previous reply was rejected because %s.\n\n", rc.Attempt+1, feedback)
	b.WriteString("Any work you already completed in the working directory is still there; do not redo it. ")
	b.WriteString("Reply again, ending with exactly one ```" + Fence + " block that contains the complete, valid JSON object matching the schema above.")
	if tail := lastRunes(rc.Output, RetryOutputTail); tail != "" {
		b.WriteString("\n\nThe end of your previous reply, for reference:\n\n<previous-reply>\n")
		b.WriteString(tail)
		b.WriteString("\n</previous-reply>\n")
	} else {
		b.WriteString("\n")
	}
	return b.String()
}

func lastRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
