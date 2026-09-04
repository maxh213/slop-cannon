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
#
# Timing: `sc run` owns the terminal only while the run is going. The TUI is
# torn down the moment the orchestrator finishes (internal/cli/run.go's
# renderTUI cancels the Bubble Tea program on run.Done), so every key below
# is pressed while the run is still live — the scripted run takes ~13.6s and
# the last key goes in at ~8.5s. Keys typed after the TUI exits would land on
# the shell prompt instead, which is why there is no `q` (and no `y`) here:
# quitting the TUI is what `q` is for, and this recording lets the run end by
# itself and holds on the summary sc prints afterwards.
# scripts/demo/scdemo/demo_test.go pins these numbers against the tape.

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

# The graph while the cannon fires: the plan lands, two beads appear and the
# first agents are shots on their way to them.
Sleep 3200ms

# The Agents view: what is live right now, with the engineer that misfired.
Type "2"
Sleep 2600ms

# The Feedback view: the QA lens has asked for changes on the notes bead by
# now, and the engineer is answering the item.
Type "3"
Sleep 2700ms

# Back to the graph for delivery: approvals, two pull requests, both beads
# closing with confetti, and the run finishing on its own at ~13.6s.
Type "1"
Sleep 7s

# The TUI is gone; what stays on screen is sc's own summary and the first-run
# notice saying where the run wrote everything.
Sleep 3s
