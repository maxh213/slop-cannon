package dash

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func balanceTestRaw(balance string) string {
	provider := `{"id":"kilo","displayName":"Kilo","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":[]`
	if balance != "" {
		provider += `,"balance":` + balance
	}
	provider += `}`
	return dandelionTestWithProvider(provider)
}

func TestDandelionBalanceValidAndOptional(t *testing.T) {
	snapshot, err := parseDandelionSnapshot(balanceTestRaw(`{"amount":31.23,"currency":"USD","unknown":"BALANCE-UNKNOWN-CANARY"}`))
	require.NoError(t, err)
	provider := snapshot.Providers[0]
	require.NotNil(t, provider.Balance)
	require.Equal(t, 31.23, provider.Balance.Amount)
	require.Equal(t, "USD", provider.Balance.Currency)
	require.Empty(t, provider.Windows)

	encoded, err := json.Marshal(provider)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "BALANCE-UNKNOWN-CANARY")
	require.NotContains(t, string(encoded), "unknown")
	require.NotContains(t, string(encoded), "used_pct")
	require.NotContains(t, string(encoded), "resets_at")
	require.NotContains(t, string(encoded), "projected_full_at")
	require.NotContains(t, string(encoded), "binding_score")
	var shape map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &shape))
	require.ElementsMatch(t, []string{
		"id", "display_name", "status", "plan_label", "eligible", "reason",
		"fetched_at", "balance", "windows", "windows_omitted",
	}, dandelionTestKeys(shape))
	var balance map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(shape["balance"], &balance))
	require.ElementsMatch(t, []string{"amount", "currency"}, dandelionTestKeys(balance))
	require.JSONEq(t, `31.23`, string(balance["amount"]))
	require.JSONEq(t, `"USD"`, string(balance["currency"]))

	for _, tc := range []struct {
		name     string
		raw      string
		amount   float64
		currency string
	}{
		{name: "zero", raw: `{"amount":0,"currency":"USD"}`, amount: 0, currency: "USD"},
		{name: "other currency", raw: `{"amount":12.5,"currency":"EUR"}`, amount: 12.5, currency: "EUR"},
		{name: "currency at cap", raw: `{"amount":1,"currency":"ABCDEFGHIJKLMNOP"}`, amount: 1, currency: "ABCDEFGHIJKLMNOP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := parseDandelionSnapshot(balanceTestRaw(tc.raw))
			require.NoError(t, err)
			require.NotNil(t, snapshot.Providers[0].Balance)
			require.Equal(t, tc.amount, snapshot.Providers[0].Balance.Amount)
			require.Equal(t, tc.currency, snapshot.Providers[0].Balance.Currency)
		})
	}
}

func TestDandelionBalanceMissingAndInvalidAreOmitted(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "missing", raw: ""},
		{name: "null", raw: "null"},
		{name: "wrong type", raw: "42"},
		{name: "empty object", raw: "{}"},
		{name: "missing amount", raw: `{"currency":"USD"}`},
		{name: "missing currency", raw: `{"amount":31.23}`},
		{name: "string amount", raw: `{"amount":"31.23","currency":"USD"}`},
		{name: "null amount", raw: `{"amount":null,"currency":"USD"}`},
		{name: "negative amount", raw: `{"amount":-0.01,"currency":"USD"}`},
		{name: "infinite amount", raw: `{"amount":1e999,"currency":"USD"}`},
		{name: "null currency", raw: `{"amount":31.23,"currency":null}`},
		{name: "non-string currency", raw: `{"amount":31.23,"currency":42}`},
		{name: "empty currency", raw: `{"amount":31.23,"currency":" "}`},
		{name: "currency over cap", raw: `{"amount":31.23,"currency":"ABCDEFGHIJKLMNOPQ"}`},
		{name: "currency markup", raw: `{"amount":31.23,"currency":"<script>BALANCE-XSS-CANARY</script>"}`},
		{name: "currency control", raw: `{"amount":31.23,"currency":"USD\nCANARY"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := parseDandelionSnapshot(balanceTestRaw(tc.raw))
			require.NoError(t, err, "an invalid optional balance must not invalidate its provider")
			require.Len(t, snapshot.Providers, 1)
			provider := snapshot.Providers[0]
			require.Equal(t, "kilo", provider.ID)
			require.Empty(t, provider.Windows)
			require.Nil(t, provider.Balance)
			encoded, err := json.Marshal(provider)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), `"balance"`)
			require.NotContains(t, string(encoded), "CANARY")
		})
	}
}

func TestDandelionLiteralDollarBalanceProjectionAndCloneIsolation(t *testing.T) {
	source, err := parseDandelionSnapshot(balanceTestRaw(`{"amount":31.232105,"currency":"$"}`))
	require.NoError(t, err)
	require.NotNil(t, source.Providers[0].Balance)
	require.Equal(t, 31.232105, source.Providers[0].Balance.Amount)
	require.Equal(t, "USD", source.Providers[0].Balance.Currency)
	require.Empty(t, cleanDandelionCurrency("$$"))

	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: observerTestRun("run-literal-dollar"), RunDir: t.TempDir(), Dandelion: true, Now: func() time.Time { return at }})
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{snapshot: source}})
	o.publish()
	want := o.Snapshot().Dandelion
	require.NotNil(t, want.Providers[0].Balance)
	require.Equal(t, 31.232105, want.Providers[0].Balance.Amount)
	require.Equal(t, "USD", want.Providers[0].Balance.Currency)
	encoded, err := json.Marshal(want.Providers[0])
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"currency":"USD"`)
	require.NotContains(t, string(encoded), `"currency":"$"`)
	require.NotContains(t, string(encoded), "used_pct")
	require.NotContains(t, string(encoded), "resets_at")
	require.NotContains(t, string(encoded), "binding_score")

	source.Providers[0].Balance.Amount = 99
	source.Providers[0].Balance.Currency = "MUTATED-SOURCE"
	require.Equal(t, want, o.Snapshot().Dandelion, "projection must not alias the raw balance")
	copyToMutate := o.Snapshot().Dandelion
	require.NotSame(t, want.Providers[0].Balance, copyToMutate.Providers[0].Balance)
	copyToMutate.Providers[0].Balance.Amount = 77
	copyToMutate.Providers[0].Balance.Currency = "MUTATED-COPY"
	require.Equal(t, want, o.Snapshot().Dandelion, "returned snapshots must not alias the cached balance")
}

func TestDandelionBalanceCloneIsolation(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: observerTestRun("run-balance-copy"), RunDir: t.TempDir(), Dandelion: true, Now: func() time.Time { return at }})
	source, err := parseDandelionSnapshot(balanceTestRaw(`{"amount":31.23,"currency":"USD"}`))
	require.NoError(t, err)
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{snapshot: source}})
	o.publish()
	want := o.Snapshot().Dandelion
	require.NotNil(t, want.Providers[0].Balance)

	source.Providers[0].Balance.Amount = 99
	source.Providers[0].Balance.Currency = "MUTATED-SOURCE"
	require.Equal(t, want, o.Snapshot().Dandelion, "collector results must not alias observer balances")

	copyToMutate := o.Snapshot().Dandelion
	require.NotSame(t, want.Providers[0].Balance, copyToMutate.Providers[0].Balance)
	copyToMutate.Providers[0].Balance.Amount = 77
	copyToMutate.Providers[0].Balance.Currency = "MUTATED-COPY"
	require.Equal(t, want, o.Snapshot().Dandelion, "returned snapshots must not alias cached balances")
}

func TestDandelionBalanceTextBoundsRemainASCII(t *testing.T) {
	for _, currency := range []string{"US$", "€", "USD/EUR", strings.Repeat("é", 4)} {
		require.Empty(t, cleanDandelionCurrency(currency), currency)
	}
	require.Equal(t, "USD", cleanDandelionCurrency(" USD "))
	require.Len(t, cleanDandelionCurrency(strings.Repeat("A", maxDandelionCurrencyText)), maxDandelionCurrencyText)
}
