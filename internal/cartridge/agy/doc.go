// Package agy implements cartridge.Cartridge on top of the agy CLI. One
// firing is one "agy --output-format json --print <prompt>" process in the
// agent's worktree, with the print flag and its prompt value last. Prompts over
// 96 KiB are written verbatim to a mode-0600 file inside that worktree and
// replaced by a short Read instruction; the file is removed when the process
// ends or fails to start.
//
// The flags were checked against "agy --help" from VerifiedVersion. The final
// JSON envelope fixtures under testdata/agy are synthetic offline fixtures,
// not captures of a provider session. Registration and routing enablement are
// deliberately left to the shared integration stage.
//
// read_write pins --mode accept-edits and --dangerously-skip-permissions so a
// headless firing can write without waiting for approval. read_only has a
// candidate --mode plan mapping, but the
// adapter refuses to declare it enforced until the live write-refusal canary
// in testdata/agy/README.md passes. read_exec is not supported.
package agy
