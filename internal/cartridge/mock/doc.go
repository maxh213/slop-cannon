// Package mock implements a scripted cartridge.Cartridge used by tests to
// drive the orchestrator without any real LLM CLI.
//
// A mock adapter plays a [Scenario]: an ordered list of [Rule]s, each with a
// [Match] on the firing's labels (role, lens, bead, round, task, or any
// label the orchestrator sets through cartridge.LabelsFor), the prompt, the
// tool policy or the model. The first matching rule whose use budget is not
// spent plays its [Step]s in order: assistant text, tool use and results,
// real side effects in the working directory (write, append or delete a
// file, run a command), pauses, and injected failures of every
// cartridge.Kind, plus a "malformed" outcome that succeeds with an
// unparseable sc-result block so the structured-output layer (P2-07) has
// something to retry. Side effects honour cartridge.RunSpec.ToolPolicy:
// writes under read_only or read_exec and commands under read_only are
// refused with an error tool result instead of being performed.
//
// Scenarios come from JSON (see testdata/cartridge/mock in the repository
// root for examples and [Load]) or from the programmatic builder:
//
//	s := mock.NewScenario("review loop")
//	s.Rule("implement", mock.Match{Role: "engineer"}).
//		Say("Adding the feature.").
//		Write("feature.go", "package x\n").
//		Exec("git", "add", "-A").
//		Structured(map[string]any{"summary": "done"})
//	s.Rule("first review", mock.Match{Role: "tester", Round: 1}).Once().
//		Fail(mock.FailRateLimited, "slow down")
//	s.Otherwise("echo").Echo()
//	adapter, err := mock.NewAdapter("mock", s, mock.WithClock(clock))
//
// Delays and the session timeout are measured on a [Clock]; tests pass a
// [FakeClock] and drive it with [FakeClock.Advance] so long pauses cost no
// wall time. Every cartridge.RunSpec the adapter receives is recorded and
// available through [Adapter.Calls], so tests can assert on prompts, tool
// policies and labels after the fact.
//
// The adapter registers itself with the default registry under [Name]. When
// built from configuration, the scenario file path comes from the
// cartridge's env table under [ScenarioEnv]; without one the adapter plays
// [DefaultScenario], which echoes the prompt back and succeeds.
package mock
