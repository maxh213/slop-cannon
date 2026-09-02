package conformance

import (
	"time"

	"slop-cannon/internal/cartridge/mock"
)

// MockScenario scripts a mock cartridge that passes every check for the
// given token: it says the token back to the echo prompt, ends the
// structured prompt with a valid block, writes WriteFileName for the write
// prompt (the mock refuses the write by itself under read_only, which is
// what the refuse check expects), sleeps for an hour on the hang prompt
// so the timeout and kill checks have something to cut short, and runs the
// two bd commands of the beads prompt through `sh -c` so the shim on the
// session's PATH is what answers. Anything else echoes its prompt. Pass
// the same token in Options.Token.
func MockScenario(token string) *mock.Scenario {
	s := mock.NewScenario("conformance")
	s.Rule("echo", mock.Match{PromptContains: echoMarker}).Say(token)
	s.Rule("structured", mock.Match{PromptContains: structuredMarker}).
		Say("Reporting the token.").
		Structured(map[string]any{"token": token, "ok": true})
	s.Rule("write", mock.Match{PromptContains: writeMarker}).
		Say("Creating the file.").
		Write(WriteFileName, token+"\n").
		Say("Done.")
	s.Rule("hang", mock.Match{PromptContains: hangMarker}).
		Say("Running the command.").
		Tool("Bash", "", hangMarker).
		Sleep(time.Hour)
	s.Rule("beads", mock.Match{PromptContains: beadsMarker}).
		Say("Running the commands.").
		Exec("sh", "-c", "bd update sc-shim-probe -s open").
		Exec("sh", "-c", "bd ready --json").
		Say("Reported.")
	s.Otherwise("echo-anything").Echo()
	return s
}

// NewMock builds a mock adapter under name (empty means "mock") playing
// MockScenario(token).
func NewMock(name, token string) (*mock.Adapter, error) {
	return mock.NewAdapter(name, MockScenario(token))
}
