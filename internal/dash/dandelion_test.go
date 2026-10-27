package dash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
)

const (
	dandelionTestGenerated = "2026-10-24T12:00:00.123456789Z"
	dandelionTestFetched   = "2026-10-24T11:59:00Z"
	dandelionTestEmptyJSON = `{"generatedAt":"2026-10-24T12:00:00Z","providers":[],"route":null,"routeHigh":null}`
)

func dandelionTestWithProviders(providers string) string {
	return `{"generatedAt":"` + dandelionTestGenerated + `","providers":[` + providers + `],"route":"model-a high alpha","routeHigh":"model-b max beta"}`
}

func dandelionTestWithProvider(provider string) string {
	return dandelionTestWithProviders(provider)
}

func dandelionTestWithWindow(window string) string {
	return dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":[` + window + `]}`)
}

func dandelionTestSnapshot(t *testing.T) *DandelionSnapshot {
	t.Helper()
	raw := `{
		"generatedAt":"2026-10-24T12:00:00.123456789+00:00",
		"route":"model-a high alpha",
		"routeHigh":"model-b max beta",
		"routesError":"ROUTES-ERROR-CANARY",
		"unknownTop":"UNKNOWN-TOP-CANARY",
		"providers":[{
			"id":"alpha",
			"displayName":"Alpha account",
			"status":"ok",
			"planLabel":"Pro",
			"fetchedAt":"2026-10-24T11:59:00Z",
			"note":"NOTE-CANARY",
			"balance":{"amount":12,"currency":"<script>BALANCE-CANARY</script>"},
			"unknownProvider":"UNKNOWN-PROVIDER-CANARY",
			"windows":[{
				"label":"Five hour",
				"kind":"rolling",
				"usedPct":49.5,
				"resetsAt":"2026-10-24T13:00:00Z",
				"projectedFullAt":"2026-10-24T12:30:00Z",
				"unknownWindow":"UNKNOWN-WINDOW-CANARY"
			},{
				"label":"Weekly",
				"kind":"weekly",
				"usedPct":0,
				"notes":"NOTES-CANARY"
			}]
		},{
			"id":"beta",
			"displayName":"Beta account",
			"status":"error",
			"eligible":false,
			"reason":"cooling down",
			"fetchedAt":"2026-10-24T11:58:00Z",
			"windows":[]
		}]
	}`
	snapshot, err := parseDandelionSnapshot(raw)
	require.NoError(t, err)
	return snapshot
}

func dandelionTestKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestParseDandelionSnapshotProjectsOnlyAllowlistedData(t *testing.T) {
	snapshot := dandelionTestSnapshot(t)
	require.Equal(t, time.Date(2026, 10, 24, 12, 0, 0, 123456789, time.UTC), *snapshot.GeneratedAt)
	require.Equal(t, "model-a high alpha", snapshot.Route)
	require.Equal(t, "model-b max beta", snapshot.RouteHigh)
	require.Len(t, snapshot.Providers, 2)
	require.Zero(t, snapshot.ProvidersOmitted)

	alpha := snapshot.Providers[0]
	require.Equal(t, "alpha", alpha.ID)
	require.Equal(t, "Alpha account", alpha.DisplayName)
	require.Equal(t, "ok", alpha.Status)
	require.Equal(t, "Pro", alpha.PlanLabel)
	require.True(t, alpha.Eligible, "a missing eligible field defaults to true")
	require.Empty(t, alpha.Reason)
	require.Equal(t, time.Date(2026, 10, 24, 11, 59, 0, 0, time.UTC), alpha.FetchedAt)
	require.Len(t, alpha.Windows, 2)
	require.Zero(t, alpha.WindowsOmitted)
	require.Equal(t, DandelionWindowSnapshot{
		Label: "Five hour", Kind: "rolling", UsedPct: 49.5,
		ResetsAt:        dandelionTestTimePointer(time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)),
		ProjectedFullAt: dandelionTestTimePointer(time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)),
	}, alpha.Windows[0])
	require.Equal(t, "Weekly", alpha.Windows[1].Label)
	require.Zero(t, alpha.Windows[1].UsedPct)
	require.Nil(t, alpha.Windows[1].ResetsAt)
	require.Nil(t, alpha.Windows[1].ProjectedFullAt)

	beta := snapshot.Providers[1]
	require.False(t, beta.Eligible)
	require.Equal(t, "error", beta.Status)
	require.Equal(t, "cooling down", beta.Reason)
	require.Empty(t, beta.PlanLabel)
	require.NotNil(t, beta.Windows)
	require.Empty(t, beta.Windows)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, canary := range []string{
		"ROUTES-ERROR-CANARY", "UNKNOWN-TOP-CANARY", "NOTE-CANARY", "BALANCE-CANARY",
		"UNKNOWN-PROVIDER-CANARY", "UNKNOWN-WINDOW-CANARY", "NOTES-CANARY",
	} {
		require.NotContains(t, string(encoded), canary)
	}
	for _, key := range []string{
		`"routesError"`, `"note"`, `"notes"`, `"balance"`, `"balances"`, `"stderr"`,
		`"unknownTop"`, `"unknownProvider"`, `"unknownWindow"`, `"raw_response"`,
	} {
		require.NotContains(t, string(encoded), key)
	}
}

func dandelionTestTimePointer(value time.Time) *time.Time {
	return &value
}

func TestParseDandelionSnapshotRejectsMalformedData(t *testing.T) {
	validProvider := `{"id":"alpha","displayName":"Alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "not JSON", raw: "not-json"},
		{name: "null root", raw: "null"},
		{name: "trailing value", raw: dandelionTestEmptyJSON + `{}`},
		{name: "trailing garbage", raw: dandelionTestEmptyJSON + `garbage`},
		{name: "missing generated_at", raw: `{"providers":[],"route":null,"routeHigh":null}`},
		{name: "null generated_at", raw: `{"generatedAt":null,"providers":[],"route":null,"routeHigh":null}`},
		{name: "invalid generated_at", raw: `{"generatedAt":"yesterday","providers":[],"route":null,"routeHigh":null}`},
		{name: "missing providers", raw: `{"generatedAt":"2026-10-24T12:00:00Z","route":null,"routeHigh":null}`},
		{name: "null providers", raw: `{"generatedAt":"2026-10-24T12:00:00Z","providers":null,"route":null,"routeHigh":null}`},
		{name: "missing route", raw: `{"generatedAt":"2026-10-24T12:00:00Z","providers":[],"routeHigh":null}`},
		{name: "missing high route", raw: `{"generatedAt":"2026-10-24T12:00:00Z","providers":[],"route":null}`},
		{name: "invalid route", raw: `{"generatedAt":"2026-10-24T12:00:00Z","providers":[],"route":{},"routeHigh":null}`},
		{name: "null provider", raw: dandelionTestWithProvider(`null`)},
		{name: "missing provider id", raw: dandelionTestWithProvider(`{"displayName":"Alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "missing display name", raw: dandelionTestWithProvider(`{"id":"alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "missing fetched_at", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","windows":[]}`)},
		{name: "invalid fetched_at", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","fetchedAt":"never","windows":[]}`)},
		{name: "missing windows", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `"}`)},
		{name: "null windows", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","fetchedAt":"` + dandelionTestFetched + `","windows":null}`)},
		{name: "unknown status", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"limited","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "missing failure reason", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"error","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "null eligible", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","eligible":null,"fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "non-boolean eligible", raw: dandelionTestWithProvider(`{"id":"alpha","displayName":"Alpha","status":"ok","eligible":"yes","fetchedAt":"` + dandelionTestFetched + `","windows":[]}`)},
		{name: "null window", raw: dandelionTestWithWindow(`null`)},
		{name: "missing window label", raw: dandelionTestWithWindow(`{"kind":"rolling","usedPct":1}`)},
		{name: "missing window kind", raw: dandelionTestWithWindow(`{"label":"Five hour","usedPct":1}`)},
		{name: "unknown window kind", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"session","usedPct":1}`)},
		{name: "missing used_pct", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling"}`)},
		{name: "null used_pct", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":null}`)},
		{name: "string used_pct", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":"50"}`)},
		{name: "non-finite used_pct", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":1e999}`)},
		{name: "invalid reset", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":1,"resetsAt":"soon"}`)},
		{name: "non-string reset", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":1,"resetsAt":false}`)},
		{name: "invalid projection", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":1,"projectedFullAt":"soon"}`)},
		{name: "non-string projection", raw: dandelionTestWithWindow(`{"label":"Five hour","kind":"rolling","usedPct":1,"projectedFullAt":42}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := parseDandelionSnapshot(tc.raw)
			require.ErrorIs(t, err, errDandelionInvalidSnapshot)
			require.Nil(t, snapshot)
		})
	}

	snapshot, err := parseDandelionSnapshot(dandelionTestWithWindow(`{"label":"Other","kind":"other","usedPct":0,"resetsAt":null,"projectedFullAt":null}`))
	require.NoError(t, err)
	require.Nil(t, snapshot.Providers[0].Windows[0].ResetsAt)
	require.Nil(t, snapshot.Providers[0].Windows[0].ProjectedFullAt)
	snapshot, err = parseDandelionSnapshot(dandelionTestWithProvider(validProvider))
	require.NoError(t, err)
	require.True(t, snapshot.Providers[0].Eligible)
}

func TestRunDandelionProbeUsesFixedBoundedInvocation(t *testing.T) {
	var captured proc.Spec
	result := runDandelionProbe(context.Background(), DefaultDandelionRefresh, func(_ context.Context, spec proc.Spec) (proc.Result, error) {
		captured = spec
		return proc.Result{Stdout: dandelionTestEmptyJSON, Stderr: "RAW-STDERR-CANARY"}, nil
	})
	require.Empty(t, result.error)
	require.NotNil(t, result.snapshot)
	require.Equal(t, dandelionCommand, captured.Command)
	require.Equal(t, []string{"--json", "--max-age", "600"}, captured.Args)
	require.Equal(t, 5*time.Minute, captured.Timeout)
	require.Equal(t, dandelionProbeTimeout, captured.Timeout)
	require.True(t, captured.ReapDescendants)
	require.Equal(t, dandelionMaxStdoutBytes+1, captured.StdoutTail)
	require.Equal(t, dandelionMaxStderrBytes, captured.StderrTail)
	encoded, err := json.Marshal(result.snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "RAW-STDERR-CANARY")
}

func TestRunDandelionProbeAcceptsStdoutAtCap(t *testing.T) {
	output := dandelionTestEmptyJSON + strings.Repeat(" ", dandelionMaxStdoutBytes-len(dandelionTestEmptyJSON))
	require.Len(t, output, dandelionMaxStdoutBytes)
	result := runDandelionProbe(context.Background(), time.Minute, func(context.Context, proc.Spec) (proc.Result, error) {
		return proc.Result{Stdout: output}, nil
	})
	require.Empty(t, result.error)
	require.NotNil(t, result.snapshot)
}

func TestRunDandelionProbeMapsFailuresToFixedSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result proc.Result
		err    error
		want   string
	}{
		{name: "not installed", err: fmt.Errorf("RAW-ERROR-CANARY: %w", exec.ErrNotFound), want: dandelionErrorNotInstalled},
		{name: "timeout result", result: proc.Result{TimedOut: true}, want: dandelionErrorTimedOut},
		{name: "timeout error", err: fmt.Errorf("RAW-ERROR-CANARY: %w", proc.ErrTimeout), want: dandelionErrorTimedOut},
		{name: "probe failure", result: proc.Result{Stderr: "RAW-STDERR-CANARY"}, err: errors.New("RAW-ERROR-CANARY"), want: dandelionErrorProbeFailed},
		{name: "invalid JSON", result: proc.Result{Stdout: "RAW-STDOUT-CANARY"}, want: dandelionErrorInvalidSnapshot},
		{name: "stdout cap", result: proc.Result{Stdout: dandelionTestEmptyJSON + strings.Repeat(" ", dandelionMaxStdoutBytes)}, want: dandelionErrorInvalidSnapshot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runDandelionProbe(context.Background(), time.Minute, func(_ context.Context, spec proc.Spec) (proc.Result, error) {
				require.Equal(t, []string{"--json", "--max-age", "60"}, spec.Args)
				return tc.result, tc.err
			})
			require.Equal(t, tc.want, result.error)
			require.Nil(t, result.snapshot)

			o := NewObserver(Options{Run: observerTestRun("run-safe-error"), RunDir: t.TempDir(), Dandelion: true})
			o.applyDandelionEvent(dandelionEvent{started: true, at: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)})
			o.applyDandelionEvent(dandelionEvent{at: time.Date(2026, 10, 24, 12, 0, 1, 0, time.UTC), result: result})
			o.publish()
			encoded, err := json.Marshal(o.Snapshot())
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "RAW-STDOUT-CANARY")
			require.NotContains(t, string(encoded), "RAW-STDERR-CANARY")
			require.NotContains(t, string(encoded), "RAW-ERROR-CANARY")
			require.Equal(t, tc.want, o.Snapshot().Dandelion.Error)
		})
	}
}

func TestDandelionCollectionAndResponseBounds(t *testing.T) {
	providers := make([]any, maxDandelionProviders+8)
	for i := range providers {
		windows := make([]any, maxDandelionWindows+4)
		for j := range windows {
			windows[j] = map[string]any{
				"label": "\x1b[31m" + strings.Repeat("l", maxTitleText+100) + "\n",
				"kind":  "rolling", "usedPct": j,
			}
		}
		providers[i] = map[string]any{
			"id": strings.Repeat("i", maxIDText+100), "displayName": strings.Repeat("d", maxTitleText+100),
			"status": "error", "planLabel": strings.Repeat("p", maxTitleText+100),
			"reason": strings.Repeat("r", maxActivityText+100), "fetchedAt": dandelionTestFetched,
			"windows": windows,
		}
	}
	raw, err := json.Marshal(map[string]any{
		"generatedAt": dandelionTestGenerated, "providers": providers,
		"route": strings.Repeat("r", maxActivityText+100), "routeHigh": strings.Repeat("h", maxActivityText+100),
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), dandelionMaxStdoutBytes)
	snapshot, err := parseDandelionSnapshot(string(raw))
	require.NoError(t, err)
	require.Len(t, snapshot.Providers, maxDandelionProviders)
	require.Equal(t, 8, snapshot.ProvidersOmitted)
	require.LessOrEqual(t, utf8.RuneCountInString(snapshot.Route), maxActivityText)
	require.LessOrEqual(t, utf8.RuneCountInString(snapshot.RouteHigh), maxActivityText)
	for _, provider := range snapshot.Providers {
		require.LessOrEqual(t, utf8.RuneCountInString(provider.ID), maxIDText)
		require.LessOrEqual(t, utf8.RuneCountInString(provider.DisplayName), maxTitleText)
		require.LessOrEqual(t, utf8.RuneCountInString(provider.PlanLabel), maxTitleText)
		require.LessOrEqual(t, utf8.RuneCountInString(provider.Reason), maxActivityText)
		require.Len(t, provider.Windows, maxDandelionWindows)
		require.Equal(t, 4, provider.WindowsOmitted)
		for _, window := range provider.Windows {
			require.LessOrEqual(t, utf8.RuneCountInString(window.Label), maxTitleText)
			require.NotContains(t, window.Label, "\x1b")
			require.NotContains(t, window.Label, "\n")
		}
	}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), maxDandelionResponseBytes-dandelionResponseHeadroom)
}

func TestDandelionResponseCapRejectsEscapingExpansion(t *testing.T) {
	providers := make([]any, maxDandelionProviders)
	for i := range providers {
		windows := make([]any, maxDandelionWindows)
		for j := range windows {
			windows[j] = map[string]any{
				"label": strings.Repeat("<", maxTitleText), "kind": "other", "usedPct": 0,
			}
		}
		providers[i] = map[string]any{
			"id": fmt.Sprintf("provider-%d", i), "displayName": "Provider", "status": "ok",
			"fetchedAt": dandelionTestFetched, "windows": windows,
		}
	}
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	require.NoError(t, encoder.Encode(map[string]any{
		"generatedAt": dandelionTestGenerated, "providers": providers, "route": nil, "routeHigh": nil,
	}))
	require.LessOrEqual(t, raw.Len(), dandelionMaxStdoutBytes)
	snapshot, err := parseDandelionSnapshot(raw.String())
	require.ErrorIs(t, err, errDandelionInvalidSnapshot)
	require.Nil(t, snapshot)
}

func TestValidateDandelionRefresh(t *testing.T) {
	require.NoError(t, ValidateDandelionRefresh(MinDandelionRefresh))
	require.NoError(t, ValidateDandelionRefresh(DefaultDandelionRefresh))
	for _, refresh := range []time.Duration{0, -time.Second, time.Millisecond, MinDandelionRefresh - time.Nanosecond} {
		require.Error(t, ValidateDandelionRefresh(refresh))
	}
}

func TestDandelionDisabledAndInitialJSONShape(t *testing.T) {
	run := observerTestRun("run-dandelion-shape")
	disabled := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	require.Nil(t, disabled.Snapshot().Dandelion)
	encoded, err := json.Marshal(disabled.Snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"dandelion"`)

	enabled := NewObserver(Options{Run: run, RunDir: t.TempDir(), Dandelion: true})
	initial := enabled.Snapshot().Dandelion
	require.NotNil(t, initial)
	require.True(t, initial.Stale)
	require.False(t, initial.Probing)
	require.Nil(t, initial.GeneratedAt)
	require.NotNil(t, initial.Providers)
	require.Empty(t, initial.Providers)
	encoded, err = json.Marshal(enabled.Snapshot())
	require.NoError(t, err)
	var snapshot map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &snapshot))
	var quota map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(snapshot["dandelion"], &quota))
	require.ElementsMatch(t, []string{
		"probing", "stale", "generated_at", "route", "route_high", "providers", "providers_omitted",
	}, dandelionTestKeys(quota))
	require.JSONEq(t, "null", string(quota["generated_at"]))
	require.JSONEq(t, "[]", string(quota["providers"]))
}

func TestDandelionHTTPSnapshotHasFixedAllowlistedShape(t *testing.T) {
	run := observerTestRun("run-dandelion-http-shape")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Dandelion: true, Now: func() time.Time { return at }})
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{snapshot: dandelionTestSnapshot(t)}})
	o.publish()
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	response := serverTestRequest(t, handler, http.MethodGet, "/api/snapshot")
	require.Equal(t, http.StatusOK, response.Code)
	body := response.Body.String()
	for _, canary := range []string{
		"ROUTES-ERROR-CANARY", "UNKNOWN-TOP-CANARY", "NOTE-CANARY", "BALANCE-CANARY",
		"UNKNOWN-PROVIDER-CANARY", "UNKNOWN-WINDOW-CANARY", "NOTES-CANARY",
	} {
		require.NotContains(t, body, canary)
	}

	var snapshot map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
	var quota map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(snapshot["dandelion"], &quota))
	require.ElementsMatch(t, []string{
		"probing", "last_attempt_at", "last_success_at", "stale", "generated_at",
		"route", "route_high", "providers", "providers_omitted",
	}, dandelionTestKeys(quota))
	var providers []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(quota["providers"], &providers))
	require.Len(t, providers, 2)
	for _, provider := range providers {
		require.ElementsMatch(t, []string{
			"id", "display_name", "status", "plan_label", "eligible", "reason",
			"fetched_at", "windows", "windows_omitted",
		}, dandelionTestKeys(provider))
	}
	var windows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(providers[0]["windows"], &windows))
	require.Len(t, windows, 2)
	for _, window := range windows {
		require.ElementsMatch(t, []string{"label", "kind", "used_pct", "resets_at", "projected_full_at"}, dandelionTestKeys(window))
	}
	require.JSONEq(t, "null", string(windows[1]["resets_at"]))
	require.JSONEq(t, "null", string(windows[1]["projected_full_at"]))
	require.JSONEq(t, `""`, string(providers[0]["reason"]))
}

func TestDandelionLastGoodLifecycleAndFreshness(t *testing.T) {
	run := observerTestRun("run-dandelion-lifecycle")
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Dandelion: true, DandelionRefresh: time.Minute})
	source := dandelionTestSnapshot(t)

	o.applyDandelionEvent(dandelionEvent{started: true, at: start})
	quota := o.projectDandelion(start)
	require.True(t, quota.Probing)
	require.Equal(t, start, *quota.LastAttemptAt)
	require.Nil(t, quota.LastSuccessAt)
	require.True(t, quota.Stale)

	firstSuccess := start.Add(5 * time.Second)
	o.applyDandelionEvent(dandelionEvent{at: firstSuccess, result: dandelionProbeResult{snapshot: source}})
	quota = o.projectDandelion(firstSuccess)
	require.False(t, quota.Probing)
	require.Equal(t, start, *quota.LastAttemptAt)
	require.Equal(t, firstSuccess, *quota.LastSuccessAt)
	require.False(t, quota.Stale)
	require.Empty(t, quota.Error)
	require.Equal(t, source.GeneratedAt, quota.GeneratedAt)
	require.False(t, o.projectDandelion(firstSuccess.Add(time.Minute-time.Nanosecond)).Stale)
	require.True(t, o.projectDandelion(firstSuccess.Add(time.Minute)).Stale)

	secondAttempt := start.Add(70 * time.Second)
	o.applyDandelionEvent(dandelionEvent{started: true, at: secondAttempt})
	o.applyDandelionEvent(dandelionEvent{at: secondAttempt.Add(5 * time.Second), result: dandelionProbeResult{error: dandelionErrorInvalidSnapshot}})
	quota = o.projectDandelion(secondAttempt.Add(5 * time.Second))
	require.Equal(t, secondAttempt, *quota.LastAttemptAt)
	require.Equal(t, firstSuccess, *quota.LastSuccessAt, "a failed attempt must not refresh last-good freshness")
	require.Equal(t, dandelionErrorInvalidSnapshot, quota.Error)
	require.True(t, quota.Stale)
	require.Equal(t, source.Route, quota.Route)
	require.Equal(t, source.Providers, quota.Providers)

	thirdAttempt := start.Add(80 * time.Second)
	thirdSuccess := start.Add(85 * time.Second)
	o.applyDandelionEvent(dandelionEvent{started: true, at: thirdAttempt})
	o.applyDandelionEvent(dandelionEvent{at: thirdSuccess, result: dandelionProbeResult{snapshot: source}})
	quota = o.projectDandelion(thirdSuccess)
	require.Equal(t, thirdAttempt, *quota.LastAttemptAt)
	require.Equal(t, thirdSuccess, *quota.LastSuccessAt)
	require.Empty(t, quota.Error)
	require.False(t, quota.Stale)

	o.applyDandelionEvent(dandelionEvent{started: true, at: start.Add(90 * time.Second)})
	o.applyDandelionEvent(dandelionEvent{at: start.Add(91 * time.Second), result: dandelionProbeResult{error: "RAW-UNTRUSTED-ERROR-CANARY"}})
	quota = o.projectDandelion(start.Add(91 * time.Second))
	require.Equal(t, dandelionErrorProbeFailed, quota.Error)
	require.Equal(t, thirdSuccess, *quota.LastSuccessAt)
}

func TestDandelionSnapshotsAreDeepCopied(t *testing.T) {
	run := observerTestRun("run-dandelion-copies")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Dandelion: true, Now: func() time.Time { return at }})
	source := dandelionTestSnapshot(t)
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{snapshot: source}})
	o.publish()
	want := o.Snapshot().Dandelion

	*source.GeneratedAt = at.Add(24 * time.Hour)
	source.Route = "MUTATED-SOURCE"
	source.Providers[0].DisplayName = "MUTATED-SOURCE"
	*source.Providers[0].Windows[0].ResetsAt = at.Add(48 * time.Hour)
	source.Providers[0].Windows[0].Label = "MUTATED-SOURCE"
	require.Equal(t, want, o.Snapshot().Dandelion, "collector results must not alias observer state")

	copyToMutate := o.Snapshot().Dandelion
	*copyToMutate.LastAttemptAt = at.Add(72 * time.Hour)
	*copyToMutate.LastSuccessAt = at.Add(72 * time.Hour)
	*copyToMutate.GeneratedAt = at.Add(72 * time.Hour)
	copyToMutate.Route = "MUTATED-COPY"
	copyToMutate.Providers[0].DisplayName = "MUTATED-COPY"
	copyToMutate.Providers[0].Windows[0].Label = "MUTATED-COPY"
	*copyToMutate.Providers[0].Windows[0].ResetsAt = at.Add(96 * time.Hour)
	*copyToMutate.Providers[0].Windows[0].ProjectedFullAt = at.Add(96 * time.Hour)
	copyToMutate.Providers[0].Windows[1].ResetsAt = dandelionTestTimePointer(at.Add(96 * time.Hour))
	copyToMutate.Providers[1].Windows = append(copyToMutate.Providers[1].Windows, DandelionWindowSnapshot{Label: "MUTATED-COPY"})
	require.Equal(t, want, o.Snapshot().Dandelion, "HTTP and caller snapshots must not alias the cache")
}

func TestDandelionLastGoodIsMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-dandelion-memory")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: run, RunDir: dir, Dandelion: true, Now: func() time.Time { return at }})
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{snapshot: dandelionTestSnapshot(t)}})
	o.publish()
	require.NotNil(t, o.Snapshot().Dandelion.LastSuccessAt)

	restarted := NewObserver(Options{Run: run, RunDir: dir, Dandelion: true, Now: func() time.Time { return at }})
	quota := restarted.Snapshot().Dandelion
	require.Nil(t, quota.LastAttemptAt)
	require.Nil(t, quota.LastSuccessAt)
	require.Nil(t, quota.GeneratedAt)
	require.Empty(t, quota.Providers)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "observer last-good data must not create a disk cache")
}

func TestObserverDandelionCollectionIsImmediateAndSequential(t *testing.T) {
	run := observerTestRun("run-dandelion-sequential")
	const refresh = 2 * time.Millisecond
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour, FollowPoll: observerTestPoll,
		Dandelion: true, DandelionRefresh: refresh,
	})
	source := dandelionTestSnapshot(t)
	var calls, active, maxActive atomic.Int32
	var wrongRefresh atomic.Bool
	o.probeDandelion = func(ctx context.Context, gotRefresh time.Duration) dandelionProbeResult {
		if gotRefresh != refresh {
			wrongRefresh.Store(true)
		}
		calls.Add(1)
		concurrent := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maxActive.Load()
			if concurrent <= previous || maxActive.CompareAndSwap(previous, concurrent) {
				break
			}
		}
		timer := time.NewTimer(3 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return dandelionProbeResult{error: dandelionErrorProbeFailed}
		case <-timer.C:
			return dandelionProbeResult{snapshot: source}
		}
	}
	observerTestStart(t, o)
	quota := observerTestWaitFor(t, o, func(snapshot Snapshot) bool {
		return calls.Load() >= 3 && snapshot.Dandelion != nil && snapshot.Dandelion.LastSuccessAt != nil
	}).Dandelion
	require.NotNil(t, quota.LastAttemptAt)
	require.Equal(t, int32(1), maxActive.Load())
	require.False(t, wrongRefresh.Load())
	require.Equal(t, "ok", o.Health().Status)
}

func TestObserverDandelionCancellationIsAwaitedAndHTTPNeverProbes(t *testing.T) {
	run := observerTestRun("run-dandelion-cancel")
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour, FollowPoll: observerTestPoll,
		Dandelion: true, DandelionRefresh: time.Hour,
	})
	var calls atomic.Int32
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, cancelOnce, releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	o.probeDandelion = func(ctx context.Context, _ time.Duration) dandelionProbeResult {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		cancelOnce.Do(func() { close(cancelObserved) })
		<-release
		return dandelionProbeResult{error: dandelionErrorProbeFailed}
	}
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		releaseProbe()
		select {
		case <-done:
		case <-time.After(observerTestWait):
			t.Error("observer did not stop after releasing its Dandelion probe")
		}
	})
	select {
	case <-started:
	case <-time.After(observerTestWait):
		t.Fatal("first Dandelion probe was not immediate")
	}
	for range 10 {
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/api/snapshot?stale=true").Code)
		require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/health").Code)
	}
	require.Equal(t, http.StatusOK, serverTestRequest(t, handler, http.MethodGet, "/").Code)
	require.Equal(t, int32(1), calls.Load(), "HTTP requests must not start or overlap probes")

	cancel()
	select {
	case <-cancelObserved:
	case <-time.After(observerTestWait):
		t.Fatal("Dandelion probe did not receive cancellation")
	}
	select {
	case <-done:
		t.Fatal("observer returned before its Dandelion probe finished")
	case <-time.After(20 * time.Millisecond):
	}
	releaseProbe()
	select {
	case <-done:
	case <-time.After(observerTestWait):
		t.Fatal("observer did not finish after its Dandelion probe returned")
	}
	require.False(t, o.Snapshot().Dandelion.Probing)
	require.Equal(t, int32(1), calls.Load())
}

func TestObserverDandelionDisabledNeverProbes(t *testing.T) {
	run := observerTestRun("run-dandelion-disabled")
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), RefreshInterval: time.Hour, FollowPoll: observerTestPoll,
		DandelionRefresh: time.Millisecond,
	})
	var calls atomic.Int32
	o.probeDandelion = func(context.Context, time.Duration) dandelionProbeResult {
		calls.Add(1)
		return dandelionProbeResult{error: dandelionErrorProbeFailed}
	}
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(snapshot Snapshot) bool { return snapshot.Observer.Stream != "starting" })
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)
	for range 10 {
		response := serverTestRequest(t, handler, http.MethodGet, "/api/snapshot")
		require.Equal(t, http.StatusOK, response.Code)
		require.NotContains(t, response.Body.String(), `"dandelion"`)
	}
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, calls.Load())
	require.Nil(t, o.Snapshot().Dandelion)
}

func TestDandelionFailureDoesNotChangeObserverHealth(t *testing.T) {
	run := observerTestRun("run-dandelion-health")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Dandelion: true, Now: func() time.Time { return at }})
	o.applyDandelionEvent(dandelionEvent{started: true, at: at})
	o.applyDandelionEvent(dandelionEvent{at: at.Add(time.Second), result: dandelionProbeResult{error: "RAW-ERROR-CANARY"}})
	o.publish()
	handler, err := NewHandler(o, "/")
	require.NoError(t, err)

	healthResponse := serverTestRequest(t, handler, http.MethodGet, "/health")
	require.Equal(t, http.StatusOK, healthResponse.Code)
	var health Health
	require.NoError(t, json.Unmarshal(healthResponse.Body.Bytes(), &health))
	require.Equal(t, "ok", health.Status)
	require.NotContains(t, healthResponse.Body.String(), "dandelion")
	require.NotContains(t, healthResponse.Body.String(), "RAW-ERROR-CANARY")
	require.NotContains(t, healthResponse.Body.String(), `"error"`)

	snapshotResponse := serverTestRequest(t, handler, http.MethodGet, "/api/snapshot")
	require.Equal(t, http.StatusOK, snapshotResponse.Code)
	var snapshot Snapshot
	require.NoError(t, json.Unmarshal(snapshotResponse.Body.Bytes(), &snapshot))
	require.Equal(t, "ok", snapshot.Observer.Status)
	require.Equal(t, dandelionErrorProbeFailed, snapshot.Dandelion.Error)
	require.True(t, snapshot.Dandelion.Stale)
	require.NotContains(t, snapshotResponse.Body.String(), "RAW-ERROR-CANARY")
	require.NotContains(t, snapshotResponse.Body.String(), `"stderr"`)
}
