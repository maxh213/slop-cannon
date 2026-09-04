#!/bin/sh
# brief-run.sh — the P8-02 brief-mode manual verification.
#
# It fires the real cannon, with the real Claude cartridge, at a throwaway
# copy of the fixture repository and then checks the recorded event log for
# the shape P8-02 asks for:
#
#     plan → implement → 3 verdicts → approved → PR opened
#
# Unlike scripts/demo/demo.sh (which is deterministic and scripted), this
# script spends real tokens: every agent reply comes from Claude Code. It is
# a manual verification, not a test, so it is never run by `make check`.
#
# What is real and what is not:
#
#   * the cartridge is real — the installed, logged-in `claude` CLI, driven
#     by internal/cartridge/claude, with the model of --model;
#   * `sc` is the production binary built from ./cmd/sc (not the demo's
#     scdemo, which also links the mock);
#   * `bd` is the real beads binary (bin/bd-1.2.2 when it is present,
#     otherwise the one on PATH, which must be ≥ 1.2.2);
#   * git is real: the fixture copy gets an initial commit, a "main" branch
#     and a bare origin, so branches, worktrees and pushes all happen;
#   * `gh` is a stand-in. sc must never open a pull request against a real
#     GitHub repository from a script, so the stand-in numbers the pull
#     requests it is asked to open, keeps their bodies under $work/gh, and
#     reports them merged so the D-20 gate closes and the run finishes.
#     Everything sc does up to and including `gh pr create` is production
#     code; only GitHub itself is faked.
#
# Nothing outside the temporary directory is written: the repository is a
# copy, XDG_CONFIG_HOME and XDG_STATE_HOME point inside it, and the user's
# own bd database is never touched. HOME is *not* moved, because the Claude
# CLI needs the real login in ~/.claude; that is the one deliberate
# difference from the demo, and it is why this script is manual.
#
# Usage:
#   scripts/manual/brief-run.sh [options]
#
#   --model M      model to fire (default: sonnet; e.g. haiku, opus, or a
#                  full id like claude-haiku-4-5-20251001)
#   --brief TEXT   the -m brief (default: the two planted bugs)
#   --ammo N       ammo for the run (default 4, as P8-02 asks)
#   --safety on|off  safety (default on: open a pull request, never merge)
#   --timeout SECS wall-clock cap on the whole run (default 1800); the run
#                  gets SIGTERM, which sc turns into a graceful stop
#   --tui          render the TUI instead of --headless (needs a terminal)
#   --preflight    build the machine, run `sc doctor` and resolve the run
#                  against it, then stop without firing an agent — the free
#                  way to check the plumbing before spending tokens
#   --no-record    do not write testdata/recorded/brief-run.jsonl
#   --keep         leave the temporary machine on disk and print its path
#
# The recording is redacted on the way out (paths, home directory, e-mail
# addresses and anything shaped like a token); sc has already redacted
# credentials and stripped control characters where it published them. It is
# written to testdata/recorded/brief-run.jsonl only when every check passed,
# so a botched run cannot overwrite the evidence; a run that failed leaves
# its redacted log in the temporary directory instead.
#
# The recording committed as testdata/recorded/brief-run.jsonl came from
# claude/sonnet, ammo 4, safety on (2026-09-04): a two-bead plan, nine agent
# sessions, three approving verdicts in round 1 on each bead, two pull
# requests, 2m56s, 1104 events.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

model=sonnet
brief='The stats package has two bugs: `go test ./...` fails in stats/. Fix them so the whole test suite passes. Change nothing else.'
ammo=4
safety=true
limit=1800
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
	--safety)
		case ${2:-} in
		on) safety=true ;;
		off) safety=false ;;
		*)
			echo "brief-run.sh: --safety takes on or off" >&2
			exit 2
			;;
		esac
		shift 2
		;;
	--timeout)
		limit=${2:?--timeout needs a value}
		shift 2
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
		echo "brief-run.sh: unknown argument: $1 (try --help)" >&2
		exit 2
		;;
	esac
done

for tool in go git claude timeout; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "brief-run.sh: $tool is not on PATH" >&2
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
	echo "brief-run.sh: no bd binary (build one into $root/bin/bd-1.2.2 or put bd on PATH)" >&2
	exit 1
fi
bd_version=$("$bd_bin" version 2>/dev/null | head -1)
case $bd_version in
*" 1.2."* | *" 1.3."* | *" 1.4."* | *" 2."*) ;;
*)
	echo "brief-run.sh: $bd_bin reports '$bd_version'; bd 1.2.2 or newer is required (D-21)" >&2
	exit 1
	;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/sc-brief.XXXXXXXX")
if [ "$keep" -eq 1 ]; then
	trap 'echo "brief-run.sh: the machine is at $work" >&2' EXIT
else
	trap 'rm -rf "$work"' EXIT
fi

bin=$work/bin
repo=$work/repo
mkdir -p "$bin" "$work/config" "$work/state" "$work/gh"

echo "brief-run.sh: building sc" >&2
(cd "$root" && go build -o "$bin/sc" ./cmd/sc)
ln -sf "$bd_bin" "$bin/bd"

# --- the fixture copy -------------------------------------------------------
cp -R "$root/testdata/fixture-repo" "$repo"

GIT_AUTHOR_NAME="Fixture Author"
GIT_AUTHOR_EMAIL=fixture@example.com
GIT_COMMITTER_NAME="Fixture Author"
GIT_COMMITTER_EMAIL=fixture@example.com
GIT_TERMINAL_PROMPT=0
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL GIT_TERMINAL_PROMPT

git -C "$repo" init -q -b main
git -C "$repo" add -A
git -C "$repo" commit -q -m "Initial import of fixture project"
git init -q --bare -b main "$work/remote.git"
git -C "$repo" remote add origin "$work/remote.git"
git -C "$repo" push -q -u origin main

# --- the gh stand-in --------------------------------------------------------
# See the header: sc's delivery path is real, GitHub is not. Every pull
# request it opens is reported merged, which is what closes a safety-on bead
# (D-20) so a brief run can finish.
cat >"$bin/gh" <<EOF
#!/bin/sh
dir=$work/gh
EOF
cat >>"$bin/gh" <<'EOF'
mkdir -p "$dir"
echo "$*" >>"$dir/calls"
case "$1 ${2:-}" in
"--version " | "version ") echo "gh version 2.98.0 (stand-in)" ;;
"auth status") echo "Logged in to github.com as manual-fixture" ;;
"pr list") echo '[]' ;;
"label create") ;;
"pr create")
	head=; base=
	while [ $# -gt 0 ]; do
		case $1 in
		--head)
			head=$2
			shift
			;;
		--base)
			base=$2
			shift
			;;
		esac
		shift
	done
	n=$(cat "$dir/count" 2>/dev/null || echo 0)
	n=$((n + 1))
	echo "$n" >"$dir/count"
	echo "$head" >"$dir/head-$n"
	echo "$base" >"$dir/base-$n"
	cat >"$dir/pr-$n.md"
	echo "https://github.com/slop-cannon/fixture/pull/$n"
	;;
"pr view")
	n=${3:-1}
	case $n in
	*[!0-9]*) n=$(cat "$dir/count" 2>/dev/null || echo 1) ;;
	esac
	head=$(cat "$dir/head-$n" 2>/dev/null || echo "sc/unknown")
	base=$(cat "$dir/base-$n" 2>/dev/null || echo main)
	printf '{"number":%s,"url":"https://github.com/slop-cannon/fixture/pull/%s","state":"MERGED","title":"sc pull request %s","headRefName":"%s","baseRefName":"%s","merged":true,"mergedAt":"2026-01-01T00:00:00Z","mergeCommit":{"oid":"0000000000000000000000000000000000000000"}}\n' \
		"$n" "$n" "$n" "$head" "$base"
	;;
*)
	echo "gh stand-in: unexpected command: $*" >&2
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
safety = $safety
check_command = "go test ./..."
gate_poll_interval = "5s"
max_beads_per_plan = 3
# A planning firing that fails for a reason that is not the plan's fault
# waits an idle interval before trying again (P6-19). The default is ten
# minutes, which is most of a manual run's budget, so shorten the wait.
idle_replan_interval = "30s"

[limits]
agent_timeout = "10m"
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
	echo "brief-run.sh: preflight — nothing is fired" >&2
	"$bin/sc" doctor --repo "$repo"
	"$bin/sc" "$repo" --message "$brief" --headless --dry-run
	if [ "$keep" -eq 1 ]; then echo "brief-run.sh: repo $repo" >&2; fi
	exit 0
fi

# --- fire -------------------------------------------------------------------
set -- "$repo" --message "$brief"
if [ "$headless" -eq 1 ]; then set -- "$@" --headless; fi

echo "brief-run.sh: firing claude/$model at $repo (ammo $ammo, safety $safety, cap ${limit}s)" >&2
status=0
timeout --signal=TERM --kill-after=120s "$limit" "$bin/sc" "$@" || status=$?
echo "brief-run.sh: sc exited $status" >&2

log=$(find "$work/state/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
if [ -z "$log" ]; then
	echo "brief-run.sh: the run wrote no event log; nothing to record" >&2
	exit 1
fi

# --- record -----------------------------------------------------------------
# sc redacts credentials and strips control characters where it publishes
# (P9-05), so this pass is only about the machine the run happened on:
# temporary paths, the checkout, the home directory, the git identity the
# agents' commits are authored with, e-mail addresses, and a belt-and-braces
# sweep for token-shaped strings.
#
# The agents commit their own work (D-14) with the machine's own git
# identity — sc does not hand them GIT_AUTHOR_NAME — so `git log` and `git
# show` output in the recording carries the user's name. That is correct in
# a real run and must not be committed here.
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
		-e "s|$work|/tmp/sc-brief|g" \
		-e "s|$root|/src/slop-cannon|g" \
		-e "s|$HOME|/home/user|g" \
		-e 's/sk-ant-[A-Za-z0-9_-]*/[REDACTED]/g' \
		-e 's/gh[pousr]_[A-Za-z0-9]*/[REDACTED]/g' \
		-e 's/\(u003c\)\{0,1\}[A-Za-z0-9._%+-]\{1,\}@[A-Za-z0-9.-]\{1,\}\.[A-Za-z]\{2,\}/\1redacted@example.com/g' |
		scrub_name
}

# --- check ------------------------------------------------------------------
# P8-02's "Done when", asserted from the event log: plan → implement → three
# verdicts → approved → PR opened.
fail=0
say() { echo "brief-run.sh: check: $1 $2" >&2; }
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
want "approving verdict" '"kind":"verdict_received".*"outcome":"approve"'
want "pull request opened" '"kind":"pr_opened"'

verdicts=$(grep -c '"kind":"verdict_received"' "$log" || true)
if [ "$verdicts" -ge 3 ]; then
	say ok "three verdicts in a round ($verdicts verdicts in the run)"
else
	say MISS "three verdicts in a round (saw $verdicts)"
	fail=1
fi

# The committed recording is the evidence for P8-02, so only a run that
# passed every check may replace it; a failed run leaves its own redacted
# copy beside the machine instead.
if [ "$record" -eq 1 ]; then
	if [ "$fail" -eq 0 ] && [ "$status" -eq 0 ]; then
		out=$root/testdata/recorded/brief-run.jsonl
		mkdir -p "$(dirname "$out")"
	else
		out=$work/brief-run.jsonl
		echo "brief-run.sh: the checks did not pass; not touching the committed recording" >&2
	fi
	redact <"$log" >"$out"
	# The redaction must lose no events and introduce no escapes of its own.
	src_lines=$(wc -l <"$log" | tr -d ' ')
	out_lines=$(wc -l <"$out" | tr -d ' ')
	src_cr=$(grep -o '\\r' <"$log" | wc -l | tr -d ' ')
	out_cr=$(grep -o '\\r' <"$out" | wc -l | tr -d ' ')
	if [ "$src_lines" != "$out_lines" ] || [ "$src_cr" != "$out_cr" ]; then
		echo "brief-run.sh: the redaction damaged the log ($src_lines→$out_lines lines, $src_cr→$out_cr escapes)" >&2
		exit 1
	fi
	if [ -n "$git_name" ] && grep -q -- "$git_name" "$out"; then
		echo "brief-run.sh: the recording still names $git_name; not committing it" >&2
		exit 1
	fi
	echo "brief-run.sh: recorded $out_lines events into $out" >&2
fi

if [ "$keep" -eq 1 ]; then
	echo "brief-run.sh: repo  $repo" >&2
	echo "brief-run.sh: state $work/state/slop-cannon" >&2
	echo "brief-run.sh: prs   $work/gh" >&2
fi

if [ "$fail" -ne 0 ] || [ "$status" -ne 0 ]; then
	echo "brief-run.sh: FAILED (sc exited $status)" >&2
	exit 1
fi
echo "brief-run.sh: the brief run reached a pull request" >&2
