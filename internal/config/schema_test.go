package config

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

func TestAllocate(t *testing.T) {
	tests := []struct {
		ammo      int
		eng, test int
		want      string
	}{
		{ammo: 3, eng: 1, test: 1, want: "ammo 3 → 1 PO, up to 1 engineer, up to 1 tester"},
		{ammo: 4, eng: 1, test: 2, want: "ammo 4 → 1 PO, up to 1 engineer, up to 2 testers"},
		{ammo: 10, eng: 2, test: 7, want: "ammo 10 → 1 PO, up to 2 engineers, up to 7 testers"},
		{ammo: 12, eng: 2, test: 9, want: "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers"},
		{ammo: 13, eng: 3, test: 9, want: "ammo 13 → 1 PO, up to 3 engineers, up to 9 testers"},
	}
	for _, tc := range tests {
		a, err := Allocate(tc.ammo)
		require.NoError(t, err, tc.ammo)
		assert.Equal(t, Allocation{Ammo: tc.ammo, PO: 1, MaxEngineers: tc.eng, MaxTesters: tc.test}, a)
		assert.Equal(t, tc.want, a.String())
		assert.Equal(t, tc.ammo, a.PO+a.MaxEngineers+a.MaxTesters, "slots add up")
	}
	for _, low := range []int{-1, 0, 1, 2} {
		_, err := Allocate(low)
		assert.ErrorIs(t, err, ErrAmmoTooLow, low)
	}
	assert.Equal(t, "ammo must be at least 3: one engineer, one tester, one product owner", ErrAmmoTooLow.Error())
}

func TestValidateCartridgeName(t *testing.T) {
	assert.NoError(t, ValidateCartridgeName("claude"))
	assert.EqualError(t, ValidateCartridgeName("kimi"), `cartridge "kimi" is not supported yet (only "claude" for now)`)
	assert.EqualError(t, ValidateCartridgeName("animus"), `cartridge "animus" is not supported yet (only "claude" for now)`)
	assert.Error(t, ValidateCartridgeName(""))
	assert.Error(t, ValidateCartridgeName("Claude"), "names are case-sensitive")
}

func TestParseSafety(t *testing.T) {
	for _, s := range []string{"on", "ON", "true", "yes", "1", " on "} {
		v, err := ParseSafety(s)
		require.NoError(t, err, s)
		assert.True(t, v, s)
	}
	for _, s := range []string{"off", "false", "no", "0"} {
		v, err := ParseSafety(s)
		require.NoError(t, err, s)
		assert.False(t, v, s)
	}
	_, err := ParseSafety("maybe")
	assert.EqualError(t, err, `safety must be on or off, got "maybe"`)
}

func TestDurationText(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		out  string
	}{
		{"10m", 10 * time.Minute, "10m"},
		{"1h", time.Hour, "1h"},
		{"1h30m", 90 * time.Minute, "1h30m"},
		{"90s", 90 * time.Second, "1m30s"},
		{"500ms", 500 * time.Millisecond, "500ms"},
		{"0s", 0, "0s"},
		{"2h0m0s", 2 * time.Hour, "2h"},
	}
	for _, tc := range tests {
		var d Duration
		require.NoError(t, d.UnmarshalText([]byte(tc.in)), tc.in)
		assert.Equal(t, tc.want, d.Duration(), tc.in)
		assert.Equal(t, tc.out, d.String(), tc.in)
		b, err := d.MarshalText()
		require.NoError(t, err)
		assert.Equal(t, tc.out, string(b))
	}
	var d Duration
	assert.EqualError(t, d.UnmarshalText([]byte("10x")), `invalid duration "10x" (use a Go duration such as "10m" or "1h30m")`)

	// JSON uses the same text form.
	js, err := json.Marshal(Limits{AgentTimeout: NewDuration(DefaultAgentTimeout), MaxReviewRounds: 5, MaxPushbackRounds: 2,
		MaxConflictRounds: DefaultMaxConflictRounds,
		MaxPlanRejections: DefaultMaxPlanRejections, MaxPlanFailures: DefaultMaxPlanFailures})
	require.NoError(t, err)
	assert.JSONEq(t, `{"max_review_rounds":5,"max_pushback_rounds":2,"max_conflict_rounds":3,"agent_timeout":"30m",`+
		`"max_plan_rejections":2,"max_plan_failures":5}`, string(js))
}

func TestDefaultsMatchD04(t *testing.T) {
	d := Defaults()
	assert.Equal(t, 10, d.Ammo)
	assert.True(t, d.Safety)
	assert.Empty(t, d.Vibes)
	assert.Empty(t, d.Rules)
	assert.Empty(t, d.Cartridges)
	assert.Equal(t, "", d.ActiveCartridge)
	assert.Equal(t, 10*time.Minute, d.IdleReplanInterval.Duration())
	assert.Equal(t, time.Minute, d.GatePollInterval.Duration())
	assert.Equal(t, time.Minute, d.PRPollInterval.Duration())
	assert.Equal(t, 2*time.Second, d.GraphPollInterval.Duration())
	assert.Equal(t, 200*1024, d.MaxDiffBytes)
	assert.InEpsilon(t, 0.4, d.ReReviewThreshold, 1e-9)
	assert.Equal(t, DefaultMaxBeadsPerPlan, d.MaxBeadsPerPlan)
	assert.Equal(t, 8, d.MaxBeadsPerPlan)
	assert.Equal(t, Limits{MaxReviewRounds: 5, MaxPushbackRounds: 2, MaxConflictRounds: 3,
		AgentTimeout: NewDuration(30 * time.Minute), MaxPlanRejections: 2, MaxPlanFailures: 5}, d.Limits)
	assert.Empty(t, d.Roles)
}

func TestConfigHelpers(t *testing.T) {
	c := Defaults()
	_, _, ok := c.Active()
	assert.False(t, ok)
	_, ok = c.CartridgeFor(domain.RoleTester)
	assert.False(t, ok)

	c.Cartridges["claude"] = Cartridge{Model: "fable", Command: "/opt/claude"}
	c.ActiveCartridge = "claude"
	c.Roles[domain.RoleTester] = "claude"
	c.Vibes = []string{"/v.md"}

	name, cart, ok := c.Active()
	assert.True(t, ok)
	assert.Equal(t, "claude", name)
	assert.Equal(t, "/opt/claude", cart.Binary("claude"))
	assert.Equal(t, "claude", Cartridge{}.Binary("claude"))

	n, ok := c.CartridgeFor(domain.RoleEngineer)
	assert.True(t, ok)
	assert.Equal(t, "claude", n)

	snap := c.Snapshot()
	assert.Equal(t, domain.ConfigSnapshot{Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable", Vibes: []string{"/v.md"}}, snap)
	snap.Vibes[0] = "changed"
	assert.Equal(t, "/v.md", c.Vibes[0], "snapshot copies slices")

	f := File{Cartridges: map[string]Cartridge{"zeta": {}, "claude": {}}}
	assert.Equal(t, []string{"claude", "zeta"}, f.CartridgeNames())
}
