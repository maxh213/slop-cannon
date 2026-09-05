#!/bin/sh
# dogfood.sh — the P10-05 dogfood run: sc fired at slop-cannon itself.
#
# P10-05 asks for `sc . -m "<a real small task from Discovered Tasks>"` on
# this repository, safety on, ammo 4, and an honest account of what came
# back. This script does that, with one safety change that is not
# negotiable:
#
#     sc is NEVER fired at the working tree you are sitting in.
#
# It clones the repository into a temporary directory and fires at the
# clone, so a misbehaving agent — a stray `git checkout`, a `rm -rf`, a
# rebase in the wrong worktree — cannot reach your work. The clone is a real
# clone with real history, which is the point: the product owner reads the
# real README, the real docs and the real IMPLEMENTATION-PROGRESS.md, and
# the engineer edits the real code.
#
# `git remote remove origin` is run in the clone before anything is fired.
# That is deliberate. Safety on means "push the branch and open a pull
# request", and the clone's origin is your working tree, so leaving it would
# push an agent's branch straight back into the repository this script
# exists to protect. With no remote, sc's safety-on delivery degrades the
# way P4-05 documents it: commit, leave the branch in the clone, say so, and
# park the bead in `needs_human`. Nothing is pushed and nothing is merged.
# Passing --safety off is refused outright — that path merges to the default
# branch, and this script has no business merging anything.
#
# What is real and what is not:
#
#   * the cartridge is real — the installed, logged-in `claude` CLI, driven
#     by internal/cartridge/claude, with the model of --model. This spends
#     real tokens;
#   * `sc` is the production binary built from ./cmd/sc;
#   * `bd` is the real beads binary (bin/bd-1.2.2 when it is present,
#     otherwise the one on PATH, which must be >= 1.2.2). It writes into the
#     clone's own .beads, never yours;
#   * git is real, but only inside the clone: branches, commits and
#     worktrees all happen, and the worktrees live under the run's state
#     directory (D-23), which is inside the temporary machine;
#   * `gh` is a stand-in that can only answer `--version`, `auth status` and
#     an empty `pr list`, and fails loudly on anything else. Nothing in this
#     script may reach GitHub. `--review-prs=false` is passed as well, so
#     the external-PR poller never starts.
#
# Nothing outside the temporary directory is written: XDG_CONFIG_HOME and
# XDG_STATE_HOME point inside it and your own bd database is never touched.
# HOME is *not* moved, because the Claude CLI needs the real login in
# ~/.claude; that is the one deliberate difference from the demo, and it is
# why this script is manual. The adapter drops the CLAUDECODE / CLAUDE_CODE_*
# markers a parent Claude Code session sets (claude/args.go, sessionMarkers),
# so a nested run does not confuse the child.
#
# Usage:
#   scripts/manual/dogfood.sh [options]
#
#   --model M      model to fire (default: sonnet; e.g. haiku, opus, or a
#                  full id like claude-haiku-4-5-20251001)
#   --brief TEXT   the -m brief (default: the P10-09 discovered task)
#   --ammo N       ammo for the run (default 4, as P10-05 asks)
#   --beads N      max_beads_per_plan (default 1: the brief names one small
#                  task, and a bigger first plan makes a longer, dearer run)
#   --timeout SECS wall-clock cap on the whole run (default 1800); the run
#                  gets SIGTERM, which sc turns into a graceful stop
#   --source PATH  repository to clone (default: this repository)
#   --tui          render the TUI instead of --headless (needs a terminal)
#   --preflight    build the machine, run `sc doctor` and resolve the run
#                  against it, then stop without firing an agent — the free
#                  way to check the plumbing before spending tokens
#   --no-record    do not write testdata/recorded/dogfood-run.jsonl
#   --keep         leave the temporary machine on disk and print its path
#
# After the run the script prints what the agents actually produced — the
# `sc/` branches in the clone, their commits, their diffstat against the
# default branch and the bead states — because that, not the exit status, is
# what a dogfood run is for. The same summary is written to $work/verdict.txt.
#
# The recording is redacted on the way out (paths, home directory, the git
# identity the agents' commits are authored with, e-mail addresses and
# anything shaped like a token); sc has already redacted credentials and
# stripped control characters where it published them. It is written to
# testdata/recorded/dogfood-run.jsonl only when every check passed, so a
# botched run cannot overwrite the evidence; a run that failed leaves its
# redacted log in the temporary directory instead.
#
# The recording committed as testdata/recorded/dogfood-run.jsonl came from
# claude/sonnet, ammo 4, safety on, one bead (2026-09-05), fired at a clone
# of this repository at 5ccfce7 with the P10-09 brief above: 3m39s, 721
# events, 5 agents (1 product owner, 1 engineer, 3 testers), a one-bead plan,
# a 13-line test added to internal/cli/root_test.go, three approving verdicts
# in round 1 and not one request for changes. With no remote the safety-on
# delivery handed off exactly as P4-05 says it should — the branch
# sc/sc-dogfood-xdm-… was left in the clone, the bead went to needs_human,
# the worktree was kept, and sc exited 0. Reviewed afterwards by hand: the
# branch carries one commit touching one file, `go test ./internal/cli/`
# passes on it, and renaming `internal/cli.Commit` in .goreleaser.yaml makes
# the new test fail — which is what P10-09 asked for.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

model=sonnet
brief='The Discovered Tasks section of IMPLEMENTATION-PROGRESS.md has an open task, P10-09 "Pin `.goreleaser.yaml`'"'"'s ldflags to `internal/cli` with a test". Do exactly that task and nothing else. `.goreleaser.yaml` carries three `-X slop-cannon/internal/cli.{Version,Commit,Date}` ldflags and no test guards them, so renaming one of those variables would silently ship a release binary that prints `dev`. `internal/cli/root_test.go` already has `TestVersionLdflagsMatchMakefile`, which reads the Makefile and fails on such a rename; add a sibling test in the same file that reads `../../.goreleaser.yaml` and asserts the same three `slop-cannon/internal/cli` variables are named in its ldflags. Done when renaming one of the three variables fails a test that names the release config. Do not change `.goreleaser.yaml` itself, the Makefile, or IMPLEMENTATION-PROGRESS.md.'
ammo=4
beads=1
limit=1800
source=$root
headless=1
record=1
keep=0
preflight=0

while [ $# -gt 0 ]; do
	case $1 in
	--model)
		model=${2:?--model needs a value}
		shift 2
		;;
	--brief)
		brief=${2:?--brief needs a value}
		shift 2
		;;
	--ammo)
		ammo=${2:?--ammo needs a value}
		shift 2
		;;
	--beads)
		beads=${2:?--beads needs a value}
		shift 2
		;;
	--timeout)
		limit=${2:?--timeout needs a value}
		shift 2
		;;
	--source)
		source=${2:?--source needs a value}
		shift 2
		;;
	--safety)
		echo "dogfood.sh: safety is always on here: safety off merges to the default branch, and this script must never merge anything" >&2
		exit 2
		;;
	--tui)
		headless=0
		shift
		;;
	--preflight)
		preflight=1
		shift
		;;
	--no-record)
		record=0
		shift
		;;
	--keep)
		keep=1
		shift
		;;
	-h | --help)
		sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "dogfood.sh: unknown argument: $1 (try --help)" >&2
		exit 2
		;;
	esac
done

for tool in go git claude timeout; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "dogfood.sh: $tool is not on PATH" >&2
		exit 1
	}
done

# The pinned, verified bd (Appendix A) wins over whatever is on PATH, which
# on some machines is older than the D-21 minimum.
bd_bin=$root/bin/bd-1.2.2
if [ ! -x "$bd_bin" ]; then
	bd_bin=$(command -v bd 2>/dev/null || true)
fi
if [ -z "$bd_bin" ]; then
	echo "dogfood.sh: no bd binary (build one into $root/bin/bd-1.2.2 or put bd on PATH)" >&2
	exit 1
fi
bd_version=$("$bd_bin" version 2>/dev/null | head -1)
case $bd_version in
*" 1.2."* | *" 1.3."* | *" 1.4."* | *" 2."*) ;;
*)
	echo "dogfood.sh: $bd_bin reports '$bd_version'; bd 1.2.2 or newer is required (D-21)" >&2
	exit 1
	;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/sc-dogfood.XXXXXXXX")
if [ "$keep" -eq 1 ]; then
	trap 'echo "dogfood.sh: the machine is at $work" >&2' EXIT
else
	trap 'rm -rf "$work"' EXIT
fi

bin=$work/bin
repo=$work/sc-dogfood
mkdir -p "$bin" "$work/config" "$work/state" "$work/gh"

echo "dogfood.sh: building sc" >&2
(cd "$root" && go build -o "$bin/sc" ./cmd/sc)
ln -sf "$bd_bin" "$bin/bd"

# --- the clone --------------------------------------------------------------
# A full clone of the committed history, then the remote is removed so that
# safety-on delivery cannot push anything back into the source repository.
GIT_TERMINAL_PROMPT=0
export GIT_TERMINAL_PROMPT
echo "dogfood.sh: cloning $source" >&2
git clone -q "$source" "$repo"
git -C "$repo" remote remove origin
if [ -n "$(git -C "$repo" remote)" ]; then
	echo "dogfood.sh: the clone still has a remote; refusing to fire" >&2
	exit 1
fi
clone_head=$(git -C "$repo" rev-parse HEAD)
clone_branch=$(git -C "$repo" rev-parse --abbrev-ref HEAD)
echo "dogfood.sh: clone at $clone_branch $clone_head" >&2

# --- the gh stand-in --------------------------------------------------------
# Nothing here may reach GitHub. With no remote sc should never ask gh to do
# anything, so this stand-in exists to prove it: it answers the three
# read-only probes `sc doctor` makes and fails loudly on everything else,
# and every call it sees is recorded for the report.
cat >"$bin/gh" <<EOF
#!/bin/sh
dir=$work/gh
EOF
cat >>"$bin/gh" <<'EOF'
mkdir -p "$dir"
echo "$*" >>"$dir/calls"
case "$1 ${2:-}" in
"--version " | "version ") echo "gh version 2.98.0 (stand-in)" ;;
"auth status") echo "Logged in to github.com as dogfood-stand-in" ;;
"pr list") echo '[]' ;;
*)
	echo "gh stand-in: refusing '$*': the dogfood run must not reach GitHub" >&2
	exit 1
	;;
esac
EOF
chmod +x "$bin/gh"

# --- the configuration ------------------------------------------------------
# Only XDG_CONFIG_HOME and XDG_STATE_HOME move; HOME stays put so the Claude
# CLI finds its login. The limits are tightened from their defaults to keep a
# manual run short and cheap, not because the defaults are wrong.
mkdir -p "$work/config/slop-cannon"
cat >"$work/config/slop-cannon/config.toml" <<EOF
schema_version = 1
active_cartridge = "claude"
ammo = $ammo
safety = true
check_command = "go build ./..."
gate_poll_interval = "5s"
max_beads_per_plan = $beads
idle_replan_interval = "30s"

[limits]
agent_timeout = "12m"
max_review_rounds = 2

[cartridges.claude]
model = "$model"
EOF

PATH=$bin:$PATH
XDG_CONFIG_HOME=$work/config
XDG_STATE_HOME=$work/state
# The adapter already drops the markers a parent Claude Code session sets
# (claude/args.go, sessionMarkers), but pin the CLI's own configuration
# directory too, so moving XDG_CONFIG_HOME cannot move the login with it.
CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
export PATH XDG_CONFIG_HOME XDG_STATE_HOME CLAUDE_CONFIG_DIR

if [ "$preflight" -eq 1 ]; then
	echo "dogfood.sh: preflight — nothing is fired" >&2
	"$bin/sc" doctor --repo "$repo" || true
	"$bin/sc" "$repo" --message "$brief" --headless --review-prs=false --dry-run
	if [ "$keep" -eq 1 ]; then echo "dogfood.sh: repo $repo" >&2; fi
	exit 0
fi

# --- fire -------------------------------------------------------------------
set -- "$repo" --message "$brief" --review-prs=false
if [ "$headless" -eq 1 ]; then set -- "$@" --headless; fi

echo "dogfood.sh: firing claude/$model at $repo (ammo $ammo, safety on, cap ${limit}s)" >&2
status=0
timeout --signal=TERM --kill-after=120s "$limit" "$bin/sc" "$@" || status=$?
echo "dogfood.sh: sc exited $status" >&2

log=$(find "$work/state/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
if [ -z "$log" ]; then
	echo "dogfood.sh: the run wrote no event log; nothing to record" >&2
	exit 1
fi

# --- what the agents produced ----------------------------------------------
# A dogfood run is judged on the branch, not the exit status.
verdict=$work/verdict.txt
{
	echo "# dogfood run"
	echo "sc exit status: $status"
	echo "clone: $clone_branch at $clone_head"
	echo
	echo "## the source repository is untouched"
	echo "source HEAD: $(git -C "$source" rev-parse HEAD 2>/dev/null || echo '?')"
	echo "source branches: $(git -C "$source" for-each-ref --format='%(refname:short)' refs/heads | tr '\n' ' ')"
	echo
	echo "## branches in the clone"
	git -C "$repo" for-each-ref --format='%(refname:short) %(objectname:short)' refs/heads
	echo
	for br in $(git -C "$repo" for-each-ref --format='%(refname:short)' 'refs/heads/sc/*'); do
		echo "### $br"
		git -C "$repo" log --oneline "$clone_branch..$br" || true
		echo
		git -C "$repo" diff --stat "$clone_branch...$br" || true
		echo
		git -C "$repo" diff "$clone_branch...$br" || true
		echo
	done
	echo "## beads"
	(cd "$repo" && "$bin/bd" list --all 2>&1 | head -40) || true
	echo
	echo "## gh stand-in calls"
	cat "$work/gh/calls" 2>/dev/null || echo "(none — nothing asked gh to do anything)"
} >"$verdict" 2>&1
cat "$verdict" >&2

# --- record -----------------------------------------------------------------
# sc redacts credentials and strips control characters where it publishes
# (P9-05), so this pass is only about the machine the run happened on:
# temporary paths, the checkout, the home directory, the git identity the
# agents' commits are authored with, e-mail addresses, and a belt-and-braces
# sweep for token-shaped strings.
git_name=$(git -C "$root" config --get user.name 2>/dev/null || true)
scrub_name() {
	if [ -n "$git_name" ]; then sed "s|$git_name|Fixture Author|g"; else cat; fi
}
redact() {
	# The e-mail rule keeps a "u003c" in front of the address: an address
	# inside a JSON <...> pair would otherwise lose the backslash
	# escape's body and leave a stray control character in text sc had
	# already scrubbed.
	sed \
		-e "s|$work|/tmp/sc-dogfood|g" \
		-e "s|$root|/src/slop-cannon|g" \
		-e "s|$HOME|/home/user|g" \
		-e 's/sk-ant-[A-Za-z0-9_-]*/[REDACTED]/g' \
		-e 's/gh[pousr]_[A-Za-z0-9]*/[REDACTED]/g' \
		-e 's/\(u003c\)\{0,1\}[A-Za-z0-9._%+-]\{1,\}@[A-Za-z0-9.-]\{1,\}\.[A-Za-z]\{2,\}/\1redacted@example.com/g' |
		scrub_name
}

# --- check ------------------------------------------------------------------
# P10-05's shape, asserted from the event log: the product owner planned, a
# bead was created, an engineer implemented, testers voted, and the work was
# delivered as far as a repository with no remote allows.
fail=0
say() { echo "dogfood.sh: check: $1 $2" >&2; }
want() {
	if grep -q -- "$2" "$log"; then
		say ok "$1"
	else
		say MISS "$1 (no event matching $2)"
		fail=1
	fi
}
want "plan produced" '"kind":"plan_produced"'
want "bead created" '"kind":"bead_created"'
want "engineer fired" '"kind":"agent_spawned".*"role":"engineer"'
want "implementation reported" '"kind":"response_received"'
want "tester voted" '"kind":"verdict_received"'
want "run finished" '"kind":"run_finished"'

verdicts=$(grep -c '"kind":"verdict_received"' "$log" || true)
if [ "$verdicts" -ge 3 ]; then
	say ok "three verdicts in a round ($verdicts verdicts in the run)"
else
	say MISS "three verdicts in a round (saw $verdicts)"
	fail=1
fi

branches=$(git -C "$repo" for-each-ref --format='%(refname:short)' 'refs/heads/sc/*' | wc -l | tr -d ' ')
if [ "$branches" -ge 1 ]; then
	say ok "the run left $branches sc/ branch(es) in the clone"
else
	say MISS "no sc/ branch in the clone"
	fail=1
fi

commits=0
for br in $(git -C "$repo" for-each-ref --format='%(refname:short)' 'refs/heads/sc/*'); do
	n=$(git -C "$repo" rev-list --count "$clone_branch..$br")
	commits=$((commits + n))
done
if [ "$commits" -ge 1 ]; then
	say ok "the branches carry $commits commit(s) of their own"
else
	say MISS "the sc/ branches carry no commit"
	fail=1
fi

# The default branch of the clone must not have moved: safety on never
# merges, and with no remote it never pushes either.
if [ "$(git -C "$repo" rev-parse "$clone_branch")" = "$clone_head" ]; then
	say ok "the clone's default branch never moved"
else
	say MISS "the clone's default branch moved to $(git -C "$repo" rev-parse "$clone_branch")"
	fail=1
fi

# The committed recording is the evidence for P10-05, so only a run that
# passed every check may replace it; a failed run leaves its own redacted
# copy beside the machine instead.
if [ "$record" -eq 1 ]; then
	if [ "$fail" -eq 0 ]; then
		out=$root/testdata/recorded/dogfood-run.jsonl
		mkdir -p "$(dirname "$out")"
	else
		out=$work/dogfood-run.jsonl
		echo "dogfood.sh: the checks did not pass; not touching the committed recording" >&2
	fi
	redact <"$log" >"$out"
	# The redaction must lose no events and introduce no escapes of its own.
	src_lines=$(wc -l <"$log" | tr -d ' ')
	out_lines=$(wc -l <"$out" | tr -d ' ')
	src_cr=$(grep -o '\\r' <"$log" | wc -l | tr -d ' ')
	out_cr=$(grep -o '\\r' <"$out" | wc -l | tr -d ' ')
	if [ "$src_lines" != "$out_lines" ] || [ "$src_cr" != "$out_cr" ]; then
		echo "dogfood.sh: the redaction damaged the log ($src_lines→$out_lines lines, $src_cr→$out_cr escapes)" >&2
		exit 1
	fi
	if [ -n "$git_name" ] && grep -q -- "$git_name" "$out"; then
		echo "dogfood.sh: the recording still names $git_name; not committing it" >&2
		exit 1
	fi
	echo "dogfood.sh: recorded $out_lines events into $out" >&2
fi

if [ "$keep" -eq 1 ]; then
	echo "dogfood.sh: clone   $repo" >&2
	echo "dogfood.sh: state   $work/state/slop-cannon" >&2
	echo "dogfood.sh: verdict $verdict" >&2
fi

if [ "$fail" -ne 0 ]; then
	echo "dogfood.sh: the dogfood run did not reach the shape P10-05 asks for (sc exited $status)" >&2
	exit 1
fi
echo "dogfood.sh: the dogfood run produced a reviewable branch (sc exited $status)" >&2
