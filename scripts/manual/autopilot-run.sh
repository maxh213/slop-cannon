#!/bin/sh
# autopilot-run.sh — the P8-03 autopilot-mode manual verification.
#
# It is scripts/manual/brief-run.sh without the brief: the same throwaway
# machine, the same real Claude cartridge, the same `gh` stand-in, but `sc`
# is fired at the repository with no -m, so the product owner decides for
# itself what the fixture project needs.
#
# Autopilot never stops on its own (D-18), so *this script* stops it. It
# watches the run's event log and sends one SIGINT — the signal half of the
# `s` key, both of which land on orchestrator.Run.Interrupt — once the run
# has, in autopilot, done the whole job at least once:
#
#     planned → created beads → implemented → reviewed → opened a pull
#     request → gone quiet → planned *again* (the D-18 idle re-plan)
#
# Being stopped is the expected ending here, not a failure: `run_finished`
# with reason `stopped` and exit status 0 is what a healthy autopilot run
# looks like when the human presses `s`.
#
# What is real and what is not (identical to brief-run.sh):
#
#   * the cartridge is real — the installed, logged-in `claude` CLI, driven
#     by internal/cartridge/claude, with the model of --model;
#   * `sc` is the production binary built from ./cmd/sc;
#   * `bd` is the real beads binary (bin/bd-1.2.2 when present, otherwise
#     the one on PATH, which must be >= 1.2.2);
#   * git is real: the fixture copy gets an initial commit, a "main" branch
#     and a bare origin, so branches, worktrees and pushes all happen;
#   * `gh` is a stand-in. sc must never open a pull request against a real
#     GitHub repository from a script, so the stand-in numbers the pull
#     requests it is asked to open, keeps their bodies under $work/gh, and
#     reports them merged so the D-20 gate closes, the beads close, and the
#     ready queue can empty — which is what makes the planner idle and
#     re-plan. Everything sc does up to and including `gh pr create` is
#     production code; only GitHub itself is faked.
#
# Nothing outside the temporary directory is written: the repository is a
# copy, XDG_CONFIG_HOME and XDG_STATE_HOME point inside it, and the user's
# own bd database is never touched. HOME is *not* moved, because the Claude
# CLI needs the real login in ~/.claude; that is the one deliberate
# difference from the demo, and it is why this script is manual.
#
# Usage:
#   scripts/manual/autopilot-run.sh [options]
#
#   --model M      model to fire (default: sonnet; e.g. haiku, opus, or a
#                  full id like claude-haiku-4-5-20251001)
#   --ammo N       ammo for the run (default 4, as P8-02 asks)
#   --safety on|off  safety (default on: open a pull request, never merge)
#   --beads N      max_beads_per_plan for the run (default 3: the fixture
#                  has exactly three things wrong with it, and a smaller
#                  first plan is a shorter, cheaper manual run)
#   --idle D       idle_replan_interval (default 30s; the shipped default is
#                  10m, which is longer than a whole manual run)
#   --linger SECS  after the idle re-plan is seen, wait up to this long for
#                  that product owner to finish before stopping (default
#                  180), so the recording shows what it decided
#   --timeout SECS hard cap on the whole run (default 2700). Reaching it
#                  stops the run the same way, and the checks then say the
#                  conditions were never met.
#   --tui          render the TUI instead of --headless (needs a terminal;
#                  the stop is still SIGINT, which is the same code path the
#                  `s` key takes)
#   --credits      do not fire Claude: run the deterministic mock check
#                  instead — an autopilot run whose only cartridge reports
#                  its credits exhausted must finish by itself with reason
#                  `credits_exhausted` and exit 3 (D-18b). Free and fast.
#   --preflight    build the machine, run `sc doctor` and resolve the run
#                  against it, then stop without firing an agent — the free
#                  way to check the plumbing before spending tokens
#   --no-record    do not write testdata/recorded/autopilot-run.jsonl
#   --keep         leave the temporary machine on disk and print its path
#
# The recording is redacted on the way out (paths, home directory, e-mail
# addresses and anything shaped like a token); sc has already redacted
# credentials and stripped control characters where it published them. It is
# written to testdata/recorded/autopilot-run.jsonl only when every check
# passed, so a botched run cannot overwrite the evidence; a run that failed
# leaves its redacted log in the temporary directory instead.
#
# The recording committed as testdata/recorded/autopilot-run.jsonl came from
# claude/sonnet, ammo 4, safety on, max_beads_per_plan 3 (2026-09-04):
# 10m46s, 6405 events, 39 agents (7 product owners, 9 engineers, 23
# testers), 10 beads planned, 7 shipped through three approving verdicts
# each to a pull request, 21 verdicts and not one request for changes. The
# first plan named the Mean off-by-one and the Median the README promises;
# the second added the Max all-negative bug, so all three planted faults
# were found inside three minutes. The fifth product owner proposed nothing
# ("every concrete defect ... is already claimed"), the run idled and
# planned a sixth time, and it was then stopped with one SIGINT, which is
# the only reason it stopped.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

model=sonnet
ammo=4
safety=true
beads=3
idle=30s
linger=180
limit=2700
headless=1
record=1
keep=0
preflight=0
credits=0

while [ $# -gt 0 ]; do
	case $1 in
	--model)
		model=${2:?--model needs a value}
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
			echo "autopilot-run.sh: --safety takes on or off" >&2
			exit 2
			;;
		esac
		shift 2
		;;
	--beads)
		beads=${2:?--beads needs a value}
		shift 2
		;;
	--idle)
		idle=${2:?--idle needs a value}
		shift 2
		;;
	--linger)
		linger=${2:?--linger needs a value}
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
	--credits)
		credits=1
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
		echo "autopilot-run.sh: unknown argument: $1 (try --help)" >&2
		exit 2
		;;
	esac
done

need="go git"
if [ "$credits" -eq 1 ]; then
	need="$need timeout"
else
	need="$need claude"
fi
for tool in $need; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "autopilot-run.sh: $tool is not on PATH" >&2
		exit 1
	}
done

work=$(mktemp -d "${TMPDIR:-/tmp}/sc-autopilot.XXXXXXXX")
if [ "$keep" -eq 1 ]; then
	trap 'echo "autopilot-run.sh: the machine is at $work" >&2' EXIT
else
	trap 'rm -rf "$work"' EXIT
fi

bin=$work/bin
repo=$work/repo
mkdir -p "$bin" "$work/config" "$work/state" "$work/gh"

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
# (D-20) — and in autopilot that matters twice over, because a bead that
# never closes keeps the ready queue busy and the planner never idles.
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

PATH=$bin:$PATH
XDG_CONFIG_HOME=$work/config
XDG_STATE_HOME=$work/state
export PATH XDG_CONFIG_HOME XDG_STATE_HOME
mkdir -p "$work/config/slop-cannon"

# ============================================================================
# --credits: the deterministic half of P8-03.
#
# D-18 gives autopilot exactly two endings: the user stops it, or every
# configured cartridge is out of credits. The first half of this script
# proves the first ending with a real model; this half proves the second one
# with the mock, and costs nothing. The machine is the demo's (P8-07): the
# sc-demo binary, which is `sc` plus the mock adapter, the stateful bd
# stand-in, and a scenario whose only rule answers the planning firing with
# cartridge.ErrCreditsExhausted.
# ============================================================================
if [ "$credits" -eq 1 ]; then
	echo "autopilot-run.sh: building sc-demo and the bd stand-in (no tokens are spent)" >&2
	(cd "$root" && go build -o "$bin/sc-demo" ./scripts/demo/scdemo)
	(cd "$root" && go build -o "$bin/bd-real" ./testdata/orchestrator/fakebd)
	mkdir -p "$repo/.beads" "$work/bd" # the tracker is already initialised (D-13)

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

	cat >"$work/credits.json" <<'EOF'
{
  "name": "autopilot: the only cartridge runs out of credits",
  "detect": { "version": "mock/credits", "auth": "ok" },
  "rules": [
    {
      "name": "plan-out-of-credits",
      "match": { "role": "po", "task": "plan" },
      "steps": [
        { "kind": "text", "text": "Reading the repository." },
        {
          "kind": "fail",
          "failure": {
            "kind": "credits_exhausted",
            "message": "Credit balance is too low to continue"
          }
        }
      ],
      "result": {}
    }
  ]
}
EOF

	cat >"$work/config/slop-cannon/config.toml" <<EOF
schema_version = 1
active_cartridge = "mock"
ammo = $ammo
safety = $safety
check_command = "true"
gate_poll_interval = "500ms"
idle_replan_interval = "$idle"

[cartridges.mock]
env = { SC_MOCK_SCENARIO = "$work/credits.json" }
EOF

	HOME=$work/home
	mkdir -p "$HOME"
	export HOME

	echo "autopilot-run.sh: firing the mock at $repo with no brief (autopilot)" >&2
	status=0
	# 120s is far longer than the scenario needs; if autopilot has *not*
	# ended by itself the check below fails on the timeout, which is the
	# point of the cap.
	timeout --signal=KILL 120s "$bin/sc-demo" "$repo" --cartridge mock --headless || status=$?
	echo "autopilot-run.sh: sc-demo exited $status" >&2

	log=$(find "$work/state/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
	if [ -z "$log" ]; then
		echo "autopilot-run.sh: the credits run wrote no event log" >&2
		exit 1
	fi
	fail=0
	if grep -q '"kind":"run_finished".*"reason":"credits_exhausted"' "$log"; then
		echo "autopilot-run.sh: check: ok   autopilot ended itself on credits exhaustion (D-18b)" >&2
	else
		echo "autopilot-run.sh: check: MISS no run_finished with reason credits_exhausted" >&2
		fail=1
	fi
	if [ "$status" -eq 3 ]; then
		echo "autopilot-run.sh: check: ok   exit status 3 (tui.ExitCreditsExhausted)" >&2
	else
		echo "autopilot-run.sh: check: MISS exit status $status, want 3" >&2
		fail=1
	fi
	if [ "$keep" -eq 1 ]; then echo "autopilot-run.sh: log $log" >&2; fi
	if [ "$fail" -ne 0 ]; then
		echo "autopilot-run.sh: FAILED (credits check)" >&2
		exit 1
	fi
	echo "autopilot-run.sh: the credits check passed" >&2
	exit 0
fi

# ============================================================================
# The real thing: Claude, no brief, stopped by SIGINT.
# ============================================================================

# The pinned, verified bd (Appendix A) wins over whatever is on PATH, which
# on some machines is older than the D-21 minimum.
bd_bin=$root/bin/bd-1.2.2
if [ ! -x "$bd_bin" ]; then
	bd_bin=$(command -v bd 2>/dev/null || true)
fi
if [ -z "$bd_bin" ]; then
	echo "autopilot-run.sh: no bd binary (build one into $root/bin/bd-1.2.2 or put bd on PATH)" >&2
	exit 1
fi
bd_version=$("$bd_bin" version 2>/dev/null | head -1)
case $bd_version in
*" 1.2."* | *" 1.3."* | *" 1.4."* | *" 2."*) ;;
*)
	echo "autopilot-run.sh: $bd_bin reports '$bd_version'; bd 1.2.2 or newer is required (D-21)" >&2
	exit 1
	;;
esac
ln -sf "$bd_bin" "$bin/bd"

echo "autopilot-run.sh: building sc" >&2
(cd "$root" && go build -o "$bin/sc" ./cmd/sc)

# --- the configuration ------------------------------------------------------
# Only XDG_CONFIG_HOME and XDG_STATE_HOME move; HOME stays put so the Claude
# CLI finds its login. The limits are tightened from their defaults to keep a
# manual run short and cheap, not because the defaults are wrong.
cat >"$work/config/slop-cannon/config.toml" <<EOF
schema_version = 1
active_cartridge = "claude"
ammo = $ammo
safety = $safety
check_command = "go test ./..."
gate_poll_interval = "5s"
max_beads_per_plan = $beads
# D-18's idle re-plan interval. The default is ten minutes, which is longer
# than this whole verification; shortening it is what makes the second
# planning firing observable inside a manual run.
idle_replan_interval = "$idle"

[limits]
agent_timeout = "10m"
max_review_rounds = 2

[cartridges.claude]
model = "$model"
EOF

# The adapter already drops the markers a parent Claude Code session sets
# (claude/args.go, sessionMarkers), but pin the CLI's own configuration
# directory too, so moving XDG_CONFIG_HOME cannot move the login with it.
CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
export CLAUDE_CONFIG_DIR

if [ "$preflight" -eq 1 ]; then
	echo "autopilot-run.sh: preflight — nothing is fired" >&2
	"$bin/sc" doctor --repo "$repo"
	"$bin/sc" "$repo" --headless --dry-run
	if [ "$keep" -eq 1 ]; then echo "autopilot-run.sh: repo $repo" >&2; fi
	exit 0
fi

# --- fire -------------------------------------------------------------------
set -- "$repo"
if [ "$headless" -eq 1 ]; then set -- "$@" --headless; fi

echo "autopilot-run.sh: firing claude/$model at $repo with no brief (ammo $ammo, safety $safety)" >&2
"$bin/sc" "$@" &
pid=$!

# --- the stop watch ---------------------------------------------------------
# Autopilot never ends on its own (D-18), so the script is the human here.
# It waits for the run to have done the whole job once — a pull request from
# a planned, implemented, reviewed bead — and then to have planned *again
# with nothing left to do*, which is the D-18 idle re-plan. Once that
# product owner has finished (or --linger has run out) it sends one SIGINT.
count() { grep -c -- "$1" "$log" 2>/dev/null || true; }
alive() { kill -0 "$pid" 2>/dev/null; }

# idle_plans counts the planning firings that are D-18's idle re-plan, and
# only those. Merely counting planning firings is not enough: Planner.Demands
# reads SchedulerStatus.Quiet, which is the *last* dispatch's verdict, so the
# tick right after a plan lands still looks quiet and fires a second product
# owner before the new beads have been dispatched (seen on 2026-09-04; filed
# as P8-28). That firing is not the idle path and must not be mistaken for it.
#
# Two shapes count, and both are exactly what D-18 describes:
#
#   * a planning firing that started when every bead the run had created was
#     closed — the ready queue empty, nothing in flight, "when the ready
#     queue is empty the PO plans more work";
#   * a planning firing that follows a plan proposing nothing — "when the PO
#     has nothing to add the run idles and re-plans after
#     `idle_replan_interval`". An empty plan publishes a PlanProduced whose
#     bead list is empty and which mints nothing, and Planner.record sets
#     the next planning time to now + idle_replan_interval, so the firing
#     after it provably waited that interval out.
idle_plans() {
	awk '
		/"kind":"bead_created"/ { created++ }
		/"kind":"bead_state_changed"/ && /"to":"closed"/ { closed++ }
		/"kind":"plan_produced"/ { empty = (index($0, "\"beads\":[]") > 0) }
		/"kind":"agent_spawned"/ && /"role":"po"/ && /"task":"plan"/ {
			if (empty || (created > 0 && closed >= created)) n++
		}
		END { print n+0 }
	' "$log" 2>/dev/null || echo 0
}

# live_planners is how many planning product owners have been fired but not
# yet finished or failed, so the watch can wait for the idle one's answer
# instead of killing it mid-thought.
live_planners() {
	fired=$(count '"kind":"agent_spawned".*"role":"po".*"task":"plan"')
	done_=$(grep -cE '"kind":"agent_(finished|failed)".*"agent":"po-plan-' "$log" 2>/dev/null || true)
	echo $((${fired:-0} - ${done_:-0}))
}

started=$(date +%s)
deadline=$((started + limit))
log=
lingering=0
lingered=0
reason=

while :; do
	if ! alive; then
		reason="the run ended on its own"
		break
	fi
	if [ -z "$log" ]; then
		log=$(find "$work/state/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
	fi
	now=$(date +%s)
	if [ "$now" -ge "$deadline" ]; then
		reason="the ${limit}s cap was reached"
		break
	fi
	if [ -n "$log" ]; then
		prs=$(count '"kind":"pr_opened"')
		idle_seen=$(idle_plans)
		if [ "${prs:-0}" -ge 1 ] && [ "${idle_seen:-0}" -ge 1 ]; then
			if [ "$lingering" -eq 0 ]; then
				lingering=1
				lingered=$((now + linger))
				echo "autopilot-run.sh: the run shipped its plan and then re-planned with nothing left to do; waiting up to ${linger}s for that product owner" >&2
			elif [ "$(live_planners)" -le 0 ]; then
				reason="the idle re-plan finished"
				break
			elif [ "$now" -ge "$lingered" ]; then
				reason="the stop conditions were met"
				break
			fi
		fi
	fi
	sleep 3
done

status=0
if alive; then
	echo "autopilot-run.sh: $reason; sending SIGINT (the signal half of the \`s\` key)" >&2
	interrupted=1
	kill -INT "$pid" 2>/dev/null || true
	# A graceful stop lets in-flight agents finish or be stopped with a
	# grace period, so give it a real chance before escalating. A second
	# interrupt is orchestrator.Run.Kill; SIGKILL after that means sc is
	# wedged, and the checks below will say so.
	waited=0
	while alive && [ "$waited" -lt 180 ]; do
		sleep 3
		waited=$((waited + 3))
	done
	if alive; then
		echo "autopilot-run.sh: still running after ${waited}s; sending a second SIGINT" >&2
		kill -INT "$pid" 2>/dev/null || true
		waited=0
		while alive && [ "$waited" -lt 60 ]; do
			sleep 3
			waited=$((waited + 3))
		done
	fi
	if alive; then
		echo "autopilot-run.sh: sc did not stop; killing it" >&2
		kill -KILL "$pid" 2>/dev/null || true
	fi
else
	echo "autopilot-run.sh: $reason before it could be stopped" >&2
	interrupted=0
fi
wait "$pid" || status=$?
echo "autopilot-run.sh: sc exited $status" >&2

if [ -z "$log" ]; then
	log=$(find "$work/state/slop-cannon" -name events.jsonl -print 2>/dev/null | head -1)
fi
if [ -z "$log" ]; then
	echo "autopilot-run.sh: the run wrote no event log; nothing to record" >&2
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
		-e "s|$work|/tmp/sc-autopilot|g" \
		-e "s|$root|/src/slop-cannon|g" \
		-e "s|$HOME|/home/user|g" \
		-e 's/sk-ant-[A-Za-z0-9_-]*/[REDACTED]/g' \
		-e 's/gh[pousr]_[A-Za-z0-9]*/[REDACTED]/g' \
		-e 's/\(u003c\)\{0,1\}[A-Za-z0-9._%+-]\{1,\}@[A-Za-z0-9.-]\{1,\}\.[A-Za-z]\{2,\}/\1redacted@example.com/g' |
		scrub_name
}

# --- check ------------------------------------------------------------------
# P8-03's "Done when", asserted from the event log.
fail=0
say() { echo "autopilot-run.sh: check: $1 $2" >&2; }
want() {
	if grep -q -- "$2" "$log"; then
		say ok "$1"
	else
		say MISS "$1 (no event matching $2)"
		fail=1
	fi
}

plan=$(grep -m1 '"kind":"plan_produced"' "$log" || true)
if [ -n "$plan" ]; then
	say ok "the product owner planned with no brief"
else
	say MISS "the product owner planned with no brief"
	fail=1
fi

# Plan quality: the fixture has exactly three things wrong with it, and a
# product owner worth its ammo finds all three. Mean sums from index 1, so
# it drops values[0]; Max seeds its running maximum from zero, so every
# all-negative input answers 0; and stats.go ends with a TODO for Median
# that the README already promises. These are matched against every plan the
# run produced, not only the first.
plans_text=$(grep '"kind":"plan_produced"' "$log" || true)
planted() {
	if printf '%s' "$plans_text" | grep -qi -- "$2"; then
		say ok "the plan names $1"
	else
		say MISS "the plan never names $1"
		fail=1
	fi
}
planted "the Mean bug" 'Mean'
planted "the Max bug" 'Max'
planted "the Median TODO" 'Median'

want "bead created" '"kind":"bead_created"'
want "engineer fired" '"kind":"agent_spawned".*"role":"engineer"'
want "implementation reported" '"kind":"response_received"'
want "approving verdict" '"kind":"verdict_received".*"outcome":"approve"'
want "pull request opened" '"kind":"pr_opened"'
want "bead closed" '"kind":"bead_state_changed".*"to":"closed"'

verdicts=$(count '"kind":"verdict_received"')
if [ "${verdicts:-0}" -ge 3 ]; then
	say ok "three verdicts in a round ($verdicts verdicts in the run)"
else
	say MISS "three verdicts in a round (saw ${verdicts:-0})"
	fail=1
fi

# D-18's idle re-plan: with every bead it had planned now closed, the ready
# queue is empty and nothing is in flight, so the planner offers its demand
# again and another product owner is fired. See idle_plans above for why a
# bare count of planning firings would not prove this.
plans=$(count '"kind":"agent_spawned".*"role":"po".*"task":"plan"')
idle_seen=$(idle_plans)
if [ "${idle_seen:-0}" -ge 1 ]; then
	say ok "the run re-planned once it had nothing left to do ($idle_seen idle of $plans planning firings)"
else
	say MISS "the run never re-planned while idle (${plans:-0} planning firings, none of them idle)"
	fail=1
fi

# The ending: autopilot did not stop by itself, and the SIGINT stopped it
# gracefully. `stopped` is exit 0 (tui.ExitCodeFor).
if [ "$interrupted" -eq 1 ]; then
	say ok "autopilot was still running when it was stopped (D-18: it never ends on its own)"
else
	say MISS "autopilot ended on its own, which D-18 forbids"
	fail=1
fi
want "the run stopped on the interrupt" '"kind":"run_finished".*"reason":"stopped"'
if [ "$status" -eq 0 ]; then
	say ok "exit status 0"
else
	say MISS "exit status $status, want 0"
	fail=1
fi

# The committed recording is the evidence for P8-03, so only a run that
# passed every check may replace it; a failed run leaves its own redacted
# copy beside the machine instead.
if [ "$record" -eq 1 ]; then
	if [ "$fail" -eq 0 ]; then
		out=$root/testdata/recorded/autopilot-run.jsonl
		mkdir -p "$(dirname "$out")"
	else
		out=$work/autopilot-run.jsonl
		echo "autopilot-run.sh: the checks did not pass; not touching the committed recording" >&2
	fi
	redact <"$log" >"$out"
	# The redaction must lose no events and introduce no escapes of its own.
	src_lines=$(wc -l <"$log" | tr -d ' ')
	out_lines=$(wc -l <"$out" | tr -d ' ')
	src_cr=$(grep -o '\\r' <"$log" | wc -l | tr -d ' ')
	out_cr=$(grep -o '\\r' <"$out" | wc -l | tr -d ' ')
	if [ "$src_lines" != "$out_lines" ] || [ "$src_cr" != "$out_cr" ]; then
		echo "autopilot-run.sh: the redaction damaged the log ($src_lines→$out_lines lines, $src_cr→$out_cr escapes)" >&2
		exit 1
	fi
	if [ -n "$git_name" ] && grep -q -- "$git_name" "$out"; then
		echo "autopilot-run.sh: the recording still names $git_name; not committing it" >&2
		exit 1
	fi
	echo "autopilot-run.sh: recorded $out_lines events into $out" >&2
fi

if [ "$keep" -eq 1 ]; then
	echo "autopilot-run.sh: repo  $repo" >&2
	echo "autopilot-run.sh: state $work/state/slop-cannon" >&2
	echo "autopilot-run.sh: prs   $work/gh" >&2
fi

if [ "$fail" -ne 0 ]; then
	echo "autopilot-run.sh: FAILED (sc exited $status)" >&2
	exit 1
fi
echo "autopilot-run.sh: autopilot planned, shipped and re-planned, and stopped only when told to" >&2
