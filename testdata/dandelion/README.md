# dandelion fixtures

Verbatim copies of the two dandelion source files the `dandelion` cartridge
(`internal/cartridge/dandelion`, D-30) mirrors, taken from the dandelion
repository at commit `bfede3d` (2026-10-27):

| File | Copied from | What the cartridge takes from it |
|---|---|---|
| `launch.ts` | `src/app/launch.ts` | `TARGETS` (every account a route line can name) and `ACCOUNT_HOMES` (the `CLAUDE_CONFIG_DIR` each Claude account launches with, its override variable and its fallback under `$HOME`) |
| `probes-index.ts` | `src/probes/index.ts` | `rawProbes` (every provider id `DANDELION_DISABLE` can name) |

`TestProviders_MatchPinnedDandelionSources` parses both files and fails when
an account or provider id is not in `dandelion.Providers`, or when a Claude
account's config dir differs from the table. To refresh after dandelion
gains an account:

```sh
cp ~/workspace/dandelion/src/app/launch.ts testdata/dandelion/launch.ts
cp ~/workspace/dandelion/src/probes/index.ts testdata/dandelion/probes-index.ts
go test ./internal/cartridge/dandelion/ -run TestProviders
```

then classify every id the test names in `dandelion.Providers` and update the
commit above. `SC_DANDELION_SRC=~/workspace/dandelion go test -run
TestProviders ./internal/cartridge/dandelion/` runs the same check against a
live checkout instead of these copies.
