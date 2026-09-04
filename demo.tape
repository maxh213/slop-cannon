# demo.tape — records docs/demo.gif with VHS (P8-07).
#
# Install and run, from the repository root:
#
#   go install github.com/charmbracelet/vhs@latest && vhs demo.tape
#
# VHS also needs ttyd and ffmpeg on PATH (see https://github.com/charmbracelet/vhs).
# Run `make demo DEMO_ARGS=--check` once beforehand: it warms the Go build
# cache and asserts the scripted run still exercises every animation, so the
# recording below opens on the cannon rather than on a cold compile.
#
# What is recorded is `make demo` — the deterministic demo of scripts/demo:
# the real cannon fired at a throwaway fixture repository, with the mock
# cartridge playing scripts/demo/scenario.json and stand-ins for bd and gh.
# The scenario exercises every cannon animation (P7-06): a shot per agent
# spawned, smoke for the engineer whose first reply is malformed, a ricochet
# for the QA lens asking for changes, a burst per approval, and confetti as
# each bead closes.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 13
Set Width 1500
Set Height 860
Set Padding 12
Set Framerate 30
Set PlaybackSpeed 1.0
Set TypingSpeed 60ms

# Rebuild the demo binaries off camera, so the recording does not open on a
# Go build. With a warm cache this is a second or two.
Hide
Type "go build -o /dev/null ./scripts/demo/scdemo && go build -o /dev/null ./testdata/orchestrator/fakebd && clear"
Enter
Sleep 20s
Show

Type "make demo"
Sleep 700ms
Enter

# The run: plan, two beads, a misfire, a round of changes, approvals and
# delivery. The scripted run takes about 15s; the TUI stays on the finished
# graph until it is quit, so a little slack costs only still frames.
Sleep 22s

# A look at the other views before quitting.
Type "2"
Sleep 2500ms
Type "3"
Sleep 2500ms
Type "1"
Sleep 2s
Type "q"
Sleep 800ms
# `q` asks for confirmation while a run is live; after it finished it just quits.
Type "y"
Sleep 1500ms
