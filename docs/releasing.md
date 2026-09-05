# Releasing sc

What a version number of `sc` promises, and the steps that turn a green
`main` into a published release. The pipeline itself — goreleaser, the
archives, `install.sh`, the Homebrew cask — is described in
[`.goreleaser.yaml`](../.goreleaser.yaml) and
[`.github/workflows/release.yml`](../.github/workflows/release.yml); this page
is the policy around it.

- [Versioning policy](#versioning-policy)
- [What the version number covers](#what-the-version-number-covers)
- [What counts as a breaking change](#what-counts-as-a-breaking-change)
- [The other version numbers](#the-other-version-numbers)
- [Deprecating something](#deprecating-something)
- [Cutting a release](#cutting-a-release)
- [Pre-releases and patch releases](#pre-releases-and-patch-releases)
- [If a release goes wrong](#if-a-release-goes-wrong)

## Versioning policy

`sc` follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).
Tags are `vMAJOR.MINOR.PATCH`, annotated, on `main`.

**While the major version is 0** the API is not stable and semver says so, but
the project does not use that as licence to break things silently:

| Change | Bumps |
|---|---|
| A breaking change to anything in [the covered surface](#what-the-version-number-covers) | **minor** (`0.1.0` → `0.2.0`), and it is listed under `Changed` or `Removed` in the changelog |
| A new command, flag, config key, event or view | **minor** |
| A fix, a performance change, a docs change, an internal refactor | **patch** (`0.1.0` → `0.1.1`) |

At `1.0.0` the table becomes the ordinary semver one: breaking changes take a
**major** bump, features a minor, fixes a patch. `1.0.0` is cut when the
covered surface has stopped moving between releases and someone other than the
authors has run `sc` at a repository they care about — not on a date.

Every release has an entry in [`CHANGELOG.md`](../CHANGELOG.md), in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) form. Landing a
user-visible change means adding a line to its `Unreleased` section in the same
change; the release turns that section into the version's entry.

## What the version number covers

The promise is about what a user or a script touches, not about the Go code.

**Covered — a breaking change here bumps the version as above:**

| Surface | Where it is documented |
|---|---|
| Commands, subcommands, flags and their meanings | [cli.md](cli.md) |
| The exit codes of `sc run` and `sc doctor` | [prompts.md § Exit codes](prompts.md#exit-codes) |
| Configuration file names, precedence, keys, value types and defaults | [config.md](config.md) |
| `--json` output shapes and field names (`sc version`, `sc status`, `sc config show`, `sc run --json`) | [cli.md](cli.md) |
| The `events.jsonl` event names and field names | [prompts.md](prompts.md) |
| The `sc_` bead metadata keys, `sc:` labels, custom bd statuses and the structured bead-comment types | [beads-conventions.md](beads-conventions.md) |
| The JSON Schemas an agent reply must satisfy | [schemas/](schemas/README.md) |
| The state directory layout under `$XDG_STATE_HOME/slop-cannon/`, and the environment variables that move it | [prompts.md § Environment variables](prompts.md#environment-variables) |
| The release archive names, `checksums.txt` and the `install.sh` flags and variables | [`.goreleaser.yaml`](../.goreleaser.yaml), [`install.sh`](../install.sh) |

**Not covered — these change in any release, including a patch:**

- Everything under `internal/`. The module has no public Go packages; import it
  at your own risk. The module path is `slop-cannon`, which no proxy can
  resolve, so this is not an accident waiting to be depended on.
- Prompt text and prompt layering. What an agent is told is tuning, not API.
- The TUI: layout, colours, glyphs, animation, golden frames, and the wording
  of the headless log lines. The *event stream* behind them is covered; the
  rendering of it is not.
- Log file contents and wording, and the contents of the per-agent debug copies
  in a run directory.
- Anything the docs mark as experimental or as belonging to an unreleased task.

## What counts as a breaking change

Concretely, in the covered surface:

- Removing or renaming a command, subcommand or flag, or narrowing what a flag
  accepts.
- Changing what a command does by default, including changing a default value
  in [config.md](config.md).
- Changing an exit code, or the meaning of one.
- Removing or renaming a config key, or refusing a value that used to load.
- Renaming or removing a field in a `--json` payload or in an `events.jsonl`
  event, or changing its type. *Adding* a field is not breaking; consumers must
  ignore fields they do not know.
- Raising the minimum version of an external tool (`bd`, `git`, `gh`) or
  dropping an OS or architecture from the release matrix.
- Bumping a schema version in a way that makes an older `sc` refuse a file a
  newer one wrote (see below) — the refusal is deliberate, but it is still a
  break for the user who downgrades.

Fixing a bug so a command stops doing something it documented as wrong is a
patch, not a break; say what changed in the changelog under `Fixed`.

## The other version numbers

Three schema versions travel with the binary and are bumped on their own rules.
They are not the release version and they never appear in a tag.

| Number | Where | Rule |
|---|---|---|
| `schema_version` in the config files | `internal/config`, [config.md](config.md) | Bump when the file layout changes, and add the migration step that upgrades the previous version in memory. A file from the future is refused with "upgrade sc". Currently `1`. |
| `version` in a run's `state.json` | `orchestrator.StateFileVersion` | Bump when the resume state changes shape. A state file from the future is refused rather than half-read; a resume of an older one must keep working, or the release notes must say resumes do not cross it. Currently `1`. |
| The agent message schemas | [schemas/](schemas/README.md) | Adding an optional field is free. Requiring a new field, or changing a type, is a breaking change to the covered surface. |

The minimum `bd` version (`1.2.2`) lives in `internal/beads` and is enforced by
the version gate and by `sc doctor`. Raising it is a breaking change; say so in
the changelog and in [troubleshooting.md](troubleshooting.md).

## Deprecating something

1. Keep it working. Add the replacement in the same release.
2. Make it say so once per invocation: a `warning: …` line on stderr, in the
   shape the config and cartridge commands already print, naming the
   replacement. Never a failure.
3. List it under `Deprecated` in the changelog, with the release that will
   remove it.
4. Remove it in the next **minor** while the major is 0, and in the next
   **major** after 1.0.0 — never in a patch, and never in the release that
   deprecated it.

## Cutting a release

Releases are cut from `main`, from a clean tree, by pushing a tag. Everything
below is done before the tag exists, because pushing the tag publishes.

1. **Write the entry.** Rename `## [Unreleased]` in
   [`CHANGELOG.md`](../CHANGELOG.md) to `## [X.Y.Z] - YYYY-MM-DD` with the
   release date, open a fresh empty `Unreleased` above it, and update the two
   link definitions at the bottom (`[Unreleased]` compares the new tag to
   `HEAD`; add a `[X.Y.Z]` release link). Check the entry against the history it
   covers: `git log --oneline <previous tag>..HEAD`.
2. **Green gates**, in this order:

   ```sh
   make check         # fmt + lint + race tests
   make check-golden  # goldens and docs/cli.md are not stale; needs a clean tree
   make integration   # needs a real bd >= 1.2.2 first on PATH
   ```

3. **Dry-run the pipeline** locally — it publishes nothing and needs no tag:

   ```sh
   GOBIN=$PWD/bin go install github.com/goreleaser/goreleaser/v2@v2.18.0
   ./bin/goreleaser check
   ./bin/goreleaser release --snapshot --clean --skip=publish
   ./dist/sc_linux_amd64_v1/sc version
   test -s dist/checksums.txt
   ```

   The same dry run in CI is the release workflow's `workflow_dispatch` with
   `dry_run` left on; it uploads `dist/` as an artifact to inspect.
4. **Commit** the changelog (`docs: changelog for vX.Y.Z`) and push it.
5. **Tag and push:**

   ```sh
   git tag -a vX.Y.Z -m vX.Y.Z
   git push origin vX.Y.Z
   ```

   The tag is what the release is built from: `.goreleaser.yaml` stamps
   `internal/cli.Version` with `{{ .Tag }}`, `Commit` with the short commit and
   `Date` with the *commit* date, so a rebuild of the same tag is
   byte-identical. Those three `-X` paths exist in three places — the Makefile,
   `.goreleaser.yaml` and `internal/cli/version.go` — and a test fails if they
   drift (P1-10).
6. **Watch the workflow.** `.github/workflows/release.yml` runs `make check` and
   `make check-golden` as a gate, then `goreleaser release --clean`: tarballs
   for `linux`/`darwin` × `amd64`/`arm64`, `checksums.txt`, the GitHub release
   with the generated notes, and the Homebrew cask when a tap token is
   configured. It fails the release if `sc version` in the built binary does not
   print the tag.
7. **Verify what a user gets**, on a machine that is not the build machine:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/maxh213/slop-cannon/vX.Y.Z/install.sh | sh
   sc version   # prints vX.Y.Z
   sc doctor
   ```

   `install.sh` verifies `checksums.txt` before it installs; do not use
   `SC_NO_VERIFY=1` to get past a mismatch — a mismatch means the release is
   wrong.
8. **Paste the changelog entry** into the GitHub release body above the
   generated notes, so the release page says what changed in prose.

> The repository has no `origin` remote yet, so the owner and name are a guess,
> and the two artefacts spell them differently. `.goreleaser.yaml` reads them as
> two environment variables, `SC_REPO_OWNER` and `SC_REPO_NAME`, which the
> release workflow fills from its GitHub context (`github.repository_owner` and
> `github.event.repository.name`), defaulting to `maxh213` and `slop-cannon`.
> `install.sh` takes the pair as a single value instead — `--repo <owner/name>`
> or `SC_REPO`, defaulting to `maxh213/slop-cannon` — and does not read
> `SC_REPO_OWNER` or `SC_REPO_NAME` at all. When the repository moves, both
> defaults, the `install.sh` URL quoted in the README, and the changelog's link
> definitions move with it.

## Pre-releases and patch releases

- A pre-release is a tag with a suffix: `v0.2.0-rc.1`. goreleaser's
  `prerelease: auto` marks the GitHub release as a pre-release for any tag with
  one, `install.sh`'s `latest` skips it, and the changelog gets its own entry
  which is folded into the final version's entry when that ships. Pre-release
  ordering is semver's: `v0.2.0-rc.1` < `v0.2.0`.
- A patch release for something already released is cut the same way from
  `main` when `main` has nothing else in it. When it does, branch from the tag
  (`git switch -c release/v0.1.x v0.1.0`), cherry-pick the fix, tag from the
  branch, and land the fix on `main` too — a tag that is not an ancestor of
  `main` is a fix that will regress in the next minor.

## If a release goes wrong

- **Before anyone installed it**: delete the GitHub release and the tag
  (`git push --delete origin vX.Y.Z`), fix, and re-tag the same number. This is
  the only case where a tag is ever reused.
- **After it is out**: never re-point the tag. Mark the GitHub release as a
  pre-release or add a warning at the top of its body, cut `vX.Y.Z+1` with the
  fix, and record the bad version in the changelog under the new entry's
  `Fixed`, naming what was wrong with it.
- A release whose artefacts are broken but whose code is fine still needs a new
  version number: `checksums.txt` is signed into nothing but people's shell
  history, and re-uploading different bytes under the same tag breaks
  `install.sh` for anyone who cached it.

See also: [docs index](README.md), [cli.md](cli.md) for the command surface,
[config.md](config.md) for the config surface, and
[goldens.md](goldens.md) for the golden files `make check-golden` compares.
