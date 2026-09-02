package mock

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

// mockTestdata returns the path of a scenario fixture under
// testdata/cartridge/mock in the repository root.
func mockTestdata(t testing.TB, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "cartridge", "mock", name))
	require.NoError(t, err)
	return p
}

func TestLoadFixturesParseAndValidate(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"review-loop.json", "failures.json", "detect-credits.json"} {
		s, err := Load(mockTestdata(t, name))
		require.NoError(t, err, name)
		require.NoError(t, s.Validate(), name)
	}
	s, err := Load(mockTestdata(t, "review-loop.json"))
	require.NoError(t, err)
	assert.Equal(t, "review loop", s.Name)
	require.Len(t, s.Rules, 4)
	assert.Equal(t, "implement", s.Rules[1].Name)
	assert.Equal(t, 250*time.Millisecond, s.Rules[1].Steps[0].Delay.Duration())
	assert.True(t, s.Rules[1].Steps[2].Must)
	require.NotNil(t, s.Fallback)
	assert.Equal(t, StepEcho, s.Fallback.Steps[0].Kind)
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load scenario")
}

func TestParseRejectsBadScenarios(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"unknown field":         `{"rules":[], "bogus": 1}`,
		"numeric duration":      `{"rules":[{"name":"a","match":{},"steps":[{"kind":"sleep","delay":100}]}]}`,
		"negative duration":     `{"rules":[{"name":"a","match":{},"steps":[{"kind":"sleep","delay":"-1s"}]}]}`,
		"unknown step kind":     `{"rules":[{"name":"a","match":{},"steps":[{"kind":"dance"}]}]}`,
		"unknown failure kind":  `{"rules":[{"name":"a","match":{},"steps":[{"kind":"fail","failure":{"kind":"meltdown"}}]}]}`,
		"fail without failure":  `{"rules":[{"name":"a","match":{},"steps":[{"kind":"fail"}]}]}`,
		"exec without command":  `{"rules":[{"name":"a","match":{},"steps":[{"kind":"exec"}]}]}`,
		"write without path":    `{"rules":[{"name":"a","match":{},"steps":[{"kind":"write","content":"x"}]}]}`,
		"absolute path":         `{"rules":[{"name":"a","match":{},"steps":[{"kind":"write","path":"/etc/passwd"}]}]}`,
		"escaping path":         `{"rules":[{"name":"a","match":{},"steps":[{"kind":"write","path":"../x"}]}]}`,
		"nameless rule":         `{"rules":[{"match":{}}]}`,
		"duplicate rule":        `{"rules":[{"name":"a","match":{}},{"name":"a","match":{}}]}`,
		"fallback duplicates":   `{"rules":[{"name":"a","match":{}}],"fallback":{"name":"a"}}`,
		"negative max uses":     `{"rules":[{"name":"a","match":{},"max_uses":-1}]}`,
		"bad structured":        `{"rules":[{"name":"a","match":{},"result":{"structured":{"x":tru}}}]}`,
		"bad detect auth":       `{"detect":{"auth":"maybe"},"rules":[]}`,
		"bad detect failure":    `{"detect":{"failure":{"kind":"nope"}},"rules":[]}`,
		"negative exit code":    `{"rules":[{"name":"a","match":{},"steps":[{"kind":"fail","failure":{"kind":"crash","exit_code":-2}}]}]}`,
		"not json at all":       `hello`,
		"rule steps not a list": `{"rules":[{"name":"a","match":{},"steps":{}}]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(src))
			assert.Error(t, err)
		})
	}
}

func TestScenarioRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewScenario("built")
	s.Detect.Version = "mock/9"
	s.Rule("eng", Match{Role: "engineer", Round: 2, Labels: map[string]string{"extra": "*"}}).
		Say("hello").Delay(300*time.Millisecond).
		Tool("Read", "t1", "main.go").
		ToolResult("Read", "t1", "package main", false).
		Write("a.txt", "A").AppendFile("a.txt", "B").Delete("b.txt").
		Exec("true").MustExec("false").
		Sleep(time.Second).
		Structured(map[string]int{"n": 1}).
		Usage(cartridge.Usage{InputTokens: 5}).
		SessionID("s1").Times(3)
	s.Rule("limit", Match{Lens: "qa"}).Once().FailWith(Failure{Kind: FailRateLimited, Message: "slow", RetryAfter: Duration(time.Minute)})
	s.Otherwise("echo").Echo().Output("fixed")
	require.NoError(t, s.Validate())

	data, err := s.Marshal()
	require.NoError(t, err)
	back, err := Parse(data)
	require.NoError(t, err)

	// Unexported fields aside, the round trip is lossless.
	want, err := json.Marshal(s)
	require.NoError(t, err)
	got, err := json.Marshal(back)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
	assert.Equal(t, 3, back.Rules[0].MaxUses)
	assert.Equal(t, 300*time.Millisecond, back.Rules[0].Steps[0].Delay.Duration())
	assert.Equal(t, time.Minute, back.Rules[1].Steps[0].Failure.RetryAfter.Duration())
	assert.Equal(t, "fixed", back.Fallback.Result.Output)

	path := filepath.Join(t.TempDir(), "nested", "s.json")
	require.NoError(t, s.WriteFile(path))
	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "built", loaded.Name)
}

func TestBuilderSurfacesEncodingErrors(t *testing.T) {
	t.Parallel()
	s := NewScenario("bad")
	s.Rule("r", Match{}).Structured(make(chan int))
	err := s.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `rule "r"`)
	assert.Contains(t, err.Error(), "structured result")

	s2 := NewScenario("bad2")
	s2.Rule("r", Match{}).Structured("{not json")
	require.Error(t, s2.Validate())

	var nilRule *Rule
	require.Error(t, nilRule.Validate())
	var nilScenario *Scenario
	require.Error(t, nilScenario.Validate())
}

func TestDelayOnEmptyRuleIsNoop(t *testing.T) {
	t.Parallel()
	r := NewScenario("x").Rule("r", Match{}).Delay(time.Second)
	assert.Empty(t, r.Steps)
}

func TestMatchMatches(t *testing.T) {
	t.Parallel()
	spec := cartridge.RunSpec{
		SystemPrompt: "You are QA.",
		Prompt:       "Review bead sc-1 please",
		Workdir:      "/tmp/x",
		Model:        "fable",
		ToolPolicy:   cartridge.ToolReadExec,
		Labels: cartridge.Labels{
			cartridge.LabelRole: "tester", cartridge.LabelLens: "qa", cartridge.LabelBead: "sc-1",
			cartridge.LabelRound: "2", cartridge.LabelTask: "review", cartridge.LabelAgent: "tester-qa-sc-1-r2",
			"custom": "yes",
		},
	}
	cases := []struct {
		name string
		m    Match
		want bool
	}{
		{"empty matches all", Match{}, true},
		{"role", Match{Role: "tester"}, true},
		{"wrong role", Match{Role: "engineer"}, false},
		{"lens", Match{Lens: "qa"}, true},
		{"wrong lens", Match{Lens: "ux"}, false},
		{"bead", Match{Bead: "sc-1"}, true},
		{"wrong bead", Match{Bead: "sc-2"}, false},
		{"round", Match{Round: 2}, true},
		{"wrong round", Match{Round: 1}, false},
		{"task", Match{Task: "review"}, true},
		{"wrong task", Match{Task: "implement"}, false},
		{"agent", Match{Agent: "tester-qa-sc-1-r2"}, true},
		{"wrong agent", Match{Agent: "other"}, false},
		{"label value", Match{Labels: map[string]string{"custom": "yes"}}, true},
		{"label wildcard", Match{Labels: map[string]string{"custom": "*"}}, true},
		{"label wrong value", Match{Labels: map[string]string{"custom": "no"}}, false},
		{"label absent", Match{Labels: map[string]string{"missing": "*"}}, false},
		{"prompt", Match{PromptContains: "sc-1"}, true},
		{"wrong prompt", Match{PromptContains: "sc-9"}, false},
		{"system prompt", Match{SystemPromptContains: "QA"}, true},
		{"wrong system prompt", Match{SystemPromptContains: "UX"}, false},
		{"policy", Match{ToolPolicy: "read_exec"}, true},
		{"wrong policy", Match{ToolPolicy: "read_only"}, false},
		{"model", Match{Model: "fable"}, true},
		{"wrong model", Match{Model: "other"}, false},
		{"combined", Match{Role: "tester", Lens: "qa", Round: 2, PromptContains: "Review"}, true},
		{"combined one wrong", Match{Role: "tester", Lens: "qa", Round: 3}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.m.Matches(spec))
		})
	}
	assert.True(t, Match{}.Matches(cartridge.RunSpec{}), "nil labels still match an empty Match")
	assert.False(t, Match{Role: "po"}.Matches(cartridge.RunSpec{}))
}

func TestMatchString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "*", Match{}.String())
	m := Match{Role: "tester", Lens: "qa", Round: 2, Labels: map[string]string{"z": "1", "a": "*"}, PromptContains: "x", ToolPolicy: "read_only", Model: "m"}
	assert.Equal(t, "role=tester lens=qa round=2 a=* z=1 prompt~=x policy=read_only model=m", m.String())
}

func TestFailKinds(t *testing.T) {
	t.Parallel()
	assert.Len(t, FailKinds(), 8)
	for _, k := range FailKinds() {
		assert.True(t, k.Valid(), k)
		assert.Equal(t, string(k), k.String())
	}
	assert.False(t, FailKind("nope").Valid())
	assert.Equal(t, cartridge.KindCrash, FailCrash.CartridgeKind())
	assert.Equal(t, cartridge.KindTimeout, FailTimeout.CartridgeKind())
	assert.Equal(t, cartridge.KindRateLimited, FailRateLimited.CartridgeKind())
	assert.Equal(t, cartridge.KindCreditsExhausted, FailCreditsExhausted.CartridgeKind())
	assert.Equal(t, cartridge.KindAuth, FailAuth.CartridgeKind())
	assert.Equal(t, cartridge.KindKilled, FailKilled.CartridgeKind())
	assert.Equal(t, cartridge.KindNotInstalled, FailNotInstalled.CartridgeKind())
	assert.Equal(t, cartridge.KindUnknown, FailMalformed.CartridgeKind())

	assert.Nil(t, Failure{Kind: FailMalformed}.Err("mock"))
	e := Failure{Kind: FailRateLimited, RetryAfter: Duration(time.Minute)}.Err("mock")
	require.NotNil(t, e)
	assert.Equal(t, cartridge.KindRateLimited, e.Kind)
	assert.Equal(t, "mock", e.Cartridge)
	assert.Equal(t, "injected rate_limited", e.Msg)
	assert.Equal(t, time.Minute, e.RetryAfter)
	assert.ErrorIs(t, e, cartridge.ErrRateLimited)

	assert.Len(t, StepKinds(), 9)
	for _, k := range StepKinds() {
		assert.True(t, k.Valid(), k)
		assert.Equal(t, string(k), k.String())
	}
	assert.False(t, StepKind("x").Valid())
}

func TestResultBlock(t *testing.T) {
	t.Parallel()
	b, err := ResultBlock(map[string]any{"ok": true})
	require.NoError(t, err)
	assert.Equal(t, "```sc-result\n{\"ok\":true}\n```", b)
	b, err = ResultBlock("raw text\n")
	require.NoError(t, err)
	assert.Equal(t, "```sc-result\nraw text\n```", b)
	b, err = ResultBlock(json.RawMessage(`[1]`))
	require.NoError(t, err)
	assert.Equal(t, "```sc-result\n[1]\n```", b)
	b, err = ResultBlock([]byte(`2`))
	require.NoError(t, err)
	assert.Equal(t, "```sc-result\n2\n```", b)
	_, err = ResultBlock(make(chan int))
	require.Error(t, err)
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()
	var d Duration
	require.NoError(t, json.Unmarshal([]byte(`"1m30s"`), &d))
	assert.Equal(t, 90*time.Second, d.Duration())
	require.Error(t, json.Unmarshal([]byte(`90`), &d))
	require.Error(t, json.Unmarshal([]byte(`"soon"`), &d))
	out, err := json.Marshal(Duration(1500 * time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, `"1.5s"`, string(out))
}

func TestDefaultScenario(t *testing.T) {
	t.Parallel()
	s := DefaultScenario()
	require.NoError(t, s.Validate())
	assert.Empty(t, s.Rules)
	require.NotNil(t, s.Fallback)
	assert.Equal(t, StepEcho, s.Fallback.Steps[0].Kind)
}
