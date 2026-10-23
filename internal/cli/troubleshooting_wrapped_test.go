package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// P9-19: hold the wrapped packages' messages to a row each.
//
// TestTroubleshootingCoversEveryMessageTheCommandLayerBuilds (P9-08) holds
// internal/cli and internal/config to a row apiece, and
// TestTroubleshootingCoversEveryUserFacingSentinel holds the *exported
// sentinels* of the packages underneath. What was outside the net until this
// file is the other several hundred messages those packages build with
// fmt.Errorf: a user reads `sc: <message>`, and most of what a run fails
// with is written in internal/orchestrator, internal/gitx or internal/beads
// rather than in the command layer — every run-lock refusal, for one.
//
// The guard below is the same shape as the command layer's, over the deeper
// packages: every message a non-test file of tsWrappedPackages builds must
// match a row of docs/troubleshooting.md or carry a written exemption in
// tsWrappedExempt. It is a large list because those packages are large, and
// that is the point: a new `orchestrator: …` sentence cannot reach a user
// without someone deciding, in writing, whether it needs a row.

// tsWrappedPackages are the packages underneath the command layer whose
// messages a user can read verbatim: the command layer prints what it
// returns, wrapped error and all.
var tsWrappedPackages = []string{
	"internal/agents",
	"internal/beads",
	"internal/cartridge",
	"internal/gitx",
	"internal/orchestrator",
	"internal/proc",
}

// tsWrappedSkipDirs are the directories inside those packages that are test
// scaffolding rather than product code, with the reason each is skipped.
// Their messages are read by `make check`, never by a user.
var tsWrappedSkipDirs = map[string]string{
	"conformance": "the cartridge conformance suite (P2-09) is a test harness: it is run by `make check` against a candidate adapter, and its messages are read by whoever is writing that adapter",
	"mock":        "the mock cartridge is a test double; it is never fired at a user's repository, and mock.ErrNoMatch is already exempted as a sentinel",
}

// The reasons an exemption can carry. Each one is a claim about why the
// message needs no row of its own, and every entry of tsWrappedExempt is
// one of them or a sentence written for that message alone.
const (
	// tsWrapDetail is for a message that begins with the wrapped error:
	// `%w: role %q has no tool policy`. What a user reads first is the
	// sentinel's own sentence, which has a row or an exemption of its own,
	// and this text is the detail behind it.
	tsWrapDetail = "the sentinel it wraps is the sentence a user reads; this is the detail behind it, and the sentinel has a row or an exemption of its own"
	// tsWrapStep is for a message that names a step and hands on the error
	// under it: `orchestrator: create run directory: %w`. The words a user
	// acts on are the wrapped error's — an OS error, git's, bd's, gh's —
	// and this text only says where sc was.
	tsWrapStep = "names the step and hands on the error under it, whose own words are what a user reads"
	// tsWrapInvariant is for a call sc assembles itself: a nil argument, an
	// empty id, a relative path where an absolute one is required. Reaching
	// one is a bug in sc, so there is nothing a user could be advised to do.
	tsWrapInvariant = "an invariant of sc's own assembly: it can only be reached by a bug in sc, so there is nothing to advise"
	// tsWrapToolOutput is for output sc could not read from a tool it
	// drives. The line quotes what the tool printed, and the only useful
	// answer is a bug report against sc or that tool's version.
	tsWrapToolOutput = "a tool sc drives answered in a shape sc cannot read; the line quotes the output, and the only remedy is a bug report"
	// tsWrapWrongAnswer is for an agent that returned a schema-valid answer
	// of the wrong kind. It is handled inside the run — the firing is
	// retried, then the bead is parked — and what a user reads is that
	// outcome, not this line.
	tsWrapWrongAnswer = "an agent answered with the wrong message type; the run retries the firing and parks the bead, and that outcome is what a user reads"
)

// tsWrappedExempt is one entry per message of tsWrappedPackages that
// docs/troubleshooting.md deliberately does not document, keyed by the text
// the code builds (with "…" wherever a verb interpolates something that is
// not a string constant). A new message must be documented or listed here,
// and an entry that outlives its message — or that a row now covers — fails
// too.
var tsWrappedExempt = map[string]string{
	"agents: InstallGitGuardForWorktree: empty worktree path":                               tsWrapInvariant,
	"agents: InstallGitGuardForWorktree: worktree path … is not absolute":                   tsWrapInvariant,
	"agents: agent run has neither a contract nor a message type":                           tsWrapInvariant,
	"agents: an enforcement for … is already registered":                                    tsWrapInvariant,
	"agents: built-in exclude pattern … is malformed: …":                                    tsWrapStep,
	"agents: cannot apply a tool policy to a nil run spec":                                  tsWrapInvariant,
	"agents: cannot register a nil enforcement for …":                                       tsWrapInvariant,
	"agents: cannot register an enforcement for an empty cartridge name":                    tsWrapInvariant,
	"agents: cannot resolve a tool policy for a nil cartridge":                              tsWrapInvariant,
	"agents: cartridge … refused tool policy …: …":                                          tsWrapStep,
	"agents: cartridge … reported no enforcement mode for …":                                tsWrapInvariant,
	"agents: creating beads for run …: …":                                                   tsWrapStep,
	"agents: creating the git guard directory: …":                                           tsWrapStep,
	"agents: creating the git guard: …":                                                     tsWrapStep,
	"agents: decoding … message: …":                                                         tsWrapStep,
	"agents: embedded schema dir: …":                                                        tsWrapStep,
	"agents: embedded schemas are missing ….schema.json":                                    tsWrapInvariant,
	"agents: engineer request for bead … has no worktree":                                   tsWrapInvariant,
	"agents: engineer request for bead … is a resolve_conflict with no conflict to resolve": tsWrapInvariant,
	"agents: engineer request for bead … is an address_feedback with no feedback items":     tsWrapInvariant,
	"agents: engineer request has no agent id":                                              tsWrapInvariant,
	"agents: engineer request has no bead":                                                  tsWrapInvariant,
	"agents: engineer worktree … is not an absolute path":                                   tsWrapInvariant,
	"agents: engineer …: …":                                                                 tsWrapStep,
	"agents: feedback item … (…) has no id":                                                 tsWrapInvariant,
	"agents: feedback item … appears twice":                                                 tsWrapInvariant,
	"agents: git guard directory is empty":                                                  tsWrapInvariant,
	"agents: git guard worktree … is not absolute":                                          tsWrapInvariant,
	"agents: installing the git guard: …":                                                   tsWrapStep,
	"agents: loading embedded schemas: …":                                                   tsWrapStep,
	"agents: making the git guard executable: …":                                            tsWrapStep,
	"agents: reading .gitignore in …: …":                                                    tsWrapStep,
	"agents: resolving the git guard directory: …":                                          tsWrapStep,
	"agents: resolving the real git path: …":                                                tsWrapStep,
	"agents: tester diff ladder is empty":                                                   tsWrapInvariant,
	"agents: tester evaluate_pushback needs at least one pushback or wontfix response":      tsWrapInvariant,
	"agents: tester evaluate_pushback needs the items the engineer pushed back on":          tsWrapInvariant,
	"agents: tester re_review needs the items raised in the previous round":                 tsWrapInvariant,
	"agents: tester request has no agent id":                                                tsWrapInvariant,
	"agents: tester request has no bead":                                                    tsWrapInvariant,
	"agents: tester request has round …; rounds are 1-based":                                tsWrapInvariant,
	"agents: tester workdir … is not an absolute path":                                      tsWrapInvariant,
	"agents: tester … needs the diff to review (gitx.Diff)":                                 tsWrapInvariant,
	"agents: writing the git guard: …":                                                      tsWrapStep,
	"agents: … is itself a git guard, refusing to chain guards":                             tsWrapInvariant,
	"applying …: …":                                  tsWrapStep,
	"bd comment: …":                                  tsWrapStep,
	"bd config set: set …, bd reported …":            tsWrapToolOutput,
	"bd context: …":                                  tsWrapStep,
	"bd create --graph: …":                           tsWrapStep,
	"bd export: line …: …":                           tsWrapStep,
	"bd export: …":                                   tsWrapStep,
	"bd gate check: …":                               tsWrapStep,
	"bd returned no issue":                           tsWrapToolOutput,
	"bd show: …":                                     tsWrapStep,
	"bead … duplicates open bead … (…)":              tsWrapInvariant,
	"bead … repeats the title of bead …":             tsWrapInvariant,
	"bead …: …":                                      tsWrapStep,
	"bead …: …: …":                                   tsWrapStep,
	"beads: InstallForWorktree: empty worktree path": tsWrapInvariant,
	"beads: InstallForWorktree: worktree path … is not absolute":                             tsWrapInvariant,
	"beads: add labels …: …: no labels":                                                      tsWrapInvariant,
	"beads: apply plan: epic …: …":                                                           tsWrapStep,
	"beads: apply plan: …: … is not an epic":                                                 tsWrapInvariant,
	"beads: build plan: …":                                                                   tsWrapStep,
	"beads: build plan: …: empty run id":                                                     tsWrapInvariant,
	"beads: build plan: …: plan has no beads":                                                tsWrapInvariant,
	"beads: comment …: …: empty body":                                                        tsWrapInvariant,
	"beads: create --graph: …: missing …":                                                    tsWrapInvariant,
	"beads: create --graph: …: plan has no nodes":                                            tsWrapInvariant,
	"beads: create run epic: …: empty run id":                                                tsWrapInvariant,
	"beads: create: …: empty title":                                                          tsWrapInvariant,
	"beads: creating shim directory: …":                                                      tsWrapStep,
	"beads: creating shim: …":                                                                tsWrapStep,
	"beads: creating temp file: …":                                                           tsWrapStep,
	"beads: custom status …: …":                                                              tsWrapStep,
	"beads: decoding …: …":                                                                   tsWrapStep,
	"beads: dep add: …: empty id":                                                            tsWrapInvariant,
	"beads: dep list: …: no ids":                                                             tsWrapInvariant,
	"beads: encoding … message: …":                                                           tsWrapStep,
	"beads: encoding …: …":                                                                   tsWrapStep,
	"beads: find run epic: …: empty run id":                                                  tsWrapInvariant,
	"beads: gate create: …: PR number …":                                                     tsWrapInvariant,
	"beads: gate create: …: no bead id":                                                      tsWrapInvariant,
	"beads: gate for: …: no bead id":                                                         tsWrapInvariant,
	"beads: gate resolve: …: no gate id":                                                     tsWrapInvariant,
	"beads: gate show: …: no gate id":                                                        tsWrapInvariant,
	"beads: history: …: empty id":                                                            tsWrapInvariant,
	"beads: installing shim: …":                                                              tsWrapStep,
	"beads: invalid custom status …":                                                         tsWrapInvariant,
	"beads: list run: …: empty run id":                                                       tsWrapInvariant,
	"beads: making shim executable: …":                                                       tsWrapStep,
	"beads: no custom statuses to set":                                                       tsWrapInvariant,
	"beads: plan created but finalising … (key …) failed: …":                                 tsWrapStep,
	"beads: plan dry run: …: bd counted … edges, plan has … dependencies":                    tsWrapInvariant,
	"beads: plan dry run: …: bd counted … nodes, plan has …":                                 tsWrapInvariant,
	"beads: plan dry run: …: …":                                                              tsWrapStep,
	"beads: reading …: …":                                                                    tsWrapStep,
	"beads: release: …: empty bead id":                                                       tsWrapInvariant,
	"beads: remove labels …: …: no labels":                                                   tsWrapInvariant,
	"beads: renew …: …: status …, assignee …":                                                tsWrapInvariant,
	"beads: renew …: …: …":                                                                   tsWrapStep,
	"beads: resolving real bd path: …":                                                       tsWrapStep,
	"beads: resolving shim directory: …":                                                     tsWrapStep,
	"beads: reverting … after bd init: …":                                                    tsWrapStep,
	"beads: run … has no epic: …":                                                            tsWrapStep,
	"beads: set metadata …: …: no keys":                                                      tsWrapInvariant,
	"beads: set status …: …: use Close to close a bead":                                      tsWrapInvariant,
	"beads: set status …: …: … is not a workflow state (want one of …)":                      tsWrapInvariant,
	"beads: shim directory is empty":                                                         tsWrapInvariant,
	"beads: shim … cannot run bd: …":                                                         tsWrapStep,
	"beads: shim …: `…` failed for another reason: …":                                        tsWrapStep,
	"beads: shim …: `…` was refused as …, want …":                                            tsWrapInvariant,
	"beads: shim …: bd version printed no version: …":                                        tsWrapStep,
	"beads: shim …: running `…`: …":                                                          tsWrapStep,
	"beads: show: …: no ids":                                                                 tsWrapInvariant,
	"beads: unset metadata …: …: no keys":                                                    tsWrapInvariant,
	"beads: update …: …: empty patch":                                                        tsWrapInvariant,
	"beads: writing shim: …":                                                                 tsWrapStep,
	"beads: writing …: …":                                                                    tsWrapStep,
	"beads: … is itself a read-only shim, refusing to chain shims":                           tsWrapInvariant,
	"beads: … …: …: empty agent id":                                                          tsWrapInvariant,
	"beads: …: …":                                                                            tsWrapStep,
	"beads: …: …: empty bead id":                                                             tsWrapInvariant,
	"cartridge …: factory returned nil":                                                      tsWrapInvariant,
	"cartridge …: the Claude Code adapter reports no tool-policy enforcement":                tsWrapInvariant,
	"cartridge …: …":                                                                         tsWrapStep,
	"cartridge: cannot register an empty name":                                               tsWrapInvariant,
	"cartridge: cannot register … with a nil factory":                                        tsWrapInvariant,
	"cartridge: run spec has an empty prompt":                                                tsWrapInvariant,
	"cartridge: run spec has no tool policy":                                                 tsWrapInvariant,
	"cartridge: run spec has no workdir":                                                     tsWrapInvariant,
	"cartridge: run spec max turns … is negative":                                            tsWrapInvariant,
	"cartridge: run spec timeout … is negative":                                              tsWrapInvariant,
	"cartridge: run spec tool policy … is not one of …":                                      tsWrapInvariant,
	"cartridge: run spec workdir … is not absolute":                                          tsWrapInvariant,
	"cartridge: … is already registered":                                                     tsWrapInvariant,
	"decoding bd envelope: …":                                                                tsWrapStep,
	"decoding bd output: …":                                                                  tsWrapStep,
	"gitx: ChangeRatio: both diffs are required":                                             tsWrapInvariant,
	"gitx: CommitAll: empty commit message":                                                  tsWrapInvariant,
	"gitx: CommitLeftovers: bead has no id":                                                  tsWrapInvariant,
	"gitx: Create: bead has no id":                                                           tsWrapInvariant,
	"gitx: CreatePR: empty head or base":                                                     tsWrapInvariant,
	"gitx: CreatePR: empty title":                                                            tsWrapInvariant,
	"gitx: Deliver: bead has no id":                                                          tsWrapInvariant,
	"gitx: DeliverMerge: bead has no id":                                                     tsWrapInvariant,
	"gitx: FindPR: empty branch":                                                             tsWrapInvariant,
	"gitx: NewManager: nil repo":                                                             tsWrapInvariant,
	"gitx: NewPRDeliverer: nil manager":                                                      tsWrapInvariant,
	"gitx: NewPRDeliverer: nil tracker":                                                      tsWrapInvariant,
	"gitx: NewPRSettler: nil manager":                                                        tsWrapInvariant,
	"gitx: NewPRSettler: nil tracker":                                                        tsWrapInvariant,
	"gitx: NewResumer: nil manager":                                                          tsWrapInvariant,
	"gitx: PushBranch: empty remote or branch":                                               tsWrapInvariant,
	"gitx: Reattach: empty bead id":                                                          tsWrapInvariant,
	"gitx: RebaseSafeChangeRatio: both diffs are required":                                   tsWrapInvariant,
	"gitx: Remove: empty bead id":                                                            tsWrapInvariant,
	"gitx: Resume: bead has no id":                                                           tsWrapInvariant,
	"gitx: clearing the stale gh:pr gate … on …: …":                                          tsWrapStep,
	"gitx: creating review bead for PR #…: …":                                                tsWrapStep,
	"gitx: creating the gh:pr gate for …: …":                                                 tsWrapStep,
	"gitx: creating worktree directory: …":                                                   tsWrapStep,
	"gitx: diff: bad status …: …":                                                            tsWrapStep,
	"gitx: diff: empty status at field …":                                                    tsWrapToolOutput,
	"gitx: diff: no status for …":                                                            tsWrapToolOutput,
	"gitx: diff: truncated entry …":                                                          tsWrapToolOutput,
	"gitx: diff: truncated rename entry …":                                                   tsWrapToolOutput,
	"gitx: diff: …":                                                                          tsWrapStep,
	"gitx: gh pr create: cannot read the PR number from …":                                   tsWrapToolOutput,
	"gitx: gh pr create: no pull request URL in output …":                                    tsWrapToolOutput,
	"gitx: gh pr list: decoding output: …":                                                   tsWrapStep,
	"gitx: gh pr list: unexpected output …: …":                                               tsWrapStep,
	"gitx: gh pr view …: unexpected output …: …":                                             tsWrapStep,
	"gitx: git: …":                                                                           tsWrapStep,
	"gitx: listing review beads: …":                                                          tsWrapStep,
	"gitx: looking up the gate of …: …":                                                      tsWrapStep,
	"gitx: moving … to needs_human: …":                                                       tsWrapStep,
	"gitx: post review: invalid pull request number …":                                       tsWrapToolOutput,
	"gitx: prByNumber: … is not a pull request number":                                       tsWrapToolOutput,
	"gitx: reading the reflog of …: …":                                                       tsWrapStep,
	"gitx: reading …: …":                                                                     tsWrapStep,
	"gitx: recording PR on …: …":                                                             tsWrapStep,
	"gitx: refusing to clear a lock outside …: …":                                            tsWrapStep,
	"gitx: removing the stale index lock …: …":                                               tsWrapStep,
	"gitx: the worktree registered at … is on branch …, which is not one of …'s own; …":      tsWrapInvariant,
	"gitx: truncated numstat rename entry …":                                                 tsWrapToolOutput,
	"gitx: unexpected numstat entry …":                                                       tsWrapToolOutput,
	"gitx: unexpected numstat entry …: …":                                                    tsWrapStep,
	"gitx: unexpected rev-list output …":                                                     tsWrapToolOutput,
	"gitx: unexpected rev-parse output …":                                                    tsWrapToolOutput,
	"gitx: …":                                                                                tsWrapStep,
	"gitx: …: …":                                                                             tsWrapStep,
	"listing the run's beads: …":                                                             tsWrapStep,
	"no responses list":                                                                      tsWrapToolOutput,
	"orchestrator: FireAgent: nil supervisor":                                                tsWrapInvariant,
	"orchestrator: New: nil run config":                                                      tsWrapInvariant,
	"orchestrator: New: run has no id":                                                       tsWrapInvariant,
	"orchestrator: New: run has no repository path":                                          tsWrapInvariant,
	"orchestrator: New: run has no state directory (D-23)":                                   tsWrapInvariant,
	"orchestrator: NewConductor: no beads client":                                            tsWrapInvariant,
	"orchestrator: NewConductor: no run":                                                     tsWrapInvariant,
	"orchestrator: NewPRWorktreeCheckout: no worktree manager":                               tsWrapInvariant,
	"orchestrator: OpenEventLog: empty run directory":                                        tsWrapInvariant,
	"orchestrator: RestoreRunConfig: nil run config":                                         tsWrapInvariant,
	"orchestrator: TeeRunLog: nil options":                                                   tsWrapInvariant,
	"orchestrator: a failed check on … raised no items to answer":                            tsWrapInvariant,
	"orchestrator: bead … has nothing for the engineer to address":                           tsWrapInvariant,
	"orchestrator: building the firing for …: …":                                             tsWrapStep,
	"orchestrator: clearing the worktree copy …: …":                                          tsWrapStep,
	"orchestrator: close …: …":                                                               tsWrapStep,
	"orchestrator: conductor: acting on the delivery of …: …":                                tsWrapStep,
	"orchestrator: conductor: delivering …: …":                                               tsWrapStep,
	"orchestrator: conductor: diffing …: …":                                                  tsWrapStep,
	"orchestrator: conductor: parking … for a person: …":                                     tsWrapStep,
	"orchestrator: conductor: re-delivering … after its conflict: …":                         tsWrapStep,
	"orchestrator: conductor: reading …'s comments: …":                                       tsWrapStep,
	"orchestrator: conductor: reading …: …":                                                  tsWrapStep,
	"orchestrator: conductor: restoring …'s thread: …":                                       tsWrapStep,
	"orchestrator: conductor: reviewing … again after its resume: …":                         tsWrapStep,
	"orchestrator: conductor: the delivery queue stopped before … for … ran":                 tsWrapInvariant,
	"orchestrator: conductor: … for …: …":                                                    tsWrapStep,
	"orchestrator: copy worktree: …":                                                         tsWrapStep,
	"orchestrator: copying … for …: …":                                                       tsWrapStep,
	"orchestrator: create run directory: …":                                                  tsWrapStep,
	"orchestrator: create the state directory: …":                                            tsWrapStep,
	"orchestrator: drop the stale run lock: …":                                               tsWrapStep,
	"orchestrator: encode state: …":                                                          tsWrapStep,
	"orchestrator: encode the run lock: …":                                                   tsWrapStep,
	"orchestrator: engineer … produced …, not an impl_report or responses":                   tsWrapWrongAnswer,
	"orchestrator: feedback context for …: …":                                                tsWrapStep,
	"orchestrator: installing the git guard for …: …":                                        tsWrapStep,
	"orchestrator: installing the read-only bd shim for …: …":                                tsWrapStep,
	"orchestrator: open …: …":                                                                tsWrapStep,
	"orchestrator: parse the run lock …: …":                                                  tsWrapStep,
	"orchestrator: parse …: …":                                                               tsWrapStep,
	"orchestrator: planning: installing the read-only bd shim in … (… in a row): …":          tsWrapStep,
	"orchestrator: planning: listing the run's beads: …":                                     tsWrapStep,
	"orchestrator: planning: no run directory for the read-only bd shim":                     tsWrapInvariant,
	"orchestrator: planning: rules: …":                                                       tsWrapStep,
	"orchestrator: planning: vibes: …":                                                       tsWrapStep,
	"orchestrator: planning: …":                                                              tsWrapStep,
	"orchestrator: pr checkout: diffing #… against …: …":                                     tsWrapStep,
	"orchestrator: pr checkout: invalid pull request number …":                               tsWrapToolOutput,
	"orchestrator: pr checkout: no remote to fetch #… from: …":                               tsWrapStep,
	"orchestrator: pr checkout: opening …: …":                                                tsWrapStep,
	"orchestrator: pr checkout: resolving the head of #…: …":                                 tsWrapStep,
	"orchestrator: pr checkout: the review bead has no id":                                   tsWrapInvariant,
	"orchestrator: pr checkout: worktree for …: …":                                           tsWrapStep,
	"orchestrator: product owner … produced …, not a ruling":                                 tsWrapWrongAnswer,
	"orchestrator: read the run lock …: …":                                                   tsWrapStep,
	"orchestrator: read …: …":                                                                tsWrapStep,
	"orchestrator: removing the bd shim …: …":                                                tsWrapStep,
	"orchestrator: resume: …: …":                                                             tsWrapStep,
	"orchestrator: rotate …: …":                                                              tsWrapStep,
	"orchestrator: run … recorded ammo …: …":                                                 tsWrapStep,
	"orchestrator: scheduler: nil run":                                                       tsWrapInvariant,
	"orchestrator: scheduler: unknown claim mode …":                                          tsWrapInvariant,
	"orchestrator: stat …: …":                                                                tsWrapStep,
	"orchestrator: sync …: …":                                                                tsWrapStep,
	"orchestrator: take the run lock: …":                                                     tsWrapStep,
	"orchestrator: terminate …: …":                                                           tsWrapStep,
	"orchestrator: tester … produced …, not a verdict":                                       tsWrapWrongAnswer,
	"orchestrator: the budget was raised to …, but the run could not be resumed: …":          tsWrapStep,
	"orchestrator: the budget was raised to …, but the run is over its other cap as well: …": tsWrapStep,
	"orchestrator: the merge path returned no result":                                        tsWrapInvariant,
	"orchestrator: the supervisor has no cartridge source":                                   tsWrapInvariant,
	"orchestrator: write the run lock …: …":                                                  tsWrapStep,
	"orchestrator: write the run lock: …":                                                    tsWrapStep,
	"orchestrator: write …: …":                                                               tsWrapStep,
	"proc: empty command":                                                                    tsWrapInvariant,
	"proc: renice: listing threads: …":                                                       tsWrapInvariant,
	"proc: renice thread … to …: …":                                                          tsWrapInvariant,
	"proc: renice to …: …":                                                                   tsWrapInvariant,
	"proc: negative grace period …":                                                          tsWrapInvariant,
	"proc: negative timeout …":                                                               tsWrapInvariant,
	"proc: …: …":                                                                             tsWrapStep,
	"prompt: cannot write a prompt without a run directory":                                  tsWrapInvariant,
	"prompt: cannot write a prompt without an agent id":                                      tsWrapInvariant,
	"prompt: creating …: …":                                                                  tsWrapStep,
	"prompt: embedded template dir: …":                                                       tsWrapStep,
	"prompt: reading template …: …":                                                          tsWrapStep,
	"prompt: repository path …: …":                                                           tsWrapStep,
	"prompt: scanning repository: …":                                                         tsWrapStep,
	"prompt: writing …: …":                                                                   tsWrapStep,
	"prompt: …":                                                                              tsWrapStep,
	"prompt: …: …":                                                                           tsWrapStep,
	"reading …: …":                                                                           tsWrapStep,
	"recreating the worktree: …":                                                             tsWrapStep,
	"release … after a lost claim: …":                                                        tsWrapStep,
	"release … before handing it to …: …":                                                    tsWrapStep,
	"removing …: …":                                                                          tsWrapStep,
	"reported version … (…)":                                                                 tsWrapToolOutput,
	"restoring …: …":                                                                         tsWrapStep,
	"resume: list the run's beads: …":                                                        tsWrapStep,
	"resume: reattach worktrees: …":                                                          tsWrapStep,
	"resume: …: …":                                                                           tsWrapStep,
	"structured: compile schema …: …":                                                        tsWrapStep,
	"structured: nil cartridge":                                                              tsWrapInvariant,
	"structured: nil contract":                                                               tsWrapInvariant,
	"structured: read schema dir: …":                                                         tsWrapStep,
	"structured: read schema …: …":                                                           tsWrapStep,
	"structured: register schema …: …":                                                       tsWrapStep,
	"structured: schema … is not valid JSON: …":                                              tsWrapStep,
	"structured: two schema files named …":                                                   tsWrapInvariant,
	"… (run …)":                                                                              tsWrapDetail,
	"… (… attempts): …":                                                                      tsWrapDetail,
	"… (… does not enforce --readonly itself)":                                               tsWrapDetail,
	"… after … (… dropped)":                                                                  tsWrapDetail,
	"… … (known: …)":                                                                         tsWrapDetail,
	"… … (registered: …)":                                                                    tsWrapDetail,
	"…: RunMessage needs a message type":                                                     tsWrapDetail,
	"…: a tester prompt needs a lens (one of …)":                                             tsWrapDetail,
	"…: active cartridge … (built: …)":                                                       tsWrapDetail,
	"…: agent id … is not usable as a directory name":                                        tsWrapDetail,
	"…: agent lens … is not a lens":                                                          tsWrapDetail,
	"…: agent role … is not a role":                                                          tsWrapDetail,
	"…: agent … has no builder":                                                              tsWrapDetail,
	"…: an arbitration needs the id of the disputed item":                                    tsWrapDetail,
	"…: an escalation needs the review round that ran out (got …)":                           tsWrapDetail,
	"…: arbitration ruling … (want one of …)":                                                tsWrapDetail,
	"…: autopilot has no brief; use brief mode":                                              tsWrapDetail,
	"…: bd returned nothing for …":                                                           tsWrapDetail,
	"…: bead … has no worktree":                                                              tsWrapDetail,
	"…: brief mode needs a brief":                                                            tsWrapDetail,
	"…: cannot marshal tool policy … (want one of …)":                                        tsWrapDetail,
	"…: cartridge … cannot enforce …":                                                        tsWrapDetail,
	"…: cartridge … cannot hold … and the firing has no worktree to copy":                    tsWrapDetail,
	"…: cartridge … must fire … in the worktree copy …, not …":                               tsWrapDetail,
	"…: claim … for …":                                                                       tsWrapDetail,
	"…: close …":                                                                             tsWrapDetail,
	"…: empty bead id":                                                                       tsWrapDetail,
	"…: empty run id":                                                                        tsWrapDetail,
	"…: empty tool policy (want one of …)":                                                   tsWrapDetail,
	"…: empty worktree path":                                                                 tsWrapDetail,
	"…: engineer mode … (want one of …)":                                                     tsWrapDetail,
	"…: escalation outcome … (want one of …)":                                                tsWrapDetail,
	"…: it has spent $…, and $… is not more":                                                 tsWrapDetail,
	"…: it has spent …, and … is not more":                                                   tsWrapDetail,
	"…: item … is a rules violation, so it cannot be ruled uphold_engineer; uphold the tester or impose a compromise that satisfies the rule": tsWrapDetail,
	"…: item … is not disputed on …":                     tsWrapDetail,
	"…: item … is not on …":                              tsWrapDetail,
	"…: lens … has no prompt template":                   tsWrapDetail,
	"…: lens … on a … prompt; only testers have a lens":  tsWrapDetail,
	"…: max beads per plan … is negative":                tsWrapDetail,
	"…: message has no payload":                          tsWrapDetail,
	"…: message type …":                                  tsWrapDetail,
	"…: nil message":                                     tsWrapDetail,
	"…: nil plan applier":                                tsWrapDetail,
	"…: no agent id":                                     tsWrapDetail,
	"…: no cartridge":                                    tsWrapDetail,
	"…: no cartridge is built for this run":              tsWrapDetail,
	"…: no releaser":                                     tsWrapDetail,
	"…: no repository root and no workdir":               tsWrapDetail,
	"…: no worktree registered at …":                     tsWrapDetail,
	"…: nothing to fire … with":                          tsWrapDetail,
	"…: nothing usable among …":                          tsWrapDetail,
	"…: reclaim … for …":                                 tsWrapDetail,
	"…: response message wants []domain.Response, got …": tsWrapDetail,
	"…: role … has no prompt template":                   tsWrapDetail,
	"…: role … has no tool policy (want one of …)":       tsWrapDetail,
	"…: role … override … (built: …)":                    tsWrapDetail,
	"…: run directory … is not absolute":                 tsWrapDetail,
	"…: run mode … (want one of …)":                      tsWrapDetail,
	"…: safety is off and there is no merge path":        tsWrapDetail,
	"…: safety is on and there is no pull request path":  tsWrapDetail,
	"…: scope …":                                         tsWrapDetail,
	"…: set … to …":                                      tsWrapDetail,
	"…: task … belongs to role …, not …":                 tsWrapDetail,
	"…: task … has no role (want one of …)":              tsWrapDetail,
	"…: task … has no tool policy (want one of …)":       tsWrapDetail,
	"…: task … is not an engineer task (want one of …)":  tsWrapDetail,
	"…: tester lens … (want one of …)":                   tsWrapDetail,
	"…: tester mode … (want one of …)":                   tsWrapDetail,
	"…: the feedback loop needs a FeedbackContextFunc":   tsWrapDetail,
	"…: the request has no bead":                         tsWrapDetail,
	"…: the rules gate refuses … on …: … still … a rules file, so fix the code first":                           tsWrapDetail,
	"…: the supervisor has no firing planner for …":                                                             tsWrapDetail,
	"…: the … prompt is ~… tokens against a budget of …, even with …; raise the budget or fire it deliberately": tsWrapDetail,
	"…: tool policy …":                 tsWrapDetail,
	"…: tool policy … (want one of …)": tsWrapDetail,
	"…: tool policy … is not one of …": tsWrapDetail,
	"…: tool policy … was not chosen for this firing: … runs … (build the spec with ApplyPolicy)": tsWrapDetail,
	"…: workdir … is not absolute":                                  tsWrapDetail,
	"…: worktree path … has no name":                                tsWrapDetail,
	"…: worktree path … is not absolute":                            tsWrapDetail,
	"…: worktree … is not absolute":                                 tsWrapDetail,
	"…: … (agent …)":                                                tsWrapDetail,
	"…: … (mapped to … by category …)":                              tsWrapDetail,
	"…: … (want one of …)":                                          tsWrapDetail,
	"…: … cannot handle … (it accepts …)":                           tsWrapDetail,
	"…: … carries no commit beyond …":                               tsWrapDetail,
	"…: … carries no feedback work":                                 tsWrapDetail,
	"…: … gave up after …":                                          tsWrapDetail,
	"…: … has conflict markers to resolve, not leftovers to commit": tsWrapDetail,
	"…: … has no commits":                                           tsWrapDetail,
	"…: … is a linked worktree of …":                                tsWrapDetail,
	"…: … is a rules violation and cannot be …; it stays open until the diff fixes it and the tester marks it resolved": tsWrapDetail,
	"…: … is a rules violation, so its severity is blocker, not …":                                                      tsWrapDetail,
	"…: … is at … after the fast-forward, want …":                                                                       tsWrapDetail,
	"…: … is inside …":                tsWrapDetail,
	"…: … is mid-rebase of …, want …": tsWrapDetail,
	"…: … is not <run-dir>/wt/<bead-id>, so the shim directory would land outside the run": tsWrapDetail,
	"…: … is not a budget":              tsWrapDetail,
	"…: … is not an ancestor of …":      tsWrapDetail,
	"…: … is not empty":                 tsWrapDetail,
	"…: … is on …, want …":              tsWrapDetail,
	"…: … is prunable (…)":              tsWrapDetail,
	"…: … is … (want …, … or …)":        tsWrapDetail,
	"…: … payload is not a JSON object": tsWrapDetail,
	"…: … still … a rules file, so approve_with_notes is not available: narrow the scope or block the bead": tsWrapDetail,
	"…: … wants …": tsWrapDetail,
	"…: … was raised as a rules violation and cannot be moved to category …": tsWrapDetail,
	"…: … was stopped before its session started":                            tsWrapDetail,
}

// tsWrappedUndocumented returns every message the wrapped packages build
// that no row of docs/troubleshooting.md matches, keyed the way
// tsWrappedExempt is. Pure punctuation wrappers ("%s: %w") are skipped, as
// is an exempted sentinel's own text: tsExempt already carries its reason.
func tsWrappedUndocumented(t *testing.T) map[string]tsMessage {
	t.Helper()
	documented := tsDocumented(t)
	exemptText := map[string]bool{}
	for _, s := range tsSentinels(t) {
		if _, ok := tsExempt[s.key()]; ok {
			exemptText[s.msg] = true
		}
	}
	out := map[string]tsMessage{}
	for _, m := range tsCodeMessages(t, tsWrappedPackages) {
		if tsWrappedSkipped(m.file) {
			continue
		}
		if !tsSaysSomething(m.text) || exemptText[m.text] || tsCovered(m.text, documented) {
			continue
		}
		out[m.display()] = m
	}
	return out
}

// tsWrappedSkipped reports whether a file is inside one of the test-only
// directories tsWrappedSkipDirs names.
func tsWrappedSkipped(path string) bool {
	for dir := range tsWrappedSkipDirs {
		if strings.Contains(filepath.ToSlash(path), "/"+dir+"/") {
			return true
		}
	}
	return false
}

// TestTroubleshootingCoversEveryMessageTheWrappedPackagesBuild is P9-19:
// the same contract the command layer is held to, over the packages
// underneath it. A message a user can read verbatim must have a row on the
// page or a written exemption here.
func TestTroubleshootingCoversEveryMessageTheWrappedPackagesBuild(t *testing.T) {
	var missing []string
	for text, m := range tsWrappedUndocumented(t) {
		if reason := tsWrappedExempt[text]; reason != "" {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s:%d %q", m.file, m.line, text))
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"docs/troubleshooting.md matches no row for these messages: add a row keyed by a phrase of one "+
			"that no verb interrupts, or add it to tsWrappedExempt with the reason a user never has to look it up")
}

// TestTroubleshootingWrappedExemptionsAreReal keeps the list from outliving
// the messages it excuses, and from quietly covering one the page now
// documents.
func TestTroubleshootingWrappedExemptionsAreReal(t *testing.T) {
	undocumented := tsWrappedUndocumented(t)
	var stale []string
	for text, reason := range tsWrappedExempt {
		require.NotEmptyf(t, reason, "%q needs a reason", text)
		if _, ok := undocumented[text]; !ok {
			stale = append(stale, text)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale,
		"tsWrappedExempt names messages nothing builds any more, or that a row of the page now documents: "+
			"drop the entries")
}

// TestTroubleshootingWrappedSkipsAreRealDirectories keeps the skipped
// directories honest: a skip that names nothing hides nothing, and one that
// outlives its directory would quietly widen the next time a package is
// renamed.
func TestTroubleshootingWrappedSkipsAreRealDirectories(t *testing.T) {
	require.NotEmpty(t, tsWrappedSkipDirs)
	seen := map[string]bool{}
	for _, pkg := range tsWrappedPackages {
		for _, path := range tsGoFiles(t, pkg) {
			for dir := range tsWrappedSkipDirs {
				if strings.Contains(filepath.ToSlash(path), "/"+dir+"/") {
					seen[dir] = true
				}
			}
		}
	}
	for dir, reason := range tsWrappedSkipDirs {
		require.NotEmptyf(t, reason, "%s needs a reason", dir)
		require.Truef(t, seen[dir], "tsWrappedSkipDirs names %q, which no file of the wrapped packages is in: "+
			"drop the entry", dir)
	}
}
