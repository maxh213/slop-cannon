package structured

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

// structuredReply is one scripted session outcome.
type structuredReply struct {
	output string
	usage  cartridge.Usage
	err    error // session error (Wait) when non-nil
	runErr error // Run error when non-nil: nothing starts
}

// structuredFakeCartridge answers each Run with the next scripted reply and
// records every RunSpec it received.
type structuredFakeCartridge struct {
	mu      sync.Mutex
	replies []structuredReply
	specs   []cartridge.RunSpec
}

var _ cartridge.Cartridge = (*structuredFakeCartridge)(nil)

func (f *structuredFakeCartridge) Name() string { return "fake" }

func (f *structuredFakeCartridge) Detect(context.Context) (cartridge.Status, error) {
	return cartridge.Status{Installed: true}, nil
}

func (f *structuredFakeCartridge) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	if len(f.replies) == 0 {
		f.mu.Unlock()
		return nil, cartridge.NewError(cartridge.KindCrash, "fake", "no scripted reply left", nil)
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()
	if r.runErr != nil {
		return nil, r.runErr
	}
	p := cartridge.NewPipe("fake", nil)
	go func() {
		select {
		case <-ctx.Done():
			p.Finish(cartridge.Result{Usage: r.usage}, ctx.Err())
			return
		default:
		}
		p.Emit(cartridge.TextDelta(r.output))
		p.Finish(cartridge.Result{Output: r.output, Usage: r.usage, ExitCode: 0}, r.err)
	}()
	return p, nil
}

func (f *structuredFakeCartridge) prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.specs))
	for i, s := range f.specs {
		out[i] = s.Prompt
	}
	return out
}

func structuredSpec(t *testing.T) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{Prompt: "Review the diff.", Workdir: t.TempDir(), ToolPolicy: cartridge.ToolReadExec, Timeout: time.Minute}
}

const structuredGood = "All good.\n\n```sc-result\n{\"outcome\": \"approve\", \"items\": []}\n```\n"

func TestFireFirstAttemptSucceeds(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	fake := &structuredFakeCartridge{replies: []structuredReply{{output: structuredGood, usage: cartridge.Usage{InputTokens: 10, OutputTokens: 5}}}}
	var seen []cartridge.SessionEvent
	out, err := Fire(context.Background(), fake, structuredSpec(t), c, WithObserver(func(ev cartridge.SessionEvent) { seen = append(seen, ev) }))
	require.NoError(t, err)
	assert.Len(t, out.Attempts, 1)
	assert.Equal(t, "verdict", out.Parsed.Contract)
	assert.JSONEq(t, `{"outcome":"approve","items":[]}`, string(out.Parsed.JSON))
	assert.Equal(t, structuredGood, out.Result.Output)
	assert.Equal(t, 15, out.Usage.TotalTokens())
	require.Len(t, seen, 2)
	assert.Equal(t, cartridge.EventTextDelta, seen[0].Kind)
	assert.Equal(t, cartridge.EventDone, seen[1].Kind)

	prompts := fake.prompts()
	require.Len(t, prompts, 1)
	assert.True(t, strings.HasPrefix(prompts[0], "Review the diff.\n\n## Output contract (verdict)"), "suffix appended: %q", prompts[0])
	assert.Equal(t, 1, strings.Count(prompts[0], "## Output contract"))
}

func TestFireRetriesOnceWithFeedback(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	bad := "Verdict:\n\n```sc-result\n{\"outcome\": \"maybe\", \"items\": []}\n```\n"
	fake := &structuredFakeCartridge{replies: []structuredReply{
		{output: bad, usage: cartridge.Usage{InputTokens: 1}},
		{output: structuredGood, usage: cartridge.Usage{InputTokens: 2}},
	}}
	out, err := Fire(context.Background(), fake, structuredSpec(t), c)
	require.NoError(t, err)
	require.Len(t, out.Attempts, 2)
	assert.ErrorIs(t, out.Attempts[0].Err, ErrSchema)
	assert.NoError(t, out.Attempts[1].Err)
	assert.Equal(t, bad, out.Attempts[0].Result.Output)
	assert.Equal(t, 3, out.Usage.InputTokens, "usage summed over attempts")
	assert.Equal(t, "approve", func() string {
		var v struct {
			Outcome string `json:"outcome"`
		}
		require.NoError(t, out.Parsed.Decode(&v))
		return v.Outcome
	}())

	prompts := fake.prompts()
	require.Len(t, prompts, 2)
	retry := prompts[1]
	assert.True(t, strings.HasPrefix(retry, prompts[0]), "retry prompt starts with the original prompt")
	assert.Equal(t, 1, strings.Count(retry, "## Output contract"), "schema shown once")
	assert.Contains(t, retry, "## Correction required")
	assert.Contains(t, retry, "This is attempt 2.")
	assert.Contains(t, retry, `did not match the "verdict" schema:`)
	assert.Contains(t, retry, "at '/outcome'")
	assert.Contains(t, retry, "do not redo it")
	assert.Contains(t, retry, "<previous-reply>\n"+strings.TrimSpace(bad)+"\n</previous-reply>")
}

func TestFireGivesUpAfterRetries(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	fake := &structuredFakeCartridge{replies: []structuredReply{{output: "no block here"}, {output: "still nothing"}, {output: structuredGood}}}
	out, err := Fire(context.Background(), fake, structuredSpec(t), c)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissing)
	pe, ok := AsParseError(err)
	require.True(t, ok)
	assert.Equal(t, "verdict", pe.Contract)
	assert.Len(t, out.Attempts, 2, "one retry by default")
	assert.Empty(t, out.Parsed.JSON)
	assert.Len(t, fake.prompts(), 2)
}

func TestFireMaxRetriesOption(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	t.Run("zero disables retry", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{{output: "nothing"}, {output: structuredGood}}}
		out, err := Fire(context.Background(), fake, structuredSpec(t), c, WithMaxRetries(0))
		assert.ErrorIs(t, err, ErrMissing)
		assert.Len(t, out.Attempts, 1)
	})
	t.Run("negative is zero", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{{output: "nothing"}, {output: structuredGood}}}
		out, err := Fire(context.Background(), fake, structuredSpec(t), c, WithMaxRetries(-3))
		assert.ErrorIs(t, err, ErrMissing)
		assert.Len(t, out.Attempts, 1)
	})
	t.Run("two retries", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{{output: "nothing"}, {output: "```sc-result\n{\n"}, {output: structuredGood}}}
		out, err := Fire(context.Background(), fake, structuredSpec(t), c, WithMaxRetries(2))
		require.NoError(t, err)
		require.Len(t, out.Attempts, 3)
		assert.ErrorIs(t, out.Attempts[0].Err, ErrMissing)
		assert.ErrorIs(t, out.Attempts[1].Err, ErrTruncated)
		assert.Contains(t, fake.prompts()[2], "This is attempt 3.")
		assert.Contains(t, fake.prompts()[2], "cut off before the JSON was complete")
	})
}

func TestFireCustomRetryPromptAndNoSuffix(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	fake := &structuredFakeCartridge{replies: []structuredReply{{output: "nothing"}, {output: structuredGood}}}
	var got RetryContext
	_, err := Fire(context.Background(), fake, structuredSpec(t), c,
		WithoutPromptSuffix(),
		WithRetryPrompt(func(rc RetryContext) string { got = rc; return "custom retry" }))
	require.NoError(t, err)
	prompts := fake.prompts()
	require.Len(t, prompts, 2)
	assert.Equal(t, "Review the diff.", prompts[0], "suffix not appended")
	assert.Equal(t, "custom retry", prompts[1])
	assert.Equal(t, "Review the diff.", got.Prompt)
	assert.Equal(t, "nothing", got.Output)
	assert.Equal(t, 1, got.Attempt)
	assert.Same(t, c, got.Contract)
	require.NotNil(t, got.Err)
	assert.Equal(t, ReasonMissing, got.Err.Reason)
}

func TestFireDoesNotRetryCartridgeFailures(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	t.Run("session error", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{
			{err: cartridge.NewError(cartridge.KindRateLimited, "fake", "slow down", nil), usage: cartridge.Usage{OutputTokens: 7}},
			{output: structuredGood},
		}}
		out, err := Fire(context.Background(), fake, structuredSpec(t), c)
		require.Error(t, err)
		assert.ErrorIs(t, err, cartridge.ErrRateLimited)
		_, isParse := AsParseError(err)
		assert.False(t, isParse)
		require.Len(t, out.Attempts, 1)
		assert.ErrorIs(t, out.Attempts[0].Err, cartridge.ErrRateLimited)
		assert.Equal(t, 7, out.Usage.OutputTokens, "partial usage kept")
		assert.Len(t, fake.prompts(), 1)
	})
	t.Run("run error", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{{runErr: cartridge.NewError(cartridge.KindCreditsExhausted, "fake", "", nil)}}}
		out, err := Fire(context.Background(), fake, structuredSpec(t), c)
		assert.ErrorIs(t, err, cartridge.ErrCreditsExhausted)
		assert.Len(t, out.Attempts, 1)
	})
	t.Run("spec validation error", func(t *testing.T) {
		fake := &structuredFakeCartridge{replies: []structuredReply{{output: structuredGood}}}
		spec := structuredSpec(t)
		spec.Workdir = "relative"
		_, err := Fire(context.Background(), fake, spec, c)
		require.ErrorContains(t, err, "not absolute")
	})
	t.Run("nil arguments", func(t *testing.T) {
		_, err := Fire(context.Background(), nil, structuredSpec(t), c)
		assert.ErrorContains(t, err, "nil cartridge")
		_, err = Fire(context.Background(), &structuredFakeCartridge{}, structuredSpec(t), nil)
		assert.ErrorContains(t, err, "nil contract")
	})
}

func TestFireStopsWhenContextCancelled(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	ctx, cancel := context.WithCancel(context.Background())
	fake := &structuredFakeCartridge{replies: []structuredReply{{output: "nothing"}, {output: structuredGood}}}
	_, err := Fire(ctx, fake, structuredSpec(t), c, WithObserver(func(cartridge.SessionEvent) { cancel() }))
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrKilled)
	assert.Len(t, fake.prompts(), 1, "no retry after cancellation")
}

func TestRetryPromptTruncatesLongOutput(t *testing.T) {
	long := strings.Repeat("x", RetryOutputTail+100) + "END"
	p := RetryPrompt(RetryContext{Prompt: "P", Output: long, Err: &ParseError{Reason: ReasonMissing}, Attempt: 1})
	assert.Contains(t, p, "…")
	assert.Contains(t, p, "END\n</previous-reply>")
	assert.NotContains(t, p, strings.Repeat("x", RetryOutputTail+1))
	assert.True(t, strings.HasPrefix(p, "P\n\n## Correction required"))

	empty := RetryPrompt(RetryContext{Prompt: "P", Err: nil, Attempt: 1})
	assert.NotContains(t, empty, "<previous-reply>")
	assert.Contains(t, empty, "usable result")
	assert.False(t, errors.Is(nil, ErrMissing))
}
