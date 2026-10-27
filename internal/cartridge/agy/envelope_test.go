package agy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

func TestParserRejectsUnknownStatus(t *testing.T) {
	p := NewParser()
	p.Stdout(`{"conversation_id":"c","status":"MAYBE","response":"not final"}`)
	p.Parse()
	require.NoError(t, p.parseErr)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(Classify(Name, p, nil, 0)))
}

func TestParserBoundsDiagnostics(t *testing.T) {
	p := NewParser()
	for i := 0; i < maxDiagnosticLines+20; i++ {
		p.Stderr(fmt.Sprintf("line-%d-%s", i, strings.Repeat("x", maxDiagnosticLine*2)))
	}
	assert.Len(t, p.stderr, maxDiagnosticLines)
	for _, line := range p.stderr {
		assert.LessOrEqual(t, len(line), maxDiagnosticLine+len("…"))
	}
}

func TestClassifyAgentErrorHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   cartridge.Kind
	}{
		{401, cartridge.KindAuth}, {402, cartridge.KindCreditsExhausted},
		{429, cartridge.KindRateLimited}, {503, cartridge.KindRateLimited},
		{404, cartridge.KindCrash},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			p := NewParser()
			p.agentErr = &agentError{ShortError: "request failed", ErrorCode: tc.status}
			assert.Equal(t, tc.kind, cartridge.KindOf(Classify(Name, p, nil, 0)))
		})
	}
}

func TestClassifyAgentErrorCanonicalStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		kind   cartridge.Kind
	}{
		{"UNAUTHENTICATED", cartridge.KindAuth},
		{"PERMISSION_DENIED", cartridge.KindAuth},
		{"RESOURCE_EXHAUSTED", cartridge.KindRateLimited},
		{"UNAVAILABLE", cartridge.KindRateLimited},
	} {
		t.Run(tc.status, func(t *testing.T) {
			p := NewParser()
			p.agentErr = &agentError{Status: tc.status}
			assert.Equal(t, tc.kind, cartridge.KindOf(Classify(Name, p, nil, 0)))
		})
	}
}

func TestErrorEnvelopeDoesNotRequireResponse(t *testing.T) {
	p := NewParser()
	p.Stdout(`{"conversation_id":"c","status":"ERROR","response":"","error":"authentication required"}`)
	p.Parse()
	require.NoError(t, p.parseErr)
	assert.ErrorIs(t, Classify(Name, p, nil, 0), cartridge.ErrAuth)
}

func TestSuccessRequiresConsumableResponse(t *testing.T) {
	p := NewParser()
	p.Stdout(`{"conversation_id":"c","status":"SUCCESS","response":"  \n","denied_actions":[{"action":"command","display_name":"RunCommand"}]}`)
	p.Parse()
	require.NoError(t, p.parseErr)
	err := Classify(Name, p, nil, 0)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Contains(t, err.Error(), "empty response")
	assert.Contains(t, err.Error(), "1 denied tool action")
}
