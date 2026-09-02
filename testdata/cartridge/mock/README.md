# Mock cartridge scenarios

Scenario files for `internal/cartridge/mock` (P2-02). They are plain JSON,
parsed by `mock.Load` with unknown fields rejected; see the package
documentation for the schema (`Scenario`, `Rule`, `Match`, `Step`,
`Failure`, `DetectSpec`).

| File | What it scripts |
|---|---|
| `review-loop.json` | A full feedback loop: the PO plans (structured `beads` result), the engineer writes `FIX.md` and commits it (real diff in the workdir), the QA tester requests changes once (`max_uses: 1`) and every later tester run approves; unmatched firings echo their prompt. |
| `failures.json` | One rule per injectable failure, matched on the bead label: `crash`, `timeout`, `rate_limited` (with `retry_after`), `credits_exhausted`, `auth`, `killed`, `not_installed`, and `malformed` (a successful session whose `sc-result` block is not valid JSON). |
| `detect-credits.json` | `Detect` answers after a 1 s delay with `ErrCreditsExhausted` and every session fails the same way, which is what a dead cartridge looks like to the supervisor (D-18). |

Hand a file to a configured adapter through the cartridge's env table:
`[cartridges.mock] env = { SC_MOCK_SCENARIO = "/abs/path/to/scenario.json" }`.
Tests reach these files through `mockTestdata(t, name)` in
`internal/cartridge/mock/scenario_test.go`.
