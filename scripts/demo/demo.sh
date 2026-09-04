#!/bin/sh
# demo.sh — the deterministic Slop Cannon demo (P8-07). `make demo` runs it.
#
# It builds a complete throwaway machine for one run and then fires the real
# cannon at it:
#
#   * a fixture repository (testdata/fixture-repo) with an initial commit,
#     a "main" branch and a bare origin, so git, worktrees and delivery are
#     real;
#   * the stateful `bd` stand-in (testdata/orchestrator/fakebd) behind a
#     wrapper that also answers `bd version` and enforces --readonly, so the
#     read-only agent shim's self-check passes as it does against real bd;
#   * a `gh` stand-in that opens numbered pull requests and reports them
#     merged, so safety-on delivery closes its beads;
#   * the mock cartridge playing scripts/demo/scenario.json, so every agent
#     reply is scripted rather than generated.
#
# Nothing outside the temporary directory is touched: HOME, XDG_CONFIG_HOME
# and XDG_STATE_HOME all point inside it, the user's own bd is never called
# and no network is used. The demo binary (scripts/demo/scdemo) is `sc` plus
# the mock cartridge; everything the run does is production code.
#
# Usage:
#   scripts/demo/demo.sh [--headless] [--check] [--no-anim] [--keep] [--safety-off]
#
#   --headless    render the run as one line per event instead of the TUI
#                 (the TUI needs a terminal; this is what CI and a pipe want)
#   --check       run headless and then assert, from the run's event log, that
#                 the scenario really did raise the event behind every cannon
#                 animation; the demo's own test, and it needs no terminal
#   --no-anim     run the TUI with the cannon animations turned off
#   --keep        leave the temporary machine on disk and print its path
#   --safety-off  merge into the default branch instead of opening a PR
#
# The scenario exercises every cannon animation (P7-06): a shot per agent
# spawned, smoke for the engineer whose first reply is malformed, a ricochet
# for the QA lens asking for changes, a burst per approval, and confetti as
# each bead closes.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

headless=0
check=0
noanim=0
keep=0
safety=true
for arg in "$@"; do
	case "$arg" in
	--headless) headless=1 ;;
	--check)
		check=1
		headless=1
		;;
	--no-anim) noanim=1 ;;
	--keep) keep=1 ;;
	--safety-off) safety=false ;;
	-h | --help)
		sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "demo.sh: unknown argument: $arg (try --help)" >&2
		exit 2
		;;
	esac
done

for tool in go git; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "demo.sh: $tool is not on PATH" >&2
		exit 1
	}
done

work=$(mktemp -d "${TMPDIR:-/tmp}/sc-demo.XXXXXXXX")
if [ "$keep" -eq 1 ]; then
	trap 'echo "demo.sh: the demo machine is at $work" >&2' EXIT
else
	trap 'rm -rf "$work"' EXIT
fi

bin=$work/bin
repo=$work/repo
home=$work/home
mkdir -p "$bin" "$home" "$work/config" "$work/state" "$work/bd"

# --- the binaries -----------------------------------------------------------
# Built into the temporary directory: `make demo` never writes to bin/.
echo "demo.sh: building sc-demo and the bd stand-in" >&2
(cd "$root" && go build -o "$bin/sc-demo" ./scripts/demo/scdemo)
(cd "$root" && go build -o "$bin/bd-real" ./testdata/orchestrator/fakebd)

# --- the fixture repository -------------------------------------------------
cp -R "$root/testdata/fixture-repo" "$repo"
mkdir -p "$repo/.beads" # the tracker is already initialised (D-13)

# git runs with an isolated identity and no user or system configuration, so
# the demo looks the same on every machine.
GIT_CONFIG_NOSYSTEM=1
GIT_CONFIG_GLOBAL=/dev/null
GIT_AUTHOR_NAME="Fixture Author"
GIT_AUTHOR_EMAIL=fixture@example.com
GIT_COMMITTER_NAME="Fixture Author"
GIT_COMMITTER_EMAIL=fixture@example.com
GIT_TERMINAL_PROMPT=0
export GIT_CONFIG_NOSYSTEM GIT_CONFIG_GLOBAL GIT_TERMINAL_PROMPT
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

git -C "$repo" init -q -b main
git -C "$repo" add -A
git -C "$repo" commit -q -m "Initial import of fixture project"
git init -q --bare -b main "$work/remote.git"
git -C "$repo" remote add origin "$work/remote.git"
git -C "$repo" push -q -u origin main

# --- the bd stand-in --------------------------------------------------------
# The wrapper adds the two things the fake tracker does not implement and the
# run needs: `bd ping` (the D-13 health check) and `bd version` plus
# --readonly enforcement (what beads.Shim.Verify probes before every agent,
# so the read-only shim is installed and checked exactly as in production).
cat >"$bin/bd" <<EOF
#!/bin/sh
SC_E2E_FAKEBD_DIR=$work/bd
export SC_E2E_FAKEBD_DIR
EOF
cat >>"$bin/bd" <<'EOF'
ro=0
if [ "${1:-}" = "--readonly" ]; then
	ro=1
	shift
fi
cmd=${1:-}
if [ "$cmd" = "-C" ]; then cmd=${3:-}; fi
case "$cmd" in
version) echo '{"schema_version":1,"data":{"version":"1.2.2"}}'; exit 0 ;;
ping) echo '{"schema_version":1,"data":{"status":"ok","total_ms":1}}'; exit 0 ;;
esac
if [ "$ro" -eq 1 ]; then
	case "$cmd" in
	ready | show | list | blocked | comments | dep | gate | stats | statuses | config) ;;
	*)
		echo "Error: operation '$cmd' is not allowed in read-only mode" >&2
		exit 1
		;;
	esac
fi
EOF
echo "exec \"$bin/bd-real\" \"\$@\"" >>"$bin/bd"
chmod +x "$bin/bd"

# --- the gh stand-in --------------------------------------------------------
# Every pull request it opens is reported merged, which is what closes a
# safety-on bead (D-20). Bodies are kept so --keep can show them.
cat >"$bin/gh" <<EOF
#!/bin/sh
dir=$work/gh
EOF
cat >>"$bin/gh" <<'EOF'
mkdir -p "$dir"
case "$1 ${2:-}" in
"auth status") echo "Logged in to github.com as demo" ;;
"pr list") echo '[]' ;;
"label create") ;;
"pr create")
	n=$(cat "$dir/count" 2>/dev/null || echo 0)
	n=$((n + 1))
	echo "$n" >"$dir/count"
	cat >"$dir/pr-$n.md"
	echo "https://github.com/slop-cannon/demo/pull/$n"
	;;
"pr view") echo '{"state":"MERGED"}' ;;
*)
	echo "gh stand-in: unexpected command: $*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$bin/gh"

# --- the configuration ------------------------------------------------------
mkdir -p "$work/config/slop-cannon"
cat >"$work/config/slop-cannon/config.toml" <<EOF
active_cartridge = "mock"
ammo = 4
safety = $safety
check_command = "true"
gate_poll_interval = "500ms"

[cartridges.mock]
env = { SC_MOCK_SCENARIO = "$root/scripts/demo/scenario.json" }
EOF

# --- fire -------------------------------------------------------------------
PATH=$bin:$PATH
HOME=$home
XDG_CONFIG_HOME=$work/config
XDG_STATE_HOME=$work/state
export PATH HOME XDG_CONFIG_HOME XDG_STATE_HOME

# The brief the product owner is given. The scripted plan answers this one
# brief; scripts/demo/scenario.json quotes it back in its plan.
brief="Give the fixture project somewhere to write things down."

set -- "$repo" --message "$brief" --cartridge mock
if [ "$headless" -eq 1 ]; then set -- "$@" --headless; fi
if [ "$noanim" -eq 1 ]; then set -- "$@" --no-anim; fi

echo "demo.sh: firing at $repo" >&2
status=0
"$bin/sc-demo" "$@" || status=$?

if [ "$check" -eq 1 ]; then
	log=$(find "$XDG_STATE_HOME/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
	if [ -z "$log" ]; then
		echo "demo.sh: check: the run wrote no event log" >&2
		exit 1
	fi
	# One assertion per cannon animation (P7-06). The demo only demonstrates
	# the animations if the scripted run really raises the events they are
	# played for, and internal/tui/anim.go maps them exactly this way: an
	# agent spawned is a shot, an agent failed is smoke, an approving verdict
	# is a burst, a changes-requested verdict is a ricochet, and a bead
	# reaching `closed` is confetti.
	missed=0
	for spec in \
		'shot      (agent spawned)|"kind":"agent_spawned"' \
		'smoke     (agent failed)|"kind":"agent_failed".*"failure":"bad_output"' \
		'burst     (approving verdict)|"kind":"verdict_received".*"outcome":"approve"' \
		'ricochet  (changes requested)|"kind":"verdict_received".*"outcome":"changes"' \
		'confetti  (bead closed)|"kind":"bead_state_changed".*"to":"closed"' \
		'run completed|"kind":"run_finished".*"reason":"completed"'; do
		what=${spec%%|*}
		pattern=${spec#*|}
		if grep -q -- "$pattern" "$log"; then
			echo "demo.sh: check: ok   $what" >&2
		else
			echo "demo.sh: check: MISS $what (no event matching $pattern)" >&2
			missed=1
		fi
	done
	if [ "$missed" -ne 0 ] || [ "$status" -ne 0 ]; then
		echo "demo.sh: check: FAILED (the run exited $status)" >&2
		exit 1
	fi
	echo "demo.sh: check: every animation was exercised" >&2
fi

if [ "$keep" -eq 1 ]; then
	echo "demo.sh: repo $repo" >&2
	echo "demo.sh: state $XDG_STATE_HOME/slop-cannon" >&2
fi
exit "$status"
